package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func floatPtr(v float64) *float64 {
	return &v
}

func createTestCliOrderPool(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	t.Cleanup(cleanup)
	acc1 := &storage.Account{
		ID:           "acc_1",
		Name:         "Alpha",
		Email:        "alpha@test.com",
		RequestCount: 10,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.9)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.9)},
		},
	}
	acc2 := &storage.Account{
		ID:           "acc_2",
		Name:         "Beta",
		Email:        "beta@test.com",
		RequestCount: 50,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.7)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.7)},
		},
	}
	acc3 := &storage.Account{
		ID:           "acc_3",
		Name:         "Gamma",
		Email:        "gamma@test.com",
		RequestCount: 30,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.5)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.5)},
		},
	}

	p := storage.NewEmptyPool()
	p.Accounts = []*storage.Account{acc1, acc2, acc3}
	p.ActiveAccountID = &acc1.ID
	if err := storage.SavePool(p); err != nil {
		t.Fatal(err)
	}
}

func TestMoveCmd(t *testing.T) {
	createTestCliOrderPool(t)

	var stdout, stderr bytes.Buffer

	// --help
	code := MoveCmd(&stdout, &stderr, "--help")
	if code != 0 || !strings.Contains(stdout.String(), "usage: agy-pool move") {
		t.Fatalf("expected help output, got code=%d, out=%s", code, stdout.String())
	}

	// Missing args
	stdout.Reset()
	stderr.Reset()
	code = MoveCmd(&stdout, &stderr, "acc_1")
	if code != 2 || !strings.Contains(stderr.String(), "required: target, destination") {
		t.Fatalf("expected code 2 for missing destination, got code=%d, err=%s", code, stderr.String())
	}

	// Valid move: acc_1 to bottom
	stdout.Reset()
	stderr.Reset()
	code = MoveCmd(&stdout, &stderr, "acc_1", "bottom")
	if code != 0 {
		t.Fatalf("expected code 0, got %d, err=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Moved") || !strings.Contains(stdout.String(), "Alpha") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}

	// Move to same position
	stdout.Reset()
	stderr.Reset()
	code = MoveCmd(&stdout, &stderr, "acc_1", "3")
	if code != 0 || !strings.Contains(stdout.String(), "already at position") {
		t.Fatalf("expected same position message, got code=%d, out=%s", code, stdout.String())
	}

	// Unknown account
	stdout.Reset()
	stderr.Reset()
	code = MoveCmd(&stdout, &stderr, "unknown", "top")
	if code != 1 || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("expected code 1, got code=%d, err=%s", code, stderr.String())
	}
}

func TestSwapCmd(t *testing.T) {
	createTestCliOrderPool(t)

	var stdout, stderr bytes.Buffer

	// --help
	code := SwapCmd(&stdout, &stderr, "--help")
	if code != 0 || !strings.Contains(stdout.String(), "usage: agy-pool swap") {
		t.Fatalf("expected help output, got code=%d, out=%s", code, stdout.String())
	}

	// Missing args
	stdout.Reset()
	stderr.Reset()
	code = SwapCmd(&stdout, &stderr, "acc_1")
	if code != 2 || !strings.Contains(stderr.String(), "required: target1, target2") {
		t.Fatalf("expected code 2, got code=%d, err=%s", code, stderr.String())
	}

	// Swap 1 and 2
	stdout.Reset()
	stderr.Reset()
	code = SwapCmd(&stdout, &stderr, "1", "2")
	if code != 0 || !strings.Contains(stdout.String(), "Swapped") {
		t.Fatalf("expected success, got code=%d, out=%s, err=%s", code, stdout.String(), stderr.String())
	}

	// Swap same account
	stdout.Reset()
	stderr.Reset()
	code = SwapCmd(&stdout, &stderr, "1", "1")
	if code != 0 || !strings.Contains(stdout.String(), "Both targets refer to the same account") {
		t.Fatalf("expected same account message, got code=%d, out=%s", code, stdout.String())
	}
}

