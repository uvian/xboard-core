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
	"strconv"
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
	// 机器模式下的 token 是"机器"身份，但面板 V2 每个请求仍要 node_id，
	// 所以一个机器管 N 个节点 = N 条独立 runner（各自 tag / 计数器 / 内核实例）。
	var runners []*runner
	if cfg.Machine.Enabled {
		for _, nid := range cfg.Machine.NodeIDs {
			c := panel.NewMachine(cfg.Machine.APIHost, fmt.Sprint(cfg.Machine.MachineID), nid, cfg.Machine.Token, cfg.Timeout())
			runners = append(runners, newRunner(cfg, c, g, mx))
		}
	} else {
		c := panel.New(cfg.Node.APIHost, cfg.Node.NodeID, cfg.Node.APIKey, cfg.Timeout())
		runners = append(runners, newRunner(cfg, c, g, mx))
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

	for _, r := range runners {
		r.handshake(ctx)
	}

	errs := make(chan error, len(runners))
	for _, r := range runners {
		r := r
		go func() { errs <- r.loop(ctx) }()
	}
	var first error
	for range runners {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// newRunner 每个节点一条：独立的内核实例、流量计数器与标签。
func newRunner(cfg *config.Conf, c *panel.Client, g *gate.Gate, mx *metrics.Registry) *runner {
	return &runner{
		cfg:      cfg,
		client:   c,
		registry: kernel.NewRegistry(xray.NewNode(g)),
		gate:     g,
		traffic:  traffic.NewCollector(),
		metrics:  mx,
		since:    time.Now(),
		pull:     time.Duration(cfg.Node.PullEvery) * time.Second,
		resched:  make(chan time.Duration, 1),
	}
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
	pull   time.Duration

	// resched 让面板下发的 base_config.pull_interval 能改本地节奏（面板改配置不用重启节点）。
	resched chan time.Duration
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
	if r.pull <= 0 {
		r.pull = 60 * time.Second
	}
	tick := time.NewTicker(r.pull)
	defer tick.Stop()
	reportTick := time.NewTicker(reportEvery(r.pull))
	defer reportTick.Stop()

	if err := r.pullOnce(ctx); err != nil {
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
			if err := r.pullOnce(ctx); err != nil {
				slog.Error("pull", "err", err)
				r.metrics.ReloadErrors.Add(1)
			}
		case every := <-r.resched:
			r.pull = every
			tick.Reset(every)
			reportTick.Reset(reportEvery(every))
			slog.Info("pull interval updated by panel", "every", every.String())
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
func (r *runner) pullOnce(ctx context.Context) error {
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
	r.applyPanelIntervals(node)

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

// applyPanelIntervals 采信面板下发的 pull_interval（管理员改面板即可改节点节奏），
// 下限 15s：面板默认 60，但填 1 会把面板打爆。push_interval 只影响上报，这里跟随。
func (r *runner) applyPanelIntervals(node *panel.NodeConfig) {
	if node == nil || node.BaseConfig == nil {
		return
	}
	sec, ok := toInt(node.BaseConfig.PullInterval)
	if !ok || sec <= 0 {
		return
	}
	want := time.Duration(sec) * time.Second
	if want < 15*time.Second {
		want = 15 * time.Second
	}
	if want == r.pull {
		return
	}
	select {
	case r.resched <- want:
	default:
	}
}

// toInt 兼容面板把间隔下发成数字或字符串两种写法。
func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	default:
		return 0, false
	}
}

// report 一次上报带全部四类数据（V2 的 report 就是为此设计的：一个 RTT）。
func (r *runner) report(ctx context.Context) error {
	if r.kernel == nil || r.node == nil {
		return nil
	}
	online := r.gate.Online(r.node.Tag)
	rep := &panel.Report{
		Traffic: r.traffic.Drain(),
		// 面板 processAlive 会给每个 uid 写一次 Redis；只报"有设备数限制"的用户即可，
		// 因为 alivelist（踢人/在线设备面板）只查 device_limit > 0 的人。
		Alive:  aliveMap(limitedOnly(online, r.users)),
		Online: onlineCount(online),
	}
	if l, err := sysinfo.Stat(); err == nil {
		rep.Status = &panel.LoadStatus{
			CPU:          l.CPUPercent,
			Mem:          panel.Usage{Total: l.MemTotal, Used: l.MemUsed},
			Swap:         panel.Usage{Total: l.SwapTotal, Used: l.SwapUsed},
			Disk:         panel.Usage{Total: l.DiskTotal, Used: l.DiskUsed},
			KernelStatus: r.kernel != nil,
		}
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

// limitedOnly 过滤出「面板设置了 device_limit」的用户，减少 alive 上报的写放大。
func limitedOnly(online []gate.Online, users []panel.UserInfo) []gate.Online {
	if len(online) == 0 {
		return online
	}
	limited := make(map[int]struct{}, len(users))
	for _, u := range users {
		if u.DeviceLimit > 0 {
			limited[u.ID] = struct{}{}
		}
	}
	if len(limited) == 0 {
		return nil
	}
	out := online[:0]
	for _, o := range online {
		if _, ok := limited[o.UID]; ok {
			out = append(out, o)
		}
	}
	return out
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
