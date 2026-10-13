package tun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/device/tun"
	tun2socksLog "github.com/xjasonlyu/tun2socks/v2/log"
	"github.com/xjasonlyu/tun2socks/v2/core/option"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	_ "github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
	"go.uber.org/zap"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func init() {
	// Silence verbose packet-level logging by default
	tun2socksLog.SetLogger(zap.NewNop())
}

const (
	DefaultTunMTU     = 1420
	DefaultTunAddress = "10.8.0.2"
	DefaultTunGateway = "10.8.0.1"
	DefaultDNS        = "1.1.1.1"
)

type Config struct {
	TunName          string // Interface name, e.g. "ffv0" on Linux or "utun" on macOS
	MTU              int
	IPv4             string // e.g. "10.8.0.2"
	Gateway          string // e.g. "10.8.0.1"
	DNS              string // e.g. "1.1.1.1"
	Socks5Addr       string // e.g. "127.0.0.1:1088"
	UpstreamEndpoint string // host:port of Fastly edge or upstream proxy
	Verbose          bool
}

type Runner struct {
	cfg        Config
	dev        device.Device
	stack      *stack.Stack
	actualName string
	routeState *RouteState
	mu         sync.Mutex
	isStarted  bool
	isClosed   bool
}

func NewRunner(cfg Config) *Runner {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultTunMTU
	}
	if cfg.IPv4 == "" {
		cfg.IPv4 = DefaultTunAddress
	}
	if cfg.Gateway == "" {
		cfg.Gateway = DefaultTunGateway
	}
	if cfg.DNS == "" {
		cfg.DNS = DefaultDNS
	}
	return &Runner{
		cfg: cfg,
	}
}

// Start brings up the TUN interface, configures routes, and hooks up tun2socks.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isStarted {
		return errors.New("TUN runner already started")
	}

	// 1. Resolve upstream endpoint IP(s) so we can create host routes to prevent routing loops
	upstreamIPs, err := resolveEndpointIPs(r.cfg.UpstreamEndpoint)
	if err != nil {
		log.Printf("[TUN] warning: resolving upstream endpoint %s: %v", r.cfg.UpstreamEndpoint, err)
	}

	// 2. Open TUN device
	devName := r.cfg.TunName
	if devName == "" {
		devName = DefaultTunDeviceName()
	}

	dev, err := tun.Open(devName, uint32(r.cfg.MTU))
	if err != nil {
		if runtime.GOOS == "windows" {
			return fmt.Errorf("opening TUN device %q: %w\n[NOTE] On Windows, TUN mode requires administrator privileges and 'wintun.dll' placed next to ffv.exe. Alternatively, use Proxy-Only mode: 'ffv connect --proxy-only'", devName, err)
		}
		return fmt.Errorf("opening TUN device %q: %w (ensure you are running with root/sudo)", devName, err)
	}
	r.dev = dev
	r.actualName = dev.Name()
	if r.cfg.Verbose {
		log.Printf("[TUN] device opened: %s (MTU %d)", r.actualName, r.cfg.MTU)
	}

	// 3. Configure interface IP address & routing
	routeState, err := setupRoutesAndInterface(r.actualName, r.cfg.IPv4, r.cfg.Gateway, r.cfg.MTU, upstreamIPs, r.cfg.DNS, r.cfg.Verbose)
	if err != nil {
		dev.Close()
		return fmt.Errorf("configuring interface & routing: %w", err)
	}
	r.routeState = routeState

	// 4. Configure tun2socks proxy
	socksURL, err := url.Parse(fmt.Sprintf("socks5://%s", r.cfg.Socks5Addr))
	if err != nil {
		r.cleanupRoutes()
		dev.Close()
		return fmt.Errorf("invalid socks5 proxy URL: %w", err)
	}

	// Configure tun2socks internal logger
	if r.cfg.Verbose {
		tun2socksLog.SetLogger(zap.Must(zap.NewDevelopment()))
	} else {
		tun2socksLog.SetLogger(zap.NewNop())
	}

	socksProxy, err := proxy.Parse(socksURL)
	if err != nil {
		r.cleanupRoutes()
		dev.Close()
		return fmt.Errorf("initializing tun2socks proxy: %w", err)
	}
	tunnel.T().SetProxy(socksProxy)

	// 5. Create gVisor user-space network stack with optimized TCP tuning (CUBIC + Auto-tuned buffers)
	stk, err := core.CreateStack(&core.Config{
		LinkEndpoint:     dev,
		TransportHandler: tunnel.T(),
		Options: []option.Option{
			option.WithTCPCongestionControl("cubic"),
			option.WithTCPModerateReceiveBuffer(true),
			option.WithTCPReceiveBufferSizeRange(4096, 524288, 4194304),
			option.WithTCPSendBufferSizeRange(4096, 524288, 4194304),
		},
	})
	if err != nil {
		r.cleanupRoutes()
		dev.Close()
		return fmt.Errorf("creating network stack: %w", err)
	}
	r.stack = stk
	r.isStarted = true

	log.Printf("[TUN] VPN tunnel active on interface %s (IP %s, Gateway %s)", r.actualName, r.cfg.IPv4, r.cfg.Gateway)
	return nil
}

// Stop cleanly removes routes, restores DNS, and tears down the TUN device.
func (r *Runner) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isStarted || r.isClosed {
		return nil
	}
	r.isClosed = true

	log.Printf("[TUN] shutting down TUN interface %s...", r.actualName)

	// 1. Immediately restore routing and DNS first
	r.cleanupRoutes()

	// 2. Close TUN device to unblock any pending read calls on the descriptor
	if r.dev != nil {
		r.dev.Close()
	}

	// 3. Close gVisor network stack with drain timeout
	if r.stack != nil {
		r.stack.Close()
		done := make(chan struct{})
		go func() {
			r.stack.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(1 * time.Second):
		}
	}

	log.Printf("[TUN] cleaned up routes and interface successfully")
	return nil
}

func (r *Runner) cleanupRoutes() {
	if r.routeState != nil {
		teardownRoutesAndInterface(r.routeState, r.cfg.Verbose)
		r.routeState = nil
	}
}

func resolveEndpointIPs(endpoint string) ([]net.IP, error) {
	if endpoint == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	var ipv4s []net.IP
	for _, a := range addrs {
		if a.To4() != nil {
			ipv4s = append(ipv4s, a)
		}
	}
	return ipv4s, nil
}

func parseNetip(s string) (netip.Addr, error) {
	return netip.ParseAddr(s)
}
