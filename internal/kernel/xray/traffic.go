package xray

import (
	"github.com/xtls/xray-core/features/stats"
)

// SampleTraffic 返回「自上次采样以来」每个用户 id 的增量流量。
//
// xray 的 stats 计数是累计值，直接上报会让面板重复计费，所以这里对同一
// email 保存上一次读数并做差；内核重启后计数归零，差值为负时按 0 处理。
func (n *Node) SampleTraffic() map[int][2]int64 {
	sm := n.Stats()
	if sm == nil {
		return nil
	}

	n.mu.RLock()
	users := n.users
	prev := n.lastTraffic
	n.mu.RUnlock()

	next := make(map[string][2]int64, len(users))
	out := make(map[int][2]int64, len(users))
	for _, u := range users {
		email := Email(u)
		up := counterValue(sm, "user>>>"+email+">>>traffic>>>uplink")
		down := counterValue(sm, "user>>>"+email+">>>traffic>>>downlink")
		next[email] = [2]int64{up, down}

		p := prev[email]
		du, dd := up-p[0], down-p[1]
		if du < 0 {
			du = 0
		}
		if dd < 0 {
			dd = 0
		}
		if du == 0 && dd == 0 {
			continue
		}
		out[u.ID] = [2]int64{du, dd}
	}

	n.mu.Lock()
	n.lastTraffic = next
	n.mu.Unlock()
	return out
}

func counterValue(sm stats.Manager, name string) int64 {
	c := sm.GetCounter(name)
	if c == nil {
		return 0
	}
	return c.Value()
}
