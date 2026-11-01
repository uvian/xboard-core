// Package sysinfo 采集面板 status 必填的四组数字。
//
// 为什么自己写：Xboard 的 UniProxyController::status() 对 cpu/mem.total/mem.used/
// swap.total/swap.used/disk.total/disk.used 做了 required 校验，缺一项就 422；
// 而 gopsutil 会给这个"只想读 7 个数字"的节点进程拖进一整棵依赖树。
package sysinfo

// Load 是一次采样的快照。
type Load struct {
	CPUPercent float64
	MemTotal   int64
	MemUsed    int64
	SwapTotal  int64
	SwapUsed   int64
	DiskTotal  int64
	DiskUsed   int64
}

// DiskPath 是 disk 统计的对象，xray/证书/log 都落在根分区。
const DiskPath = "/"
