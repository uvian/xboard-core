package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xboard-ng/xboard-core/internal/wsclient"
)

const (
	// wsPollFallback 是推送在线时的轮询间隔：事件是主路径，轮询只防"事件丢了"。
	wsPollFallback = 300 * time.Second
	// wsReadIdle 多久收不到任何帧就认为链路已死（面板每 55s ping 一次，留足余量）。
	wsReadIdle = 90 * time.Second
	// wsPingEvery 我们自己的探活：面板没开 ping 时靠它把半开连接踢掉。
	wsPingEvery  = 30 * time.Second
	wsBackoffMin = 2 * time.Second
	wsBackoffMax = 30 * time.Second
)

// socket 维持 WS 推送连接。断线自动重连（指数退避），重连期间由轮询兜底，
// 所以这里任何错误都不该让进程退出——最坏情况等于"回到纯轮询"。
func (r *runner) socket(ctx context.Context) {
	backoff := wsBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := r.socketOnce(ctx)
		r.signalWS(false)
		if ctx.Err() != nil {
			return
		}
		// 连上又稳定跑过一阵，退避归零，避免偶发断线后长期慢重连。
		if time.Since(start) > 30*time.Second {
			backoff = wsBackoffMin
		}
		if err != nil && !errors.Is(err, wsclient.ErrClosed) {
			slog.Warn("ws push disconnected", "err", err, "retry_in", backoff.String())
		} else {
			slog.Info("ws push closed", "retry_in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > wsBackoffMax {
			backoff = wsBackoffMax
		}
	}
}

func (r *runner) socketOnce(ctx context.Context) error {
	c, err := wsclient.Dial(ctx, r.wsURL, 10*time.Second, r.wsInsecure)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Ping(); err != nil {
		return err
	}
	// 面板连上就会推 auth.success + 全量 sync；这里仍主动拉一次，
	// 免得推送丢了之后要等兜底轮询（最长 300s）才恢复。
	go func() {
		if err := r.pullOnce(ctx); err != nil {
			slog.Warn("pull after ws connect", "err", err)
		}
	}()
	r.signalWS(true)
	slog.Info("ws push connected")

	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(wsPingEvery)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.Ping(); err != nil {
					return
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.SetReadDeadline(time.Now().Add(wsReadIdle)); err != nil {
			return err
		}
		raw, err := c.ReadMessage()
		if err != nil {
			return err
		}
		if err := r.onEvent(ctx, c, raw); err != nil {
			return err
		}
	}
}

func (r *runner) signalWS(up bool) {
	select {
	case r.wsUp <- up:
	default:
		// 上一状态还没被 loop 消费；丢掉这次通知没影响（状态是幂等的"当前值"）。
	}
}

// onEvent 处理面板推来的事件。未知事件只记 debug——面板升级加事件不该让节点断连。
func (r *runner) onEvent(ctx context.Context, c *wsclient.Conn, raw []byte) error {
	var ev panel.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("ws: 非法事件载荷: %w", err)
	}
	switch ev.Event {
	case "ping":
		return c.WriteText([]byte(`{"event":"pong"}`))

	case "auth.success":
		slog.Debug("ws authenticated", "data", string(ev.Data))
		return nil

	case "error":
		return fmt.Errorf("ws: 面板报错 %s", string(ev.Data))

	case panel.EventSyncConfig:
		var p struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("ws sync.config: %w", err)
		}
		node, err := r.client.AdoptConfig(p.Config)
		if err != nil {
			return fmt.Errorf("ws sync.config: %w", err)
		}
		r.metrics.LastPullOK.Store(time.Now().Unix())
		// changed=true：面板主动推说明配置变了，交给 finger() 判断是重建还是热更。
		return r.apply(ctx, node, r.snapshotUsers(), true)

	case panel.EventSyncUsers:
		var p panel.UsersResponse
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("ws sync.users: %w", err)
		}
		r.metrics.LastPullOK.Store(time.Now().Unix())
		r.applyMu.Lock()
		node := r.node
		r.applyMu.Unlock()
		if node == nil {
			// 配置还没来，等 sync.config 或兜底轮询处理。
			return nil
		}
		return r.apply(ctx, node, p.Users, false)

	case panel.EventSyncUserDelta:
		var p struct {
			Action string           `json:"action"`
			Users  []panel.UserInfo `json:"users"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("ws sync.user.delta: %w", err)
		}
		return r.applyDelta(ctx, p.Action, p.Users)

	case panel.EventSyncDevices:
		// 面板推的是"它记的在线设备"。本节点自己就是设备状态的来源（gate 按连接记 IP），
		// 拿面板的回灌会和自己看到的打架，所以只记 debug。
		slog.Debug("ws sync.devices ignored (gate owns device state)")
		return nil

	case "sync.nodes":
		var p struct {
			Nodes []struct {
				ID int `json:"id"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("ws sync.nodes: %w", err)
		}
		ids := make([]int, 0, len(p.Nodes))
		for _, n := range p.Nodes {
			ids = append(ids, n.ID)
		}
		slog.Info("ws machine node set updated", "nodes", ids)
		if r.onNodes != nil {
			r.onNodes(ctx, ids)
		}
		return nil

	default:
		slog.Debug("ws event ignored", "event", ev.Event)
		return nil
	}
}

