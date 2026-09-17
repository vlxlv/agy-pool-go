package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func TestLogsCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	logFile := config.GetLogFile()
	_ = os.WriteFile(logFile, []byte("line1\nline2\nline3\n"), 0600)

	var stdout, stderr bytes.Buffer
	code := Main([]string{"logs", "-n", "2"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("logs expected 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Antigravity Gateway Log") {
		t.Errorf("expected log header, got: %s", out)
	}
	if !strings.Contains(out, "line2") || !strings.Contains(out, "line3") {
		t.Errorf("expected tail lines, got: %s", out)
	}

	// Clear logs
	stdout.Reset()
	code = Main([]string{"logs", "--clear"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("logs --clear expected 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Successfully cleared gateway log") {
		t.Errorf("expected clear log success message, got: %s", stdout.String())
	}
}

func TestDaemonRun_RemovedFromCLI(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer

	// 1. Calling "daemon-run" directly must fail with invalid choice (code 2)
	code := Main([]string{"daemon-run"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected code 2 when invoking removed daemon-run, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid choice: 'daemon-run'") {
		t.Fatalf("expected 'invalid choice: 'daemon-run'' in stderr, got: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "daemon-run,") {
		t.Fatalf("usage list in error output should not contain 'daemon-run': %s", stderr.String())
	}

	// 2. Help output must not mention daemon-run
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"--help"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0 for --help, got %d", code)
	}
	if strings.Contains(stdout.String(), "daemon-run") {
		t.Fatalf("help text must not mention 'daemon-run', got: %s", stdout.String())
	}
}
