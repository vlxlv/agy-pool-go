package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestRenameCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Missing arguments -> exit 2
	var stdout, stderr bytes.Buffer
	code := Main([]string{"rename"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing rename args, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing rename target name, got %d", code)
	}

	// 2. Empty name -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1", "   "}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on empty rename name, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account name cannot be empty.") {
		t.Errorf("expected empty name error, got: %s", stdout.String())
	}

	// 3. Target not found in empty pool -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "nonexistent", "NewName"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on account not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account 'nonexistent' not found.") {
		t.Errorf("expected not found error, got: %s", stdout.String())
	}
}

func TestListAccounts_EmptyAndPopulated(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Empty pool
	var stdout, stderr bytes.Buffer
	code := Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on empty list, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No accounts in pool yet.") {
		t.Errorf("expected empty pool notice, got: %s", stdout.String())
	}

	// 2. Populate pool
	now := time.Now().Unix()
	frac := 0.75
	acc1 := &storage.Account{
		ID:           "acc_1",
		Email:        "user1@gmail.com",
		Name:         "Personal",
		Status:       "active",
		RequestCount: 42,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &frac},
		},
	}
	activeID := "acc_1"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1},
	}
	_ = storage.SavePool(pool)

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on populated list, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Personal") {
		t.Errorf("expected account name Personal, got: %s", out)
	}
	if !strings.Contains(out, "CLI Base") {
		t.Errorf("expected CLI Base status, got: %s", out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("expected 42 hits, got: %s", out)
	}

	// Also verify quota command renders the full visual dashboard
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"quota"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on quota, got %d", code)
	}
	qOut := stdout.String()
	if !strings.Contains(qOut, "Personal") || !strings.Contains(qOut, "Hits: 42") || !strings.Contains(qOut, "[CLI Base]") {
		t.Errorf("expected dashboard cards in quota output, got: %s", qOut)
	}

	// 3. Target not found -> exit 0 matching Python
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list", "nonexistent"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on list target not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account 'nonexistent' not found.") {
		t.Errorf("expected not found error on stdout, got: %s", stdout.String())
	}
}

func TestMigrateLegacyCommand(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	srcFile := filepath.Join(tempDir, "legacy_pool.json")
	targetDir := filepath.Join(tempDir, "target_data")
	content := `{
  "version": 1,
  "strategy": "max_quota",
  "accounts": [
    {"id": "acc_1", "email": "migrated@example.com", "access_token": "tok1"}
  ]
}`
	if err := os.WriteFile(srcFile, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write legacy pool: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"migrate-legacy", "-s", srcFile, "-D", targetDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("migrate-legacy failed with code %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Successfully migrated 1 account") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}

	// Verify target accounts.json exists
	targetAccounts := filepath.Join(targetDir, "accounts.json")
	if _, err := os.Stat(targetAccounts); err != nil {
		t.Fatalf("target accounts file was not created: %v", err)
	}

	// Verify source was untouched
	data, err := os.ReadFile(srcFile)
	if err != nil || string(data) != content {
		t.Fatalf("source file was modified or unreadable")
	}
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func visibleRuneLength(s string) int {
	return utf8.RuneCountInString(stripANSI(s))
}

func TestChooseBarWidth(t *testing.T) {
	cases := []struct {
		lineWidth  int
		nonBarLen  int
		maxDesired int
		minUseful  int
		expected   int
	}{
		{80, 20, 10, 4, 10},
		{50, 15, 10, 4, 10},
		{25, 18, 10, 4, 7}, // shrinks before wrapping
		{22, 18, 10, 4, 4}, // minimum useful
		{21, 18, 10, 4, 0}, // below minUseful -> removed
		{15, 18, 10, 4, 0}, // negative available -> removed
	}

	for _, c := range cases {
		actual := chooseBarWidth(c.lineWidth, c.nonBarLen, c.maxDesired, c.minUseful)
		if actual != c.expected {
			t.Errorf("chooseBarWidth(%d, %d, %d, %d): expected %d, got %d",
				c.lineWidth, c.nonBarLen, c.maxDesired, c.minUseful, c.expected, actual)
		}
	}
}

func TestTerminalWidth_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	w := terminalWidth(&buf)
	if w != 100 {
		t.Errorf("expected deterministic default 100 for non-TTY writer, got %d", w)
	}

	// Verify ListAccounts on non-TTY writer renders desktop wide layout
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	activeID := "acc_test"
	_ = storage.SavePool(&storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Accounts: []*storage.Account{
			{ID: "acc_test", Name: "TestAccount", Status: "ready"},
		},
	})

	var stdout, stderr bytes.Buffer
	ListAccounts("", &stdout, &stderr)
	clean := stripANSI(stdout.String())
	if !strings.Contains(clean, "Antigravity Multi-Account Pool") {
		t.Errorf("expected desktop wide banner for non-TTY output, got:\n%s", clean)
	}
}

