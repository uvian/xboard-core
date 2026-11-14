package xray

import (
	"encoding/json"
	"testing"

	"github.com/xboard-ng/xboard-core/internal/panel"
)

// REALITY 的 dest 是"校验失败就把 TLS 连接原样转过去"的目标。
// 它一旦指向内网，节点就变成任何扫描器都能借用的内网探测器。
func TestValidateRealityDestRejectsInternal(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"@xboard.sock",
		"//var/run/socket",
		"localhost:443",
		"LOCALHOST",
		"127.0.0.1:443",
		"10.1.2.3:443",
		"192.168.1.1",
		"172.16.0.9:8443",
		"169.254.169.254:80",
		"[::1]:443",
		"fe80::1",
	}
	for _, dest := range bad {
		if err := validateRealityDest(dest); err == nil {
			t.Fatalf("dest %q 应被拒绝，实际放行", dest)
		}
	}
}

func TestValidateRealityDestAcceptsPublic(t *testing.T) {
	good := []string{
		"www.microsoft.com:443",
		"swdist.apple.com",
		"1.1.1.1:443",
		"[2606:4700:4700::1111]:443",
	}
	for _, dest := range good {
		if err := validateRealityDest(dest); err != nil {
			t.Fatalf("dest %q 应被接受，实际报错 %v", dest, err)
		}
	}
}

// dest 缺省时的回落路径（面板老 schema 只给 server_name/server_port）也必须过校验：
// 否则管理员把 server_name 写成内网服务，dest 就会悄悄变成内网目标。
func TestBuildRealityDefaultDestValidated(t *testing.T) {
	raw := map[string]any{
		"show":         false,
		"private_key":  testPrivKey,
		"server_names": []string{"127.0.0.1"},
		"server_port":  443,
		"short_ids":    []string{"0123abcd"},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	n := &panel.NodeConfig{
		Protocol:    "vless",
		ListenIP:    "0.0.0.0",
		ServerPort:  443,
		Network:     "tcp",
		TLS:         2,
		Flow:        "xtls-rprx-vision",
		TLSSettings: json.RawMessage(b),
		Tag:         "node-1",
	}
	if _, err := Build(n, users()); err == nil {
		t.Fatal("server_name 回落成回环地址时应报错")
	}
}
