package vpnclient

import "net/http"

func applyMozillaVPNHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "MozillaVPN/2.35.0 (sys:linux; iap:true)")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Client-Version", "2.35.0")
	req.Header.Set("X-Client-Platform", "linux")
}
