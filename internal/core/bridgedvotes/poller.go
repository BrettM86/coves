package bridgedvotes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultLookback = 90 * 24 * time.Hour

	// Two thousand subjects are twenty full bridge requests. That bounds one
	// sweep against the bridge's 10 rps/burst-30 limiter while leaving headroom
	// for other AppView traffic sharing the same limiter.
	defaultSweepCap = 2_000

	// maxTransientStreak is how many consecutive sweeps one batch may fail
	// transiently before it is marked past the rotation anyway. Transient
	// failures deliberately leave a batch unmarked so an outage is retried, but
	// selection is deterministic oldest-first, so a single subject the bridge
	// 5xxs on would otherwise sit at the head of its host's rotation and block
	// every other subject behind it forever. Three sweeps at the default
	// interval is fifteen minutes: long enough to ride out a deploy, short
	// enough that a poisoned head costs one batch of healthy rows a cycle
	// rather than the whole host.
	maxTransientStreak = 3
)

// Poller runs sweeps: select candidates, batch per bridge host, fetch, apply,
// advance watermarks.
type Poller struct {
	store        Store
	client       *Client
	trustedHosts []TrustedHost
	opts         Options

	// transientStreaks counts consecutive transient failures per leading
	// batch across sweeps. Sweeps run on one goroutine under runTicker; the
	// mutex is for callers that do not.
	mu               sync.Mutex
	transientStreaks map[string]int
}

// NewPoller wires a Poller over the store, client, and the operator's trusted
// bridge hosts (TRUSTED_BRIDGE_PDS_HOSTS). Every host must satisfy
// ParseTrustedHost; config.Validate applies the same rule at boot, so a
// rejection here means the poller was built from an unvalidated Config.
func NewPoller(store Store, client *Client, trustedHosts []string, opts Options) (*Poller, error) {
	if store == nil {
		return nil, errors.New("creating bridged vote poller: Store is nil")
	}
	if client == nil {
		return nil, errors.New("creating bridged vote poller: Client is nil")
	}
	parsed := make([]TrustedHost, 0, len(trustedHosts))
	for _, raw := range trustedHosts {
		host, err := ParseTrustedHost(raw)
		if err != nil {
			// Rejecting one bad member avoids a poller that reports itself
			// configured but silently never reaches that bridge.
			return nil, fmt.Errorf("creating bridged vote poller: %w", err)
		}
		parsed = append(parsed, host)
	}
	if opts.Lookback <= 0 {
		opts.Lookback = defaultLookback
	}
	if opts.SweepCap <= 0 {
		opts.SweepCap = defaultSweepCap
	}

	return &Poller{
		store:            store,
		client:           client,
		trustedHosts:     parsed,
		opts:             opts,
		transientStreaks: make(map[string]int),
	}, nil
}

// hostRouting is the sweep's view of which stored community URLs map to which
// trusted host. It is built once per sweep from the store's distinct URLs.
type hostRouting struct {
	// order is the matched hosts in first-seen order; it fixes iteration order
	// for selection, fetching and error reporting.
	order []TrustedHost
	// storedByHost lists the exact stored strings that matched each host; they
	// go back to the store as its equality filter.
	storedByHost map[TrustedHost][]string
	// hostByStored maps each matched stored string to its host, for the
	// defense-in-depth recheck of what the store returns.
	hostByStored map[string]TrustedHost
}

func (p *Poller) routeStoredHosts(storedHosts []string) hostRouting {
	hostByNormalized := make(map[string]TrustedHost, len(p.trustedHosts))
	for _, host := range p.trustedHosts {
		hostByNormalized[host.String()] = host
	}

	routing := hostRouting{
		storedByHost: make(map[TrustedHost][]string, len(p.trustedHosts)),
		hostByStored: make(map[string]TrustedHost, len(storedHosts)),
	}
	for _, stored := range storedHosts {
		host, ok := hostByNormalized[NormalizeHost(stored)]
		if !ok {
			continue
		}
		if _, duplicate := routing.hostByStored[stored]; duplicate {
			continue
		}
		routing.hostByStored[stored] = host
		if _, seen := routing.storedByHost[host]; !seen {
			routing.order = append(routing.order, host)
		}
		routing.storedByHost[host] = append(routing.storedByHost[host], stored)
	}
	return routing
}

