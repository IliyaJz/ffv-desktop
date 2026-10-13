package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ffv/pkg/proxy"
	"ffv/pkg/tui"
	"ffv/pkg/tun"
	"ffv/pkg/vpnclient"
	"golang.org/x/term"
)

const version = "1.0.0"

type StateStore struct {
	LastCountry string `json:"last_country,omitempty"`
	LastServer  string `json:"last_server,omitempty"`
}

func stateFilePath() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
			cand := filepath.Join(u.HomeDir, ".ffv", "state.json")
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
			foxy := filepath.Join(u.HomeDir, ".foxyvpn", "state.json")
			if _, err := os.Stat(foxy); err == nil {
				return foxy
			}
		}
		fallback := filepath.Join("/home", sudoUser, ".ffv", "state.json")
		if _, err := os.Stat(fallback); err == nil {
			return fallback
		}
	}
	home, _ := os.UserHomeDir()
	cand := filepath.Join(home, ".ffv", "state.json")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	foxy := filepath.Join(home, ".foxyvpn", "state.json")
	if _, err := os.Stat(foxy); err == nil {
		return foxy
	}
	return filepath.Join(home, ".ffv", "state.json")
}

func loadState() StateStore {
	var s StateStore
	data, err := os.ReadFile(stateFilePath())
	if err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func saveState(s StateStore) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		path := stateFilePath()
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		_ = os.WriteFile(path, data, 0600)
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
			if u, err := user.Lookup(sudoUser); err == nil {
				if uid, err := strconv.Atoi(u.Uid); err == nil {
					if gid, err := strconv.Atoi(u.Gid); err == nil {
						_ = os.Chown(filepath.Dir(path), uid, gid)
						_ = os.Chown(path, uid, gid)
					}
				}
			}
		}
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			runLogin(os.Args[2:])
			return
		case "logout":
			runLogout()
			return
		case "status", "info":
			runStatus(os.Args[2:])
			return
		case "servers", "list":
			runServers(os.Args[2:])
			return
		case "connect":
			runConnect(os.Args[2:])
			return
		case "tui", "--tui", "-tui":
			runTUI(os.Args[2:])
			return
		case "version", "--version", "-v":
			fmt.Printf("FFV CLI v%s (macOS, Linux, Windows)\n", version)
			return
		case "help", "--help", "-h":
			printHelp()
			return
		}
	} else {
		// When run without arguments (e.g. ./bin/ffv or sudo ./bin/ffv)
		// in an interactive terminal, launch the interactive TUI!
		if term.IsTerminal(int(os.Stdin.Fd())) {
			runTUI(nil)
			return
		}
	}

	// Default action if flags passed without subcommand: connect
	runConnect(os.Args[1:])
}

func printHelp() {
	fmt.Printf(`FFV CLI v%s - High-performance Firefox VPN client for macOS, Linux, and Windows

USAGE:
    ffv [COMMAND] [OPTIONS]
    ffv                 (launches interactive TUI dashboard in terminal)

COMMANDS:
    tui         Launch interactive Terminal User Interface (TUI)
    connect     Connect to VPN tunnel (TUN mode or Proxy-only mode)
    login       Sign in to your Firefox Account (supports 2FA confirmation)
    logout      Sign out and delete saved credentials
    status      Show account quota, subscription status, and token validity
    servers     List available VPN countries, cities, and server hostnames
    version     Print version information
    help        Show this help message

OPTIONS (for 'connect' or 'tui'):
    -c, --country CODE       Country code or name to route through (e.g. US, JP, DE, FR)
    -s, --server HOST:PORT   Explicit Fastly MASQUE edge node (e.g. host:2499)
    --upstream-proxy URL     Upstream proxy for BOTH control-plane & data-plane
                             (HTTP, HTTPS, SOCKS5 e.g. socks5://127.0.0.1:1080)
    --tun                    Enable system-wide TUN VPN mode (default if run as root)
    --proxy-only             Disable TUN mode; run local SOCKS5 proxy only (no root needed)
    -l, --listen ADDR        Local SOCKS5 listen address (default: 127.0.0.1:1088)
    --dns IP                 DNS server for VPN tunnel (default: 1.1.1.1)
    --tun-name NAME          Custom TUN device name (e.g. ffv0, utun)
    --session-token TOKEN    Use explicit session token or token file
    --verify-exit            Verify and display exit public IP and country (default: true)
    -v, --verbose            Enable verbose debug logs

EXAMPLES:
    # 1. Launch interactive TUI:
    ffv
    # or with root/admin privileges for system-wide TUN mode:
    sudo ffv

    # 2. Login with your Firefox account:
    ffv login

    # 3. Connect with full system-wide TUN mode (CLI):
    sudo ffv connect --country US

    # 4. Connect through an upstream SOCKS5 proxy in TUN mode:
    sudo ffv connect --country JP --upstream-proxy socks5://127.0.0.1:1080

    # 5. Run in Proxy-Only mode (SOCKS5 listener on 127.0.0.1:1088, no sudo needed):
    ffv connect --country DE --proxy-only

    # 6. List available server locations:
    ffv servers
`, version)
}

