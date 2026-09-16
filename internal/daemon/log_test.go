package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{500, "500 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{2048, "2.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{5242880, "5.0 MB"},
	}

	for _, tc := range cases {
		got := FormatSize(tc.bytes)
		if got != tc.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestRotateLogIfNeeded_CopytruncateAndShift(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	// 1. Nonexistent file
	rotated, err := RotateLogIfNeeded(logPath, 100, 2, false)
	if err != nil || rotated {
		t.Fatalf("expected (false, nil) for nonexistent file, got (%v, %v)", rotated, err)
	}

	// 2. Open file for writing to test copytruncate preserving open descriptor
	writer, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer writer.Close()

	_, _ = writer.WriteString("first generation log line\n")
	_ = writer.Sync()

	// 3. Below threshold: no rotation
	rotated, err = RotateLogIfNeeded(logPath, 1000, 2, false)
	if err != nil || rotated {
		t.Fatalf("expected no rotation when below threshold")
	}

	// 4. Force rotate: should rotate and copytruncate
	rotated, err = RotateLogIfNeeded(logPath, 1000, 2, true)
	if err != nil || !rotated {
		t.Fatalf("expected successful forced rotation, got (%v, %v)", rotated, err)
	}

	// Verify backup .1 exists and has first line
	b1Path := logPath + ".1"
	b1Content, err := os.ReadFile(b1Path)
	if err != nil {
		t.Fatalf("failed to read backup .1: %v", err)
	}
	if !strings.Contains(string(b1Content), "first generation") {
		t.Fatalf("backup .1 missing original content: %s", string(b1Content))
	}

	// Verify active log was truncated to 0
	activeSt, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat active log: %v", err)
	}
	if activeSt.Size() != 0 {
		t.Fatalf("expected active log size 0 after rotation, got %d", activeSt.Size())
	}

	// 5. CRITICAL: Write to original open writer without reopening
	_, _ = writer.WriteString("second generation log line after rotation\n")
	_ = writer.Sync()

	// Verify active log has the new content
	activeContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read active log after new write: %v", err)
	}
	if !strings.Contains(string(activeContent), "second generation") {
		t.Fatalf("active log missing content written to open writer: %s", string(activeContent))
	}

	// 6. Rotate again: .1 should shift to .2, and new .1 created
	rotated, err = RotateLogIfNeeded(logPath, 10, 2, true)
	if err != nil || !rotated {
		t.Fatalf("expected second rotation to succeed")
	}

	b2Path := logPath + ".2"
	b2Content, err := os.ReadFile(b2Path)
	if err != nil {
		t.Fatalf("backup .2 not found after shift: %v", err)
	}
	if !strings.Contains(string(b2Content), "first generation") {
		t.Fatalf("backup .2 should contain first generation data")
	}

	b1Content, _ = os.ReadFile(b1Path)
	if !strings.Contains(string(b1Content), "second generation") {
		t.Fatalf("backup .1 should contain second generation data")
	}
}

func TestClearLog_Semantics(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "clear_test.log")

	// Create active log and 3 backups
	_ = os.WriteFile(logPath, []byte("active data\n"), 0o600)
	_ = os.WriteFile(logPath+".1", []byte("backup 1\n"), 0o600)
	_ = os.WriteFile(logPath+".2", []byte("backup 2\n"), 0o600)

	// Keep an open writer to active log
	writer, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("failed to open log: %v", err)
	}
	defer writer.Close()

	if err := ClearLog(logPath, 2); err != nil {
		t.Fatalf("ClearLog failed: %v", err)
	}

	// Verify backups are gone
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("backup .1 should have been removed")
	}
	if _, err := os.Stat(logPath + ".2"); !os.IsNotExist(err) {
		t.Fatalf("backup .2 should have been removed")
	}

	// Verify active log exists and is size 0
	st, err := os.Stat(logPath)
	if err != nil || st.Size() != 0 {
		t.Fatalf("expected active log size 0 after ClearLog, got %v (err: %v)", st.Size(), err)
	}

	// Verify open writer can still write to cleared log
	_, _ = writer.WriteString("written after clear\n")
	_ = writer.Sync()

	content, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(content), "written after clear") {
		t.Fatalf("log not writable after ClearLog: %s", string(content))
	}
}

func TestFailClosedProductionLogPath(t *testing.T) {
	prodDir := config.GetRealProductionGeminiDir()
	prodLogFile := filepath.Join(prodDir, "test.log")

	_, err := RotateLogIfNeeded(prodLogFile, 100, 1, true)
	if err == nil {
		t.Fatalf("expected RotateLogIfNeeded to fail closed on production path")
	}

	err = ClearLog(prodLogFile, 1)
	if err == nil {
		t.Fatalf("expected ClearLog to fail closed on production path")
	}
}

func TestMaybeRotateLog_RateLimiting(t *testing.T) {
	// First call should set the timer
	_ = MaybeRotateLog()

	// Second immediate call must be rate-limited and return false
	if MaybeRotateLog() {
		t.Fatalf("expected second immediate MaybeRotateLog call to be rate limited")
	}

	// Reset timer to past
	logRotateMu.Lock()
	lastLogRotateCheck = time.Now().Add(-2 * LogRotateInterval)
	logRotateMu.Unlock()

	// Call after interval should execute (and return false because default log file doesn't exceed limit)
	_ = MaybeRotateLog()
}
