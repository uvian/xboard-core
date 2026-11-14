package xray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xboard-ng/xboard-core/internal/panel"
)

// 面板里配了规则但节点不执行，是"兼容"里最坏的一种：面板上看得见、线上不生效。
func TestRoutingCompilesPanelRoutes(t *testing.T) {
	n := realityNode(t)
	n.Routes = []panel.Route{
		{ID: 1, Match: []string{"ads.example.com"}, Action: "block"},
		{ID: 2, Match: []string{"10.0.0.0/8", "192.168.0.0/16"}, Action: "direct"},
		{ID: 3, Match: []string{"*.landing.example.com"}, Action: "proxy", ActionValue: "landing"},
	}
	n.CustomOutbounds = json.RawMessage(`[{"tag":"landing","protocol":"freedom","settings":{}}]`)

	cfg, err := Build(n, users())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 6 个固定 app + router；router 必须在，否则规则一条都不会被评估。
	if len(cfg.App) != 7 {
		t.Fatalf("配了 routes 应当注册 router（app 数 7），实际 %d", len(cfg.App))
	}
	// 出站顺序：direct / block / landing（默认出口仍是第一个，不能被自定义出站顶掉）。
	if len(cfg.Outbound) != 3 {
		t.Fatalf("出站数应为 3（direct/block/自定义），实际 %d", len(cfg.Outbound))
	}
}

func TestRoutingDomainAndIPSplit(t *testing.T) {
	n := realityNode(t)
	n.Routes = []panel.Route{
		{ID: 1, Match: []string{"example.com", "*.foo.com", "just-a-domain.test", "1.2.3.0/24", "5.6.0.0/16"}, Action: "direct"},
	}
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}
	cfg, err := buildRouting(n, known, true)
	if err != nil {
		t.Fatalf("buildRouting: %v", err)
	}
	if cfg == nil || len(cfg.RuleList) != 2 {
		t.Fatalf("域名与 IP 应拆成两条规则，实际 %v", cfg)
	}
	var dom rawRouteRule
	if err := json.Unmarshal(cfg.RuleList[0], &dom); err != nil {
		t.Fatal(err)
	}
	if len(dom.Domain) != 3 {
		t.Fatalf("域名规则数量不对：%+v", dom.Domain)
	}
	if dom.Domain[0] != "domain:example.com" || dom.Domain[1] != "domain:foo.com" {
		t.Fatalf("域名规则不对：%+v", dom.Domain)
	}
	var ipRule rawRouteRule
	if err := json.Unmarshal(cfg.RuleList[1], &ipRule); err != nil {
		t.Fatal(err)
	}
	if len(ipRule.IP) != 2 || ipRule.IP[0] != "1.2.3.0/24" {
		t.Fatalf("IP 规则不对：%+v", ipRule.IP)
	}
	if dom.OutboundTag != outboundTagDirect || ipRule.OutboundTag != outboundTagDirect {
		t.Fatalf("action=direct 应指向 direct 出站：%+v %+v", dom, ipRule)
	}
}

func TestRoutingProxyNeedsExistingOutbound(t *testing.T) {
	n := realityNode(t)
	n.Routes = []panel.Route{{ID: 7, Match: []string{"example.com"}, Action: "proxy", ActionValue: "nope"}}
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}
	if _, err := buildRouting(n, known, true); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("指向不存在的出站必须报错，得到 %v", err)
	}
}

func TestRoutingProxyWithoutValueFails(t *testing.T) {
	n := realityNode(t)
	n.Routes = []panel.Route{{ID: 8, Match: []string{"example.com"}, Action: "proxy"}}
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}
	if _, err := buildRouting(n, known, true); err == nil {
		t.Fatal("action=proxy 但没有 action_value 必须报错")
	}
}

func TestRoutingUnknownActionFails(t *testing.T) {
	n := realityNode(t)
	n.Routes = []panel.Route{{ID: 9, Match: []string{"example.com"}, Action: "reject-everything"}}
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}
	if _, err := buildRouting(n, known, true); err == nil {
		t.Fatal("未知 action 必须报错（面板改语义时要被人看见）")
	}
}

func TestRoutingEmptyMatchIsSkipped(t *testing.T) {
	n := realityNode(t)
	n.Routes = []panel.Route{{ID: 10, Match: nil, Action: "block"}}
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}

	// allow_private=true 时没有其它规则就该完全不注册 router。
	cfg, err := buildRouting(n, known, true)
	if err != nil {
		t.Fatalf("空 match 应跳过而不是报错：%v", err)
	}
	if cfg != nil {
		t.Fatalf("没有有效规则时不该注册 router，得到 %+v", cfg)
	}

	// allow_private=false 时剩下的只有那条私网兜底规则。
	cfg, err = buildRouting(n, known, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || len(cfg.RuleList) != 1 {
		t.Fatalf("allow_private=false 时必须保留私网兜底规则，得到 %+v", cfg)
	}
	if cfg.DomainStrategy == nil || *cfg.DomainStrategy != "IpIfNonMatch" {
		t.Fatalf("有私网兜底时应使用 IpIfNonMatch，得到 %v", cfg.DomainStrategy)
	}
}

