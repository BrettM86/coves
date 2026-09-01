package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/posts"
)

const (
	// oauthCleanupInterval is how often expired OAuth sessions and pending
	// authorization requests are purged.
	oauthCleanupInterval = time.Hour

	// tokenRefreshInterval is how often aggregator OAuth tokens are checked
	// for imminent expiry.
	tokenRefreshInterval = 30 * time.Minute

	// tokenRefreshExpiryBuffer is how far ahead of expiry a token is
	// refreshed. At two ticks of headroom, a single failed attempt still
	// leaves another before the token actually expires.
	tokenRefreshExpiryBuffer = time.Hour

	// tokenRefreshHeartbeatCycles controls how often the refresh job logs
	// that it is alive while it has no work to do. At 6 cycles that is once
	// every three hours — enough to distinguish "idle" from "dead".
	tokenRefreshHeartbeatCycles = 6
)

// runTicker runs work on an interval until ctx is cancelled, recovering from
// panics so a single bad cycle cannot kill the job permanently.
//
// That recovery placement is the point. A recover() guarding the whole
// goroutine logs the panic and then lets the job exit forever, which for
// aggregator token refresh means every aggregator silently stops working at
// the next token expiry — a failure with no error, no alert, and no restart.
// Recovering per cycle turns that into one lost tick.
//
// A cycle runs immediately at startup rather than only after the first full
// interval, matching the dead letter redriver: work that accumulated while the
// process was down should not have to wait out a 30- or 60-minute tick, and a
// frequently-restarted deployment would otherwise never run these jobs at all.
func runTicker(ctx context.Context, wg *sync.WaitGroup, name string, interval time.Duration, work func(context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		runGuarded(ctx, name, interval, work)

		for {
			select {
			case <-ctx.Done():
				slog.Info("background job stopped", "job", name)
				return
			case <-ticker.C:
				runGuarded(ctx, name, interval, work)
			}
		}
	}()
}

// runGuarded executes one cycle, converting a panic into a logged error so the
// surrounding loop survives, and bounding the cycle so a hang cannot stop the
// job permanently.
//
// The deadline is as important as the recover. time.Ticker drops ticks while
// the receiver is busy, so a cycle that blocks forever means the loop never
// re-enters its select: the job stops running, never observes cancellation,
// and logs nothing. That is strictly worse than the panic case, which at least
// leaves a record.
func runGuarded(ctx context.Context, name string, timeout time.Duration, work func(context.Context)) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("background job cycle panicked; job continues",
				"job", name,
				"panic", recovered,
			)
		}
	}()

	cycleCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	work(cycleCtx)
}

// expiredRecordCleaner removes OAuth records that have aged out. Declared here
// rather than taking the concrete store so the job body is testable without a
// database.
type expiredRecordCleaner interface {
	CleanupExpiredSessions(ctx context.Context) (int64, error)
	CleanupExpiredAuthRequests(ctx context.Context) (int64, error)
}

// startOAuthCleanupJob purges expired OAuth sessions and abandoned
// authorization requests on an interval, so neither table grows without bound.
//
// The cleaner is resolved once, by the caller, rather than re-derived inside
// every cycle: the previous shape returned early on a nil store with no log,
// so wrapping the OAuth store in any decorator would have turned this into a
// silent hourly no-op while both tables grew without bound — discovered
// eventually as disk pressure, with nothing pointing back here.
func startOAuthCleanupJob(ctx context.Context, wg *sync.WaitGroup, cleaner expiredRecordCleaner) {
	runTicker(ctx, wg, "oauth-cleanup", oauthCleanupInterval, func(ctx context.Context) {
		sessions, err := cleaner.CleanupExpiredSessions(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("failed to clean up expired OAuth sessions", "error", err)
		}
		requests, err := cleaner.CleanupExpiredAuthRequests(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("failed to clean up expired OAuth auth requests", "error", err)
		}
		if sessions > 0 || requests > 0 {
			slog.Info("OAuth cleanup completed",
				"expired_sessions_removed", sessions,
				"expired_auth_requests_removed", requests,
			)
		}
	})
}

// expiringTokenRefresher renews aggregator OAuth tokens that are close to
// expiry. An interface rather than the concrete service so the job body can be
// exercised without a database.
type expiringTokenRefresher interface {
	RefreshExpiringTokens(ctx context.Context, expiryBuffer time.Duration) (int, []error)
}

// acceptanceQueuePass is one walk of the acceptance engine's backlog.
// Declared as an interface so this job body is testable without an engine, a
// PDS or a database behind it.
type acceptanceQueuePass interface {
	RunPass(ctx context.Context) (posts.PassReport, error)
}

// bridgedVoteSweeper is the seam startBridgedVotePollJob drives: one poll cycle
// against the bridge's vote-aggregate side channel. *bridgedvotes.Poller
// satisfies it.
type bridgedVoteSweeper interface {
	Sweep(ctx context.Context) (bridgedvotes.Report, error)
}

