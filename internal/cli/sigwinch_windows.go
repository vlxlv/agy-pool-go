//go:build windows

package cli

import "os"

func notifyResize(sigChan chan<- os.Signal) {}
