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