// startBridgedVotePollJob runs the bridged-vote poller on an interval. Bridge
// outages are transient: a failed sweep is logged and the next tick retries.
// runGuarded deadline-bounds each sweep at the interval so a stalled bridge
// cannot permanently stop the rotation.
//
// The guard is defensive only: config.Validate rejects a non-positive interval
// and main.go checks the concrete poller for nil, so reaching the early return
// means one of those was bypassed, which is worth a line in the log.
func startBridgedVotePollJob(ctx context.Context, wg *sync.WaitGroup, poller bridgedVoteSweeper, interval time.Duration) {
	if poller == nil || interval <= 0 {
		slog.Warn("bridged vote poll job not started",
			"poller_present", poller != nil,
			"interval", interval,
		)
		return
	}

	runTicker(ctx, wg, "bridged-vote-poll", interval, func(ctx context.Context) {
		report, err := poller.Sweep(ctx)
		if err != nil {
			// Cancellation alone is shutdown; joinSweepErrors guarantees a real
			// fault joined with a canceled leaf does not classify as canceled.
			if !errors.Is(err, context.Canceled) {
				slog.Error("bridged vote poll sweep failed",
					"error", err,
					"matched_hosts", report.MatchedHosts,
					"failed_hosts", report.FailedHosts,
					"candidates", report.Candidates,
					"applied", report.Applied,
					"marked", report.Marked,
				)
			}
			return
		}

		// Three quiet outcomes need telling apart. A sweep that found no
		// bridged community at all is the normal state of a fresh instance
		// and logs at debug. One that saw stored community hosts and matched
		// none of them to the trust list is the misconfiguration this poller
		// cannot otherwise surface: identity resolution stored one URL form,
		// the operator configured another, and every sweep returns nil.
		switch {
		case report.MatchedHosts == 0 && report.StoredHosts > 0:
			slog.Warn("bridged vote poll matched no community PDS URL to a trusted bridge host",
				"trusted_hosts", report.TrustedHosts,
				"stored_hosts", report.StoredHosts,
			)
		case report.Candidates > 0 || report.PoisonMarked > 0:
			slog.Info("bridged vote poll sweep completed",
				"matched_hosts", report.MatchedHosts,
				"candidates", report.Candidates,
				"fetched", report.Fetched,
				"applied", report.Applied,
				"marked", report.Marked,
				"poison_marked", report.PoisonMarked,
			)
		default:
			slog.Debug("bridged vote poll sweep found nothing to poll",
				"matched_hosts", report.MatchedHosts)
		}
	})
}

// startAcceptanceQueueJob walks the undecided admission backlog on an interval.
//
// It is the PULL half of admission (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6). The
// synchronous write path and the firehose consumer both push work at the
// engine, and neither can see a subject that was left undecided because a
// credential expired or a lookup blipped — this pass is the only thing that
// eventually reaches those, so a deployment where it stops running is one where
// posts quietly stay invisible.
//
// Nothing here fails the process: a pass that could not read the backlog is
// logged and the next tick tries again, because the reason a backlog is
// unreadable is almost always the database being briefly unavailable, which the
// rest of the AppView is already reporting.
func startAcceptanceQueueJob(ctx context.Context, wg *sync.WaitGroup, queue acceptanceQueuePass, interval time.Duration) {
	if queue == nil || interval <= 0 {
		return
	}

	runTicker(ctx, wg, "acceptance-queue", interval, func(ctx context.Context) {
		report, err := queue.RunPass(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Error("acceptance queue pass failed", "error", err)
			}
			return
		}

		// Deferrals and failures are reported apart because they mean opposite
		// things to whoever is deciding whether to page: a pass that defers
		// everything is usually credentials and will clear, while a pass that
		// FAILS everything is a bug. A quiet pass logs nothing, which is the
		// common case on an instance that hosts no communities.
		if report.Processed > 0 || report.Failed > 0 {
			slog.Info("acceptance queue pass completed",
				"listed", report.Listed,
				"processed", report.Processed,
				"settled", report.Settled,
				"deferred", report.Deferred,
				"failed", report.Failed,
			)
		}
	})
}

// startAggregatorTokenRefreshJob proactively refreshes aggregator OAuth tokens
// before they expire.
//
// This complements the on-demand refresh inside APIKeyService (which uses a
// much shorter buffer): without it, an aggregator that goes idle long enough
// for its token to expire would find its next request rejected rather than
// transparently refreshed.
func startAggregatorTokenRefreshJob(ctx context.Context, wg *sync.WaitGroup, refresher expiringTokenRefresher) {
	cycleCount := 0
	runTicker(ctx, wg, "aggregator-token-refresh", tokenRefreshInterval, func(ctx context.Context) {
		cycleCount++

		refreshed, errs := refresher.RefreshExpiringTokens(ctx, tokenRefreshExpiryBuffer)

		// Cancellation during shutdown is not a failure. Logging it at ERROR
		// meant every clean deploy produced error-rate noise.
		reportable := errs[:0:0]
		for _, err := range errs {
			if !errors.Is(err, context.Canceled) {
				reportable = append(reportable, err)
			}
		}

		switch {
		case len(reportable) > 0:
			slog.Warn("aggregator token refresh completed with errors",
				"refreshed", refreshed,
				"failed", len(reportable),
			)
			for _, err := range reportable {
				slog.Error("aggregator token refresh error", "error", err)
			}
		case refreshed > 0:
			slog.Info("aggregator token refresh completed", "refreshed", refreshed)
		case cycleCount%tokenRefreshHeartbeatCycles == 0:
			slog.Info("aggregator token refresh heartbeat: running, no tokens needed refresh",
				"cycles_completed", cycleCount,
			)
		}
	})
}
