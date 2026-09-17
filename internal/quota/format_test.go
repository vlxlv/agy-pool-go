package quota

import (
	"regexp"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func stripANSIString(s string) string {
	return regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`).ReplaceAllString(s, "")
}

func TestRenderBlockProgressBar(t *testing.T) {
	cases := []struct {
		frac     *float64
		width    int
		expChars string
	}{
		{nil, 10, "░░░░░░░░░░"},
		{floatPtr(1.0), 10, "██████████"},
		{floatPtr(0.5), 10, "█████░░░░░"},
		{floatPtr(0.0), 10, "░░░░░░░░░░"},
		{floatPtr(0.2), 10, "██░░░░░░░░"},
		{floatPtr(0.5), 0, ""},
		{floatPtr(0.5), -1, ""},
	}

	for _, c := range cases {
		out := RenderBlockProgressBar(c.frac, c.width)
		clean := stripANSIString(out)
		if clean != c.expChars {
			t.Errorf("frac %v: expected %q, got %q", c.frac, c.expChars, clean)
		}
	}
}

func TestFormatCompactRemainingTime(t *testing.T) {
	now := time.Now()

	// Ready in the past
	past := now.Add(-10 * time.Minute).Format(time.RFC3339)
	if res := FormatCompactRemainingTime(past); res != "ready" {
		t.Errorf("expected ready, got %s", res)
	}

	// 4h57m
	f4h57m := now.Add(4*time.Hour + 57*time.Minute).Format(time.RFC3339)
	if res := FormatCompactRemainingTime(f4h57m); res != "4h57m" {
		t.Errorf("expected 4h57m, got %s", res)
	}

	// 5d15h
	f5d15h := now.Add(5*24*time.Hour + 15*time.Hour).Format(time.RFC3339)
	if res := FormatCompactRemainingTime(f5d15h); res != "5d15h" {
		t.Errorf("expected 5d15h, got %s", res)
	}

	// nil -> N/A
	if res := FormatCompactRemainingTime(nil); res != "N/A" {
		t.Errorf("expected N/A, got %s", res)
	}
}

func TestFormatShortQuotaAge(t *testing.T) {
	now := int64(1000000)

	// nil last quota -> unk
	accNil := &storage.Account{}
	if res := FormatShortQuotaAge(accNil, float64(now)); res != "unk" {
		t.Errorf("expected unk, got %s", res)
	}

	// 45s
	up45 := now - 45
	acc45 := &storage.Account{
		LastQuota: &storage.QuotaState{UpdatedAt: &up45},
	}
	if res := FormatShortQuotaAge(acc45, float64(now)); res != "45s" {
		t.Errorf("expected 45s, got %s", res)
	}

	// 2m
	up2m := now - 154
	acc2m := &storage.Account{
		LastQuota: &storage.QuotaState{UpdatedAt: &up2m},
	}
	if res := FormatShortQuotaAge(acc2m, float64(now)); res != "2m" {
		t.Errorf("expected 2m, got %s", res)
	}

	// 3h
	up3h := now - 3*3600 - 10
	acc3h := &storage.Account{
		LastQuota: &storage.QuotaState{UpdatedAt: &up3h},
	}
	if res := FormatShortQuotaAge(acc3h, float64(now)); res != "3h" {
		t.Errorf("expected 3h, got %s", res)
	}
}
