package xray

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xtls/xray-core/infra/conf"
)

// 面板的路由规则（v2_server_route）与 xray 的 routing 规则不是同一套 schema：
// 面板给的是 {match:[...], action, action_value}，xray 要的是 {domain/ip/port, outboundTag}。
// 这里做的是翻译，不是替代——面板里配了"广告域名走 block""某些 IP 走自建出口"，
// 节点必须照做，否则面板上看得见、实际不生效，这是最糟的一种"兼容"。
//
// 规则统一用原生 JSON 拼：conf.RouterConfig.RuleList 就是 []json.RawMessage，
// 上游自己解析成 router.Rule；custom_routes（面板的原始 JSON 逃生口）也正好同构，可原样透传。
type rawRouteRule struct {
	Type        string   `json:"type"`
	Domain      []string `json:"domain,omitempty"`
	IP          []string `json:"ip,omitempty"`
	Port        string   `json:"port,omitempty"`
	OutboundTag string   `json:"outboundTag"`
}

// outboundTagDirect / outboundTagBlock 是 buildOutbounds 里固定产出的两个 tag。
const (
	outboundTagDirect = "direct"
	outboundTagBlock  = "block"
)

// buildRouting 把面板 routes + custom_routes 编译成 router 配置。
//
// 即使面板一条规则都没配，只要 allowPrivate 为假也要注册 router：我们在这里放一条
// "私网/回环/云元数据 → block" 的兜底规则。原因是上游的默认私网拦截不可靠——
// proxy/freedom 的 getDefaultFinalRule 靠 session.Inbound.Name 判定，而只有 vless/vmess
// 的 inbound 会给这个字段赋值（proxy/vless/inbound/inbound.go:534、proxy/vmess/inbound/inbound.go:272），
// trojan / shadowsocks 入站的 Name 是空串，于是默认规则恒为 nil，私网目标直接放行。
// 节点一旦被当成内网跳板（尤其云上 169.254.169.254 的实例凭证接口），后果比限速失效严重得多。
func buildRouting(node *panel.NodeConfig, knownOutbounds map[string]bool, allowPrivate bool) (*conf.RouterConfig, error) {
	var rules []json.RawMessage

	if !allowPrivate {
		rules = append(rules, mustRule(rawRouteRule{
			Type:        "field",
			IP:          privateTargets,
			OutboundTag: outboundTagBlock,
		}))
	}

	for _, r := range node.Routes {
		tag, err := routeTarget(r)
		if err != nil {
			return nil, err
		}
		if tag == "" {
			continue // match 为空 = 面板里这条规则没写完，跳过而不是报错
		}
		if tag != outboundTagDirect && tag != outboundTagBlock && !knownOutbounds[tag] {
			return nil, fmt.Errorf("xray: 路由规则 #%d 指向不存在的出站 %q（可用：%s）",
				r.ID, tag, outboundNames(knownOutbounds))
		}
		if err := checkGeoAssets(r.Match); err != nil {
			return nil, err
		}
		rules = append(rules, compilePanelRoute(r, tag)...)
	}

	// custom_routes 是逃生口：运维直接写 xray 原生规则，我们只做透传与形状校验。
	if raw := strings.TrimSpace(string(node.CustomRoutes)); raw != "" && raw != "null" {
		var extra []json.RawMessage
		if err := json.Unmarshal(node.CustomRoutes, &extra); err != nil {
			return nil, fmt.Errorf("xray: custom_routes 必须是 xray 原生规则数组: %w", err)
		}
		for i, r := range extra {
			var probe struct {
				OutboundTag string `json:"outboundTag"`
			}
			if err := json.Unmarshal(r, &probe); err != nil {
				return nil, fmt.Errorf("xray: custom_routes[%d] 不是合法 JSON: %w", i, err)
			}
			if probe.OutboundTag == "" {
				return nil, fmt.Errorf("xray: custom_routes[%d] 缺少 outboundTag", i)
			}
			if !knownOutbounds[probe.OutboundTag] {
				return nil, fmt.Errorf("xray: custom_routes[%d] 指向不存在的出站 %q（可用：%s）",
					i, probe.OutboundTag, outboundNames(knownOutbounds))
			}
			rules = append(rules, r)
		}
	}

	if len(rules) == 0 {
		return nil, nil
	}
	// 面板配了规则就一定要在日志里留下痕迹：出问题时运维第一件事就是确认"规则到底进没进内核"。
	slog.Info("xray: routing enabled", "rules", len(rules),
		"panel_routes", len(node.Routes), "allow_private", allowPrivate)
	// IpIfNonMatch 只在域名没被规则命中时才解析域名，用于让上面的私网兜底也覆盖"域名指向内网"的情况。
	// 不需要兜底时保持 AsIs：少一次解析、少一次 DNS 泄漏。
	ds := "AsIs"
	if !allowPrivate {
		ds = "IpIfNonMatch"
	}
	return &conf.RouterConfig{RuleList: rules, DomainStrategy: &ds}, nil
}

// routeTarget 把面板的 action/action_value 翻译成 xray 的 outboundTag。
//
// 面板只有三种语义（见 app/Models/ServerRoute.php 的 admin 表单）：
//
//	block（默认）→ block 出站；direct → direct 出站；proxy → action_value 指定的自定义出站 tag。
func routeTarget(r panel.Route) (string, error) {
	if len(r.Match) == 0 {
		return "", nil
	}
	switch strings.ToLower(strings.TrimSpace(r.Action)) {
	case "", "block":
		return outboundTagBlock, nil
	case "direct":
		return outboundTagDirect, nil
	case "proxy", "route":
		v := strings.TrimSpace(fmt.Sprint(r.ActionValue))
		if v == "" || v == "<nil>" {
			return "", fmt.Errorf("xray: 路由规则 #%d 的 action=proxy 但 action_value 为空", r.ID)
		}
		return v, nil
	default:
		return "", fmt.Errorf("xray: 路由规则 #%d 的 action=%q 不认识（支持 block/direct/proxy）", r.ID, r.Action)
	}
}

