package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/posts"
)

// CommunityRecordDesired is what an instance decision wants a hosted
// community's records to say about a subject.
type CommunityRecordDesired string

const (
	// CommunityRecordRemoved: the community's removal record for the subject
	// stands.
	CommunityRecordRemoved CommunityRecordDesired = "removed"
	// CommunityRecordRestored: the removal record the instance wrote is gone,
	// and an acceptance it withdrew is back.
	CommunityRecordRestored CommunityRecordDesired = "restored"
)

// CommunityRecordSubject identifies one community record write: one subject in
// one hosted community.
type CommunityRecordSubject struct {
	CommunityDID string
	SubjectURI   string
}

// CommunityRecordWrite is one outbox row: the community records an instance
// removal or restore is owed in a community this AppView hosts.
type CommunityRecordWrite struct {
	CommunityDID      string
	SubjectURI        string
	SubjectCollection string
	// SubjectCID is the CID the admin reviewed at removal: the record's strongRef.
	SubjectCID string
	// RemovalActionID is the instance removal action that owns the row.
	RemovalActionID string
	// Code is the public removal code the record carries.
	Code posts.DecisionCode
	// RemovalCreatedAt is the owning action's time, the record's createdAt.
	RemovalCreatedAt time.Time
	Desired          CommunityRecordDesired
	// RestoreAcceptanceCID is the content CID a restore's acceptance pins:
	// the post's indexed CID at restore, empty for a comment.
	RestoreAcceptanceCID string
	// OwnedRemovalCID is the record CID of the removal the instance wrote or
	// adopted, empty when none is known.
	OwnedRemovalCID string
	// AcceptanceSeen records that an acceptance stood when a removal write
	// began.
	AcceptanceSeen bool
	// WithdrewAcceptance records that the instance's removal withdrew an
	// acceptance, so its undo writes one back.
	WithdrewAcceptance bool
	// LastWriteStartedAt is when the last removal write began, nil before any.
	LastWriteStartedAt *time.Time
	// LastWriteSubjectCID, LastWriteCode and LastWriteRecordCreatedAt are the
	// content of the removal record the last write sent. A takeover by a new
	// action keeps them, so a record that write left with its response lost is
	// still recognised as the instance's.
	LastWriteSubjectCID      string
	LastWriteCode            posts.DecisionCode
	LastWriteRecordCreatedAt string
	// RestoreStampRev is the rev of an undo that committed before its
	// admission stamp was applied; empty when no stamp is owed. The
	// RestoreStamp fields beside it are what that stamp writes: an acceptance
	// pinning RestoreStampPinnedCID, or, when it is empty, the removal's
	// delete.
	RestoreStampRev            string
	RestoreStampAcceptanceURI  string
	RestoreStampAcceptanceRkey string
	RestoreStampPinnedCID      string
	Completed                  bool
	Attempts                   int
	NextAttemptAt              time.Time
	LastFailureCode            string
	// Generation changes with every desired change; an attempt completes the
	// row only if it is unchanged.
	Generation int64
	// Claim identifies the attempt holding the row's lease.
	Claim int64
}

// CommunityRecordWrites receives the rows a committed transaction recorded.
type CommunityRecordWrites interface {
	WriteAfterCommit(ctx context.Context, subject CommunityRecordSubject)
}

// CommunityRecordWriteClaim selects due rows for one attempt.
type CommunityRecordWriteClaim struct {
	// DueAt is the cutoff: only rows whose next attempt is at or before it are
	// claimed.
	DueAt time.Time
	Lease time.Duration
	Limit int
	// Subject, when set, restricts the claim to that one row.
	Subject  *CommunityRecordSubject
	excluded []CommunityRecordSubject
}

// ExcludedSubjects lists rows already attempted by this sweep, which the store
// must not claim again in the same pass.
func (claim CommunityRecordWriteClaim) ExcludedSubjects() []CommunityRecordSubject {
	return claim.excluded
}