// Sweep runs one poll cycle. The Report is populated even when err is non-nil,
// so a partially failed sweep still tells the job what it managed to do.
func (p *Poller) Sweep(ctx context.Context) (Report, error) {
	report := Report{TrustedHosts: len(p.trustedHosts)}

	// No configured bridge means no trust root. Return before even reading stored
	// URLs so an unconfigured deployment cannot accidentally turn database values
	// into network destinations.
	if len(p.trustedHosts) == 0 {
		return report, nil
	}

	storedHosts, err := p.store.DistinctCommunityPDSURLs(ctx)
	if err != nil {
		return report, joinSweepErrors(fmt.Errorf("list community PDS URLs for bridged vote sweep: %w", err))
	}
	report.StoredHosts = len(storedHosts)

	routing := p.routeStoredHosts(storedHosts)
	report.MatchedHosts = len(routing.order)
	if len(routing.order) == 0 {
		return report, nil
	}

	// A global select followed by grouping let one transiently failing bridge's
	// deep never-polled backlog consume the entire cap forever: because those rows
	// correctly remained unmarked for retry, healthy bridges never entered a sweep.
	// Give every matched host its own budget while preserving the cap exactly for
	// the single-host case. The 100-row floor is one full client batch and keeps
	// small multi-host caps from recreating starvation with tiny allocations.
	perHost := p.opts.SweepCap
	if len(routing.order) > 1 {
		perHost = max(p.opts.SweepCap/len(routing.order), maxAggregateBatch)
	}

	var sweepErrors []error
	failedHosts := make(map[TrustedHost]struct{})
	batchURIsByHost := make(map[TrustedHost][]string, len(routing.order))
	for _, host := range routing.order {
		candidates, err := p.store.SelectCandidates(ctx, routing.storedByHost[host], p.opts.Lookback, perHost)
		if err != nil {
			// The store is shared, so a selection fault is rarely host-specific,
			// but the fetch loop below isolates per host and selection should
			// not be the one stage that lets a single failure empty the sweep.
			sweepErrors = append(sweepErrors, fmt.Errorf("select bridged vote sweep candidates for %q: %w", host, err))
			failedHosts[host] = struct{}{}
			continue
		}
		// Append in repository order: its watermark ordering is the fairness
		// contract, and batching must not reshuffle candidates within a host.
		for _, candidate := range candidates {
			if matched, ok := routing.hostByStored[candidate.StoredPDSURL]; !ok || matched != host {
				// Defense in depth: even if a Store returns more than this host's
				// requested values, a database URL never becomes a dial target or
				// crosses into another host's budget. The real repository filters
				// by exact equality, so this firing means a Store bug.
				slog.Warn("bridged vote candidate outside its host filter dropped",
					"host", host.String(),
					"stored_pds_url", candidate.StoredPDSURL,
					"uri", candidate.URI,
				)
				continue
			}
			batchURIsByHost[host] = append(batchURIsByHost[host], candidate.URI)
			report.Candidates++
		}
	}

	for _, host := range routing.order {
		if _, failed := failedHosts[host]; failed {
			continue
		}
		hostErrs, fatal := p.sweepHost(ctx, host, batchURIsByHost[host], &report)
		if len(hostErrs) > 0 {
			// Kept as separate leaves, not pre-joined: joinSweepErrors classifies
			// per leaf, so a canceled mark must not drag the fetch failure it
			// followed out of the job log with it.
			sweepErrors = append(sweepErrors, hostErrs...)
			failedHosts[host] = struct{}{}
		}
		if fatal {
			break
		}
	}
	report.FailedHosts = len(failedHosts)

	return report, joinSweepErrors(sweepErrors...)
}