func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	upstreamProxy := fs.String("upstream-proxy", "", "Upstream proxy URL (e.g. socks5://127.0.0.1:1080)")
	_ = fs.Parse(args)

	if *upstreamProxy != "" {
		dialer, err := proxy.NewUpstreamDialer(*upstreamProxy, 20*time.Second)
		if err != nil {
			log.Fatalf("Invalid upstream proxy: %v", err)
		}
		vpnclient.SetControlPlaneTransport(dialer.BuildControlPlaneTransport())
		fmt.Printf("Routing login through upstream proxy: %s\n", *upstreamProxy)
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Println("=== Firefox Account Login ===")
	fmt.Print("Email: ")
	email, err := reader.ReadString('\n')
	if err != nil {
		log.Fatalf("Error reading email: %v", err)
	}
	email = strings.TrimSpace(email)
	if email == "" {
		log.Fatal("Email cannot be empty")
	}

	fmt.Print("Password: ")
	bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		log.Fatalf("Error reading password: %v", err)
	}
	password := string(bytePassword)
	if password == "" {
		log.Fatal("Password cannot be empty")
	}

	fmt.Println("Signing in to Firefox Accounts...")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	loginResp, err := vpnclient.FxaLogin(ctx, email, password)
	if err != nil {
		log.Fatalf("Sign-in failed: %v", err)
	}

	sessionToken := loginResp.SessionToken

	if !loginResp.Verified {
		fmt.Printf("\n[2FA Required] A verification code was emailed to %s.\n", email)
		fmt.Print("Enter confirmation code (or press Enter after confirming link): ")
		code, _ := reader.ReadString('\n')
		code = strings.TrimSpace(code)

		if code != "" {
			fmt.Println("Verifying confirmation code...")
			if err := vpnclient.FxaVerifySession(ctx, sessionToken, code); err != nil {
				log.Fatalf("Code verification failed: %v", err)
			}
			fmt.Println("Code verified successfully!")
		} else {
			fmt.Println("Continuing with session...")
		}
	}

	fmt.Println("Requesting VPN OAuth access token...")
	tokens, err := vpnclient.FxaOAuthToken(ctx, sessionToken)
	if err != nil {
		log.Fatalf("Obtaining OAuth token failed: %v", err)
	}

	if err := vpnclient.SaveTokens(tokens); err != nil {
		log.Fatalf("Saving tokens failed: %v", err)
	}

	fmt.Println("Activating Guardian VPN proxy entitlement...")
	if _, err := vpnclient.ActivateGuardian(ctx, "", tokens.AccessToken); err != nil {
		fmt.Printf("Note: Guardian activation returned: %v (continuing)\n", err)
	}

	fmt.Println("Testing Guardian VPN proxy access...")
	pass, err := vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
	if err != nil {
		fmt.Printf("Warning: Guardian check returned: %v\n", err)
	} else {
		fmt.Printf("Guardian proxy pass verified! Monthly quota remaining: %s\n", pass.QuotaLeft)
	}

	fmt.Printf("\nSUCCESS! Saved credentials to %s\n", vpnclient.TokenFilePath())
	fmt.Println("You can now connect with: sudo foxyvpn connect --country US")
}

