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
// logLevel 由进程启动时按配置设定（SetLogLevel）。默认 warning：生产环境不该把每条连接都打出来。
var logLevel = "warning"

// SetLogLevel 设置 xray 内核日志级别（debug/info/warning/error）。空值保持默认。
func SetLogLevel(level string) {
	if level != "" {
		logLevel = level
	}
}

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

// allowPrivateTargets 打开后，freedom 出站允许连私网/回环目标。
// 默认关闭：xray v26 对 vless/vmess/trojan/shadowsocks 入站内置了「禁止私网」的
// finalRule（防 SSRF / 防内网跳板），节点默认沿用这个安全姿势。
var allowPrivateTargets bool

// SetAllowPrivateTargets 由配置决定是否放行私网目标。
func SetAllowPrivateTargets(v bool) { allowPrivateTargets = v }

// strictClientVer 打开后，面板下发会打掉 mihomo 系客户端的 minClientVer 时直接拒绝启动。
// 默认关闭（只告警）：minClientVer 本身是 REALITY 的防降级旋钮，属于运维的正当选择；
// 但服务端对不满足门禁的连接是**静默**回落 dest 的，没有日志、没有错误，只有用户报「连不上」，
// 所以这道闸门在默认配置下必须喊出来。想把误配变成硬失败，就把它打开。
var strictClientVer bool

// SetStrictClientVer 由配置决定 minClientVer 越界时是报错还是告警。
func SetStrictClientVer(v bool) { strictClientVer = v }
