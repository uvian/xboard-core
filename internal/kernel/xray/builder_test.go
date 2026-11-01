package xray

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xboard-ng/xboard-core/internal/panel"
)

// testPrivKey 是 32 字节 x25519 私钥的 base64url 编码（上游会调用 ecdh.NewPrivateKey 校验）。
const testPrivKey = "I1jE6ktIxSX3gbQ3czIa0lN7h1MEbTQDKM_e8f09wiA"

func realityNode(t *testing.T) *panel.NodeConfig {
	t.Helper()
	raw := map[string]any{
		"show":        false,
		"private_key": testPrivKey,
		"server_names": []string{
			"example.com",
		},
		"short_ids": []string{
			"0123abcd",
		},
		"dest": "example.com:443",
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &panel.NodeConfig{
		Protocol:    "vless",
		ListenIP:    "0.0.0.0",
		ServerPort:  443,
		Network:     "tcp",
		TLS:         2,
		Flow:        "xtls-rprx-vision",
		TLSSettings: json.RawMessage(b),
		Tag:         "node-1",
	}
}

func users() []panel.UserInfo {
	return []panel.UserInfo{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}}
}

// 正常 REALITY 节点必须能装配出一个 inbound，且必须带上 dispatcher（usergate 钩子载体）。
func TestBuildRealityInbound(t *testing.T) {
	cfg, err := Build(realityNode(t), users())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(cfg.Inbound) != 1 || cfg.Inbound[0].Tag != "node-1" {
		t.Fatalf("unexpected inbound: %+v", cfg.Inbound)
	}
	if len(cfg.Outbound) == 0 {
		t.Fatal("outbound missing")
	}
	var hasDispatcher, hasStats bool
	for _, app := range cfg.App {
		switch {
		case strings.Contains(app.Type, "dispatcher"):
			hasDispatcher = true
		case strings.Contains(app.Type, "stats"):
			hasStats = true
		}
	}
	if !hasDispatcher || !hasStats {
		t.Fatalf("app 列表缺 dispatcher/stats：dispatcher=%v stats=%v", hasDispatcher, hasStats)
	}
}

// h2 在 v26 已被移除，必须在构建期报错而不是让内核起不来。
func TestBuildRejectsRemovedNetwork(t *testing.T) {
	n := realityNode(t)
	n.Network = "h2"
	_, err := Build(n, users())
	if err == nil || !strings.Contains(err.Error(), "h2") {
		t.Fatalf("want h2 rejection, got %v", err)
	}
}

// REALITY 缺 shortIds 时上游会静默拒绝所有客户端，必须先报错。
func TestBuildRejectsRealityWithoutShortIDs(t *testing.T) {
	n := realityNode(t)
	b, _ := json.Marshal(map[string]any{"private_key": testPrivKey, "server_names": []string{"example.com"}, "dest": "example.com:443"})
	n.TLSSettings = b
	_, err := Build(n, users())
	if err == nil || !strings.Contains(err.Error(), "short_id") {
		t.Fatalf("want shortIds error, got %v", err)
	}
}

// 显式设置 minClientVer >= 26 会把 mihomo 系客户端（自报 1.8.2）全部打回 dest。
func TestBuildRejectsModernMinClientVer(t *testing.T) {
	n := realityNode(t)
	b, _ := json.Marshal(map[string]any{
		"private_key":  testPrivKey,
		"server_names": []string{"example.com"},
		"short_ids":    []string{"0123abcd"},
		"dest":         "example.com:443",
		"minClientVer": "26.3.27",
	})
	n.TLSSettings = b
	_, err := Build(n, users())
	if err == nil || !strings.Contains(err.Error(), "minClientVer") {
		t.Fatalf("want minClientVer error, got %v", err)
	}
}

// 1.0.0 是安全的下限写法，必须放行。
func TestBuildAcceptsLegacyMinClientVer(t *testing.T) {
	n := realityNode(t)
	b, _ := json.Marshal(map[string]any{
		"private_key":  testPrivKey,
		"server_names": []string{"example.com"},
		"short_ids":    []string{"0123abcd"},
		"dest":         "example.com:443",
		"minClientVer": "1.0.0",
	})
	n.TLSSettings = b
	if _, err := Build(n, users()); err != nil {
		t.Fatalf("1.0.0 must be accepted: %v", err)
	}
}

// "1.26.10" 逐段比较大于 (1,8,2)，只看主版本会把它当 1.x 放行 → 所有 mihomo 客户端被打回 dest。
func TestBuildRejectsSneakyMinClientVer(t *testing.T) {
	for _, v := range []string{"1.26.10", "2.0.0", "26.3.27"} {
		n := realityNode(t)
		b, _ := json.Marshal(map[string]any{
			"private_key":  testPrivKey,
			"server_names": []string{"example.com"},
			"short_ids":    []string{"0123abcd"},
			"dest":         "example.com:443",
			"minClientVer": v,
		})
		n.TLSSettings = b
		if _, err := Build(n, users()); err == nil {
			t.Fatalf("minClientVer=%s 应该被拒（它 >= mihomo 自报的 1.8.2）", v)
		}
	}
}

// allow_insecure 在 v26 已移除：面板老表单还带着时必须报错而不是静默丢弃。
func TestBuildRejectsAllowInsecure(t *testing.T) {
	n := &panel.NodeConfig{
		Protocol:    "vless",
		ListenIP:    "0.0.0.0",
		ServerPort:  8443,
		Network:     "tcp",
		TLS:         panel.SecurityTLS,
		TLSSettings: json.RawMessage(`{"allow_insecure":true,"cert_file":"/c.pem","key_file":"/k.pem"}`),
		Tag:         "node-1",
	}
	if _, err := Build(n, users()); err == nil || !strings.Contains(err.Error(), "allow_insecure") {
		t.Fatalf("want allow_insecure rejection, got %v", err)
	}
}

// 非 xray 处理的协议（anytls/tuic/hysteria2 归 sing-box）必须明确报错。
func TestBuildUnknownProtocol(t *testing.T) {
	n := realityNode(t)
	n.Protocol = "anytls"
	_, err := Build(n, users())
	if err == nil || !strings.Contains(err.Error(), "not handled") {
		t.Fatalf("want protocol rejection, got %v", err)
	}
}

// v26 的 freedom 对 vless 等入站默认禁止私网目标；只有显式打开才放行。
func TestFreedomBlocksPrivateByDefault(t *testing.T) {
	if got := freedomSettings(false); strings.Contains(got, "finalRules") {
		t.Fatalf("默认不应该写 finalRules，实际: %s", got)
	}
	if _, err := buildOutbounds(false); err != nil {
		t.Fatalf("默认出站应能构建: %v", err)
	}
}

func TestFreedomAllowsPrivateWhenEnabled(t *testing.T) {
	got := freedomSettings(true)
	if !strings.Contains(got, "finalRules") || !strings.Contains(got, "127.0.0.0/8") {
		t.Fatalf("打开后应写入 allow 规则，实际: %s", got)
	}
	// 必须真的能被 conf 解析，否则只是拼对了字符串。
	if _, err := buildOutbounds(true); err != nil {
		t.Fatalf("allow 规则没被上游接受: %v", err)
	}
}
