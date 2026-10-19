package main

import (
	"os"
	"strings"
	"sync"

	"github.com/vlxlv/agy-pool-go/internal/cli"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
)

func main() {
	// Upstream only reports Gemini quota to a versioned agy client UA (see quota.AgyClientUA).
	// Built lazily so commands that never call upstream skip `agy --version`.
	ua := sync.OnceValue(func() string { return quota.AgyClientUA(diagnostics.GetInstalledAgyVersion()) })
	quota.UserAgentProvider = ua
	proxy.UserAgentProvider = ua

	if os.Getenv(daemon.EnvDaemonChild) == "1" {
		code := cli.Main(nil, os.Stdin, os.Stdout, os.Stderr)
		if code != 0 {
			os.Exit(code)
		}
		return
	}

	if len(os.Args) > 1 {
		cmd := os.Args[1]
		if strings.HasPrefix(cmd, "eval-") || cmd == "proxy-test-server" {
			if cli.HandleEval(cmd, os.Args[1:]) {
				return
			}
		}
	}
	code := cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}
