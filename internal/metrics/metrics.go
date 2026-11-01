package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Registry 是最小可用的指标集合，刻意不引入 prometheus 依赖：
// 输出格式就是 Prometheus 文本协议，抓取端无需任何适配。
type Registry struct {
	started        time.Time
	Reloads        atomic.Int64
	ReloadErrors   atomic.Int64
	GateRejects    atomic.Int64
	ActiveConns    atomic.Int64
	Users          atomic.Int64
	LastPullOK     atomic.Int64 // unix 秒
	KernelRestarts atomic.Int64
}

func New() *Registry { return &Registry{started: time.Now()} }

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP xboard_core_uptime_seconds Process uptime.\n# TYPE xboard_core_uptime_seconds gauge\nxboard_core_uptime_seconds %d\n", int64(time.Since(r.started).Seconds()))
		for _, m := range []struct {
			name, help string
			v          int64
		}{
			{"xboard_core_config_reloads_total", "Config reload attempts.", r.Reloads.Load()},
			{"xboard_core_config_reload_errors_total", "Config reload failures.", r.ReloadErrors.Load()},
			{"xboard_core_gate_rejects_total", "Connections rejected by device limit.", r.GateRejects.Load()},
			{"xboard_core_active_conns", "Active connections.", r.ActiveConns.Load()},
			{"xboard_core_users", "Users loaded in kernel.", r.Users.Load()},
			{"xboard_core_last_pull_ok_timestamp", "Last successful panel pull.", r.LastPullOK.Load()},
			{"xboard_core_kernel_restarts_total", "Kernel instance restarts.", r.KernelRestarts.Load()},
		} {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", m.name, m.help, m.name, m.name, m.v)
		}
	})
}
