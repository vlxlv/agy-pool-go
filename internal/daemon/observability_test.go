package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/observability"
)

// Case M: Older runtime stats JSON missing new counter fields decodes safely with zeros without errors.
func TestObservability_CaseM_OlderRuntimeJSONDecodesSafely(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)
	resetSandbox := config.SetSyntheticSandboxRoot(tmpDir)
	defer resetSandbox()

	olderJSON := `{
  "pid": 12345,
  "started_at": "2026-09-19T10:00:00Z",
  "updated_at": "2026-09-19T11:00:00Z",
  "goroutines": 12,
  "alloc_bytes": 1048576,
  "heap_bytes": 2097152,
  "sys_bytes": 4194304,
  "num_gc": 5,
  "go_version": "go1.22.5"
}`

	statsFile := filepath.Join(tmpDir, "older-runtime.json")
	if err := os.WriteFile(statsFile, []byte(olderJSON), 0o600); err != nil {
		t.Fatalf("failed to write older runtime stats: %v", err)
	}

	stats, err := ReadRuntimeStats(statsFile)
	if err != nil {
		t.Fatalf("expected older runtime stats to decode safely, got error: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}

	// Verify original fields decoded correctly
	if stats.PID != 12345 {
		t.Errorf("expected PID 12345, got %d", stats.PID)
	}
	if stats.Goroutines != 12 {
		t.Errorf("expected Goroutines 12, got %d", stats.Goroutines)
	}
	if stats.AllocBytes != 1048576 {
		t.Errorf("expected AllocBytes 1048576, got %d", stats.AllocBytes)
	}
	if stats.HeapBytes != 2097152 {
		t.Errorf("expected HeapBytes 2097152, got %d", stats.HeapBytes)
	}
	if stats.SysBytes != 4194304 {
		t.Errorf("expected SysBytes 4194304, got %d", stats.SysBytes)
	}
	if stats.NumGC != 5 {
		t.Errorf("expected NumGC 5, got %d", stats.NumGC)
	}
	if stats.GoVersion != "go1.22.5" {
		t.Errorf("expected GoVersion go1.22.5, got %s", stats.GoVersion)
	}

	// Verify all 14 counters default to 0
	expectedZeros := observability.Snapshot{}
	if stats.Snapshot != expectedZeros {
		t.Errorf("expected zero counters for older JSON, got %+v", stats.Snapshot)
	}
}

// Case N: Stale PID / PID mismatch rejects stats and does not present old PID counters as current.
func TestObservability_CaseN_StalePIDRejectsStats(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)
	resetSandbox := config.SetSyntheticSandboxRoot(tmpDir)
	defer resetSandbox()

	ctx, err := ResolveInstanceContext(tmpDir, "", 8899, "")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	// 1. Stats file has old PID 9999 with non-zero counters
	oldStats := RuntimeStats{
		PID:        9999,
		StartedAt:  time.Now().Add(-1 * time.Hour),
		UpdatedAt:  time.Now(),
		Goroutines: 10,
		GoVersion:  "go1.22.5",
		Snapshot: observability.Snapshot{
			GenerationRequests: 42,
			RoutingDecisions:   10,
			FailoverAttempts:   3,
			FailoverSuccess:    3,
		},
	}
	if err := WriteRuntimeStats(ctx.RuntimeFile(), oldStats); err != nil {
		t.Fatalf("WriteRuntimeStats failed: %v", err)
	}

	// Calling GetRuntimeStats with expected PID 1234 must reject stats due to PID mismatch
	stats, err := ctx.GetRuntimeStats(1234)
	if err == nil {
		t.Fatal("expected error on PID mismatch, got nil")
	}
	if !errors.Is(err, ErrRuntimeStatsMismatch) {
		t.Errorf("expected ErrRuntimeStatsMismatch, got %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats on PID mismatch, got %+v", stats)
	}

	// 2. Stats file has matching PID 1234, but is stale (> StaleRuntimeThreshold)
	staleStats := RuntimeStats{
		PID:        1234,
		StartedAt:  time.Now().Add(-2 * time.Hour),
		UpdatedAt:  time.Now().Add(-2 * time.Minute),
		Goroutines: 10,
		GoVersion:  "go1.22.5",
		Snapshot: observability.Snapshot{
			GenerationRequests: 99,
		},
	}
	if err := WriteRuntimeStats(ctx.RuntimeFile(), staleStats); err != nil {
		t.Fatalf("WriteRuntimeStats failed: %v", err)
	}

	stats, err = ctx.GetRuntimeStats(1234)
	if err == nil {
		t.Fatal("expected error on stale runtime stats, got nil")
	}
	if !errors.Is(err, ErrRuntimeStatsStale) {
		t.Errorf("expected ErrRuntimeStatsStale, got %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats on stale stats, got %+v", stats)
	}
}
