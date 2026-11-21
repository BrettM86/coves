package moderation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Fingerprint operations of the community roster actions. They differ from
// every instance mutation operation.
const (
	resignRoleOperation      = "resign-role"
	removeModeratorOperation = "remove-moderator"
)

// expiredIntentSweepLimit bounds the intents one SweepExpiredIntents call
// settles, and is its page size.
const expiredIntentSweepLimit = 100

// CommunityActionConfig wires the community action service.
type CommunityActionConfig struct {
	Authorizer     communities.RoleAuthorizer // community role authority
	Store          CommunityActionStore
	Repos          posts.CommunityRepoFactory // community credentials
	Now            func() time.Time
	IntentTTL      time.Duration // > 0; longer than any PDS write timeout
	IdempotencyTTL time.Duration // > 0; the instance moderation key retention
	Hooks          CommunityActionHooks
	// Posts resolves a post's community for the post actions, which are
	// unavailable without it.
	Posts CommunityPostResolver
	// Comments resolves a comment's community for the comment actions, which
	// are unavailable without it.
	Comments CommunityCommentResolver
	// RemovalLimits bounds non-admin post removals.
	RemovalLimits CommunityRemovalLimits
	// BanLimits bounds non-admin community bans, a budget separate from
	// RemovalLimits.
	BanLimits CommunityRemovalLimits
	// Bans lists a community's indexed bans for ListCommunityBans.
	Bans CommunityBanLister
	// ExpiredIntentSweepLimit is how many intents one SweepExpiredIntents
	// call settles, and its page size; zero is expiredIntentSweepLimit.
	ExpiredIntentSweepLimit int
}

// CommunityActionHooks are optional test seams; the zero value is a no-op.
type CommunityActionHooks struct {
	// AfterAuthorize runs after the first authorization, before any lock.
	AfterAuthorize func(ctx context.Context)
	// BeforeWrite runs after the intent is stored, before the PDS write, with
	// the action's locks held.
	BeforeWrite func(ctx context.Context)
}

// CommunityActionService runs community roster actions through the community
// write pipeline: authorize, lock, re-authorize, idempotency and version,
// read the community repo, store the intent, write, then apply the local
// effect in the action transaction.
type CommunityActionService struct {
	cfg CommunityActionConfig
	// writer writes the post actions' stand records into community repos.
	writer posts.CommunityRecordWriter
	// removals writes the comment actions' removal records, which have no
	// paired acceptance.
	removals posts.CommunityRemovalRecordWriter
	// attempts records each post and comment batch's attempt; nil when the
	// store does not record attempts.
	attempts CommunityIntentAttempts
}

// NewCommunityActionService builds the service. Authorizer, Store, Repos and
// Now are required and both TTLs must be positive.
func NewCommunityActionService(cfg CommunityActionConfig) *CommunityActionService {
	if cfg.Authorizer == nil || cfg.Store == nil || cfg.Repos == nil || cfg.Now == nil {
		panic("moderation.NewCommunityActionService: Authorizer, Store, Repos and Now are required")
	}
	if cfg.IntentTTL <= 0 || cfg.IdempotencyTTL <= 0 {
		panic("moderation.NewCommunityActionService: IntentTTL and IdempotencyTTL must be positive")
	}
	if cfg.ExpiredIntentSweepLimit <= 0 {
		cfg.ExpiredIntentSweepLimit = expiredIntentSweepLimit
	}
	attempts, _ := cfg.Store.(CommunityIntentAttempts)
	return &CommunityActionService{
		cfg:      cfg,
		writer:   posts.NewCommunityRecordWriter(cfg.Repos, cfg.Now),
		removals: posts.NewCommunityRemovalRecordWriter(cfg.Repos, cfg.Now),
		attempts: attempts,
	}
}

// ResignRoleRequest asks to resign the caller's own moderator role.
type ResignRoleRequest struct {
	ActorDID, CommunityDID string
	ExpectedVersion        int64
	IdempotencyKey         string
}

// RemoveModeratorRequest asks to remove a moderator's role.
type RemoveModeratorRequest struct {
	ActorDID, CommunityDID, SubjectDID string
	ExpectedVersion                    int64
	IdempotencyKey                     string
}

// CommunityActionResult is what a completed community action produced.
// ActionID is the moderation action's ID; Version is the roster version after
// the change.
type CommunityActionResult struct {
	CommunityDID, SubjectDID, RecordURI, ActionID string
	Version                                       int64
	// InvitationID is the invitation an invite, offer, accept or decline
	// acted on.
	InvitationID string
	// AlreadyApplied reports that the decision was already in place.
	AlreadyApplied bool
}

// rosterAction is one validated roster removal request.
type rosterAction struct {
	operation       string
	required        string // the role the actor must hold: RoleModerator (resign) or RoleOwner (remove)
	actorDID        string
	communityDID    string
	subjectDID      string
	expectedVersion int64
	key             string
	fingerprint     string
	recordKey       string
	targetURI       string
}

func (a rosterAction) scope() idempotencyScope {
	return idempotencyScope{actorDID: a.actorDID, communityDID: a.communityDID, key: a.key, fingerprint: a.fingerprint}
}

// idempotencyScope is where a request's result is stored: the requester, the
// community and the key, with the request's fingerprint.
type idempotencyScope struct {
	actorDID, communityDID, key, fingerprint string
}

// restart is an attempt's request to run the pipeline again from the locks:
// under lock, because an open intent on a target needs more locks than the
// attempt held, or after settling an open intent (settled).
type restart struct {
	lock    CommunityActionLock
	settled bool
}

