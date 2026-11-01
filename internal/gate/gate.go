package gate

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrDeviceLimit 表示超过设备数上限；被 dispatcher 捕获后直接断连，
// 面板侧的"在线设备"下一轮上报即消失，用户端表现为"连上即断"。
var ErrDeviceLimit = errors.New("usergate: device limit exceeded")

// ErrUnknownUser 表示用户已从面板删除但内核里还在（热更新窗口期）。
var ErrUnknownUser = errors.New("usergate: user not found")

// Meta 是 xray 侧传入的最小身份信息。
type Meta struct {
	Tag   string // inbound tag，等于节点标识
	Email string // 我们构建配置时写入的 "uid.uuid"
	IP    string // 已剥离 ::ffff: 前缀
}

// Limits 是单个用户在某节点上的有效限制。
type Limits struct {
	UID          int
	SpeedLimit   int // Mbps，0 = 不限
	DeviceLimit  int // 台，0 = 不限
	DynamicLimit int // Mbps，面板临时提速/降速
	ExpireAt     int64
}

// Gate 实现 vendored xray 里 app/usergate.Gate 接口。
type Gate struct {
	mu     sync.Mutex
	nodes  map[string]*nodeLimits // tag -> 节点级限制
	users  map[string]*userState  // tag+email -> 状态
	seenAt map[string]time.Time   // tag+email -> 最近一次 Admit
	sweepT time.Duration
	stop   chan struct{}
	once   sync.Once

	// onReject 在拒绝连接时回调（metrics 计数用），可为 nil。
	onReject func(err error)
}

type nodeLimits struct {
	speedLimit int // Mbps，节点级
}

type userState struct {
	uid   int
	limit Limits
	ips   map[string]time.Time // ip -> 最近活跃
}

func New() *Gate {
	g := &Gate{
		nodes:  make(map[string]*nodeLimits),
		users:  make(map[string]*userState),
		seenAt: make(map[string]time.Time),
		sweepT: 30 * time.Second,
		stop:   make(chan struct{}),
	}
	go g.sweeper()
	return g
}

// SetNodeLimit 设置节点级限速（Mbps）。
// OnReject 注册拒绝回调。只应设置一次（main 启动时），不做并发保护。
func (g *Gate) OnReject(fn func(error)) { g.onReject = fn }

// rejectHeld 是 Admit 内的版本：Admit 已持锁，这里只做回调，不再取锁。
func (g *Gate) rejectHeld(err error) error {
	if g.onReject != nil {
		g.onReject(err)
	}
	return err
}

func (g *Gate) reject(err error) error {
	if g.onReject != nil {
		g.onReject(err)
	}
	return err
}

func (g *Gate) SetNodeLimit(tag string, mbps int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[tag] = &nodeLimits{speedLimit: mbps}
}

// LoadUsers 全量装载某节点的用户（启动与配置重载时调用）。
func (g *Gate) LoadUsers(tag string, users []User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.users {
		if len(k) > len(tag) && k[:len(tag)+1] == tag+">" {
			delete(g.users, k)
		}
	}
	for _, u := range users {
		g.users[key(tag, u.Email)] = &userState{
			uid: u.UID,
			limit: Limits{
				UID:         u.UID,
				SpeedLimit:  u.SpeedLimit,
				DeviceLimit: u.DeviceLimit,
			},
			ips: make(map[string]time.Time, 4),
		}
	}
}

// UpdateUser 处理面板增删改（热更新）。
func (g *Gate) UpdateUser(tag string, added, deleted []User, modified []User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, u := range deleted {
		delete(g.users, key(tag, u.Email))
		delete(g.seenAt, key(tag, u.Email))
	}
	for _, u := range modified {
		if st, ok := g.users[key(tag, u.Email)]; ok {
			st.limit.SpeedLimit = u.SpeedLimit
			st.limit.DeviceLimit = u.DeviceLimit
			continue
		}
		g.users[key(tag, u.Email)] = &userState{uid: u.UID, limit: Limits{UID: u.UID, SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit}, ips: map[string]time.Time{}}
	}
	for _, u := range added {
		g.users[key(tag, u.Email)] = &userState{uid: u.UID, limit: Limits{UID: u.UID, SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit}, ips: map[string]time.Time{}}
	}
}

