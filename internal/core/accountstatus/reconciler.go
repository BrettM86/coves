package accountstatus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Reconciler applies PDS-confirmed account statuses and retries failed
// confirmations.
//
// A failed confirmation keeps the stored restriction and schedules a recheck
// row; the recheck worker (RecheckDue) retries it with backoff until the PDS
// answers, and a newer #account event that asks the PDS again supersedes it.
// The exception is an activation of an account with no stored restriction:
// it asks no PDS and keeps a pending recheck, so a forged active event cannot
// cancel a pending takedown check.
//
// Jetstream carries #account events for the whole network. A takendown or
// suspended event for a DID with no Coves presence (State.Present) and no
// stored restriction is still confirmed and a confirmed restriction stored,
// since content the account wrote before the takedown can be indexed after
// the event. A failed confirmation for such a DID stores nothing and keeps no
// recheck row, so it is not retried. An activation for such a DID asks no
// PDS; see docs/MODERATION.md.
type Reconciler struct {
	store     Store
	confirmer Confirmer
	now       func() time.Time
}

// NewReconciler returns a Reconciler over store and confirmer, reading the
// time from now.
func NewReconciler(store Store, confirmer Confirmer, now func() time.Time) *Reconciler {
	return &Reconciler{store: store, confirmer: confirmer, now: now}
}

// HandleEvent reconciles one #account event for did, already filtered to a
// takendown or suspended status (active false) or an activation (active true).
//
// An activation for a DID with no Coves presence and no stored restriction
// asks no PDS, changes no restriction and clears any recheck row; a takendown
// or suspended event for such a DID is confirmed like any other, except that
// a failed confirmation clears any recheck row instead of scheduling one. An
// activation of a present account with no restriction asks no PDS and changes
// nothing, keeping any pending recheck. Otherwise the PDS answer is applied and any pending recheck
// cleared; a failed confirmation keeps the restriction, schedules the first
// recheck (restarting any earlier backoff, since this event supersedes it) and
// returns nil. When ctx itself ends during the confirmation the error is
// returned and nothing changes. A store failure is returned.
func (r *Reconciler) HandleEvent(ctx context.Context, did string, active bool) error {
	err := r.store.Reconcile(ctx, did, func(ctx context.Context, current State) (Change, error) {
		if unknownAccount(current) {
			if active {
				return dropUnknown(current), nil
			}
			change, failed, err := r.confirm(ctx, did, current, 1)
			if err != nil || !failed {
				return change, err
			}
			// The recheck worker drops an unknown DID's row unasked, so keep none.
			return dropUnknown(current), nil
		}
		if active && !current.Found {
			// Nothing to lift; skip the PDS call every activation would cost.
			return Change{Kind: ChangeNone}, nil
		}
		change, _, err := r.confirm(ctx, did, current, 1)
		return change, err
	})
	if err != nil {
		return fmt.Errorf("reconciling the account status of %s: %w", did, err)
	}
	return nil
}

// RecheckReport counts one RecheckDue pass: Due rechecks listed, of which
// Confirmed got a PDS answer, Failed failed again (RepoNotFound over a stored
// restriction included) and Dropped belonged to a DID with no Coves presence
// and no stored restriction, cleared without asking any PDS. A listed recheck
// superseded before its turn counts in Due only.
type RecheckReport struct {
	Due, Confirmed, Failed, Dropped int
}

