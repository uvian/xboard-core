package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// snake_case 是 YAML 里的常见写法，必须和结构体上的 CamelCase 等价。
func TestLoadAcceptsSnakeCase(t *testing.T) {
	p := write(t, `
log: {level: debug}
node:
  api_host: http://127.0.0.1:8099
  node_id: 7
  api_key: tok
  pull_every: 45
api: {listen: 127.0.0.1:8091, metrics: true}
kernel: {xray: true, singbox: false}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Node.APIHost != "http://127.0.0.1:8099" || c.Node.NodeID != 7 || c.Node.APIKey != "tok" {
		t.Fatalf("node 段没解析到：%+v", c.Node)
	}
	if c.Node.PullEvery != 45 || c.Log.Level != "debug" {
		t.Fatalf("标量没解析到：%+v %+v", c.Node, c.Log)
	}
}

func TestLoadAcceptsCamelCase(t *testing.T) {
	p := write(t, `
Log: {Level: info}
Node: {ApiHost: http://p:1, NodeID: 1, ApiKey: t}
Kernel: {Xray: true}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Node.NodeID != 1 || c.Node.APIHost != "http://p:1" {
		t.Fatalf("camel 写法没解析到：%+v", c.Node)
	}
}

func TestLoadRejectsEmptyAPIListen(t *testing.T) {
	p := write(t, `
Node: {ApiHost: http://p:1, NodeID: 1, ApiKey: t}
API: {Listen: ""}
Kernel: {Xray: true}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("空 API.Listen 应该报错（等于 0.0.0.0:80）")
	}
}

func TestLoadMachineMode(t *testing.T) {
	p := write(t, `
machine:
  enabled: true
  api_host: http://p:1
  machine_id: 3
  node_ids: [1,2]
  token: t
kernel: {xray: true}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Machine.Enabled || c.Machine.MachineID != 3 || len(c.Machine.NodeIDs) != 2 {
		t.Fatalf("machine 段没解析到：%+v", c.Machine)
	}
}
