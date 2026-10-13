package tui

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ffv/pkg/proxy"
	"ffv/pkg/tun"
	"ffv/pkg/vpnclient"
	"golang.org/x/term"
)

type ConnectionStatus int

const (
	StatusDisconnected ConnectionStatus = iota
	StatusConnecting
	StatusConnected
	StatusDisconnecting
	StatusError
)

type LogEntry struct {
	Timestamp time.Time
	Message   string
	IsError   bool
}

type AppConfig struct {
	InitialCountry string
	UseTun         bool
	ListenAddr     string
	UpstreamProxy  string
	DNS            string
	Verbose        bool
}

type App struct {
	cfg             AppConfig
	mu              sync.RWMutex
	status          ConnectionStatus
	statusMessage   string
	selectedCountry string
	countries       []vpnclient.Country
	activeServer    string
	exitInfo        *proxy.ExitInfo
	connectedAt     time.Time
	quotaRemaining  string
	logs            []LogEntry
	activeTab       int // 0: Dashboard, 1: Locations, 2: Logs

	// Locations tab search & cursor
	locationCursor int
	locationFilter string
	isSearching    bool

	// Active VPN objects
	controller  *proxy.Controller
	tunRunner   *tun.Runner
	socksServer *proxy.SocksServer

	// Terminal state
	termWidth    int
	termHeight   int
	oldTermState *term.State
	clearScreen  atomic.Bool

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
}

func NewApp(cfg AppConfig) *App {
	if cfg.InitialCountry == "" {
		cfg.InitialCountry = "FR"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:1088"
	}
	if cfg.DNS == "" {
		cfg.DNS = "1.1.1.1"
	}

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		cfg:             cfg,
		status:          StatusDisconnected,
		selectedCountry: strings.ToUpper(cfg.InitialCountry),
		ctx:             ctx,
		cancel:          cancel,
		termWidth:       80,
		termHeight:      24,
	}

	app.clearScreen.Store(true)
	app.addLog("FFV TUI initialized. Ready to connect.", false)
	return app
}

func (a *App) Run() error {
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return fmt.Errorf("standard input is not a terminal")
	}

	// 1. Enter raw mode
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("entering raw mode: %w", err)
	}
	a.oldTermState = oldState

	// 2. Enter alternate screen & hide cursor
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l")

	// Ensure screen is restored and tunnel is cleanly disconnected on exit
	defer func() {
		// Restore terminal immediately
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		_ = term.Restore(stdinFd, oldState)
		a.disconnect()
	}()

	// 3. Detect initial terminal size
	if tw, th, err := term.GetSize(stdinFd); err == nil {
		a.termWidth = tw
		a.termHeight = th
	}

	// 4. Background tasks: load server list & quota
	go a.loadInitialData()

	// 5. Setup resize listener
	sigwinch := make(chan os.Signal, 2)
	registerResizeSignal(sigwinch)
	defer unregisterResizeSignal(sigwinch)

	// 6. Keyboard input loop
	keyEvents := PollKeys(a.ctx, os.Stdin)

	// 7. Render ticker (200ms)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	// Initial render
	os.Stdout.WriteString(a.Render())

	for {
		select {
		case <-a.ctx.Done():
			return nil

		case <-sigwinch:
			if tw, th, err := term.GetSize(stdinFd); err == nil {
				a.mu.Lock()
				a.termWidth = tw
				a.termHeight = th
				a.clearScreen.Store(true)
				a.mu.Unlock()
				os.Stdout.WriteString(a.Render())
			}

		case <-ticker.C:
			os.Stdout.WriteString(a.Render())

		case key := <-keyEvents:
			if shouldQuit := a.handleKey(key); shouldQuit {
				return nil
			}
			os.Stdout.WriteString(a.Render())
		}
	}
}

