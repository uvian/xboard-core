package singbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// execProcess 是真的子进程实现：stdout/stderr 追加进节点日志文件，
// 出错时 tail() 能直接把 sing-box 的报错原文带回给调用方。
type execProcess struct {
	bin  string
	args []string
	log  string

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func newExecProcess(bin string, args []string, logPath string) process {
	return &execProcess{bin: bin, args: args, log: logPath}
}

func (p *execProcess) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		return fmt.Errorf("process already started")
	}
	f, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.bin, p.args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	p.cmd = cmd
	done := make(chan struct{})
	p.done = done
	go func(c *exec.Cmd, file *os.File) {
		defer close(done)
		_ = c.Wait()
		file.Close()
	}(cmd, f)
	return nil
}

func (p *execProcess) Stop() error {
	p.mu.Lock()
	cmd := p.cmd
	done := p.done
	p.cmd, p.done = nil, nil
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// sing-box 收到 kill 会立刻退出；这里不追求优雅关闭（inbound 没有需要 flush 的状态）。
	_ = cmd.Process.Kill()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("process did not exit within 5s")
		}
	}
	return nil
}

func (p *execProcess) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}

// waitPort 用 TCP 连通性确认 inbound 真正起来了。
// listen 是 0.0.0.0/:: 时改拨回环地址（Windows 上拨 0.0.0.0 的行为不确定）。
func waitPort(ctx context.Context, listen string, port int, timeout time.Duration) error {
	host := listen
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		d := net.Dialer{Timeout: time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("dial %s: %w", addr, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
