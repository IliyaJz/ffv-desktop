//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

func registerResizeSignal(ch chan os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}

func unregisterResizeSignal(ch chan os.Signal) {
	signal.Stop(ch)
}

func isPrivileged() bool {
	return os.Geteuid() == 0
}
