package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

	kaOnce sync.Once

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
func NewMachine(base, machineID string, nodeID int, token string, timeout time.Duration) *Client {
	c := New(base, 0, token, timeout)
	c.machid = machineID
	return c
}

func (c *Client) query() map[string]string {
	// 面板 V2 中间件除 handshake 外都要求 node_id；machine_id 只是额外的机器身份。
	q := map[string]string{"token": c.token, "node_id": strconv.Itoa(c.node)}
	if c.machid != "" {
		q["machine_id"] = c.machid
	}
	return q
}

// Tag 是内部分桶键：xray inbound tag、gate 的 tag、日志都用它。
// 面板不下发 tag，用节点 ID 派生即可稳定唯一。
// 面板常挂在 nginx + PHP-FPM 后面，keep-alive 空闲超时后服务端会直接发 RST，
// 客户端复用这条连接就会拿到 "wsarecv: An existing connection was forcibly closed"。
// resty 自带的 RetryCount 只覆盖 5xx 与连接建立失败，不覆盖"连接复用到一半被对端重置"，
// 所以这里显式做一次连接层重试：识别出重置 → 关掉空闲连接 → 后续改走短连接。
func (c *Client) do(ctx context.Context, method, path string, prep func(*resty.Request) *resty.Request) (*resty.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req := prep(c.http.R().SetContext(ctx).SetQueryParams(c.query()))
		var r *resty.Response
		if method == http.MethodGet {
			r, lastErr = req.Get(c.base + path)
		} else {
			r, lastErr = req.Post(c.base + path)
		}
		if lastErr == nil {
			return r, nil
		}
		if !isConnReset(lastErr) {
			return nil, lastErr
		}
		c.disableKeepAlive()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 150 * time.Millisecond):
		}
	}
	return nil, lastErr
}

// isConnReset 判断是不是"连接层"错误（值得换一条新连接重试），而不是业务层错误。
func isConnReset(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"forcibly closed", "wsarecv", "connection reset", "econnreset",
		"server closed idle connection", "unexpected eof", "broken pipe", "eof",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// disableKeepAlive 关掉池里的空闲连接，并让后续请求不再复用（幂等）。
func (c *Client) disableKeepAlive() {
	c.kaOnce.Do(func() {
		if tr, ok := c.http.GetClient().Transport.(*http.Transport); ok {
			tr.DisableKeepAlives = true
		}
	})
	c.http.GetClient().CloseIdleConnections()
}

func (c *Client) Tag() string {
	if c.machid != "" {
		return "machine-" + c.machid
	}
	return "node-" + strconv.Itoa(c.node)
}

// Config 拉节点配置。第二个返回值表示「配置真的变了」（面板 304 -> false）。
// ETag 命中 304 时返回缓存副本，调用方据此跳过重建内核——这是节点进程最贵的操作。
func (c *Client) Config(ctx context.Context) (*NodeConfig, bool, error) {
	r, err := c.do(ctx, http.MethodGet, "/api/v2/server/config", func(req *resty.Request) *resty.Request {
		return req.SetHeader("If-None-Match", c.etag()).ForceContentType("application/json")
	})
	if err != nil {
		return nil, false, fmt.Errorf("GET config: %w", redact(err))
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

// AdoptConfig 把 WS 推送来的 sync.config 载荷解成 NodeConfig，走和 REST 完全相同的
// 归一化（Tag、Raw）。面板推送用的是同一个 buildNodeConfig，所以不需要第二套解析。
func (c *Client) AdoptConfig(raw []byte) (*NodeConfig, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("config: 空载荷")
	}
	var out NodeConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("config: decode push: %w", err)
	}
	out.Raw = append(out.Raw[:0], raw...)
	out.Tag = c.Tag()
	return &out, nil
}

// Users 拉可用用户。返回 nil, nil 表示 304（没变）。
//
// 面板 master 的 user() 直接 response($response)，并不看 X-Response-Format；
// 早期社区后端用的 msgpack 头部在 PHP 侧没有任何读取点。这里发 Accept 而非伪造头，
// 解码仍按响应 Content-Type 分派，将来面板真上 msgpack 也不用改代码。
func (c *Client) Users(ctx context.Context) ([]UserInfo, error) {
	r, err := c.do(ctx, http.MethodGet, "/api/v2/server/user", func(req *resty.Request) *resty.Request {
		return req.SetHeader("If-None-Match", c.etagUser()).
			SetHeader("Accept", "application/x-msgpack, application/json").
			SetDoNotParseResponse(true)
	})
	if err != nil {
		return nil, fmt.Errorf("GET user: %w", redact(err))
	}
	if r.RawResponse == nil {
		return nil, fmt.Errorf("user: empty response")
	}
	defer r.RawResponse.Body.Close()

	switch r.StatusCode() {
	case http.StatusNotModified:
		// 304 响应不带 ETag；这里必须保留旧值，否则下一轮会全量拉取。
		return nil, nil
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("user: status %d", r.StatusCode())
	}
	if et := r.Header().Get("Etag"); et != "" {
		c.mu.Lock()
		c.userETag = et
		c.mu.Unlock()
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
	r, err := c.do(ctx, http.MethodPost, "/api/v2/server/report", func(req *resty.Request) *resty.Request {
		return req.SetBody(rep)
	})
	if err != nil {
		return fmt.Errorf("POST report: %w", redact(err))
	}
	if r.StatusCode() != http.StatusOK {
		return fmt.Errorf("report: status %d body %s", r.StatusCode(), truncate(r.Body(), 256))
	}
	return nil
}

// Handshake 拿 WS 推送通道。面板侧要求 server_ws_enable 打开且 NodeWorker 心跳存在，
// 否则 websocket.enabled=false —— 此时退回纯轮询，功能不受影响。
func (c *Client) Handshake(ctx context.Context) (*Handshake, error) {
	r, err := c.do(ctx, http.MethodGet, "/api/v2/server/handshake", func(req *resty.Request) *resty.Request {
		return req
	})
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", redact(err))
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

// redact 去掉 *url.Error 里的完整 URL（含 ?token=），避免令牌经错误日志外泄。
// resty 把底层错误包成 *url.Error，它的 Error() 会打印 "Get \"https://…?token=…\": …"。
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, "<redacted-url>", ue.Err)
	}
	return err
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
