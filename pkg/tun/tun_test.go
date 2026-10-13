package tun

import (
	"testing"
)

func TestResolveEndpointIPs(t *testing.T) {
	// IP address literal
	ips, err := resolveEndpointIPs("127.0.0.1:2499")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %v", ips)
	}

	// Empty endpoint
	emptyIPs, err := resolveEndpointIPs("")
	if err != nil || len(emptyIPs) != 0 {
		t.Errorf("expected empty IPs for empty string, got %v, err: %v", emptyIPs, err)
	}
}

func TestNewRunnerDefaults(t *testing.T) {
	r := NewRunner(Config{})
	if r.cfg.MTU != DefaultTunMTU {
		t.Errorf("expected MTU %d, got %d", DefaultTunMTU, r.cfg.MTU)
	}
	if r.cfg.IPv4 != DefaultTunAddress {
		t.Errorf("expected IP %s, got %s", DefaultTunAddress, r.cfg.IPv4)
	}
	if r.cfg.Gateway != DefaultTunGateway {
		t.Errorf("expected Gateway %s, got %s", DefaultTunGateway, r.cfg.Gateway)
	}
	if r.cfg.DNS != DefaultDNS {
		t.Errorf("expected DNS %s, got %s", DefaultDNS, r.cfg.DNS)
	}
}