// sweepHost fetches, applies and marks one host's candidates in client-sized
// batches. It returns the host's errors, if any, and whether the last of them
// is a store fault the rest of the sweep cannot proceed past: a DB failure is
// not host-isolated, and batches committed and marked earlier remain honest
// completed work.
func (p *Poller) sweepHost(ctx context.Context, host TrustedHost, uris []string, report *Report) (errs []error, fatal bool) {
	for start := 0; start < len(uris); start += maxAggregateBatch {
		end := min(start+maxAggregateBatch, len(uris))
		batchURIs := uris[start:end]

		aggregates, fetchErr := p.client.GetVoteAggregates(ctx, host, batchURIs)
		if fetchErr != nil {
			fetchErr = fmt.Errorf("fetch bridged vote batch from %q: %w", host, fetchErr)
			if markErr := p.handleFetchFailure(ctx, host, batchURIs, fetchErr, report); markErr != nil {
				return []error{fetchErr, markErr}, true
			}
			// A host fault should not block another bridge, but continuing later
			// batches on the same failed host would amplify load during an outage.
			return []error{fetchErr}, false
		}
		p.clearTransientStreak(host, batchURIs)
		report.Fetched += len(aggregates)

		for _, aggregate := range aggregates {
			if err := p.store.ApplyAggregate(ctx, aggregate); err != nil {
				return []error{fmt.Errorf("apply bridged vote aggregate for %q: %w", aggregate.URI, err)}, true
			}
			report.Applied++
		}
		// Advance every attempted URI, including subjects omitted by the bridge,
		// so absent aggregates cannot monopolize the oldest rotation slots.
		if err := p.store.MarkPolled(ctx, batchURIs); err != nil {
			return []error{fmt.Errorf("mark bridged vote batch polled for %q: %w", host, err)}, true
		}
		report.Marked += len(batchURIs)
	}
	return nil, false
}

// handleFetchFailure decides whether a failed batch stays at its watermark for
// retry or is marked past the rotation. It returns a non-nil error only when
// the mark itself failed.
func (p *Poller) handleFetchFailure(ctx context.Context, host TrustedHost, batchURIs []string, fetchErr error, report *Report) error {
	if ctx.Err() != nil {
		// The sweep's own context ended: shutdown, or the cycle deadline in
		// runGuarded. Neither says anything about the bridge, so the batch
		// stays at its watermark and does not count toward a streak — a
		// deadline that lands on the same batch three cycles running would
		// otherwise poison-mark healthy rows for the AppView's slowness.
		return nil
	}

	reason := "permanent contract failure"
	if IsTransient(fetchErr) {
		streak := p.recordTransientStreak(host, batchURIs)
		if streak < maxTransientStreak {
			slog.Warn("bridged vote batch failed transiently; left at its watermark for retry",
				"host", host.String(),
				"batch_size", len(batchURIs),
				"consecutive_sweeps", streak,
				"error", fetchErr,
			)
			return nil
		}
		reason = "transient failure persisted across sweeps"
	}

	// A proxy serving HTML with HTTP 200 wedged the Tidepool side in exactly
	// this position: a failure at the oldest watermark must advance or it
	// monopolizes the rotation forever.
	if markErr := p.store.MarkPolled(ctx, batchURIs); markErr != nil {
		return fmt.Errorf("mark poison bridged vote batch polled for %q: %w", host, markErr)
	}
	p.clearTransientStreak(host, batchURIs)
	report.Marked += len(batchURIs)
	report.PoisonMarked += len(batchURIs)
	slog.Warn("bridged vote batch skipped past rotation",
		"host", host.String(),
		"reason", reason,
		"batch_size", len(batchURIs),
		"first_uri", batchURIs[0],
		"error", fetchErr,
	)
	return nil
}

// streakKey identifies a batch across sweeps. Selection is deterministic, so a
// batch that failed last sweep is the same leading run of URIs this sweep; the
// first and last member are enough to recognize it without hashing the set.
func streakKey(host TrustedHost, batchURIs []string) string {
	return host.String() + "\x00" + batchURIs[0] + "\x00" + batchURIs[len(batchURIs)-1]
}

func (p *Poller) recordTransientStreak(host TrustedHost, batchURIs []string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := streakKey(host, batchURIs)
	p.transientStreaks[key]++
	return p.transientStreaks[key]
}

func (p *Poller) clearTransientStreak(host TrustedHost, batchURIs []string) {
	if len(batchURIs) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.transientStreaks, streakKey(host, batchURIs))
}

// joinSweepErrors keeps shutdown cancellation quiet only when it is the sole
// failure. A canceled DB or fetch leaf joined with an earlier bridge fault would
// otherwise make errors.Is(result, context.Canceled) true and suppress the whole
// cycle in startBridgedVotePollJob, hiding the actionable failure.
func joinSweepErrors(errs ...error) error {
	nonCancellation := make([]error, 0, len(errs))
	cancellation := make([]error, 0, 1)
	for _, err := range errs {
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			cancellation = append(cancellation, err)
			continue
		}
		nonCancellation = append(nonCancellation, err)
	}
	if len(nonCancellation) > 0 {
		return errors.Join(nonCancellation...)
	}
	if len(cancellation) == 1 {
		return cancellation[0]
	}
	return errors.Join(cancellation...)
}
