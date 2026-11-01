package singbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xboard-ng/xboard-core/internal/gate"
	"github.com/xboard-ng/xboard-core/internal/panel"
)

func certNode(proto string) *panel.NodeConfig {
	return &panel.NodeConfig{
		Protocol:    proto,
		ListenIP:    "0.0.0.0",
		ServerPort:  8443,
		Tag:         "node-9",
		TLSSettings: json.RawMessage(`{"server_name":"example.com","cert_file":"/c.pem","key_file":"/k.pem"}`),
	}
}

func us() []panel.UserInfo {
	return []panel.UserInfo{{ID: 1, UUID: "uuid-a"}, {ID: 2, UUID: "uuid-b"}}
}

func inboundOf(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	in, ok := cfg["inbounds"].([]any)
	if !ok || len(in) != 1 {
		t.Fatalf("inbounds 形状不对: %#v", cfg["inbounds"])
	}
	m, ok := in[0].(map[string]any)
	if !ok {
		t.Fatalf("inbound 不是对象: %#v", in[0])
	}
	return m
}

func TestBuildAnytls(t *testing.T) {
	n := certNode("anytls")
	n.PaddingScheme = json.RawMessage(`["stop=8","0=30-30"]`)
	cfg, err := Build(n, us())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	in := inboundOf(t, cfg)
	if in["type"] != "anytls" || in["listen_port"] != 8443 || in["tag"] != "node-9" {
		t.Fatalf("inbound 基础字段错: %#v", in)
	}
	users := in["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("用户数 %d", len(users))
	}
	first := users[0].(map[string]any)
	if first["password"] != "uuid-a" || first["name"] != "1.uuid-a" {
		t.Fatalf("用户字段错: %#v", first)
	}
	scheme := in["padding_scheme"].([]string)
	if len(scheme) != 2 || scheme[0] != "stop=8" {
		t.Fatalf("padding_scheme 没透传: %#v", in["padding_scheme"])
	}
	tls := in["tls"].(map[string]any)
	if tls["enabled"] != true || tls["certificate_path"] != "/c.pem" || tls["server_name"] != "example.com" {
		t.Fatalf("tls 段错: %#v", tls)
	}
}

func TestBuildTuicDefaults(t *testing.T) {
	n := certNode("tuic")
	cfg, err := Build(n, us())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	in := inboundOf(t, cfg)
	if in["type"] != "tuic" {
		t.Fatalf("type=%v", in["type"])
	}
	// 面板没下发时要落到 sing-box 能接受的默认值，否则 config 直接被拒。
	if in["congestion_control"] != "bbr" || in["auth_timeout"] != "3s" || in["zero_rtt_handshake"] != false {
		t.Fatalf("tuic 默认值错: %#v", in)
	}
	u := in["users"].([]any)[0].(map[string]any)
	if u["uuid"] != "uuid-a" || u["password"] != "uuid-a" {
		t.Fatalf("tuic 用户字段错: %#v", u)
	}
}

func TestBuildHysteria2ObfsAndBandwidth(t *testing.T) {
	n := certNode("hysteria2")
	n.UpMbps, n.DownMbps = 100, 500
	n.Obfs, n.ObfsPassword = "salamander", "pw"
	cfg, err := Build(n, us())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	in := inboundOf(t, cfg)
	if in["up_mbps"] != 100 || in["down_mbps"] != 500 {
		t.Fatalf("带宽没透传: %#v", in)
	}
	obfs := in["obfs"].(map[string]any)
	if obfs["type"] != "salamander" || obfs["password"] != "pw" {
		t.Fatalf("obfs 段错: %#v", obfs)
	}
}

func TestBuildIgnoresUnknownObfs(t *testing.T) {
	n := certNode("hysteria2")
	n.Obfs, n.ObfsPassword = "not-a-real-obfs", "pw"
	cfg, err := Build(n, us())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 认不出的 obfs 必须整段丢掉：写进去 sing-box 会拒掉整份 config，
	// 结果是"节点起不来"，比"没有 obfs"严重得多。
	if _, ok := inboundOf(t, cfg)["obfs"]; ok {
		t.Fatal("未知 obfs 不该写进 config")
	}
}

func TestBuildHysteriaAliasMapsToV2(t *testing.T) {
	n := certNode("hysteria")
	cfg, err := Build(n, us())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if inboundOf(t, cfg)["type"] != "hysteria2" {
		t.Fatalf("hysteria 应映射成 hysteria2: %v", inboundOf(t, cfg)["type"])
	}
}

