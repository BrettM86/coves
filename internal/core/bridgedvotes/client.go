package bridgedvotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
)

const (
	maxAggregateBatch = 100
	// One hundred aggregates occupy tens of kilobytes. A misbehaving host must
	// not stream unbounded JSON into a long-lived background job.
	maxResponseBytes = 1 << 20
	// clientTimeout is the per-request budget. A full default sweep is twenty
	// batches, so this must leave the 5-minute cycle deadline in
	// startBridgedVotePollJob real headroom: 20 × 10 s = 200 s.
	clientTimeout = 10 * time.Second
	// keepAliveDrainBytes bounds the read that returns a connection to the
	// keep-alive pool after an error: enough for an ordinary error body, far
	// too little for an adversarial host to stream forever.
	keepAliveDrainBytes = 4096
	aggregatesPath      = "/xrpc/social.coves.bridge.getVoteAggregates"
)

// Client fetches vote aggregates from a bridge host.
type Client struct {
	httpClient *http.Client
}

// NewClient builds a Client. A nil httpClient uses the SSRF-guarded default:
// a trusted bridge is third-party infrastructure, not the operator's own PDS,
// so its DNS answers are vetted like every other outbound fetch in this
// codebase. Callers that need the dev-only private-host hatch build the
// guarded client themselves with oauth.PrivateAddressOptions and pass it in.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = covesoauth.NewSSRFSafeHTTPClient()
	}

	// Clone caller-owned clients rather than mutating shared policy. Redirects
	// are forbidden because a compromised trusted bridge must not be able to
	// 302 the poller into an internal service, escaping the operator-configured
	// dial-target invariant; the resulting 3xx is handled as a permanent non-200
	// contract failure. The timeout is set unconditionally so the sweep's time
	// budget does not depend on whichever client was handed in.
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Timeout = clientTimeout
	return &Client{httpClient: &client}
}

type transientError struct {
	err error
}

func (e *transientError) Error() string {
	return e.err.Error()
}

func (e *transientError) Unwrap() error {
	return e.err
}

// IsTransient reports whether err is a fetch fault the next sweep may not see
// again (rate limit, server error, transport failure) as opposed to a contract
// violation. "Transient" here means "do not poison-mark the batch past the
// rotation", not "retry now": the poller never retries within a sweep, which is
// also why the PDS client's choice not to retry 429 is no contradiction.
func IsTransient(err error) bool {
	var transient *transientError
	return errors.As(err, &transient)
}

func isTransientStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return status >= http.StatusInternalServerError && status < 600
}

// drainForReuse reads a bounded tail of an errored body so the connection can
// return to the keep-alive pool. The count and error are irrelevant: every
// caller is already returning a primary error.
func drainForReuse(body io.Reader) {
	_, _ = io.CopyN(io.Discard, body, keepAliveDrainBytes)
}

type rawAggregate struct {
	URI       string `json:"uri"`
	Upvotes   int    `json:"upvotes"`
	Downvotes int    `json:"downvotes"`
	UpdatedAt string `json:"updatedAt"`
}

