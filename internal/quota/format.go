package quota

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

const (
	clrReset  = "\033[0m"
	clrDim    = "\033[2m"
	clrGreen  = "\033[32m"
	clrYellow = "\033[33m"
	clrRed    = "\033[31m"
)

// RenderProgressBar renders the visual quota gauge matching Python reference.
func RenderProgressBar(fraction *float64, width int) string {
	if width <= 0 {
		width = 10
	}
	if fraction == nil {
		return fmt.Sprintf("[%s%s%s]   N/A", clrDim, strings.Repeat("─", width), clrReset)
	}

	f := *fraction
	if f < 0.0 {
		f = 0.0
	} else if f > 1.0 {
		f = 1.0
	}

	filled := int(math.Round(f * float64(width)))
	if filled > width {
		filled = width
	}
	empty := width - filled

	pct := fmt.Sprintf("%5.1f%%", f*100.0)
	color := clrRed
	if f > 0.4 {
		color = clrGreen
	} else if f > 0.15 {
		color = clrYellow
	}

	filledBar := strings.Repeat("━", filled)
	emptyBar := strings.Repeat("─", empty)

	return fmt.Sprintf("[%s%s%s%s%s%s] %s%s%s",
		color, filledBar, clrReset,
		clrDim, emptyBar, clrReset,
		color, pct, clrReset)
}

// FormatRemainingTime formats a reset timestamp into relative string matching Python reference.
func FormatRemainingTime(resetTime any) string {
	if resetTime == nil {
		return "N/A"
	}

	ts := ParseISOOrTimestamp(resetTime)
	if ts == nil {
		str := fmt.Sprintf("%v", resetTime)
		if len(str) > 16 {
			return str[:16]
		}
		return str
	}

	diffSec := int64(*ts - float64(time.Now().Unix()))
	if diffSec <= 0 {
		return "Ready"
	}

	days := diffSec / 86400
	rem := diffSec % 86400
	hours := rem / 3600
	rem = rem % 3600
	minutes := rem / 60

	if days > 0 {
		return fmt.Sprintf("in %dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("in %dh %dm", hours, minutes)
	}
	return fmt.Sprintf("in %dm", minutes)
}

// DisplayQuotaFractions extracts (q5, q7) fractions from last_quota with legacy fallback.
func DisplayQuotaFractions(q *storage.QuotaState) (*float64, *float64) {
	if q == nil {
		return nil, nil
	}

	hasExplicit := q.Gemini5H != nil || q.GeminiWeekly != nil
	var q5, q7 *float64

	if q.Gemini5H != nil && q.Gemini5H.Fraction != nil {
		q5 = ParseQuotaFraction(*q.Gemini5H.Fraction)
	}
	if q.GeminiWeekly != nil && q.GeminiWeekly.Fraction != nil {
		q7 = ParseQuotaFraction(*q.GeminiWeekly.Fraction)
	}

	if !hasExplicit && q.RemainingFraction != nil {
		legacy := ParseQuotaFraction(*q.RemainingFraction)
		q5 = legacy
		q7 = legacy
	}

	return q5, q7
}
