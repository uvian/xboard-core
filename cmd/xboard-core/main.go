package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xboard-ng/xboard-core/internal/config"
	"github.com/xboard-ng/xboard-core/internal/gate"
	"github.com/xboard-ng/xboard-core/internal/kernel"
	"github.com/xboard-ng/xboard-core/internal/kernel/singbox"
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
	xray.SetLogLevel(cfg.Log.Level)
	xray.SetAllowPrivateTargets(cfg.Kernel.AllowPrivateTargets)

	// 纵深防御：MPTCP 是 CVE-2026-98432 的触发面。vendored 的 v26.10.31 在
	// DefaultListener 里已经 SetMultipathTCP(false)，这里再确保运行时层也被兜住。
	if v, ok := os.LookupEnv("GODEBUG"); !ok || !strings.Contains(v, "multipathtcp=0") {
		// v26.10.31 的 DefaultListener 已经 SetMultipathTCP(false)，这一层只是纵深防御，
		// 所以用 Info：默认出现 WARN 会让人以为配置错了（见 docs/STATUS.md §5）。
		slog.Info("GODEBUG lacks multipathtcp=0; socket-level SetMultipathTCP(false) still applies", "GODEBUG", v)
	}

	g := gate.New()
	mx := metrics.New()
	g.OnReject(func(error) { mx.GateRejects.Add(1) })
	// 机器模式下的 token 是"机器"身份，但面板 V2 每个请求仍要 node_id，
	// 所以一个机器管 N 个节点 = N 条独立 runner（各自 tag / 计数器 / 内核实例）。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sup := &supervisor{cfg: cfg, gate: g, metrics: mx, baseCtx: ctx,
		runners: map[int]*runner{}, cancels: map[int]context.CancelFunc{}}
	if cfg.Machine.Enabled {
		// 面板可能在运行中改"这台机器管哪些节点"（sync.nodes），由 supervisor 增删。
		sup.reconcile(ctx, cfg.Machine.NodeIDs)
	} else {
		sup.spawn(0, panel.New(cfg.Node.APIHost, cfg.Node.NodeID, cfg.Node.APIKey, cfg.Timeout()))
	}

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

	<-ctx.Done()
	sup.stopAll()
	sup.wg.Wait()
	return nil
}

// supervisor 管一批 runner 的生命周期。单节点模式只有 key=0 一条；
// 机器模式每条节点一个 key，面板推 sync.nodes 时在这里增删。
type supervisor struct {
	cfg     *config.Conf
	gate    *gate.Gate
	metrics *metrics.Registry
	baseCtx context.Context

	mu      sync.Mutex
	runners map[int]*runner
	cancels map[int]context.CancelFunc
	wg      sync.WaitGroup
}

func (s *supervisor) spawn(key int, client *panel.Client) {
	r := newRunner(s.cfg, client, s.gate, s.metrics)
	if s.cfg.Machine.Enabled {
		r.onNodes = s.reconcile
	}
	rctx, cancel := context.WithCancel(s.baseCtx)

	s.mu.Lock()
	s.runners[key] = r
	s.cancels[key] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// handshake 可能慢（面板没起、证书不对），放 goroutine 里免得卡住 reconcile。
		r.handshake(rctx)
		if err := r.loop(rctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("runner stopped", "node", key, "err", err)
		}
	}()
	slog.Info("runner scheduled", "node", key)
}

func (s *supervisor) stop(key int) {
	s.mu.Lock()
	cancel := s.cancels[key]
	delete(s.cancels, key)
	delete(s.runners, key)
	s.mu.Unlock()
	if cancel != nil {
		cancel() // loop 退出时会关内核
	}
	slog.Info("runner stopped", "node", key)
}

