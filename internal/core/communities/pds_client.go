package communities

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	covesoauth "Coves/internal/atproto/oauth"

	"github.com/bluesky-social/indigo/util"
	"github.com/hashicorp/go-retryablehttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// pdsRequestTimeout is the ceiling every xrpc call in this package has always
// run under, and it is indigo's rather than ours: util.RobustHTTPClient sets
// exactly this on the client it hands back. It is re-applied over the shared
// SSRF client's 15s for the reason blobs/service.go and pds/factory.go re-apply
// theirs — halving the allowance for every community provisioning and token
// refresh would be a second change wearing an SSRF fix's clothes.
//
// It bounds the WHOLE call including retries, again matching indigo: the inner
// client's own Timeout is cleared so three attempts cannot add up to ninety
// seconds.
const pdsRequestTimeout = 30 * time.Second

// pdsClientConfig is what the options below assemble.
type pdsClientConfig struct {
	// allowPrivateHosts opens the SSRF hatch. NEVER set in production.
	allowPrivateHosts bool

	// transportOptions is the TEST SEAM, unexported deliberately: the resolver
	// seam these tests need must not be reachable from any non-test package.
	// pds/factory.go's field of the same name is the shape this copies.
	transportOptions []covesoauth.Option
}

// PDSClientOption configures the HTTP client this package's xrpc calls go
// through.
type PDSClientOption func(*pdsClientConfig)

// WithPrivateHostsAllowed disables the SSRF address guard on the PDS clients
// this package builds.
//
// THE NAME IS THE CONTRACT: production must not call this. The host these calls
// dial is a community's PDSURL — a per-community database column — and the
// AppView shares a network with its Postgres, its PDS, its Jetstream and, in
// production, a metadata endpoint that hands credentials to anything that can
// reach it. Tests and the local dev stack drive a PDS on loopback, which is
// exactly the address class the guard refuses.
func WithPrivateHostsAllowed() PDSClientOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(c *pdsClientConfig) { c.allowPrivateHosts = true }
}

