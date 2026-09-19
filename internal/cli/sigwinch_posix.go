//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyResize(sigChan chan<- os.Signal) {
	signal.Notify(sigChan, syscall.SIGWINCH)
}