func TestBuildRejects(t *testing.T) {
	cases := []struct {
		name string
		node *panel.NodeConfig
		user []panel.UserInfo
		want string
	}{
		{"无证书", &panel.NodeConfig{Protocol: "tuic", ServerPort: 443}, us(), "缺少证书"},
		{"未知协议", certNode("vless"), us(), "不支持协议"},
		{"空用户", certNode("anytls"), nil, "没有可用用户"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Build(c.node, c.user); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望包含 %q 的错误，得到 %v", c.want, err)
			}
		})
	}
}

// fakeProcess 记录启停次数，替代真的 sing-box 进程。
type fakeProcess struct {
	starts, stops int
	startErr      error
}

func (f *fakeProcess) Start() error  { f.starts++; return f.startErr }
func (f *fakeProcess) Stop() error   { f.stops++; return nil }
func (f *fakeProcess) Running() bool { return f.starts > f.stops }

func withFakes(t *testing.T, ok bool) (*fakeProcess, func()) {
	t.Helper()
	fp := &fakeProcess{}
	oldNew, oldWait := newProcess, waitFor
	newProcess = func(string, []string, string) process { return fp }
	waitFor = func(context.Context, string, int, time.Duration) error {
		if ok {
			return nil
		}
		return context.DeadlineExceeded
	}
	return fp, func() { newProcess, waitFor = oldNew, oldWait }
}

func TestStartWritesConfigAndRestartsWithUsers(t *testing.T) {
	fp, restore := withFakes(t, true)
	defer restore()

	dir := t.TempDir()
	g := gate.New()
	defer g.Close()
	n := New(g, Options{Binary: "sing-box", WorkDir: dir})

	ctx := context.Background()
	if err := n.Start(ctx, certNode("anytls"), us()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cfgPath := filepath.Join(dir, "node-9.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config 没落盘: %v", err)
	}
	if !strings.Contains(string(raw), "uuid-a") {
		t.Fatalf("config 里没有用户: %s", raw)
	}
	if fp.starts != 1 {
		t.Fatalf("进程启动 %d 次", fp.starts)
	}

	// 用户变化 → 重写 config + 重启（sing-box 无法热更用户）。
	newUser := []panel.UserInfo{{ID: 3, UUID: "uuid-c"}}
	if err := n.UpdateUsers(ctx, newUser, nil, nil); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	if fp.starts != 2 || fp.stops != 1 {
		t.Fatalf("期望重启（starts=%d stops=%d）", fp.starts, fp.stops)
	}
	// added 是"新增"，老的必须还在（删人走 deleted 参数）。
	raw, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), "uuid-c") || !strings.Contains(string(raw), "uuid-a") {
		t.Fatalf("重启后的 config 没带上新用户: %s", raw)
	}
	if strings.Count(string(raw), `"password"`) != 3 {
		t.Fatalf("期望 3 个用户，得到 %s", raw)
	}

	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestUpdateUsersNoopWhenUnchanged(t *testing.T) {
	fp, restore := withFakes(t, true)
	defer restore()
	n := New(gate.New(), Options{WorkDir: t.TempDir()})
	ctx := context.Background()
	if err := n.Start(ctx, certNode("tuic"), us()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 同名同 uuid 的"变更"不该触发重启（面板常见重复推同一批）。
	if err := n.UpdateUsers(ctx, []panel.UserInfo{{ID: 1, UUID: "uuid-a"}}, nil, nil); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	if fp.starts != 1 {
		t.Fatalf("不该重启，实际启动 %d 次", fp.starts)
	}
}

func TestStartFailsWhenPortNeverOpens(t *testing.T) {
	fp, restore := withFakes(t, false)
	defer restore()
	n := New(gate.New(), Options{WorkDir: t.TempDir()})
	err := n.Start(context.Background(), certNode("anytls"), us())
	if err == nil || !strings.Contains(err.Error(), "未就绪") {
		t.Fatalf("期望端口未就绪的错误，得到 %v", err)
	}
	if fp.stops != 1 {
		t.Fatal("端口没起来要把半死的进程收掉")
	}
}

func TestSupported(t *testing.T) {
	n := New(gate.New(), Options{})
	for _, p := range []string{"anytls", "tuic", "hysteria2", "Hysteria", " hy2 "} {
		if !n.Supported(p) {
			t.Fatalf("%q 应支持", p)
		}
	}
	for _, p := range []string{"vless", "vmess", "trojan", "shadowsocks", ""} {
		if n.Supported(p) {
			t.Fatalf("%q 不该由 sing-box 承载", p)
		}
	}
}