func (a *App) handleKey(k KeyEvent) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 1. Universal Quit: Ctrl+C, Ctrl+Q, Ctrl+D ALWAYS exits immediately
	if k.Type == KeyCtrlC || k.Type == KeyCtrlQ || k.Type == KeyCtrlD {
		return true // Exit TUI
	}

	// 2. Ensure isSearching is only ever active when on Locations tab (tab 1)
	if a.activeTab != 1 {
		a.isSearching = false
	}

	// 3. Searching mode in Locations tab
	if a.isSearching {
		switch k.Type {
		case KeyTab:
			a.isSearching = false
			a.activeTab = (a.activeTab + 1) % 3
			a.clearScreen.Store(true)
			return false

		case KeyBackTab:
			a.isSearching = false
			a.activeTab = (a.activeTab + 2) % 3
			a.clearScreen.Store(true)
			return false

		case KeyEsc:
			a.isSearching = false
			a.clearScreen.Store(true)
			return false

		case KeyEnter:
			a.isSearching = false
			filtered := a.filteredCountriesLocked()
			if len(filtered) > 0 && a.locationCursor < len(filtered) {
				sel := filtered[a.locationCursor]
				a.selectedCountry = sel.Code
				a.addLog(fmt.Sprintf("Selected location: %s (%s)", sel.Name, sel.Code), false)
				a.activeTab = 0 // Switch back to dashboard
				a.clearScreen.Store(true)
				go a.reconnect()
			}
			return false

		case KeyUp, KeyDown, KeyPageUp, KeyPageDown:
			// Navigating stops typing in search
			a.isSearching = false

		case KeyBackspace:
			if len(a.locationFilter) > 0 {
				a.locationFilter = a.locationFilter[:len(a.locationFilter)-1]
				a.locationCursor = 0
			}
			return false

		case KeyRune:
			// If user pressed 'q' or 'Q' and filter is empty, exit TUI!
			if (k.Rune == 'q' || k.Rune == 'Q') && a.locationFilter == "" {
				return true // Exit TUI
			}
			a.locationFilter += string(k.Rune)
			a.locationCursor = 0
			return false
		}
	}

	// 4. Standard navigation
	switch k.Type {
	case KeyRune:
		switch k.Rune {
		case 'q', 'Q':
			return true // Exit TUI immediately!
		case 'm', 'M':
			// Toggle TUN vs Proxy-Only mode
			if !a.cfg.UseTun && !isPrivileged() {
				a.addLog("Cannot switch to TUN mode: Administrator / root privileges required", true)
				return false
			}
			a.cfg.UseTun = !a.cfg.UseTun
			modeName := "Proxy-Only Mode"
			if a.cfg.UseTun {
				modeName = "System-Wide TUN Mode"
			}
			a.addLog(fmt.Sprintf("Switched to %s", modeName), false)
			if a.status == StatusConnected {
				go a.reconnect()
			}
			return false
		case 'r', 'R':
			a.addLog("Refreshing servers and account quota...", false)
			go a.loadInitialData()
			return false
		case '/':
			if a.activeTab == 1 {
				a.isSearching = true
			} else {
				a.activeTab = 1
				a.clearScreen.Store(true)
				a.isSearching = true
			}
			return false
		case '1':
			if a.activeTab != 0 {
				a.activeTab = 0
				a.clearScreen.Store(true)
			}
			return false
		case '2':
			if a.activeTab != 1 {
				a.activeTab = 1
				a.clearScreen.Store(true)
			}
			return false
		case '3':
			if a.activeTab != 2 {
				a.activeTab = 2
				a.clearScreen.Store(true)
			}
			return false
		}

	case KeyTab:
		a.activeTab = (a.activeTab + 1) % 3
		a.clearScreen.Store(true)
		return false

	case KeyBackTab:
		a.activeTab = (a.activeTab + 2) % 3
		a.clearScreen.Store(true)
		return false

	case KeySpace:
		if a.status == StatusConnected {
			go a.disconnect()
		} else if a.status == StatusDisconnected || a.status == StatusError {
			go a.connect()
		}
		return false

	case KeyEnter:
		if a.activeTab == 1 {
			// Connect to selected country from list
			filtered := a.filteredCountriesLocked()
			if len(filtered) > 0 && a.locationCursor < len(filtered) {
				sel := filtered[a.locationCursor]
				a.selectedCountry = sel.Code
				a.addLog(fmt.Sprintf("Selected location: %s (%s)", sel.Name, sel.Code), false)
				a.activeTab = 0 // Switch back to dashboard
				a.clearScreen.Store(true)
				go a.reconnect()
			}
		} else {
			if a.status == StatusConnected {
				go a.disconnect()
			} else if a.status == StatusDisconnected || a.status == StatusError {
				go a.connect()
			}
		}
		return false

	case KeyUp:
		if a.activeTab == 1 {
			if a.locationCursor > 0 {
				a.locationCursor--
			}
		}
		return false

	case KeyDown:
		if a.activeTab == 1 {
			filtered := a.filteredCountriesLocked()
			if a.locationCursor < len(filtered)-1 {
				a.locationCursor++
			}
		}
		return false

	case KeyPageUp:
		if a.activeTab == 1 {
			a.locationCursor -= 10
			if a.locationCursor < 0 {
				a.locationCursor = 0
			}
		}
		return false

	case KeyPageDown:
		if a.activeTab == 1 {
			filtered := a.filteredCountriesLocked()
			a.locationCursor += 10
			if a.locationCursor >= len(filtered) {
				a.locationCursor = len(filtered) - 1
			}
			if a.locationCursor < 0 {
				a.locationCursor = 0
			}
		}
		return false

	case KeyEsc:
		if a.locationFilter != "" {
			a.locationFilter = ""
			a.locationCursor = 0
		} else if a.activeTab != 0 {
			a.activeTab = 0
			a.clearScreen.Store(true)
		}
		return false
	}

	return false
}

