package identity

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	indigoidentity "github.com/bluesky-social/indigo/atproto/identity"
)

// didWebDocumentPath is where indigo fetches a did:web DID document.
const didWebDocumentPath = "/.well-known/did.json"

// NewIndigoDirectory builds the indigo identity directory used by the
// AppView's service-JWT validation and by label clients. didWebHosts rewrites
// did:web document fetches for the listed hosts (DID_WEB_HOSTS, dev and CI only).
//
// The HTTP client is the same SSRF-guarded client the resolver uses, with its
// 10s timeout. allowPrivateHosts is an argument, never an ambient read of the
// environment. A did:web mapping dials an operator-named address, so it
// requires allowPrivateHosts and panics without it, like WithWellKnownHosts.
func NewIndigoDirectory(plcURL string, allowPrivateHosts bool, didWebHosts map[string]string) *indigoidentity.BaseDirectory {
	if len(didWebHosts) > 0 && !allowPrivateHosts {
		panic(fmt.Sprintf("identity: did:web host mapping redirects %d host(s) to operator-named addresses, "+
			"so allowPrivateHosts is required alongside it", len(didWebHosts)))
	}
	client := resolverHTTPClient(allowPrivateHosts)
	if len(didWebHosts) > 0 {
		client.Transport = newDIDWebRewriteTransport(client.Transport, didWebHosts)
	}
	return &indigoidentity.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: *client,
	}
}

// didWebRewriteTransport sends https://<host>/.well-known/did.json for a mapped
// host to http://<host:port>/.well-known/did.json, keeping <host> in the Host
// header, and passes every other request to base unchanged. It changes where
// the document is fetched, not how indigo checks it: the fetched document's id
// must still match the DID being resolved.
type didWebRewriteTransport struct {
	// base is the SSRF-guarded transport this wraps; the rewritten address is
	// still dialled through it.
	base  http.RoundTripper
	hosts map[string]string
}

func newDIDWebRewriteTransport(base http.RoundTripper, hosts map[string]string) *didWebRewriteTransport {
	if base == nil {
		panic("identity: did:web host mapping needs a guarded transport to wrap, and the client supplied none")
	}
	lowered := make(map[string]string, len(hosts))
	for host, target := range hosts {
		lowered[strings.ToLower(host)] = target
	}
	return &didWebRewriteTransport{base: base, hosts: lowered}
}

// RoundTrip rewrites a mapped host's DID document fetch and passes every other
// request through.
func (t *didWebRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.Path != didWebDocumentPath {
		return t.base.RoundTrip(req) // coves:allow-bare-client: base IS the guarded transport, delegated to unchanged
	}
	host := strings.ToLower(req.URL.Hostname())
	target, ok := t.hosts[host]
	if !ok {
		return t.base.RoundTrip(req) // coves:allow-bare-client: base IS the guarded transport, delegated to unchanged
	}

	rewritten := req.Clone(req.Context())
	rewritten.Host = host
	rewritten.URL.Scheme = "http"
	rewritten.URL.Host = target

	resp, err := t.base.RoundTrip(rewritten) // coves:allow-bare-client: base IS the guarded transport, and it vets the rewritten address too
	if err != nil {
		// Indigo folds this into a generic resolution failure, so the mapped
		// address is named here. This path exists only in dev and CI.
		err = fmt.Errorf("did:web document fetch for %q redirected to %s: %w", host, target, err)
		log.Printf("WARNING: %v", err)
		return nil, err
	}
	return resp, nil
}