// CommunityRecordWriteStore persists the outbox rows the queue works.
type CommunityRecordWriteStore interface {
	ClaimDueCommunityRecordWrites(ctx context.Context, claim CommunityRecordWriteClaim) ([]CommunityRecordWrite, error)
	// SaveCommunityRecordWriteProgress persists the attempt progress fields
	// (OwnedRemovalCID, AcceptanceSeen, WithdrewAcceptance, the LastWrite
	// fields and the RestoreStamp fields) when the row's claim is still
	// write.Claim. Every method below reports ErrCommunityRecordClaimLost,
	// persisting nothing, when it is not.
	SaveCommunityRecordWriteProgress(ctx context.Context, write CommunityRecordWrite) error
	// CompleteCommunityRecordWrite persists the progress fields under the
	// claim, then completes the row only if its generation is still
	// write.Generation; otherwise the row stays pending.
	CompleteCommunityRecordWrite(ctx context.Context, write CommunityRecordWrite) error
	// RescheduleCommunityRecordWrite persists the progress fields under the
	// claim. If the generation is still write.Generation it makes the row due
	// at nextAttemptAt, and a non-empty failureCode counts a failed attempt;
	// otherwise the new generation's schedule and failure count are left
	// alone.
	RescheduleCommunityRecordWrite(ctx context.Context, write CommunityRecordWrite, nextAttemptAt time.Time, failureCode string) error
}

// CommunityRecordAdmissions stamps a post's admission row after the queue's
// own commits, with the commit rev as the watermark.
type CommunityRecordAdmissions interface {
	ApplyRemoval(ctx context.Context, cmd posts.ApplyRemovalCommand) (posts.AdmissionResult, error)
	ApplyAcceptance(ctx context.Context, cmd posts.ApplyAcceptanceCommand) (posts.AdmissionResult, error)
	ApplyRemovalDelete(ctx context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error)
}

// communityRecordCodes maps an instance removal reason onto the public code the
// community's removal record carries. Doxing and illegal content are not named
// in public: the record is signed by the community and replicated by everyone
// who reads its repo, and the code alone would tell them what to look for.
var communityRecordCodes = map[string]posts.DecisionCode{
	"social.coves.moderation.defs#reasonSpam":          posts.DecisionCode("spam"),
	"social.coves.moderation.defs#reasonRuleViolation": posts.DecisionCode("rule-violation"),
	"social.coves.moderation.defs#reasonHarassment":    posts.DecisionCode("rule-violation"),
	doxingReason:         posts.DecisionCode("moderator-discretion"),
	illegalContentReason: posts.DecisionCode("moderator-discretion"),
	"social.coves.moderation.defs#reasonModeratorDiscretion": posts.DecisionCode("moderator-discretion"),
}

// communityRecordCode is the public code for an instance removal reason. A
// reason this map does not know is published as moderator discretion rather
// than verbatim.
func communityRecordCode(reason string) posts.DecisionCode {
	if code, ok := communityRecordCodes[reason]; ok {
		return code
	}
	return posts.DecisionCode("moderator-discretion")
}

// requireCommunityRecordRemoval records, in the removal's transaction, that a
// subject in a community this AppView hosts is owed the community's own removal
// record. It returns the subject for the post-commit hook, or nil when no row
// was recorded: the subject names no community or no CID, or the community is
// hosted elsewhere and gets the label only.
func requireCommunityRecordRemoval(ctx context.Context, tx Transaction, subject *indexedSubject, action *Action) (*CommunityRecordSubject, error) {
	if subject.CommunityDID == "" || action.ObservedCID == "" {
		return nil, nil
	}
	hosted, err := tx.IsHostedCommunity(ctx, subject.CommunityDID)
	if err != nil {
		return nil, err
	}
	if !hosted {
		return nil, nil
	}
	if err := tx.RequireCommunityRecordRemoval(ctx, CommunityRecordWrite{
		CommunityDID:      subject.CommunityDID,
		SubjectURI:        action.SubjectURI,
		SubjectCollection: subject.Collection,
		SubjectCID:        action.ObservedCID,
		RemovalActionID:   action.ID,
		Code:              communityRecordCode(action.Reason),
		RemovalCreatedAt:  action.CreatedAt,
		Desired:           CommunityRecordRemoved,
	}); err != nil {
		return nil, err
	}
	return &CommunityRecordSubject{CommunityDID: subject.CommunityDID, SubjectURI: action.SubjectURI}, nil
}

