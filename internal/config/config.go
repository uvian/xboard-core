package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Conf 是节点进程的全部本地配置。面板侧的节点参数不在这里，走 panel.NodeInfo 拉取。
type Conf struct {
	Log     LogConf     `mapstructure:"Log"`
	Node    NodeConf    `mapstructure:"Node"`
	Machine MachineConf `mapstructure:"Machine"`
	API     APIConf     `mapstructure:"API"`
	Kernel  KernelConf  `mapstructure:"Kernel"`
}

type LogConf struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
}

type NodeConf struct {
	APIHost   string `mapstructure:"ApiHost"`
	NodeID    int    `mapstructure:"NodeID"`
	APIKey    string `mapstructure:"ApiKey"`
	Timeout   int    `mapstructure:"Timeout"`   // 秒，默认 15
	Retry     int    `mapstructure:"Retry"`     // 默认 3
	PullEvery int    `mapstructure:"PullEvery"` // 秒，默认 60；WS 推送可用时降级为兜底
	// WSInsecure 只在面板的 ws_url 用自签证书时需要打开。
	// 单独一个开关而不是全局 InsecureSkipVerify：TLS 校验一旦全局关掉，
	// 配置下发这条链路（token 就在里面）就没有任何保护了。
	WSInsecure bool `mapstructure:"WsInsecure"`
}

type MachineConf struct {
	Enabled    bool   `mapstructure:"Enabled"`
	WSInsecure bool   `mapstructure:"WsInsecure"`
	MachineID  int    `mapstructure:"MachineID"`
	NodeIDs    []int  `mapstructure:"NodeIDs"`
	Token      string `mapstructure:"Token"`
	APIHost    string `mapstructure:"ApiHost"`
}

type APIConf struct {
	Listen  string `mapstructure:"Listen"`  // 默认 127.0.0.1:8090，仅监听本地
	Metrics bool   `mapstructure:"Metrics"` // /metrics 开关
}

type KernelConf struct {
	// Xray 用于 vless/vmess/trojan/ss2022 与 REALITY/xhttp；
	// sing-box 用于 anytls/tuic/hysteria2。二者可同时启用。
	Xray    bool `mapstructure:"Xray"`
	Singbox bool `mapstructure:"Singbox"`
	// SingboxBinary / SingboxDir：sing-box 走子进程（不是库），这两个是它的可执行文件与工作目录。
	SingboxBinary string `mapstructure:"SingboxBinary"`
	SingboxDir    string `mapstructure:"SingboxDir"`
}

func defaults() *Conf {
	return &Conf{
		Log:    LogConf{Level: "info"},
		Node:   NodeConf{Timeout: 15, Retry: 3, PullEvery: 60},
		API:    APIConf{Listen: "127.0.0.1:8090", Metrics: true},
		Kernel: KernelConf{Xray: true, SingboxBinary: "sing-box", SingboxDir: "run/singbox"},
	}
}

// Load 读取 YAML 并做最小校验。缺项填默认值，不向调用方索要信息。
func Load(path string) (*Conf, error) {
	c := defaults()
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	applyAliases(v)
	if err := v.Unmarshal(c); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Conf) validate() error {
	if strings.TrimSpace(c.API.Listen) == "" {
		// 空 Addr 在 net/http 里等于 ":http"（0.0.0.0:80），而 /metrics 没有鉴权。
		return fmt.Errorf("API.Listen must not be empty (default 127.0.0.1:8090)")
	}
	if c.Machine.Enabled {
		if c.Machine.APIHost == "" || c.Machine.Token == "" {
			return fmt.Errorf("Machine.Enabled requires ApiHost and Token")
		}
		if len(c.Machine.NodeIDs) == 0 {
			return fmt.Errorf("Machine.Enabled requires at least one NodeID")
		}
	} else if c.Node.APIHost == "" || c.Node.NodeID == 0 {
		return fmt.Errorf("node mode requires Node.ApiHost and Node.NodeID")
	}
	if !c.Kernel.Xray && !c.Kernel.Singbox {
		return fmt.Errorf("at least one kernel must be enabled")
	}
	if c.Node.Timeout <= 0 {
		c.Node.Timeout = 15
	}
	if c.Node.Retry <= 0 {
		c.Node.Retry = 3
	}
	if c.Node.PullEvery <= 0 {
		c.Node.PullEvery = 60
	}
	return nil
}

// Timeout 返回面板请求超时。
func (c *Conf) Timeout() time.Duration { return time.Duration(c.Node.Timeout) * time.Second }

// aliases 让 snake_case 写法和结构体上的 CamelCase 等价。
// 用户大多按 YAML 习惯写 api_host，而 mapstructure tag 是 ApiHost；
// viper 的 key 是小写但不做下划线折叠，所以这里显式搬一次。
var aliases = map[string]string{
	"log.level":             "Log.Level",
	"log.output":            "Log.Output",
	"node.api_host":         "Node.ApiHost",
	"node.node_id":          "Node.NodeID",
	"node.api_key":          "Node.ApiKey",
	"node.timeout":          "Node.Timeout",
	"node.retry":            "Node.Retry",
	"node.pull_every":       "Node.PullEvery",
	"node.ws_insecure":      "Node.WsInsecure",
	"machine.enabled":       "Machine.Enabled",
	"machine.api_host":      "Machine.ApiHost",
	"machine.machine_id":    "Machine.MachineID",
	"machine.node_ids":      "Machine.NodeIDs",
	"machine.token":         "Machine.Token",
	"machine.ws_insecure":   "Machine.WSInsecure",
	"api.listen":            "API.Listen",
	"api.metrics":           "API.Metrics",
	"kernel.xray":           "Kernel.Xray",
	"kernel.singbox":        "Kernel.Singbox",
	"kernel.singbox_binary": "Kernel.SingboxBinary",
	"kernel.singbox_dir":    "Kernel.SingboxDir",
}

func applyAliases(v *viper.Viper) {
	for from, to := range aliases {
		if v.IsSet(from) && !v.IsSet(to) {
			v.Set(to, v.Get(from))
		}
	}
}
