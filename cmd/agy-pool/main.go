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
	// Upstream only reports Gemini quota to a versioned agy client UA (see quota.AgyClientUA) above a
	// minimum version, so quota refreshes read the installed agy version each time and pick up upgrades
	// without a gateway restart. The proxy's UA is a hot-path fallback for non-agy clients, so it is cached.
	// Both are lazy so commands that never call upstream skip `agy --version`.
	agyUA := func() string { return quota.AgyClientUA(diagnostics.GetInstalledAgyVersion()) }
	quota.UserAgentProvider = agyUA
	proxy.UserAgentProvider = sync.OnceValue(agyUA)

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