func (r *runner) snapshotUsers() []panel.UserInfo {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	return r.users
}

// applyDelta 处理单用户增删改。面板的 remove 载荷只带 id，
// uuid 要从本地快照补——内核删用户是按 email（id.uuid）索引的。
func (r *runner) applyDelta(ctx context.Context, action string, incoming []panel.UserInfo) error {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	if r.node == nil || r.kernel == nil || len(incoming) == 0 {
		return nil
	}

	switch action {
	case "add", "update":
		added := make([]panel.UserInfo, 0, len(incoming))
		modified := make([]panel.UserInfo, 0, len(incoming))
		for _, u := range incoming {
			old, exists := byID(r.users, u.ID)
			if exists {
				// 修改类 delta 可能只带要改的字段：uuid/限额用本地值补齐，
				// 否则内核会按 email（id.uuid）找不到用户而静默不生效。
				u.UUID = mergeUUID(u.UUID, old.UUID)
				if u.SpeedLimit == 0 {
					u.SpeedLimit = old.SpeedLimit
				}
				if u.DeviceLimit == 0 {
					u.DeviceLimit = old.DeviceLimit
				}
				modified = append(modified, u)
			} else {
				if u.UUID == "" {
					slog.Warn("sync.user.delta add without uuid, skipped", "uid", u.ID)
					continue
				}
				added = append(added, u)
			}
			r.users = upsertUser(r.users, u)
		}
		if len(added)+len(modified) == 0 {
			return nil
		}
		r.gate.UpdateUser(r.node.Tag, toGateUsers(added), nil, toGateUsers(modified))
		if err := r.kernel.UpdateUsers(ctx, added, nil, modified); err != nil {
			return err
		}
		r.metrics.Users.Store(int64(len(r.users)))
		slog.Info("user delta applied", "action", action, "added", len(added), "modified", len(modified))

	case "remove":
		deleted := make([]panel.UserInfo, 0, len(incoming))
		for _, u := range incoming {
			old, ok := byID(r.users, u.ID)
			if !ok {
				continue
			}
			deleted = append(deleted, old)
			r.users = removeUser(r.users, u.ID)
		}
		if len(deleted) == 0 {
			return nil
		}
		r.gate.UpdateUser(r.node.Tag, nil, toGateUsers(deleted), nil)
		if err := r.kernel.UpdateUsers(ctx, nil, deleted, nil); err != nil {
			return err
		}
		r.metrics.Users.Store(int64(len(r.users)))
		slog.Info("user delta applied", "action", action, "deleted", len(deleted))

	default:
		slog.Warn("unknown sync.user.delta action", "action", action)
	}
	return nil
}

func byID(users []panel.UserInfo, id int) (panel.UserInfo, bool) {
	for _, u := range users {
		if u.ID == id {
			return u, true
		}
	}
	return panel.UserInfo{}, false
}

func upsertUser(users []panel.UserInfo, u panel.UserInfo) []panel.UserInfo {
	out := make([]panel.UserInfo, 0, len(users)+1)
	replaced := false
	for _, old := range users {
		if old.ID == u.ID {
			out = append(out, u)
			replaced = true
			continue
		}
		out = append(out, old)
	}
	if !replaced {
		out = append(out, u)
	}
	return out
}

func removeUser(users []panel.UserInfo, id int) []panel.UserInfo {
	out := make([]panel.UserInfo, 0, len(users))
	for _, u := range users {
		if u.ID == id {
			continue
		}
		out = append(out, u)
	}
	return out
}

func mergeUUID(incoming, existing string) string {
	if incoming != "" {
		return incoming
	}
	return existing
}