// SetDynamicLimit 面板下发的临时限速（到期自动失效）。
func (g *Gate) SetDynamicLimit(tag, email string, mbps int, expire time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.users[key(tag, email)]; ok {
		st.limit.DynamicLimit = mbps
		st.limit.ExpireAt = expire.Unix()
	}
}

// Admit 是数据面热路径：只做设备数与过期判定，必须是常数级复杂度。
func (g *Gate) Admit(ctx context.Context, tag, email, ip string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.users[key(tag, email)]
	if !ok {
		return g.rejectHeld(ErrUnknownUser)
	}
	now := time.Now()
	g.seenAt[key(tag, email)] = now

	dl := st.limit.DeviceLimit
	if dl <= 0 {
		st.ips[ip] = now
		return nil
	}
	if _, known := st.ips[ip]; known {
		st.ips[ip] = now
		return nil
	}
	live := 0
	for k, ts := range st.ips {
		if now.Sub(ts) > 2*time.Minute {
			delete(st.ips, k)
			continue
		}
		live++
	}
	if live >= dl {
		return g.rejectHeld(ErrDeviceLimit)
	}
	st.ips[ip] = now
	return nil
}

// Rate 返回用户在该节点的有效限速（字节/秒）；0 表示不限。
// 取节点级与用户级（含动态）中的非零最小值，与 v2node determineSpeedLimit 语义一致。
func (g *Gate) Rate(tag, email string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.users[key(tag, email)]
	if !ok {
		return 0
	}
	mbps := 0
	if nl, ok := g.nodes[tag]; ok {
		mbps = nl.speedLimit
	}
	ul := st.limit.SpeedLimit
	if st.limit.ExpireAt != 0 && time.Now().Unix() > st.limit.ExpireAt {
		st.limit.DynamicLimit = 0
		st.limit.ExpireAt = 0
	} else if st.limit.DynamicLimit > 0 {
		if ul == 0 {
			ul = st.limit.DynamicLimit
		} else if st.limit.DynamicLimit < ul {
			ul = st.limit.DynamicLimit
		}
	}
	mbps = minNonZero(mbps, ul)
	if mbps <= 0 {
		return 0
	}
	return int64(mbps) * 1000 * 1000 / 8
}

// Online 导出在线设备快照（供 alive 上报），不重置状态。
func (g *Gate) Online(tag string) []Online {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	out := make([]Online, 0, 64)
	for k, st := range g.users {
		if len(k) <= len(tag) || k[:len(tag)+1] != tag+">" {
			continue
		}
		for ip, ts := range st.ips {
			if now.Sub(ts) > 2*time.Minute {
				continue
			}
			out = append(out, Online{UID: st.uid, IP: ip})
		}
	}
	return out
}

type Online struct {
	UID int
	IP  string
}

type User struct {
	UID         int
	Email       string
	SpeedLimit  int
	DeviceLimit int
}

func key(tag, email string) string { return tag + ">" + email }

func minNonZero(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// Close 停掉后台清扫协程（进程退出与测试用）。
func (g *Gate) Close() {
	g.once.Do(func() { close(g.stop) })
}

func (g *Gate) sweeper() {
	t := time.NewTicker(g.sweepT)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}

		g.mu.Lock()
		now := time.Now()
		for k, ts := range g.seenAt {
			if now.Sub(ts) > 10*time.Minute {
				delete(g.seenAt, k)
			}
		}
		for _, st := range g.users {
			for ip, ts := range st.ips {
				if now.Sub(ts) > 5*time.Minute {
					delete(st.ips, ip)
				}
			}
		}
		g.mu.Unlock()
	}
}
