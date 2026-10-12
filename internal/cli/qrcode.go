package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/skip2/go-qrcode"
	"golang.org/x/term"
)

// renderTerminalQRCode renders an ASCII/Unicode QR code directly to the terminal
// if the writer is an interactive TTY and wide enough.
func renderTerminalQRCode(w io.Writer, rawURL string) {
	if rawURL == "" {
		return
	}

	isTTY := false
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		isTTY = true
	}
	if !isTTY && os.Getenv("AGY_POOL_QR") != "1" {
		return
	}

	qr, err := qrcode.New(rawURL, qrcode.Low)
	if err != nil {
		return
	}

	qrStr := qr.ToSmallString(false)
	lines := strings.Split(strings.TrimRight(qrStr, "\n"), "\n")
	if len(lines) == 0 {
		return
	}

	qrWidth := len([]rune(lines[0]))
	tw := terminalWidth(w)
	if tw > 0 && tw < qrWidth {
		return
	}

	fmt.Fprintln(w, "\n[QR Code] Scan with your mobile device to complete Google verification:")
	fmt.Fprintln(w, qrStr)
}
