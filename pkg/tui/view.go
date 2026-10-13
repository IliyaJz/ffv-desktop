package tui

import (
	"fmt"
	"strings"
	"time"
)

// Render generates the complete terminal screen frame
func (a *App) Render() string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	w := a.termWidth
	h := a.termHeight
	if w < 40 {
		w = 40
	}
	if h < 16 {
		h = 16
	}

	var sb strings.Builder

	// Clear full screen on tab switch / resize, otherwise move cursor home
	if a.clearScreen.Swap(false) {
		sb.WriteString("\x1b[2J\x1b[H")
	} else {
		sb.WriteString("\x1b[H")
	}

	// 1. Render Header (3 lines)
	header := a.renderHeader(w)
	sb.WriteString(header)
	sb.WriteString("\x1b[K\r\n")

	// 2. Render Tabs (1 line + 1 blank line separator)
	tabs := a.renderTabs(w)
	sb.WriteString(tabs)
	sb.WriteString("\x1b[K\r\n")
	sb.WriteString("\x1b[K\r\n")

	// 3. Render Active Tab Body
	// header(3) + tabs(1) + tab-sep(1) + body-sep(1) + footer(1) = 7 lines fixed overhead
	bodyHeight := h - 7
	if bodyHeight < 8 {
		bodyHeight = 8
	}

	var body string
	switch a.activeTab {
	case 1:
		body = a.renderLocationsTab(w, bodyHeight)
	case 2:
		body = a.renderLogsTab(w, bodyHeight)
	default:
		body = a.renderDashboardTab(w, bodyHeight)
	}
	sb.WriteString(body)

	// 4. Render Footer (1 blank line separator + 1 footer line)
	sb.WriteString("\x1b[K\r\n")
	footer := a.renderFooter(w)
	sb.WriteString(footer)
	sb.WriteString("\x1b[K")

	// 5. Clear everything below the current line to the bottom of the screen
	sb.WriteString("\x1b[J")

	return sb.String()
}

func (a *App) countryName(code string) string {
	for _, c := range a.countries {
		if strings.EqualFold(c.Code, code) {
			return c.Name
		}
	}
	return code
}

func (a *App) renderHeader(w int) string {
	var sb strings.Builder

	// Top border
	title := fmt.Sprintf(" %s🦊 FFV%s CLI v1.0.0 ", FgFoxOrange+Bold, Reset)
	topBorderWidth := w - 2 - VisibleWidth(title)
	if topBorderWidth < 0 {
		topBorderWidth = 0
	}
	sb.WriteString(fmt.Sprintf("%s╭─%s%s%s%s╮%s\x1b[K\r\n", FgDarkGray, Reset, title, FgDarkGray, strings.Repeat("─", topBorderWidth), Reset))

	// Status line
	statusPill := a.statusPill()
	modePill := fmt.Sprintf("%s[%s]%s", FgCyan, a.modeString(), Reset)
	cName := a.countryName(a.selectedCountry)
	locationInfo := fmt.Sprintf("Location: %s[%s] %s%s", FgFoxOrange+Bold, a.selectedCountry, cName, Reset)

	var quotaStr string
	if a.quotaRemaining != "" {
		quotaStr = fmt.Sprintf("Quota: %s%s left%s", FgGreen, a.quotaRemaining, Reset)
	} else {
		quotaStr = fmt.Sprintf("Quota: %s50 GB Free%s", FgGreen, Reset)
	}

	line1Content := fmt.Sprintf(" %s   %s   %s   %s", statusPill, modePill, locationInfo, quotaStr)
	sb.WriteString(fmt.Sprintf("%s│%s%s%s│%s\x1b[K\r\n", FgDarkGray, Reset, PadRight(line1Content, w-2), FgDarkGray, Reset))

	// Bottom header border
	sb.WriteString(fmt.Sprintf("%s╰%s╯%s", FgDarkGray, strings.Repeat("─", w-2), Reset))
	return sb.String()
}

