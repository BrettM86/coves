package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/rivo/uniseg"
)

// Fingerprint operations of the community ban actions. They differ from the
// instance account ban's, which share the idempotency table.
const (
	communityBanOperation   = "community-ban-account"
	communityUnbanOperation = "community-unban-account"
)

// CommunityBanActionReason is the reason every community ban and unban action
// is logged with. The ban's free-text reason is shown only to the banned
// account and is never put in the action log.
const CommunityBanActionReason = communityRemovalDefaultActionReason

// The ban record's reason bounds (ban.json maxGraphemes, maxLength).
const (
	maxCommunityBanReasonGraphemes = 2000
	maxCommunityBanReasonBytes     = 20000
)

// The listBans page size bounds.
const (
	defaultCommunityBanListLimit = 50
	maxCommunityBanListLimit     = 100
	maxCommunityBanCursorBytes   = 500
)

// BanCommunityAccountRequest asks to ban an account from a community. Reason
// is optional; ExpiresAt nil is a ban with no expiry.
type BanCommunityAccountRequest struct {
	ActorDID, CommunityDID, SubjectDID, Reason string
	ExpiresAt                                  *time.Time
	ExpectedVersion                            int64
	IdempotencyKey                             string
}

// UnbanCommunityAccountRequest asks to lift an account's community ban.
type UnbanCommunityAccountRequest struct {
	ActorDID, CommunityDID, SubjectDID string
	ExpectedVersion                    int64
	IdempotencyKey                     string
}

// ListCommunityBansRequest asks for a page of a community's bans. Limit zero
// is the default page size.
type ListCommunityBansRequest struct {
	ActorDID, CommunityDID string
	Limit                  int
	Cursor                 string
}

// CommunityBanPage is one page of a community's bans.
type CommunityBanPage struct {
	Bans   []CommunityBanEntry
	Cursor string
}

// CommunityBanEntry is one indexed ban. Active reports no expiry or an expiry
// after the service clock.
type CommunityBanEntry struct {
	SubjectDID, URI, CID, Reason string
	ExpiresAt                    *time.Time
	CreatedAt                    *time.Time
	IndexedAt                    time.Time
	Active                       bool
}

// CommunityAccountBanState is the subject's standing ban row in a community
// (in force or lapsed; nil when none) and the (community, subject DID)
// version.
type CommunityAccountBanState struct {
	Ban     *communities.CommunityBan
	Version int64
}

// CommunityBanLister lists a community's indexed bans, newest first, with the
// cursor of the next page ("" on the last). A malformed cursor is
// ErrInvalidRequest.
type CommunityBanLister interface {
	ListCommunityBans(ctx context.Context, communityDID string, limit int, cursor string) ([]CommunityBanEntry, string, error)
}

// banAction is one validated ban or unban request.
type banAction struct {
	banning                                    bool
	actorDID, communityDID, subjectDID, reason string
	// expiresAt is the ban's expiry in UTC whole seconds, the precision the
	// record carries; nil is no expiry.
	expiresAt       *time.Time
	expectedVersion int64
	key             string
	fingerprint     string
	recordKey       string
	recordURI       string
}

func (a banAction) scope() idempotencyScope {
	return idempotencyScope{actorDID: a.actorDID, communityDID: a.communityDID, key: a.key, fingerprint: a.fingerprint}
}

