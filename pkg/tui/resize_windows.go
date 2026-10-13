//go:build windows

package tui

import (
	"os"

	"golang.org/x/sys/windows"
)

func registerResizeSignal(ch chan os.Signal) {
	// Windows console resize is polled by the ticker or handled by console events
}

func unregisterResizeSignal(ch chan os.Signal) {
}

func isPrivileged() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
