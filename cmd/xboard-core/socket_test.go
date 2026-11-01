package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/xboard-ng/xboard-core/internal/config"
	"github.com/xboard-ng/xboard-core/internal/gate"
	"github.com/xboard-ng/xboard-core/internal/metrics"
	"github.com/xboard-ng/xboard-core/internal/panel"
)

// fakeKernel 记录内核收到的用户变更，用来验证"WS 事件 → 内核"这条链路。
type fakeKernel struct {
	mu       sync.Mutex
	started  []panel.UserInfo
	added    []panel.UserInfo
	deleted  []panel.UserInfo
	modified []panel.UserInfo
	closed   bool
}

func (f *fakeKernel) Name() string                                    { return "fake" }
func (f *fakeKernel) Supported(string) bool                           { return true }
func (f *fakeKernel) Reload(context.Context, *panel.NodeConfig) error { return nil }
func (f *fakeKernel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *fakeKernel) Start(_ context.Context, _ *panel.NodeConfig, users []panel.UserInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = users
	return nil
}
func (f *fakeKernel) UpdateUsers(_ context.Context, added, deleted, modified []panel.UserInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, added...)
	f.deleted = append(f.deleted, deleted...)
	f.modified = append(f.modified, modified...)
	return nil
}

func testRunner(t *testing.T, users []panel.UserInfo) (*runner, *gate.Gate, *fakeKernel) {
	t.Helper()
	cfg := &config.Conf{}
	cfg.Node.PullEvery = 60
	g := gate.New()
	t.Cleanup(g.Close)
	node := &panel.NodeConfig{Protocol: "vless", ServerPort: 443, Tag: "node-7"}
	g.SetNodeLimit(node.Tag, 0)
	g.LoadUsers(node.Tag, toGateUsers(users))

	r := newRunner(cfg, panel.New("http://127.0.0.1:1", 7, "tok", time.Second), g, metrics.New())
	k := &fakeKernel{started: users}
	r.kernel, r.node, r.users = k, node, users
	return r, g, k
}

func TestApplyDeltaAddThenRemove(t *testing.T) {
	old := panel.UserInfo{ID: 1, UUID: "uuid-1", DeviceLimit: 1}
	r, g, k := testRunner(t, []panel.UserInfo{old})

	nu := panel.UserInfo{ID: 2, UUID: "uuid-2", SpeedLimit: 100, DeviceLimit: 2}
	if err := r.applyDelta(context.Background(), "add", []panel.UserInfo{nu}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(k.added) != 1 || k.added[0].UUID != "uuid-2" {
		t.Fatalf("内核没收到新增用户: %+v", k.added)
	}
	if len(r.users) != 2 {
		t.Fatalf("本地快照 %d 条，期望 2", len(r.users))
	}
	// 闸门必须立刻认识新用户，否则用户侧"刚加就断流"。
	if err := g.Admit(context.Background(), "node-7", "2.uuid-2", "1.2.3.4"); err != nil {
		t.Fatalf("新增用户被闸门拒: %v", err)
	}

	if err := r.applyDelta(context.Background(), "remove", []panel.UserInfo{{ID: 2}}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(k.deleted) != 1 || k.deleted[0].UUID != "uuid-2" {
		t.Fatalf("内核没收到删除（应补全 uuid）: %+v", k.deleted)
	}
	if err := g.Admit(context.Background(), "node-7", "2.uuid-2", "1.2.3.4"); err == nil {
		t.Fatal("已删除用户仍被放行")
	}
}

func TestApplyDeltaModifyUsesLocalUUID(t *testing.T) {
	old := panel.UserInfo{ID: 3, UUID: "uuid-3", SpeedLimit: 10, DeviceLimit: 1}
	r, _, k := testRunner(t, []panel.UserInfo{old})

	// 面板的 delta 可能不带 uuid；匹配到本地记录就该走"改"而不是"加/删"。
	upd := panel.UserInfo{ID: 3, SpeedLimit: 200, DeviceLimit: 5}
	if err := r.applyDelta(context.Background(), "update", []panel.UserInfo{upd}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(k.modified) != 1 || k.modified[0].UUID != "uuid-3" || k.modified[0].SpeedLimit != 200 {
		t.Fatalf("期望按 uid 命中并补 uuid，实际 %+v", k.modified)
	}
	if len(r.users) != 1 || r.users[0].SpeedLimit != 200 {
		t.Fatalf("本地快照没更新: %+v", r.users)
	}
}

func TestApplyDeltaRemoveUnknownIsNoop(t *testing.T) {
	r, _, k := testRunner(t, nil)
	if err := r.applyDelta(context.Background(), "remove", []panel.UserInfo{{ID: 99}}); err != nil {
		t.Fatalf("remove unknown: %v", err)
	}
	if len(k.deleted) != 0 {
		t.Fatalf("不该给内核发删除: %+v", k.deleted)
	}
}

func TestOnEventSyncUsersApplies(t *testing.T) {
	r, g, k := testRunner(t, []panel.UserInfo{{ID: 1, UUID: "uuid-1"}})
	raw, _ := json.Marshal(map[string]any{
		"event": "sync.users",
		"data": map[string]any{
			"users": []map[string]any{
				{"id": 1, "uuid": "uuid-1"},
				{"id": 2, "uuid": "uuid-2", "device_limit": 1},
			},
		},
	})
	if err := r.onEvent(context.Background(), nil, raw); err != nil {
		t.Fatalf("onEvent: %v", err)
	}
	if len(k.added) != 1 || k.added[0].UUID != "uuid-2" {
		t.Fatalf("内核没收到新增: %+v", k.added)
	}
	if err := g.Admit(context.Background(), "node-7", "2.uuid-2", "9.9.9.9"); err != nil {
		t.Fatalf("新用户被拒: %v", err)
	}
}

func TestOnEventIgnoresUnknownAndDevices(t *testing.T) {
	r, _, _ := testRunner(t, nil)
	for _, ev := range []string{
		`{"event":"metrics.something","data":{}}`,
		`{"event":"sync.devices","data":{"users":{"1":["1.1.1.1"]}}}`,
	} {
		if err := r.onEvent(context.Background(), nil, []byte(ev)); err != nil {
			t.Fatalf("事件 %s 不该报错: %v", ev, err)
		}
	}
	if err := r.onEvent(context.Background(), nil, []byte(`{"event":"error","data":{"message":"invalid token"}}`)); err == nil {
		t.Fatal("面板 error 事件应该让连接重建")
	}
}

func TestSupervisorReconcileDropsStale(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := &supervisor{
		cfg:     &config.Conf{},
		gate:    gate.New(),
		metrics: metrics.New(),
		baseCtx: base,
		runners: map[int]*runner{},
		cancels: map[int]context.CancelFunc{},
	}
	stopped := false
	// 只放一条"假" runner（不真起循环），验证不在新列表里的会被取消。
	sup.runners[1] = &runner{}
	sup.cancels[1] = func() { stopped = true }

	sup.reconcile(base, []int{2})
	if !stopped {
		t.Fatal("节点 1 不在新列表里，应该被停掉")
	}
	if _, ok := sup.runners[1]; ok {
		t.Fatal("节点 1 应从 runners 里移除")
	}
	if _, ok := sup.runners[2]; !ok {
		t.Fatal("节点 2 应被加入")
	}
	sup.stopAll()
}
