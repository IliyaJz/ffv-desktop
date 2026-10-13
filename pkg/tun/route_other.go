//go:build !darwin && !linux && !windows

package tun

import (
	"errors"
	"net"
	"runtime"
)

type RouteState struct {
	DeviceName string
}

func DefaultTunDeviceName() string {
	return "ffv0"
}

func setupRoutesAndInterface(devName, ip, gw string, mtu int, upstreamIPs []net.IP, dns string, verbose bool) (*RouteState, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("TUN mode on Windows requires Wintun driver. Please use Proxy-Only mode: ffv.exe --proxy-only (or in TUI press 'm' to switch to Proxy-Only)")
	}
	return nil, errors.New("TUN routing is only implemented on macOS and Linux")
}

func teardownRoutesAndInterface(state *RouteState, verbose bool) {
}
