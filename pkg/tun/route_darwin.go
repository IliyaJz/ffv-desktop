//go:build darwin

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
	OriginalDNS     []string
	DNSService      string
}

func DefaultTunDeviceName() string {
	return "utun"
}

func setupRoutesAndInterface(devName, ip, gw string, mtu int, upstreamIPs []net.IP, dns string, verbose bool) (*RouteState, error) {
	state := &RouteState{
		DeviceName:  devName,
		UpstreamIPs: upstreamIPs,
	}

	// 1. Configure utun interface IP and MTU
	cmd := exec.Command("ifconfig", devName, "inet", ip, gw, "netmask", "255.255.255.0", "mtu", fmt.Sprintf("%d", mtu), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ifconfig %s failed: %w (output: %s)", devName, err, strings.TrimSpace(string(out)))
	}
	if verbose {
		log.Printf("[TUN] configured %s: inet %s -> %s (mtu %d)", devName, ip, gw, mtu)
	}

	// 2. Discover default gateway & interface
	origGW, origIface, err := findDefaultGatewayDarwin()
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
				_ = exec.Command("route", "-q", "-n", "add", "-host", ipStr, state.OriginalGateway).Run()
				if verbose {
					log.Printf("[TUN] added host route: %s -> %s", ipStr, state.OriginalGateway)
				}
			}
		}
	}

	// 4. Add split default routes through TUN gateway
	if err := exec.Command("route", "-q", "-n", "add", "-net", "0.0.0.0/1", gw).Run(); err != nil {
		return nil, fmt.Errorf("adding route 0.0.0.0/1 via %s: %w", gw, err)
	}
	if err := exec.Command("route", "-q", "-n", "add", "-net", "128.0.0.0/1", gw).Run(); err != nil {
		_ = exec.Command("route", "-q", "-n", "delete", "-net", "0.0.0.0/1").Run()
		return nil, fmt.Errorf("adding route 128.0.0.0/1 via %s: %w", gw, err)
	}
	if verbose {
		log.Printf("[TUN] added split default routes: 0.0.0.0/1 and 128.0.0.0/1 -> %s", gw)
	}

	// 5. Configure DNS
	if dns != "" {
		service, origDNS := configureDarwinDNS(dns, verbose)
		state.DNSService = service
		state.OriginalDNS = origDNS
	}

	return state, nil
}

func teardownRoutesAndInterface(state *RouteState, verbose bool) {
	if state == nil {
		return
	}

	// 1. Remove split default routes
	_ = exec.Command("route", "-q", "-n", "delete", "-net", "0.0.0.0/1").Run()
	_ = exec.Command("route", "-q", "-n", "delete", "-net", "128.0.0.0/1").Run()

	// 2. Remove host routes to upstream IPs
	for _, uip := range state.UpstreamIPs {
		if uip.To4() != nil {
			_ = exec.Command("route", "-q", "-n", "delete", "-host", uip.String()).Run()
		}
	}

	// 3. Restore DNS
	if state.DNSService != "" {
		restoreDarwinDNS(state.DNSService, state.OriginalDNS, verbose)
	}

	// 4. Bring down interface
	if state.DeviceName != "" {
		_ = exec.Command("ifconfig", state.DeviceName, "down").Run()
	}
}

func findDefaultGatewayDarwin() (string, string, error) {
	cmd := exec.Command("netstat", "-nr", "-f", "inet")
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "default" {
			gw := fields[1]
			iface := fields[3]
			// Avoid matching utun if multiple VPNs exist
			if !strings.HasPrefix(iface, "utun") {
				return gw, iface, nil
			}
		}
	}
	return "", "", fmt.Errorf("default physical gateway not found")
}

func configureDarwinDNS(dns string, verbose bool) (string, []string) {
	// Find primary active service (usually Wi-Fi or Ethernet)
	cmd := exec.Command("networksetup", "-listallnetworkservices")
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}

	var service string
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.Contains(trimmed, "*") || trimmed == "" {
			continue
		}
		// Check if service has an IP address assigned
		infoCmd := exec.Command("networksetup", "-getinfo", trimmed)
		if infoOut, err := infoCmd.Output(); err == nil {
			if strings.Contains(string(infoOut), "IP address:") && !strings.Contains(string(infoOut), "IP address: none") {
				service = trimmed
				break
			}
		}
	}

	if service == "" {
		// Fallback to "Wi-Fi"
		service = "Wi-Fi"
	}

	// Read existing DNS
	var origDNS []string
	getCmd := exec.Command("networksetup", "-getdnsservers", service)
	if getOut, err := getCmd.Output(); err == nil {
		s := strings.TrimSpace(string(getOut))
		if !strings.Contains(s, "aren't any") && s != "" {
			origDNS = strings.Fields(s)
		}
	}

	// Set new DNS
	_ = exec.Command("networksetup", "-setdnsservers", service, dns).Run()
	if verbose {
		log.Printf("[TUN] set DNS on %s to %s (saved original: %v)", service, dns, origDNS)
	}

	return service, origDNS
}

func restoreDarwinDNS(service string, origDNS []string, verbose bool) {
	if len(origDNS) > 0 {
		args := append([]string{"-setdnsservers", service}, origDNS...)
		_ = exec.Command("networksetup", args...).Run()
	} else {
		_ = exec.Command("networksetup", "-setdnsservers", service, "empty").Run()
	}
	if verbose {
		log.Printf("[TUN] restored DNS on %s", service)
	}
}
