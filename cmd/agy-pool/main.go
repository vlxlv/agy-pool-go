package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/vlxlv/agy-pool-go/internal/cli"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
)

func main() {
	// Upstream only reports Gemini quota buckets to a versioned agy client UA; a bare "antigravity/cli"
	// gets the 3p buckets alone. Built lazily so commands that never call upstream skip `agy --version`.
	ua := sync.OnceValue(func() string {
		return fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; auth_method=consumer)",
			diagnostics.GetInstalledAgyVersion(), runtime.GOOS, runtime.GOARCH)
	})
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
