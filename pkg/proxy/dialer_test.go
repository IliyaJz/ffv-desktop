package proxy

import (
	"testing"
	"time"
)

func TestNewUpstreamDialer(t *testing.T) {
	tests := []struct {
		input       string
		expectError bool
		expectNil   bool
		scheme      string
		host        string
		hasAuth     bool
	}{
		{
			input:     "",
			expectNil: true,
		},
		{
			input:  "socks5://127.0.0.1:1080",
			scheme: "socks5",
			host:   "127.0.0.1:1080",
		},
		{
			input:   "socks5://user:pass@127.0.0.1:1080",
			scheme:  "socks5",
			host:    "127.0.0.1:1080",
			hasAuth: true,
		},
		{
			input:  "http://10.0.0.1:8080",
			scheme: "http",
			host:   "10.0.0.1:8080",
		},
		{
			input:   "http://admin:secret@proxy.example.com:3128",
			scheme:  "http",
			host:    "proxy.example.com:3128",
			hasAuth: true,
		},
		{
			input:  "https://secure-proxy.com:8443",
			scheme: "https",
			host:   "secure-proxy.com:8443",
		},
		{
			input:  "socks5h://127.0.0.1:9050",
			scheme: "socks5h",
			host:   "127.0.0.1:9050",
		},
		{
			input:       "ftp://127.0.0.1:21",
			expectError: true,
		},
	}

	for _, tt := range tests {
		d, err := NewUpstreamDialer(tt.input, 10*time.Second)
		if tt.expectError {
			if err == nil {
				t.Errorf("expected error for input %q, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("unexpected error for input %q: %v", tt.input, err)
			continue
		}
		if tt.expectNil {
			if d.ProxyURL != nil {
				t.Errorf("expected ProxyURL to be nil for empty input, got %v", d.ProxyURL)
			}
			continue
		}
		if d.ProxyURL == nil {
			t.Errorf("expected ProxyURL not to be nil for input %q", tt.input)
			continue
		}
		if d.ProxyURL.Scheme != tt.scheme {
			t.Errorf("expected scheme %q, got %q", tt.scheme, d.ProxyURL.Scheme)
		}
		if d.ProxyURL.Host != tt.host {
			t.Errorf("expected host %q, got %q", tt.host, d.ProxyURL.Host)
		}
		if tt.hasAuth && d.ProxyURL.User == nil {
			t.Errorf("expected auth for input %q, got none", tt.input)
		}
	}
}

func TestBuildControlPlaneTransport(t *testing.T) {
	d, err := NewUpstreamDialer("http://127.0.0.1:8080", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tr := d.BuildControlPlaneTransport()
	if tr == nil {
		t.Fatal("expected transport, got nil")
	}
	if tr.Proxy == nil {
		t.Fatal("expected Proxy function set on transport")
	}
}