// RecheckDue retries up to limit confirmations whose recheck is due:
// max(1, limit/10) slots go to the oldest due whatever their attempts, the
// rest fewest attempts first, then oldest due. Each account is reconciled
// under the store's lock and skipped without a PDS call when its recheck row
// is gone or no longer due by then; a DID with no Coves presence and no stored
// restriction has its row cleared without a PDS call and counts as Dropped. A
// failure advances the row's backoff. The pass stops at the first store error or when
// ctx ends, recording no attempt for the account in flight.
func (r *Reconciler) RecheckDue(ctx context.Context, limit int) (RecheckReport, error) {
	passStart := r.now()
	dids, err := r.store.DueRechecks(ctx, passStart, limit)
	if err != nil {
		return RecheckReport{}, fmt.Errorf("listing due account status rechecks: %w", err)
	}

	report := RecheckReport{Due: len(dids)}
	for _, did := range dids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		var outcome recheckOutcome
		err := r.store.Reconcile(ctx, did, func(ctx context.Context, current State) (Change, error) {
			outcome = recheckSkipped
			if current.Recheck == nil || current.Recheck.NextAttemptAt.After(passStart) {
				// Superseded by a newer event or rescheduled since listing.
				return Change{Kind: ChangeNone}, nil
			}
			if unknownAccount(current) {
				outcome = recheckDropped
				return dropUnknown(current), nil
			}
			change, failed, err := r.confirm(ctx, did, current, current.Recheck.Attempts+1)
			if err != nil {
				return Change{}, err
			}
			outcome = recheckConfirmed
			if failed {
				outcome = recheckFailed
			}
			return change, nil
		})
		if err != nil {
			return report, fmt.Errorf("rechecking the account status of %s: %w", did, err)
		}
		switch outcome {
		case recheckConfirmed:
			report.Confirmed++
		case recheckFailed:
			report.Failed++
		case recheckDropped:
			report.Dropped++
		}
	}
	return report, nil
}

type recheckOutcome int

const (
	recheckSkipped recheckOutcome = iota
	recheckConfirmed
	recheckFailed
	recheckDropped
)

// unknownAccount reports whether current is a DID Coves holds nothing for:
// no presence and no stored restriction.
func unknownAccount(current State) bool {
	return !current.Present && !current.Found
}

// dropUnknown is the change for an unknown DID: no restriction change, and
// its recheck row, if any, cleared.
func dropUnknown(current State) Change {
	if current.Recheck == nil {
		return Change{Kind: ChangeNone}
	}
	return Change{Kind: ChangeNone, Recheck: RecheckChange{Action: RecheckClear}}
}

// confirm asks the PDS for did's status and returns the change to apply:
// the confirmed answer with the recheck cleared, or, on a failed
// confirmation, no restriction change with a recheck scheduled as attempt
// failedAttempts (failed reports that). RepoNotFound clears the recheck when
// no restriction is stored, and over a stored restriction keeps it and counts
// as a failure with FailureRepoNotFound, since a PDS that lost track of a
// taken-down repo is no answer that lifts it. An error means ctx ended during the
// confirmation and nothing may change.
func (r *Reconciler) confirm(ctx context.Context, did string, current State, failedAttempts int) (change Change, failed bool, err error) {
	confirmed, confirmErr := r.confirmer.RepoStatus(ctx, did)
	if confirmErr == nil {
		change = confirmedChange(confirmed, current.Found)
		change.Recheck = RecheckChange{Action: RecheckClear}
		return change, false, nil
	}
	failure := FailureOf(confirmErr)
	if errors.Is(confirmErr, ErrRepoNotFound) {
		if !current.Found {
			return Change{Kind: ChangeNone, Recheck: RecheckChange{Action: RecheckClear}}, false, nil
		}
		failure = FailureRepoNotFound
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return Change{}, false, fmt.Errorf("confirming the repo status of %s: %w (confirmation: %w)", did, ctxErr, confirmErr)
	}

	slog.Warn("account status confirmation failed",
		"did", did,
		"failure", string(failure),
		"attempts", failedAttempts,
		"error", confirmErr,
	)
	return Change{
		Kind: ChangeNone,
		Recheck: RecheckChange{
			Action:        RecheckSchedule,
			Attempts:      failedAttempts,
			NextAttemptAt: r.now().Add(RecheckDelay(failedAttempts)),
			LastErrorCode: failure,
		},
	}, true, nil
}

// confirmedChange maps a PDS-confirmed answer onto the stored restriction:
// active lifts a stored restriction, takendown or suspended sets it, and any
// other status changes nothing.
func confirmedChange(confirmed RepoStatus, found bool) Change {
	switch {
	case confirmed.Active && found:
		return Change{Kind: ChangeClear}
	case confirmed.Active:
		return Change{Kind: ChangeNone}
	case confirmed.Status == string(Takendown):
		return Change{Kind: ChangeSet, Status: Takendown}
	case confirmed.Status == string(Suspended):
		return Change{Kind: ChangeSet, Status: Suspended}
	default:
		return Change{Kind: ChangeNone}
	}
}
