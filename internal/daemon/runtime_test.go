package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		d        time.Duration
		expected string
	}{
		{d: 0, expected: "0s"},
		{d: 37 * time.Second, expected: "37s"},
		{d: 59 * time.Second, expected: "59s"},
		{d: 60 * time.Second, expected: "1m"},
		{d: 12*time.Minute + 41*time.Second, expected: "12m 41s"},
		{d: 12 * time.Minute, expected: "12m"},
		{d: 1*time.Hour + 27*time.Minute, expected: "1h 27m"},
		{d: 3*time.Hour + 18*time.Minute, expected: "3h 18m"},
		{d: 3 * time.Hour, expected: "3h"},
		{d: 24 * time.Hour, expected: "1d"},
		{d: 2*24*time.Hour + 4*time.Hour, expected: "2d 4h"},
		{d: -10 * time.Second, expected: "0s"},
	}

	for _, tc := range tests {
		got := FormatUptime(tc.d)
		if got != tc.expected {
			t.Errorf("FormatUptime(%v) = %q, want %q", tc.d, got, tc.expected)
		}
	}
}

func TestFormatMemoryBytes(t *testing.T) {
	toMiB := func(m float64) uint64 { return uint64(m * 1024 * 1024) }
	toGiB := func(g float64) uint64 { return uint64(g * 1024 * 1024 * 1024) }

	tests := []struct {
		bytes    uint64
		expected string
	}{
		{bytes: 512, expected: "512 B"},
		{bytes: 1024, expected: "1.0 KiB"},
		{bytes: 2048, expected: "2.0 KiB"},
		{bytes: toMiB(12.4), expected: "12.4 MiB"},
		{bytes: toMiB(18.7), expected: "18.7 MiB"},
		{bytes: toMiB(31.2), expected: "31.2 MiB"},
		{bytes: toGiB(1.5), expected: "1.5 GiB"},
	}

	for _, tc := range tests {
		got := FormatMemoryBytes(tc.bytes)
		if got != tc.expected {
			t.Errorf("FormatMemoryBytes(%d) = %q, want %q", tc.bytes, got, tc.expected)
		}
	}
}

func TestFormatGoVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{input: "go1.26.7", expected: "Go 1.26.7"},
		{input: "go1.22.5", expected: "Go 1.22.5"},
		{input: "Go1.23.0", expected: "Go 1.23.0"},
		{input: "", expected: "Go (unknown)"},
		{input: "custom-build", expected: "custom-build"},
	}

	for _, tc := range tests {
		got := FormatGoVersion(tc.input)
		if got != tc.expected {
			t.Errorf("FormatGoVersion(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestCollectRuntimeStats(t *testing.T) {
	startedAt := time.Now().Add(-10 * time.Minute)
	stats := CollectRuntimeStats(12345, startedAt)

	if stats.PID != 12345 {
		t.Errorf("expected PID 12345, got %d", stats.PID)
	}
	if !stats.StartedAt.Equal(startedAt) {
		t.Errorf("expected StartedAt %v, got %v", startedAt, stats.StartedAt)
	}
	if stats.Goroutines <= 0 {
		t.Errorf("expected positive goroutine count, got %d", stats.Goroutines)
	}
	if stats.AllocBytes == 0 {
		t.Errorf("expected non-zero AllocBytes")
	}
	if stats.SysBytes == 0 {
		t.Errorf("expected non-zero SysBytes")
	}
	if stats.GoVersion == "" {
		t.Errorf("expected non-empty GoVersion")
	}
	if stats.UpdatedAt.IsZero() {
		t.Errorf("expected non-zero UpdatedAt")
	}
}

func TestRuntimeStats_WriteAndReadRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)

	targetFile := filepath.Join(tmpDir, "agy-pool-runtime.json")
	startedAt := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	original := RuntimeStats{
		PID:        4567,
		StartedAt:  startedAt,
		UpdatedAt:  time.Now().Truncate(time.Second),
		Goroutines: 24,
		AllocBytes: 12400000,
		HeapBytes:  18700000,
		SysBytes:   31200000,
		NumGC:      42,
		GoVersion:  "go1.26.7",
	}

	if err := WriteRuntimeStats(targetFile, original); err != nil {
		t.Fatalf("WriteRuntimeStats failed: %v", err)
	}

	// Verify permissions are 0600
	st, err := os.Stat(targetFile)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected file mode 0600, got %o", perm)
	}

	readBack, err := ReadRuntimeStats(targetFile)
	if err != nil {
		t.Fatalf("ReadRuntimeStats failed: %v", err)
	}
	if readBack.PID != original.PID {
		t.Errorf("PID mismatch: got %d, want %d", readBack.PID, original.PID)
	}
	if readBack.Goroutines != original.Goroutines {
		t.Errorf("Goroutines mismatch: got %d, want %d", readBack.Goroutines, original.Goroutines)
	}
	if readBack.AllocBytes != original.AllocBytes {
		t.Errorf("AllocBytes mismatch: got %d, want %d", readBack.AllocBytes, original.AllocBytes)
	}
	if readBack.HeapBytes != original.HeapBytes {
		t.Errorf("HeapBytes mismatch: got %d, want %d", readBack.HeapBytes, original.HeapBytes)
	}
	if readBack.SysBytes != original.SysBytes {
		t.Errorf("SysBytes mismatch: got %d, want %d", readBack.SysBytes, original.SysBytes)
	}
	if readBack.NumGC != original.NumGC {
		t.Errorf("NumGC mismatch: got %d, want %d", readBack.NumGC, original.NumGC)
	}
	if readBack.GoVersion != original.GoVersion {
		t.Errorf("GoVersion mismatch: got %q, want %q", readBack.GoVersion, original.GoVersion)
	}
}

