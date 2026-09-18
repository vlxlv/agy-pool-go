package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestCP1DVerifyRedactsSensitiveURL(t *testing.T) {
	setupCLITestEnv(t)
	url := "https://example.test/verify?Email=user@example.com&login_hint=user@example.com&foo=bar&code=secret"
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{{ID: "acc_1", Email: "user@example.com", Status: "validation_required", ValidationURL: &url}}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"verify", "1"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("verify exit code = %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Verification required for Account 1.") {
		t.Fatalf("missing safe account label: %q", out)
	}
	if strings.Contains(out, "user@example.com") || strings.Contains(out, "secret") {
		t.Fatalf("sensitive URL data leaked: %q", out)
	}
	if !strings.Contains(out, "foo=bar") || !strings.Contains(out, "REDACTED") {
		t.Fatalf("URL was not safely rendered: %q", out)
	}
}

func TestCP1DRequiredCommandFailures(t *testing.T) {
	setupCLITestEnv(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"import-current", []string{"import-current"}},
		{"import", []string{"import", "/does/not/exist.json"}},
		{"verify", []string{"verify", "user@example.com"}},
		{"quota refresh", []string{"quota", "--refresh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Main(tc.args, nil, &stdout, &stderr); code == 0 {
				t.Fatalf("expected failure, stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 && tc.name != "quota refresh" {
				t.Fatalf("unexpected success output: %q", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("expected safe error on stderr")
			}
		})
	}
}

func TestCP1DRunSyncFailureDoesNotExec(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()
	pool := storage.NewEmptyPool()
	acc := &storage.Account{ID: "acc_1", AccessToken: "access-secret"}
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.GetAgyTokenFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeAgy := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	diagnostics.SetAgyBinaryFinder(func() string { return fakeAgy })
	var execCalled bool
	oldExec := ExecHandler
	t.Cleanup(func() { ExecHandler = oldExec; diagnostics.SetAgyBinaryFinder(nil) })
	ExecHandler = func(string, []string, []string) error { execCalled = true; return nil }
	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "--", "-p", "hello"}, nil, &stdout, &stderr)
	if code == 0 || execCalled || strings.Contains(stderr.String(), "access-secret") {
		t.Fatalf("code=%d exec=%v stderr=%q", code, execCalled, stderr.String())
	}
}

func TestRedactUserURL(t *testing.T) {
	got, ok := redactUserURL("https://example.test/path?Email=a%40b.test&foo=bar&token=abc#fragment")
	if !ok || strings.Contains(got, "a@b.test") || strings.Contains(got, "abc") || strings.Contains(got, "fragment") {
		t.Fatalf("unsafe redaction: ok=%v url=%q", ok, got)
	}
	if !strings.Contains(got, "foo=bar") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("unexpected redacted URL: %q", got)
	}
}
