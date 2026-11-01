package sysinfo

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	mu      sync.Mutex
	lastAll uint64
	lastIdl uint64
	primed  bool
)

// Stat 采一次快照。cpu 用两次采样之间的增量算占用率；
// 第一次调用只做基线，返回 0。
func Stat() (Load, error) {
	var l Load
	total, idle, err := cpuTimes()
	if err != nil {
		return l, err
	}
	mu.Lock()
	if primed && total > lastAll {
		dt := float64(total - lastAll)
		di := float64(idle - lastIdl)
		if dt > 0 {
			l.CPUPercent = (dt - di) / dt * 100
			if l.CPUPercent < 0 {
				l.CPUPercent = 0
			}
			if l.CPUPercent > 100 {
				l.CPUPercent = 100
			}
		}
	}
	lastAll, lastIdl, primed = total, idle, true
	mu.Unlock()

	if l.MemTotal, l.MemUsed, l.SwapTotal, l.SwapUsed, err = memInfo(); err != nil {
		return l, err
	}
	if l.DiskTotal, l.DiskUsed, err = diskUsage(DiskPath); err != nil {
		return l, err
	}
	return l, nil
}

func cpuTimes() (total, idle uint64, err error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		// cpu  user nice system idle iowait irq softirq steal guest guest_nice
		fields := strings.Fields(line)[1:]
		for i, f := range fields {
			v, perr := strconv.ParseUint(f, 10, 64)
			if perr != nil {
				return 0, 0, perr
			}
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		return total, idle, nil
	}
	return 0, 0, nil
}

func memInfo() (total, used, swapTotal, swapUsed int64, err error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, 0, err
	}
	vals := make(map[string]int64, 8)
	for _, line := range strings.Split(string(b), "\n") {
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		f := strings.Fields(kv[1])
		if len(f) == 0 {
			continue
		}
		v, perr := strconv.ParseInt(f[0], 10, 64)
		if perr != nil {
			continue
		}
		vals[strings.TrimSpace(kv[0])] = v * 1024 // kB -> B
	}
	total = vals["MemTotal"]
	avail := vals["MemAvailable"]
	if avail == 0 {
		avail = vals["MemFree"]
	}
	used = total - avail
	swapTotal = vals["SwapTotal"]
	swapUsed = swapTotal - vals["SwapFree"]
	if swapUsed < 0 {
		swapUsed = 0
	}
	return total, used, swapTotal, swapUsed, nil
}

func diskUsage(path string) (total, used int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	return total, total - free, nil
}
