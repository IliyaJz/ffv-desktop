//go:build linux

package tun

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
)

type RouteState struct {
	DeviceName      string
	OriginalGateway string
	OriginalIface   string
	UpstreamIPs     []net.IP
	DNSConfigured   bool
}

func DefaultTunDeviceName() string {
	return "ffv0"
}

func setupRoutesAndInterface(devName, ip, gw string, mtu int, upstreamIPs []net.IP, dns string, verbose bool) (*RouteState, error) {
	state := &RouteState{
		DeviceName:  devName,
		UpstreamIPs: upstreamIPs,
	}

	// 1. Assign IP address and MTU to TUN interface
	cidr := fmt.Sprintf("%s/24", ip)
	if out, err := exec.Command("ip", "addr", "add", cidr, "dev", devName).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ip addr add %s dev %s failed: %w (output: %s)", cidr, devName, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("ip", "link", "set", "dev", devName, "mtu", fmt.Sprintf("%d", mtu), "up").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ip link set %s up failed: %w (output: %s)", devName, err, strings.TrimSpace(string(out)))
	}
	if verbose {
		log.Printf("[TUN] configured %s: %s (mtu %d)", devName, cidr, mtu)
	}

	// 2. Discover default physical gateway & interface
	origGW, origIface, err := findDefaultGatewayLinux()
	if err != nil {
		log.Printf("[TUN] warning: discovering default gateway: %v", err)
	} else {
		state.OriginalGateway = origGW
		state.OriginalIface = origIface
		if verbose {
			log.Printf("[TUN] physical default gateway=%s interface=%s", origGW, origIface)
		}
	}

	// 3. Add host routes for upstream endpoints via original physical gateway
	if state.OriginalGateway != "" {
		for _, uip := range upstreamIPs {
			if uip.To4() != nil {
				ipStr := uip.String()
				args := []string{"route", "add", ipStr + "/32", "via", state.OriginalGateway}
				if state.OriginalIface != "" {
					args = append(args, "dev", state.OriginalIface)
				}
				_ = exec.Command("ip", args...).Run()
				if verbose {
					log.Printf("[TUN] added host route: %s/32 via %s", ipStr, state.OriginalGateway)
				}
			}
		}
	}

	// 4. Add split default routes
	if out, err := exec.Command("ip", "route", "add", "0.0.0.0/1", "dev", devName).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("adding route 0.0.0.0/1 via %s: %w (output: %s)", devName, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("ip", "route", "add", "128.0.0.0/1", "dev", devName).CombinedOutput(); err != nil {
		_ = exec.Command("ip", "route", "del", "0.0.0.0/1", "dev", devName).Run()
		return nil, fmt.Errorf("adding route 128.0.0.0/1 via %s: %w (output: %s)", devName, err, strings.TrimSpace(string(out)))
	}
	if verbose {
		log.Printf("[TUN] added split default routes: 0.0.0.0/1 and 128.0.0.0/1 dev %s", devName)
	}

	// 5. Configure DNS (using resolvectl if available)
	if dns != "" {
		if err := exec.Command("resolvectl", "dns", devName, dns).Run(); err == nil {
			_ = exec.Command("resolvectl", "default-route", devName, "true").Run()
			state.DNSConfigured = true
			if verbose {
				log.Printf("[TUN] configured DNS via resolvectl for %s to %s", devName, dns)
			}
		}
	}

	return state, nil
}

func teardownRoutesAndInterface(state *RouteState, verbose bool) {
	if state == nil {
		return
	}

	// 1. Remove split default routes
	_ = exec.Command("ip", "route", "del", "0.0.0.0/1", "dev", state.DeviceName).Run()
	_ = exec.Command("ip", "route", "del", "128.0.0.0/1", "dev", state.DeviceName).Run()

	// 2. Remove host routes to upstream IPs
	for _, uip := range state.UpstreamIPs {
		if uip.To4() != nil {
			_ = exec.Command("ip", "route", "del", uip.String()+"/32").Run()
		}
	}

	// 3. Revert resolvectl DNS if configured
	if state.DNSConfigured {
		_ = exec.Command("resolvectl", "revert", state.DeviceName).Run()
	}

	// 4. Bring down interface
	if state.DeviceName != "" {
		_ = exec.Command("ip", "link", "set", "dev", state.DeviceName, "down").Run()
		_ = exec.Command("ip", "link", "del", "dev", state.DeviceName).Run()
	}
}

func findDefaultGatewayLinux() (string, string, error) {
	cmd := exec.Command("ip", "route", "show", "default")
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		// Format: default via 192.168.1.1 dev eth0 ...
		for i := 0; i < len(fields)-2; i++ {
			if fields[i] == "via" {
				gw := fields[i+1]
				iface := ""
				for j := i + 2; j < len(fields)-1; j++ {
					if fields[j] == "dev" {
						iface = fields[j+1]
						break
					}
				}
				if !strings.HasPrefix(iface, "tun") && !strings.HasPrefix(iface, "ffv") && !strings.HasPrefix(iface, "foxy") {
					return gw, iface, nil
				}
			}
		}
	}
	return "", "", fmt.Errorf("default physical gateway not found")
}
