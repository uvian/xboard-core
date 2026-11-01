package singbox

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xboard-ng/xboard-core/internal/panel"
)

// Build 把面板的节点配置翻译成 sing-box 的 config.json。
//
// 只做 inbound + 直连 outbound，不碰路由：节点是"哑"转发器，分流逻辑在面板侧下发，
// 这里多一层 route 只会让排障更难。
//
// 面板字段映射（app/Services/ServerService.php:280-380）：
//   - hysteria/v2：up_mbps / down_mbps / obfs + obfs-password
//   - tuic：congestion_control / auth_timeout / zero_rtt_handshake / heartbeat
//   - anytls：padding_scheme
//   - 三种都必须有 TLS（sing-box 侧 tls.enabled=true），证书来自 cert_config 或 tls_settings
func Build(node *panel.NodeConfig, users []panel.UserInfo) (map[string]any, error) {
	if node == nil {
		return nil, fmt.Errorf("singbox: nil node")
	}
	proto := normalize(node.Protocol)
	if _, ok := protocols[proto]; !ok {
		return nil, fmt.Errorf("singbox: 不支持协议 %q", node.Protocol)
	}
	if len(users) == 0 {
		// 没有用户时 sing-box 会照常起监听但谁都连不上，不如下游 gate 一样直接报错，
		// 免得运维对着"端口通了但全失败"排查。
		return nil, fmt.Errorf("singbox: 没有可用用户，拒绝启动空节点")
	}

	tlsCfg, err := buildTLS(node)
	if err != nil {
		return nil, err
	}

	listen := node.ListenIP
	if listen == "" {
		listen = "0.0.0.0"
	}
	tag := node.Tag
	if tag == "" {
		tag = "xboard-in"
	}

	in := map[string]any{
		"type":        inboundType(proto),
		"tag":         tag,
		"listen":      listen,
		"listen_port": node.ServerPort,
		"tls":         tlsCfg,
	}

	switch proto {
	case "anytls":
		in["users"] = anytlsUsers(users)
		if scheme := paddingScheme(node.PaddingScheme); len(scheme) > 0 {
			in["padding_scheme"] = scheme
		}

	case "tuic":
		in["users"] = tuicUsers(users)
		in["congestion_control"] = orDefault(node.Congestion, "bbr")
		in["auth_timeout"] = orDefault(node.AuthTimeout, "3s")
		in["zero_rtt_handshake"] = node.ZeroRTT
		if node.Heartbeat != "" {
			in["heartbeat"] = node.Heartbeat
		}

	case "hysteria2":
		in["users"] = passwordUsers(users)
		if node.UpMbps > 0 {
			in["up_mbps"] = node.UpMbps
		}
		if node.DownMbps > 0 {
			in["down_mbps"] = node.DownMbps
		}
		if obfs := strings.TrimSpace(node.Obfs); obfs != "" && node.ObfsPassword != "" {
			// 面板的 obfs 值是 salamander（hysteria2 唯一实现）；不认的名字直接忽略，
			// 因为 sing-box 会因此拒绝整份 config。
			if strings.EqualFold(obfs, "salamander") {
				in["obfs"] = map[string]any{"type": "salamander", "password": node.ObfsPassword}
			}
		}
	}

	return map[string]any{
		"log": map[string]any{
			"level":     "warn",
			"timestamp": true,
		},
		"inbounds": []any{in},
		"outbounds": []any{
			map[string]any{"type": "direct", "tag": "direct"},
		},
	}, nil
}

func inboundType(proto string) string {
	if proto == "hysteria" {
		// 面板老版本把 hysteria2 写成 hysteria；sing-box 侧的 hysteria 是 v1 协议，
		// 但 Xboard 的 hysteria 现在统一是 v2 参数集，按 hysteria2 处理。
		return "hysteria2"
	}
	return proto
}

func anytlsUsers(users []panel.UserInfo) []any {
	out := make([]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"name":     userName(u),
			"password": u.UUID,
		})
	}
	return out
}

func tuicUsers(users []panel.UserInfo) []any {
	out := make([]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"name":     userName(u),
			"uuid":     u.UUID,
			"password": u.UUID,
		})
	}
	return out
}

func passwordUsers(users []panel.UserInfo) []any {
	out := make([]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"name":     userName(u),
			"password": u.UUID,
		})
	}
	return out
}

// userName 用 id.uuid 和 xray 侧的 email 保持同一格式，日志里能直接对上人。
func userName(u panel.UserInfo) string {
	return fmt.Sprintf("%d.%s", u.ID, u.UUID)
}

func paddingScheme(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func buildTLS(node *panel.NodeConfig) (map[string]any, error) {
	certFile, keyFile, serverName := "", "", node.ServerName
	if cc := node.CertConfig; cc != nil {
		certFile, keyFile = cc.CertFile, cc.KeyFile
		if cc.Mode == "" && cc.CertMode != "" {
			certFile, keyFile = cc.CertFile, cc.KeyFile
		}
	}
	if ts, err := node.TLSCfg(); err == nil && ts != nil {
		if certFile == "" {
			certFile = ts.CertFile
		}
		if keyFile == "" {
			keyFile = ts.KeyFile
		}
		if serverName == "" {
			serverName = ts.ServerName
		}
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("singbox: TLS 协议（anytls/tuic/hysteria2）缺少证书：cert_config.cert_file/key_file 或 tls_settings")
	}
	tls := map[string]any{
		"enabled":          true,
		"certificate_path": certFile,
		"key_path":         keyFile,
	}
	if serverName != "" {
		tls["server_name"] = serverName
	}
	return tls, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
