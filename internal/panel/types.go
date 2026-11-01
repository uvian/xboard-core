package panel

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 Xboard 面板「节点侧契约」的唯一真相，逐项对照源码核过：
//
//   - 路由：app/Http/Routes/V2/ServerRoute.php —— V2 的 config/user/push/alive/status
//     复用 V1 的 UniProxyController，另有 report/handshake；机器模式走 server/machine/*。
//   - payload：app/Services/ServerService.php:251 buildNodeConfig() 返回**扁平**对象，
//     UniProxyController::config() 再补顶层 base_config。
//     => 没有 "common" 包装层；协议字段、传输字段、routes、cert_config 全在顶层。
//   - 鉴权：app/Http/Middleware/ServerV2.php —— query `token` + `node_id`；
//     面板**忽略** node_type（V1 的 node_type=v2board 是历史包袱）。
//   - 用户：getAvailableUsers() 只 select id/uuid/speed_limit/device_limit，
//     由 UniProxyController::user() 包成 {"users": [...]}，带 ETag/304。
//
// 面板 schema 的硬事实（app/Models/Server.php:326-372 castSettingsWithConfig）：
// protocol_settings 按 PROTOCOL_CONFIGURATIONS 常量做**白名单投射**，
// 不在 schema 里的键读写都会消失。所以 mldsa65Seed/xver/dest/short_ids 在后端只能占位，
// 必须由面板侧补 schema 才会出现（见 docs/PANEL-PATCH.md）。

const (
	SecurityNone    = 0
	SecurityTLS     = 1
	SecurityReality = 2
)

// NodeConfig 对应 GET /api/v2/server/config 的响应体（扁平）。
type NodeConfig struct {
	Protocol        string          `json:"protocol"`
	ListenIP        string          `json:"listen_ip"`
	ServerPort      int             `json:"server_port"`
	Network         string          `json:"network"`
	NetworkSettings json.RawMessage `json:"networkSettings"` // 面板是 camelCase，不是 network_settings
	TLS             int             `json:"tls"`             // 0 none / 1 tls / 2 reality
	TLSSettings     json.RawMessage `json:"tls_settings"`    // tls==2 时实为 reality_settings
	Decryption      string          `json:"decryption"`      // vless：encryption.decryption 的投影
	Flow            string          `json:"flow"`
	Cipher          string          `json:"cipher"`
	Plugin          string          `json:"plugin"`
	PluginOpts      string          `json:"plugin_opts"`
	ServerKey       string          `json:"server_key"`
	ServerName      string          `json:"server_name"`
	Multiplex       json.RawMessage `json:"multiplex"`
	UpMbps          int             `json:"up_mbps"`
	DownMbps        int             `json:"down_mbps"`
	Obfs            string          `json:"obfs"`
	ObfsPassword    string          `json:"obfs-password"`
	Congestion      string          `json:"congestion_control"`
	ZeroRTT         bool            `json:"zero_rtt_handshake"`
	PaddingScheme   json.RawMessage `json:"padding_scheme"`
	// 以下六项 xray 内核不用，但面板对 hysteria/tuic/mieru 会下发；
	// 留字段是为了将来切 sing-box 内核时不用再改契约层（见 docs/STATUS.md M2）。
	Host            string          `json:"host"`
	Version         int             `json:"version"`
	AuthTimeout     string          `json:"auth_timeout"`
	Heartbeat       string          `json:"heartbeat"`
	Transport       string          `json:"transport"`
	TrafficPattern  string          `json:"traffic_pattern"`
	CertConfig      *CertConfig     `json:"cert_config"`
	Routes          []Route         `json:"routes"`
	CustomOutbounds json.RawMessage `json:"custom_outbounds"`
	CustomRoutes    json.RawMessage `json:"custom_routes"`
	BaseConfig      *BaseConfig     `json:"base_config"`

	// Tag 由 Client 填（面板不下发），用作 xray inbound tag / gate 分桶键。
	Tag string          `json:"-"`
	Raw json.RawMessage `json:"-"`
}