func (a *App) connect() {
	a.mu.Lock()
	if a.status == StatusConnecting || a.status == StatusConnected {
		a.mu.Unlock()
		return
	}
	a.status = StatusConnecting
	a.statusMessage = ""
	country := a.selectedCountry
	useTun := a.cfg.UseTun
	listen := a.cfg.ListenAddr
	upstreamProxyStr := a.cfg.UpstreamProxy
	dnsIP := a.cfg.DNS
	verbose := a.cfg.Verbose
	countries := a.countries
	a.addLog(fmt.Sprintf("Connecting to Fastly edge in %s...", country), false)
	a.mu.Unlock()

	// 1. Configure upstream proxy if provided
	var dialer *proxy.UpstreamDialer
	if upstreamProxyStr != "" {
		d, err := proxy.NewUpstreamDialer(upstreamProxyStr, 20*time.Second)
		if err != nil {
			a.failConnect(fmt.Sprintf("invalid upstream proxy: %v", err))
			return
		}
		dialer = d
		vpnclient.SetControlPlaneTransport(d.BuildControlPlaneTransport())
	}

	// 2. Select server node
	targetHost, err := vpnclient.SelectServer(countries, country, "")
	if err != nil {
		// Try fetching serverlist afresh
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fresh, fErr := vpnclient.FetchServerList(ctx)
		cancel()
		if fErr == nil {
			a.mu.Lock()
			a.countries = fresh
			a.mu.Unlock()
			targetHost, err = vpnclient.SelectServer(fresh, country, "")
		}
		if err != nil {
			a.failConnect(fmt.Sprintf("locating server node for %s: %v", country, err))
			return
		}
	}

	a.mu.Lock()
	a.activeServer = targetHost
	a.addLog(fmt.Sprintf("Selected Fastly node: %s", targetHost), false)
	a.mu.Unlock()

	// Parse proxy URL
	proxyURL, err := url.Parse("https://" + targetHost)
	if err != nil {
		a.failConnect(fmt.Sprintf("invalid server URL: %v", err))
		return
	}

	// 3. Create controller & H2 session
	ctrl, err := proxy.NewController(proxy.ControllerConfig{
		ProxyURL:       proxyURL,
		Country:        country,
		UpstreamDialer: dialer,
		ConnectTimeout: 25 * time.Second,
		Verbose:        verbose,
	})
	if err != nil {
		a.failConnect(fmt.Sprintf("establishing tunnel session: %v", err))
		return
	}

	// 4. Start local SOCKS5 proxy server
	socksSrv := proxy.NewSocksServer(ctrl, 10*time.Second, 0, verbose)
	go func() {
		_ = socksSrv.ListenAndServe(listen)
	}()

	// 5. Start TUN mode if requested
	var tunRun *tun.Runner
	if useTun {
		if !isPrivileged() {
			a.addLog("Administrator / root privileges required for TUN mode -> falling back to Proxy-Only", true)
			useTun = false
			a.mu.Lock()
			a.cfg.UseTun = false
			a.mu.Unlock()
		} else {
			a.addLog(fmt.Sprintf("Activating TUN interface (DNS: %s)...", dnsIP), false)
			upstreamEndpoint := targetHost
			if dialer != nil && dialer.ProxyURL != nil {
				upstreamEndpoint = dialer.ProxyURL.Host
			}

			tunRun = tun.NewRunner(tun.Config{
				MTU:              tun.DefaultTunMTU,
				IPv4:             tun.DefaultTunAddress,
				Gateway:          tun.DefaultTunGateway,
				DNS:              dnsIP,
				Socks5Addr:       listen,
				UpstreamEndpoint: upstreamEndpoint,
				Verbose:          verbose,
			})

			if err := tunRun.Start(context.Background()); err != nil {
				socksSrv.Close()
				ctrl.Close()
				a.failConnect(fmt.Sprintf("activating TUN interface: %v", err))
				return
			}
		}
	}

	// 6. Verify Exit IP
	exitInfo, _ := ctrl.VerifyExit(8 * time.Second)

	a.mu.Lock()
	a.controller = ctrl
	a.socksServer = socksSrv
	a.tunRunner = tunRun
	a.exitInfo = exitInfo
	a.status = StatusConnected
	a.connectedAt = time.Now()
	if exitInfo != nil {
		a.addLog(fmt.Sprintf("Connected! Exit IP: %s (%s, %s)", exitInfo.IP, exitInfo.CountryCode, exitInfo.Latency.Round(time.Millisecond)), false)
	} else {
		a.addLog("Tunnel established and active!", false)
	}
	a.mu.Unlock()
}

