package wsclient

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsServer 是测试用服务端：手写握手 + 帧，够验证客户端行为即可。
type wsServer struct {
	t      *testing.T
	srv    *httptest.Server
	conns  chan *testConn
	onText func(c *testConn, msg string)
}

type testConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func newWSServer(t *testing.T, onText func(c *testConn, msg string)) *wsServer {
	t.Helper()
	ws := &wsServer{t: t, conns: make(chan *testConn, 4), onText: onText}
	ws.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not a websocket", 400)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + acceptGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])

		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err := rw.Flush(); err != nil {
			conn.Close()
			return
		}
		ws.conns <- &testConn{conn: conn, br: rw.Reader}
	}))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (s *wsServer) accept(t *testing.T) *testConn {
	t.Helper()
	select {
	case c := <-s.conns:
		t.Cleanup(func() { c.conn.Close() })
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没等到连接")
		return nil
	}
}

func (c *testConn) readFrame(t *testing.T) (op byte, payload []byte) {
	t.Helper()
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		t.Fatalf("读帧头: %v", err)
	}
	op = hdr[0] & 0x0f
	if hdr[1]&0x80 == 0 {
		t.Fatal("客户端发出的帧必须带掩码（RFC6455 §5.3）")
	}
	n := int64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		io.ReadFull(c.br, ext[:])
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		io.ReadFull(c.br, ext[:])
		n = int64(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	io.ReadFull(c.br, mask[:])
	payload = make([]byte, n)
	io.ReadFull(c.br, payload)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload
}

func (c *testConn) writeFrame(t *testing.T, op byte, payload []byte) {
	t.Helper()
	var hdr [10]byte
	hdr[0] = 0x80 | op
	switch {
	case len(payload) < 126:
		hdr[1] = byte(len(payload))
		c.conn.Write(append(hdr[:2], payload...))
	case len(payload) <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(payload)))
		c.conn.Write(append(hdr[:4], payload...))
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(len(payload)))
		c.conn.Write(append(hdr[:10], payload...))
	}
}

func (s *wsServer) url() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") }

func TestDialEcho(t *testing.T) {
	s := newWSServer(t, nil)
	c, err := Dial(context.Background(), s.url()+"/ws?token=abc", 3*time.Second, false)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	srv := s.accept(t)
	if err := c.WriteText([]byte(`{"event":"pong"}`)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	op, payload := srv.readFrame(t)
	if op != opText || string(payload) != `{"event":"pong"}` {
		t.Fatalf("服务端收到 op=%d payload=%q", op, payload)
	}

	srv.writeFrame(t, opText, []byte(`{"event":"ping"}`))
	msg, err := c.ReadMessage()
	if err != nil || string(msg) != `{"event":"ping"}` {
		t.Fatalf("ReadMessage: %q %v", msg, err)
	}
}

func TestRepliesPongToPing(t *testing.T) {
	s := newWSServer(t, nil)
	c, err := Dial(context.Background(), s.url(), 3*time.Second, false)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	srv := s.accept(t)

	// pong 是在 ReadMessage 处理 ping 时就地回的，所以必须有人正在读。
	go func() {
		_, _ = c.ReadMessage()
	}()
	srv.writeFrame(t, opPing, []byte("hb"))
	op, payload := srv.readFrame(t)
	if op != opPong || string(payload) != "hb" {
		t.Fatalf("期望回 pong(带 echo payload)，实际 op=%d payload=%q", op, payload)
	}
}

func TestAssemblesFragments(t *testing.T) {
	s := newWSServer(t, nil)
	c, err := Dial(context.Background(), s.url(), 3*time.Second, false)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	srv := s.accept(t)

	// 手工发三帧：text(!fin) + continuation(!fin) + continuation(fin)
	writeFragment := func(op byte, fin bool, b string) {
		first := byte(op)
		if fin {
			first |= 0x80
		}
		srv.conn.Write([]byte{first, byte(len(b))})
		srv.conn.Write([]byte(b))
	}
	writeFragment(opText, false, `{"event":`)
	writeFragment(opContinuation, false, `"sync.`)
	writeFragment(opContinuation, true, `config"}`)

	msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if want := `{"event":"sync.config"}`; string(msg) != want {
		t.Fatalf("分片拼接结果 %q，期望 %q", msg, want)
	}
}

func TestRejectsBadAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// 故意给错的 Sec-WebSocket-Accept
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bogus\r\n\r\n")
		rw.Flush()
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	_, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), 2*time.Second, false)
	if err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Fatalf("期望 Accept 校验失败，得到 %v", err)
	}
}

func TestCloseFromPeer(t *testing.T) {
	s := newWSServer(t, nil)
	c, err := Dial(context.Background(), s.url(), 3*time.Second, false)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	srv := s.accept(t)

	srv.writeFrame(t, opClose, []byte{0x03, 0xe8})
	if _, err := c.ReadMessage(); !errors.Is(err, ErrClosed) {
		t.Fatalf("期望 ErrClosed，得到 %v", err)
	}
}
