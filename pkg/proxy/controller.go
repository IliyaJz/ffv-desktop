package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ffv/pkg/vpnclient"
)

const (
	defaultExitCheckURL         = "https://www.cloudflare.com/cdn-cgi/trace"
	maxExitCheckResponseSize    = 32 * 1024
	renewalCheckInterval        = 30 * time.Second
	minRenewalBuffer            = 5 * time.Minute
)

type ControllerConfig struct {
	ProxyURL        *url.URL
	SessionToken    string // explicit session token or empty if using saved login
	Country         string
	UpstreamDialer  *UpstreamDialer
	ConnectTimeout  time.Duration
	Verbose         bool
}

type ExitInfo struct {
	IP          string
	CountryCode string
	Latency     time.Duration
	ProbeHost   string
}

type Controller struct {
	cfg          ControllerConfig
	mu           sync.RWMutex
	session      *H2ProxySession
	pass         *vpnclient.ProxyPassInfo
	tokens       *vpnclient.SavedTokens
	stopChan     chan struct{}
	closeOnce    sync.Once
	isClosed     bool
}

func NewController(cfg ControllerConfig) (*Controller, error) {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 25 * time.Second
	}
	c := &Controller{
		cfg:      cfg,
		stopChan: make(chan struct{}),
	}

	// 1. Configure upstream proxy for control plane if specified
	if cfg.UpstreamDialer != nil && cfg.UpstreamDialer.ProxyURL != nil {
		transport := cfg.UpstreamDialer.BuildControlPlaneTransport()
		vpnclient.SetControlPlaneTransport(transport)
		if cfg.Verbose {
			log.Printf("[CONTROLLER] control-plane requests routed via upstream proxy %s", cfg.UpstreamDialer.ProxyURL.String())
		}
	}

	// 2. Obtain proxy pass token
	if cfg.SessionToken != "" {
		c.pass = &vpnclient.ProxyPassInfo{
			RawToken: cfg.SessionToken,
		}
		if cfg.Verbose {
			log.Printf("[CONTROLLER] using provided session token")
		}
	} else {
		// Load saved tokens
		tokens, err := vpnclient.LoadTokens()
		if err != nil {
			return nil, fmt.Errorf("could not load account tokens (run 'ffv login' first): %w", err)
		}
		c.tokens = tokens

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		pass, err := c.obtainProxyPass(ctx)
		if err != nil {
			return nil, fmt.Errorf("obtaining proxy pass from Guardian failed: %w", err)
		}
		c.pass = pass
		if cfg.Verbose {
			log.Printf("[CONTROLLER] Guardian proxy pass obtained, quota remaining: %s", pass.QuotaLeft)
		}
	}

	// 3. Connect upstream H2 session
	sess, err := NewH2ProxySession(cfg.ProxyURL, c.pass.RawToken, cfg.ConnectTimeout, cfg.UpstreamDialer)
	if err != nil {
		return nil, fmt.Errorf("establishing upstream HTTP/2 tunnel to %s: %w", cfg.ProxyURL.Host, err)
	}
	c.session = sess

	// Start background renewal loop
	go c.runRenewalLoop()

	return c, nil
}

func (c *Controller) OpenTunnel(authority string) (net.Conn, error) {
	c.mu.RLock()
	sess := c.session
	closed := c.isClosed
	c.mu.RUnlock()

	if closed {
		return nil, errors.New("controller is closed")
	}

	if sess == nil || !sess.IsAlive() {
		// Rebuild session if dead
		if err := c.rebuildSession(); err != nil {
			return nil, fmt.Errorf("rebuilding upstream session: %w", err)
		}
		c.mu.RLock()
		sess = c.session
		c.mu.RUnlock()
	}

	return sess.OpenTunnel(authority)
}

func (c *Controller) rebuildSession() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isClosed {
		return errors.New("controller closed")
	}

	if c.session != nil {
		_ = c.session.Close()
	}

	token := ""
	if c.pass != nil {
		token = c.pass.RawToken
	}

	sess, err := NewH2ProxySession(c.cfg.ProxyURL, token, c.cfg.ConnectTimeout, c.cfg.UpstreamDialer)
	if err != nil {
		return err
	}
	c.session = sess
	if c.cfg.Verbose {
		log.Printf("[CONTROLLER] upstream session rebuilt to %s", c.cfg.ProxyURL.Host)
	}
	return nil
}

func (c *Controller) runRenewalLoop() {
	ticker := time.NewTicker(renewalCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.checkAndRenew()
		}
	}
}