func TestSortCmd(t *testing.T) {
	createTestCliOrderPool(t)

	var stdout, stderr bytes.Buffer

	// --help
	code := SortCmd(&stdout, &stderr, "--help")
	if code != 0 || !strings.Contains(stdout.String(), "usage: agy-pool sort") {
		t.Fatalf("expected help output, got code=%d, out=%s", code, stdout.String())
	}

	// Sort by quota
	stdout.Reset()
	stderr.Reset()
	code = SortCmd(&stdout, &stderr, "quota")
	if code != 0 || !strings.Contains(stdout.String(), "Sorted account pool by") || !strings.Contains(stdout.String(), "quota") {
		t.Fatalf("expected sort success, got code=%d, out=%s, err=%s", code, stdout.String(), stderr.String())
	}

	// Reverse sort by hits
	stdout.Reset()
	stderr.Reset()
	code = SortCmd(&stdout, &stderr, "hits", "-r")
	if code != 0 || !strings.Contains(stdout.String(), "Sorted account pool by") || !strings.Contains(stdout.String(), "hits") {
		t.Fatalf("expected reverse sort success, got code=%d, out=%s, err=%s", code, stdout.String(), stderr.String())
	}

	// Invalid criterion
	stdout.Reset()
	stderr.Reset()
	code = SortCmd(&stdout, &stderr, "nonexistent")
	if code != 1 || !strings.Contains(stderr.String(), "unknown sort criterion") {
		t.Fatalf("expected unknown criterion error, got code=%d, err=%s", code, stderr.String())
	}
}

func TestReorderCmd(t *testing.T) {
	createTestCliOrderPool(t)

	var stdout, stderr bytes.Buffer

	// No args: show guide and preview
	code := ReorderCmd(&stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Current Account Pool Order") || !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("expected guide output, got code=%d, out=%s", code, stdout.String())
	}

	// 2 args relative: delegates to MoveCmd
	stdout.Reset()
	stderr.Reset()
	code = ReorderCmd(&stdout, &stderr, "1", "bottom")
	if code != 0 || !strings.Contains(stdout.String(), "Moved") {
		t.Fatalf("expected delegation to move, got code=%d, out=%s, err=%s", code, stdout.String(), stderr.String())
	}

	// Exact sequence
	stdout.Reset()
	stderr.Reset()
	code = ReorderCmd(&stdout, &stderr, "3", "2", "1")
	if code != 0 || !strings.Contains(stdout.String(), "Reordered all 3 accounts successfully") {
		t.Fatalf("expected reorder success, got code=%d, out=%s, err=%s", code, stdout.String(), stderr.String())
	}

	// Mismatched sequence length (1 argument for a 3-account pool)
	stdout.Reset()
	stderr.Reset()
	code = ReorderCmd(&stdout, &stderr, "1")
	if code != 1 || !strings.Contains(stderr.String(), "does not match") {
		t.Fatalf("expected count mismatch error, got code=%d, err=%s", code, stderr.String())
	}
}

func TestMain_OrderSubcommands(t *testing.T) {
	createTestCliOrderPool(t)

	var stdout, stderr bytes.Buffer

	// "mv" alias
	code := Main([]string{"mv", "acc_1", "bottom"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("mv failed: code=%d, err=%s", code, stderr.String())
	}

	// "move"
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"move", "acc_1", "top"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("move failed: code=%d, err=%s", code, stderr.String())
	}

	// "swap"
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"swap", "1", "2"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("swap failed: code=%d, err=%s", code, stderr.String())
	}

	// "sort"
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"sort", "quota"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sort failed: code=%d, err=%s", code, stderr.String())
	}

	// "order" alias
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"order"}, nil, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Current Account Pool Order") {
		t.Fatalf("order guide failed: code=%d, out=%s", code, stdout.String())
	}

	// "reorder"
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"reorder", "3", "2", "1"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("reorder failed: code=%d, err=%s", code, stderr.String())
	}
}

func TestConfigCmd(t *testing.T) {
	createTestCliOrderPool(t)

	var stdout, stderr bytes.Buffer

	// View config (no args)
	code := ConfigCmd(&stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Pool Configuration Settings") {
		t.Fatalf("expected config view, got code=%d, out=%s", code, stdout.String())
	}

	// Set show_email true
	stdout.Reset()
	stderr.Reset()
	code = ConfigCmd(&stdout, &stderr, "show_email", "true")
	if code != 0 || !strings.Contains(stdout.String(), "enabled (true)") {
		t.Fatalf("expected show_email enabled, got code=%d, out=%s", code, stdout.String())
	}

	// Query show_email
	stdout.Reset()
	stderr.Reset()
	code = ConfigCmd(&stdout, &stderr, "show_email")
	if code != 0 || !strings.Contains(stdout.String(), "show_email: true") {
		t.Fatalf("expected show_email query true, got code=%d, out=%s", code, stdout.String())
	}

	// Set show_email false
	stdout.Reset()
	stderr.Reset()
	code = ConfigCmd(&stdout, &stderr, "show_email", "false")
	if code != 0 || !strings.Contains(stdout.String(), "disabled (false)") {
		t.Fatalf("expected show_email disabled, got code=%d, out=%s", code, stdout.String())
	}

	// Main dispatch "config"
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"config"}, nil, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Pool Configuration Settings") {
		t.Fatalf("main config failed, got code=%d, out=%s", code, stdout.String())
	}
}
