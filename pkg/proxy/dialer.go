package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

var (
	ErrUnsupportedProxyScheme = errors.New("unsupported proxy scheme")
	ErrProxyConnectFailed     = errors.New("upstream HTTP proxy connect failed")
)

// UpstreamDialer handles establishing TCP connections, either directly
// or chained through an upstream HTTP, HTTPS, or SOCKS5 proxy.
type UpstreamDialer struct {
	ProxyURL *url.URL
	Timeout  time.Duration
}

// NewUpstreamDialer parses a proxy URL string (e.g. socks5://127.0.0.1:1080 or http://127.0.0.1:8080)
// and returns an UpstreamDialer. If raw is empty, a direct dialer is returned.
func NewUpstreamDialer(raw string, timeout time.Duration) (*UpstreamDialer, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &UpstreamDialer{Timeout: timeout}, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing proxy host in %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
		u.Scheme = scheme
		return &UpstreamDialer{ProxyURL: u, Timeout: timeout}, nil
	default:
		return nil, fmt.Errorf("%w %q (supported: http, https, socks5, socks5h)", ErrUnsupportedProxyScheme, u.Scheme)
	}
}

// DialContext establishes a connection to target (host:port).
func (d *UpstreamDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	baseDialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	if d.ProxyURL == nil {
		return baseDialer.DialContext(ctx, network, target)
	}

	switch d.ProxyURL.Scheme {
	case "socks5", "socks5h":
		return d.dialSOCKS5(ctx, baseDialer, network, target)
	case "http", "https":
		return d.dialHTTPConnect(ctx, baseDialer, target)
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupportedProxyScheme, d.ProxyURL.Scheme)
	}
}

func (d *UpstreamDialer) dialSOCKS5(ctx context.Context, baseDialer *net.Dialer, network, target string) (net.Conn, error) {
	var auth *proxy.Auth
	if d.ProxyURL.User != nil {
		password, _ := d.ProxyURL.User.Password()
		auth = &proxy.Auth{
			User:     d.ProxyURL.User.Username(),
			Password: password,
		}
	}

	socksProxy, err := proxy.SOCKS5("tcp", d.ProxyURL.Host, auth, baseDialer)
	if err != nil {
		return nil, fmt.Errorf("initializing socks5 proxy: %w", err)
	}

	contextDialer, ok := socksProxy.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks5 proxy dialer does not support context")
	}

	return contextDialer.DialContext(ctx, network, target)
}

func (d *UpstreamDialer) dialHTTPConnect(ctx context.Context, baseDialer *net.Dialer, target string) (net.Conn, error) {
	proxyHost := d.ProxyURL.Host
	if d.ProxyURL.Port() == "" {
		if d.ProxyURL.Scheme == "https" {
			proxyHost = net.JoinHostPort(d.ProxyURL.Hostname(), "443")
		} else {
			proxyHost = net.JoinHostPort(d.ProxyURL.Hostname(), "80")
		}
	}

	var conn net.Conn
	var err error

	if d.ProxyURL.Scheme == "https" {
		tlsDialer := &tls.Dialer{
			NetDialer: baseDialer,
			Config: &tls.Config{
				ServerName: d.ProxyURL.Hostname(),
				MinVersion: tls.VersionTLS12,
			},
		}
		conn, err = tlsDialer.DialContext(ctx, "tcp", proxyHost)
	} else {
		conn, err = baseDialer.DialContext(ctx, "tcp", proxyHost)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to http proxy %s: %w", proxyHost, err)
	}

	// Build CONNECT request
	req, err := http.NewRequestWithContext(ctx, http.MethodConnect, "//"+target, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Host = target
	req.Header.Set("User-Agent", "FoxyVPN-CLI/1.0")
	req.Header.Set("Proxy-Connection", "Keep-Alive")

	if d.ProxyURL.User != nil {
		user := d.ProxyURL.User.Username()
		password, _ := d.ProxyURL.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
		req.Header.Set("Proxy-Authorization", "Basic "+auth)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending CONNECT to proxy %s: %w", proxyHost, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading CONNECT response from proxy %s: %w", proxyHost, err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("%w %s (status %s)", ErrProxyConnectFailed, proxyHost, resp.Status)
	}

	// If bufio.Reader buffered any bytes beyond the HTTP headers, wrap the conn
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, br: br}, nil
	}
	return conn, nil
}

// BuildControlPlaneTransport returns an http.Transport configured to use this upstream proxy
// for all REST API calls (Firefox Accounts, Guardian, Server List).
func (d *UpstreamDialer) BuildControlPlaneTransport() *http.Transport {
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: 30 * time.Second,
	}

	if d.ProxyURL == nil {
		transport.DialContext = d.DialContext
		return transport
	}

	switch d.ProxyURL.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(d.ProxyURL)
		transport.DialContext = (&net.Dialer{Timeout: d.Timeout}).DialContext
	case "socks5", "socks5h":
		transport.DialContext = d.DialContext
	}
	return transport
}

type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	if c.br.Buffered() > 0 {
		return c.br.Read(b)
	}
	return c.Conn.Read(b)
}

func (c *bufferedConn) ReadByte() (byte, error) {
	return c.br.ReadByte()
}

func (c *bufferedConn) WriteTo(w io.Writer) (int64, error) {
	return c.br.WriteTo(w)
}