func (a *App) failConnect(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status = StatusError
	a.statusMessage = msg
	a.addLog(msg, true)
}

func (a *App) disconnect() {
	a.mu.Lock()
	if a.status == StatusDisconnected {
		a.mu.Unlock()
		return
	}
	a.status = StatusDisconnecting
	a.addLog("Disconnecting tunnel...", false)

	tunRun := a.tunRunner
	ctrl := a.controller
	socks := a.socksServer

	a.tunRunner = nil
	a.controller = nil
	a.socksServer = nil
	a.exitInfo = nil
	a.mu.Unlock()

	if tunRun != nil {
		_ = tunRun.Stop()
	}
	if socks != nil {
		_ = socks.Close()
	}
	if ctrl != nil {
		_ = ctrl.Close()
	}

	a.mu.Lock()
	a.status = StatusDisconnected
	a.addLog("Disconnected cleanly.", false)
	a.mu.Unlock()
}

func (a *App) reconnect() {
	a.disconnect()
	time.Sleep(300 * time.Millisecond)
	a.connect()
}

func (a *App) loadInitialData() {
	// 1. Fetch server list
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	countries, err := vpnclient.FetchServerList(ctx)
	cancel()
	if err == nil && len(countries) > 0 {
		sort.Slice(countries, func(i, j int) bool {
			return countries[i].Name < countries[j].Name
		})
		a.mu.Lock()
		a.countries = countries
		a.addLog(fmt.Sprintf("Loaded %d global VPN locations", len(countries)), false)
		a.mu.Unlock()
	}

	// 2. Load quota from tokens
	tokens, err := vpnclient.LoadTokens()
	if err == nil && tokens != nil {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		pass, passErr := vpnclient.FetchProxyPass(ctx2, "", tokens.AccessToken)
		cancel2()
		if passErr == nil && pass != nil {
			a.mu.Lock()
			if pass.QuotaLeft != "" {
				a.quotaRemaining = pass.QuotaLeft
			}
			a.mu.Unlock()
		}
	}
}

func (a *App) addLog(msg string, isError bool) {
	entry := LogEntry{
		Timestamp: time.Now(),
		Message:   msg,
		IsError:   isError,
	}
	a.logs = append(a.logs, entry)
	if len(a.logs) > 100 {
		a.logs = a.logs[len(a.logs)-100:]
	}
}

func (a *App) filteredCountries() []vpnclient.Country {
	return a.filteredCountriesLocked()
}

func (a *App) filteredCountriesLocked() []vpnclient.Country {
	if a.locationFilter == "" {
		return a.countries
	}
	filter := strings.ToLower(a.locationFilter)
	var res []vpnclient.Country
	for _, c := range a.countries {
		if strings.Contains(strings.ToLower(c.Name), filter) || strings.Contains(strings.ToLower(c.Code), filter) {
			res = append(res, c)
		}
	}
	return res
}
