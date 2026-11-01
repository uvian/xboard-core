package xray

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xtls/xray-core/infra/conf"
)

// 传输层白名单：只放行 v26.10.31 仍在、且我们真的会用的四种 + tcp。
// h2/http/h3/quic 在上游已被移除，这里报硬错而不是静默降级——
// 静默降级会把"面板还在发 h2、节点以为跑在 h2"变成线上连接被拒且无人知情。
var removedNetworks = map[string]string{
	"h2":   "已被 Xray-core v26.x 移除，请改用 xhttp（stream-one）",
	"http": "已被 Xray-core v26.x 移除，请改用 xhttp（stream-one）",
	"h3":   "已被 Xray-core v26.x 移除，请改用 xhttp（stream-one）",
	"quic": "已被 Xray-core v26.x 移除，请改用 xhttp 或交给 sing-box 内核",
}

// normalizeNetwork 把面板/其它后端的历史写法归一到 v26 的名字。
func normalizeNetwork(n string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(n)) {
	case "", "raw", "tcp":
		return "tcp", nil
	case "ws", "websocket":
		return "ws", nil
	case "grpc":
		return "grpc", nil
	case "httpupgrade", "http-upgrade":
		return "httpupgrade", nil
	case "xhttp", "splithttp", "split-http":
		// v26 里两者是同一个传输，面板的 splithttp 一律按 xhttp 处理。
		return "xhttp", nil
	}
	if reason, bad := removedNetworks[strings.ToLower(strings.TrimSpace(n))]; bad {
		return "", fmt.Errorf("xray: 传输 %q %s", n, reason)
	}
	return "", fmt.Errorf("xray: 不支持的传输 %q", n)
}

// networkSettings 是面板 networkSettings 的宽容解析目标（camelCase）。
// 面板对 ws/grpc/xhttp 都塞同一个对象，字段按需取，不用严格 schema。
type networkSettings struct {
	Path        string            `json:"path"`
	Host        string            `json:"host"`
	Headers     map[string]string `json:"headers"`
	ServiceName string            `json:"serviceName"`
	Mode        string            `json:"mode"`
}

func decodeNetworkSettings(raw json.RawMessage) networkSettings {
	var ns networkSettings
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &ns) // 面板给什么就解析什么，坏字段退化为默认值
	}
	if ns.Path == "" {
		ns.Path = "/"
	}
	if ns.Headers == nil {
		ns.Headers = map[string]string{}
	}
	return ns
}

// buildStream 产出 conf.StreamConfig。
//
// 刻意走上游 infra/conf 而不是手搓 proto：v26 把安全/传输配置统一挪进
// SecuritySettings/TransportSettings 的 TypedMessage，字段级 API 每年都在变；
// conf 这层是上游自己维护的 JSON→proto 翻译器，跟着它走升级成本最低。
func buildStream(node *panel.NodeConfig) (*conf.StreamConfig, error) {
	network, err := normalizeNetwork(node.Network)
	if err != nil {
		return nil, err
	}
	tp := conf.TransportProtocol(network)
	sc := &conf.StreamConfig{
		Network: &tp,
		// 安全默认集中在这里，面板下发什么都绕不过去：
		// MPTCP 关（CVE-2026-98432）、不接 PROXY protocol（避免伪造源 IP 绕过设备数）、
		// TFO 关（服务端 TFO cookie 是额外指纹，且对 REALITY 无收益）。
		SocketSettings: &conf.SocketConfig{
			AcceptProxyProtocol: false,
			TcpMptcp:            false,
			TFO:                 false,
		},
	}

	ns := decodeNetworkSettings(node.NetworkSettings)
	switch network {
	case "ws":
		sc.WSSettings = &conf.WebSocketConfig{Path: ns.Path, Host: ns.Host, Headers: ns.Headers}
	case "httpupgrade":
		sc.HTTPUPGRADESettings = &conf.HttpUpgradeConfig{Path: ns.Path, Host: ns.Host, Headers: ns.Headers}
	case "grpc":
		sc.GRPCSettings = &conf.GRPCConfig{ServiceName: ns.ServiceName}
	case "xhttp":
		sc.XHTTPSettings = &conf.SplitHTTPConfig{Path: ns.Path, Host: ns.Host, Headers: ns.Headers, Mode: ns.Mode}
	case "tcp":
		// tcp 不需要额外参数；REALITY / Vision 都跑在裸 tcp 上。
	}

	switch node.TLS {
	case panel.SecurityNone:
		sc.Security = "none"
	case panel.SecurityTLS:
		sc.Security = "tls"
		tls, err := buildTLS(node)
		if err != nil {
			return nil, err
		}
		sc.TLSSettings = tls
	case panel.SecurityReality:
		sc.Security = "reality"
		reality, err := buildReality(node)
		if err != nil {
			return nil, err
		}
		sc.REALITYSettings = reality
	default:
		return nil, fmt.Errorf("xray: 未知的 security 取值 %d（0 none / 1 tls / 2 reality）", node.TLS)
	}
	return sc, nil
}