func runLogout() {
	vpnclient.DeleteTokens()
	fmt.Printf("Logged out successfully. Removed saved credentials from %s\n", vpnclient.TokenFilePath())
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	upstreamProxy := fs.String("upstream-proxy", "", "Upstream proxy URL")
	_ = fs.Parse(args)

	if *upstreamProxy != "" {
		dialer, err := proxy.NewUpstreamDialer(*upstreamProxy, 20*time.Second)
		if err == nil {
			vpnclient.SetControlPlaneTransport(dialer.BuildControlPlaneTransport())
		}
	}

	tokens, err := vpnclient.LoadTokens()
	if err != nil {
		fmt.Println("No active login found. Run 'ffv login' to authenticate.")
		return
	}

	fmt.Println("=== FFV Status ===")
	fmt.Printf("Token File:       %s\n", vpnclient.TokenFilePath())
	fmt.Printf("Access Token:     Valid (expires in %s)\n", time.Until(tokens.ObtainedAt.Add(time.Duration(tokens.ExpiresIn)*time.Second)).Round(time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !tokens.AccessTokenValid() {
		fmt.Println("Access token expired, refreshing...")
		newTok, err := vpnclient.FxaRefreshToken(ctx, tokens.RefreshToken)
		if err == nil {
			_ = vpnclient.SaveTokens(newTok)
			tokens.AccessToken = newTok.AccessToken
		}
	}

	pass, err := vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
	if errors.Is(err, vpnclient.ErrTokenInvalid) {
		fmt.Println("Guardian returned HTTP 401/403, attempting entitlement activation...")
		if _, actErr := vpnclient.ActivateGuardian(ctx, "", tokens.AccessToken); actErr == nil {
			pass, err = vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
		} else {
			fmt.Println("Refreshing access token with Firefox Accounts...")
			newTok, refErr := vpnclient.FxaRefreshToken(ctx, tokens.RefreshToken)
			if refErr == nil {
				_ = vpnclient.SaveTokens(newTok)
				tokens.AccessToken = newTok.AccessToken
				_, _ = vpnclient.ActivateGuardian(ctx, "", tokens.AccessToken)
				pass, err = vpnclient.FetchProxyPass(ctx, "", tokens.AccessToken)
			}
		}
	}

	if err != nil {
		fmt.Printf("Guardian Pass:    Error (%v)\n", err)
	} else {
		fmt.Printf("Guardian Pass:    Active\n")
		fmt.Printf("Pass Expiration:  %s\n", pass.ExpiresAt().Format(time.RFC1123))
		if pass.QuotaLeft != "" {
			fmt.Printf("Quota Remaining:  %s\n", pass.QuotaLeft)
		}
		if pass.QuotaReset != "" {
			sec, err := strconv.ParseInt(pass.QuotaReset, 10, 64)
			if err == nil {
				fmt.Printf("Quota Reset:      %s\n", time.Unix(sec, 0).Format(time.RFC1123))
			}
		}
	}
}

func runServers(args []string) {
	fs := flag.NewFlagSet("servers", flag.ExitOnError)
	countryFilter := fs.String("country", "", "Filter by country code (e.g. US)")
	upstreamProxy := fs.String("upstream-proxy", "", "Upstream proxy URL")
	_ = fs.Parse(args)

	if *upstreamProxy != "" {
		dialer, err := proxy.NewUpstreamDialer(*upstreamProxy, 20*time.Second)
		if err == nil {
			vpnclient.SetControlPlaneTransport(dialer.BuildControlPlaneTransport())
		}
	}

	fmt.Println("Fetching available VPN server locations from Mozilla...")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	countries, err := vpnclient.FetchServerList(ctx)
	if err != nil {
		log.Fatalf("Failed to fetch server list: %v", err)
	}

	sort.Slice(countries, func(i, j int) bool {
		return countries[i].Name < countries[j].Name
	})

	fmt.Printf("\n%-24s %-6s %-8s %s\n", "COUNTRY", "CODE", "SERVERS", "CITIES")
	fmt.Println(strings.Repeat("-", 75))

	for _, c := range countries {
		if *countryFilter != "" && !strings.EqualFold(c.Code, *countryFilter) && !strings.Contains(strings.ToLower(c.Name), strings.ToLower(*countryFilter)) {
			continue
		}

		serverCount := 0
		var cityNames []string
		for _, city := range c.Cities {
			cityNames = append(cityNames, city.Name)
			serverCount += len(city.Servers)
		}

		citiesStr := strings.Join(cityNames, ", ")
		if len(citiesStr) > 35 {
			citiesStr = citiesStr[:32] + "..."
		}

		fmt.Printf("%-24s %-6s %-8d %s\n", c.Name, c.Code, serverCount, citiesStr)
	}
	fmt.Println()
}

func runConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)

	countryFlag := fs.String("country", "", "Country code (e.g. US, JP, DE, GB)")
	cShort := fs.String("c", "", "Country code alias")

	serverFlag := fs.String("server", "", "Explicit Fastly MASQUE proxy hostname (e.g. host:2499)")
	sShort := fs.String("s", "", "Explicit server alias")

	upstreamProxy := fs.String("upstream-proxy", "", "Upstream HTTP/SOCKS5 proxy URL (e.g. socks5://127.0.0.1:1080)")
	listenAddr := fs.String("listen", "127.0.0.1:1088", "Local SOCKS5 listen address")
	lShort := fs.String("l", "127.0.0.1:1088", "Local SOCKS5 listen address alias")

	tunFlag := fs.Bool("tun", false, "Enable system-wide TUN VPN mode")
	proxyOnlyFlag := fs.Bool("proxy-only", false, "Run in proxy-only mode (SOCKS5 only, no TUN/root needed)")

	dnsFlag := fs.String("dns", "1.1.1.1", "DNS server for VPN tunnel")
	tunNameFlag := fs.String("tun-name", "", "Custom TUN device name")
	sessionTokenFlag := fs.String("session-token", "", "Explicit Guardian session token or token file")
	verifyExitFlag := fs.Bool("verify-exit", true, "Verify exit IP and country through tunnel")
	verboseFlag := fs.Bool("verbose", false, "Enable verbose debug logs")
	vShort := fs.Bool("v", false, "Verbose alias")

	_ = fs.Parse(args)

	country := *countryFlag
	if country == "" {
		country = *cShort
	}
	server := *serverFlag
	if server == "" {
		server = *sShort
	}
	listen := *listenAddr
	if listen == "127.0.0.1:1088" && *lShort != "127.0.0.1:1088" {
		listen = *lShort
	}
	verbose := *verboseFlag || *vShort

	// Check environment variable fallback for upstream proxy
	if *upstreamProxy == "" {
		for _, envKey := range []string{"UPSTREAM_PROXY", "ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY"} {
			if v := os.Getenv(envKey); v != "" {
				*upstreamProxy = v
				break
			}
		}
	}

	// Determine TUN vs Proxy-only mode
	isRoot := isPrivilegedUser()
	useTun := false

	if *proxyOnlyFlag {
		useTun = false
	} else if *tunFlag {
		if !isRoot {
			log.Fatal("[ERROR] --tun mode requires root privileges. Please re-run with 'sudo ffv connect ...' or use '--proxy-only'.")
		}
		useTun = true
	} else {
		// Not specified explicitly: default to TUN if root, else proxy-only with message
		if isRoot {
			useTun = true
		} else {
			useTun = false
			fmt.Println("[INFO] Running without root privileges -> starting in PROXY-ONLY mode.")
			fmt.Println("       For system-wide TUN VPN mode, run with: sudo ffv connect [options]")
		}
	}

	state := loadState()
	if country == "" && server == "" {
		if state.LastCountry != "" {
			country = state.LastCountry
		} else {
			country = "US"
		}
	}

	// 1. Initialize upstream dialer
	var dialer *proxy.UpstreamDialer
	if *upstreamProxy != "" {
		var err error
		dialer, err = proxy.NewUpstreamDialer(*upstreamProxy, 20*time.Second)
		if err != nil {
			log.Fatalf("Invalid upstream proxy: %v", err)
		}
		fmt.Printf("[PROXY] Chaining connections through upstream proxy: %s\n", *upstreamProxy)
		vpnclient.SetControlPlaneTransport(dialer.BuildControlPlaneTransport())
	}

	// 2. Select Fastly edge server
	var targetHost string
	if server != "" {
		targetHost = server
		if !strings.Contains(targetHost, ":") {
			targetHost += ":2499"
		}
	} else {
		fmt.Printf("Fetching VPN server list for country: %s...\n", country)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		countries, err := vpnclient.FetchServerList(ctx)
		if err != nil {
			log.Fatalf("Failed to fetch server list: %v", err)
		}

		selected, err := selectServer(countries, country, state.LastServer)
		if err != nil {
			log.Fatalf("Server selection failed: %v", err)
		}
		targetHost = selected
		state.LastCountry = country
		state.LastServer = selected
		saveState(state)
	}

	targetURL, err := url.Parse("//" + targetHost)
	if err != nil {
		log.Fatalf("Invalid server target: %v", err)
	}

	fmt.Printf("[UPSTREAM] Selected Fastly Edge node: %s\n", targetHost)

	// Check session token
	sessionToken := *sessionTokenFlag
	if sessionToken != "" {
		// Check if it's a file
		if data, err := os.ReadFile(sessionToken); err == nil {
			lines := strings.Split(string(data), "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if l != "" && !strings.HasPrefix(l, "#") {
					sessionToken = l
					break
				}
			}
		}
	}

	// 3. Create proxy controller
	fmt.Println("Establishing encrypted HTTP/2 tunnel session...")
	controller, err := proxy.NewController(proxy.ControllerConfig{
		ProxyURL:       targetURL,
		SessionToken:   sessionToken,
		Country:        country,
		UpstreamDialer: dialer,
		ConnectTimeout: 25 * time.Second,
		Verbose:        verbose,
	})
	if err != nil {
		log.Fatalf("Failed to establish upstream session: %v", err)
	}
	defer controller.Close()

	// 4. Start local SOCKS5 proxy server
	socksServer := proxy.NewSocksServer(controller, 10*time.Second, 0, verbose)
	go func() {
		if err := socksServer.ListenAndServe(listen); err != nil {
			log.Printf("[SOCKS5] Listener stopped: %v", err)
		}
	}()
	defer socksServer.Close()

	// Give socks5 listener a moment to bind
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("[SOCKS5] Local proxy listening on %s\n", listen)

	// 5. Exit IP verification
	if *verifyExitFlag {
		fmt.Print("Verifying tunnel exit IP... ")
		exitInfo, err := controller.VerifyExit(10 * time.Second)
		if err != nil {
			fmt.Printf("verification warning: %v\n", err)
		} else {
			fmt.Printf("VERIFIED! Exit IP: %s (Country: %s, RTT: %s)\n", exitInfo.IP, exitInfo.CountryCode, exitInfo.Latency.Round(time.Millisecond))
		}
	}

	// 6. Setup TUN if requested
	var tunRunner *tun.Runner
	if useTun {
		fmt.Printf("Configuring TUN interface (DNS: %s)...\n", *dnsFlag)
		upstreamEndpoint := targetHost
		if dialer != nil && dialer.ProxyURL != nil {
			// When using an upstream proxy, route the upstream proxy host directly
			upstreamEndpoint = dialer.ProxyURL.Host
		}

		tunRunner = tun.NewRunner(tun.Config{
			TunName:          *tunNameFlag,
			MTU:              tun.DefaultTunMTU,
			IPv4:             tun.DefaultTunAddress,
			Gateway:          tun.DefaultTunGateway,
			DNS:              *dnsFlag,
			Socks5Addr:       listen,
			UpstreamEndpoint: upstreamEndpoint,
			Verbose:          verbose,
		})

		ctx := context.Background()
		if err := tunRunner.Start(ctx); err != nil {
			log.Fatalf("Failed to activate TUN mode: %v", err)
		}
		defer tunRunner.Stop()

		fmt.Println("==================================================")
		fmt.Println(" [STATUS] FFV is CONNECTED in TUN Mode!")
		fmt.Println(" All system traffic is routed through the VPN tunnel.")
		fmt.Println(" Press Ctrl+C to disconnect and restore routes.")
		fmt.Println("==================================================")
	} else {
		fmt.Println("==================================================")
		fmt.Println(" [STATUS] FFV is RUNNING in Proxy-Only Mode!")
		fmt.Printf(" Configure your browser or apps to use SOCKS5: %s\n", listen)
		fmt.Println(" Press Ctrl+C to disconnect.")
		fmt.Println("==================================================")
	}

	// 7. Wait for interrupt signal (SIGINT / SIGTERM / SIGHUP)
	sigCh := make(chan os.Signal, 2)
	notifyInterrupt(sigCh)
	<-sigCh

	fmt.Println("\n[INFO] Interrupt received, disconnecting and restoring routes...")

	// Fail-safe: if user presses Ctrl+C again during cleanup, exit immediately
	go func() {
		<-sigCh
		fmt.Println("\n[INFO] Forced shutdown.")
		os.Exit(130)
	}()

	cleanupDone := make(chan struct{})
	go func() {
		if tunRunner != nil {
			_ = tunRunner.Stop()
		}
		_ = controller.Close()
		_ = socksServer.Close()
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
		fmt.Println("Disconnected cleanly. Goodbye!")
	case <-time.After(3 * time.Second):
		fmt.Println("Shutdown complete. Goodbye!")
	}
}