func (s *supervisor) stopAll() {
	s.mu.Lock()
	keys := make([]int, 0, len(s.cancels))
	for k := range s.cancels {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	for _, k := range keys {
		s.stop(k)
	}
}

func (s *supervisor) has(key int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.runners[key]
	return ok
}

// reconcile 让运行中的 runner 集合与面板下发的节点列表对齐。
func (s *supervisor) reconcile(_ context.Context, ids []int) {
	want := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	s.mu.Lock()
	current := make([]int, 0, len(s.runners))
	for k := range s.runners {
		current = append(current, k)
	}
	s.mu.Unlock()

	for _, k := range current {
		if _, ok := want[k]; !ok {
			s.stop(k)
		}
	}
	for _, id := range ids {
		if s.has(id) {
			continue
		}
		c := panel.NewMachine(s.cfg.Machine.APIHost, fmt.Sprint(s.cfg.Machine.MachineID), id, s.cfg.Machine.Token, s.cfg.Timeout())
		s.spawn(id, c)
	}
}

// buildRegistry 按配置注册内核。顺序即优先级：能承载同一协议时取先注册的（xray 在前）。
func buildRegistry(cfg *config.Conf, g *gate.Gate) *kernel.Registry {
	ks := make([]kernel.Kernel, 0, 2)
	if cfg.Kernel.Xray {
		ks = append(ks, xray.NewNode(g))
	}
	if cfg.Kernel.Singbox {
		ks = append(ks, singbox.New(g, singbox.Options{
			Binary:  cfg.Kernel.SingboxBinary,
			WorkDir: cfg.Kernel.SingboxDir,
		}))
	}
	return kernel.NewRegistry(ks...)
}

// newRunner 每个节点一条：独立的内核实例、流量计数器与标签。
func newRunner(cfg *config.Conf, c *panel.Client, g *gate.Gate, mx *metrics.Registry) *runner {
	return &runner{
		cfg:      cfg,
		client:   c,
		registry: buildRegistry(cfg, g),
		gate:     g,
		traffic:  traffic.NewCollector(),
		metrics:  mx,
		since:    time.Now(),
		pull:     time.Duration(cfg.Node.PullEvery) * time.Second,
		resched:  make(chan time.Duration, 1),
		wsUp:     make(chan bool, 1),
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

	// wsURL 为空表示面板没开推送（或 handshake 失败），此时纯轮询。
	wsURL      string
	wsInsecure bool
	// wsUp 由 socket goroutine 通知"推送通道是否在线"，在线时轮询退化为兜底。
	wsUp chan bool
	// applyMu 串行化"应用配置/用户"的逻辑：WS 事件与兜底轮询会同时到达。
	applyMu sync.Mutex
	// onNodes 只在机器模式用到：面板推 sync.nodes 时通知上层增删 runner。
	onNodes func(ctx context.Context, ids []int)
	// gateWarned 保证"sing-box 无闸门"这条警告每个节点只打一次。
	gateWarned bool
}

// handshake 问面板要不要走 WS 推送。拿不到就纯轮询，正确性不受影响：
// WS 只把轮询间隔拉长到兜底值，省掉无意义的 config/user 请求（见 socket.go）。
func (r *runner) handshake(ctx context.Context) {
	h, err := r.client.Handshake(ctx)
	if err != nil {
		slog.Debug("handshake failed, polling only", "err", err)
		return
	}
	if !h.Websocket.Enabled {
		return
	}
	url := h.Websocket.WSURL
	if url == "" {
		slog.Warn("panel reports ws enabled but ws_url is empty, polling only")
		return
	}
	// 面板只给 ws://host:port/ws，鉴权参数要自己拼：Workerman 侧从 query 取
	// token + node_id（节点模式）或 token + machine_id（机器模式），否则连上即被 close。
	r.wsURL = withAuth(url, r.cfg)
	r.wsInsecure = r.cfg.Node.WSInsecure
	slog.Info("panel offers websocket push", "ws_url", url)
}

// withAuth 把面板要求鉴权参数拼进 ws_url 的 query。
func withAuth(raw string, cfg *config.Conf) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("token", cfg.Node.APIKey)
	if cfg.Machine.Enabled {
		q.Set("machine_id", strconv.Itoa(cfg.Machine.MachineID))
		q.Set("node_id", strconv.Itoa(cfg.Node.NodeID))
	} else {
		q.Set("node_id", strconv.Itoa(cfg.Node.NodeID))
	}
	u.RawQuery = q.Encode()
	return u.String()
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
	if r.wsURL != "" {
		go r.socket(ctx)
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
		case up := <-r.wsUp:
			// 推送在线时把轮询退成兜底（防"事件丢了"）；掉线立刻回到面板配置的节奏。
			every := r.pull
			if up {
				every = wsPollFallback
				if r.pull > every {
					every = r.pull
				}
			}
			tick.Reset(every)
			reportTick.Reset(reportEvery(every))
		case <-reportTick.C:
			if err := r.report(ctx); err != nil {
				slog.Warn("report", "err", err)
			}
		}
	}
}

