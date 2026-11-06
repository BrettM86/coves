// Package accountstatus holds an account's PDS-confirmed takendown or
// suspended status, set and cleared from Jetstream #account events.
package accountstatus

import (
	"context"
	"errors"
	"time"
)

// Status is a stored account status restriction.
type Status string

const (
	// Takendown is a repository its PDS reports as taken down.
	Takendown Status = "takendown"
	// Suspended is a repository its PDS reports as suspended.
	Suspended Status = "suspended"
)

// RepoStatus is a PDS's confirmed answer for one repository.
type RepoStatus struct {
	Active bool
	Status string
}

// ErrRepoNotFound is the confirmer's answer when the PDS hosts no such repo.
var ErrRepoNotFound = errors.New("account status: repo not found")

// Confirmer asks the account's PDS for its current repository status.
type Confirmer interface {
	RepoStatus(ctx context.Context, did string) (RepoStatus, error)
}

// Reader reads an account's stored restriction.
type Reader interface {
	Restriction(ctx context.Context, did string) (Status, bool, error)
}

// ChangeKind is what Reconcile applies to an account's row.
type ChangeKind int

const (
	// ChangeNone leaves the account's restriction row as it is.
	ChangeNone ChangeKind = iota
	// ChangeSet stores Change.Status as the account's restriction.
	ChangeSet
	// ChangeClear removes the account's restriction.
	ChangeClear
)

// Change is decide's verdict; Status is meaningful only for ChangeSet.
// Recheck's zero value keeps the account's recheck row as it is.
type Change struct {
	Kind    ChangeKind
	Status  Status
	Recheck RecheckChange
}

// ConfirmationFailure is the stored code for why a PDS confirmation failed.
type ConfirmationFailure string

const (
	// FailureIdentityUnresolved: the DID did not resolve to a PDS endpoint.
	FailureIdentityUnresolved ConfirmationFailure = "identity_unresolved"
	// FailurePDSUnreachable: the request to the PDS could not be made or read.
	FailurePDSUnreachable ConfirmationFailure = "pds_unreachable"
	// FailurePDSErrorStatus: the PDS answered with an error HTTP status.
	FailurePDSErrorStatus ConfirmationFailure = "pds_error_status"
	// FailurePDSInvalidResponse: the PDS answer was oversized, undecodable,
	// for another DID, or missing its active flag.
	FailurePDSInvalidResponse ConfirmationFailure = "pds_invalid_response"
	// FailureUnclassified: the error carried no *ConfirmationError.
	FailureUnclassified ConfirmationFailure = "unclassified"
	// FailureRepoNotFound: the PDS answered RepoNotFound while a restriction
	// is stored; the restriction is kept and the recheck retried with backoff.
	FailureRepoNotFound ConfirmationFailure = "repo_not_found"
)

// ConfirmationError is a confirmer failure carrying its failure code.
type ConfirmationError struct {
	Failure ConfirmationFailure
	Err     error
}

func (e *ConfirmationError) Error() string {
	if e.Err == nil {
		return "account status confirmation failed: " + string(e.Failure)
	}
	return e.Err.Error()
}

func (e *ConfirmationError) Unwrap() error { return e.Err }

// FailureOf returns err's confirmation failure code, FailureUnclassified when
// err carries no *ConfirmationError.
func FailureOf(err error) ConfirmationFailure {
	var confirmationError *ConfirmationError
	if errors.As(err, &confirmationError) {
		return confirmationError.Failure
	}
	return FailureUnclassified
}

const (
	// RecheckBaseDelay is the wait before the first recheck attempt.
	RecheckBaseDelay = time.Minute
	// RecheckMaxDelay caps the doubling wait between recheck attempts.
	RecheckMaxDelay = time.Hour
)

// RecheckDelay is the wait before recheck attempt number attempts:
// RecheckBaseDelay doubled per attempt after the first, capped at
// RecheckMaxDelay. Attempts below one count as one.
func RecheckDelay(attempts int) time.Duration {
	delay := RecheckBaseDelay
	for attempt := 1; attempt < attempts; attempt++ {
		delay *= 2
		if delay >= RecheckMaxDelay {
			return RecheckMaxDelay
		}
	}
	return delay
}

// RecheckAction is what Reconcile applies to an account's recheck row.
type RecheckAction int

const (
	// RecheckKeep leaves the recheck row as it is.
	RecheckKeep RecheckAction = iota
	// RecheckSchedule writes the recheck row from RecheckChange's fields.
	RecheckSchedule
	// RecheckClear deletes the recheck row.
	RecheckClear
)

// RecheckChange is decide's verdict on the recheck row; the other fields are
// meaningful only for RecheckSchedule.
type RecheckChange struct {
	Action        RecheckAction
	Attempts      int
	NextAttemptAt time.Time
	LastErrorCode ConfirmationFailure
}

// PendingRecheck is a stored recheck row.
type PendingRecheck struct {
	Attempts      int
	NextAttemptAt time.Time
	LastErrorCode ConfirmationFailure
}

// State is an account's stored restriction and recheck row; Recheck is nil
// when there is no recheck row. Present reports Coves presence for the DID: a
// users or communities row, or an authored post or comment.
type State struct {
	Status  Status
	Found   bool
	Present bool
	Recheck *PendingRecheck
}

// Store reads and reconciles stored restrictions.
type Store interface {
	Reader
	// Reconcile locks did, reads its restriction row and recheck row, calls
	// decide, applies the returned change (restriction and recheck) in one
	// transaction and commits; an error from decide changes nothing.
	//
	// decide, and the PDS confirmation inside it, deliberately runs while the
	// per-DID lock is held, so one DID's read-confirm-apply is serialized
	// across the users lanes and the recheck worker. The bound on held
	// connections is the number of callers: one users-lane connector per
	// Jetstream feed, the redrive lane, and the recheck worker, each serial.
	// Each confirmation is bounded by the confirmer's own timeout
	// (jetstream's repoStatusTimeout, 5s); waiting for the lock itself is not.
	Reconcile(ctx context.Context, did string, decide func(ctx context.Context, current State) (Change, error)) error
	// DueRechecks lists at most limit DIDs whose recheck row has
	// next_attempt_at <= now, each once. max(1, limit/10) slots (none for
	// limit 0) are reserved for the oldest due rows by next_attempt_at, then
	// DID, whatever their attempts, and listed first, so a sustained inflow of
	// fresh rows cannot starve a long-failing DID. The remaining slots go to
	// the other due rows fewest attempts first, then oldest next_attempt_at,
	// then DID, so a long-failing DID cannot starve newer ones either.
	DueRechecks(ctx context.Context, now time.Time, limit int) ([]string, error)
}
