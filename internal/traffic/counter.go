package traffic

import (
	"sync"
	"sync/atomic"
)

// Collector 维护 per-user 增量计数，上报后归零。
// 数据来源优先用内核 stats（xray 的 user>>>email>>>traffic>>>{uplink,downlink}），
// 取不到时退回本地计数。
// 输出形状直接对齐面板 V2 的 `traffic: {uid: [up, down]}`，省掉一次中间转换。
type Collector struct {
	mu  sync.Mutex
	acc map[int]*pair
}

type pair struct {
	up   atomic.Int64
	down atomic.Int64
}

func NewCollector() *Collector { return &Collector{acc: make(map[int]*pair)} }

func (c *Collector) Add(uid int, up, down int64) {
	c.mu.Lock()
	p, ok := c.acc[uid]
	if !ok {
		p = &pair{}
		c.acc[uid] = p
	}
	c.mu.Unlock()
	if up != 0 {
		p.up.Add(up)
	}
	if down != 0 {
		p.down.Add(down)
	}
}

// Drain 取出并清零全部增量；返回 nil 表示本轮无需上报。
func (c *Collector) Drain() map[int][2]int64 {
	c.mu.Lock()
	local := c.acc
	c.acc = make(map[int]*pair, len(local))
	c.mu.Unlock()

	if len(local) == 0 {
		return nil
	}
	out := make(map[int][2]int64, len(local))
	for uid, p := range local {
		up, down := p.up.Swap(0), p.down.Swap(0)
		if up == 0 && down == 0 {
			continue
		}
		out[uid] = [2]int64{up, down}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Pending 返回是否有待上报数据（省一次空请求）。
func (c *Collector) Pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.acc {
		if p.up.Load() != 0 || p.down.Load() != 0 {
			return true
		}
	}
	return false
}
