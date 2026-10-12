package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRenderTerminalQRCode_SkipsOnEmpty(t *testing.T) {
	var buf bytes.Buffer
	renderTerminalQRCode(&buf, "")
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for empty URL, got: %q", buf.String())
	}
}

func TestRenderTerminalQRCode_ForcedViaEnv(t *testing.T) {
	t.Setenv("AGY_POOL_QR", "1")
	var buf bytes.Buffer
	renderTerminalQRCode(&buf, "https://accounts.google.com/test")
	out := buf.String()
	if !strings.Contains(out, "[QR Code]") {
		t.Fatalf("expected QR Code header in output, got: %q", out)
	}
	if !strings.Contains(out, "█") && !strings.Contains(out, "▀") {
		t.Fatalf("expected QR block characters in output, got: %q", out)
	}
}

func TestRenderTerminalQRCode_SkipsOnNonTTYByDefault(t *testing.T) {
	_ = os.Unsetenv("AGY_POOL_QR")
	var buf bytes.Buffer
	renderTerminalQRCode(&buf, "https://accounts.google.com/test")
	if buf.Len() != 0 {
		t.Fatalf("expected no output on non-TTY buffer by default, got: %q", buf.String())
	}
}