func (a *App) renderTabs(w int) string {
	tabTitles := []string{"1. Dashboard", "2. Locations", "3. Logs & Settings"}
	var renderedTabs []string

	for i, title := range tabTitles {
		if i == a.activeTab {
			renderedTabs = append(renderedTabs, fmt.Sprintf("%s%s[%s]%s", FgFoxOrange+Bold, BgSelected, title, Reset))
		} else {
			renderedTabs = append(renderedTabs, fmt.Sprintf("%s %s %s", FgGray, title, Reset))
		}
	}

	tabsLine := "  " + strings.Join(renderedTabs, "   ")
	return PadRight(tabsLine, w)
}

func (a *App) renderDashboardTab(w, h int) string {
	colWidth := (w - 6) / 2
	if colWidth < 30 {
		colWidth = w - 4
	}

	// Left box: Connection State
	var leftLines []string
	leftLines = append(leftLines, fmt.Sprintf("%sStatus:%s       %s", Bold, Reset, a.statusDescription()))
	cName := a.countryName(a.selectedCountry)
	leftLines = append(leftLines, fmt.Sprintf("%sCountry:%s      %s[%s]%s %s", Bold, Reset, FgCyan+Bold, a.selectedCountry, Reset, cName))

	if a.exitInfo != nil && a.exitInfo.IP != "" {
		leftLines = append(leftLines, fmt.Sprintf("%sExit IP:%s      %s%s%s (%s)", Bold, Reset, FgGreen, a.exitInfo.IP, Reset, a.exitInfo.CountryCode))
		leftLines = append(leftLines, fmt.Sprintf("%sLatency:%s      %s%s%s", Bold, Reset, FgCyan, a.exitInfo.Latency.Round(time.Millisecond), Reset))
	} else if a.status == StatusConnected {
		leftLines = append(leftLines, fmt.Sprintf("%sExit IP:%s      %sActive%s", Bold, Reset, FgGreen, Reset))
		leftLines = append(leftLines, fmt.Sprintf("%sLatency:%s      %s--%s", Bold, Reset, FgGray, Reset))
	} else {
		leftLines = append(leftLines, fmt.Sprintf("%sExit IP:%s      %s--%s", Bold, Reset, FgGray, Reset))
		leftLines = append(leftLines, fmt.Sprintf("%sLatency:%s      %s--%s", Bold, Reset, FgGray, Reset))
	}

	leftLines = append(leftLines, fmt.Sprintf("%sMode:%s         %s", Bold, Reset, a.modeDescription()))
	leftLines = append(leftLines, fmt.Sprintf("%sLocal SOCKS5:%s %s%s%s", Bold, Reset, FgCyan, a.cfg.ListenAddr, Reset))

	if a.cfg.UpstreamProxy != "" {
		leftLines = append(leftLines, fmt.Sprintf("%sUpstream:%s     %s%s%s", Bold, Reset, FgYellow, Truncate(a.cfg.UpstreamProxy, colWidth-16), Reset))
	} else {
		leftLines = append(leftLines, fmt.Sprintf("%sUpstream:%s     %sDirect (None)%s", Bold, Reset, FgGray, Reset))
	}

	leftLines = append(leftLines, fmt.Sprintf("%sDNS Tunnel:%s   %s%s%s", Bold, Reset, FgCyan, a.cfg.DNS, Reset))

	if a.status == StatusConnected && !a.connectedAt.IsZero() {
		uptime := time.Since(a.connectedAt).Round(time.Second)
		leftLines = append(leftLines, fmt.Sprintf("%sUptime:%s       %s%s%s", Bold, Reset, FgWhite, uptime.String(), Reset))
	} else {
		leftLines = append(leftLines, fmt.Sprintf("%sUptime:%s       %s00:00:00%s", Bold, Reset, FgGray, Reset))
	}

	// Right box: Account & Usage
	var rightLines []string
	rightLines = append(rightLines, fmt.Sprintf("%sAccount Plan:%s  Mozilla Free (50 GB / mo)", Bold, Reset))
	if a.quotaRemaining != "" {
		rightLines = append(rightLines, fmt.Sprintf("%sData Left:%s     %s%s%s", Bold, Reset, FgGreen+Bold, a.quotaRemaining, Reset))
	} else {
		rightLines = append(rightLines, fmt.Sprintf("%sData Left:%s     %s50.0 GB%s", Bold, Reset, FgGreen, Reset))
	}

	rightLines = append(rightLines, "")
	rightLines = append(rightLines, fmt.Sprintf("%sMonthly Allowance:%s", Dim, Reset))
	rightLines = append(rightLines, ProgressBar(0.68, colWidth-4))

	rightLines = append(rightLines, "")
	rightLines = append(rightLines, fmt.Sprintf("%sQuick Action:%s", Bold, Reset))
	if a.status == StatusConnected {
		rightLines = append(rightLines, fmt.Sprintf("Press %s[Space]%s to Disconnect tunnel", FgFoxOrange+Bold, Reset))
	} else {
		rightLines = append(rightLines, fmt.Sprintf("Press %s[Space]%s to Connect tunnel", FgGreen+Bold, Reset))
	}
	rightLines = append(rightLines, fmt.Sprintf("Press %s[m]%s to toggle TUN / Proxy mode", FgCyan+Bold, Reset))
	rightLines = append(rightLines, fmt.Sprintf("Press %s[Tab]%s to browse locations", FgFoxOrange+Bold, Reset))

	leftBox := a.renderBox(" Connection Overview ", leftLines, colWidth)
	rightBox := a.renderBox(" Data Quota & Controls ", rightLines, colWidth)

	leftParts := strings.Split(leftBox, "\r\n")
	rightParts := strings.Split(rightBox, "\r\n")

	linesCount := len(leftParts)
	if len(rightParts) > linesCount {
		linesCount = len(rightParts)
	}

	var sb strings.Builder
	for i := 0; i < h; i++ {
		l := ""
		if i < len(leftParts) && leftParts[i] != "" {
			l = leftParts[i]
		} else if i < linesCount {
			l = strings.Repeat(" ", colWidth)
		}

		r := ""
		if i < len(rightParts) && rightParts[i] != "" {
			r = rightParts[i]
		} else if i < linesCount {
			r = strings.Repeat(" ", colWidth)
		}

		if l != "" || r != "" {
			sb.WriteString("  ")
			sb.WriteString(l)
			sb.WriteString("  ")
			sb.WriteString(r)
			sb.WriteString("\x1b[K\r\n")
		} else {
			sb.WriteString("\x1b[K\r\n")
		}
	}

	return sb.String()
}