// requestCommunityRecordRestore records, in the restore's transaction, that the
// community record the reversed removal owns is to be undone. A post present at
// restore pins the CID indexed now for the acceptance the undo writes back if
// the removal withdrew one; a comment, or a post its author deleted, pins none
// and gets the removal deleted only. It returns nil when the removal names no
// community, or when the store changed no row: the removal recorded none, or a
// later removal owns the row.
func requestCommunityRecordRestore(ctx context.Context, tx Transaction, removal *Action, current *StrongRef) (*CommunityRecordSubject, error) {
	if removal.SubjectCommunityDID == "" {
		return nil, nil
	}
	acceptanceCID := ""
	if current != nil && removal.SubjectCollection != CommentCollection {
		acceptanceCID = current.CID
	}
	changed, err := tx.RequestCommunityRecordRestore(ctx, removal.SubjectCommunityDID, removal.SubjectURI, removal.ID, acceptanceCID)
	if err != nil || !changed {
		return nil, err
	}
	return &CommunityRecordSubject{CommunityDID: removal.SubjectCommunityDID, SubjectURI: removal.SubjectURI}, nil
}

const (
	// communityRecordLease is how long a claim holds a row before another
	// worker may claim it, measured from when the row is claimed. It is also
	// how long after a removal write began that write may still land.
	communityRecordLease = 2 * time.Minute
	// communityRecordAttemptTimeout is the default bound on one row's
	// reconcile. With communityRecordOutcomeWriteTimeout it stays below
	// communityRecordLease, so the outcome is written before the lease can
	// pass to another worker. A configured AttemptTimeout must keep that
	// margin too.
	communityRecordAttemptTimeout = 90 * time.Second
	// communityRecordOutcomeWriteTimeout bounds writing an attempt's outcome,
	// which runs after the attempt's own context may have ended.
	communityRecordOutcomeWriteTimeout = 5 * time.Second
	// communityRecordImmediateAttemptLimit caps immediate attempts in flight.
	// Beyond it, rows wait for the next sweep.
	communityRecordImmediateAttemptLimit = 4

	communityRecordFailurePDS      = "pds_write_failed"
	communityRecordFailureTimedOut = "pds_timed_out"
	communityRecordFailureStamp    = "admission_stamp_failed"
	communityRecordFailureStore    = "progress_save_failed"
)

// ErrCommunityRecordClaimLost reports that a row's claim passed to another
// attempt, so nothing was persisted.
var ErrCommunityRecordClaimLost = errors.New("community record write claim lost")

// CommunityRecordWriteQueueConfig configures a CommunityRecordWriteQueue.
type CommunityRecordWriteQueueConfig struct {
	Now func() time.Time
	// AttemptTimeout bounds one row's reconcile; zero means
	// communityRecordAttemptTimeout.
	AttemptTimeout time.Duration
}

// CommunityRecordWriteQueue reconciles outbox rows after commit and on sweeps.
//
// Each claimed row is reconciled against what the community's repo holds now.
// The queue owns a removal record when its CID is the one the row recorded, or
// when its content is exactly what the queue writes for the owning action (a
// write whose response was lost). It never rewrites or undoes any other
// removal: that is the community's own moderation.
type CommunityRecordWriteQueue struct {
	store      CommunityRecordWriteStore
	writer     posts.InstanceRemovalWriter
	admissions CommunityRecordAdmissions
	config     CommunityRecordWriteQueueConfig
	slots      chan struct{}
	mu         sync.Mutex
	closed     bool
	inFlight   int
	idle       chan struct{}
}

