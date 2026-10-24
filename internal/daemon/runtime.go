package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

var (
	ErrRuntimeStatsNotFound = errors.New("runtime stats not found")
	ErrRuntimeStatsStale    = errors.New("runtime stats are stale")
	ErrRuntimeStatsMismatch = errors.New("runtime stats PID mismatch")
)

// StaleRuntimeThreshold defines when a runtime snapshot is considered stale.
const StaleRuntimeThreshold = 60 * time.Second

// RuntimeStats represents a snapshot of the daemon Go runtime, memory, and observability metrics.
type RuntimeStats struct {
	PID        int              `json:"pid"`
	StartedAt  time.Time        `json:"started_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	Goroutines int              `json:"goroutines"`
	AllocBytes uint64           `json:"alloc_bytes"`
	HeapBytes  uint64           `json:"heap_bytes"`
	SysBytes   uint64           `json:"sys_bytes"`
	NumGC      uint32           `json:"num_gc"`
	GoVersion  string           `json:"go_version"`
	InFlight   map[string]int64 `json:"in_flight,omitempty"`
	// Per-model account pauses (accountID -> model -> until) and the model last routed, so CLI
	// commands can show the account routing will actually pick.
	ModelPauses map[string]map[string]time.Time `json:"model_pauses,omitempty"`
	LastModel   string                          `json:"last_model,omitempty"`

	observability.Snapshot
}

// CollectRuntimeStats captures a snapshot of the current process's Go runtime, memory, and counters.
func CollectRuntimeStats(pid int, startedAt time.Time) RuntimeStats {
	if pid <= 0 {
		pid = os.Getpid()
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return RuntimeStats{
		PID:         pid,
		StartedAt:   startedAt,
		UpdatedAt:   time.Now(),
		Goroutines:  runtime.NumGoroutine(),
		AllocBytes:  m.Alloc,
		HeapBytes:   m.HeapInuse,
		SysBytes:    m.Sys,
		NumGC:       m.NumGC,
		GoVersion:   runtime.Version(),
		InFlight:    observability.GetAllInFlightGenerations(),
		ModelPauses: proxy.ActiveModelPauses(time.Now()),
		LastModel:   proxy.LastGenerationModel(),
		Snapshot:    observability.GetSnapshot(),
	}
}

// WriteRuntimeStats atomically writes runtime stats to the specified path with 0600 mode.
func WriteRuntimeStats(path string, stats RuntimeStats) error {
	if err := config.AssertSafeWritePath(path); err != nil {
		return err
	}
	return storage.AtomicJSONWrite(path, stats)
}

// ReadRuntimeStats reads runtime stats from path and verifies basic schema integrity.
func ReadRuntimeStats(path string) (*RuntimeStats, error) {
	if err := config.AssertSafeReadPath(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrRuntimeStatsNotFound
		}
		return nil, err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("empty runtime stats file")
	}
	var stats RuntimeStats
	if err := json.Unmarshal([]byte(trimmed), &stats); err != nil {
		return nil, fmt.Errorf("corrupt runtime stats: %w", err)
	}
	if stats.PID <= 0 || stats.StartedAt.IsZero() {
		return nil, errors.New("incomplete runtime stats")
	}
	return &stats, nil
}

// FormatUptime formats a duration into human-readable shorthand (e.g. 37s, 12m 41s, 3h 18m, 2d 4h).
func FormatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int64(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := secs / 60
	remSecs := secs % 60
	if mins < 60 {
		if remSecs > 0 {
			return fmt.Sprintf("%dm %ds", mins, remSecs)
		}
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	remMins := mins % 60
	if hours < 24 {
		if remMins > 0 {
			return fmt.Sprintf("%dh %dm", hours, remMins)
		}
		return fmt.Sprintf("%dh", hours)
	}
	days := hours / 24
	remHours := hours % 24
	if remHours > 0 {
		return fmt.Sprintf("%dd %dh", days, remHours)
	}
	return fmt.Sprintf("%dd", days)
}

// FormatMemoryBytes formats bytes into human-readable binary units (KiB, MiB, GiB, TiB).
func FormatMemoryBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	val := float64(b) / float64(div)
	var suffix string
	switch exp {
	case 0:
		suffix = "KiB"
	case 1:
		suffix = "MiB"
	case 2:
		suffix = "GiB"
	default:
		suffix = "TiB"
	}
	return fmt.Sprintf("%.1f %s", val, suffix)
}

// FormatGoVersion formats runtime.Version() string cleanly (e.g. "go1.22.5" -> "Go 1.22.5").
func FormatGoVersion(v string) string {
	if v == "" {
		return "Go (unknown)"
	}
	vTrim := strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(vTrim), "go") {
		return "Go " + strings.TrimSpace(vTrim[2:])
	}
	return vTrim
}

// RuntimeStatsFlushThrottle defines the minimum interval between disk writes during rapid in-flight transitions (100-250ms).
const RuntimeStatsFlushThrottle = 200 * time.Millisecond

// RuntimeStatsWriter manages periodic and coalesced writes of daemon runtime statistics.
type RuntimeStatsWriter struct {
	PID         int
	StartTime   time.Time
	RuntimeFile string
	Throttle    time.Duration
	Periodic    time.Duration
	WriteFunc   func(string, RuntimeStats) error
	CollectFunc func(int, time.Time) RuntimeStats

	triggerCh chan struct{}
	doneCh    chan struct{}
	cancel    context.CancelFunc
}

// StartRuntimeStatsWriter starts a background writer that flushes runtime stats periodically and on in-flight changes,
// coalescing rapid in-flight updates within the throttle window.
func StartRuntimeStatsWriter(
	ctx context.Context,
	pid int,
	startTime time.Time,
	runtimeFile string,
	throttle time.Duration,
	periodic time.Duration,
) *RuntimeStatsWriter {
	if throttle <= 0 {
		throttle = RuntimeStatsFlushThrottle
	}
	if periodic <= 0 {
		periodic = 1 * time.Second
	}

	subCtx, cancel := context.WithCancel(ctx)
	w := &RuntimeStatsWriter{
		PID:         pid,
		StartTime:   startTime,
		RuntimeFile: runtimeFile,
		Throttle:    throttle,
		Periodic:    periodic,
		WriteFunc:   WriteRuntimeStats,
		CollectFunc: CollectRuntimeStats,
		triggerCh:   make(chan struct{}, 1),
		doneCh:      make(chan struct{}),
		cancel:      cancel,
	}

	go w.run(subCtx)
	return w
}

// Trigger notifies the writer that an in-flight change occurred.
func (w *RuntimeStatsWriter) Trigger() {
	select {
	case w.triggerCh <- struct{}{}:
	default:
	}
}

// Done returns a channel that is closed when the writer goroutine has terminated.
func (w *RuntimeStatsWriter) Done() <-chan struct{} {
	return w.doneCh
}

// Stop cancels the writer context and waits for the writer goroutine to terminate.
func (w *RuntimeStatsWriter) Stop() {
	w.cancel()
	<-w.doneCh
}

func (w *RuntimeStatsWriter) run(ctx context.Context) {
	defer close(w.doneCh)

	ticker := time.NewTicker(w.Periodic)
	defer ticker.Stop()

	var coalesceTimer *time.Timer
	defer func() {
		if coalesceTimer != nil {
			coalesceTimer.Stop()
		}
	}()

	lastWrite := time.Now()
	pendingWrite := false

	flush := func() {
		pendingWrite = false
		lastWrite = time.Now()
		stats := w.CollectFunc(w.PID, w.StartTime)
		_ = w.WriteFunc(w.RuntimeFile, stats)
	}

	for {
		var timerCh <-chan time.Time
		if coalesceTimer != nil {
			timerCh = coalesceTimer.C
		}

		select {
		case <-ctx.Done():
			if coalesceTimer != nil {
				coalesceTimer.Stop()
				coalesceTimer = nil
			}
			if pendingWrite {
				flush()
			}
			return

		case <-ticker.C:
			if coalesceTimer != nil {
				coalesceTimer.Stop()
				coalesceTimer = nil
			}
			flush()

		case <-timerCh:
			coalesceTimer = nil
			flush()

		case <-w.triggerCh:
			now := time.Now()
			elapsed := now.Sub(lastWrite)
			if elapsed >= w.Throttle {
				if coalesceTimer != nil {
					coalesceTimer.Stop()
					coalesceTimer = nil
				}
				flush()
			} else {
				pendingWrite = true
				if coalesceTimer == nil {
					waitDur := w.Throttle - elapsed
					if waitDur < 5*time.Millisecond {
						waitDur = 5 * time.Millisecond
					}
					coalesceTimer = time.NewTimer(waitDur)
				}
			}
		}
	}
}