// BanCommunityAccount bans an account from a community: one commit writes the
// ban record at the subject's derived key (a standing ban is rewritten), and
// the ban index, version and action are applied before it returns. The caller
// must be a moderator of the community or an instance admin, and the subject
// must hold no role there.
func (s *CommunityActionService) BanCommunityAccount(ctx context.Context, req BanCommunityAccountRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if len(req.Reason) > maxCommunityBanReasonBytes || uniseg.GraphemeClusterCount(req.Reason) > maxCommunityBanReasonGraphemes {
		return nil, fmt.Errorf("%w: reason must be at most %d graphemes and %d bytes",
			ErrInvalidRequest, maxCommunityBanReasonGraphemes, maxCommunityBanReasonBytes)
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		normalized := req.ExpiresAt.UTC().Truncate(time.Second)
		// The record carries the expiry as an RFC 3339 UTC timestamp, which
		// has a four-digit year; an offset can push a year-9999 input past it.
		if normalized.Year() > 9999 {
			return nil, fmt.Errorf("%w: expiresAt must be before year 10000 in UTC", ErrInvalidRequest)
		}
		expiresAt = &normalized
	}
	action, err := newBanAction(true, req.ActorDID, req.CommunityDID, req.SubjectDID, req.Reason, expiresAt, req.ExpectedVersion, req.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return s.runBanAction(ctx, action)
}

// UnbanCommunityAccount lifts an account's community ban: one commit deletes
// the ban record, and the ban index, version and action are applied before it
// returns. The caller must be a moderator of the community or an instance
// admin.
func (s *CommunityActionService) UnbanCommunityAccount(ctx context.Context, req UnbanCommunityAccountRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	action, err := newBanAction(false, req.ActorDID, req.CommunityDID, req.SubjectDID, "", nil, req.ExpectedVersion, req.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return s.runBanAction(ctx, action)
}

func newBanAction(banning bool, actorDID, communityDID, subjectDID, reason string, expiresAt *time.Time, expectedVersion int64, key string) (banAction, error) {
	if _, err := syntax.ParseDID(communityDID); err != nil {
		return banAction{}, fmt.Errorf("%w: community must be a DID", ErrInvalidRequest)
	}
	if _, err := syntax.ParseDID(subjectDID); err != nil {
		return banAction{}, fmt.Errorf("%w: subject must be a DID", ErrInvalidRequest)
	}
	if subjectDID == communityDID {
		return banAction{}, fmt.Errorf("%w: a community cannot ban itself", ErrInvalidRequest)
	}
	if err := validateKeyAndVersion(key, expectedVersion); err != nil {
		return banAction{}, err
	}
	operation := communityUnbanOperation
	if banning {
		operation = communityBanOperation
	}
	expiry := ""
	if expiresAt != nil {
		expiry = expiresAt.Format(time.RFC3339)
	}
	payload, err := json.Marshal([6]string{
		operation, communityDID, subjectDID, strconv.FormatInt(expectedVersion, 10), reason, expiry,
	})
	if err != nil {
		return banAction{}, fmt.Errorf("encoding the request fingerprint: %w", err)
	}
	sum := sha256.Sum256(payload)
	recordKey := communities.BanRecordKey(subjectDID)
	return banAction{
		banning:         banning,
		actorDID:        actorDID,
		communityDID:    communityDID,
		subjectDID:      subjectDID,
		reason:          reason,
		expiresAt:       expiresAt,
		expectedVersion: expectedVersion,
		key:             key,
		fingerprint:     hex.EncodeToString(sum[:]),
		recordKey:       recordKey,
		recordURI:       banRecordURI(communityDID, recordKey),
	}, nil
}

// banRecordURI is the AT-URI of a community's ban record.
func banRecordURI(communityDID, recordKey string) string {
	return "at://" + communityDID + "/" + communities.BanCollection + "/" + recordKey
}

// isBanIntent reports whether the intent writes a community ban record.
func isBanIntent(intent CommunityIntent) bool {
	uri, err := syntax.ParseATURI(intent.TargetURI)
	return err == nil && uri.Collection().String() == communities.BanCollection
}

// runBanAction authorizes without locks (a refusal reads nothing more), then
// attempts the action under the roster lock, the actor's lock, the ban
// record's rev-gate row and the subject's ban-subject lock, which a role
// grant takes too, so a ban and a grant of the same account serialize.
func (s *CommunityActionService) runBanAction(ctx context.Context, action banAction) (*CommunityActionResult, error) {
	if _, err := authorizeRole(ctx, s.cfg.Authorizer, action.communityDID, action.actorDID, communities.RoleModerator); err != nil {
		return nil, err
	}
	if s.cfg.Hooks.AfterAuthorize != nil {
		s.cfg.Hooks.AfterAuthorize(ctx)
	}
	lock := newCommunityActionLock(action.communityDID, []string{action.actorDID},
		[]string{action.recordURI}, []string{action.subjectDID})
	return s.runAttempts(lock, action.recordURI, func(lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
		return s.attemptBanAction(ctx, action, lock)
	})
}

// attemptBanAction runs the ban or unban in one action transaction.
func (s *CommunityActionService) attemptBanAction(ctx context.Context, action banAction, lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
	tx, err := s.cfg.Store.BeginAction(ctx, lock)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: opening the community action transaction: %w", ErrModerationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	auth, err := authorizeRole(ctx, s.cfg.Authorizer, action.communityDID, action.actorDID, communities.RoleModerator)
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
	// The expiry is checked after the replay and the own intent's
	// reconciliation, so a retry of a ban whose expiry has since passed gets
	// its result.
	if action.expiresAt != nil && !action.expiresAt.After(now) {
		return nil, nil, fmt.Errorf("%w: expiresAt must be in the future", ErrInvalidRequest)
	}
	other, err := tx.OpenIntentByTarget(ctx, action.recordURI)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the target's open intent: %w", ErrModerationUnavailable, err)
	}
	if other != nil {
		again, err := s.settleOtherIntent(ctx, tx, lock, *other)
		return nil, again, err
	}
	if action.banning {
		if err := s.refuseBanOfRoleHolder(ctx, tx, action); err != nil {
			return nil, nil, err
		}
	}

	state, err := tx.CommunityAccountBan(ctx, action.communityDID, action.subjectDID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the subject's community ban: %w", ErrModerationUnavailable, err)
	}
	if state.Version != action.expectedVersion {
		return nil, nil, fmt.Errorf("%w: the subject's community version is %d, not %d", ErrStateConflict, state.Version, action.expectedVersion)
	}
	inPlace := !action.banning && state.Ban == nil
	if action.banning && state.Ban != nil && state.Ban.Reason == action.reason && sameExpiry(state.Ban.ExpiresAt, action.expiresAt) {
		inPlace = true
	}
	if action.banning && !inPlace && !auth.InstanceAdmin {
		if err := s.checkBanLimits(ctx, tx, action.communityDID, action.actorDID, now); err != nil {
			return nil, nil, err
		}
	}

	repo, err := s.cfg.Repos(ctx, action.communityDID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: opening the community repo: %w", ErrModerationUnavailable, err)
	}
	head, err := repo.GetLatestCommit(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the community repo head: %w", ErrModerationUnavailable, err)
	}
	record, err := readStandRecord(ctx, repo, communities.BanCollection, action.recordKey)
	if err != nil {
		return nil, nil, err
	}
	indexedCID, recordCID := "", ""
	if state.Ban != nil {
		indexedCID = state.Ban.RecordCID
	}
	if record != nil {
		recordCID = record.CID
	}
	if indexedCID != recordCID {
		return nil, nil, fmt.Errorf("%w: the indexed ban is behind the community repo", ErrStateConflict)
	}
	if inPlace {
		result, err := s.banAlreadyApplied(ctx, tx, action, state.Version)
		return result, nil, err
	}

	intent, err := s.newBanIntent(action, auth, head, record, now)
	if err != nil {
		return nil, nil, err
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

// refuseBanOfRoleHolder refuses a ban of an account that holds a role in the
// community (the owner, a moderator, the acting moderator itself) and, while
// an acceptance of the subject's invitation has an unknown outcome, a ban that
// could leave a role holder banned. Both are read under the subject's
// ban-subject lock, which an acceptance holds through its write.
func (s *CommunityActionService) refuseBanOfRoleHolder(ctx context.Context, tx CommunityActionTx, action banAction) error {
	rows, err := tx.ListCommunityRoles(ctx, action.communityDID)
	if err != nil {
		return fmt.Errorf("%w: reading the community roster: %w", ErrModerationUnavailable, err)
	}
	if rosterRow(communities.EffectiveRoster(rows), action.subjectDID) != nil {
		return fmt.Errorf("%w: remove the subject's role in community %s before banning it", ErrSubjectHoldsRole, action.communityDID)
	}
	accept, err := tx.OpenInvitationIntent(ctx, action.communityDID, action.subjectDID)
	if err != nil {
		return fmt.Errorf("%w: reading the subject's open invitation acceptance: %w", ErrModerationUnavailable, err)
	}
	if accept != nil {
		return fmt.Errorf("%w: the subject's acceptance of an invitation has an unknown outcome", ErrStateConflict)
	}
	return nil
}

// sameExpiry reports whether two ban expiries are the same instant, nil being
// no expiry.
func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// checkBanLimits refuses a non-admin ban once the moderator or the community
// has reached its ban limit within the window, a budget separate from
// removals.
func (s *CommunityActionService) checkBanLimits(ctx context.Context, tx CommunityActionTx, communityDID, actorDID string, now time.Time) error {
	limits := s.cfg.BanLimits
	if limits.PerModerator <= 0 && limits.PerCommunity <= 0 {
		return nil
	}
	byActor, byCommunity, err := tx.RecentCommunityBans(ctx, communityDID, actorDID, now.Add(-limits.Window))
	if err != nil {
		return fmt.Errorf("%w: counting recent community bans: %w", ErrModerationUnavailable, err)
	}
	return refuseOverLimits(limits, byActor, byCommunity, "bans")
}

// banAlreadyApplied answers a ban whose identical record already stands, or
// an unban with no ban standing: the result is stored under the key and
// nothing is written.
func (s *CommunityActionService) banAlreadyApplied(ctx context.Context, tx CommunityActionTx, action banAction, version int64) (*CommunityActionResult, error) {
	result := CommunityActionResult{
		CommunityDID:   action.communityDID,
		SubjectDID:     action.subjectDID,
		RecordURI:      action.recordURI,
		Version:        version,
		AlreadyApplied: true,
	}
	record := CommunityIdempotencyRecord{Fingerprint: action.fingerprint, Result: result}
	if err := tx.SaveIdempotencyRecord(ctx, action.actorDID, action.communityDID, action.key, record, s.cfg.IdempotencyTTL); err != nil {
		return nil, fmt.Errorf("%w: storing the idempotency record: %w", ErrModerationUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: committing the community action: %w", ErrModerationUnavailable, err)
	}
	return &result, nil
}

// newBanIntent is the action's intent on the ban record: a ban creates it, or
// updates the standing record (a rewrite replaces its reason, expiry and
// createdAt); an unban deletes it.
func (s *CommunityActionService) newBanIntent(action banAction, auth communities.RoleAuthorization, head *pds.LatestCommit, record *pds.RecordResponse, now time.Time) (CommunityIntent, error) {
	intentID, err := newCommunityIntentID()
	if err != nil {
		return CommunityIntent{}, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	intent := CommunityIntent{
		ID:                   intentID,
		Action:               ActionRestore,
		ActorDID:             action.actorDID,
		AdminActing:          auth.InstanceAdmin,
		CommunityDID:         action.communityDID,
		SubjectDID:           action.subjectDID,
		TargetURI:            action.recordURI,
		Op:                   string(pds.WriteOpDelete),
		GuardCommitCID:       head.CID,
		GuardRev:             head.Rev,
		IdempotencyKey:       action.key,
		Fingerprint:          action.fingerprint,
		State:                CommunityIntentOpen,
		IdempotencyRetention: s.cfg.IdempotencyTTL,
		CreatedAt:            now,
		ExpiresAt:            now.Add(s.cfg.IntentTTL),
	}
	if record != nil {
		intent.ReplacedCID = record.CID
	}
	if !action.banning {
		return intent, nil
	}
	intent.Action = ActionRemove
	intent.Op = string(pds.WriteOpCreate)
	if record != nil {
		intent.Op = string(pds.WriteOpUpdate)
	}
	body := map[string]any{
		"$type":     communities.BanCollection,
		"subject":   action.subjectDID,
		"createdAt": postIntentCreatedAt(now),
	}
	if action.reason != "" {
		body["reason"] = action.reason
	}
	if action.expiresAt != nil {
		body["expiresAt"] = action.expiresAt.Format(time.RFC3339)
	}
	if intent.Record, err = json.Marshal(body); err != nil {
		return CommunityIntent{}, fmt.Errorf("%w: encoding the ban record: %w", ErrModerationUnavailable, err)
	}
	return intent, nil
}

// ListCommunityBans lists a page of a community's indexed bans, in force and
// lapsed, for its moderators and instance admins.
func (s *CommunityActionService) ListCommunityBans(ctx context.Context, req ListCommunityBansRequest) (*CommunityBanPage, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if _, err := syntax.ParseDID(req.CommunityDID); err != nil {
		return nil, fmt.Errorf("%w: community must be a DID", ErrInvalidRequest)
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultCommunityBanListLimit
	}
	if limit < 1 || limit > maxCommunityBanListLimit {
		return nil, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRequest, maxCommunityBanListLimit)
	}
	if len(req.Cursor) > maxCommunityBanCursorBytes {
		return nil, fmt.Errorf("%w: cursor is too long", ErrInvalidRequest)
	}
	if s.cfg.Bans == nil {
		return nil, fmt.Errorf("%w: the community ban list is not configured", ErrModerationUnavailable)
	}
	if _, err := authorizeRole(ctx, s.cfg.Authorizer, req.CommunityDID, req.ActorDID, communities.RoleModerator); err != nil {
		return nil, err
	}
	entries, next, err := s.cfg.Bans.ListCommunityBans(ctx, req.CommunityDID, limit, req.Cursor)
	if err != nil {
		if errors.Is(err, ErrInvalidRequest) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: listing the community's bans: %w", ErrModerationUnavailable, err)
	}
	now := s.cfg.Now()
	for i := range entries {
		entries[i].Active = entries[i].ExpiresAt == nil || entries[i].ExpiresAt.After(now)
	}
	return &CommunityBanPage{Bans: entries, Cursor: next}, nil
}
