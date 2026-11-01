//go:build !linux && !windows

package sysinfo

// 既非 Linux 也非 Windows：只用于本地开发（macOS 上跑 go build 不代表节点要部署在那里）。
// 返回全零会让面板 status 校验失败，所以这里给一组满足校验的最小值。
func Stat() (Load, error) {
	return Load{CPUPercent: 0, MemTotal: 1, MemUsed: 0, SwapTotal: 1, SwapUsed: 0, DiskTotal: 1, DiskUsed: 0}, nil
}
