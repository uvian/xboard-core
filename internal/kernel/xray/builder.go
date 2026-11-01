package xray

import (
	"encoding/json"
	"fmt"

	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
)

// Build 把面板的扁平 NodeConfig 翻译成 xray 的 core.Config。
//
// 装配走上游 infra/conf（InboundDetourConfig / OutboundDetourConfig / StreamConfig），
// 而不是手搓 proto：v26 把传输与安全配置统一改成 TypedMessage 列表，字段级 API 变动频繁，
// conf 这层是上游自己维护的 JSON→proto 翻译器，跟着它升级成本最低，也仍然保有编译期检查。
//
// 安全默认集中在 buildStream / buildTLS / buildReality：面板下发什么都绕不过去
// （MPTCP 关闭、REALITY 前置校验、已删传输拦截、minClientVer 拒发）。
func Build(node *panel.NodeConfig, users []panel.UserInfo) (*core.Config, error) {
	if node == nil {
		return nil, fmt.Errorf("xray: empty node config")
	}
	inbound, err := buildInbound(node, users)
	if err != nil {
		return nil, err
	}
	outbound, err := buildOutbounds()
	if err != nil {
		return nil, err
	}
	return &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(logConfig("")),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(policyConfig()),
			serial.ToTypedMessage(&stats.Config{}),
			// 关键：必须显式塞 dispatcher 的 Config，vendored xray 的 usergate 钩子才会被实例化。
			serial.ToTypedMessage(&dispatcher.Config{}),
		},
		Inbound:  []*core.InboundHandlerConfig{inbound},
		Outbound: outbound,
	}, nil
}

func buildInbound(node *panel.NodeConfig, users []panel.UserInfo) (*core.InboundHandlerConfig, error) {
	port := node.ServerPort
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("xray: invalid server_port %d", port)
	}
	// 提前校验传输，避免把 h2 这类已删传输交给上游后只得到一条含混的错误。
	if _, err := normalizeNetwork(node.Network); err != nil {
		return nil, err
	}

	settings, err := buildSettings(node, users)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("xray: marshal settings: %w", err)
	}
	stream, err := buildStream(node)
	if err != nil {
		return nil, err
	}

	ic := &conf.InboundDetourConfig{
		Protocol: node.Protocol,
		// 只监听单端口；面板的 listen_ip 固定 0.0.0.0，这里不跟随，
		// 需要指定地址时走 conf.yaml 的高级覆盖（M3）。
		PortList:      &conf.PortList{Range: []conf.PortRange{{From: uint32(port), To: uint32(port)}}},
		Tag:           node.Tag,
		Settings:      (*json.RawMessage)(&raw),
		StreamSetting: stream,
	}
	handler, err := ic.Build()
	if err != nil {
		return nil, fmt.Errorf("xray: build inbound %s: %w", node.Protocol, err)
	}
	return handler, nil
}

// buildSettings 产出各协议的 settings JSON。accounts.go 只负责热更用户时的 protocol.User，
// 这里负责启动配置，两者共用 Email() 作为账号主键。
func buildSettings(node *panel.NodeConfig, users []panel.UserInfo) (map[string]any, error) {
	switch node.Protocol {
	case "vless":
		return buildVLESS(node, users)
	case "vmess":
		return buildVMess(node, users)
	case "trojan":
		return buildTrojan(users), nil
	case "shadowsocks":
		return buildShadowsocks(node, users)
	default:
		// anytls / tuic / hysteria2 由 sing-box 内核接管，不在这里硬凑。
		return nil, fmt.Errorf("xray: protocol %q is not handled by the xray kernel", node.Protocol)
	}
}

func buildVLESS(n *panel.NodeConfig, users []panel.UserInfo) (map[string]any, error) {
	clients := make([]any, 0, len(users))
	for _, u := range users {
		c := map[string]any{"id": u.UUID, "email": Email(u), "level": 0}
		if n.Flow != "" {
			// 面板只应下发 "xtls-rprx-vision"；别的值上游会在 inbound 启动时拒。
			c["flow"] = n.Flow
		}
		clients = append(clients, c)
	}
	// 面板的 decryption 来自 encryption.decryption；未启用时是 null -> 空串。
	decryption := "none"
	switch n.Decryption {
	case "", "none":
	case "mlkem768x25519plus":
		decryption = n.Decryption // v26.x 的 VLESS 抗量子加密
	default:
		return nil, fmt.Errorf("vless: unsupported decryption %q", n.Decryption)
	}
	return map[string]any{
		"clients":    clients,
		"decryption": decryption,
	}, nil
}

// buildVMess：注意面板 master 的 vmess 分支只下发 tls/tls_settings/multiplex，
// 根本没有 cipher 键（ServerService.php 的 vmess 组装 + Server.php 的 schema 都没有），
// 所以 n.Cipher 恒为空、security 走 0（auto）。保留读取是为了兼容自建面板的旧下发。
func buildVMess(n *panel.NodeConfig, users []panel.UserInfo) (map[string]any, error) {
	clients := make([]any, 0, len(users))
	for _, u := range users {
		c := map[string]any{"id": u.UUID, "email": Email(u), "level": 0}
		if n.Cipher != "" {
			c["security"] = n.Cipher
		}
		clients = append(clients, c)
	}
	return map[string]any{"clients": clients}, nil
}

func buildTrojan(users []panel.UserInfo) map[string]any {
	clients := make([]any, 0, len(users))
	for _, u := range users {
		clients = append(clients, map[string]any{"password": u.UUID, "email": Email(u), "level": 0})
	}
	return map[string]any{"clients": clients}
}

func buildShadowsocks(n *panel.NodeConfig, users []panel.UserInfo) (map[string]any, error) {
	if n.Cipher == "" || n.ServerKey == "" {
		return nil, fmt.Errorf("shadowsocks: cipher/server_key required")
	}
	clients := make([]any, 0, len(users))
	for _, u := range users {
		// shadowsocks-2022 多用户：password 是该用户自己的 PSK，email 仍走 Email() 以便统计。
		clients = append(clients, map[string]any{"email": Email(u), "password": u.UUID})
	}
	return map[string]any{
		"method":   n.Cipher,
		"password": n.ServerKey,
		"clients":  clients,
	}, nil
}

// buildOutbounds 只放必要项：默认直出 + block。
// 出站不做策略精调——节点是服务端，路由交给面板的 routes/custom_routes（M3）。
func buildOutbounds() ([]*core.OutboundHandlerConfig, error) {
	// freedom 的 domainStrategy 在 v26 已迁到 sockopt.domainStrategy；写在 freedom 里会告警并被自动迁移。
	out := make([]*core.OutboundHandlerConfig, 0, 2)
	for _, spec := range []struct {
		tag      string
		protocol string
		settings string
		sockopt  *conf.SocketConfig
	}{
		{tag: "direct", protocol: "freedom", settings: `{}`, sockopt: &conf.SocketConfig{DomainStrategy: "UseIP"}},
		{tag: "block", protocol: "blackhole", settings: `{}`},
	} {
		raw := json.RawMessage(spec.settings)
		oc := &conf.OutboundDetourConfig{Protocol: spec.protocol, Tag: spec.tag, Settings: &raw}
		if spec.sockopt != nil {
			oc.StreamSetting = &conf.StreamConfig{SocketSettings: spec.sockopt}
		}
		h, err := oc.Build()
		if err != nil {
			return nil, fmt.Errorf("xray: build outbound %s: %w", spec.tag, err)
		}
		out = append(out, h)
	}
	return out, nil
}
