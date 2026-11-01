package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/vmihailenco/msgpack/v5"
)

// Client 实现 Xboard 的**节点侧 V2 契约**，路径与 payload 形状全部对照源码核过：
//
//	GET  /api/v2/server/config    -> 扁平 NodeConfig（+ 顶层 base_config），ETag/304
//	GET  /api/v2/server/user      -> {"users":[...]}，支持 X-Response-Format: msgpack
//	POST /api/v2/server/report    -> traffic/alive/online/status/metrics 五合一的合并上报
//	GET  /api/v2/server/handshake -> {"websocket":{"enabled":true,"ws_url":"wss://..."}}
//
// 鉴权：query `token` + `node_id`（middleware ServerV2.php）。面板忽略 node_type，我们不发。
// 机器模式：query `machine_id` + `token`，此时节点列表由 handshake 下发（M2 里程碑实现）。
type Client struct {
	base   string
	token  string
	node   int
	machid string
	http   *resty.Client

	mu       sync.RWMutex
	nodeETag string
	userETag string
	cache    *NodeConfig
}

func New(base string, nodeID int, token string, timeout time.Duration) *Client {
	c := resty.New().
		SetTimeout(timeout).
		SetRetryCount(2).
		SetHeader("User-Agent", "xboard-core/0.1 (+https://github.com/cedar2025/Xboard)")
	return &Client{base: strings.TrimRight(base, "/"), token: token, node: nodeID, http: c}
}

// NewMachine 返回机器模式客户端（一个 token 带多个节点）。
func NewMachine(base, machineID, token string, timeout time.Duration) *Client {
	c := New(base, 0, token, timeout)
	c.machid = machineID
	return c
}

func (c *Client) query() map[string]string {
	q := map[string]string{"token": c.token}
	if c.machid != "" {
		q["machine_id"] = c.machid
	} else {
		q["node_id"] = strconv.Itoa(c.node)
	}
	return q
}

// Tag 是内部分桶键：xray inbound tag、gate 的 tag、日志都用它。
// 面板不下发 tag，用节点 ID 派生即可稳定唯一。
func (c *Client) Tag() string {
	if c.machid != "" {
		return "machine-" + c.machid
	}
	return "node-" + strconv.Itoa(c.node)
}

// Config 拉节点配置。第二个返回值表示「配置真的变了」（面板 304 -> false）。
// ETag 命中 304 时返回缓存副本，调用方据此跳过重建内核——这是节点进程最贵的操作。
func (c *Client) Config(ctx context.Context) (*NodeConfig, bool, error) {
	r, err := c.http.R().
		SetContext(ctx).
		SetQueryParams(c.query()).
		SetHeader("If-None-Match", c.etag()).
		ForceContentType("application/json").
		Get(c.base + "/api/v2/server/config")
	if err != nil {
		return nil, false, fmt.Errorf("GET config: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.StatusCode() {
	case http.StatusNotModified:
		if c.cache == nil {
			return nil, false, fmt.Errorf("config: 304 without cache")
		}
		return c.cache, false, nil
	case http.StatusOK:
	default:
		return nil, false, fmt.Errorf("config: status %d body %s", r.StatusCode(), truncate(r.Body(), 256))
	}
	var out NodeConfig
	if err := json.Unmarshal(r.Body(), &out); err != nil {
		return nil, false, fmt.Errorf("config: decode: %w", err)
	}
	out.Raw = append(out.Raw[:0], r.Body()...)
	out.Tag = c.Tag()
	c.nodeETag = r.Header().Get("Etag")
	c.cache = &out
	return &out, true, nil
}

// Users 拉可用用户。返回 nil, nil 表示 304（没变）。
func (c *Client) Users(ctx context.Context) ([]UserInfo, error) {
	r, err := c.http.R().
		SetContext(ctx).
		SetQueryParams(c.query()).
		SetHeader("If-None-Match", c.etagUser()).
		SetHeader("X-Response-Format", "msgpack").
		SetDoNotParseResponse(true).
		Get(c.base + "/api/v2/server/user")
	if err != nil {
		return nil, fmt.Errorf("GET user: %w", err)
	}
	if r.RawResponse == nil {
		return nil, fmt.Errorf("user: empty response")
	}
	defer r.RawResponse.Body.Close()

	c.mu.Lock()
	c.userETag = r.Header().Get("Etag")
	c.mu.Unlock()

	switch r.StatusCode() {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("user: status %d", r.StatusCode())
	}

	// 面板只在 Accept 到位时才回 msgpack；这里两种都吃。
	var body UsersResponse
	if strings.Contains(r.Header().Get("Content-Type"), "msgpack") {
		if err := msgpack.NewDecoder(r.RawResponse.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("user: decode msgpack: %w", err)
		}
	} else if err := json.NewDecoder(r.RawResponse.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("user: decode json: %w", err)
	}
	return body.Users, nil
}

// Report 发合并上报。空字段会被 omitempty 去掉，面板按 is_array && !empty 逐类处理，
// 所以"只报流量"和"只报状态"都是合法调用——V2 相比 V1 的收益就在这里（一次 RTT）。
func (c *Client) Report(ctx context.Context, rep *Report) error {
	if rep == nil {
		return nil
	}
	if len(rep.Traffic) == 0 && len(rep.Alive) == 0 && len(rep.Online) == 0 && rep.Status == nil {
		return nil
	}
	r, err := c.http.R().
		SetContext(ctx).
		SetQueryParams(c.query()).
		SetBody(rep).
		Post(c.base + "/api/v2/server/report")
	if err != nil {
		return fmt.Errorf("POST report: %w", err)
	}
	if r.StatusCode() != http.StatusOK {
		return fmt.Errorf("report: status %d body %s", r.StatusCode(), truncate(r.Body(), 256))
	}
	return nil
}

// Handshake 拿 WS 推送通道。面板侧要求 server_ws_enable 打开且 NodeWorker 心跳存在，
// 否则 websocket.enabled=false —— 此时退回纯轮询，功能不受影响。
func (c *Client) Handshake(ctx context.Context) (*Handshake, error) {
	r, err := c.http.R().
		SetContext(ctx).
		SetQueryParams(c.query()).
		Get(c.base + "/api/v2/server/handshake")
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if r.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("handshake: status %d body %s", r.StatusCode(), truncate(r.Body(), 256))
	}
	var h Handshake
	if err := json.Unmarshal(r.Body(), &h); err != nil {
		return nil, fmt.Errorf("handshake: decode: %w", err)
	}
	return &h, nil
}

func (c *Client) etag() string     { c.mu.RLock(); defer c.mu.RUnlock(); return c.nodeETag }
func (c *Client) etagUser() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.userETag }

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
