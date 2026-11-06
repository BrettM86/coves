package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/accountstatus"
)

const (
	accountStatusRecheckInterval  = time.Minute
	accountStatusRecheckBatchSize = 50
)

// accountStatusRechecker retries the account status confirmations whose
// recheck is due.
type accountStatusRechecker interface {
	RecheckDue(ctx context.Context, limit int) (accountstatus.RecheckReport, error)
}

// startAccountStatusRecheckJob periodically retries due account status
// confirmations: the #account takedowns, suspensions and restores whose PDS
// confirmation failed. PDSes emit #account only on change, so without this
// pass an outage during the event would leave the restriction wrong until the
// next change. A pass cut at the cycle deadline is logged at Info with its
// counts, a pass ended by shutdown is not logged as a failure, and any other
// failed pass is logged at Error; the next tick retries.
func startAccountStatusRecheckJob(ctx context.Context, wg *sync.WaitGroup, rechecker accountStatusRechecker, interval time.Duration) {
	if rechecker == nil || interval <= 0 {
		slog.Warn("account status recheck job not started",
			"rechecker_present", rechecker != nil,
			"interval", interval,
		)
		return
	}

	runTicker(ctx, wg, "account-status-recheck", interval, func(ctx context.Context) {
		report, err := rechecker.RecheckDue(ctx, accountStatusRecheckBatchSize)
		if err != nil {
			// Classify by the pass's own context, not by err: an in-flight
			// query ended by the context returns a driver error that wraps
			// no context error.
			switch ctxErr := ctx.Err(); {
			case errors.Is(ctxErr, context.DeadlineExceeded):
				slog.Info("account status recheck pass cut at the cycle deadline",
					"error", err,
					"due", report.Due,
					"confirmed", report.Confirmed,
					"failed", report.Failed,
					"dropped", report.Dropped,
				)
			case ctxErr != nil:
				// Shutdown, not a failure.
				slog.Debug("account status recheck pass ended by shutdown", "error", err)
			default:
				slog.Error("account status recheck pass failed",
					"error", err,
					"due", report.Due,
					"confirmed", report.Confirmed,
					"failed", report.Failed,
					"dropped", report.Dropped,
				)
			}
			return
		}
		if report.Due > 0 {
			slog.Info("account status recheck pass completed",
				"due", report.Due,
				"confirmed", report.Confirmed,
				"failed", report.Failed,
				"dropped", report.Dropped,
			)
		}
	})
}