func TestRuntimeStats_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)

	nonExistent := filepath.Join(tmpDir, "missing-runtime.json")
	_, err := ReadRuntimeStats(nonExistent)
	if err == nil {
		t.Fatal("expected error reading missing file, got nil")
	}
	if !errors.Is(err, ErrRuntimeStatsNotFound) {
		t.Errorf("expected ErrRuntimeStatsNotFound, got: %v", err)
	}
}

func TestRuntimeStats_CorruptFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)

	corruptFile := filepath.Join(tmpDir, "corrupt-runtime.json")
	if err := os.WriteFile(corruptFile, []byte("{invalid json content"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadRuntimeStats(corruptFile)
	if err == nil {
		t.Fatal("expected error reading corrupt file, got nil")
	}
}

func TestRuntimeStats_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)

	emptyFile := filepath.Join(tmpDir, "empty-runtime.json")
	if err := os.WriteFile(emptyFile, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadRuntimeStats(emptyFile)
	if err == nil {
		t.Fatal("expected error reading empty file, got nil")
	}
}

func TestGetRuntimeStats_FreshnessAndPID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_TEST_MODE", "1")
	t.Setenv("AGY_GEMINI_DIR", tmpDir)

	ctx, err := ResolveInstanceContext(tmpDir, "", 8899, "")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Initially missing
	_, err = ctx.GetRuntimeStats(1000)
	if err == nil {
		t.Fatal("expected error when file does not exist")
	}

	// 2. Fresh valid stats
	stats := RuntimeStats{
		PID:        1000,
		StartedAt:  time.Now().Add(-1 * time.Hour),
		UpdatedAt:  time.Now(),
		Goroutines: 10,
		GoVersion:  "go1.22.5",
	}
	if err := WriteRuntimeStats(ctx.RuntimeFile(), stats); err != nil {
		t.Fatal(err)
	}

	got, err := ctx.GetRuntimeStats(1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PID != 1000 || got.Goroutines != 10 {
		t.Fatalf("unexpected stats: %+v", got)
	}

	// 3. PID mismatch
	_, err = ctx.GetRuntimeStats(2000)
	if err == nil {
		t.Fatal("expected error for PID mismatch")
	}
	if !errors.Is(err, ErrRuntimeStatsMismatch) {
		t.Errorf("expected ErrRuntimeStatsMismatch, got %v", err)
	}

	// 4. Stale stats (> StaleRuntimeThreshold)
	stats.UpdatedAt = time.Now().Add(-2 * time.Minute)
	if err := WriteRuntimeStats(ctx.RuntimeFile(), stats); err != nil {
		t.Fatal(err)
	}
	_, err = ctx.GetRuntimeStats(1000)
	if err == nil {
		t.Fatal("expected error for stale stats")
	}
	if !errors.Is(err, ErrRuntimeStatsStale) {
		t.Errorf("expected ErrRuntimeStatsStale, got %v", err)
	}

	// 5. Fallback to PID file if RuntimeFile is missing
	_ = os.Remove(ctx.RuntimeFile())
	pidInfo := DaemonInfo{
		PID:  1000,
		Port: 8899,
		Runtime: &RuntimeStats{
			PID:        1000,
			StartedAt:  time.Now().Add(-30 * time.Minute),
			UpdatedAt:  time.Now(),
			Goroutines: 15,
			GoVersion:  "go1.22.5",
		},
	}
	if err := WritePIDFile(ctx.PIDFile(), pidInfo); err != nil {
		t.Fatal(err)
	}

	fallback, err := ctx.GetRuntimeStats(1000)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if fallback.Goroutines != 15 {
		t.Errorf("expected fallback goroutines 15, got %d", fallback.Goroutines)
	}
}
