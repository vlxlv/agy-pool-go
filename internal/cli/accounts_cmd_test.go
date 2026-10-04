package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/auth"
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
	if !strings.Contains(stdout.String(), "[Error] The selected account was not found.") {
		t.Errorf("expected not found error, got: %s", stdout.String())
	}

	// 4. Successful rename
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", Name: "OldName"},
	}
	_ = storage.SavePool(pool)
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1", "BrandNewName"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on successful rename, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stripANSI(stdout.String()), "Renamed account [acc_1] to: BrandNewName") {
		t.Errorf("expected rename success message, got: %s", stdout.String())
	}
}

func TestDisableAndEnableCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// Missing arguments -> exit 2
	var stdout, stderr bytes.Buffer
	code := Main([]string{"disable"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing disable args, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"enable"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing enable args, got %d", code)
	}

	// Target not found -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"disable", "nonexistent"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on account not found, got %d", code)
	}

	// Add an account
	pool := storage.NewEmptyPool()
	acc := &storage.Account{ID: "acc_1", Name: "TestAcc", Email: "test@example.com"}
	pool.Accounts = []*storage.Account{acc}
	_ = storage.SavePool(pool)

	// Disable acc_1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"disable", "1"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on disable, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Disabled account TestAcc") {
		t.Errorf("expected disabled success message, got: %s", stdout.String())
	}

	// Check status via getAccountState
	loaded, _ := storage.LoadPool()
	state, _ := getAccountState(loaded.Accounts[0], loaded, time.Now().Unix())
	if state != "Disabled" {
		t.Fatalf("got state %q, want 'Disabled'", state)
	}

	// Enable acc_1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"enable", "acc_1"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on enable, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Enabled account TestAcc") {
		t.Errorf("expected enabled success message, got: %s", stdout.String())
	}

	loaded, _ = storage.LoadPool()
	state, _ = getAccountState(loaded.Accounts[0], loaded, time.Now().Unix())
	if state != "Ready" {
		t.Fatalf("got state %q, want 'Ready'", state)
	}
}

func TestGetAccountState_Protected(t *testing.T) {
	pool := storage.NewEmptyPool()
	// Weekly quota <= 10% (0.08) => protected
	qw := 0.08
	q5 := 0.40
	acc := &storage.Account{
		ID:    "acc_1",
		Name:  "Backup",
		Email: "backup@example.com",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: &q5},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &qw},
		},
	}
	pool.Accounts = []*storage.Account{acc}

	state, _ := getAccountState(acc, pool, time.Now().Unix())
	if state != "Protected" {
		t.Fatalf("got state %q, want 'Protected'", state)
	}
}

