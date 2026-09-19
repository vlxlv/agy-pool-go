package cli

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Case O: status --verbose CLI output renders runtime counters and verifies privacy (no emails, tokens, URLs, prompts).
func TestObservability_CaseO_StatusVerboseAndPrivacy(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.95
	expiry := float64(now + 3600)
	secretEmail := "sensitive-developer-account@internal.corp.example.com"
	secretToken := "ya29.a0AfH6SMD-SuperSecretOauthAccessToken123456789"
	acc := &storage.Account{
		ID:           "acc_sensitive_1",
		Email:        secretEmail,
		Name:         "ProdDeveloper",
		Status:       "ready",
		RequestCount: 150,
		CreatedAt:    &now,
		AccessToken:  secretToken,
		RefreshToken: "1//04mock_refresh_token_abcdef",
		TokenExpiry:  &expiry,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &frac},
		},
	}
	activeID := acc.ID
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	ctx, err := daemon.ResolveInstanceContext(tmpDir, "", port, "")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	currentPID := os.Getpid()
	pidInfo := daemon.DaemonInfo{
		PID:        currentPID,
		Port:       port,
		DataDir:    ctx.DataDir,
		Version:    ctx.Version,
		ListenHost: ctx.ListenHost,
		Listen:     ctx.ListenHost,
		ConfigPath: ctx.ConfigPath,
		ConfigHash: ctx.ConfigHash,
	}
	if err := daemon.WritePIDFile(ctx.PIDFile(), pidInfo); err != nil {
		t.Fatalf("WritePIDFile failed: %v", err)
	}

	stats := daemon.RuntimeStats{
		PID:        currentPID,
		StartedAt:  time.Now().Add(-2 * time.Hour),
		UpdatedAt:  time.Now(),
		Goroutines: 14,
		AllocBytes: 15 * 1024 * 1024,
		HeapBytes:  22 * 1024 * 1024,
		SysBytes:   35 * 1024 * 1024,
		NumGC:      12,
		GoVersion:  "go1.26.7",
		Snapshot: observability.Snapshot{
			GenerationRequests:   1250,
			GenerationSuccess:    1240,
			RoutingDecisions:     1250,
			FailoverAttempts:     15,
			FailoverSuccess:      14,
			RestrictedSkips:      5,
			QuotaRefreshAttempts: 80,
			QuotaRefreshSuccess:  78,
			QuotaRefreshFailure:  2,
			AuthRefreshAttempts:  10,
			AuthRefreshSuccess:   10,
			AuthRefreshFailure:   0,
			PersistenceErrors:    1,
			NoReplayPrevented:    3,
		},
	}
	if err := daemon.WriteRuntimeStats(ctx.RuntimeFile(), stats); err != nil {
		t.Fatalf("WriteRuntimeStats failed: %v", err)
	}

	// 1. Default status (no -v/--verbose) MUST NOT contain "Runtime Counters"
	var outDefault, errDefault bytes.Buffer
	code := Main([]string{"status", "-p", strconv.Itoa(port), "-D", tmpDir}, nil, &outDefault, &errDefault)
	if code != 0 {
		t.Fatalf("expected exit code 0 on default status, got %d. stderr: %s", code, errDefault.String())
	}
	defaultStr := outDefault.String()
	if strings.Contains(defaultStr, "Runtime Counters") {
		t.Errorf("default status output should not contain 'Runtime Counters', got:\n%s", defaultStr)
	}

	// 2. Verbose status (-v) MUST contain "Runtime Counters" with formatted values
	var outVerbose, errVerbose bytes.Buffer
	code = Main([]string{"status", "-v", "-p", strconv.Itoa(port), "-D", tmpDir}, nil, &outVerbose, &errVerbose)
	if code != 0 {
		t.Fatalf("expected exit code 0 on status -v, got %d. stderr: %s", code, errVerbose.String())
	}
	verboseStr := outVerbose.String()

	expectedSections := []string{
		"Runtime Counters",
		"Generation", "1,250 requests / 1,240 success",
		"Routing", "1,250 decisions",
		"Failover", "15 attempts / 14 recovered",
		"Restricted skips", "5",
		"Quota refresh", "80 / 78 ok / 2 failed",
		"Auth refresh", "10 / 10 ok / 0 failed",
		"Persistence errors", "1",
		"Replay prevented", "3",
	}
	for _, exp := range expectedSections {
		if !strings.Contains(verboseStr, exp) {
			t.Errorf("verbose status output missing expected section %q\nGot:\n%s", exp, verboseStr)
		}
	}

	// 3. Privacy assertions: Output must NEVER expose account email, tokens, URLs, or secrets
	prohibitedPatterns := []string{
		secretEmail,
		secretToken,
		"ya29.",
		"refresh_token",
		"https://",
		"http://",
		"@internal.corp.example.com",
		"prompt",
		"content",
	}
	for _, pattern := range prohibitedPatterns {
		if strings.Contains(verboseStr, pattern) {
			t.Errorf("SECURITY/PRIVACY VIOLATION: verbose status leaked prohibited pattern %q\nOutput was:\n%s", pattern, verboseStr)
		}
		if strings.Contains(defaultStr, pattern) {
			t.Errorf("SECURITY/PRIVACY VIOLATION: default status leaked prohibited pattern %q\nOutput was:\n%s", pattern, defaultStr)
		}
	}

	// 4. Stopped gateway with --verbose renders clean indicator
	_ = l.Close()
	_ = os.Remove(ctx.PIDFile())
	var outStopped, errStopped bytes.Buffer
	code = Main([]string{"status", "--verbose", "-p", strconv.Itoa(port), "-D", tmpDir}, nil, &outStopped, &errStopped)
	if code != 0 {
		t.Fatalf("expected exit code 0 on stopped status --verbose, got %d", code)
	}
	stoppedStr := outStopped.String()
	if !strings.Contains(stoppedStr, "Runtime Counters") || !strings.Contains(stoppedStr, "(gateway not running)") {
		t.Errorf("expected '(gateway not running)' in stopped verbose status, got:\n%s", stoppedStr)
	}
}
