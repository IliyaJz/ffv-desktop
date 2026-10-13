//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyInterrupt(ch chan os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

func isPrivilegedUser() bool {
	return os.Geteuid() == 0
}