func TestDisabledAccount_QuotaAndListRendering(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	fullFrac := 1.0
	lowFrac := 0.08
	h5Frac := 0.40

	accDisabled := &storage.Account{
		ID:     "acc_1",
		Name:   "DisabledAcc",
		Status: storage.StatusDisabled,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &fullFrac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &fullFrac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &fullFrac},
		},
	}
	accProtected := &storage.Account{
		ID:     "acc_2",
		Name:   "ProtectedAcc",
		Status: "",
		LastQuota: &storage.QuotaState{
			RemainingFraction: &lowFrac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &h5Frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &lowFrac},
		},
	}
	activeID := "acc_2"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{accDisabled, accProtected},
	}
	_ = storage.SavePool(pool)

	// 1. Wide quota dashboard (width 100)
	var stdout, stderr bytes.Buffer
	ListAccountsWithWidth("", 100, &stdout, &stderr)
	cleanWide := stripANSI(stdout.String())

	if !strings.Contains(cleanWide, "Disabled (Excluded from pool routing)") {
		t.Errorf("wide quota dashboard missing disabled notice, got:\n%s", cleanWide)
	}
	if !strings.Contains(cleanWide, "[Reserve Protection:") {
		t.Errorf("wide quota dashboard missing reserve protection note, got:\n%s", cleanWide)
	}
	if !strings.Contains(cleanWide, "[Protected]") || !strings.Contains(cleanWide, "[Disabled]") {
		t.Errorf("wide quota dashboard legend missing [Protected] or [Disabled], got:\n%s", cleanWide)
	}

	// 2. Tablet quota dashboard (width 70) - unified with PC
	stdout.Reset()
	ListAccountsWithWidth("", 70, &stdout, &stderr)
	cleanTablet := stripANSI(stdout.String())
	if !strings.Contains(cleanTablet, "Disabled (Excluded from pool routing)") {
		t.Errorf("tablet quota dashboard missing disabled notice, got:\n%s", cleanTablet)
	}
	if !strings.Contains(cleanTablet, "[Reserve Protection:") {
		t.Errorf("tablet quota dashboard missing reserve protection note, got:\n%s", cleanTablet)
	}
	if !strings.Contains(cleanTablet, "[Protected]") || !strings.Contains(cleanTablet, "[Disabled]") {
		t.Errorf("tablet quota dashboard legend missing [Protected] or [Disabled], got:\n%s", cleanTablet)
	}

	// 3. agy-pool list (ListCmd)
	stdout.Reset()
	code := ListCmd(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on list, got %d", code)
	}
	cleanList := stripANSI(stdout.String())
	lines := strings.Split(strings.TrimSpace(cleanList), "\n")
	foundDisabled := false
	foundProtected := false
	for _, line := range lines {
		if strings.Contains(line, "DisabledAcc") {
			foundDisabled = true
			if !strings.Contains(line, "Disabled") {
				t.Errorf("expected Disabled status for DisabledAcc, line: %s", line)
			}
			// Both 5H and Weekly quota should be "--"
			fields := strings.Fields(line)
			if len(fields) >= 7 {
				q5, qw := fields[5], fields[6]
				if q5 != "--" || qw != "--" {
					t.Errorf("expected '--' for disabled account quotas, got q5=%s qw=%s", q5, qw)
				}
			}
		}
		if strings.Contains(line, "ProtectedAcc") {
			foundProtected = true
			if !strings.Contains(line, "Protected") {
				t.Errorf("expected Protected in status for ProtectedAcc, line: %s", line)
			}
		}
	}
	if !foundDisabled || !foundProtected {
		t.Errorf("missing accounts in list output:\n%s", cleanList)
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
	if !strings.Contains(qOut, "Personal") || !strings.Contains(qOut, "hits 42") || !strings.Contains(qOut, "CLI Base") {
		t.Errorf("expected dashboard cards in quota output, got: %s", qOut)
	}

	// 3. Target not found -> exit 0 matching Python
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list", "nonexistent"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on list target not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] The selected account was not found.") {
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
			case w >= 60:
				// Wide layout (Unified PC & Tablet: 60+)
				if !strings.Contains(cleanOutput, "Antigravity Multi-Account Pool") {
					t.Errorf("width %d: missing wide banner", w)
				}
				if !strings.Contains(cleanOutput, "Main · CLI Base") || !strings.Contains(cleanOutput, "hits 1035") {
					t.Errorf("width %d: wide title line mismatch:\n%s", w, output)
				}
				if !regexp.MustCompile(`5H\s+████████████████████\s+100%\s+resets in 4h5[67]m`).MatchString(cleanOutput) {
					t.Errorf("width %d: wide 5H line mismatch:\n%s", w, cleanOutput)
				}
				if !regexp.MustCompile(`WK\s+██████████░░░░░░░░░░\s+50%\s+resets in 5d1[456]h`).MatchString(cleanOutput) {
					t.Errorf("width %d: wide WK line mismatch:\n%s", w, cleanOutput)
				}
				if !strings.Contains(cleanOutput, "quota age") && !strings.Contains(cleanOutput, "age") {
					t.Errorf("width %d: wide quota age missing", w)
				}
				if !strings.Contains(cleanOutput, "Backup · Ready") || !strings.Contains(cleanOutput, "hits 2110") {
					t.Errorf("width %d: wide account 2 mismatch:\n%s", w, cleanOutput)
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

func setupMockOAuthTokenServer(t *testing.T, expectedCode, userEmail, accessToken, refreshToken string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("code") != expectedCode {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		payloadJSON, _ := json.Marshal(map[string]any{"email": userEmail})
		payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
		mockJWT := fmt.Sprintf("eyJhbGciOiJub25lIn0.%s.", payloadB64)
		resp := map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"id_token":      mockJWT,
			"expires_in":    3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return server
}

func TestLogin_BrowserOpenerSucceeds_HTTPCallbackSucceeds(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	server := setupMockOAuthTokenServer(t, "browser-code-123", "browser_user@gmail.com", "at-browser-123", "rt-browser-123")
	defer server.Close()

	authChan := make(chan string, 1)
	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		select {
		case authChan <- targetURL:
		default:
		}
		return nil
	}
	defer func() { LoginOpener = origOpener }()

	go func() {
		authURL := <-authChan
		u, err := url.Parse(authURL)
		if err != nil {
			return
		}
		redirectURI := u.Query().Get("redirect_uri")
		time.Sleep(20 * time.Millisecond)
		resp, err := http.Get(redirectURI + "?code=browser-code-123")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	code := LoginCmdWithOptions(strings.NewReader(""), &stdout, &stderr, accounts.LoginOptions{
		TokenEndpoint: server.URL,
		Timeout:       5 * time.Second,
		PortRange:     [2]int{18250, 18300},
	})
	if code != 0 {
		t.Fatalf("LoginCmd failed (exit %d), stderr: %s", code, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Opening browser for Google authentication...") {
		t.Errorf("expected opening browser message, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Waiting for authorization...") {
		t.Errorf("expected waiting for auth message, got: %s", outStr)
	}
	if strings.Contains(outStr, "Could not open a browser automatically.") {
		t.Errorf("unexpected failure message: %s", outStr)
	}
	if strings.Contains(outStr, "Paste the authorization code or callback URL:") {
		t.Errorf("unexpected manual prompt in browser flow: %s", outStr)
	}
	if !strings.Contains(outStr, "Successfully authenticated as Account 1") {
		t.Errorf("expected success message with display name, got: %s", outStr)
	}
	if strings.Contains(outStr, "browser_user@gmail.com") {
		t.Errorf("real email leaked in output: %s", outStr)
	}

	pool, err := storage.LoadPool()
	if err != nil || len(pool.Accounts) != 1 {
		t.Fatalf("expected 1 account in pool, got err=%v, count=%d", err, len(pool.Accounts))
	}
	if pool.Accounts[0].Email != "browser_user@gmail.com" || pool.Accounts[0].AccessToken != "at-browser-123" {
		t.Fatalf("account mismatch in pool: %+v", pool.Accounts[0])
	}
}

func TestLogin_BrowserOpenerFails_ManualRawCodeSucceeds(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	server := setupMockOAuthTokenServer(t, "manual-raw-code-456", "manual_user@gmail.com", "at-manual-456", "rt-manual-456")
	defer server.Close()

	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		return errors.New("headless: no X11 DISPLAY")
	}
	defer func() { LoginOpener = origOpener }()

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("manual-raw-code-456\n")

	code := LoginCmdWithOptions(stdin, &stdout, &stderr, accounts.LoginOptions{
		TokenEndpoint: server.URL,
		Timeout:       5 * time.Second,
		PortRange:     [2]int{18301, 18350},
	})
	if code != 0 {
		t.Fatalf("LoginCmd failed (exit %d), stderr: %s", code, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Could not open a browser automatically.") {
		t.Errorf("expected opener failed message, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Open this URL:\nhttps://accounts.google.com/o/oauth2/v2/auth?") {
		t.Errorf("expected auth URL, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Paste the authorization code or callback URL:\n> ") {
		t.Errorf("expected prompt message, got: %s", outStr)
	}
	if strings.Contains(outStr, "Opening browser for Google authentication...") {
		t.Errorf("unexpected browser opened message, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Successfully authenticated as Account 1") {
		t.Errorf("expected success message with display name, got: %s", outStr)
	}
	if strings.Contains(outStr, "manual_user@gmail.com") {
		t.Errorf("real email leaked in output: %s", outStr)
	}

	pool, err := storage.LoadPool()
	if err != nil || len(pool.Accounts) != 1 {
		t.Fatalf("expected 1 account in pool, got err=%v, count=%d", err, len(pool.Accounts))
	}
	if pool.Accounts[0].Email != "manual_user@gmail.com" || pool.Accounts[0].AccessToken != "at-manual-456" {
		t.Fatalf("account mismatch in pool: %+v", pool.Accounts[0])
	}
}

func TestLogin_BrowserOpenerFails_PastedCallbackURLSucceeds(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	server := setupMockOAuthTokenServer(t, "pasted-url-code-789", "pasted_user@gmail.com", "at-pasted-789", "rt-pasted-789")
	defer server.Close()

	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		return errors.New("headless: no browser installed")
	}
	defer func() { LoginOpener = origOpener }()

	var stdout, stderr bytes.Buffer
	pastedURL := "http://localhost:8085/auth/callback?code=pasted-url-code-789&scope=email\n"
	stdin := strings.NewReader(pastedURL)

	code := LoginCmdWithOptions(stdin, &stdout, &stderr, accounts.LoginOptions{
		TokenEndpoint: server.URL,
		Timeout:       5 * time.Second,
		PortRange:     [2]int{18351, 18400},
	})
	if code != 0 {
		t.Fatalf("LoginCmd failed (exit %d), stderr: %s", code, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Could not open a browser automatically.") {
		t.Errorf("expected opener failed message, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Paste the authorization code or callback URL:\n> ") {
		t.Errorf("expected prompt message, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Successfully authenticated as Account 1") {
		t.Errorf("expected success message with display name, got: %s", outStr)
	}
	if strings.Contains(outStr, "pasted_user@gmail.com") {
		t.Errorf("real email leaked in output: %s", outStr)
	}

	pool, err := storage.LoadPool()
	if err != nil || len(pool.Accounts) != 1 {
		t.Fatalf("expected 1 account in pool, got err=%v, count=%d", err, len(pool.Accounts))
	}
	if pool.Accounts[0].Email != "pasted_user@gmail.com" || pool.Accounts[0].AccessToken != "at-pasted-789" {
		t.Fatalf("account mismatch in pool: %+v", pool.Accounts[0])
	}
}

func TestLogin_Timeout(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		return errors.New("headless: no browser")
	}
	defer func() { LoginOpener = origOpener }()

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("") // EOF immediately

	code := LoginCmdWithOptions(stdin, &stdout, &stderr, accounts.LoginOptions{
		Timeout:   50 * time.Millisecond,
		PortRange: [2]int{18401, 18450},
	})
	if code != 1 {
		t.Fatalf("expected exit code 1 on timeout, got %d", code)
	}
	if !strings.Contains(stderr.String(), "login timed out or cancelled") {
		t.Errorf("expected timeout error in stderr, got: %s", stderr.String())
	}
}

func TestLogin_OAuthCallbackError(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	authChan := make(chan string, 1)
	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		select {
		case authChan <- targetURL:
		default:
		}
		return nil
	}
	defer func() { LoginOpener = origOpener }()

	go func() {
		authURL := <-authChan
		u, err := url.Parse(authURL)
		if err != nil {
			return
		}
		redirectURI := u.Query().Get("redirect_uri")
		time.Sleep(20 * time.Millisecond)
		resp, err := http.Get(redirectURI + "?error=access_denied")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	code := LoginCmdWithOptions(strings.NewReader(""), &stdout, &stderr, accounts.LoginOptions{
		Timeout:   5 * time.Second,
		PortRange: [2]int{18451, 18500},
	})
	if code != 1 {
		t.Fatalf("expected exit code 1 on OAuth error, got %d", code)
	}
	if !strings.Contains(stderr.String(), "OAuth callback error: access_denied") {
		t.Errorf("expected access_denied in stderr, got: %s", stderr.String())
	}
}

func TestLogin_NoTokenOrSecretLeakage(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	secretToken := auth.GetClientSecret()
	accessToken := "leaked-at-secret-12345"
	refreshToken := "leaked-rt-secret-67890"

	server := setupMockOAuthTokenServer(t, "code-leak-check", "leak_check@gmail.com", accessToken, refreshToken)
	defer server.Close()

	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		return errors.New("headless")
	}
	defer func() { LoginOpener = origOpener }()

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("code-leak-check\n")

	code := LoginCmdWithOptions(stdin, &stdout, &stderr, accounts.LoginOptions{
		TokenEndpoint: server.URL,
		Timeout:       5 * time.Second,
		PortRange:     [2]int{18501, 18550},
	})
	if code != 0 {
		t.Fatalf("LoginCmd failed, exit %d", code)
	}

	combinedOutput := stdout.String() + "\n" + stderr.String()
	if strings.Contains(combinedOutput, secretToken) {
		t.Errorf("client secret leaked in output: %s", combinedOutput)
	}
	if strings.Contains(combinedOutput, accessToken) {
		t.Errorf("access token leaked in output: %s", combinedOutput)
	}
	if strings.Contains(combinedOutput, refreshToken) {
		t.Errorf("refresh token leaked in output: %s", combinedOutput)
	}
	if strings.Contains(combinedOutput, "eyJhbGciOiJub25l") {
		t.Errorf("JWT / id token leaked in output: %s", combinedOutput)
	}
	if strings.Contains(combinedOutput, "leak_check@gmail.com") {
		t.Errorf("real email leaked in output: %s", combinedOutput)
	}
	if !strings.Contains(stdout.String(), "Successfully authenticated as Account 1") {
		t.Errorf("expected display name in success output, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "https://accounts.google.com/o/oauth2/v2/auth?") {
		t.Errorf("expected auth URL in stdout, got: %s", stdout.String())
	}
}

func TestLogin_ExistingAccountUpdate(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Seed pool with existing account having a friendly name "Work"
	now := time.Now().Unix() - 1000
	initialPool := storage.NewEmptyPool()
	initialPool.Accounts = []*storage.Account{
		{
			ID:           "acc_existing_1",
			Name:         "Work",
			Email:        "existing_user@gmail.com",
			AccessToken:  "old-access-token",
			RefreshToken: "old-refresh-token",
			RequestCount: 77,
			CreatedAt:    &now,
			UpdatedAt:    &now,
		},
	}
	initialPool.ActiveAccountID = &initialPool.Accounts[0].ID
	if err := storage.SavePool(initialPool); err != nil {
		t.Fatalf("failed to save initial pool: %v", err)
	}

	// 2. Perform login with same email
	server := setupMockOAuthTokenServer(t, "update-code-999", "existing_user@gmail.com", "new-access-token-999", "new-refresh-token-999")
	defer server.Close()

	origOpener := LoginOpener
	LoginOpener = func(targetURL string) error {
		return errors.New("headless")
	}
	defer func() { LoginOpener = origOpener }()

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("update-code-999\n")

	code := LoginCmdWithOptions(stdin, &stdout, &stderr, accounts.LoginOptions{
		TokenEndpoint: server.URL,
		Timeout:       5 * time.Second,
		PortRange:     [2]int{18551, 18600},
	})
	if code != 0 {
		t.Fatalf("LoginCmd failed, exit %d: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Successfully authenticated as Work") {
		t.Errorf("expected friendly name 'Work' in success output, got: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "existing_user@gmail.com") {
		t.Errorf("real email leaked in output: %s", stdout.String())
	}

	// 3. Verify pool state
	pool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("expected exactly 1 account in pool (update in-place), got %d accounts", len(pool.Accounts))
	}
	updatedAcc := pool.Accounts[0]
	if updatedAcc.ID != "acc_existing_1" {
		t.Errorf("account ID changed from acc_existing_1 to %s", updatedAcc.ID)
	}
	if updatedAcc.Name != "Work" {
		t.Errorf("friendly name was lost: %s", updatedAcc.Name)
	}
	if updatedAcc.AccessToken != "new-access-token-999" {
		t.Errorf("access token not updated: %s", updatedAcc.AccessToken)
	}
	if updatedAcc.RefreshToken != "new-refresh-token-999" {
		t.Errorf("refresh token not updated: %s", updatedAcc.RefreshToken)
	}
	if updatedAcc.RequestCount != 77 {
		t.Errorf("request count was reset: %d", updatedAcc.RequestCount)
	}
	if updatedAcc.CreatedAt == nil || *updatedAcc.CreatedAt != now {
		t.Errorf("created_at was overwritten: %v", updatedAcc.CreatedAt)
	}
}

func TestQuotaDashboard_EdgeCasesAndLayouts(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	up12 := now - 12
	frac100 := 1.0
	frac21 := 0.21
	frac75 := 0.75

	// Reset times in minutes, hours, days
	resetMin := time.Now().Add(45*time.Minute + 30*time.Second).Format(time.RFC3339)
	resetHour := time.Now().Add(4*time.Hour + 59*time.Minute + 30*time.Second).Format(time.RFC3339)
	resetDay := time.Now().Add(5*24*time.Hour + 14*time.Hour + 30*time.Second).Format(time.RFC3339)

	accFull := &storage.Account{
		ID:           "acc_full",
		Name:         "Main",
		Status:       "ready",
		RequestCount: 2395,
		LastQuota: &storage.QuotaState{
			UpdatedAt:    &up12,
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac100, ResetTime: resetHour},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac21, ResetTime: resetDay},
		},
	}

	acc5HOnly := &storage.Account{
		ID:           "acc_5h_only",
		Name:         "FiveHourOnly",
		Status:       "ready",
		RequestCount: 150,
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{Fraction: &frac75, ResetTime: resetMin},
		},
	}

	accWKOnly := &storage.Account{
		ID:           "acc_wk_only",
		Name:         "WeeklyOnly",
		Status:       "ready",
		RequestCount: 200,
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac21, ResetTime: resetDay},
		},
	}

	accUnknown := &storage.Account{
		ID:           "acc_unknown",
		Name:         "UnknownAccount",
		Status:       "ready",
		RequestCount: 0,
		LastQuota:    nil,
	}

	accRestricted := &storage.Account{
		ID:           "acc_restricted",
		Name:         "RestrictedAcc",
		Status:       "validation_required",
		RequestCount: 42,
	}

	accAuthErr := &storage.Account{
		ID:           "acc_auth_err",
		Name:         "AuthErrAcc",
		Status:       "auth_error",
		RequestCount: 99,
	}

	accLongAndLarge := &storage.Account{
		ID:           "acc_long_large",
		Name:         "SuperLongAccountNameExceedingNormalTerminalWidthBoundariesEasily",
		Status:       "ready",
		RequestCount: 12345678,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac100, ResetTime: resetHour},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac21, ResetTime: resetDay},
		},
	}

	activeID := "acc_full"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			accFull,
			acc5HOnly,
			accWKOnly,
			accUnknown,
			accRestricted,
			accAuthErr,
			accLongAndLarge,
		},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save test pool: %v", err)
	}

	testWidths := []int{120, 96, 95, 80, 60, 59, 50, 40, 39, 35}

	for _, w := range testWidths {
		t.Run(fmt.Sprintf("Width_%d", w), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			ListAccountsWithWidth("", w, &stdout, &stderr)

			output := stdout.String()
			lines := strings.Split(output, "\n")

			for lineIdx, line := range lines {
				vLen := visibleRuneLength(line)
				if w >= 40 && vLen > w {
					t.Errorf("width %d: line %d exceeds terminal width (%d > %d):\n%s", w, lineIdx, vLen, w, line)
				}
				trimmed := strings.TrimSpace(line)
				if trimmed == "hits" || trimmed == "Hits:" || regexp.MustCompile(`^(hits\s+\d+|\d+\s+hits)$`).MatchString(trimmed) {
					t.Errorf("width %d: hits wrapped onto its own line:\n%s", w, line)
				}
			}

			clean := stripANSI(output)

			// 1. Full dual-window verification
			if !strings.Contains(clean, "Main · CLI Base") {
				t.Errorf("width %d: missing Main · CLI Base", w)
			}
			if !strings.Contains(clean, "hits 2395") && !strings.Contains(clean, "2395 hits") {
				t.Errorf("width %d: missing hit count 2395", w)
			}

			// 2. Partial known/unknown verification
			if !strings.Contains(clean, "FiveHourOnly") {
				t.Errorf("width %d: missing FiveHourOnly account", w)
			}
			if !strings.Contains(clean, "WeeklyOnly") {
				t.Errorf("width %d: missing WeeklyOnly account", w)
			}
			if !strings.Contains(clean, "WK -- unknown") {
				t.Errorf("width %d: FiveHourOnly missing explicit WK -- unknown", w)
			}
			if !strings.Contains(clean, "5H -- unknown") {
				t.Errorf("width %d: WeeklyOnly missing explicit 5H -- unknown", w)
			}

			// 3. Both unknown verification
			if !strings.Contains(clean, "UnknownAccount") {
				t.Errorf("width %d: missing UnknownAccount", w)
			}

			// 4. Restricted account state verification
			if w >= 40 {
				if !strings.Contains(clean, "Action Required (Verify needed)") {
					t.Errorf("width %d: missing Action Required marker for restricted account", w)
				}
				if !strings.Contains(clean, "Auth Failure (Re-authenticate)") {
					t.Errorf("width %d: missing Auth Failure marker", w)
				}
			} else {
				if !strings.Contains(clean, "Verify Needed") {
					t.Errorf("width %d: missing Verify Needed marker for very narrow", w)
				}
				if !strings.Contains(clean, "Auth Error") {
					t.Errorf("width %d: missing Auth Error marker for very narrow", w)
				}
			}

			// 5. Reset duration formatting verification
			if !strings.Contains(clean, "4h59m") {
				t.Errorf("width %d: missing 4h59m reset duration", w)
			}
			if !strings.Contains(clean, "5d14h") {
				t.Errorf("width %d: missing 5d14h reset duration", w)
			}
			if !strings.Contains(clean, "45m") {
				t.Errorf("width %d: missing 45m reset duration", w)
			}

			// 6. Large hits count verification
			if !strings.Contains(clean, "12345678") {
				t.Errorf("width %d: missing large hit count 12345678", w)
			}

			// Layout-specific checks
			switch {
			case w >= 60:
				if !strings.Contains(clean, "5H  ████████████████████ 100%   resets in 4h59m") {
					t.Errorf("width %d: wide 5H line format mismatch:\n%s", w, clean)
				}
				if !strings.Contains(clean, "WK  ████░░░░░░░░░░░░░░░░  21%   resets in 5d14h") {
					t.Errorf("width %d: wide WK line format mismatch:\n%s", w, clean)
				}
				if !strings.Contains(clean, "quota age") && !strings.Contains(clean, "age") {
					t.Errorf("width %d: wide quota age missing", w)
				}

			case w >= 40:
				if !strings.Contains(clean, "5H 100% ██████████  4h59m") {
					t.Errorf("width %d: mobile 5H line format mismatch:\n%s", w, clean)
				}
				if !strings.Contains(clean, "WK  21% ██░░░░░░░░  5d14h") {
					t.Errorf("width %d: mobile WK line format mismatch:\n%s", w, clean)
				}

			default:
				if !strings.Contains(clean, "5H 100%  4h59m") {
					t.Errorf("width %d: very narrow 5H line format mismatch:\n%s", w, clean)
				}
				if !strings.Contains(clean, "WK  21%  5d14h") {
					t.Errorf("width %d: very narrow WK line format mismatch:\n%s", w, clean)
				}
			}
		})
	}
}

func TestRenderRepresentativeWidths(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	up12 := now - 12
	frac100 := 1.0
	frac21 := 0.21

	resetHour := time.Now().Add(4*time.Hour + 59*time.Minute).Format(time.RFC3339)
	resetDay := time.Now().Add(5*24*time.Hour + 14*time.Hour).Format(time.RFC3339)

	acc := &storage.Account{
		ID:           "acc_main",
		Name:         "Main",
		Status:       "ready",
		RequestCount: 2395,
		LastQuota: &storage.QuotaState{
			UpdatedAt:    &up12,
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac100, ResetTime: resetHour},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac21, ResetTime: resetDay},
		},
	}
	accUnknown := &storage.Account{
		ID:           "acc_unknown",
		Name:         "Secondary",
		Status:       "ready",
		RequestCount: 10,
		LastQuota:    nil,
	}

	activeID := "acc_main"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc, accUnknown},
	}
	_ = storage.SavePool(pool)

	for _, w := range []int{120, 80, 50, 35} {
		var out bytes.Buffer
		ListAccountsWithWidth("", w, &out, &bytes.Buffer{})
		t.Logf("\n=== WIDTH %d ===\n%s", w, stripANSI(out.String()))
	}
}