// NodeInfo 是历史命名，保留别名免得调用点四处改签名。
type NodeInfo = NodeConfig

// RealitySettings 解析 tls==2 时的 tls_settings。
// 面板现有 schema 只有 server_name/server_port/public_key/private_key/short_id/allow_insecure；
// dest/short_ids/mldsa65Seed/xver 要等面板补 schema（P0-2）。
// FlexInt 容忍 "1" / 1 / null 三种写法。
//
// 面板侧 reality_settings.server_port 是 integer，xver 我们导出成 string，
// 老面板与第三方面板（v2node 用 json:"xver,string"）又可能反着来。
// 用固定类型去接，一定会有一边 unmarshal 失败：整条节点配置解析失败，
// 表现是"节点启动不了"，而不是某个字段缺失。
type FlexInt int64

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("expect integer, got %q", string(b))
	}
	*f = FlexInt(n)
	return nil
}

func (f FlexInt) Int() int64 { return int64(f) }

func (f FlexInt) String() string { return strconv.FormatInt(int64(f), 10) }

type RealitySettings struct {
	ServerName  string   `json:"server_name"`
	ServerNames []string `json:"server_names"`
	ServerPort  FlexInt  `json:"server_port"`
	Dest        string   `json:"dest"`
	PublicKey   string   `json:"public_key"`
	PrivateKey  string   `json:"private_key"`
	ShortID     string   `json:"short_id"`
	ShortIDs    []string `json:"short_ids"`
	Mldsa65Seed string   `json:"mldsa65Seed"`
	Xver        FlexInt  `json:"xver"`
	MaxTimeDiff int64    `json:"maxTimeDiff"`
	// 显式门禁：面板若下发就照办；默认必须为空（mihomo 系客户端自报 1.8.2）。
	MinClientVer string `json:"minClientVer,omitempty"`
	MaxClientVer string `json:"maxClientVer,omitempty"`
}

// EffectiveServerNames 合并单值/多值写法。
func (r RealitySettings) EffectiveServerNames() []string {
	return dedup(append([]string{r.ServerName}, r.ServerNames...))
}

// EffectiveShortIDs 合并 short_id / short_ids。
func (r RealitySettings) EffectiveShortIDs() []string {
	return dedup(append([]string{r.ShortID}, r.ShortIDs...))
}

// TLSCfg 是 tls==1 时的 tls_settings（hysteria/tuic/anytls 用内层 tls 对象）。
type TLSSettings struct {
	ServerName       string            `json:"server_name"`
	ServerNames      []string          `json:"server_names"`
	CertMode         string            `json:"cert_mode"`
	CertFile         string            `json:"cert_file"`
	KeyFile          string            `json:"key_file"`
	Provider         string            `json:"provider"`
	DNSEnv           map[string]string `json:"dns_env"`
	RejectUnknownSNI bool              `json:"reject_unknown_sni"`
	// ECH：面板 TLS_SETTINGS_CONFIGURATION 里有 ech 开关，启用后走这里。
	ECH           bool `json:"ech"`
	AllowInsecure bool `json:"allow_insecure"`
}

// CertConfig 来自 ServerService.php:384-392 追加的 cert_config，
// 兼容库里 mode / cert_mode 两种写法。
type CertConfig struct {
	CertMode string            `json:"cert_mode"`
	Mode     string            `json:"mode"`
	CertFile string            `json:"cert_file"`
	KeyFile  string            `json:"key_file"`
	Domain   string            `json:"domain"`
	Email    string            `json:"email"`
	Provider string            `json:"provider"`
	DNSEnv   map[string]string `json:"dns_env"`
}

func (c *CertConfig) EffectiveMode() string {
	if c == nil {
		return ""
	}
	if c.CertMode != "" {
		return c.CertMode
	}
	return c.Mode
}

