package xray

import (
	"context"
	"fmt"
	"sync"

	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xtls/xray-core/app/usergate"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/stats"
)

// Node 是 xray 内核的运行时封装，一个进程内每个节点一个 Node。
type Node struct {
	mu       sync.RWMutex
	instance *core.Instance
	node     *panel.NodeConfig
	users    []panel.UserInfo
	gate     usergate.Gate
	tag      string
	// lastTraffic 保存上次采样到的累计流量，SampleTraffic 用它算增量。
	lastTraffic map[string][2]int64
}

func NewNode(g usergate.Gate) *Node { return &Node{gate: g} }

func (n *Node) Name() string { return "xray" }

// Supported 只声明 xray 真正独占的协议；anytls/tuic/hysteria2 交给 sing-box 内核，
// 这样就不必背 wyx2685/xray-core fork 里那 6900 行协议补丁。
func (n *Node) Supported(protocol string) bool {
	switch protocol {
	case "vless", "vmess", "trojan", "shadowsocks":
		return true
	}
	return false
}

func (n *Node) Start(ctx context.Context, node *panel.NodeConfig, users []panel.UserInfo) error {
	if n.gate != nil {
		// dispatcher 的 usergate 钩子只可能有一个 Gate，多节点进程共用同一个 Gate 实例。
		usergate.Install(n.gate)
	}
	cfg, err := Build(node, users)
	if err != nil {
		return err
	}
	inst, err := core.New(cfg)
	if err != nil {
		return fmt.Errorf("xray: build instance: %w", err)
	}
	if err := inst.Start(); err != nil {
		return fmt.Errorf("xray: start: %w", err)
	}
	n.mu.Lock()
	n.instance, n.node, n.users, n.tag = inst, node, users, node.Tag
	n.mu.Unlock()
	return nil
}

// Reload 用新配置重建实例并原子替换。xray 不支持原地改结构型配置，
// 所以这里是「先起新、再关旧」：新实例 Start 成功前，旧实例继续服务现有连接。
func (n *Node) Reload(ctx context.Context, node *panel.NodeConfig) error {
	n.mu.RLock()
	old, users := n.instance, n.users
	n.mu.RUnlock()

	cfg, err := Build(node, users)
	if err != nil {
		return err
	}
	inst, err := core.New(cfg)
	if err != nil {
		return err
	}
	if err := inst.Start(); err != nil {
		_ = inst.Close()
		return err
	}
	n.mu.Lock()
	n.instance, n.node, n.tag = inst, node, node.Tag
	n.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// UpdateUsers 走 inbound.Manager 的 AddUser/RemoveUser 热更新，
// 与 Xboard-Node / v2node 同款路径：加一个用户不必重启内核（重启会断所有在线连接）。
func (n *Node) UpdateUsers(ctx context.Context, added, deleted, modified []panel.UserInfo) error {
	n.mu.RLock()
	inst, tag, node, current := n.instance, n.tag, n.node, n.users
	n.mu.RUnlock()
	if inst == nil || node == nil {
		return fmt.Errorf("xray: not started")
	}
	ihm, ok := inst.GetFeature(inbound.ManagerType()).(inbound.Manager)
	if !ok {
		return fmt.Errorf("xray: inbound manager unavailable")
	}
	handler, err := ihm.GetHandler(ctx, tag)
	if err != nil {
		return fmt.Errorf("xray: inbound %q: %w", tag, err)
	}
	um, ok := handler.(interface {
		AddUser(context.Context, []*protocol.User) error
		RemoveUser(context.Context, []*protocol.User) error
	})
	if !ok {
		return fmt.Errorf("xray: inbound %q does not support hot user update", tag)
	}

	upsert := append(append([]panel.UserInfo{}, added...), modified...)
	if len(upsert) > 0 {
		list := make([]*protocol.User, 0, len(upsert))
		for _, u := range upsert {
			pu, err := buildUser(node.Protocol, u)
			if err != nil {
				return err
			}
			list = append(list, pu)
		}
		if err := um.AddUser(ctx, list); err != nil {
			return fmt.Errorf("xray: AddUser: %w", err)
		}
	}
	if len(deleted) > 0 {
		list := make([]*protocol.User, 0, len(deleted))
		for _, u := range deleted {
			pu, err := buildUser(node.Protocol, u)
			if err != nil {
				return err
			}
			list = append(list, pu)
		}
		if err := um.RemoveUser(ctx, list); err != nil {
			return fmt.Errorf("xray: RemoveUser: %w", err)
		}
	}

	// 同步本地用户快照，供下次 Reload 复用（否则热更过的用户会在重建时丢失）。
	n.mu.Lock()
	n.users = mergeUsers(current, added, deleted, modified)
	n.mu.Unlock()
	return nil
}

func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.instance == nil {
		return nil
	}
	err := n.instance.Close()
	n.instance = nil
	return err
}

// Stats 暴露 stats.Manager 供 traffic.Collector 读取 per-user 计数。
func (n *Node) Stats() stats.Manager {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.instance == nil {
		return nil
	}
	sm, _ := n.instance.GetFeature(stats.ManagerType()).(stats.Manager)
	return sm
}

func mergeUsers(cur, added, deleted, modified []panel.UserInfo) []panel.UserInfo {
	byUUID := make(map[string]panel.UserInfo, len(cur)+len(added)+len(modified))
	for _, u := range cur {
		byUUID[u.UUID] = u
	}
	for _, u := range deleted {
		delete(byUUID, u.UUID)
	}
	for _, u := range append(append([]panel.UserInfo{}, added...), modified...) {
		byUUID[u.UUID] = u
	}
	out := make([]panel.UserInfo, 0, len(byUUID))
	for _, u := range byUUID {
		out = append(out, u)
	}
	return out
}