// NewCommunityRecordWriteQueue builds the community record write queue.
func NewCommunityRecordWriteQueue(store CommunityRecordWriteStore, writer posts.InstanceRemovalWriter, admissions CommunityRecordAdmissions, config CommunityRecordWriteQueueConfig) *CommunityRecordWriteQueue {
	if config.Now == nil {
		config.Now = time.Now
	}
	idle := make(chan struct{})
	close(idle)
	return &CommunityRecordWriteQueue{
		store: store, writer: writer, admissions: admissions, config: config, idle: idle,
		slots: make(chan struct{}, communityRecordImmediateAttemptLimit),
	}
}

// WriteAfterCommit schedules one bounded, cancellation-independent attempt for
// the subject's row. After Close, or while communityRecordImmediateAttemptLimit
// attempts are in flight, it returns without attempting: the row is durable and
// the next sweep reconciles it.
func (q *CommunityRecordWriteQueue) WriteAfterCommit(ctx context.Context, subject CommunityRecordSubject) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	select {
	case q.slots <- struct{}{}:
	default:
		q.mu.Unlock()
		slog.Info("community record write immediate attempts saturated; the sweep will write this row",
			"community", subject.CommunityDID, "subject", subject.SubjectURI)
		return
	}
	if q.inFlight == 0 {
		q.idle = make(chan struct{})
	}
	q.inFlight++
	q.mu.Unlock()
	go func() {
		defer func() {
			<-q.slots
			q.mu.Lock()
			q.inFlight--
			if q.inFlight == 0 {
				close(q.idle)
			}
			q.mu.Unlock()
		}()
		// The row is durable, so a panic loses only this attempt: the sweep
		// writes the row once its lease ends.
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("community record write immediate attempt panicked; the sweep will write this row",
					"community", subject.CommunityDID, "subject", subject.SubjectURI, "panic", recovered)
			}
		}()
		attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), q.attemptTimeout())
		defer cancel()
		claim := q.claim(q.config.Now())
		claim.Subject = &subject
		claim.Limit = 1
		writes, err := q.store.ClaimDueCommunityRecordWrites(attemptCtx, claim)
		if err != nil {
			slog.Error("community record write claim failed",
				"community", subject.CommunityDID, "subject", subject.SubjectURI, "error", err)
			return
		}
		for _, write := range writes {
			if err := q.attempt(attemptCtx, write); err != nil {
				slog.Error("community record write outcome update failed",
					"community", write.CommunityDID, "subject", write.SubjectURI, "error", err)
			}
		}
	}()
}

func (q *CommunityRecordWriteQueue) claim(dueAt time.Time) CommunityRecordWriteClaim {
	return CommunityRecordWriteClaim{DueAt: dueAt, Lease: communityRecordLease, Limit: 1}
}

func (q *CommunityRecordWriteQueue) attemptTimeout() time.Duration {
	if q.config.AttemptTimeout > 0 {
		return q.config.AttemptTimeout
	}
	return communityRecordAttemptTimeout
}

// Sweep attempts the rows due at the start of this pass. Freezing the due
// cutoff, and excluding rows this pass already attempted, keeps a row
// re-pended during the pass from being retried by the same sweep. Each row is
// claimed just before its attempt, so every attempt starts on a full lease.
//
// Each attempt runs on a context detached from ctx, bounded by the attempt
// timeout alone, so ending the sweep never cuts a PDS write in flight. Once
// ctx is done the sweep claims nothing more and releases a row claimed but not
// yet attempted. A failed outcome write is logged and the pass goes on; the
// failures are returned joined, or ctx.Err() when there are none.
func (q *CommunityRecordWriteQueue) Sweep(ctx context.Context) error {
	dueAt := q.config.Now()
	var attempted []CommunityRecordSubject
	var failures []error
	for ctx.Err() == nil {
		claim := q.claim(dueAt)
		claim.excluded = attempted
		writes, err := q.store.ClaimDueCommunityRecordWrites(ctx, claim)
		if err != nil {
			if ctx.Err() == nil {
				failures = append(failures, err)
			}
			break
		}
		if len(writes) == 0 {
			break
		}
		if ctx.Err() != nil {
			q.release(ctx, writes)
			break
		}
		for _, write := range writes {
			attempted = append(attempted, CommunityRecordSubject{CommunityDID: write.CommunityDID, SubjectURI: write.SubjectURI})
			attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), q.attemptTimeout())
			err := q.attempt(attemptCtx, write)
			cancel()
			if err != nil {
				slog.Error("community record write outcome update failed",
					"community", write.CommunityDID, "subject", write.SubjectURI, "error", err)
				failures = append(failures, err)
			}
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return ctx.Err()
}

