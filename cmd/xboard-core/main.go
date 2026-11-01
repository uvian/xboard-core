package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xboard-ng/xboard-core/internal/config"
	"github.com/xboard-ng/xboard-core/internal/gate"
	"github.com/xboard-ng/xboard-core/internal/kernel"
	"github.com/xboard-ng/xboard-core/internal/kernel/xray"
	"github.com/xboard-ng/xboard-core/internal/metrics"
	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xboard-ng/xboard-core/internal/sysinfo"
	"github.com/xboard-ng/xboard-core/internal/traffic"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	if hasArg("-smoke") {
		return smoke()
	}
	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level(cfg.Log.Level)})))

	// 纵深防御：MPTCP 是 CVE-2026-98432 的触发面。vendored 的 v26.10.31 在
	// DefaultListener 里已经 SetMultipathTCP(false)，这里再确保运行时层也被兜住。
	if v, ok := os.LookupEnv("GODEBUG"); !ok || !strings.Contains(v, "multipathtcp=0") {
		slog.Warn("GODEBUG lacks multipathtcp=0; socket-level SetMultipathTCP(false) still applies", "GODEBUG", v)
	}

	g := gate.New()
	mx := metrics.New()
	counter := traffic.NewCollector()

	var client *panel.Client
	if cfg.Machine.Enabled {
		client = panel.NewMachine(cfg.Machine.APIHost, fmt.Sprint(cfg.Machine.MachineID), cfg.Machine.Token, cfg.Timeout())
	} else {
		client = panel.New(cfg.Node.APIHost, cfg.Node.NodeID, cfg.Node.APIKey, cfg.Timeout())
	}

	r := &runner{
		cfg:      cfg,
		client:   client,
		registry: kernel.NewRegistry(xray.NewNode(g)),
		gate:     g,
		traffic:  counter,
		metrics:  mx,
		since:    time.Now(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.API.Listen,
		Handler:           localMux(mx, cfg.API.Metrics),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("local api", "err", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	r.handshake(ctx)
	return r.loop(ctx)
}

type runner struct {
	cfg      *config.Conf
	client   *panel.Client
	registry *kernel.Registry
	gate     *gate.Gate
	traffic  *traffic.Collector
	metrics  *metrics.Registry

	node   *panel.NodeConfig
	kernel kernel.Kernel
	users  []panel.UserInfo
	since  time.Time

	wsConnected bool
}

// handshake 只用来判断面板是否开了 WS 推送。开与不开都走轮询，不影响正确性：
// WS 只是把 pull 间隔拉长，省掉无意义的 config/user 请求。
func (r *runner) handshake(ctx context.Context) {
	h, err := r.client.Handshake(ctx)
	if err != nil {
		slog.Debug("handshake failed, polling only", "err", err)
		return
	}
	if h.Websocket.Enabled {
		// TODO(M2): 接 sync.config / sync.users / sync.user.delta / sync.devices 四个事件，
		// 收到事件后再 pull，把 PullEvery 拉到 300s 兜底。
		slog.Info("panel offers websocket push (polling still in use)", "ws_url", h.Websocket.WSURL)
	}
}

func (r *runner) loop(ctx context.Context) error {
	every := time.Duration(r.cfg.Node.PullEvery) * time.Second
	tick := time.NewTicker(every)
	defer tick.Stop()
	reportTick := time.NewTicker(reportEvery(every))
	defer reportTick.Stop()

	if err := r.pull(ctx); err != nil {
		slog.Warn("initial pull failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			if r.kernel != nil {
				return r.kernel.Close()
			}
			return nil
		case <-tick.C:
			if err := r.pull(ctx); err != nil {
				slog.Error("pull", "err", err)
				r.metrics.ReloadErrors.Add(1)
			}
		case <-reportTick.C:
			if err := r.report(ctx); err != nil {
				slog.Warn("report", "err", err)
			}
		}
	}
}

func reportEvery(pull time.Duration) time.Duration {
	// 面板 base_config.push_interval 优先；这里只做本地兜底（≥10s，防抖）。
	d := pull / 2
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// pull 拉配置与用户，按需 启动 / 重建 / 热更用户。
func (r *runner) pull(ctx context.Context) error {
	node, changed, err := r.client.Config(ctx)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	users, err := r.client.Users(ctx)
	if err != nil {
		return fmt.Errorf("users: %w", err)
	}
	if users == nil {
		// 304：面板说用户没变，沿用本地快照。
		users = r.users
	}
	if !changed && r.kernel != nil && sameUsers(r.users, users) {
		return nil
	}
	r.metrics.LastPullOK.Store(time.Now().Unix())

	k, err := r.registry.For(node.Protocol)
	if err != nil {
		return err
	}

	switch {
	case r.kernel == nil:
		if err := k.Start(ctx, node, users); err != nil {
			return err
		}
		r.kernel, r.node, r.users = k, node, users
		r.gate.LoadUsers(node.Tag, toGateUsers(users))
		r.gate.SetNodeLimit(node.Tag, nodeUpMbps(node))
		r.metrics.Users.Store(int64(len(users)))
		slog.Info("kernel started", "kernel", k.Name(), "protocol", node.Protocol,
			"tag", node.Tag, "network", node.Network, "port", node.ServerPort, "users", len(users))

	case finger(r.node) != finger(node):
		r.metrics.Reloads.Add(1)
		if err := k.Reload(ctx, node); err != nil {
			return err
		}
		r.node = node
		r.users = users
		r.metrics.KernelRestarts.Add(1)
		r.gate.SetNodeLimit(node.Tag, nodeUpMbps(node))
		r.gate.LoadUsers(node.Tag, toGateUsers(users))
		slog.Info("kernel reloaded", "tag", node.Tag, "network", node.Network, "port", node.ServerPort)

	default:
		added, deleted, modified := diff(r.users, users)
		if len(added)+len(deleted)+len(modified) > 0 {
			if err := k.UpdateUsers(ctx, added, deleted, modified); err != nil {
				return err
			}
			r.gate.UpdateUser(node.Tag, toGateUsers(added), toGateUsers(deleted), toGateUsers(modified))
			r.users = users
			r.metrics.Users.Store(int64(len(users)))
			slog.Info("users updated", "added", len(added), "deleted", len(deleted), "modified", len(modified))
		}
	}
	return nil
}

// report 一次上报带全部四类数据（V2 的 report 就是为此设计的：一个 RTT）。
func (r *runner) report(ctx context.Context) error {
	if r.kernel == nil || r.node == nil {
		return nil
	}
	rep := &panel.Report{
		Traffic: r.traffic.Drain(),
		Alive:   aliveMap(r.gate.Online(r.node.Tag)),
		Online:  onlineCount(r.gate.Online(r.node.Tag)),
	}
	if l, err := sysinfo.Stat(); err == nil {
		rep.Status = &panel.LoadStatus{
			CPU:  l.CPUPercent,
			Mem:  panel.Usage{Total: l.MemTotal, Used: l.MemUsed},
			Swap: panel.Usage{Total: l.SwapTotal, Used: l.SwapUsed},
			Disk: panel.Usage{Total: l.DiskTotal, Used: l.DiskUsed},
		}
		_ = l
	} else {
		slog.Warn("sysinfo unavailable; status will be skipped", "err", err)
	}
	rep.Metrics = &panel.Metrics{
		Uptime:            int64(time.Since(r.since).Seconds()),
		Goroutines:        runtime.NumGoroutine(),
		ActiveConnections: int(r.metrics.ActiveConns.Load()),
		TotalUsers:        len(r.users),
		ActiveUsers:       len(rep.Alive),
		KernelStatus:      r.kernel != nil,
		Kernel:            r.kernel.Name(),
		Version:           version,
	}
	return r.client.Report(ctx, rep)
}

// smoke 是 CI 里的「配置能装配出来吗」检查，不需要面板、不需要网络。
func smoke() error {
	node := &panel.NodeConfig{
		Protocol:   "vless",
		ListenIP:   "127.0.0.1",
		ServerPort: 8443,
		Network:    "tcp",
		TLS:        panel.SecurityNone,
		Decryption: "none",
		Tag:        "smoke",
	}
	users := []panel.UserInfo{{ID: 1, UUID: "11111111-2222-3333-4444-555555555555"}}
	cfg, err := xray.Build(node, users)
	if err != nil {
		return fmt.Errorf("smoke: build vless/tcp: %w", err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("smoke: marshal: %w", err)
	}
	fmt.Printf("smoke ok: vless/tcp inbound=%d app=%d bytes=%d\n", len(cfg.Inbound), len(cfg.App), len(b))
	return nil
}

func localMux(m *metrics.Registry, withMetrics bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if withMetrics {
		mux.Handle("/metrics", m.Handler())
	}
	return mux
}

func toGateUsers(users []panel.UserInfo) []gate.User {
	out := make([]gate.User, 0, len(users))
	for _, u := range users {
		out = append(out, gate.User{UID: u.ID, Email: xray.Email(u), SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit})
	}
	return out
}

func nodeUpMbps(n *panel.NodeConfig) int {
	if n == nil {
		return 0
	}
	return n.UpMbps
}

func aliveMap(online []gate.Online) map[int][]string {
	if len(online) == 0 {
		return nil
	}
	out := make(map[int][]string, len(online))
	for _, o := range online {
		out[o.UID] = append(out[o.UID], o.IP)
	}
	return out
}

func onlineCount(online []gate.Online) map[int]int {
	if len(online) == 0 {
		return nil
	}
	out := make(map[int]int, len(online))
	for _, o := range online {
		out[o.UID]++
	}
	return out
}

// finger 只包含「改了就必须重建内核」的字段；用户/限速变化走热更。
func finger(n *panel.NodeConfig) string {
	if n == nil {
		return ""
	}
	raw := ""
	if len(n.NetworkSettings) > 0 {
		raw = string(n.NetworkSettings)
	}
	tls := ""
	if len(n.TLSSettings) > 0 {
		tls = string(n.TLSSettings)
	}
	return fmt.Sprintf("%s|%s|%d|%d|%s|%d|%s|%s|%s", n.Tag, n.Protocol, n.TLS, n.ServerPort, n.Network, n.UpMbps, n.Flow, tls, raw)
}

func diff(old, new []panel.UserInfo) (added, deleted, modified []panel.UserInfo) {
	prev := make(map[string]panel.UserInfo, len(old))
	for _, u := range old {
		prev[u.UUID] = u
	}
	seen := make(map[string]struct{}, len(new))
	for _, u := range new {
		seen[u.UUID] = struct{}{}
		if p, ok := prev[u.UUID]; !ok {
			added = append(added, u)
		} else if p.SpeedLimit != u.SpeedLimit || p.DeviceLimit != u.DeviceLimit || p.ID != u.ID {
			modified = append(modified, u)
		}
	}
	for _, u := range old {
		if _, ok := seen[u.UUID]; !ok {
			deleted = append(deleted, u)
		}
	}
	return
}

func sameUsers(a, b []panel.UserInfo) bool {
	a1, d1, m1 := diff(a, b)
	return len(a1)+len(d1)+len(m1) == 0
}

func configPath() string {
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "config.yaml"
}

func hasArg(flag string) bool {
	for _, a := range os.Args[1:] {
		if a == flag {
			return true
		}
	}
	return false
}

func level(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