func (a *App) renderLocationsTab(w, h int) string {
	boxWidth := w - 4
	if boxWidth < 30 {
		boxWidth = 30
	}

	filtered := a.filteredCountriesLocked()
	var lines []string

	// Search prompt (Line 1 inside box)
	searchPrompt := fmt.Sprintf("%s🔍 Search:%s [%s%s%s]", Bold, Reset, FgWhite+Bold, a.locationFilter, Reset)
	if a.isSearching {
		searchPrompt += fmt.Sprintf(" %s(Typing... Enter to select / Esc to cancel)%s", FgYellow, Reset)
	} else {
		searchPrompt += fmt.Sprintf(" %s(Press '/' to filter, [↑/↓] to navigate)%s", FgGray, Reset)
	}
	lines = append(lines, searchPrompt)

	// Divider (Line 2 inside box)
	lines = append(lines, strings.Repeat("─", boxWidth-4))

	// Available list rows inside box: total height h minus box borders(2) and search+divider(2) = h - 4
	listHeight := h - 4
	if listHeight < 4 {
		listHeight = 4
	}

	if len(filtered) == 0 {
		lines = append(lines, fmt.Sprintf("%sNo countries match filter %q%s", FgYellow, a.locationFilter, Reset))
		for len(lines) < listHeight+2 {
			lines = append(lines, "")
		}
	} else {
		if a.locationCursor >= len(filtered) {
			a.locationCursor = len(filtered) - 1
		}
		if a.locationCursor < 0 {
			a.locationCursor = 0
		}

		start := a.locationCursor - listHeight/2
		if start < 0 {
			start = 0
		}
		end := start + listHeight
		if end > len(filtered) {
			end = len(filtered)
			start = end - listHeight
			if start < 0 {
				start = 0
			}
		}

		for idx := start; idx < end; idx++ {
			c := filtered[idx]
			serverCount := 0
			for _, city := range c.Cities {
				serverCount += len(city.Servers)
			}

			cursor := "   "
			style := Reset
			if idx == a.locationCursor {
				cursor = fmt.Sprintf("%s❯  %s", FgFoxOrange+Bold, Reset)
				style = BgSelected + Bold
			}

			activeTag := ""
			if strings.EqualFold(c.Code, a.selectedCountry) {
				activeTag = fmt.Sprintf(" %s[CURRENT]%s", FgGreen+Bold, Reset)
			}

			// Clean, fixed-width ASCII badge [XX] that works 100% in Warp and all terminals
			badge := fmt.Sprintf("[%s]", c.Code)
			nameStr := Truncate(c.Name, 20)
			infoStr := fmt.Sprintf("(%d cities, %d servers)", len(c.Cities), serverCount)

			countryRow := fmt.Sprintf("%s%s%s%s %s%-20s%s %s%-22s%s%s",
				cursor,
				FgCyan+Bold, badge, Reset,
				style, nameStr, Reset,
				FgGray, infoStr, Reset,
				activeTag,
			)
			lines = append(lines, countryRow)
		}

		for len(lines) < listHeight+2 {
			lines = append(lines, "")
		}
	}

	box := a.renderBox(" Available VPN Edge Locations ", lines, boxWidth)
	var sb strings.Builder
	boxLines := strings.Split(box, "\r\n")
	for i := 0; i < h; i++ {
		if i < len(boxLines) && boxLines[i] != "" {
			sb.WriteString("  ")
			sb.WriteString(boxLines[i])
			sb.WriteString("\x1b[K\r\n")
		} else {
			sb.WriteString("\x1b[K\r\n")
		}
	}
	return sb.String()
}

