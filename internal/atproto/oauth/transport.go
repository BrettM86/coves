package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ssrfSafeTransport wraps http.Transport to prevent SSRF attacks
type ssrfSafeTransport struct {
	base         *http.Transport
	allowPrivate bool // For dev/testing only

	// lookupIP resolves a hostname. A field so a test can drive the
	// check-then-dial window that the guard has to close; nil means net.LookupIP.
	lookupIP func(host string) ([]net.IP, error)
}

// resolveHost is the transport's one name lookup per request.
func (t *ssrfSafeTransport) resolveHost(host string) ([]net.IP, error) {
	if t.lookupIP != nil {
		return t.lookupIP(host)
	}
	return net.LookupIP(host)
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

// vettedAddrsKeyType keys the addresses RoundTrip approved, so the dialler can
// read them off the request's own context. A private type, so nothing outside
// this file can plant a value under the same key.
type vettedAddrsKeyType struct{}

var vettedAddrsKey vettedAddrsKeyType

// RoundTrip vets the hostname's addresses and then makes the dial use THOSE
// ADDRESSES rather than the name.
//
// PASSING THE NAME ON WOULD BE A SECOND DECISION. The base transport resolves
// whatever host it is given, so a guard that approved answer A and then handed
// over the hostname lets the dialler act on answer B — and nothing binds the two
// together. DNS rebinding is the name for exploiting that: an attacker who
// controls the zone answers the first query publicly and the second with
// 169.254.169.254, and the approval describes a host the connection never went
// to. Every input that reaches this transport is chosen by a stranger (a DID
// document's PDS endpoint, an acceptance record's subject), so the attacker
// picks the moment to flip as well.
//
// The addresses ride on the request context and the dialler below consumes
// them, which also means the hostname is resolved exactly ONCE per request.
// That is the property, not an optimisation: any later answer is one the guard
// never saw.
func (t *ssrfSafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()

	// A literal address is already the thing that will be dialled, so there is
	// no second resolution to defend against — but it still has to pass the
	// private check below.
	ips, err := t.resolveHost(host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve host: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("failed to resolve host: %s resolved to no addresses", host)
	}

	// Check all resolved IPs
	if !t.allowPrivate {
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return nil, fmt.Errorf("SSRF blocked: %s resolves to private IP %s", host, ip)
			}
		}
	}

	return t.base.RoundTrip(req.WithContext(context.WithValue(req.Context(), vettedAddrsKey, ips)))
}

// NewSSRFSafeHTTPClient creates an HTTP client with SSRF protections
func NewSSRFSafeHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &ssrfSafeTransport{
		base: &http.Transport{
			// The dial IGNORES the hostname in addr and connects to an address
			// RoundTrip already approved, which is what closes the
			// check-then-dial window. It takes only the port from addr, because
			// the port is the one part of the destination the guard has no
			// opinion about.
			//
			// FAIL CLOSED when there is nothing vetted: reaching here without a
			// context value means this base transport was used directly rather
			// than through RoundTrip, which is exactly the unguarded path the
			// wrapper exists to prevent.
			//
			// TLS is unaffected. http.Transport derives the handshake's server
			// name from the request URL rather than from the dial address, so
			// certificate verification and SNI still name the host the caller
			// asked for.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				vetted, _ := ctx.Value(vettedAddrsKey).([]net.IP)
				if len(vetted) == 0 {
					return nil, fmt.Errorf("SSRF blocked: refusing to dial %s with no vetted address "+
						"(the SSRF-safe transport was bypassed)", addr)
				}
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("SSRF blocked: cannot read a port from %q: %w", addr, err)
				}
				var lastErr error
				for _, ip := range vetted {
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				return nil, lastErr
			},
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
