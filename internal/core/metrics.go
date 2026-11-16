package core

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
)

var bounds = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2, 3, 5, 10, 30, 60, 120}

type histogram struct {
	count [len(bounds)]uint64
	total uint64
	sum   float64
}

func (h *histogram) observe(d time.Duration) {
	seconds := d.Seconds()
	h.total++
	h.sum += seconds
	for i, max := range bounds {
		if seconds <= max {
			h.count[i]++
		}
	}
}

func (h histogram) write(w io.Writer, name string) {
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)
	for i, max := range bounds {
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, fmt.Sprint(max), h.count[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_count %d\n%s_sum %g\n", name, h.total, name, h.total, name, h.sum)
}

type metrics struct {
	mu               sync.Mutex
	free             atomic.Uint64
	callback, douyin histogram
	levels           [4]uint64
	reasons          map[string]uint64
}

func (m *metrics) observeCallback(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callback.observe(duration)
}

func (m *metrics) observeDouyin(result model.Result, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.douyin.observe(duration)
	if result.Level > 0 && result.Level < len(m.levels) {
		m.levels[result.Level]++
	}
	if result.Reason != "" {
		if m.reasons == nil {
			m.reasons = make(map[string]uint64)
		}
		m.reasons[result.Reason]++
	}
}

func (m *metrics) write(w io.Writer, bytes uint64) {
	m.mu.Lock()
	callback, douyin, levels := m.callback, m.douyin, m.levels
	reasons := make(map[string]uint64, len(m.reasons))
	for reason, n := range m.reasons {
		reasons[reason] = n
	}
	m.mu.Unlock()
	callback.write(w, "onebox_callback_duration_seconds")
	douyin.write(w, "douyin_resolve_latency_seconds")
	fmt.Fprintln(w, "# TYPE douyin_resolve_total counter")
	for level := 1; level < len(levels); level++ {
		fmt.Fprintf(w, "douyin_resolve_total{level=\"L%d\"} %d\n", level, levels[level])
	}
	fmt.Fprintln(w, "# TYPE douyin_fail_reason_total counter")
	for reason, n := range reasons {
		fmt.Fprintf(w, "douyin_fail_reason_total{reason=%q} %d\n", reason, n)
	}
	fmt.Fprintf(w, "# TYPE onebox_vault_available_bytes gauge\nonebox_vault_available_bytes %d\n", m.free.Load())
	fmt.Fprintf(w, "# TYPE download_bytes_total counter\ndownload_bytes_total %d\n", bytes)
}
