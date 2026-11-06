package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// didWebHostsKey is the environment variable ParseDIDWebHosts reads.
const didWebHostsKey = "DID_WEB_HOSTS"

// ParseDIDWebHosts parses DID_WEB_HOSTS: comma-separated host=host:port pairs
// with an exact did:web host on the left.
//
// Each entry redirects the fetch of https://<host>/.well-known/did.json to
// http://<host:port>/.well-known/did.json. The left side is an EXACT hostname,
// not a suffix: a did:web DID names one host, so there is nothing to match on a
// label boundary, and a leading dot is refused rather than read as a suffix.
//
// An empty value returns nil. Any malformed entry is an error, for the same
// reason as HANDLE_WELL_KNOWN_HOSTS: a partial parse leaves one DID resolving
// and another silently not. Whether the variable may be set at all (dev and CI
// only) is decided by the caller.
func ParseDIDWebHosts(raw string) (map[string]string, error) {
	var entries []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			entries = append(entries, part)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}

	hosts := make(map[string]string, len(entries))
	for _, entry := range entries {
		rawHost, rawTarget, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("%s: %q is not a <host>=<host:port> entry", didWebHostsKey, entry)
		}
		host := strings.ToLower(strings.TrimSpace(rawHost))
		target := strings.TrimSpace(rawTarget)
		if host == "" {
			return nil, fmt.Errorf("%s: %q names no did:web host", didWebHostsKey, entry)
		}
		if strings.HasPrefix(host, ".") {
			return nil, fmt.Errorf("%s: %q must name an exact did:web host, not a suffix", didWebHostsKey, host)
		}
		if _, err := syntax.ParseHandle(host); err != nil {
			return nil, fmt.Errorf("%s: %q is not a did:web hostname", didWebHostsKey, host)
		}
		if target == "" {
			return nil, fmt.Errorf("%s: %q names no host:port for %q", didWebHostsKey, entry, host)
		}
		if strings.Contains(target, "://") || strings.Contains(target, "/") {
			return nil, fmt.Errorf("%s: %q in %q must be a bare host:port; the scheme and path are supplied "+
				"by the resolver", didWebHostsKey, target, entry)
		}
		targetHost, port, splitErr := net.SplitHostPort(target)
		if splitErr != nil || targetHost == "" || port == "" {
			return nil, fmt.Errorf("%s: %q in %q is not a host:port", didWebHostsKey, target, entry)
		}
		if portNumber, portErr := strconv.Atoi(port); portErr != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("%s: %q in %q is not a port number between 1 and 65535",
				didWebHostsKey, port, entry)
		}
		if _, duplicate := hosts[host]; duplicate {
			return nil, fmt.Errorf("%s: host %q appears more than once", didWebHostsKey, host)
		}
		hosts[host] = target
	}
	return hosts, nil
}

// didWebHostsVar reads DID_WEB_HOSTS and refuses it outside dev, where it would
// let configuration redirect did:web resolution to an operator-named address.
func didWebHostsVar(isDevEnv bool) (map[string]string, error) {
	raw := lookup(didWebHostsKey)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if !isDevEnv {
		return nil, fmt.Errorf("%s: did:web resolution may only be redirected in dev and CI; "+
			"set IS_DEV_ENV=true or unset %s", didWebHostsKey, didWebHostsKey)
	}
	return ParseDIDWebHosts(raw)
}