// withTransportOptions is the test seam, unexported so production cannot reach
// it. See pdsClientConfig.transportOptions.
func withTransportOptions(opts ...covesoauth.Option) PDSClientOption {
	return func(c *pdsClientConfig) { c.transportOptions = append(c.transportOptions, opts...) }
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass: the hatch when it is set, and NOTHING when it is not.
//
// It mirrors oauth.PrivateAddressOptions and the same helper in blobs,
// imageproxy, unfurl, jetstream and pds, and it is a function rather than an
// `if` at the call site for the reason documented there: `.env.ci:140` sets
// IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch at every call site
// holding such a boolean. A unit test against this function is the only place in
// the repository where the branch production actually runs is ever evaluated.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly the constructor's own
// defaults.
func PrivateHostOptions(allowPrivate bool) []PDSClientOption {
	if !allowPrivate {
		return nil
	}
	return []PDSClientOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// newPDSHTTPClient builds the client this package's xrpc.Client values carry.
//
// # WHAT IT FIXES, AND WHY NOTHING HERE LOOKED WRONG
//
// The four call sites in this package build `&xrpc.Client{Host: pdsURL}` and
// leave the optional `.Client` field nil. indigo's getClient() (xrpc/xrpc.go:31)
// then substitutes util.RobustHTTPClient() on EVERY call — so the unguarded
// client is real, is used on every request, and appears in this repository's
// source not at all. That is why an audit sweeping for `&http.Client{` walked
// past all four: there is nothing to grep for. The fix is to stop omitting the
// field.
//
// # WHAT THE ADDRESS GUARD IS FOR HERE
//
// pdsURL is a community's PDSURL, a per-community database column. It is
// operator-pinned today — every community lives on this instance's own PDS — and
// pds_provisioning.go's own doc comment describes V2.1 portability to non-Coves
// PDSs, which is the change that makes this column carry a value somebody else
// chose. Two of the four sites are worse than an ordinary SSRF while they wait:
// refreshPDSToken sends the community's refresh token as the Authorization
// header, and reauthenticateWithPassword POSTs its CLEARTEXT password. The
// credential is on the wire before any response exists, so "the address was
// dialled" and "a live credential left the process" are one event.
//
// # THE RETRY WRAPPER IS PRESERVED, NOT ADDED
//
// util.RobustHTTPClient is not merely an unguarded client — it is an unguarded
// client with three retries, indigo's XRPC retry policy (which treats 429 as
// final), otel instrumentation and a 30s total ceiling. Replacing it with a bare
// guarded client would fix the SSRF hole and silently delete the retries, so one
// transient 5xx would fail a community's provisioning or its token refresh. This
// reproduces RobustHTTPClient exactly, with the ONE substitution that is the
// point: the SSRF-safe transport where cleanhttp's pooled transport used to be.
//
// LAYER ORDER MATTERS AND IS INDIGO'S. otel wraps the transport that dials, so
// a refused dial is still a recorded span; retryablehttp sits outside the client
// entirely, so each attempt is a full guarded round trip — resolved, classified
// and dialled afresh — rather than a retry against an address vetted once.
func newPDSHTTPClient(opts ...PDSClientOption) *http.Client {
	cfg := &pdsClientConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	inner := covesoauth.NewSSRFSafeHTTPClient(
		append(covesoauth.PrivateAddressOptions(cfg.allowPrivateHosts), cfg.transportOptions...)...)
	inner.Transport = otelhttp.NewTransport(inner.Transport)

	// Cleared so the 30s below is a budget for the whole call rather than for
	// each of four attempts, which is how util.RobustHTTPClient spends it.
	inner.Timeout = 0

	// coves:allow-bare-client: NewClient installs cleanhttp.DefaultPooledClient (go-retryablehttp client.go:431); the next line replaces it with the guarded client built above
	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = inner
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 10 * time.Second
	retryClient.Logger = retryablehttp.LeveledLogger(pdsRetryLogger{
		inner: slog.Default().With("subsystem", "communities.pdsClient"),
	})
	retryClient.CheckRetry = pdsRetryPolicy

	client := retryClient.StandardClient()
	client.Timeout = pdsRequestTimeout
	return client
}

// pdsRetryPolicy is indigo's XRPC retry policy with the transport's own
// refusals made FINAL.
//
// retryablehttp's default treats any transport-level error as transient, so
// without this a refused address is dialled again after 1s, 2s and 4s — four
// identical security decisions, seven seconds of latency on a path a request is
// waiting behind, and four log lines suggesting a flaky network where there is a
// deliberate block. A response over the byte cap is the same kind of answer:
// re-fetching it produces the same oversized body.
//
// It returns the error rather than nil so the caller still sees WHY, which is
// what every assertion on ErrBlockedAddress in this package depends on.
//
// Everything else stays indigo's, including its one deliberate departure from
// retryablehttp's default: 429 is NOT retried, so rate limiting is the
// application's decision rather than a wait this client takes on its behalf.
func pdsRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if errors.Is(err, covesoauth.ErrBlockedAddress) || errors.Is(err, covesoauth.ErrResponseTooLarge) {
		return false, err
	}
	return util.XRPCRetryPolicy(ctx, resp, err)
}

// pdsRetryLogger adapts slog to retryablehttp's leveled logger.
//
// It exists because indigo's own util.LeveledSlog has an unexported field, so it
// cannot be constructed from here — the type is exported and uninstantiable. The
// level mapping is indigo's: a retryablehttp ERROR is an INTERMEDIATE failure
// that is about to be retried, so logging it at error level would page someone
// about a request that then succeeded.
type pdsRetryLogger struct {
	inner *slog.Logger
}

func (l pdsRetryLogger) Error(msg string, args ...any) { l.inner.Warn(msg, args...) }
func (l pdsRetryLogger) Warn(msg string, args ...any)  { l.inner.Warn(msg, args...) }
func (l pdsRetryLogger) Info(msg string, args ...any)  { l.inner.Info(msg, args...) }
func (l pdsRetryLogger) Debug(msg string, args ...any) { l.inner.Debug(msg, args...) }
