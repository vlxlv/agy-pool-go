package main

import (
	"os"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/cli"
)

func main() {
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
