package vpnclient

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"time"
)

const (
	controlPlaneHTTPTimeout = 30 * time.Second
	errorBodyLogLimit       = 16 * 1024
)

var (
	controlPlaneMu         sync.RWMutex
	controlPlaneHTTPClient = func() *http.Client {
		jar, err := cookiejar.New(nil)
		if err != nil {
			panic(fmt.Sprintf("creating control plane cookie jar: %v", err))
		}
		return &http.Client{
			Timeout: controlPlaneHTTPTimeout,
			Jar:     jar,
		}
	}()
)

// SetControlPlaneTransport routes Firefox Accounts, Guardian, and Remote
// Settings requests through transport. It can be replaced when configuring
// upstream proxies.
func SetControlPlaneTransport(transport http.RoundTripper) {
	controlPlaneMu.Lock()
	defer controlPlaneMu.Unlock()
	controlPlaneHTTPClient.Transport = transport
}

// GetControlPlaneTransport returns the currently configured transport.
func GetControlPlaneTransport() http.RoundTripper {
	controlPlaneMu.RLock()
	defer controlPlaneMu.RUnlock()
	return controlPlaneHTTPClient.Transport
}

// GetControlPlaneCookieJar returns the control plane cookie jar.
func GetControlPlaneCookieJar() http.CookieJar {
	controlPlaneMu.RLock()
	defer controlPlaneMu.RUnlock()
	return controlPlaneHTTPClient.Jar
}

func doControlPlane(req *http.Request) (*http.Response, error) {
	controlPlaneMu.RLock()
	client := controlPlaneHTTPClient
	controlPlaneMu.RUnlock()
	return client.Do(req)
}

func readErrorBody(r io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(r, errorBodyLogLimit))
	if err != nil {
		return "reading error body failed: " + err.Error()
	}
	return string(body)
}
