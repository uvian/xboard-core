package kernel

import (
	"context"
	"fmt"
	"sort"

	"github.com/xboard-ng/xboard-core/internal/panel"
)

// Kernel 是一个可替换的转发内核。当前有两个实现：xray（vless/vmess/trojan/ss2022）
// 与 singbox（anytls/tuic/hysteria2）。协议到内核的映射由 Supported 决定。
type Kernel interface {
	Name() string
	Supported(protocol string) bool
	Start(ctx context.Context, node *panel.NodeConfig, users []panel.UserInfo) error
	Reload(ctx context.Context, node *panel.NodeConfig) error
	UpdateUsers(ctx context.Context, added, deleted, modified []panel.UserInfo) error
	Close() error
}

// Registry 按协议路由到内核；同一节点只由一个内核承载。
type Registry struct {
	kernels []Kernel
}

func NewRegistry(ks ...Kernel) *Registry { return &Registry{kernels: ks} }

// For 返回能承载该协议的内核；多个都能承载时按注册顺序取第一个（xray 优先）。
func (r *Registry) For(protocol string) (Kernel, error) {
	for _, k := range r.kernels {
		if k.Supported(protocol) {
			return k, nil
		}
	}
	return nil, fmt.Errorf("no kernel supports protocol %q (registered: %v)", protocol, r.Names())
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.kernels))
	for _, k := range r.kernels {
		out = append(out, k.Name())
	}
	sort.Strings(out)
	return out
}
