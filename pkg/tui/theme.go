package tui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ANSI Styling Escape Sequences
const (
	Reset     = "\x1b[0m"
	Bold      = "\x1b[1m"
	Dim       = "\x1b[2m"
	Italic    = "\x1b[3m"
	Underline = "\x1b[4m"
	Reverse   = "\x1b[7m"

	// Colors (TrueColor / High compatibility 256)
	FgFoxOrange = "\x1b[38;5;208m" // Firefox Vibrant Orange
	FgFoxRed    = "\x1b[38;5;203m" // Reddish Orange
	FgPurple    = "\x1b[38;5;141m" // Mozilla Purple
	FgGreen     = "\x1b[38;5;84m"  // Connected Green
	FgYellow    = "\x1b[38;5;221m" // Warning Yellow
	FgRed       = "\x1b[38;5;196m" // Error Red
	FgCyan      = "\x1b[38;5;45m"  // Cyan
	FgBlue      = "\x1b[38;5;75m"  // Soft Blue
	FgWhite     = "\x1b[38;5;255m" // Pure White
	FgGray      = "\x1b[38;5;244m" // Slate Gray
	FgDarkGray  = "\x1b[38;5;238m" // Dark Border Gray

	// Backgrounds
	BgFoxOrange = "\x1b[48;5;208m"
	BgPurple    = "\x1b[48;5;141m"
	BgGreen     = "\x1b[48;5;28m"
	BgDarkGray  = "\x1b[48;5;236m"
	BgSelected  = "\x1b[48;5;237m"
)

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripANSI removes escape codes to compute visible character length
func StripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

// VisibleWidth calculates actual column display width of string
func VisibleWidth(s string) int {
	clean := StripANSI(s)
	width := 0
	for _, r := range clean {
		if r >= 0x1F300 && r <= 0x1FAFF { // Single emoji symbols (e.g. 🦊, 🌐, 🔍)
			width += 2
		} else if r > 127 && utf8.RuneLen(r) > 2 {
			width += 2
		} else {
			width += 1
		}
	}
	return width
}

// Truncate ensures text fits within maxWidth columns
func Truncate(s string, maxWidth int) string {
	if maxWidth <= 3 {
		return s
	}
	if VisibleWidth(s) <= maxWidth {
		return s
	}

	clean := StripANSI(s)
	var sb strings.Builder
	curr := 0
	for _, r := range clean {
		rw := 1
		if r >= 0x1F300 && r <= 0x1FAFF {
			rw = 2
		}
		if curr+rw+1 > maxWidth {
			break
		}
		sb.WriteRune(r)
		curr += rw
	}
	sb.WriteString("…")
	return sb.String()
}

// PadRight pads string with spaces to reach target visual width
func PadRight(s string, width int) string {
	vw := VisibleWidth(s)
	if vw >= width {
		return Truncate(s, width)
	}
	return s + strings.Repeat(" ", width-vw)
}

// PadLeft pads string on left to reach target visual width
func PadLeft(s string, width int) string {
	vw := VisibleWidth(s)
	if vw >= width {
		return Truncate(s, width)
	}
	return strings.Repeat(" ", width-vw) + s
}

// Center centers string within width
func Center(s string, width int) string {
	vw := VisibleWidth(s)
	if vw >= width {
		return Truncate(s, width)
	}
	left := (width - vw) / 2
	right := width - vw - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

// CountryFlag returns a clean 4-character badge "[XX]" for maximum compatibility across all terminals (including Warp).
func CountryFlag(countryCode string) string {
	code := strings.ToUpper(strings.TrimSpace(countryCode))
	if len(code) != 2 {
		return "[--]"
	}
	return fmt.Sprintf("[%s]", code)
}

// ProgressBar renders [████████░░░░]
func ProgressBar(ratio float64, width int) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	barWidth := width - 2
	if barWidth <= 0 {
		return "[]"
	}

	filled := int(ratio * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	empty := barWidth - filled

	fillColor := FgGreen
	if ratio < 0.25 {
		fillColor = FgRed
	} else if ratio < 0.50 {
		fillColor = FgYellow
	}

	return fmt.Sprintf("%s[%s%s%s%s%s]%s",
		FgDarkGray,
		fillColor, strings.Repeat("█", filled),
		FgDarkGray, strings.Repeat("░", empty),
		FgDarkGray,
		Reset,
	)
}