type Route struct {
	ID          int      `json:"id"`
	Match       []string `json:"match"`
	Action      string   `json:"action"`
	ActionValue any      `json:"action_value"`
}

type BaseConfig struct {
	PushInterval any `json:"push_interval"`
	PullInterval any `json:"pull_interval"`
}

// Reality 解析 tls_settings（tls==2）。
func (n *NodeConfig) Reality() (*RealitySettings, error) {
	var rs RealitySettings
	if len(n.TLSSettings) == 0 {
		return &rs, nil
	}
	if err := json.Unmarshal(n.TLSSettings, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// TLSCfg 解析 tls_settings（tls==1）。
func (n *NodeConfig) TLSCfg() (*TLSSettings, error) {
	var ts TLSSettings
	if len(n.TLSSettings) == 0 {
		return &ts, nil
	}
	if err := json.Unmarshal(n.TLSSettings, &ts); err != nil {
		return nil, err
	}
	return &ts, nil
}

// UserInfo 是 getAvailableUsers() 的 select 列，一个字段都不多。
type UserInfo struct {
	ID          int    `json:"id" msgpack:"id"`
	UUID        string `json:"uuid" msgpack:"uuid"`
	SpeedLimit  int    `json:"speed_limit" msgpack:"speed_limit"`
	DeviceLimit int    `json:"device_limit" msgpack:"device_limit"`
}

// UsersResponse 是 UniProxyController::user() 的包装。
type UsersResponse struct {
	Users []UserInfo `json:"users"`
}

// Report 是 POST /api/v2/server/report 的请求体（V2 把四类上报合并成一次）。
// 形状严格对齐 processTraffic/processAlive/processOnline/processStatus/updateMetrics。
type Report struct {
	Traffic map[int][2]int64 `json:"traffic,omitempty"` // {uid: [up, down]}
	Alive   map[int][]string `json:"alive,omitempty"`   // {uid: [ip...]}
	Online  map[int]int      `json:"online,omitempty"`  // {uid: 连接数}
	Status  *LoadStatus      `json:"status,omitempty"`
	Metrics *Metrics         `json:"metrics,omitempty"`
}

// LoadStatus 是 UniProxyController::status() 的必填字段（少一个就 422）。
type LoadStatus struct {
	CPU  float64 `json:"cpu"`
	Mem  Usage   `json:"mem"`
	Swap Usage   `json:"swap"`
	Disk Usage   `json:"disk"`
	// KernelStatus 与 Metrics.KernelStatus 同义；面板 processStatus 读的是 status 顶层这个键。
	KernelStatus bool `json:"kernel_status"`
}

type Usage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

// Metrics 是面板节点详情页的扩展指标（全部可选）。
type Metrics struct {
	Uptime            int64  `json:"uptime,omitempty"`
	Goroutines        int    `json:"goroutines,omitempty"`
	ActiveConnections int    `json:"active_connections,omitempty"`
	TotalConnections  int    `json:"total_connections,omitempty"`
	TotalUsers        int    `json:"total_users,omitempty"`
	ActiveUsers       int    `json:"active_users,omitempty"`
	InboundSpeed      int64  `json:"inbound_speed,omitempty"`
	OutboundSpeed     int64  `json:"outbound_speed,omitempty"`
	KernelStatus      bool   `json:"kernel_status"`
	Kernel            string `json:"kernel,omitempty"`
	Version           string `json:"version,omitempty"`
}

// Handshake 是 GET /api/v2/server/handshake 的响应：面板通知节点 WS 推送能力。
type Handshake struct {
	Websocket struct {
		Enabled bool   `json:"enabled"`
		WSURL   string `json:"ws_url"`
	} `json:"websocket"`
}

// Event 是 WS 推送事件（app/WebSocket/NodeEventHandlers.php）。
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

const (
	EventSyncConfig    = "sync.config"
	EventSyncUsers     = "sync.users"
	EventSyncUserDelta = "sync.user.delta"
	EventSyncDevices   = "sync.devices"
)

func dedup(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