// release hands claimed rows that were not attempted back, due when they
// already were and with nothing counted, so the next sweep need not wait out
// their leases.
func (q *CommunityRecordWriteQueue) release(ctx context.Context, writes []CommunityRecordWrite) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), communityRecordOutcomeWriteTimeout)
	defer cancel()
	for _, write := range writes {
		if err := q.store.RescheduleCommunityRecordWrite(releaseCtx, write, write.NextAttemptAt, ""); err != nil &&
			!errors.Is(err, ErrCommunityRecordClaimLost) {
			slog.Warn("community record write release failed; the row waits out its lease",
				"community", write.CommunityDID, "subject", write.SubjectURI, "error", err)
		}
	}
}

// Close stops new immediate attempts. Rows recorded afterwards stay pending for
// the sweep. Attempts already in flight continue; Wait observes them.
func (q *CommunityRecordWriteQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

// Wait observes all immediate attempts in flight when called.
func (q *CommunityRecordWriteQueue) Wait(ctx context.Context) error {
	q.mu.Lock()
	idle := q.idle
	q.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
		return nil
	}
}

// communityRecordOutcome is how a reconcile ends: completed, or due again at
// nextAttemptAt. A non-empty failureCode counts a failed attempt.
type communityRecordOutcome struct {
	complete      bool
	nextAttemptAt time.Time
	failureCode   string
}

// attempt reconciles one claimed row and writes its outcome, with what the
// attempt learned about the repo, under the row's claim.
func (q *CommunityRecordWriteQueue) attempt(ctx context.Context, write CommunityRecordWrite) error {
	attemptedAt := q.config.Now()
	outcome, err := q.reconcile(ctx, &write, attemptedAt)
	switch {
	case err == nil:
	case errors.Is(err, ErrCommunityRecordClaimLost):
		// Another worker holds the row now and nothing more was sent; the
		// outcome is that worker's to write.
		slog.Warn("community record write claim lost; the current claim owns the row",
			"community", write.CommunityDID, "subject", write.SubjectURI, "error", err)
		return nil
	case errors.Is(err, posts.ErrCommunityNotHosted):
		// This AppView no longer holds the community's credentials, so no
		// retry can write; the label still stands. A restore leaves any
		// removal record the instance wrote standing in the community's repo.
		if write.Desired == CommunityRecordRestored && write.OwnedRemovalCID != "" {
			slog.Warn("community record restore skipped: the community is not hosted by this AppView; the instance's removal record stays",
				"community", write.CommunityDID, "subject", write.SubjectURI, "removal_cid", write.OwnedRemovalCID)
		} else {
			slog.Info("community record write skipped: the community is not hosted by this AppView",
				"community", write.CommunityDID, "subject", write.SubjectURI)
		}
		outcome = communityRecordOutcome{complete: true}
	case ctx.Err() != nil:
		// The attempt's own deadline: the PDS did not answer in time. It
		// counts and backs off like any failure.
		outcome = communityRecordOutcome{
			failureCode:   communityRecordFailureTimedOut,
			nextAttemptAt: attemptedAt.Add(communityRecordRetryDelay(write.Attempts + 1)),
		}
		slog.Error("community record write timed out",
			"community", write.CommunityDID, "subject", write.SubjectURI, "desired", write.Desired,
			"attempts", write.Attempts+1, "code", outcome.failureCode, "error", err)
	default:
		outcome.nextAttemptAt = attemptedAt.Add(communityRecordRetryDelay(write.Attempts + 1))
		slog.Error("community record write failed",
			"community", write.CommunityDID, "subject", write.SubjectURI, "desired", write.Desired,
			"attempts", write.Attempts+1, "code", outcome.failureCode, "error", err)
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), communityRecordOutcomeWriteTimeout)
	defer cancel()
	if outcome.complete {
		err = q.store.CompleteCommunityRecordWrite(writeCtx, write)
	} else {
		err = q.store.RescheduleCommunityRecordWrite(writeCtx, write, outcome.nextAttemptAt, outcome.failureCode)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCommunityRecordClaimLost):
		slog.Warn("community record write claim lost before its outcome was written; the current claim owns the row",
			"community", write.CommunityDID, "subject", write.SubjectURI, "complete", outcome.complete)
		return nil
	case outcome.complete:
		return fmt.Errorf("complete community record write community=%s subject=%s: %w", write.CommunityDID, write.SubjectURI, err)
	default:
		return fmt.Errorf("reschedule community record write community=%s subject=%s: %w", write.CommunityDID, write.SubjectURI, err)
	}
}

