// Package wsclient 是一个只做客户端的最小 RFC6455 实现。
//
// 为什么自己写：节点侧只需要「连上去、读文本帧、回 pong、写文本帧」这四件事，
// 为它引入一个 WebSocket 依赖树（gorilla / coder）不划算——本项目连 x/time、
// gopsutil 都是刻意不引的（见 docs/ARCHITECTURE.md ADR）。代价是必须自己保证：
//   - 握手校验 Sec-WebSocket-Accept（否则会被中间人/错端口骗成"连上了"）
//   - 控制帧不参与分片、长度 ≤125
//   - 客户端发出的帧必须掩码（RFC6455 §5.3，服务端会因此断开未掩码的帧）
//   - 单条消息上限，防止面板侧异常把内存打满
package wsclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa

	// maxMessage 单条消息上限。面板推的是配置/用户列表，正常几 MB 以内；
	// 超过这个量说明对面行为异常，宁可断开重连也不要继续吃内存。
	maxMessage = 16 << 20

	acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// ErrClosed 表示对端正常关闭（收到 close 帧）。
var ErrClosed = errors.New("wsclient: closed by peer")

// Conn 是一条已握手的 WebSocket 连接。读只能一个 goroutine 调，写可并发。
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// Dial 建立连接。rawURL 支持 ws:// wss://（也接受 http:// https://，方便直接吃面板配置）。
// 额外的查询参数由调用方写进 URL；headers 一般为空。
func Dial(ctx context.Context, rawURL string, timeout time.Duration, insecure bool) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("wsclient: 解析 URL: %w", err)
	}
	secure := false
	switch strings.ToLower(u.Scheme) {
	case "ws", "http":
	case "wss", "https":
		secure = true
	default:
		return nil, fmt.Errorf("wsclient: 不支持的 scheme %q", u.Scheme)
	}
	host := u.Host
	addr := host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if secure {
			addr = net.JoinHostPort(host, "443")
		} else {
			addr = net.JoinHostPort(host, "80")
		}
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := &net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("wsclient: 连接 %s: %w", addr, err)
	}
	if secure {
		tlsConn := tls.Client(nc, &tls.Config{
			ServerName:         u.Hostname(),
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: insecure, // 面板常挂自签证书；显式开关，不默认放开
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, fmt.Errorf("wsclient: TLS 握手: %w", err)
		}
		nc = tlsConn
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		nc.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)

	path := u.RequestURI()
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	b.WriteString("User-Agent: xboard-core/0.1\r\n\r\n")

	if err := nc.SetDeadline(time.Now().Add(timeout)); err != nil {
		nc.Close()
		return nil, err
	}
	if _, err := io.WriteString(nc, b.String()); err != nil {
		nc.Close()
		return nil, fmt.Errorf("wsclient: 发送握手: %w", err)
	}

	br := bufio.NewReaderSize(nc, 8192)
	resp, err := readResponse(br)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.status != 101 {
		nc.Close()
		return nil, fmt.Errorf("wsclient: 握手被拒: %d %s", resp.status, resp.reason)
	}
	if !strings.EqualFold(resp.headers["upgrade"], "websocket") {
		nc.Close()
		return nil, fmt.Errorf("wsclient: 服务端未升级（Upgrade=%q）", resp.headers["upgrade"])
	}
	sum := sha1.Sum([]byte(key + acceptGUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := resp.headers["sec-websocket-accept"]; got != want {
		nc.Close()
		return nil, fmt.Errorf("wsclient: Sec-WebSocket-Accept 不匹配（got %q）", got)
	}
	// 握手期间设的 deadline 必须清掉，否则连接会在 timeout 后自己断。
	if err := nc.SetDeadline(time.Time{}); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{conn: nc, br: br}, nil
}

type httpResp struct {
	status  int
	reason  string
	headers map[string]string
}

func readResponse(br *bufio.Reader) (*httpResp, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("wsclient: 读状态行: %w", err)
	}
	parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("wsclient: 非法状态行 %q", line)
	}
	var code int
	if _, err := fmt.Sscanf(parts[1], "%d", &code); err != nil {
		return nil, fmt.Errorf("wsclient: 非法状态码 %q", parts[1])
	}
	resp := &httpResp{status: code, headers: map[string]string{}}
	if len(parts) == 3 {
		resp.reason = parts[2]
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("wsclient: 读响应头: %w", err)
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			return resp, nil
		}
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		resp.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
}

// SetReadDeadline 用于上层做“多久没收到任何帧就认为链路死了”。
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// ReadMessage 返回下一条完整消息（分片会被拼起来）。控制帧在这里就地处理：
// ping 自动回 pong，close 回 close 并返回 ErrClosed。
func (c *Conn) ReadMessage() ([]byte, error) {
	var (
		msg     []byte
		fragOp  byte
		started bool
	)
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opText, opBinary:
			if started {
				return nil, errors.New("wsclient: 分片未结束就收到新消息头")
			}
			if fin {
				return payload, nil
			}
			fragOp, started, msg = op, true, payload
		case opContinuation:
			if !started {
				return nil, errors.New("wsclient: 孤儿续帧")
			}
			if len(msg)+len(payload) > maxMessage {
				return nil, errors.New("wsclient: 消息超过上限")
			}
			msg = append(msg, payload...)
			if fin {
				_ = fragOp
				return msg, nil
			}
		case opClose:
			// echo 一次 close 再返回，避免服务端等超时。
			_ = c.writeFrame(opClose, payload)
			c.closeOnce.Do(func() { c.closeErr = ErrClosed })
			return nil, ErrClosed
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
			// 心跳回应，忽略（read deadline 已经证明链路活着）。
		default:
			return nil, fmt.Errorf("wsclient: 未知 opcode 0x%x", op)
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return false, 0, nil, errors.New("wsclient: RSV 位被置位（未协商扩展）")
	}
	op = hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > maxMessage {
		return false, 0, nil, fmt.Errorf("wsclient: 帧长度 %d 超限", length)
	}
	if op >= 0x8 {
		if !fin {
			return false, 0, nil, errors.New("wsclient: 控制帧不允许分片")
		}
		if length > 125 {
			return false, 0, nil, errors.New("wsclient: 控制帧长度 >125")
		}
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

// WriteText 发一条文本帧。
func (c *Conn) WriteText(p []byte) error { return c.writeFrame(opText, p) }

// Ping 主动探活，主要给“面板从不 ping”的部署兜底。
func (c *Conn) Ping() error { return c.writeFrame(opPing, nil) }

func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	var hdr [14]byte
	hdr[0] = 0x80 | op
	n := len(payload)
	var hlen int
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
		hlen = 2
	case n <= 0xffff:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hlen = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hlen = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(hdr[hlen:hlen+4], mask[:])
	hlen += 4

	buf := make([]byte, hlen+n)
	copy(buf, hdr[:hlen])
	for i, b := range payload {
		buf[hlen+i] = b ^ mask[i%4]
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err := c.conn.Write(buf)
	return err
}

// Close 关连接（幂等）。
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.writeFrame(opClose, []byte{0x03, 0xe8}) // 1000 normal
		err = c.conn.Close()
		if err == nil {
			err = c.closeErr
		}
	})
	return err
}