func (a *App) renderLogsTab(w, h int) string {
	boxWidth := w - 4
	if boxWidth < 30 {
		boxWidth = 30
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("%sActive Mode:%s       %s", Bold, Reset, a.modeDescription()))
	lines = append(lines, fmt.Sprintf("%sLocal SOCKS5:%s      %s", Bold, Reset, a.cfg.ListenAddr))
	lines = append(lines, fmt.Sprintf("%sDNS Tunnel IP:%s     %s", Bold, Reset, a.cfg.DNS))
	if a.cfg.UpstreamProxy != "" {
		lines = append(lines, fmt.Sprintf("%sUpstream Proxy:%s    %s", Bold, Reset, Truncate(a.cfg.UpstreamProxy, boxWidth-22)))
	}
	lines = append(lines, strings.Repeat("─", boxWidth-4))
	lines = append(lines, fmt.Sprintf("%sRecent Event Log:%s", Bold, Reset))

	logMaxLines := h - len(lines) - 2
	if logMaxLines < 3 {
		logMaxLines = 3
	}

	start := len(a.logs) - logMaxLines
	if start < 0 {
		start = 0
	}

	if len(a.logs) == 0 {
		lines = append(lines, fmt.Sprintf("%sNo events logged yet.%s", FgGray, Reset))
		for len(lines) < h-2 {
			lines = append(lines, "")
		}
	} else {
		for i := start; i < len(a.logs); i++ {
			entry := a.logs[i]
			timeStr := entry.Timestamp.Format("15:04:05")
			color := FgWhite
			if entry.IsError {
				color = FgRed
			}
			msg := Truncate(entry.Message, boxWidth-15)
			lines = append(lines, fmt.Sprintf("%s[%s]%s %s%s%s", FgGray, timeStr, Reset, color, msg, Reset))
		}
		for len(lines) < h-2 {
			lines = append(lines, "")
		}
	}

	box := a.renderBox(" Configuration & Activity Log ", lines, boxWidth)
	var sb strings.Builder
	boxLines := strings.Split(box, "\r\n")
	for i := 0; i < h; i++ {
		if i < len(boxLines) && boxLines[i] != "" {
			sb.WriteString("  ")
			sb.WriteString(boxLines[i])
			sb.WriteString("\x1b[K\r\n")
		} else {
			sb.WriteString("\x1b[K\r\n")
		}
	}
	return sb.String()
}

