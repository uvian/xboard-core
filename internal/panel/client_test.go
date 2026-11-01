package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// xboardStub 按真实 Xboard V2 契约响应：config 扁平、users 有包装、report 五合一。
func xboardStub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var lastReport map[string]any

	cfg := map[string]any{
		"protocol":        "vless",
		"listen_ip":       "0.0.0.0",
		"server_port":     443,
		"network":         "xhttp",
		"networkSettings": map[string]any{"path": "/x", "host": "cdn.example.com", "mode": "auto"},
		"tls":             2,
		"flow":            "xtls-rprx-vision",
		"decryption":      nil,
		"tls_settings": map[string]any{
			"private_key": "PRIV",
			"server_names": []string{
				"example.com",
			},
			"short_ids": []string{
				"0123abcd",
			},
			"dest":         "example.com:443",
			"minClientVer": "",
		},
		"base_config": map[string]any{"push_interval": 60, "pull_interval": 60},
	}

	mux.HandleFunc("/api/v2/server/config", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "s3cret" || r.URL.Query().Get("node_id") != "7" {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		body, _ := json.Marshal(cfg)
		sum := "W/\"abc"
		w.Header().Set("Etag", sum)
		if r.Header.Get("If-None-Match") == sum {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	mux.HandleFunc("/api/v2/server/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Response-Format") != "msgpack" {
			t.Errorf("user request must ask for msgpack, got %q", r.Header.Get("X-Response-Format"))
		}
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Etag", "W/\"u1\"")
		w.Header().Set("Content-Type", "application/x-msgpack")
		_ = msgpack.NewEncoder(w).Encode(UsersResponse{Users: []UserInfo{
			{ID: 1, UUID: "uuid-1", SpeedLimit: 100, DeviceLimit: 3},
		}})
	})

	mux.HandleFunc("/api/v2/server/report", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&lastReport); err != nil {
			t.Errorf("report body: %v", err)
		}
		_, _ = w.Write([]byte(`{"data":true}`))
	})

	mux.HandleFunc("/api/v2/server/handshake", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node_id") != "" {
			t.Errorf("handshake must not carry node_id")
		}
		_, _ = w.Write([]byte(`{"websocket":{"enabled":true,"ws_url":"wss://panel.example.com/ws"}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// config 必须按扁平契约解析：networkSettings 是 camelCase、REALITY 字段在 tls_settings 里。
func TestConfigFlatContract(t *testing.T) {
	srv := xboardStub(t)
	c := New(srv.URL, 7, "s3cret", 5*time.Second)
	ctx := context.Background()

	node, changed, err := c.Config(ctx)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !changed {
		t.Fatal("first fetch must report changed")
	}
	if node.Protocol != "vless" || node.ServerPort != 443 || node.Network != "xhttp" {
		t.Fatalf("unexpected node: %+v", node)
	}
	if node.Tag != "node-7" {
		t.Fatalf("tag: %q", node.Tag)
	}
	reality, err := node.Reality()
	if err != nil {
		t.Fatalf("reality: %v", err)
	}
	if reality.PrivateKey != "PRIV" || reality.Dest != "example.com:443" {
		t.Fatalf("unexpected reality: %+v", reality)
	}
	if got := reality.EffectiveShortIDs(); len(got) != 1 || got[0] != "0123abcd" {
		t.Fatalf("shortIds: %+v", got)
	}

	// 第二次必须拿到 304，且返回同一份缓存。
	node2, changed2, err := c.Config(ctx)
	if err != nil {
		t.Fatalf("config 2: %v", err)
	}
	if changed2 {
		t.Fatal("304 must not be reported as changed")
	}
	if node2 != node {
		t.Fatal("304 must return the cached node")
	}
}

func TestUsersAnd304(t *testing.T) {
	srv := xboardStub(t)
	c := New(srv.URL, 7, "s3cret", 5*time.Second)
	ctx := context.Background()
	users, err := c.Users(ctx)
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	if len(users) != 1 || users[0].UUID != "uuid-1" || users[0].DeviceLimit != 3 {
		t.Fatalf("unexpected users: %+v", users)
	}
	again, err := c.Users(ctx)
	if err != nil {
		t.Fatalf("users 2: %v", err)
	}
	if again != nil {
		t.Fatalf("304 must yield nil slice, got %+v", again)
	}
}

// report 的线上形状必须是 {uid:[up,down]} / {uid:[ip]}，与面板 processTraffic/processAlive 一致。
func TestReportShape(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/server/report", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, 7, "s3cret", 5*time.Second)
	err := c.Report(context.Background(), &Report{
		Traffic: map[int][2]int64{1: {100, 200}},
		Alive:   map[int][]string{1: {"10.0.0.1"}},
		Online:  map[int]int{1: 1},
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	traffic, _ := got["traffic"].(map[string]any)
	if traffic == nil {
		t.Fatalf("missing traffic key: %+v", got)
	}
	pair, _ := traffic["1"].([]any)
	if len(pair) != 2 || pair[0].(float64) != 100 || pair[1].(float64) != 200 {
		t.Fatalf("traffic shape: %+v", traffic)
	}
	alive, _ := got["alive"].(map[string]any)
	if alive == nil {
		t.Fatalf("missing alive key: %+v", got)
	}
	if ips, _ := alive["1"].([]any); len(ips) != 1 || ips[0] != "10.0.0.1" {
		t.Fatalf("alive shape: %+v", alive)
	}
}

func TestHandshakeMachineMode(t *testing.T) {
	srv := xboardStub(t)
	c := NewMachine(srv.URL, "m-1", "s3cret", 5*time.Second)
	h, err := c.Handshake(context.Background())
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if !h.Websocket.Enabled || h.Websocket.WSURL == "" {
		t.Fatalf("unexpected handshake: %+v", h)
	}
	if c.Tag() != "machine-m-1" {
		t.Fatalf("machine tag: %q", c.Tag())
	}
}