// communityRecordRetryDelay is the backoff after a failed attempt: the CDN
// purge queue's schedule, from one minute doubling to one hour.
func communityRecordRetryDelay(failedAttempts int) time.Duration {
	return cdnPurgeRetryDelay(failedAttempts)
}

// reconcile makes the repo say what the row desires, recording on write what
// it wrote. An error carries its failure code in the returned outcome.
func (q *CommunityRecordWriteQueue) reconcile(ctx context.Context, write *CommunityRecordWrite, now time.Time) (communityRecordOutcome, error) {
	failed := func(code string, err error) (communityRecordOutcome, error) {
		return communityRecordOutcome{failureCode: code}, err
	}
	if write.RestoreStampRev != "" {
		// An earlier undo committed but its stamp failed. It is applied before
		// anything else, whatever the row desires now: a later removal's stamp
		// carries a later rev, so applying it first keeps the admission row
		// converging on the repo's history.
		if err := q.applyRestoreStamp(ctx, *write); err != nil {
			return failed(communityRecordFailureStamp, fmt.Errorf("re-stamping the restore: %w", err))
		}
		write.clearRestoreStamp()
	}
	standing, err := q.writer.StandingRemoval(ctx, write.CommunityDID, write.SubjectURI)
	if err != nil {
		return failed(communityRecordFailurePDS, err)
	}
	if write.Desired == CommunityRecordRestored {
		return q.reconcileRestore(ctx, write, standing, now)
	}
	return q.reconcileRemoval(ctx, write, standing, now)
}