// buildTLS 组装服务端证书。
//
// 证书来源有两处：cert_config（面板的证书管理/ACME）优先，其次 tls_settings 里的手工路径。
// v26 已删除 allowInsecure（那是客户端语义），所以这里不做任何"忽略证书校验"的翻译。
func buildTLS(node *panel.NodeConfig) (*conf.TLSConfig, error) {
	ts, err := node.TLSCfg()
	if err != nil {
		return nil, fmt.Errorf("xray: 解析 tls_settings: %w", err)
	}
	// allow_insecure 是客户端语义，v26 已从服务端配置移除（上游直接 PrintRemovedFeatureError）。
	// 面板老表单可能还留着这个开关，显式报错比静默忽略安全：否则运维以为"放宽了校验"其实没生效。
	if ts.AllowInsecure {
		return nil, fmt.Errorf("xray: allow_insecure 在 v26 已被移除，请改用 pinnedPeerCertSha256 / verifyPeerCertByName")
	}
	certFile, keyFile := ts.CertFile, ts.KeyFile
	if cc := node.CertConfig; cc != nil {
		if cc.CertFile != "" {
			certFile = cc.CertFile
		}
		if cc.KeyFile != "" {
			keyFile = cc.KeyFile
		}
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("xray: tls 已启用但缺少证书（cert_config.cert_file/key_file 或 tls_settings）")
	}
	return &conf.TLSConfig{
		Certs:            []*conf.TLSCertConfig{{CertFile: certFile, KeyFile: keyFile}},
		ServerName:       ts.ServerName,
		RejectUnknownSNI: ts.RejectUnknownSNI,
	}, nil
}

// buildReality 组装 REALITY 服务端配置，并在下发前做前置校验。
//
// serverNames/shortIds/privateKey 缺一个，REALITY 会启动即失败或直接回落到 dest；
// 与其让面板里出现"能连但一直断"的节点，不如启动时就报清楚。
//
// minClientVer 是这里最要紧的一条：mihomo 系客户端（FlClash / Clash Verge Rev / Nikki）
// 在 ClientHello 里自报 1.8.2，服务端一旦下发 >= 26.x 的门禁，它们会被**静默**打回 dest。
// 所以这里默认不下发；面板显式要求 >= 26.x 时直接报错，让人回面板改，而不是线上悄悄全灭。
func buildReality(node *panel.NodeConfig) (*conf.REALITYConfig, error) {
	rs, err := node.Reality()
	if err != nil {
		return nil, fmt.Errorf("xray: 解析 reality_settings: %w", err)
	}
	names := rs.EffectiveServerNames()
	shortIDs := rs.EffectiveShortIDs()
	if rs.PrivateKey == "" {
		return nil, fmt.Errorf("xray: REALITY 缺少 private_key")
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("xray: REALITY 缺少 server_name（决定回落目标与 SNI 白名单）")
	}
	if len(shortIDs) == 0 {
		return nil, fmt.Errorf("xray: REALITY 缺少 short_id/short_ids")
	}
	if v := rs.MinClientVer; v != "" {
		if atLeast(v, 1, 8, 2) > 0 {
			return nil, fmt.Errorf("xray: minClientVer=%s 会拒绝所有 mihomo 系客户端（它们自报 1.8.2），"+
				"请在面板里清空或设为 1.0.0", v)
		}
	}
	if seed := rs.Mldsa65Seed; seed != "" && seed == rs.PrivateKey {
		return nil, fmt.Errorf("xray: mldsa65Seed 不能与 private_key 相同")
	}

	// dest 缺省时按 server_name:server_port 回落；面板老 schema 不下发 dest。
	dest := rs.Dest
	if dest == "" {
		port := rs.ServerPort.Int()
		if port == 0 {
			port = 443
		}
		dest = fmt.Sprintf("%s:%d", names[0], port)
	}
	destJSON, err := json.Marshal(dest)
	if err != nil {
		return nil, fmt.Errorf("xray: REALITY dest: %w", err)
	}

	return &conf.REALITYConfig{
		Show:         false,
		Dest:         destJSON,
		Xver:         uint64(rs.Xver.Int()),
		ServerNames:  names,
		PrivateKey:   rs.PrivateKey,
		ShortIds:     shortIDs,
		Mldsa65Seed:  rs.Mldsa65Seed,
		MaxTimeDiff:  uint64(rs.MaxTimeDiff),
		MinClientVer: rs.MinClientVer,
		MaxClientVer: rs.MaxClientVer,
	}, nil
}

// atLeast 逐段比较 v 与 want(a,b,c)，返回 -1 / 0 / 1。
//
// 必须逐段比而不是只看主版本：上游 transport_security.go 把 minClientVer 解析成 3 个字节，
// 再与客户端 ClientHello 里的 (x,y,z) 逐字节比较。所以 "1.26.10" 主版本是 1，实际却大于
// mihomo 自报的 (1,8,2) —— 用主版本判断会放行一个必杀配置。
func atLeast(v string, want ...int) int {
	parts := strings.Split(strings.TrimSpace(v), ".")
	got := make([]int, 3)
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			return 0 // 解析不了就当没设：宁可放宽，也不误杀客户端
		}
		got[i] = n
	}
	for i := 0; i < 3; i++ {
		if got[i] != want[i] {
			if got[i] > want[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// majorVersion 从 "26.3.27" 里取主版本号，格式不对就当没写（交给上游判）。
func majorVersion(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	dot := strings.IndexByte(v, '.')
	if dot <= 0 {
		return 0, false
	}
	n := 0
	for _, r := range v[:dot] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
