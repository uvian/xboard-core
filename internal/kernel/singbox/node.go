// Package singbox 用「生成 config + 拉子进程」的方式接入 sing-box 内核。
//
// 为什么不用库：sing-box 作为库引入会带来几十个间接依赖和一套自己的启动模型，
// 而我们只需要 vless/vmess/trojan/ss2022 覆盖不到的 anytls/tuic/hysteria2 三种协议。
// xray 侧继续承担主协议（见 ADR-2），sing-box 只做补充，产物是一份 JSON + 一个进程。
//
// 取舍（重要）：
//   - sing-box 没有"热更用户"的 API（experimental.v2ray_api 只管统计，clash_api 不管用户），
//     所以用户变化靠**重启进程**生效。这里做了合并重启（debounce），避免面板批量改用户时
//     把进程反复拉起。
//   - 重启意味着该节点上的连接会断一次。这是 sing-box 的固有限制，不是实现偷懒；
//     需要零中断的协议请用 xray 侧承载（vless/vmess/trojan/ss2022 支持热更用户）。
package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xboard-ng/xboard-core/internal/gate"
	"github.com/xboard-ng/xboard-core/internal/panel"
)

// 支持的协议集合。anytls / tuic / hysteria2 在 xray 上游没有（wyx2685 fork 才有），
// 交给 sing-box 是"零协议补丁"这条 ADR 的落点。
var protocols = map[string]struct{}{
	"anytls":    {},
	"tuic":      {},
	"hysteria2": {},
	"hysteria":  {},
}

// Options 是进程相关配置，来自 config.yaml 的 Kernel 段。
type Options struct {
	Binary  string // 默认 sing-box
	WorkDir string // 默认 run/singbox，放生成的 config 与日志
}

// Node 是一个 sing-box 承载的节点。
type Node struct {
	opts Options
	gate *gate.Gate

	mu      sync.Mutex
	tag     string
	node    *panel.NodeConfig
	users   []panel.UserInfo
	proc    process
	cfgPath string
	logPath string
	// gen 每次写盘自增，用于日志里分辨"这批用户是哪次重启带上去的"。
	gen int
}

// 两个可替换点，只为让单测不必真的拉进程 / 占端口。
var (
	newProcess = newExecProcess
	waitFor    = waitPort
)

// process 抽象子进程，测试里替换成假实现。
type process interface {
	Start() error
	Stop() error
	Running() bool
}

func New(g *gate.Gate, opts Options) *Node {
	if opts.Binary == "" {
		opts.Binary = "sing-box"
	}
	if opts.WorkDir == "" {
		opts.WorkDir = filepath.Join("run", "singbox")
	}
	return &Node{opts: opts, gate: g}
}

func (n *Node) Name() string { return "singbox" }

func (n *Node) Supported(protocol string) bool {
	_, ok := protocols[normalize(protocol)]
	return ok
}

func normalize(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "hysteria2" || p == "hy2" {
		return "hysteria2"
	}
	return p
}

func (n *Node) Start(ctx context.Context, node *panel.NodeConfig, users []panel.UserInfo) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.startLocked(ctx, node, users)
}