// reconcileRemoval makes a removal stand for the subject. A removal standing
// that is not the queue's own already removes the subject and is left alone.
func (q *CommunityRecordWriteQueue) reconcileRemoval(ctx context.Context, write *CommunityRecordWrite, standing *posts.StandingRemoval, now time.Time) (communityRecordOutcome, error) {
	failed := func(code string, err error) (communityRecordOutcome, error) {
		return communityRecordOutcome{failureCode: code}, err
	}
	if standing != nil && !write.owns(standing) {
		return communityRecordOutcome{complete: true}, nil
	}
	if standing != nil {
		// Pinned by CID before this attempt's write replaces the LastWrite
		// fingerprint that recognised it.
		write.adopt(standing)
	}
	isPost := write.SubjectCollection != CommentCollection
	if standing != nil && !isPost {
		return communityRecordOutcome{complete: true}, nil
	}

	if standing == nil {
		// Read before the write and persisted under the claim, so a write whose
		// response is lost still leaves evidence that it withdrew an acceptance.
		acceptance, err := q.writer.StandingAcceptance(ctx, write.CommunityDID, write.SubjectURI)
		if err != nil {
			return failed(communityRecordFailurePDS, err)
		}
		write.AcceptanceSeen = acceptance != nil
	}
	// A post whose own removal already stands still goes through the writer:
	// its skip reports the head rev, which re-applies an admission stamp an
	// earlier attempt failed to write.
	startedAt := now
	write.LastWriteStartedAt = &startedAt
	write.LastWriteSubjectCID = write.SubjectCID
	write.LastWriteCode = write.Code
	write.LastWriteRecordCreatedAt = write.recordCreatedAt()
	if err := q.store.SaveCommunityRecordWriteProgress(ctx, *write); err != nil {
		return failed(communityRecordFailureStore, fmt.Errorf("saving progress before the removal write: %w", err))
	}

	result, err := q.writer.WriteInstanceRemoval(ctx, posts.InstanceRemovalCommand{
		CommunityDID: write.CommunityDID,
		SubjectURI:   write.SubjectURI,
		SubjectCID:   write.SubjectCID,
		Code:         write.Code,
		CreatedAt:    write.recordCreatedAt(),
		// An acceptance that landed after the pre-read is deleted by this
		// batch; the evidence is persisted before a response that may be lost.
		BeforeCommit: func(ctx context.Context, deletesAcceptance bool) error {
			if !deletesAcceptance || write.AcceptanceSeen {
				return nil
			}
			write.AcceptanceSeen = true
			return q.store.SaveCommunityRecordWriteProgress(ctx, *write)
		},
	})
	if err != nil {
		return failed(communityRecordFailurePDS, err)
	}
	switch {
	case result.Standing == nil:
		write.OwnedRemovalCID = result.CID
		write.WithdrewAcceptance = write.AcceptanceSeen || result.WithdrewAcceptance
	case write.owns(result.Standing):
		write.adopt(result.Standing)
	default:
		// A removal landed between the pre-read and the write.
		return communityRecordOutcome{complete: true}, nil
	}

	if isPost {
		if _, err := q.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
			CommunityDID: write.CommunityDID,
			PostURI:      write.SubjectURI,
			DecisionCode: string(write.Code),
			Watermark:    posts.CommunityWatermark{Rev: result.Rev},
		}); err != nil {
			return failed(communityRecordFailureStamp, fmt.Errorf("stamping the removal: %w", err))
		}
	}
	return communityRecordOutcome{complete: true}, nil
}

// reconcileRestore undoes the queue's own removal, writing back an acceptance
// only if that removal withdrew one. Anything else standing is left alone.
func (q *CommunityRecordWriteQueue) reconcileRestore(ctx context.Context, write *CommunityRecordWrite, standing *posts.StandingRemoval, now time.Time) (communityRecordOutcome, error) {
	failed := func(code string, err error) (communityRecordOutcome, error) {
		return communityRecordOutcome{failureCode: code}, err
	}
	if standing == nil || !write.owns(standing) {
		// A removal write that timed out may still commit until its lease
		// ends; completing now would leave that removal standing for good.
		if standing == nil && write.LastWriteStartedAt != nil {
			if mayLandUntil := write.LastWriteStartedAt.Add(communityRecordLease); now.Before(mayLandUntil) {
				return communityRecordOutcome{nextAttemptAt: mayLandUntil}, nil
			}
		}
		write.clearOwnership()
		return communityRecordOutcome{complete: true}, nil
	}

	// AcceptanceSeen counts for a removal adopted only now: the write that
	// left it deleted the acceptance read just before it.
	acceptanceCID := ""
	if write.WithdrewAcceptance || write.AcceptanceSeen {
		acceptanceCID = write.RestoreAcceptanceCID
	}
	result, err := q.writer.UndoInstanceRemoval(ctx, posts.InstanceRemovalUndoCommand{
		CommunityDID:  write.CommunityDID,
		SubjectURI:    write.SubjectURI,
		RemovalCID:    standing.CID,
		AcceptanceCID: acceptanceCID,
	})
	if err != nil {
		return failed(communityRecordFailurePDS, err)
	}
	// Committed, or skipped because the removal changed after the pre-read:
	// either way the instance owns no removal record any more.
	write.clearOwnership()
	if result.Skipped || write.SubjectCollection == CommentCollection {
		return communityRecordOutcome{complete: true}, nil
	}

	// The stamp owed is persisted under the claim before it is applied, so a
	// failed stamp is re-applied by the retry, which finds no removal left.
	write.RestoreStampRev = result.Rev
	write.RestoreStampPinnedCID = acceptanceCID
	if acceptanceCID != "" {
		write.RestoreStampAcceptanceURI = result.URI
		write.RestoreStampAcceptanceRkey = result.RKey
	}
	if err := q.store.SaveCommunityRecordWriteProgress(ctx, *write); err != nil {
		return failed(communityRecordFailureStore, fmt.Errorf("saving the restore stamp owed: %w", err))
	}
	if err := q.applyRestoreStamp(ctx, *write); err != nil {
		return failed(communityRecordFailureStamp, fmt.Errorf("stamping the restore: %w", err))
	}
	write.clearRestoreStamp()
	return communityRecordOutcome{complete: true}, nil
}