// compilePanelRoute 把一条面板规则拆成 xray 规则（域名与 IP 分开，便于命中不同阶段）。
//
// match 项的写法约定与 Xboard-Node 一致：
//   - "geoip:cn"、含 "/" 的 CIDR、纯 IP → ip
//   - "geosite:category-ads" → domain
//   - 其它（含 "*.example.com"）→ domain: 后缀匹配
func compilePanelRoute(r panel.Route, tag string) []json.RawMessage {
	var domains, ips []string
	for _, item := range r.Match {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		switch {
		case strings.HasPrefix(item, "geoip:") || strings.Contains(item, "/"):
			ips = append(ips, item)
		case strings.HasPrefix(item, "geosite:"):
			domains = append(domains, item)
		default:
			domains = append(domains, "domain:"+strings.TrimPrefix(item, "*."))
		}
	}

	var out []json.RawMessage
	if len(domains) > 0 {
		out = append(out, mustRule(rawRouteRule{Type: "field", Domain: domains, OutboundTag: tag}))
	}
	if len(ips) > 0 {
		out = append(out, mustRule(rawRouteRule{Type: "field", IP: ips, OutboundTag: tag}))
	}
	return out
}

func mustRule(r rawRouteRule) json.RawMessage {
	b, err := json.Marshal(r)
	if err != nil {
		// 只由常量与字符串拼接而成，不可能失败；真失败了也不该把节点带崩。
		return json.RawMessage(`{"type":"field","outboundTag":"` + outboundTagBlock + `"}`)
	}
	return b
}

func outboundNames(m map[string]bool) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return strings.Join(names, "/")
}

// checkGeoAssets 处理 geosite:/geoip: 这类"需要数据文件"的匹配项。
//
// 上游 conf.Build() 会因为打开不了 geosite.dat 而整份配置构建失败，错误信息长这样：
// `infra/conf: invalid field rule > common/geodata: failed to open geosite.dat`。
// 运维看到这句基本无法定位，所以这里提前检查、给出能照着做的提示。
//
// 我们不随包分发 geodata（几十 MB，且更新策略与面板无关）；需要 geo 规则的人自己放文件，
// 目录按上游约定走 XRAY_LOCATION_ASSET，找不到再退回可执行文件目录。
func checkGeoAssets(match []string) error {
	needGeoip, needGeosite := false, false
	for _, item := range match {
		item = strings.TrimSpace(item)
		switch {
		case strings.HasPrefix(item, "geosite:"):
			needGeosite = true
		case strings.HasPrefix(item, "geoip:"):
			needGeoip = true
		}
	}
	if needGeosite && !assetExists("geosite.dat") {
		return fmt.Errorf("xray: 面板路由规则用了 geosite:，但节点没有 geosite.dat；" +
			"把数据文件放到 XRAY_LOCATION_ASSET 目录（或可执行文件同目录），或把规则改成具体域名")
	}
	if needGeoip && !assetExists("geoip.dat") {
		return fmt.Errorf("xray: 面板路由规则用了 geoip:，但节点没有 geoip.dat；" +
			"把数据文件放到 XRAY_LOCATION_ASSET 目录（或可执行文件同目录），或把规则改成具体 CIDR")
	}
	return nil
}

// assetExists 按上游的查找顺序判断 geodata 文件在不在：XRAY_LOCATION_ASSET → 可执行文件目录 → 当前目录。
func assetExists(name string) bool {
	var dirs []string
	if env := strings.TrimSpace(os.Getenv("XRAY_LOCATION_ASSET")); env != "" {
		dirs = append(dirs, env)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, ".")
	for _, dir := range dirs {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// buildCustomOutbounds 解析面板的 custom_outbounds（原始 xray outbound 数组）。
//
// 面板允许运维给节点加自定义出站（例如给"proxy"路由指向的落地机），后端必须能落地，
// 否则 action=proxy 的规则永远指向不存在的 tag。形状与 xray 原生 outbounds 一致。
func buildCustomOutbounds(node *panel.NodeConfig) ([]*conf.OutboundDetourConfig, error) {
	raw := strings.TrimSpace(string(node.CustomOutbounds))
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var items []*conf.OutboundDetourConfig
	if err := json.Unmarshal(node.CustomOutbounds, &items); err != nil {
		return nil, fmt.Errorf("xray: custom_outbounds 必须是 xray 原生 outbound 数组: %w", err)
	}
	for i, ob := range items {
		if ob == nil {
			return nil, fmt.Errorf("xray: custom_outbounds[%d] 是 null", i)
		}
		if ob.Tag == "" {
			return nil, fmt.Errorf("xray: custom_outbounds[%d] 缺少 tag（路由规则要靠 tag 指向它）", i)
		}
		if ob.ProxySettings != nil {
			return nil, fmt.Errorf("xray: custom_outbounds[%d](%s) 使用了已移除的 proxySettings，"+
				"请改用 streamSettings.sockopt.dialerProxy", i, ob.Tag)
		}
		if ob.Protocol == "" {
			return nil, fmt.Errorf("xray: custom_outbounds[%d](%s) 缺少 protocol", i, ob.Tag)
		}
	}
	return items, nil
}
