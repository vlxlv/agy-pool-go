package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

func TestStatsCmd_TerminalOutput(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := StatsCmd(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d, err: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Algorithm Routing & Effectiveness Report") {
		t.Fatalf("expected report header in output, got:\n%s", out)
	}
	if !strings.Contains(out, "AI Diagnostic Verdict") {
		t.Fatalf("expected AI verdict in output, got:\n%s", out)
	}
}

func TestStatsCmd_JSONOutput(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := StatsCmd(&stdout, &stderr, "--json")
	if code != 0 {
		t.Fatalf("expected code 0, got %d, err: %s", code, stderr.String())
	}

	var report diagnostics.EffectivenessReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("failed to parse JSON report: %v, raw:\n%s", err, stdout.String())
	}

	if report.AIVerdict.Status == "" {
		t.Fatalf("expected non-empty AI verdict status")
	}
}