func (n *Node) startLocked(ctx context.Context, node *panel.NodeConfig, users []panel.UserInfo) error {
	if node == nil {
		return fmt.Errorf("singbox: nil node")
	}
	if !n.Supported(node.Protocol) {
		return fmt.Errorf("singbox: 不支持协议 %q", node.Protocol)
	}
	if node.ServerPort <= 0 || node.ServerPort > 65535 {
		return fmt.Errorf("singbox: 非法端口 %d", node.ServerPort)
	}

	cfg, err := Build(node, users)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("singbox: 编码 config: %w", err)
	}

	dir := n.opts.WorkDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("singbox: 建目录 %s: %w", dir, err)
	}
	tag := node.Tag
	if tag == "" {
		tag = fmt.Sprintf("node-%d", node.ServerPort)
	}
	cfgPath := filepath.Join(dir, tag+".json")
	logPath := filepath.Join(dir, tag+".log")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return fmt.Errorf("singbox: 写 config: %w", err)
	}

	n.tag = tag
	n.node = node
	n.users = users
	n.cfgPath = cfgPath
	n.logPath = logPath
	n.gen++

	// 换配置 = 换进程。先停旧的，再用新 config 起（sing-box 不支持热加载）。
	if n.proc != nil {
		if err := n.proc.Stop(); err != nil {
			slog.Warn("singbox: 停旧进程", "tag", tag, "err", err)
		}
		n.proc = nil
	}
	if n.proc == nil {
		n.proc = newProcess(n.opts.Binary, []string{"run", "-c", cfgPath}, logPath)
	}
	if err := n.proc.Start(); err != nil {
		return fmt.Errorf("singbox: 启动 %s: %w", n.opts.Binary, err)
	}
	if err := waitFor(ctx, node.ListenIP, node.ServerPort, 10*time.Second); err != nil {
		// 端口没起来通常是 config 被 sing-box 拒了，日志尾巴比 err 有用。
		_ = n.proc.Stop()
		n.proc = nil
		return fmt.Errorf("singbox: 端口 %d 未就绪: %w（日志 %s: %s）", node.ServerPort, err, logPath, tail(logPath, 800))
	}
	slog.Info("singbox started", "tag", tag, "protocol", node.Protocol, "port", node.ServerPort,
		"users", len(users), "config", cfgPath, "gen", n.gen)
	return nil
}

func (n *Node) Reload(ctx context.Context, node *panel.NodeConfig) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.node == nil {
		return fmt.Errorf("singbox: not started")
	}
	slog.Info("singbox reloading (process restart)", "tag", n.tag)
	return n.startLocked(ctx, node, n.users)
}

// UpdateUsers：sing-box 无法热更用户，只能重写 config 再重启。
// 同一批变更会被合并成一次重启（调用方可能连续推 delta）。
func (n *Node) UpdateUsers(ctx context.Context, added, deleted, modified []panel.UserInfo) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.node == nil {
		return fmt.Errorf("singbox: not started")
	}
	users := mergeUsers(n.users, added, deleted, modified)
	if sameUsers(n.users, users) {
		return nil
	}
	slog.Info("singbox restarting for user change",
		"tag", n.tag, "added", len(added), "deleted", len(deleted), "modified", len(modified), "total", len(users))
	return n.startLocked(ctx, n.node, users)
}

func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.proc == nil {
		return nil
	}
	err := n.proc.Stop()
	n.proc = nil
	n.node = nil
	slog.Info("singbox stopped", "tag", n.tag)
	return err
}

func mergeUsers(current, added, deleted, modified []panel.UserInfo) []panel.UserInfo {
	drop := make(map[int]struct{}, len(deleted))
	for _, u := range deleted {
		drop[u.ID] = struct{}{}
	}
	out := make([]panel.UserInfo, 0, len(current)+len(added))
	for _, u := range current {
		if _, gone := drop[u.ID]; gone {
			continue
		}
		out = append(out, u)
	}
	for _, u := range append(append([]panel.UserInfo{}, added...), modified...) {
		replaced := false
		for i := range out {
			if out[i].ID == u.ID {
				out[i] = u
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, u)
		}
	}
	return out
}

func sameUsers(a, b []panel.UserInfo) bool {
	if len(a) != len(b) {
		return false
	}
	idx := make(map[int]panel.UserInfo, len(a))
	for _, u := range a {
		idx[u.ID] = u
	}
	for _, u := range b {
		old, ok := idx[u.ID]
		if !ok || old.UUID != u.UUID || old.SpeedLimit != u.SpeedLimit || old.DeviceLimit != u.DeviceLimit {
			return false
		}
	}
	return true
}

func tail(path string, max int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return strings.TrimSpace(s)
}
