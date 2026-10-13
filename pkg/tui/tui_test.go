package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"ffv/pkg/proxy"
	"ffv/pkg/vpnclient"
)

func TestPollKeys(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		wantType KeyType
		wantRune rune
	}{
		{"Up Arrow", []byte{0x1b, '[', 'A'}, KeyUp, 0},
		{"Down Arrow", []byte{0x1b, '[', 'B'}, KeyDown, 0},
		{"Right Arrow", []byte{0x1b, '[', 'C'}, KeyRight, 0},
		{"Left Arrow", []byte{0x1b, '[', 'D'}, KeyLeft, 0},
		{"Enter CR", []byte{'\r'}, KeyEnter, 0},
		{"Enter LF", []byte{'\n'}, KeyEnter, 0},
		{"Space", []byte{' '}, KeySpace, ' '},
		{"Tab", []byte{'\t'}, KeyTab, 0},
		{"BackTab", []byte{0x1b, '[', 'Z'}, KeyBackTab, 0},
		{"PageUp", []byte{0x1b, '[', '5', '~'}, KeyPageUp, 0},
		{"PageDown", []byte{0x1b, '[', '6', '~'}, KeyPageDown, 0},
		{"Ctrl-C", []byte{0x03}, KeyCtrlC, 0},
		{"Esc single", []byte{0x1b}, KeyEsc, 0},
		{"Rune 'q'", []byte{'q'}, KeyRune, 'q'},
		{"Rune 'm'", []byte{'m'}, KeyRune, 'm'},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			ch := PollKeys(ctx, bytes.NewReader(tc.input))
			select {
			case k := <-ch:
				if k.Type != tc.wantType {
					t.Errorf("got type %v, want %v", k.Type, tc.wantType)
				}
				if tc.wantRune != 0 && k.Rune != tc.wantRune {
					t.Errorf("got rune %c, want %c", k.Rune, tc.wantRune)
				}
			case <-time.After(50 * time.Millisecond):
				t.Fatalf("timed out waiting for key event for input %v", tc.input)
			}
		})
	}
}

func TestVisibleWidth(t *testing.T) {
	colored := Bold + FgCyan + "Hello World" + Reset
	if w := VisibleWidth(colored); w != 11 {
		t.Errorf("expected visible width 11, got %d", w)
	}

	flag := CountryFlag("US")
	if w := VisibleWidth(flag); w != 4 {
		t.Errorf("expected flag width 4, got %d", w)
	}
}

func TestCountryFlag(t *testing.T) {
	if f := CountryFlag("US"); f != "[US]" {
		t.Errorf("expected [US], got %s", f)
	}
	if f := CountryFlag("FR"); f != "[FR]" {
		t.Errorf("expected [FR], got %s", f)
	}
	if f := CountryFlag("UNKNOWN"); f != "[--]" {
		t.Errorf("expected [--] for invalid country code, got %s", f)
	}
}

func TestProgressBar(t *testing.T) {
	bar := ProgressBar(0.5, 20)
	if !strings.Contains(bar, "█") {
		t.Errorf("expected progress bar to contain filled blocks, got %s", bar)
	}
}

func TestAppRendering(t *testing.T) {
	app := NewApp(AppConfig{
		InitialCountry: "FR",
		UseTun:         false,
		ListenAddr:     "127.0.0.1:1088",
		DNS:            "1.1.1.1",
	})

	app.countries = []vpnclient.Country{
		{
			Name: "France",
			Code: "FR",
			Cities: []vpnclient.City{
				{
					Name: "Paris",
					Code: "PAR",
					Servers: []vpnclient.Server{
						{Hostname: "par1.m1.fastly-masque.net", Port: 2499},
					},
				},
			},
		},
		{
			Name: "United States",
			Code: "US",
			Cities: []vpnclient.City{
				{
					Name: "New York",
					Code: "NYC",
					Servers: []vpnclient.Server{
						{Hostname: "nyc1.m1.fastly-masque.net", Port: 2499},
					},
				},
			},
		},
	}

	app.activeTab = 0
	out0 := app.Render()
	if !strings.Contains(strings.ToUpper(out0), "FFV") {
		t.Errorf("Dashboard render missing FFV title")
	}
	if !strings.Contains(out0, "DISCONNECTED") {
		t.Errorf("Dashboard render missing DISCONNECTED status")
	}

	// Test connected state
	app.status = StatusConnected
	app.connectedAt = time.Now().Add(-10 * time.Minute)
	app.exitInfo = &proxy.ExitInfo{
		IP:          "192.0.2.1",
		CountryCode: "FR",
		Latency:     150 * time.Millisecond,
	}
	app.activeServer = "par1.m1.fastly-masque.net:2499"

	outConnected := app.Render()
	if !strings.Contains(outConnected, "CONNECTED") {
		t.Errorf("Dashboard render missing CONNECTED status")
	}
	if !strings.Contains(outConnected, "192.0.2.1") {
		t.Errorf("Dashboard render missing exit IP")
	}

	// Test Locations tab
	app.activeTab = 1
	out1 := app.Render()
	if !strings.Contains(out1, "France") {
		t.Errorf("Locations tab missing France")
	}

	// Test Locations filter
	app.locationFilter = "United"
	filtered := app.filteredCountries()
	if len(filtered) != 1 || filtered[0].Code != "US" {
		t.Errorf("Filter expected 1 match US, got %d", len(filtered))
	}

	// Test Logs tab
	app.activeTab = 2
	out2 := app.Render()
	if !strings.Contains(out2, "Event Log") {
		t.Errorf("Logs tab missing Event Log header")
	}
}