func selectServer(countries []vpnclient.Country, countryFilter, lastServer string) (string, error) {
	var candidates []string

	for _, c := range countries {
		if strings.EqualFold(c.Code, countryFilter) || strings.EqualFold(c.Name, countryFilter) {
			for _, city := range c.Cities {
				for _, srv := range city.Servers {
					if !srv.Quarantined && srv.Hostname != "" {
						port := srv.Port
						if port == 0 {
							port = 2499
						}
						candidates = append(candidates, net.JoinHostPort(srv.Hostname, strconv.Itoa(port)))
					}
				}
			}
			break
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no active servers found for country %q", countryFilter)
	}

	// Check if last used server is still valid
	if lastServer != "" {
		for _, c := range candidates {
			if c == lastServer {
				return c, nil
			}
		}
	}

	return candidates[0], nil
}

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)

	countryFlag := fs.String("country", "", "Initial country code (e.g. US, JP, DE, FR)")
	cShort := fs.String("c", "", "Initial country code alias")

	upstreamProxy := fs.String("upstream-proxy", "", "Upstream HTTP/SOCKS5 proxy URL (e.g. socks5://127.0.0.1:1080)")
	listenAddr := fs.String("listen", "127.0.0.1:1088", "Local SOCKS5 listen address")
	lShort := fs.String("l", "127.0.0.1:1088", "Local SOCKS5 listen address alias")

	tunFlag := fs.Bool("tun", false, "Start with system-wide TUN VPN mode enabled")
	proxyOnlyFlag := fs.Bool("proxy-only", false, "Start in proxy-only mode (SOCKS5 only)")

	dnsFlag := fs.String("dns", "1.1.1.1", "DNS server for VPN tunnel")
	verboseFlag := fs.Bool("verbose", false, "Enable verbose debug logs")
	vShort := fs.Bool("v", false, "Verbose alias")

	_ = fs.Parse(args)

	country := *countryFlag
	if country == "" {
		country = *cShort
	}
	if country == "" {
		state := loadState()
		if state.LastCountry != "" {
			country = state.LastCountry
		} else {
			country = "US"
		}
	}

	listen := *listenAddr
	if listen == "127.0.0.1:1088" && *lShort != "127.0.0.1:1088" {
		listen = *lShort
	}

	proxyURL := *upstreamProxy
	if proxyURL == "" {
		for _, envKey := range []string{"UPSTREAM_PROXY", "ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY"} {
			if v := os.Getenv(envKey); v != "" {
				proxyURL = v
				break
			}
		}
	}

	isRoot := isPrivilegedUser()
	useTun := false
	if *proxyOnlyFlag {
		useTun = false
	} else if *tunFlag {
		useTun = true
	} else {
		useTun = isRoot
	}

	cfg := tui.AppConfig{
		InitialCountry: country,
		UseTun:         useTun,
		ListenAddr:     listen,
		UpstreamProxy:  proxyURL,
		DNS:            *dnsFlag,
		Verbose:        *verboseFlag || *vShort,
	}

	app := tui.NewApp(cfg)
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}
}