func reportEvery(pull time.Duration) time.Duration {
	// 面板 base_config.push_interval 优先；这里只做本地兜底（≥10s 防抖，≤60s 保证
	// 面板侧 u/d、在线数与负载不会长时间不更新——WS 在线时轮询会退成兜底值，
	// 但上报节奏不能跟着退，否则面板会认为节点数据停了）。
	d := pull / 2
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	if d > 60*time.Second {
		d = 60 * time.Second
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
	return r.apply(ctx, node, users, changed)
}

// apply 是唯一改内核/闸门状态的地方，由 applyMu 串行化（WS 事件与轮询会撞车）。
func (r *runner) apply(ctx context.Context, node *panel.NodeConfig, users []panel.UserInfo, changed bool) error {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	if node == nil {
		return nil
	}
	r.applyPanelIntervals(node)

	if !changed && r.kernel != nil && sameUsers(r.users, users) {
		return nil
	}
	r.metrics.LastPullOK.Store(time.Now().Unix())

	// 内核实例只取一次：已启动的直接复用，未启动的按协议向 registry 要。
	// （registry 里每个内核只有一个实例，这里显式区分是为了不依赖那个事实。）
	k := r.kernel
	if k == nil {
		var err error
		if k, err = r.registry.For(node.Protocol); err != nil {
			return err
		}
	}

	switch {
	case r.kernel == nil:
		// 先把用户灌进闸门再起内核：反过来的话，配置就绪到 LoadUsers 之间的新连接
		// 会全部命中 ErrUnknownUser（用户侧表现为"刚切节点就断流"）。
		r.gate.SetNodeLimit(node.Tag, nodeUpMbps(node))
		r.gate.LoadUsers(node.Tag, toGateUsers(users))
		if err := k.Start(ctx, node, users); err != nil {
			return err
		}
		r.kernel, r.node, r.users = k, node, users
		if k.Name() != "xray" && !r.gateWarned {
			// 设备数/限速挂在 xray 的 dispatcher 上（app/usergate）。sing-box 走子进程，
			// 没有等价的注入点：这类协议只做协议转换，闸门不生效。说清楚比默默放行好。
			r.gateWarned = true
			slog.Warn("该协议由 sing-box 承载：设备数限制与限速在此节点不生效",
				"protocol", node.Protocol, "kernel", k.Name())
		}
		r.metrics.Users.Store(int64(len(users)))
		slog.Info("kernel started", "kernel", k.Name(), "protocol", node.Protocol,
			"tag", node.Tag, "network", node.Network, "port", node.ServerPort, "users", len(users))
		if len(users) == 0 {
			// 面板不下发用户时入站照常监听，但任何连接都会因未知 UUID 被拒，
			// 现场很像"节点起来了却没人能连"——必须喊出来，而不是静默空转。
			slog.Warn("node has no users from panel; every connection will be rejected",
				"node", node.Tag, "hint", "check group_ids / transfer_enable / expired_at / banned")
		}

	case finger(r.node) != finger(node):
		r.metrics.Reloads.Add(1)
		// 同上：闸门先就绪，再切内核。
		r.gate.SetNodeLimit(node.Tag, nodeUpMbps(node))
		r.gate.LoadUsers(node.Tag, toGateUsers(users))
		if err := k.Reload(ctx, node); err != nil {
			return err
		}
		r.node = node
		r.users = users
		r.metrics.KernelRestarts.Add(1)
		slog.Info("kernel reloaded", "tag", node.Tag, "network", node.Network, "port", node.ServerPort)

	default:
		added, deleted, modified := diff(r.users, users)
		if len(added)+len(deleted)+len(modified) > 0 {
			// 先更新闸门再更新内核：新增用户可以立刻接入，被删用户在两次调用之间就被拒。
			r.gate.UpdateUser(node.Tag, toGateUsers(added), toGateUsers(deleted), toGateUsers(modified))
			if err := k.UpdateUsers(ctx, added, deleted, modified); err != nil {
				return err
			}
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
	// 先把内核侧的累计计数采成增量，再 Drain 出本拍要上报的量。
	if s, ok := r.kernel.(kernel.TrafficSampler); ok {
		for uid, d := range s.SampleTraffic() {
			r.traffic.Add(uid, d[0], d[1])
		}
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