func TestListAccounts_ResponsiveWidths(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac1_5 := 1.0
	frac1_w := 0.499
	r1_5 := time.Now().Add(4*time.Hour + 57*time.Minute).Format(time.RFC3339)
	r1_w := time.Now().Add(5*24*time.Hour + 15*time.Hour).Format(time.RFC3339)
	up1 := now - 154 // 2m 34s ago

	acc1 := &storage.Account{
		ID:           "acc_1",
		Name:         "Main",
		Status:       "ready",
		RequestCount: 1035,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			UpdatedAt:    &up1,
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac1_5, ResetTime: r1_5},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac1_w, ResetTime: r1_w},
		},
	}

	frac2_5 := 0.916
	frac2_w := 0.554
	r2_5 := time.Now().Add(4*time.Hour + 53*time.Minute).Format(time.RFC3339)
	r2_w := time.Now().Add(5*24*time.Hour + 16*time.Hour).Format(time.RFC3339)
	up2 := now - 46 // 46s ago

	acc2 := &storage.Account{
		ID:           "acc_2",
		Name:         "Backup",
		Status:       "ready",
		RequestCount: 2110,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			UpdatedAt:    &up2,
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac2_5, ResetTime: r2_5},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac2_w, ResetTime: r2_w},
		},
	}

	activeID := "acc_1"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1, acc2},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save test pool: %v", err)
	}

	widths := []int{120, 96, 95, 80, 60, 59, 50, 45, 40, 39, 35}

	for _, w := range widths {
		t.Run(fmt.Sprintf("Width_%d", w), func(t *testing.T) {
			subNow := time.Now().Unix()
			subUp1 := subNow - 154
			subUp2 := subNow - 46
			acc1.LastQuota.UpdatedAt = &subUp1
			acc1.LastQuota.Gemini5H.ResetTime = time.Now().Add(4*time.Hour + 57*time.Minute + 30*time.Second).Format(time.RFC3339)
			acc1.LastQuota.GeminiWeekly.ResetTime = time.Now().Add(5*24*time.Hour + 15*time.Hour + 30*time.Minute).Format(time.RFC3339)
			acc2.LastQuota.UpdatedAt = &subUp2
			acc2.LastQuota.Gemini5H.ResetTime = time.Now().Add(4*time.Hour + 53*time.Minute + 30*time.Second).Format(time.RFC3339)
			acc2.LastQuota.GeminiWeekly.ResetTime = time.Now().Add(5*24*time.Hour + 16*time.Hour + 30*time.Minute).Format(time.RFC3339)
			_ = storage.SavePool(pool)

			var stdout, stderr bytes.Buffer
			ListAccountsWithWidth("", w, &stdout, &stderr)

			output := stdout.String()
			lines := strings.Split(output, "\n")

			for lineIdx, line := range lines {
				vLen := visibleRuneLength(line)
				if w >= 40 && vLen > w {
					t.Errorf("width %d: line %d exceeds terminal width (%d > %d):\n%s", w, lineIdx, vLen, w, line)
				}
				// Verify Hits never wraps independently on a line by itself
				trimmed := strings.TrimSpace(line)
				if trimmed == "Hits:" || trimmed == "hits" || regexp.MustCompile(`^(hits\s+\d+|\d+\s+hits|Hits:\s+\d+)$`).MatchString(trimmed) {
					t.Errorf("width %d: Hits wrapped onto its own accidental line:\n%s", w, line)
				}
			}

			cleanOutput := stripANSI(output)

			switch {
			case w >= 96:
				// Wide layout
				if !strings.Contains(cleanOutput, "Antigravity Multi-Account Pool") {
					t.Errorf("width %d: missing wide banner", w)
				}
				if !strings.Contains(cleanOutput, "[1] Main") || !strings.Contains(cleanOutput, "[CLI Base]") || !strings.Contains(cleanOutput, "Hits: 1035") {
					t.Errorf("width %d: wide title line mismatch:\n%s", w, output)
				}
				if !strings.Contains(cleanOutput, "• Gemini 5-Hour:") || !strings.Contains(cleanOutput, "• Gemini Weekly:") {
					t.Errorf("width %d: wide quota lines missing", w)
				}

			case w >= 60:
				// Compact layout: 60-95
				if !strings.Contains(cleanOutput, "[1] Main · CLI Base") {
					t.Errorf("width %d: compact title line mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`5H\s+100\.0%\s+██████████\s+reset 4h5[67]m`).MatchString(cleanOutput) {
					t.Errorf("width %d: compact 5H line mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`WK\s+49\.9%\s+█████░░░░░\s+reset 5d1[45]h`).MatchString(cleanOutput) {
					t.Errorf("width %d: compact WK line mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`age 2m3[45]s · hits 1035`).MatchString(cleanOutput) {
					t.Errorf("width %d: compact metadata line mismatch:\n%s", w, cleanOutput)
				}
				// Verify account 2
				if !strings.Contains(cleanOutput, "[2] Backup · Ready") {
					t.Errorf("width %d: compact account 2 title mismatch:\n%s", w, cleanOutput)
				}

			case w >= 40:
				// Mobile layout: 40-59
				if !strings.Contains(cleanOutput, "Main · CLI Base") {
					t.Errorf("width %d: mobile title line mismatch:\n%s", w, cleanOutput)
				}
				if strings.Contains(cleanOutput, "[1] Main") {
					t.Errorf("width %d: mobile layout should not include [1] index prefix:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`5H\s+100%\s+██████████\s+4h5[67]m`).MatchString(cleanOutput) {
					t.Errorf("width %d: mobile 5H line mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`WK\s+50%\s+█████░░░░░\s+5d1[45]h`).MatchString(cleanOutput) {
					t.Errorf("width %d: mobile WK line mismatch:\n%s", w, cleanOutput)
				}
				if !strings.Contains(cleanOutput, "age 2m · hits 1035") {
					t.Errorf("width %d: mobile metadata line mismatch:\n%s", w, cleanOutput)
				}
				if strings.Contains(cleanOutput, "reset 4h") {
					t.Errorf("width %d: mobile layout should not have 'reset ' prefix:\n%s", w, cleanOutput)
				}
				if !strings.Contains(cleanOutput, "max_quota · proxy") {
					t.Errorf("width %d: mobile layout should include proxy footer:\n%s", w, cleanOutput)
				}

			default:
				// Very narrow layout: < 40
				if !strings.Contains(cleanOutput, "Main · CLI Base") {
					t.Errorf("width %d: very narrow title mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`5H\s+100%\s+4h5[67]m`).MatchString(cleanOutput) {
					t.Errorf("width %d: very narrow 5H line mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`WK\s+50%\s+5d1[45]h`).MatchString(cleanOutput) {
					t.Errorf("width %d: very narrow WK line mismatch:\n%s", w, cleanOutput)
				}
				if !strings.Contains(cleanOutput, "2m · 1035 hits") {
					t.Errorf("width %d: very narrow metadata line mismatch:\n%s", w, cleanOutput)
				}
				if strings.Contains(cleanOutput, "██") {
					t.Errorf("width %d: very narrow should remove progress bar:\n%s", w, cleanOutput)
				}
			}
		})
	}
}
