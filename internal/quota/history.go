package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// QuotaHistoryFile is the JSONL log of Gemini quota changes in the data dir. It lets the
// 5h-to-weekly consumption ratio be measured from normal use: compare g5 and gw deltas between
// lines of one account whose reset times did not change.
const QuotaHistoryFile = "quota-history.jsonl"

var maxQuotaHistoryBytes int64 = 5 << 20

// appendQuotaHistory records a quota-summary snapshot when it differs from the previous one.
// Best effort: a failed write must never fail the refresh. The log is capped at maxQuotaHistoryBytes
// and rotated to .1 when full.
func appendQuotaHistory(accountID string, prev, cur *storage.QuotaState) {
	if cur == nil || cur.Gemini5H == nil || cur.GeminiWeekly == nil || cur.Gemini5H.Fraction == nil || cur.GeminiWeekly.Fraction == nil {
		return
	}
	if prev != nil && sameWindow(prev.Gemini5H, cur.Gemini5H) && sameWindow(prev.GeminiWeekly, cur.GeminiWeekly) {
		return
	}
	line, err := json.Marshal(map[string]any{
		"ts":       time.Now().Unix(),
		"account":  accountID,
		"g5":       *cur.Gemini5H.Fraction,
		"g5_reset": cur.Gemini5H.ResetTime,
		"gw":       *cur.GeminiWeekly.Fraction,
		"gw_reset": cur.GeminiWeekly.ResetTime,
	})
	if err != nil {
		return
	}
	path := filepath.Join(config.GetDataDir(), QuotaHistoryFile)
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxQuotaHistoryBytes {
		_ = storage.WithFileLock(path+".lock", true, func() error {
			if curFi, err := os.Stat(path); err == nil && curFi.Size() >= maxQuotaHistoryBytes {
				return os.Rename(path, path+".1")
			}
			return nil
		})
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func sameWindow(a, b *storage.QuotaWindow) bool {
	if a == nil || b == nil || a.Fraction == nil || b.Fraction == nil {
		return false
	}
	return *a.Fraction == *b.Fraction && fmt.Sprint(a.ResetTime) == fmt.Sprint(b.ResetTime)
}