// applyRestoreStamp stamps the post's admission row with the restore stamp
// the row records.
func (q *CommunityRecordWriteQueue) applyRestoreStamp(ctx context.Context, write CommunityRecordWrite) error {
	watermark := posts.CommunityWatermark{Rev: write.RestoreStampRev}
	if write.RestoreStampPinnedCID != "" {
		_, err := q.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
			CommunityDID:   write.CommunityDID,
			PostURI:        write.SubjectURI,
			AcceptanceURI:  write.RestoreStampAcceptanceURI,
			AcceptanceRkey: write.RestoreStampAcceptanceRkey,
			PinnedCID:      write.RestoreStampPinnedCID,
			Watermark:      watermark,
		})
		return err
	}
	_, err := q.admissions.ApplyRemovalDelete(ctx, posts.CommunityDeleteCommand{
		CommunityDID: write.CommunityDID,
		PostURI:      write.SubjectURI,
		Watermark:    watermark,
	})
	return err
}

// recordCreatedAt is the createdAt the queue's removal record carries.
func (write CommunityRecordWrite) recordCreatedAt() string {
	return write.RemovalCreatedAt.UTC().Format(time.RFC3339)
}

// owns reports whether a standing removal is the instance's: the one the row
// recorded writing, or, once a write of the queue's has begun, one whose
// content is exactly what the queue's last write sent. A row with no recorded
// write content compares the owning action's.
func (write CommunityRecordWrite) owns(standing *posts.StandingRemoval) bool {
	if write.OwnedRemovalCID != "" && standing.CID == write.OwnedRemovalCID {
		return true
	}
	if write.LastWriteStartedAt == nil {
		return false
	}
	subjectCID, code, createdAt := write.LastWriteSubjectCID, write.LastWriteCode, write.LastWriteRecordCreatedAt
	if code == "" {
		subjectCID, code, createdAt = write.SubjectCID, write.Code, write.recordCreatedAt()
	}
	return standing.SubjectURI == write.SubjectURI &&
		standing.SubjectCID == subjectCID &&
		standing.Code == string(code) &&
		standing.Reason == "" &&
		standing.CreatedAt == createdAt
}

// adopt records a standing removal as the instance's own. The acceptance seen
// before the write that left it is the acceptance that write withdrew.
func (write *CommunityRecordWrite) adopt(standing *posts.StandingRemoval) {
	write.OwnedRemovalCID = standing.CID
	write.WithdrewAcceptance = write.WithdrewAcceptance || write.AcceptanceSeen
}

// clearOwnership records that the instance owns no removal record.
func (write *CommunityRecordWrite) clearOwnership() {
	write.OwnedRemovalCID = ""
	write.AcceptanceSeen = false
	write.WithdrewAcceptance = false
}

// clearRestoreStamp records that no restore stamp is owed.
func (write *CommunityRecordWrite) clearRestoreStamp() {
	write.RestoreStampRev = ""
	write.RestoreStampAcceptanceURI = ""
	write.RestoreStampAcceptanceRkey = ""
	write.RestoreStampPinnedCID = ""
}