func TestRoutingCustomRoutesPassthrough(t *testing.T) {
	n := realityNode(t)
	n.CustomRoutes = json.RawMessage(`[{"type":"field","domain":["domain:ads.example"],"outboundTag":"block"}]`)
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}
	cfg, err := buildRouting(n, known, true)
	if err != nil {
		t.Fatalf("custom_routes 透传失败：%v", err)
	}
	if cfg == nil || len(cfg.RuleList) != 1 {
		t.Fatalf("custom_routes 应原样进规则表，得到 %+v", cfg)
	}

	// 缺 outboundTag / 指向不存在的出站都要拦下来：xray 运行时会静默丢弃这种规则。
	for _, bad := range []string{
		`[{"type":"field","domain":["domain:a"]}]`,
		`[{"type":"field","domain":["domain:a"],"outboundTag":"ghost"}]`,
	} {
		n.CustomRoutes = json.RawMessage(bad)
		if _, err := buildRouting(n, known, true); err == nil {
			t.Fatalf("应当拒绝 %s", bad)
		}
	}
}

func TestCustomOutboundsValidation(t *testing.T) {
	// v26 已移除 outbound 的 proxySettings，写了会被上游报错；我们先给出可操作的提示。
	n := realityNode(t)
	n.CustomOutbounds = json.RawMessage(`[{"tag":"x","protocol":"freedom","proxySettings":{"tag":"direct"}}]`)
	if _, err := buildCustomOutbounds(n); err == nil || !strings.Contains(err.Error(), "dialerProxy") {
		t.Fatalf("proxySettings 应提示改用 dialerProxy，得到 %v", err)
	}

	n.CustomOutbounds = json.RawMessage(`[{"protocol":"freedom"}]`)
	if _, err := buildCustomOutbounds(n); err == nil {
		t.Fatal("缺 tag 必须报错")
	}
}

// 节点不随包分发 geodata：面板规则一旦用了 geosite:/geoip:，上游会在构建 router 时因为
// 打不开 dat 文件整份配置失败，错误信息还埋在 infra/conf 里。这里必须在 Build 之前拦下并说清怎么修。
func TestGeoAssetsGuard(t *testing.T) {
	t.Setenv("XRAY_LOCATION_ASSET", t.TempDir())

	if err := checkGeoAssets([]string{"geosite:category-ads"}); err == nil ||
		!strings.Contains(err.Error(), "geosite.dat") || !strings.Contains(err.Error(), "XRAY_LOCATION_ASSET") {
		t.Fatalf("缺 geosite.dat 时应给出可操作提示，得到 %v", err)
	}
	if err := checkGeoAssets([]string{"geoip:cn"}); err == nil ||
		!strings.Contains(err.Error(), "geoip.dat") {
		t.Fatalf("缺 geoip.dat 时应给出可操作提示，得到 %v", err)
	}
	if err := checkGeoAssets([]string{"example.com", "1.2.3.0/24"}); err != nil {
		t.Fatalf("不用 geo 规则时不该报错：%v", err)
	}

	dir := t.TempDir()
	t.Setenv("XRAY_LOCATION_ASSET", dir)
	for _, name := range []string{"geosite.dat", "geoip.dat"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkGeoAssets([]string{"geosite:cn", "geoip:cn"}); err != nil {
		t.Fatalf("数据文件就位后不该报错：%v", err)
	}
}

// 上游的默认私网拦截只对 vless/vmess 生效（Name 字段只有这两个协议会赋值），
// trojan / shadowsocks 入站默认可以打到 127.0.0.1、内网段和云元数据地址。
// 这里是那条兜底规则的守门测试。
func TestPrivateTargetsBlockedByDefault(t *testing.T) {
	SetAllowPrivateTargets(false)
	defer SetAllowPrivateTargets(false)

	cfg, err := Build(realityNode(t), users())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.App) != 7 {
		t.Fatalf("默认应注册 router 放私网兜底规则（app 数 7），实际 %d", len(cfg.App))
	}

	// 运维显式允许私网目标时不该再注册 router：节点被当内网入口是运营者自己的决定。
	SetAllowPrivateTargets(true)
	cfg, err = Build(realityNode(t), users())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.App) != 6 {
		t.Fatalf("allow_private_targets=true 时不该注册 router（app 数 6），实际 %d", len(cfg.App))
	}
}

func TestPrivateTargetRuleShape(t *testing.T) {
	SetAllowPrivateTargets(false)
	defer SetAllowPrivateTargets(false)

	n := realityNode(t)
	known := map[string]bool{outboundTagDirect: true, outboundTagBlock: true}
	cfg, err := buildRouting(n, known, false)
	if err != nil {
		t.Fatal(err)
	}
	var first rawRouteRule
	if err := json.Unmarshal(cfg.RuleList[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.OutboundTag != outboundTagBlock {
		t.Fatalf("私网兜底规则必须指向 block，得到 %q", first.OutboundTag)
	}
	want := map[string]bool{"127.0.0.0/8": true, "10.0.0.0/8": true, "169.254.0.0/16": true, "::1/128": true}
	for _, ip := range first.IP {
		delete(want, ip)
	}
	if len(want) != 0 {
		t.Fatalf("私网兜底规则缺段：%v（实际 %v）", want, first.IP)
	}
}
