//go:build windows

package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"
)

func notifyInterrupt(ch chan os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
}

func isPrivilegedUser() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