// requestFingerprint is the hex SHA-256 of a request's fingerprint payload.
func requestFingerprint(operation, communityDID, subject string, expectedVersion int64) (string, error) {
	payload, err := json.Marshal([4]string{operation, communityDID, subject, strconv.FormatInt(expectedVersion, 10)})
	if err != nil {
		return "", fmt.Errorf("encoding the request fingerprint: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// ResignRole deletes the caller's own moderator record from the community
// repo. The effective owner must transfer ownership instead.
func (s *CommunityActionService) ResignRole(ctx context.Context, req ResignRoleRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	action, err := newRosterAction(resignRoleOperation, communities.RoleModerator,
		req.ActorDID, req.CommunityDID, req.ActorDID, req.ExpectedVersion, req.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return s.run(ctx, action)
}

// RemoveModerator deletes a moderator's role record from the community repo.
// The caller must be the community's owner or an instance admin; the
// effective owner cannot be removed.
func (s *CommunityActionService) RemoveModerator(ctx context.Context, req RemoveModeratorRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if _, err := syntax.ParseDID(req.SubjectDID); err != nil {
		return nil, fmt.Errorf("%w: subject must be a DID", ErrInvalidRequest)
	}
	action, err := newRosterAction(removeModeratorOperation, communities.RoleOwner,
		req.ActorDID, req.CommunityDID, req.SubjectDID, req.ExpectedVersion, req.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return s.run(ctx, action)
}

func newRosterAction(operation, required, actorDID, communityDID, subjectDID string, expectedVersion int64, key string) (rosterAction, error) {
	if _, err := syntax.ParseDID(communityDID); err != nil {
		return rosterAction{}, fmt.Errorf("%w: community must be a DID", ErrInvalidRequest)
	}
	if !validOpaqueField(key) {
		return rosterAction{}, fmt.Errorf("%w: idempotencyKey must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	if expectedVersion < 0 {
		return rosterAction{}, fmt.Errorf("%w: expectedVersion must not be negative", ErrInvalidRequest)
	}
	recordKey := communities.RoleRecordKey(subjectDID)
	fingerprint, err := requestFingerprint(operation, communityDID, subjectDID, expectedVersion)
	if err != nil {
		return rosterAction{}, err
	}
	return rosterAction{
		operation:       operation,
		required:        required,
		actorDID:        actorDID,
		communityDID:    communityDID,
		subjectDID:      subjectDID,
		expectedVersion: expectedVersion,
		key:             key,
		fingerprint:     fingerprint,
		recordKey:       recordKey,
		targetURI:       roleRecordURI(communityDID, recordKey),
	}, nil
}

// run authorizes once without locks, then attempts the action under its locks
// through runAttempts: an attempt restarts under wider locks when an open
// intent needs locks it does not hold, and once after settling an open intent.
func (s *CommunityActionService) run(ctx context.Context, action rosterAction) (*CommunityActionResult, error) {
	if _, err := s.authorize(ctx, action); err != nil {
		if errors.Is(err, ErrForbidden) {
			return s.replayRefused(ctx, action.scope(), err)
		}
		return nil, err
	}
	if s.cfg.Hooks.AfterAuthorize != nil {
		s.cfg.Hooks.AfterAuthorize(ctx)
	}
	lock := lockFor(action.communityDID, action.actorDID, action.subjectDID, action.targetURI)
	return s.runAttempts(lock, action.targetURI, func(lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
		return s.attempt(ctx, action, lock)
	})
}

// runAttempts runs attempt from lock until it does not ask for a restart.
// Restarts that widen the locks are bounded by the locks themselves: each one
// must hold strictly more than the last, and a widening that adds nothing is
// a StateConflict. One restart after settling an open intent is allowed; a
// second settled intent is a StateConflict, since other writes to targetURI
// keep opening.
func (s *CommunityActionService) runAttempts(lock CommunityActionLock, targetURI string, attempt func(CommunityActionLock) (*CommunityActionResult, *restart, error)) (*CommunityActionResult, error) {
	settled := false
	for {
		result, again, err := attempt(lock)
		if again == nil {
			return result, err
		}
		if again.settled {
			if settled {
				slog.Warn("community action refused: a second open intent needed settling after the first",
					"community", lock.CommunityDID, "record", targetURI)
				return nil, fmt.Errorf("%w: another write to %s opened while this request settled the last one", ErrStateConflict, targetURI)
			}
			settled = true
		} else if lockCovers(lock, again.lock) {
			slog.Warn("community action refused: a restart to widen the locks asked for none it did not hold",
				"community", lock.CommunityDID, "record", targetURI)
			return nil, fmt.Errorf("%w: the locks for the write to %s did not settle", ErrStateConflict, targetURI)
		}
		lock = again.lock
	}
}

// authorize maps the role authorizer's answer onto the moderation errors.
func (s *CommunityActionService) authorize(ctx context.Context, action rosterAction) (communities.RoleAuthorization, error) {
	return authorizeRole(ctx, s.cfg.Authorizer, action.communityDID, action.actorDID, action.required)
}

// authorizeRole asks the role authorizer whether actorDID holds required in
// the community and maps a refusal or a removed community to Forbidden.
func authorizeRole(ctx context.Context, authorizer communities.RoleAuthorizer, communityDID, actorDID, required string) (communities.RoleAuthorization, error) {
	auth, err := authorizer.Authorize(ctx, communityDID, actorDID, required)
	if err != nil {
		return communities.RoleAuthorization{}, authorizationError(err)
	}
	return auth, nil
}

// authorizationError maps a role authorizer error onto the moderation errors:
// a refusal or a removed community is Forbidden, anything else Unavailable.
func authorizationError(err error) error {
	if errors.Is(err, communities.ErrUnauthorized) || errors.Is(err, communities.ErrCommunityRemoved) {
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	}
	return fmt.Errorf("%w: authorizing the community action: %w", ErrModerationUnavailable, err)
}

// replayRefused serves a caller that authorization refuses its own stored
// result for the same key and fingerprint (its action may have removed its own
// authority); otherwise the refusal stands. The record is keyed by the
// caller's DID, so the read reveals nothing about the community.
func (s *CommunityActionService) replayRefused(ctx context.Context, scope idempotencyScope, refusal error) (*CommunityActionResult, error) {
	tx, err := s.cfg.Store.BeginAction(ctx, CommunityActionLock{
		CommunityDID: scope.communityDID,
		AccountDIDs:  []string{scope.actorDID},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: opening the community action transaction: %w", ErrModerationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	return replayOrRefuse(ctx, tx, scope, s.cfg.Now(), refusal)
}

func replayOrRefuse(ctx context.Context, tx CommunityActionTx, scope idempotencyScope, now time.Time, refusal error) (*CommunityActionResult, error) {
	stored, err := tx.IdempotencyRecord(ctx, scope.actorDID, scope.communityDID, scope.key, now)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the idempotency record: %w", ErrModerationUnavailable, err)
	}
	if stored != nil && stored.Fingerprint == scope.fingerprint {
		result := stored.Result
		return &result, nil
	}
	return nil, refusal
}

// storedResult serves the result stored for scope: the replay when the
// fingerprint matches, IdempotencyConflict when the key was used for another
// request, and nil when nothing is stored.
func storedResult(ctx context.Context, tx CommunityActionTx, scope idempotencyScope, now time.Time) (*CommunityActionResult, error) {
	stored, err := tx.IdempotencyRecord(ctx, scope.actorDID, scope.communityDID, scope.key, now)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the idempotency record: %w", ErrModerationUnavailable, err)
	}
	if stored == nil {
		return nil, nil
	}
	if stored.Fingerprint != scope.fingerprint {
		return nil, fmt.Errorf("%w: idempotency key %q was used for a different request", ErrIdempotencyConflict, scope.key)
	}
	replay := stored.Result
	return &replay, nil
}

// lockFor is the action's locks: the roster, the actor and subject accounts
// in ascending DID order, and the target record's rev-gate row.
func lockFor(communityDID, actorDID, subjectDID, targetURI string) CommunityActionLock {
	return newCommunityActionLock(communityDID, []string{actorDID, subjectDID}, []string{targetURI}, nil)
}

// newCommunityActionLock is a roster-locked action lock over the accounts,
// target URIs and ban subjects, each deduplicated in ascending order. The
// lowest target URI is TargetURI and the rest are OtherTargetURIs.
func newCommunityActionLock(communityDID string, accounts, targets, banSubjects []string) CommunityActionLock {
	lock := CommunityActionLock{
		CommunityDID:   communityDID,
		Roster:         true,
		AccountDIDs:    sortedUnique(accounts),
		BanSubjectDIDs: sortedUnique(banSubjects),
	}
	if uris := sortedUnique(targets); len(uris) > 0 {
		lock.TargetURI = uris[0]
		lock.OtherTargetURIs = uris[1:]
	}
	return lock
}

// intentLock is the locks settling intent takes: the roster, the requester's,
// the subject's and a paired write's subject's accounts, every target the
// intent writes and, for an accept, the invitee's ban subject, under which a
// retry rechecks the invitee's ban before writing again. A ban or unban takes
// the requester's account and the subject's ban subject.
func intentLock(intent CommunityIntent) CommunityActionLock {
	accounts := []string{intent.ActorDID, intent.SubjectDID}
	targets := []string{intent.TargetURI}
	if intent.PairedTargetURI != "" {
		targets = append(targets, intent.PairedTargetURI)
		if subject := recordSubject(intent.PairedRecord); subject != "" {
			accounts = append(accounts, subject)
		}
	}
	var banSubjects []string
	if intent.InvitationID != "" {
		banSubjects = []string{intent.SubjectDID}
	}
	if isBanIntent(intent) {
		// A ban takes the actor's account lock and the subject's ban-subject
		// lock, not the subject's account lock.
		accounts = []string{intent.ActorDID}
		banSubjects = []string{intent.SubjectDID}
	}
	lock := newCommunityActionLock(intent.CommunityDID, accounts, targets, banSubjects)
	// An accept's audit actor is its inviter; completing the accept marks the
	// inviter's invitation accepted.
	lock.ErasureGateDIDs = sortedUnique([]string{intent.AuditActorDID})
	return lock
}

// lockTargets is every target URI lock holds.
func lockTargets(lock CommunityActionLock) []string {
	if lock.TargetURI == "" {
		return lock.OtherTargetURIs
	}
	return append([]string{lock.TargetURI}, lock.OtherTargetURIs...)
}

// mergeLocks is the union of two locks on the same community.
func mergeLocks(a, b CommunityActionLock) CommunityActionLock {
	merged := newCommunityActionLock(a.CommunityDID,
		append(append([]string(nil), a.AccountDIDs...), b.AccountDIDs...),
		append(lockTargets(a), lockTargets(b)...),
		append(append([]string(nil), a.BanSubjectDIDs...), b.BanSubjectDIDs...))
	merged.ErasureGateDIDs = sortedUnique(append(append([]string(nil), a.ErasureGateDIDs...), b.ErasureGateDIDs...))
	return merged
}

// lockCovers reports whether held holds every lock needed names.
func lockCovers(held, needed CommunityActionLock) bool {
	if needed.Roster && !held.Roster {
		return false
	}
	return containsAll(held.AccountDIDs, needed.AccountDIDs) &&
		containsAll(lockTargets(held), lockTargets(needed)) &&
		containsAll(held.BanSubjectDIDs, needed.BanSubjectDIDs) &&
		containsAll(append(append([]string(nil), held.AccountDIDs...), held.ErasureGateDIDs...), needed.ErasureGateDIDs)
}

func containsAll(set, items []string) bool {
	for _, item := range items {
		found := false
		for _, member := range set {
			if member == item {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sortedUnique(values []string) []string {
	var out []string
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	unique := out[:0]
	for i, value := range out {
		if i == 0 || value != out[i-1] {
			unique = append(unique, value)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	return unique
}

// settleOtherIntent settles another key's open intent on one of an attempt's
// targets: it asks for a restart under wider locks when the intent writes a
// target the attempt does not hold, and otherwise settles it from the repo and
// asks for a restart under the same locks.
func (s *CommunityActionService) settleOtherIntent(ctx context.Context, tx CommunityActionTx, lock CommunityActionLock, other CommunityIntent) (*restart, error) {
	needed := intentLock(other)
	if !lockCovers(lock, needed) {
		return &restart{lock: mergeLocks(lock, needed)}, nil
	}
	if _, _, err := s.reconcile(ctx, tx, other, false); err != nil {
		return nil, err
	}
	return &restart{lock: lock, settled: true}, nil
}

// attempt runs the action in one action transaction. restart reports that it
// settled an open intent on the target (an expired one of this key, or one of
// another key) and committed that, so the caller should run the pipeline again
// from the locks.
func (s *CommunityActionService) attempt(ctx context.Context, action rosterAction, lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
	tx, err := s.cfg.Store.BeginAction(ctx, lock)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: opening the community action transaction: %w", ErrModerationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	auth, err := s.authorize(ctx, action)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			result, err := replayOrRefuse(ctx, tx, action.scope(), s.cfg.Now(), err)
			return result, nil, err
		}
		return nil, nil, err
	}

	now := s.cfg.Now()
	if replay, err := storedResult(ctx, tx, action.scope(), now); replay != nil || err != nil {
		return replay, nil, err
	}
	if result, again, done, err := s.reconcileOwnIntent(ctx, tx, action.scope(), lock); done {
		return result, again, err
	}
	// Another key's open intent on the target: its writer's locks are gone, so
	// it is settled from the repo before the target is read. While it is
	// unexpired and the head is still its guard, its write may yet commit, so
	// the request is refused with StateConflict instead.
	other, err := tx.OpenIntentByTarget(ctx, action.targetURI)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the target's open intent: %w", ErrModerationUnavailable, err)
	}
	if other != nil {
		again, err := s.settleOtherIntent(ctx, tx, lock, *other)
		return nil, again, err
	}

	row, err := s.targetRole(ctx, tx, action, auth)
	if err != nil {
		return nil, nil, err
	}
	version, err := tx.RosterVersion(ctx, action.communityDID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the roster version: %w", ErrModerationUnavailable, err)
	}
	if version != action.expectedVersion {
		return nil, nil, fmt.Errorf("%w: roster version is %d, not %d", ErrStateConflict, version, action.expectedVersion)
	}

	repo, err := s.cfg.Repos(ctx, action.communityDID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: opening the community repo: %w", ErrModerationUnavailable, err)
	}
	head, err := repo.GetLatestCommit(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the community repo head: %w", ErrModerationUnavailable, err)
	}
	record, err := repo.GetRecord(ctx, communities.RoleCollection, action.recordKey)
	if err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			// No open intent explains it: the index missed the record's delete.
			slog.Error("community role indexed but absent from the community repo; the firehose or a redrive must remove it",
				"community", action.communityDID, "record", action.targetURI, "indexed_cid", row.RecordCID)
			return nil, nil, fmt.Errorf("%w: the role record is gone from the community repo but still indexed", ErrStateConflict)
		}
		return nil, nil, fmt.Errorf("%w: reading the role record: %w", ErrModerationUnavailable, err)
	}
	if record.CID != row.RecordCID {
		return nil, nil, fmt.Errorf("%w: the indexed role record is behind the community repo", ErrStateConflict)
	}

	intentID, err := newCommunityIntentID()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	intent := CommunityIntent{
		ID:                   intentID,
		Action:               ActionRemoveRole,
		ActorDID:             action.actorDID,
		AdminActing:          auth.InstanceAdmin,
		CommunityDID:         action.communityDID,
		SubjectDID:           action.subjectDID,
		TargetURI:            action.targetURI,
		Op:                   string(pds.WriteOpDelete),
		ReplacedCID:          record.CID,
		GuardCommitCID:       head.CID,
		GuardRev:             head.Rev,
		IdempotencyKey:       action.key,
		Fingerprint:          action.fingerprint,
		State:                CommunityIntentOpen,
		IdempotencyRetention: s.cfg.IdempotencyTTL,
		CreatedAt:            now,
		ExpiresAt:            now.Add(s.cfg.IntentTTL),
	}
	if err := s.cfg.Store.SaveIntent(ctx, intent); err != nil {
		if errors.Is(err, ErrStateConflict) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: storing the community write intent: %w", ErrModerationUnavailable, err)
	}
	result, err := s.write(ctx, tx, repo, intent, false)
	return result, nil, err
}

// reconcileOwnIntent settles the request's own key's open intent: done reports
// that there was one, and result, again and err are then the attempt's
// outcome. Another request on the key is IdempotencyConflict.
func (s *CommunityActionService) reconcileOwnIntent(ctx context.Context, tx CommunityActionTx, scope idempotencyScope, lock CommunityActionLock) (result *CommunityActionResult, again *restart, done bool, err error) {
	open, err := tx.IntentByKey(ctx, scope.actorDID, scope.communityDID, scope.key)
	if err != nil {
		return nil, nil, true, fmt.Errorf("%w: reading the open intent: %w", ErrModerationUnavailable, err)
	}
	if open == nil || open.State != CommunityIntentOpen {
		return nil, nil, false, nil
	}
	if open.Fingerprint != scope.fingerprint {
		return nil, nil, true, fmt.Errorf("%w: idempotency key %q was used for a different request", ErrIdempotencyConflict, scope.key)
	}
	if needed := intentLock(*open); !lockCovers(lock, needed) {
		return nil, &restart{lock: mergeLocks(lock, needed)}, true, nil
	}
	result, expired, err := s.reconcile(ctx, tx, *open, true)
	if expired {
		return nil, &restart{lock: lock, settled: true}, true, err
	}
	return result, nil, true, err
}

// targetRole applies the roster rules to the authorized actor and returns the
// subject's indexed role row, read in the action transaction.
func (s *CommunityActionService) targetRole(ctx context.Context, tx CommunityActionTx, action rosterAction, auth communities.RoleAuthorization) (communities.CommunityRole, error) {
	if action.operation == resignRoleOperation {
		if auth.Role == communities.RoleOwner {
			return communities.CommunityRole{}, fmt.Errorf("%w: the owner resigns by transferring ownership", ErrOwnerMustTransfer)
		}
		if auth.Role == "" {
			return communities.CommunityRole{}, fmt.Errorf("%w: the caller holds no role in community %s", ErrRoleNotFound, action.communityDID)
		}
	}
	rows, err := tx.ListCommunityRoles(ctx, action.communityDID)
	if err != nil {
		return communities.CommunityRole{}, fmt.Errorf("%w: reading the community roster: %w", ErrModerationUnavailable, err)
	}
	roster := communities.EffectiveRoster(rows)
	if action.operation == removeModeratorOperation && roster.Owner != nil && roster.Owner.SubjectDID == action.subjectDID {
		return communities.CommunityRole{}, fmt.Errorf("%w: community %s", ErrCannotRemoveOwner, action.communityDID)
	}
	for _, row := range roster.Moderators {
		if row.SubjectDID == action.subjectDID {
			return row, nil
		}
	}
	return communities.CommunityRole{}, fmt.Errorf("%w: the subject holds no role in community %s", ErrRoleNotFound, action.communityDID)
}

// write applies the intent's writes in one ApplyWrites call under the intent's
// guard commit and, on success, applies the local effect and commits. A
// swapCommit refusal proves only that this call committed nothing: on a first
// attempt the intent fails and the caller gets StateConflict; on a retry an
// earlier attempt may have committed since the head was read, so the intent is
// settled from the repo. On any other PDS error the outcome is unknown: the
// action transaction rolls back and the intent stays open for reconciliation.
func (s *CommunityActionService) write(ctx context.Context, tx CommunityActionTx, repo posts.CommunityRepo, intent CommunityIntent, retried bool) (*CommunityActionResult, error) {
	writes, err := intentWrites(intent)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	if s.cfg.Hooks.BeforeWrite != nil {
		s.cfg.Hooks.BeforeWrite(ctx)
	}
	written, err := repo.ApplyWrites(ctx, writes, intent.GuardCommitCID)
	if errors.Is(err, pds.ErrSwapConflict) {
		if retried {
			head, err := repo.GetLatestCommit(ctx)
			if err != nil {
				return nil, fmt.Errorf("%w: reading the community repo head after a refused retry: %w", ErrModerationUnavailable, err)
			}
			return s.settleMovedHead(ctx, tx, repo, intent, head, true)
		}
		if err := s.finish(ctx, tx, intent, CommunityIntentFailed); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the community repo changed before the role records were written; nothing was written", ErrStateConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: writing the role records: %w", ErrModerationUnavailable, err)
	}
	commit := CommunityCommit{Rev: written.CommitRev, GateRev: written.CommitRev, CompletedAt: s.cfg.Now()}
	if intent.Op != string(pds.WriteOpDelete) {
		if len(written.Results) != len(writes) {
			return nil, fmt.Errorf("%w: the community repo answered %d write results for %d writes", ErrModerationUnavailable, len(written.Results), len(writes))
		}
		commit.RecordCID = written.Results[0].CID
		if intent.PairedTargetURI != "" {
			commit.PairedRecordCID = written.Results[1].CID
		}
	}
	return s.complete(ctx, tx, intent, commit)
}

// intentWrites is the intent's ApplyWrites batch: its target's write (a role
// or ban record), then a transfer's paired update of the previous owner's
// record.
func intentWrites(intent CommunityIntent) ([]pds.Write, error) {
	collection, rkey, err := intentTargetKey(intent.TargetURI)
	if err != nil {
		return nil, err
	}
	if intent.Op == string(pds.WriteOpDelete) {
		return []pds.Write{{Op: pds.WriteOpDelete, Collection: collection, RKey: rkey}}, nil
	}
	writes := []pds.Write{{Op: pds.WriteOp(intent.Op), Collection: collection, RKey: rkey, Record: intent.Record}}
	if intent.PairedTargetURI != "" {
		pairedKey, err := roleRecordKeyOf(intent.PairedTargetURI)
		if err != nil {
			return nil, err
		}
		writes = append(writes, pds.Write{Op: pds.WriteOpUpdate, Collection: communities.RoleCollection, RKey: pairedKey, Record: intent.PairedRecord})
	}
	return writes, nil
}

// roleRecordURI is the AT-URI of a community's role record.
func roleRecordURI(communityDID, recordKey string) string {
	return "at://" + communityDID + "/" + communities.RoleCollection + "/" + recordKey
}

// intentTargetKey is the collection and record key of a role or ban intent's
// target URI.
func intentTargetKey(uri string) (string, string, error) {
	parsed, err := syntax.ParseATURI(uri)
	if err == nil && parsed.Collection().String() == communities.BanCollection && parsed.RecordKey() != "" {
		return communities.BanCollection, parsed.RecordKey().String(), nil
	}
	rkey, err := roleRecordKeyOf(uri)
	return communities.RoleCollection, rkey, err
}

// roleRecordKeyOf is the record key of a role record URI.
func roleRecordKeyOf(uri string) (string, error) {
	parsed, err := syntax.ParseATURI(uri)
	if err != nil || parsed.Collection().String() != communities.RoleCollection || parsed.RecordKey() == "" {
		return "", fmt.Errorf("%q is not a community role record URI", uri)
	}
	return parsed.RecordKey().String(), nil
}

// recordSubject is the subject DID of a role record body, "" when it has none.
func recordSubject(body json.RawMessage) string {
	var record struct {
		Subject string `json:"subject"`
	}
	if json.Unmarshal(body, &record) != nil {
		return ""
	}
	return record.Subject
}

// sameRecordBody reports whether a record read from the repo holds body,
// comparing the canonical JSON encodings of both (object keys sorted). A
// record or body that does not encode is Unavailable: it proves nothing about
// whether the write committed.
func sameRecordBody(value map[string]any, body json.RawMessage) (bool, error) {
	want, err := canonicalJSON(body)
	if err != nil {
		return false, fmt.Errorf("%w: decoding the intent's role record: %w", ErrModerationUnavailable, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("%w: encoding the role record read from the community repo: %w", ErrModerationUnavailable, err)
	}
	got, err := canonicalJSON(encoded)
	if err != nil {
		return false, fmt.Errorf("%w: decoding the role record read from the community repo: %w", ErrModerationUnavailable, err)
	}
	return bytes.Equal(got, want), nil
}

// canonicalJSON re-encodes a JSON document with its object keys sorted.
func canonicalJSON(document []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(document, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

func (s *CommunityActionService) complete(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, commit CommunityCommit) (*CommunityActionResult, error) {
	result, err := tx.CompleteIntent(ctx, intent, commit, s.cfg.IdempotencyTTL)
	if err != nil {
		return nil, fmt.Errorf("%w: completing the community write intent: %w", ErrModerationUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: committing the community action: %w", ErrModerationUnavailable, err)
	}
	return &result, nil
}

func (s *CommunityActionService) finish(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, state CommunityIntentState) error {
	if err := tx.FinishIntent(ctx, intent.ID, state); err != nil {
		return fmt.Errorf("%w: marking the community write intent %s: %w", ErrModerationUnavailable, state, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: committing the %s community write intent: %w", ErrModerationUnavailable, state, err)
	}
	return nil
}

// reconcile settles an open intent from the community repo: head first, then
// the target record. retry is a same-key request, which re-attempts an
// unexpired uncommitted write under the original guard; the sweep never
// re-attempts, and another key's request is refused while the intent is
// unexpired at its guard. restart reports an expired intent a retry must
// replace. A post intent settles from its stand record and its latest
// recorded attempt instead (reconcilePostIntent), and a comment intent from its
// removal record (reconcileCommentIntent).
func (s *CommunityActionService) reconcile(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, retry bool) (result *CommunityActionResult, restart bool, err error) {
	if isCommentIntent(intent) {
		return s.reconcileCommentIntent(ctx, tx, intent, retry)
	}
	if isPostIntent(intent) {
		return s.reconcilePostIntent(ctx, tx, intent, retry)
	}
	repo, err := s.cfg.Repos(ctx, intent.CommunityDID)
	if err != nil {
		return nil, false, fmt.Errorf("%w: opening the community repo: %w", ErrModerationUnavailable, err)
	}
	head, err := repo.GetLatestCommit(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("%w: reading the community repo head: %w", ErrModerationUnavailable, err)
	}
	if head.CID == intent.GuardCommitCID {
		if s.cfg.Now().Before(intent.ExpiresAt) {
			if retry {
				if err := s.recheckRetriedWrite(ctx, tx, intent); err != nil {
					return nil, false, err
				}
				result, err := s.write(ctx, tx, repo, intent, true)
				return result, false, err
			}
			// Another key's unexpired write may still commit on the PDS:
			// expiring it now would orphan that commit from its actor.
			return nil, false, fmt.Errorf("%w: another request's write to the role record has an unknown outcome", ErrStateConflict)
		}
		if err := s.finish(ctx, tx, intent, CommunityIntentExpired); err != nil {
			return nil, false, err
		}
		return nil, retry, nil
	}
	result, err = s.settleMovedHead(ctx, tx, repo, intent, head, retry)
	return result, false, err
}

// recheckRetriedWrite rechecks, before a same-key retry writes an uncommitted
// accept again, what the accept checked before its intent was stored: the
// invitation open and not lapsed, the invitee not banned (under the
// ban-subject lock intentLock holds) and the roster rules. A refusal fails the
// intent and is returned; an Unavailable read leaves it open. Other intents
// have no preconditions to recheck.
func (s *CommunityActionService) recheckRetriedWrite(ctx context.Context, tx CommunityActionTx, intent CommunityIntent) error {
	if intent.InvitationID == "" {
		return nil
	}
	err := s.recheckAcceptance(ctx, tx, intent)
	if err == nil || errors.Is(err, ErrModerationUnavailable) {
		return err
	}
	if finishErr := s.finish(ctx, tx, intent, CommunityIntentFailed); finishErr != nil {
		return finishErr
	}
	return err
}

// settleMovedHead settles an open intent once the head has left its guard. A
// committed intent completes with its actor. One whose records all still hold
// what they held before the write did not commit: it fails, and a retry gets
// StateConflict. Any other outcome is unknown (a record changed again after
// the write may hide it): the intent stays open for its firehose copy to
// complete, and the caller gets ModerationUnavailable.
func (s *CommunityActionService) settleMovedHead(ctx context.Context, tx CommunityActionTx, repo posts.CommunityRepo, intent CommunityIntent, head *pds.LatestCommit, retry bool) (*CommunityActionResult, error) {
	commit, outcome, err := committedWrite(ctx, repo, intent)
	if err != nil {
		return nil, err
	}
	switch outcome {
	case writeCommitted:
		commit.Rev = head.Rev
		commit.GateRev = intent.GuardRev
		commit.CompletedAt = s.cfg.Now()
		return s.complete(ctx, tx, intent, commit)
	case writeOutcomeUnknown:
		return nil, fmt.Errorf("%w: the role records changed after the write, so whether it committed is unknown until its firehose copy arrives", ErrModerationUnavailable)
	}
	if err := s.finish(ctx, tx, intent, CommunityIntentFailed); err != nil {
		return nil, err
	}
	if !retry {
		return nil, nil
	}
	return nil, fmt.Errorf("%w: the community repo moved and the role records were not written", ErrStateConflict)
}

// writeOutcome is what the repo shows of an intent's write once the head has
// moved.
type writeOutcome int

const (
	writeNotCommitted   writeOutcome = iota // every target holds what it held before the write
	writeCommitted                          // a target holds what the write left
	writeOutcomeUnknown                     // a target changed again after the write, or before it from outside
)

// committedWrite reads the intent's targets back from the repo and reports
// the write's outcome, with the record CIDs it left. A delete committed when
// its target is gone or holds another CID. A create or update committed when
// its target holds the intent's body; a transfer also when either of its two
// records holds its body, since one commit wrote both, and a record that no
// longer holds its body has its CID left empty. A create or update did not
// commit when every target is as it was before the write (absent for a
// create, at the replaced CID for an update); otherwise the outcome is
// unknown.
func committedWrite(ctx context.Context, repo posts.CommunityRepo, intent CommunityIntent) (CommunityCommit, writeOutcome, error) {
	collection, rkey, err := intentTargetKey(intent.TargetURI)
	if err != nil {
		return CommunityCommit{}, writeOutcomeUnknown, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	record, err := readStandRecord(ctx, repo, collection, rkey)
	if err != nil {
		return CommunityCommit{}, writeOutcomeUnknown, err
	}
	if intent.Op == string(pds.WriteOpDelete) {
		if record == nil || record.CID != intent.ReplacedCID {
			return CommunityCommit{}, writeCommitted, nil
		}
		return CommunityCommit{}, writeNotCommitted, nil
	}
	var commit CommunityCommit
	recordCID, unwritten, err := writtenRecordCID(record, intent.Record, intent.ReplacedCID)
	if err != nil {
		return CommunityCommit{}, writeOutcomeUnknown, err
	}
	commit.RecordCID = recordCID
	if intent.PairedTargetURI != "" {
		pairedKey, err := roleRecordKeyOf(intent.PairedTargetURI)
		if err != nil {
			return CommunityCommit{}, writeOutcomeUnknown, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
		}
		paired, err := readRoleRecord(ctx, repo, pairedKey)
		if err != nil {
			return CommunityCommit{}, writeOutcomeUnknown, err
		}
		pairedCID, pairedUnwritten, err := writtenRecordCID(paired, intent.PairedRecord, intent.PairedReplacedCID)
		if err != nil {
			return CommunityCommit{}, writeOutcomeUnknown, err
		}
		commit.PairedRecordCID = pairedCID
		unwritten = unwritten && pairedUnwritten
	}
	switch {
	case commit.RecordCID != "" || commit.PairedRecordCID != "":
		return commit, writeCommitted, nil
	case unwritten:
		return CommunityCommit{}, writeNotCommitted, nil
	default:
		return CommunityCommit{}, writeOutcomeUnknown, nil
	}
}

// writtenRecordCID compares a record read from the repo (nil when absent)
// with the body a create or update wrote to it: the record's CID when it
// holds that body, else "", and whether it is as it was before the write
// (absent when replacedCID is empty, else at replacedCID).
func writtenRecordCID(record *pds.RecordResponse, body json.RawMessage, replacedCID string) (string, bool, error) {
	if record == nil {
		return "", replacedCID == "", nil
	}
	same, err := sameRecordBody(record.Value, body)
	if err != nil {
		return "", false, err
	}
	if same {
		return record.CID, false, nil
	}
	return "", replacedCID != "" && record.CID == replacedCID, nil
}

// readRoleRecord reads a role record from the repo, nil when it is absent.
func readRoleRecord(ctx context.Context, repo posts.CommunityRepo, rkey string) (*pds.RecordResponse, error) {
	record, err := repo.GetRecord(ctx, communities.RoleCollection, rkey)
	if errors.Is(err, pds.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: reading the role record: %w", ErrModerationUnavailable, err)
	}
	return record, nil
}

// SweepExpiredIntents reconciles open intents past their expiry, each in its
// own action transaction, without re-attempting any write: an uncommitted
// intent expires, a committed one completes with its actor, one whose records
// survived a moved head unchanged fails, and one whose outcome is unknown stays
// open (its error is returned) until a post intent's firehose-lag bound
// closes it. It settles at most ExpiredIntentSweepLimit intents, reading
// further pages past the ones it could not settle so they cannot hold back
// newer expired intents, and returns how many it settled.
func (s *CommunityActionService) SweepExpiredIntents(ctx context.Context) (int, error) {
	now, limit := s.cfg.Now(), s.cfg.ExpiredIntentSweepLimit
	settled := 0
	var errs []error
	var after *CommunityIntent
	for settled < limit {
		intents, err := s.cfg.Store.ExpiredOpenIntents(ctx, now, after, limit)
		if err != nil {
			errs = append(errs, fmt.Errorf("listing expired community write intents: %w", err))
			break
		}
		for i := range intents {
			if settled >= limit {
				break
			}
			intent := intents[i]
			after = &intents[i]
			done, err := s.sweepIntent(ctx, intent)
			if err != nil {
				errs = append(errs, fmt.Errorf("reconciling community write intent %s: %w", intent.ID, err))
				continue
			}
			if done {
				settled++
			}
		}
		if len(intents) < limit {
			break
		}
	}
	return settled, errors.Join(errs...)
}

// sweepIntent reconciles one expired intent under the locks its action takes.
// It returns false when another request settled the intent first.
func (s *CommunityActionService) sweepIntent(ctx context.Context, intent CommunityIntent) (bool, error) {
	tx, err := s.cfg.Store.BeginAction(ctx, intentLock(intent))
	if err != nil {
		return false, fmt.Errorf("opening the community action transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := tx.IntentByKey(ctx, intent.ActorDID, intent.CommunityDID, intent.IdempotencyKey)
	if err != nil {
		return false, fmt.Errorf("reading the intent: %w", err)
	}
	if current == nil || current.ID != intent.ID || current.State != CommunityIntentOpen {
		return false, nil
	}
	if _, _, err := s.reconcile(ctx, tx, *current, false); err != nil {
		return false, err
	}
	return true, nil
}

// newCommunityIntentID is a random intent ID.
func newCommunityIntentID() (string, error) {
	return randomHexID("community write intent ID")
}

// randomHexID is 16 random bytes in hex; what names the ID in the error.
func randomHexID(what string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating a %s: %w", what, err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// CommunityActionStore is the durable state of community actions.
type CommunityActionStore interface {
	// BeginAction opens an action transaction holding lock's locks, taken in
	// the one community lock order (docs/MODERATION.md). Its connection comes
	// from a pool that nothing else the action does draws on, so a lock holder
	// never waits for a connection a lock waiter holds.
	BeginAction(ctx context.Context, lock CommunityActionLock) (CommunityActionTx, error)
	// SaveIntent stores a new open intent, committed outside any action
	// transaction (on the request pool) so it is durable before the PDS write.
	SaveIntent(ctx context.Context, intent CommunityIntent) error
	// ExpiredOpenIntents lists up to limit open intents whose ExpiresAt is at
	// or before now, ordered by ExpiresAt then ID, starting after the intent
	// after (nil starts from the first).
	ExpiredOpenIntents(ctx context.Context, now time.Time, after *CommunityIntent, limit int) ([]CommunityIntent, error)
	// InvitationByID reads an invitation outside any action transaction, or
	// nil when there is none.
	InvitationByID(ctx context.Context, invitationID string) (*communities.Invitation, error)
}

// CommunityActionLock names the locks an action transaction takes.
type CommunityActionLock struct {
	CommunityDID string
	Roster       bool     // the community roster lock
	AccountDIDs  []string // per-(community, account) locks, ascending
	TargetURI    string   // the target record's rev-gate row; empty for none
	// OtherTargetURIs are further target records' rev-gate rows; every target
	// URI is locked in ascending order.
	OtherTargetURIs []string
	// BanSubjectDIDs are the community ban-subject locks.
	BanSubjectDIDs []string
	// ErasureGateDIDs are further accounts whose shared erasure gate the
	// action takes, with AccountDIDs', without their per-account locks: an
	// accept's inviter, whose erasure deletes the invitation the accept
	// marks accepted.
	ErasureGateDIDs []string
}

// CommunityActionTx is one action transaction.
type CommunityActionTx interface {
	// PendingRoleRecordDeleteCID returns the CID the queued erasure delete of
	// (communityDID, recordKey) is guarded on, or "" when none is queued.
	PendingRoleRecordDeleteCID(ctx context.Context, communityDID, recordKey string) (string, error)
	IdempotencyRecord(ctx context.Context, actorDID, communityDID, key string, now time.Time) (*CommunityIdempotencyRecord, error)
	// IntentByKey returns the newest intent for the key in any state, or nil.
	IntentByKey(ctx context.Context, actorDID, communityDID, key string) (*CommunityIntent, error)
	// OpenIntentByTarget returns the open intent on the target record URI,
	// locked, or nil.
	OpenIntentByTarget(ctx context.Context, targetURI string) (*CommunityIntent, error)
	// OpenIntentByTargetUnlocked is OpenIntentByTarget without the row lock,
	// for a read that must not wait on an intent another transaction holds.
	OpenIntentByTargetUnlocked(ctx context.Context, targetURI string) (*CommunityIntent, error)
	// OpenIntentByAction returns the community's oldest open intent of
	// action, locked, or nil.
	OpenIntentByAction(ctx context.Context, communityDID, action string) (*CommunityIntent, error)
	// OpenInvitationIntent returns the oldest open intent that accepts one of
	// the invitee's invitations in the community, or nil.
	OpenInvitationIntent(ctx context.Context, communityDID, inviteeDID string) (*CommunityIntent, error)
	// ListCommunityRoles reads the community's indexed role rows, as
	// communities.RoleReader does, in the transaction.
	ListCommunityRoles(ctx context.Context, communityDID string) ([]communities.CommunityRole, error)
	RosterVersion(ctx context.Context, communityDID string) (int64, error)
	// CompleteIntent applies the intent's local effect at commit: advance the
	// rev gate of every target to commit.GateRev, then
	//   - delete: drop the index row if it still has the replaced CID;
	//   - create or update: upsert the target's index row from the intent's
	//     body with commit.RecordCID and, for a transfer, the paired target's
	//     row from the paired body with commit.PairedRecordCID (an empty CID
	//     leaves that row to the firehose; a transfer needs one of the two),
	//     both in this transaction;
	// and, when the index changed, advance the roster version once and insert
	// one moderation action (intent.Action, actor AuditActorDID when set, else
	// ActorDID), mark the intent's invitation accepted when it is still open,
	// then mark the intent completed and save the idempotency record, unless
	// the key holds another request's unexpired result, which is kept. When
	// the index did not change, the change was already indexed and logged,
	// and the intent
	// completes with the subject's latest action of its kind. When a firehose
	// copy completed the intent first it returns the result stored then.
	CompleteIntent(ctx context.Context, intent CommunityIntent, commit CommunityCommit, idempotencyTTL time.Duration) (CommunityActionResult, error)
	// FinishIntent moves an open intent to failed or expired.
	FinishIntent(ctx context.Context, intentID string, state CommunityIntentState) error
	// InvitationByID reads the invitation, row locked, or nil when there is
	// none.
	InvitationByID(ctx context.Context, invitationID string) (*communities.Invitation, error)
	// CreateInvitation supersedes the invitee's open invitation in the
	// community, inserts inv and writes the invitee's notification.
	CreateInvitation(ctx context.Context, inv communities.Invitation) error
	// SetInvitationState moves an open invitation to state; one that is not
	// open is an error.
	SetInvitationState(ctx context.Context, invitationID string, state communities.InvitationState) error
	// SaveIdempotencyRecord stores a result under (actor, community, key) for
	// ttl.
	SaveIdempotencyRecord(ctx context.Context, actorDID, communityDID, key string, record CommunityIdempotencyRecord, ttl time.Duration) error
	// GetCommunityBan reads the community ban index in the transaction, as
	// communities.BanReader does.
	GetCommunityBan(ctx context.Context, communityDID, subjectDID string) (*communities.CommunityBan, error)
	// CommunityPost reads the post's community stand state under the
	// action's locks, or ErrPostNotFound when the post is not indexed in a
	// community.
	CommunityPost(ctx context.Context, postURI string) (CommunityPostState, error)
	// CommunityComment reads the comment's community removal state under the
	// action's locks, or ErrCommentNotFound when the comment's parent chain
	// does not reach a post indexed in a community.
	CommunityComment(ctx context.Context, commentURI string) (CommunityCommentState, error)
	// RecentCommunityRemovals counts the non-admin local community post
	// removals since since, logged or with an open intent: by actorDID, and in
	// communityDID by anyone.
	RecentCommunityRemovals(ctx context.Context, communityDID, actorDID string, since time.Time) (byActor, byCommunity int, err error)
	// InstanceOutboxWroteRemoval reports whether the instance removal outbox
	// (migration 080) wrote the removal record standing in communityDID's
	// repo: the record its row owns by CID, or, while a write of the row's
	// may still be landing, a record whose content is exactly what that write
	// sent (the outbox's own ownership rule).
	InstanceOutboxWroteRemoval(ctx context.Context, communityDID string, removal posts.StandingRemoval) (bool, error)
	// CommunityAccountBan reads the subject's standing ban row in the
	// community (in force or lapsed) and the (community, subject DID)
	// version, under the action's locks.
	CommunityAccountBan(ctx context.Context, communityDID, subjectDID string) (CommunityAccountBanState, error)
	// RecentCommunityBans counts the non-admin local community bans since
	// since, logged or with an open intent: by actorDID, and in communityDID
	// by anyone.
	RecentCommunityBans(ctx context.Context, communityDID, actorDID string, since time.Time) (byActor, byCommunity int, err error)
	Commit() error
	Rollback() error
}

// CommunityCommit is the repo commit a community write landed in. RecordCID is
// empty for a delete, and for a transfer whose target record a reconciliation
// found changed again after the transfer (its paired record proved the commit).
type CommunityCommit struct {
	Rev, RecordCID string
	// PairedRecordCID is the CID of the paired write's record (a transfer's
	// previous owner record), empty when there is none or when a
	// reconciliation found that record changed again after the transfer.
	PairedRecordCID string
	// GateRev is the rev the record's rev gate is advanced to when it holds
	// an older one; empty leaves the gate. A direct write sets the commit rev
	// the PDS returned. A reconciliation reads the head, not the write's own
	// commit, so it sets the intent's guard rev: every event at or before the
	// guard is then stale, while the firehose still applies a later change.
	GateRev string
	// CompletedAt is when the service completed the intent: the start of
	// the idempotency record's retention. The action's own time is the
	// database clock read when the action is inserted, with the record's rev
	// gate row locked, as the firehose's role actions do, so the log of one
	// record's gated changes stays in commit order.
	CompletedAt time.Time
}

// CommunityIdempotencyRecord is a stored community action result.
type CommunityIdempotencyRecord struct {
	Fingerprint string
	Result      CommunityActionResult
}

// CommunityIntentState is the lifecycle state of a community write intent.
type CommunityIntentState string

// The community write intent states.
const (
	CommunityIntentOpen      CommunityIntentState = "open"
	CommunityIntentCompleted CommunityIntentState = "completed"
	CommunityIntentFailed    CommunityIntentState = "failed"
	CommunityIntentExpired   CommunityIntentState = "expired"
)

// CommunityIntent is a durable record of a community repo write in flight.
// Op is "create", "update" or "delete"; Record is the create/update body.
type CommunityIntent struct {
	ID, Action, ActorDID string
	// AuditActorDID is the action's actor when it differs from the requester
	// ActorDID (an accept's inviter); empty otherwise.
	AuditActorDID                           string
	AdminActing                             bool
	CommunityDID, SubjectDID, TargetURI, Op string
	// SubjectURI is the record a comment action's removal is about; empty
	// for every other intent.
	SubjectURI                            string
	Record                                json.RawMessage
	ReplacedCID, GuardCommitCID, GuardRev string
	// InvitationID is the invitation an accept settles.
	InvitationID string
	// PairedTargetURI, PairedRecord and PairedReplacedCID are a transfer's
	// second write: the previous owner's record rewritten as moderator.
	PairedTargetURI             string
	PairedRecord                json.RawMessage
	PairedReplacedCID           string
	IdempotencyKey, Fingerprint string
	// IdempotencyRetention is how long the idempotency record saved when the
	// intent completes is kept, whichever path completes it.
	IdempotencyRetention           time.Duration
	State                          CommunityIntentState
	CommitRev, RecordCID, ActionID string
	CreatedAt, ExpiresAt           time.Time
}
