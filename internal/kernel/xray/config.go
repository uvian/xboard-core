package xray

import (
	"time"

	applog "github.com/xtls/xray-core/app/log"
	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/infra/conf"
)

// logConfig 统一节点日志策略：只出 warning 以上，且默认不写 access log。
//
// 节点是常驻进程，access log 会随连接数线性增长，把磁盘写满比丢日志危险得多；
// 需要排障时把配置里的 log.level 调到 debug（main.go 透传）。
func logConfig(level string) *applog.Config {
	if level == "" {
		level = "warning"
	}
	return (&conf.LogConfig{LogLevel: level}).Build()
}

// policyConfig 是所有 inbound 共用的 level-0 策略。
//
// 四个数字来自 Xboard-Node 的生产取值（Handshake 4s / ConnIdle 120s），
// 不要随手改：Handshake 太大给了慢速握手探测更多时间，太小会误杀高延迟移动端。
func policyConfig() *policy.Config {
	return &policy.Config{
		Level: map[uint32]*policy.Policy{
			0: {
				Timeout: &policy.Policy_Timeout{
					Handshake:      second(4 * time.Second),
					ConnectionIdle: second(120 * time.Second),
					UplinkOnly:     second(2 * time.Second),
					DownlinkOnly:   second(4 * time.Second),
				},
				// 用户级上下行统计必须开：gate / usergate 的限速与面板流量上报都靠它。
				Stats: &policy.Policy_Stats{
					UserUplink:   true,
					UserDownlink: true,
				},
				Buffer: &policy.Policy_Buffer{Connection: 128},
			},
		},
	}
}

// second 是 policy 里唯一的时长单位（秒），向上取整避免 0 秒把连接直接掐掉。
func second(d time.Duration) *policy.Second {
	v := uint32((d + time.Second - 1) / time.Second)
	return &policy.Second{Value: v}
}
