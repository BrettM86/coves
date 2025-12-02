package oauth

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// ssrfSafeTransport wraps http.Transport to prevent SSRF attacks
type ssrfSafeTransport struct {
	base         *http.Transport
	allowPrivate bool // For dev/testing only
}

// isPrivateIP checks if an IP is in a private/reserved range
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}

	// Check for loopback
	if ip.IsLoopback() {
		return true
	}

	// Check for link-local
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// Check for private ranges
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}

	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(ip) {
			return true
		}
	}

	return false
}

func (t *ssrfSafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()

	// Resolve hostname to IP
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve host: %w", err)
	}

	// Check all resolved IPs
	if !t.allowPrivate {
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return nil, fmt.Errorf("SSRF blocked: %s resolves to private IP %s", host, ip)
			}
		}
	}

	return t.base.RoundTrip(req)
}

// NewSSRFSafeHTTPClient creates an HTTP client with SSRF protections
func NewSSRFSafeHTTPClient(allowPrivate bool) *http.Client {
	transport := &ssrfSafeTransport{
		base: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		allowPrivate: allowPrivate,
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}
