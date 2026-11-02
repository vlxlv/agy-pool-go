package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Only changed snapshots with both Gemini windows are recorded, so the file stays small and every
// line is real upstream data.
func TestAppendQuotaHistory(t *testing.T) {
	dir := setupQuotaTestDir(t)
	snap := func(g5, gw float64) *storage.QuotaState {
		return &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(g5), ResetTime: "2030-01-01T05:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(gw), ResetTime: "2030-01-07T00:00:00Z"},
		}
	}

	appendQuotaHistory("acc-1", nil, snap(0.9, 0.8))
	appendQuotaHistory("acc-1", snap(0.9, 0.8), snap(0.9, 0.8))                          // unchanged: skipped
	appendQuotaHistory("acc-1", snap(0.9, 0.8), snap(0.85, 0.79))                        // changed: recorded
	appendQuotaHistory("acc-1", nil, &storage.QuotaState{Gemini5H: snap(1, 1).Gemini5H}) // no weekly: skipped

	b, err := os.ReadFile(filepath.Join(dir, QuotaHistoryFile))
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), b)
	}
	if !strings.Contains(lines[1], `"g5":0.85`) || !strings.Contains(lines[1], `"gw":0.79`) || !strings.Contains(lines[1], `"gw_reset":"2030-01-07T00:00:00Z"`) {
		t.Errorf("unexpected second line: %s", lines[1])
	}
}

func TestAppendQuotaHistory_Rotation(t *testing.T) {
	dir := setupQuotaTestDir(t)
	origMax := maxQuotaHistoryBytes
	t.Cleanup(func() {
		maxQuotaHistoryBytes = origMax
	})
	maxQuotaHistoryBytes = 200

	snap := func(g5, gw float64) *storage.QuotaState {
		return &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(g5), ResetTime: "2030-01-01T05:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(gw), ResetTime: "2030-01-07T00:00:00Z"},
		}
	}

	var prev *storage.QuotaState
	for i := 0; i < 5; i++ {
		cur := snap(0.9-float64(i)*0.01, 0.8-float64(i)*0.01)
		appendQuotaHistory("acc-1", prev, cur)
		prev = cur
	}

	rotPath := filepath.Join(dir, QuotaHistoryFile+".1")
	livePath := filepath.Join(dir, QuotaHistoryFile)

	rotFi, err := os.Stat(rotPath)
	if err != nil {
		t.Fatalf("expected .1 rotation file to exist: %v", err)
	}
	if rotFi.Size() < maxQuotaHistoryBytes {
		t.Fatalf(".1 file size %d is less than cap %d", rotFi.Size(), maxQuotaHistoryBytes)
	}

	liveFi, err := os.Stat(livePath)
	if err != nil {
		t.Fatalf("expected live history file to exist: %v", err)
	}
	if liveFi.Size() >= maxQuotaHistoryBytes {
		t.Fatalf("expected live file size %d to be smaller than cap %d", liveFi.Size(), maxQuotaHistoryBytes)
	}
}
