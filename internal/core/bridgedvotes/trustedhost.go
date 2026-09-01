package bridgedvotes

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// TrustedHost is an operator-configured bridge dial target. Only ParseTrustedHost
// builds a non-zero value, so a community's stored pds_url cannot reach
// Client.GetVoteAggregates without passing the validation TRUSTED_BRIDGE_PDS_HOSTS
// itself passes. That turns the poller's central invariant — a database value is
// never a dial target — from a code-review property into a type-level one.
type TrustedHost struct {
	// dialURL is the canonical scheme://host[:port] form, normalized exactly as
	// NormalizeHost would render it so trust matching and dialing share one key.
	dialURL string
}

// ParseTrustedHost validates one TRUSTED_BRIDGE_PDS_HOSTS entry. The contract is
// scheme + host (+ optional port) and nothing else: userinfo would be sent as
// basic auth and echoed into error logs, a path would be prefixed onto the XRPC
// path and turn every request into a 404 that poison-marks its batch, and a
// query or fragment has no meaning for a dial target.
func ParseTrustedHost(raw string) (TrustedHost, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return TrustedHost{}, errors.New("trusted bridge host is empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q is not a URL: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q must be an absolute http(s) URL", raw)
	}
	if u.Hostname() == "" {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q must name a host", raw)
	}
	if u.User != nil {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q must not carry credentials", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q must not carry a path; the XRPC path is appended by the poller", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q must not carry a query", raw)
	}
	if u.Fragment != "" {
		return TrustedHost{}, fmt.Errorf("trusted bridge host %q must not carry a fragment", raw)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return TrustedHost{}, fmt.Errorf("trusted bridge host %q has an invalid port %q", raw, port)
		}
	}
	return TrustedHost{dialURL: NormalizeHost(scheme + "://" + u.Host)}, nil
}

// String returns the canonical dial URL, or "" for the zero value.
func (h TrustedHost) String() string { return h.dialURL }

// IsZero reports whether h was never produced by ParseTrustedHost.
func (h TrustedHost) IsZero() bool { return h.dialURL == "" }