func (a *App) renderBox(title string, lines []string, width int) string {
	var sb strings.Builder

	titleStr := fmt.Sprintf("%s%s%s", FgFoxOrange+Bold, title, Reset)
	topDashes := width - 2 - VisibleWidth(title)
	if topDashes < 0 {
		topDashes = 0
	}

	sb.WriteString(fmt.Sprintf("%s╭─%s%s%s╮%s\x1b[K\r\n", FgDarkGray, titleStr, FgDarkGray, strings.Repeat("─", topDashes), Reset))

	for _, line := range lines {
		content := line
		if VisibleWidth(content) > width-4 {
			content = Truncate(content, width-4)
		}
		padded := PadRight(" "+content, width-2)
		sb.WriteString(fmt.Sprintf("%s│%s%s%s│%s\x1b[K\r\n", FgDarkGray, Reset, padded, FgDarkGray, Reset))
	}

	sb.WriteString(fmt.Sprintf("%s╰%s╯%s\x1b[K", FgDarkGray, strings.Repeat("─", width-2), Reset))
	return sb.String()
}

func (a *App) renderFooter(w int) string {
	shortcuts := []string{
		fmt.Sprintf("%s[Space]%s Connect", FgFoxOrange+Bold, Reset),
		fmt.Sprintf("%s[Tab]%s View", FgCyan+Bold, Reset),
		fmt.Sprintf("%s[m]%s Mode", FgYellow+Bold, Reset),
		fmt.Sprintf("%s[Enter]%s Select", FgWhite+Bold, Reset),
		fmt.Sprintf("%s[↑/↓]%s Pick", FgWhite+Bold, Reset),
		fmt.Sprintf("%s[/]%s Search", FgWhite+Bold, Reset),
		fmt.Sprintf("%s[q]%s Quit", FgRed+Bold, Reset),
	}
	content := " " + strings.Join(shortcuts, "  ")
	return PadRight(content, w)
}

func (a *App) statusPill() string {
	switch a.status {
	case StatusConnected:
		return fmt.Sprintf("%s● CONNECTED%s", FgGreen+Bold, Reset)
	case StatusConnecting:
		return fmt.Sprintf("%s◐ CONNECTING...%s", FgYellow+Bold, Reset)
	case StatusDisconnecting:
		return fmt.Sprintf("%s◐ DISCONNECTING%s", FgYellow+Bold, Reset)
	case StatusError:
		return fmt.Sprintf("%s✖ ERROR%s", FgRed+Bold, Reset)
	default:
		return fmt.Sprintf("%s○ DISCONNECTED%s", FgGray+Bold, Reset)
	}
}

func (a *App) statusDescription() string {
	switch a.status {
	case StatusConnected:
		return fmt.Sprintf("%sActive (Encrypted HTTP/2 Tunnel)%s", FgGreen+Bold, Reset)
	case StatusConnecting:
		return fmt.Sprintf("%sEstablishing Upstream Session...%s", FgYellow, Reset)
	case StatusDisconnecting:
		return fmt.Sprintf("%sTearing down tunnel and routes...%s", FgYellow, Reset)
	case StatusError:
		if a.statusMessage != "" {
			return fmt.Sprintf("%sError: %s%s", FgRed, a.statusMessage, Reset)
		}
		return fmt.Sprintf("%sError%s", FgRed, Reset)
	default:
		return fmt.Sprintf("%sReady to connect%s", FgGray, Reset)
	}
}

func (a *App) modeString() string {
	if a.cfg.UseTun {
		return "TUN: System-Wide"
	}
	return "Proxy-Only"
}

func (a *App) modeDescription() string {
	if a.cfg.UseTun {
		return fmt.Sprintf("%sTUN Mode (System-wide virtual interface)%s", FgCyan, Reset)
	}
	return fmt.Sprintf("%sProxy-Only (Local SOCKS5 on %s)%s", FgYellow, a.cfg.ListenAddr, Reset)
}
