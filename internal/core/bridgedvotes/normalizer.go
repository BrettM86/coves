package bridgedvotes

import (
	"net"
	"net/url"
	"strings"
)

// NormalizeHost is THE shared normalizer for bridge trust and poll routing;
// jetstream.normalizePDSHost delegates here so provenance checks and poller
// selection cannot drift into different answers. The fallback canonicalizes
// schemeless inputs for BridgeTrust's legacy equal-form comparison; it cannot
// match them to schemeful config, and NewPoller rejects them as dial targets.
// In normal operation identity resolution supplies schemeful communities.pds_url.
func NormalizeHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.ToLower(strings.TrimRight(s, "/"))
	}

	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}

	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host
}
