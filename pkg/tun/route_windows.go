//go:build windows

package tun

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type RouteState struct {
	DeviceName      string
	OriginalGateway string
	OriginalIface   string
	UpstreamIPs     []net.IP
	SplitRoutes     bool
}

func DefaultTunDeviceName() string {
	return "ffv0"
}

// findDefaultGatewayWindows queries the Windows routing table using "route print 0.0.0.0"
// to discover the active default gateway IP and interface.
func findDefaultGatewayWindows() (string, string, error) {
	cmd := exec.Command("route", "print", "0.0.0.0")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("route print failed: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var bestGW, bestIface string
	lowestMetric := 999999

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		// Expected row format:
		// Destination     Netmask          Gateway       Interface  Metric
		// 0.0.0.0         0.0.0.0          192.168.1.1   192.168.1.50   25
		if len(fields) >= 5 && fields[0] == "0.0.0.0" && fields[1] == "0.0.0.0" {
			gw := fields[2]
			iface := fields[3]
			metricStr := fields[4]

			parsedGW := net.ParseIP(gw)
			// Ignore invalid, loopback, or TUN IPs (e.g. 10.8.0.x)
			if parsedGW == nil || parsedGW.IsLoopback() || strings.HasPrefix(gw, "10.8.") || gw == "0.0.0.0" {
				continue
			}

			metric, err := strconv.Atoi(metricStr)
			if err != nil {
				metric = 100
			}

			if metric < lowestMetric {
				lowestMetric = metric
				bestGW = gw
				bestIface = iface
			}
		}
	}

	if bestGW == "" {
		return "", "", fmt.Errorf("no physical default gateway found in routing table")
	}

	return bestGW, bestIface, nil
}

func setupRoutesAndInterface(devName, ip, gw string, mtu int, upstreamIPs []net.IP, dns string, verbose bool) (*RouteState, error) {
	state := &RouteState{
		DeviceName:  devName,
		UpstreamIPs: upstreamIPs,
	}

	// 1. Give Windows network stack a brief moment to register the new Wintun adapter
	time.Sleep(300 * time.Millisecond)

	// 2. Discover physical default gateway before adding any VPN routes
	origGW, origIface, err := findDefaultGatewayWindows()
	if err != nil {
		if verbose {
			log.Printf("[TUN] warning: discovering default gateway on Windows: %v", err)
		}
	} else {
		state.OriginalGateway = origGW
		state.OriginalIface = origIface
		if verbose {
			log.Printf("[TUN] physical default gateway=%s (interface IP=%s)", origGW, origIface)
		}
	}

	// 3. Configure IP address on the Wintun adapter using netsh
	mask := "255.255.255.0"
	setAddrCmd := exec.Command("netsh", "interface", "ipv4", "set", "address",
		fmt.Sprintf("name=%s", devName),
		"source=static",
		fmt.Sprintf("addr=%s", ip),
		fmt.Sprintf("mask=%s", mask),
		"gateway=none",
	)
	if out, err := setAddrCmd.CombinedOutput(); err != nil {
		// Fallback syntax without name=
		setAddrCmd2 := exec.Command("netsh", "interface", "ip", "set", "address", devName, "static", ip, mask)
		if out2, err2 := setAddrCmd2.CombinedOutput(); err2 != nil {
			return nil, fmt.Errorf("configuring IP address on %s via netsh failed: %v (output: %s / %s)", devName, err, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	if verbose {
		log.Printf("[TUN] configured %s: %s/24", devName, ip)
	}

	// 4. Set MTU if specified
	if mtu > 0 {
		_ = exec.Command("netsh", "interface", "ipv4", "set", "subinterface", devName, fmt.Sprintf("mtu=%d", mtu), "store=active").Run()
	}

	// 5. Configure DNS server if specified
	if dns != "" {
		_ = exec.Command("netsh", "interface", "ipv4", "set", "dnsservers", fmt.Sprintf("name=%s", devName), "source=static", fmt.Sprintf("address=%s", dns), "validate=no").Run()
		if verbose {
			log.Printf("[TUN] configured DNS %s on %s", dns, devName)
		}
	}

	// 6. Add host routes for upstream endpoints via the original physical gateway to prevent routing loops
	if state.OriginalGateway != "" {
		for _, uip := range upstreamIPs {
			if uip.To4() != nil {
				ipStr := uip.String()
				_ = exec.Command("route", "add", ipStr, "mask", "255.255.255.255", state.OriginalGateway, "metric", "1").Run()
				if verbose {
					log.Printf("[TUN] added host route: %s via %s", ipStr, state.OriginalGateway)
				}
			}
		}
	}

	// 7. Find interface index for route.exe
	var ifIndexStr string
	if iface, err := net.InterfaceByName(devName); err == nil {
		ifIndexStr = fmt.Sprintf("%d", iface.Index)
	}

	// 8. Add split default routes: 0.0.0.0/1 and 128.0.0.0/1 via TUN IP
	// This covers the entire IPv4 address space without deleting or overwriting the original default gateway
	r1Args := []string{"add", "0.0.0.0", "mask", "128.0.0.0", ip, "metric", "1"}
	r2Args := []string{"add", "128.0.0.0", "mask", "128.0.0.0", ip, "metric", "1"}
	if ifIndexStr != "" {
		r1Args = append(r1Args, "if", ifIndexStr)
		r2Args = append(r2Args, "if", ifIndexStr)
	}

	if out, err := exec.Command("route", r1Args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("adding route 0.0.0.0/1 failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("route", r2Args...).CombinedOutput(); err != nil {
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "128.0.0.0").Run()
		return nil, fmt.Errorf("adding route 128.0.0.0/1 failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	state.SplitRoutes = true
	if verbose {
		log.Printf("[TUN] added split default routes (0.0.0.0/1 and 128.0.0.0/1) via %s", ip)
	}

	return state, nil
}

func teardownRoutesAndInterface(state *RouteState, verbose bool) {
	if state == nil {
		return
	}

	// 1. Delete split routes
	if state.SplitRoutes {
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "128.0.0.0").Run()
		_ = exec.Command("route", "delete", "128.0.0.0", "mask", "128.0.0.0").Run()
		if verbose {
			log.Printf("[TUN] removed split default routes")
		}
	}

	// 2. Delete upstream endpoint host routes
	if state.OriginalGateway != "" {
		for _, uip := range state.UpstreamIPs {
			if uip.To4() != nil {
				_ = exec.Command("route", "delete", uip.String(), "mask", "255.255.255.255").Run()
			}
		}
		if verbose {
			log.Printf("[TUN] removed upstream host routes")
		}
	}
}