// GetVoteAggregates fetches aggregates for up to 100 uris from host. The host
// parameter's type is the vetting: a TrustedHost exists only by way of
// ParseTrustedHost, so no stored community URL can reach this dial.
func (c *Client) GetVoteAggregates(ctx context.Context, host TrustedHost, uris []string) ([]Aggregate, error) {
	if host.IsZero() {
		return nil, errors.New("get vote aggregates: host is not a parsed trusted host")
	}
	if len(uris) > maxAggregateBatch {
		return nil, fmt.Errorf("get vote aggregates from %q: batch contains %d URIs; maximum is %d", host, len(uris), maxAggregateBatch)
	}
	if len(uris) == 0 {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host.String()+aggregatesPath, nil)
	if err != nil {
		return nil, fmt.Errorf("build vote aggregate request for %q: %w", host, err)
	}
	query := req.URL.Query()
	for _, uri := range uris {
		query.Add("uris", uri)
	}
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, covesoauth.ErrResponseTooLarge) {
			// The guarded transport refused a declared Content-Length above its
			// cap. That is the bridge violating the contract, not the network.
			return nil, fmt.Errorf("get vote aggregates from %q: %w", host, err)
		}
		return nil, fmt.Errorf("get vote aggregates from %q: %w", host, &transientError{err: err})
	}
	defer func() {
		drainForReuse(resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("HTTP status %d", resp.StatusCode)
		if isTransientStatus(resp.StatusCode) {
			return nil, fmt.Errorf("get vote aggregates from %q: %w", host, &transientError{err: statusErr})
		}
		return nil, fmt.Errorf("get vote aggregates from %q: %w", host, statusErr)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, covesoauth.ErrResponseTooLarge) {
			return nil, fmt.Errorf("decode vote aggregates from %q: %w", host, err)
		}
		// A truncated body or HTTP/2 reset is a transport fault worth retrying;
		// treating it as a permanent contract failure would poison-mark healthy
		// subjects past the rotation without ever receiving a complete response.
		return nil, fmt.Errorf("decode vote aggregates from %q: %w", host, &transientError{err: err})
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("decode vote aggregates from %q: response exceeds %d byte size cap", host, maxResponseBytes)
	}

	// The pointer distinguishes an absent "aggregates" key from an empty list.
	// An empty list is a bridge that knows nothing about these subjects; a
	// missing key is a bridge speaking a different contract, and accepting it
	// would silently mark every subject polled while landing no data.
	var payload struct {
		Aggregates *[]rawAggregate `json:"aggregates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode vote aggregates from %q: %w", host, err)
	}
	if payload.Aggregates == nil {
		return nil, fmt.Errorf("decode vote aggregates from %q: response has no aggregates field", host)
	}

	return c.acceptAggregates(host, uris, *payload.Aggregates)
}

// acceptAggregates applies the response contract entry by entry. A trusted
// bridge may answer only about subjects named in the request — without that
// binding one compromised bridge could rewrite bridged counts for any content
// platform-wide — and first-wins deduplication bounds DB writes to at most one
// per requested subject per response. Counts and their stamp are one atomic
// trio, mirroring validatedBridgedStats in the Jetstream adapter, so one
// malformed member drops the entire entry.
func (c *Client) acceptAggregates(host TrustedHost, uris []string, entries []rawAggregate) ([]Aggregate, error) {
	requested := make(map[string]struct{}, len(uris))
	for _, uri := range uris {
		requested[uri] = struct{}{}
	}
	accepted := make(map[string]struct{}, len(entries))
	aggregates := make([]Aggregate, 0, len(entries))
	now := time.Now()

	var droppedInvalid, droppedUnrequested, droppedDuplicate int
	var sampleURI, sampleReason, sampleUpdatedAt string
	sample := func(entry rawAggregate, reason string) {
		if sampleReason != "" {
			return
		}
		sampleURI, sampleReason, sampleUpdatedAt = entry.URI, reason, entry.UpdatedAt
	}

	for _, entry := range entries {
		if _, ok := requested[entry.URI]; !ok {
			droppedUnrequested++
			sample(entry, "unrequested")
			continue
		}
		if _, ok := accepted[entry.URI]; ok {
			droppedDuplicate++
			sample(entry, "duplicate")
			continue
		}
		asOf, err := ParseAsOf(entry.UpdatedAt, now)
		if err != nil {
			droppedInvalid++
			sample(entry, "invalid updatedAt: "+err.Error())
			continue
		}
		if entry.Upvotes < 0 || entry.Downvotes < 0 ||
			entry.Upvotes > MaxBridgedCount || entry.Downvotes > MaxBridgedCount {
			droppedInvalid++
			sample(entry, "count out of range")
			continue
		}
		accepted[entry.URI] = struct{}{}
		aggregates = append(aggregates, Aggregate{
			URI:       entry.URI,
			Upvotes:   entry.Upvotes,
			Downvotes: entry.Downvotes,
			AsOf:      asOf,
		})
	}

	dropped := droppedInvalid + droppedUnrequested + droppedDuplicate
	if len(entries) > 0 && len(aggregates) == 0 {
		// A response that answers and gets every entry wrong is the same
		// contract break as a malformed body, and takes the same poison-mark
		// path. Handling it as a Warn-only partial success would advance
		// watermarks over healthy subjects with no data landing and no error.
		return nil, fmt.Errorf("decode vote aggregates from %q: all %d entries rejected (invalid=%d unrequested=%d duplicate=%d; first: %s %s updatedAt=%q)",
			host, len(entries), droppedInvalid, droppedUnrequested, droppedDuplicate, sampleURI, sampleReason, sampleUpdatedAt)
	}
	if dropped > 0 {
		slog.Warn("bridged vote response entries dropped",
			"host", host.String(),
			"invalid", droppedInvalid,
			"unrequested", droppedUnrequested,
			"duplicates", droppedDuplicate,
			"sample_uri", sampleURI,
			"sample_reason", sampleReason,
			"sample_updated_at", sampleUpdatedAt,
		)
	}

	return aggregates, nil
}