func (c *Controller) obtainProxyPass(ctx context.Context) (*vpnclient.ProxyPassInfo, error) {
	c.mu.Lock()
	tokens := c.tokens
	c.mu.Unlock()

	if tokens == nil {
		return nil, errors.New("no saved tokens available")
	}

	// 1. Refresh OAuth access token if expired according to local clock
	if !tokens.AccessTokenValid() {
		if c.cfg.Verbose {
			log.Printf("[CONTROLLER] access token expired, refreshing with Firefox Accounts...")
		}
		newTok, err := vpnclient.FxaRefreshToken(ctx, tokens.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("refreshing OAuth token: %w", err)
		}
		_ = vpnclient.SaveTokens(newTok)
		c.mu.Lock()
		tokens.AccessToken = newTok.AccessToken
		tokens.RefreshToken = newTok.RefreshToken
		tokens.ExpiresIn = newTok.ExpiresIn
		tokens.ObtainedAt = time.Now()
		c.mu.Unlock()
	}

	// 2. Attempt fetching proxy pass directly
	pass, err := vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
	if err == nil {
		return pass, nil
	}

	// If the failure was quota exceeded or something non-auth related, don't spam activation
	if errors.Is(err, vpnclient.ErrQuotaExceeded) {
		return nil, err
	}

	// 3. If token was invalid / forbidden (HTTP 401 or HTTP 403), Mozilla Guardian
	// either needs the free entitlement activated or the access token re-issued.
	if c.cfg.Verbose {
		log.Printf("[CONTROLLER] Guardian proxy pass fetch failed (%v), activating entitlement...", err)
	}

	// Try activating Guardian entitlement
	_, actErr := vpnclient.ActivateGuardian(ctx, "", tokens.AccessToken)
	if actErr == nil {
		// Entitlement activation succeeded! Retry FetchProxyPass
		pass, err = vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
		if err == nil {
			return pass, nil
		}
	} else if c.cfg.Verbose {
		log.Printf("[CONTROLLER] activation attempt with current token returned: %v", actErr)
	}

	// 4. If still rejected, force-refresh the OAuth token and re-activate
	if c.cfg.Verbose {
		log.Printf("[CONTROLLER] refreshing OAuth token with Firefox Accounts...")
	}
	newTok, refErr := vpnclient.FxaRefreshToken(ctx, tokens.RefreshToken)
	if refErr != nil {
		return nil, fmt.Errorf("OAuth token refresh failed (original Guardian error: %v): %w", err, refErr)
	}
	_ = vpnclient.SaveTokens(newTok)
	c.mu.Lock()
	tokens.AccessToken = newTok.AccessToken
	tokens.RefreshToken = newTok.RefreshToken
	tokens.ExpiresIn = newTok.ExpiresIn
	tokens.ObtainedAt = time.Now()
	c.mu.Unlock()

	// Activate with the fresh access token
	if _, actErr := vpnclient.ActivateGuardian(ctx, "", tokens.AccessToken); actErr != nil && c.cfg.Verbose {
		log.Printf("[CONTROLLER] activation with refreshed token: %v", actErr)
	}

	// Retry FetchProxyPass
	pass, passErr := vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
	if passErr != nil {
		return nil, fmt.Errorf("fetching proxy pass after token refresh and activation: %w", passErr)
	}
	return pass, nil
}

func (c *Controller) checkAndRenew() {
	c.mu.RLock()
	pass := c.pass
	tokens := c.tokens
	closed := c.isClosed
	c.mu.RUnlock()

	if closed || tokens == nil || pass == nil {
		return
	}

	// Check if proxy pass is expiring soon
	expiresAt := pass.ExpiresAt()
	if !expiresAt.IsZero() && time.Until(expiresAt) < minRenewalBuffer {
		if c.cfg.Verbose {
			log.Printf("[CONTROLLER] proxy pass expires in %s, renewing...", time.Until(expiresAt).Round(time.Second))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		newPass, err := c.obtainProxyPass(ctx)
		if err == nil {
			c.mu.Lock()
			c.pass = newPass
			if c.session != nil {
				_ = c.session.UpdateToken(newPass.RawToken)
			}
			c.mu.Unlock()
			if c.cfg.Verbose {
				log.Printf("[CONTROLLER] proxy pass renewed, expires at: %s", newPass.ExpiresAt().Format(time.RFC3339))
			}
		} else {
			log.Printf("[CONTROLLER] renewing proxy pass failed: %v", err)
		}
	}
}

func (c *Controller) VerifyExit(timeout time.Duration) (*ExitInfo, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, authority string) (net.Conn, error) {
			return c.OpenTunnel(authority)
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	start := time.Now()
	resp, err := client.Get(defaultExitCheckURL)
	if err != nil {
		return nil, fmt.Errorf("exit verification request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exit check returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExitCheckResponseSize))
	if err != nil {
		return nil, fmt.Errorf("reading exit check response: %w", err)
	}

	ip, countryCode, err := parseExitProbeResponse(body)
	if err != nil {
		return nil, err
	}

	return &ExitInfo{
		IP:          ip,
		CountryCode: countryCode,
		Latency:     time.Since(start),
		ProbeHost:   "cloudflare.com",
	}, nil
}

func parseExitProbeResponse(body []byte) (string, string, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			fields[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}

	ip := fields["ip"]
	country := fields["loc"]

	if ip == "" || country == "" {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			if v, ok := payload["ip"].(string); ok {
				ip = v
			}
			if v, ok := payload["country"].(string); ok {
				country = v
			}
		}
	}

	if ip == "" {
		return "", "", fmt.Errorf("could not parse exit IP from response: %s", string(body))
	}
	return ip, country, nil
}

func (c *Controller) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.isClosed = true
		close(c.stopChan)
		if c.session != nil {
			_ = c.session.Close()
		}
		c.mu.Unlock()
	})
	return nil
}
