package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/rivo/uniseg"
)

// postIntentFirehoseLag is how long past its expiry a post intent whose
// outcome is unknown (its stand record changed to a body its batch did not
// send) waits for its firehose copy before it closes as failed. A copy that
// arrives later is an external change: its action has no actor.
const postIntentFirehoseLag = 30 * time.Minute

// Fingerprint operations of the community post actions.
const (
	removePostOperation  = "remove-post"
	restorePostOperation = "restore-post"
)

// The removal record's reason bounds (removal.json maxGraphemes, maxLength).
const (
	maxCommunityRemovalReasonGraphemes = 1000
	maxCommunityRemovalReasonBytes     = 10000
)

// communityRemovalRequestCodes are the codes a moderator may remove a post
// with: the removal record's known codes plus harassment and doxing. The
// record carries the code verbatim.
var communityRemovalRequestCodes = map[string]struct{}{
	string(posts.DecisionRuleViolation):       {},
	string(posts.DecisionSpam):                {},
	string(posts.DecisionOffTopic):            {},
	string(posts.DecisionIllegalContent):      {},
	string(posts.DecisionAuthorBanned):        {},
	string(posts.DecisionModeratorDiscretion): {},
	"harassment": {},
	"doxing":     {},
}

// communityRemovalActionReasons maps a community removal code onto the
// moderation action reason its action is logged with.
var communityRemovalActionReasons = map[string]string{
	string(posts.DecisionSpam):                "social.coves.moderation.defs#reasonSpam",
	string(posts.DecisionRuleViolation):       "social.coves.moderation.defs#reasonRuleViolation",
	string(posts.DecisionOffTopic):            "social.coves.moderation.defs#reasonRuleViolation",
	"harassment":                              "social.coves.moderation.defs#reasonHarassment",
	"doxing":                                  doxingReason,
	string(posts.DecisionIllegalContent):      illegalContentReason,
	string(posts.DecisionAuthorBanned):        communityRemovalDefaultActionReason,
	string(posts.DecisionModeratorDiscretion): communityRemovalDefaultActionReason,
}

// communityRemovalDefaultActionReason is the reason of a code with no mapping,
// including codes another host's community minted.
const communityRemovalDefaultActionReason = "social.coves.moderation.defs#reasonModeratorDiscretion"

// CommunityRemovalActionReason maps a community removal code onto the
// moderation action reason its action is logged with: local removals,
// firehose-indexed removals and the restores that delete them. An unknown code
// is moderator discretion.
func CommunityRemovalActionReason(code string) string {
	if reason, ok := communityRemovalActionReasons[code]; ok {
		return reason
	}
	return communityRemovalDefaultActionReason
}

// RemovePostRequest asks to remove a post from its community. Code is one of
// the removal codes; Reason is optional.
type RemovePostRequest struct {
	ActorDID, PostURI, Code, Reason string
	ExpectedVersion                 int64
	IdempotencyKey                  string
}

// RestorePostRequest asks to restore a community-removed post.
type RestorePostRequest struct {
	ActorDID, PostURI string
	ExpectedVersion   int64
	IdempotencyKey    string
}

// CommunityPostResolver resolves the community an indexed post is in. It is
// the one read a post action makes before authorization.
type CommunityPostResolver interface {
	// PostCommunity returns the DID of the community the post is indexed in,
	// or ErrPostNotFound.
	PostCommunity(ctx context.Context, postURI string) (string, error)
}

// CommunityRemovalLimits bounds non-admin community post removals: at most
// PerModerator by one moderator and PerCommunity in one community within
// Window. A zero limit is no limit.
type CommunityRemovalLimits struct {
	PerModerator, PerCommunity int
	Window                     time.Duration
}

// CommunityPostState is the index view of a post's community stand records:
// the post's current CID and admission status, the CID a standing acceptance
// pins, the standing removal record's CID, code and reason, and the post
// subject's community record version. RemovalCID is empty when no removal
// stands, and also for a removal indexed before removal records were tracked.
type CommunityPostState struct {
	CommunityDID, CID, Status, PinnedCID   string
	HasAcceptance                          bool
	RemovalCID, RemovalCode, RemovalReason string
	Version                                int64
}

// CommunityIntentAttempts durably records, in its own transaction, each
// batch a post intent's writer is about to send, and reads the latest back. A
// store that records attempts lets a later retry or sweep tell this intent's
// lost commit from a decision that was already in place.
type CommunityIntentAttempts interface {
	RecordIntentAttempt(ctx context.Context, intentID string, attempt posts.CommunityCommitAttempt) error
	// LatestIntentAttempt is the intent's most recently recorded attempt, or
	// nil when none was recorded.
	LatestIntentAttempt(ctx context.Context, intentID string) (*posts.CommunityCommitAttempt, error)
}

// postAction is one validated post remove or restore request.
type postAction struct {
	operation       string
	actorDID        string
	postURI         string
	code, reason    string // remove only
	expectedVersion int64
	key             string
}

func (a postAction) removing() bool { return a.operation == removePostOperation }

// RemovePost removes a post from its community: one commit deletes the
// community's acceptance and writes its removal record, and the admission
// change is applied before it returns. The caller must be a moderator of the
// post's community or an instance admin.
func (s *CommunityActionService) RemovePost(ctx context.Context, req RemovePostRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if err := validateRemovalCodeAndReason(req.Code, req.Reason); err != nil {
		return nil, err
	}
	action := postAction{
		operation: removePostOperation, actorDID: req.ActorDID, postURI: req.PostURI,
		code: req.Code, reason: req.Reason, expectedVersion: req.ExpectedVersion, key: req.IdempotencyKey,
	}
	if err := validatePostAction(action); err != nil {
		return nil, err
	}
	return s.runPostAction(ctx, action)
}

// RestorePost restores a post its community removed: one commit deletes the
// removal record and writes an acceptance pinned to the post's current CID.
// The caller must be a moderator of the post's community or an instance
// admin.
func (s *CommunityActionService) RestorePost(ctx context.Context, req RestorePostRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	action := postAction{
		operation: restorePostOperation, actorDID: req.ActorDID, postURI: req.PostURI,
		expectedVersion: req.ExpectedVersion, key: req.IdempotencyKey,
	}
	if err := validatePostAction(action); err != nil {
		return nil, err
	}
	return s.runPostAction(ctx, action)
}

// validateRemovalCodeAndReason checks a removal request's code and reason
// against the removal record's bounds.
func validateRemovalCodeAndReason(code, reason string) error {
	if _, ok := communityRemovalRequestCodes[code]; !ok {
		return fmt.Errorf("%w: code %q is not a community removal code", ErrInvalidRequest, code)
	}
	if len(reason) > maxCommunityRemovalReasonBytes || uniseg.GraphemeClusterCount(reason) > maxCommunityRemovalReasonGraphemes {
		return fmt.Errorf("%w: reason must be at most %d graphemes and %d bytes",
			ErrInvalidRequest, maxCommunityRemovalReasonGraphemes, maxCommunityRemovalReasonBytes)
	}
	return nil
}

func validatePostAction(action postAction) error {
	uri, err := syntax.ParseATURI(action.postURI)
	if err != nil || !uri.Authority().IsDID() || uri.RecordKey().String() == "" {
		return fmt.Errorf("%w: subject must be a post record URI with a DID authority", ErrInvalidRequest)
	}
	return validateKeyAndVersion(action.key, action.expectedVersion)
}

// postFingerprint is the hex SHA-256 of every input of a post action.
func postFingerprint(action postAction, communityDID string) (string, error) {
	return removalActionFingerprint(action.operation, communityDID, action.postURI, action.expectedVersion, action.code, action.reason)
}

// removalActionFingerprint is the hex SHA-256 of every input of a community
// post or comment remove or restore.
func removalActionFingerprint(operation, communityDID, subjectURI string, expectedVersion int64, code, reason string) (string, error) {
	payload, err := json.Marshal([6]string{
		operation, communityDID, subjectURI, strconv.FormatInt(expectedVersion, 10), code, reason,
	})
	if err != nil {
		return "", fmt.Errorf("encoding the request fingerprint: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// resolvedPostAction is a post action with the community it acts in.
type resolvedPostAction struct {
	postAction
	communityDID  string
	fingerprint   string
	rkey          string
	removalURI    string
	acceptanceURI string
}

func (a resolvedPostAction) scope() idempotencyScope {
	return idempotencyScope{actorDID: a.actorDID, communityDID: a.communityDID, key: a.key, fingerprint: a.fingerprint}
}

// runPostAction resolves the post's community (the one read before
// authorization), authorizes without locks, then attempts the action under
// the community's roster lock, the actor's lock and both stand records' locks.
func (s *CommunityActionService) runPostAction(ctx context.Context, action postAction) (*CommunityActionResult, error) {
	if s.cfg.Posts == nil {
		return nil, fmt.Errorf("%w: community post actions are not configured", ErrModerationUnavailable)
	}
	communityDID, err := s.cfg.Posts.PostCommunity(ctx, action.postURI)
	if err != nil {
		if errors.Is(err, ErrPostNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: resolving the post's community: %w", ErrModerationUnavailable, err)
	}
	if _, err := authorizeRole(ctx, s.cfg.Authorizer, communityDID, action.actorDID, communities.RoleModerator); err != nil {
		return nil, err
	}
	if s.cfg.Hooks.AfterAuthorize != nil {
		s.cfg.Hooks.AfterAuthorize(ctx)
	}

	fingerprint, err := postFingerprint(action, communityDID)
	if err != nil {
		return nil, err
	}
	rkey := posts.SubjectRkey(action.postURI)
	resolved := resolvedPostAction{
		postAction:    action,
		communityDID:  communityDID,
		fingerprint:   fingerprint,
		rkey:          rkey,
		removalURI:    "at://" + communityDID + "/" + posts.RemovalCollection + "/" + rkey,
		acceptanceURI: "at://" + communityDID + "/" + posts.AcceptanceCollection + "/" + rkey,
	}
	// The roster lock serializes the per-community removal count with the
	// intent that reserves a removal against it.
	lock := newCommunityActionLock(communityDID, []string{action.actorDID},
		[]string{resolved.removalURI, resolved.acceptanceURI}, nil)
	return s.runAttempts(lock, resolved.removalURI, func(lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
		return s.attemptPostAction(ctx, resolved, lock)
	})
}

// attemptPostAction runs the post action in one action transaction.
func (s *CommunityActionService) attemptPostAction(ctx context.Context, action resolvedPostAction, lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
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
	other, err := tx.OpenIntentByTarget(ctx, action.removalURI)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the target's open intent: %w", ErrModerationUnavailable, err)
	}
	if other != nil {
		again, err := s.settleOtherIntent(ctx, tx, lock, *other)
		return nil, again, err
	}

	state, err := tx.CommunityPost(ctx, action.postURI)
	if err != nil {
		if errors.Is(err, ErrPostNotFound) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: reading the post's community state: %w", ErrModerationUnavailable, err)
	}
	if state.CommunityDID != action.communityDID {
		return nil, nil, fmt.Errorf("%w: the post moved to another community while the request was authorized", ErrStateConflict)
	}
	if state.Version != action.expectedVersion {
		return nil, nil, fmt.Errorf("%w: the post's community version is %d, not %d", ErrStateConflict, state.Version, action.expectedVersion)
	}
	if action.removing() && !auth.InstanceAdmin {
		if err := s.checkRemovalLimits(ctx, tx, action.communityDID, action.actorDID, now); err != nil {
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
	acceptance, err := readStandRecord(ctx, repo, posts.AcceptanceCollection, action.rkey)
	if err != nil {
		return nil, nil, err
	}
	removal, err := readStandRecord(ctx, repo, posts.RemovalCollection, action.rkey)
	if err != nil {
		return nil, nil, err
	}
	if err := checkStandRecordsIndexed(state, acceptance, removal); err != nil {
		return nil, nil, err
	}
	if !action.removing() && removal == nil && (acceptance == nil || recordSubjectCID(acceptance) != state.CID) {
		return nil, nil, fmt.Errorf("%w: no community removal stands for the post, so there is nothing to restore", ErrStateConflict)
	}

	removalCreatedAt := ""
	if action.removing() {
		if removalCreatedAt, err = s.removalCreatedAt(ctx, tx, action.communityDID, action.postURI, removal, now); err != nil {
			return nil, nil, err
		}
	}
	intent, err := s.newPostIntent(action, auth, state, head, acceptance, removal, removalCreatedAt, now)
	if err != nil {
		return nil, nil, err
	}
	if err := s.cfg.Store.SaveIntent(ctx, intent); err != nil {
		if errors.Is(err, ErrStateConflict) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: storing the community write intent: %w", ErrModerationUnavailable, err)
	}
	result, err := s.writePostIntent(ctx, tx, intent, &state)
	return result, nil, err
}

// checkRemovalLimits refuses a non-admin post or comment removal once the
// moderator or the community has reached its limit within the window. Post
// and comment removals logged and removal intents still open all count.
func (s *CommunityActionService) checkRemovalLimits(ctx context.Context, tx CommunityActionTx, communityDID, actorDID string, now time.Time) error {
	limits := s.cfg.RemovalLimits
	if limits.PerModerator <= 0 && limits.PerCommunity <= 0 {
		return nil
	}
	byActor, byCommunity, err := tx.RecentCommunityRemovals(ctx, communityDID, actorDID, now.Add(-limits.Window))
	if err != nil {
		return fmt.Errorf("%w: counting recent community removals: %w", ErrModerationUnavailable, err)
	}
	return refuseOverLimits(limits, byActor, byCommunity, "removals")
}

// refuseOverLimits is RateLimited once the moderator's or the community's
// count of what (removals, bans) within the window reaches its limit.
func refuseOverLimits(limits CommunityRemovalLimits, byActor, byCommunity int, what string) error {
	if limits.PerModerator > 0 && byActor >= limits.PerModerator {
		return fmt.Errorf("%w: the moderator has made %d %s in the last %s", ErrRateLimited, byActor, what, limits.Window)
	}
	if limits.PerCommunity > 0 && byCommunity >= limits.PerCommunity {
		return fmt.Errorf("%w: the community has made %d %s in the last %s", ErrRateLimited, byCommunity, what, limits.Window)
	}
	return nil
}

// readStandRecord reads a stand record from the community repo, nil when it
// is absent.
func readStandRecord(ctx context.Context, repo posts.CommunityRepo, collection, rkey string) (*pds.RecordResponse, error) {
	record, err := repo.GetRecord(ctx, collection, rkey)
	if errors.Is(err, pds.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s/%s from the community repo: %w", ErrModerationUnavailable, collection, rkey, err)
	}
	return record, nil
}

// recordSubjectCID is the CID a stand record's strongRef subject pins.
func recordSubjectCID(record *pds.RecordResponse) string {
	subject, _ := record.Value["subject"].(map[string]any)
	cid, _ := subject["cid"].(string)
	return cid
}

// recordString is a string field of a record, "" when absent.
func recordString(record *pds.RecordResponse, field string) string {
	value, _ := record.Value[field].(string)
	return value
}

// checkStandRecordsIndexed compares the stand records the community repo
// holds with what the index holds: an acceptance stands in the repo exactly
// when the index has one, pinning the same CID, and the removal record's CID
// is the indexed one. A removal indexed before removal records were tracked
// has no indexed CID and accepts whatever removal the repo holds. A mismatch
// is StateConflict: the index is behind the repo.
func checkStandRecordsIndexed(state CommunityPostState, acceptance, removal *pds.RecordResponse) error {
	if (acceptance != nil) != state.HasAcceptance ||
		(acceptance != nil && recordSubjectCID(acceptance) != state.PinnedCID) {
		return fmt.Errorf("%w: the indexed acceptance is behind the community repo", ErrStateConflict)
	}
	switch {
	case state.RemovalCID != "":
		if removal == nil || removal.CID != state.RemovalCID {
			return fmt.Errorf("%w: the indexed removal is behind the community repo", ErrStateConflict)
		}
	case removal != nil && state.Status != string(posts.AdmissionStatusRemoved):
		return fmt.Errorf("%w: a removal stands in the community repo that the index has not applied", ErrStateConflict)
	}
	return nil
}

// newPostIntent is the action's intent. Its target is the removal record: a
// remove creates or updates it and deletes the acceptance (the paired
// target); a restore deletes it and writes the acceptance pinned to the
// post's current CID (the paired record). Each body is the one the writer
// sends: the removal record carries removalCreatedAt, and a new acceptance the
// intent's createdAt while an updated one keeps the standing record's.
func (s *CommunityActionService) newPostIntent(action resolvedPostAction, auth communities.RoleAuthorization, state CommunityPostState, head *pds.LatestCommit, acceptance, removal *pds.RecordResponse, removalCreatedAt string, now time.Time) (CommunityIntent, error) {
	intentID, err := newCommunityIntentID()
	if err != nil {
		return CommunityIntent{}, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	intent := CommunityIntent{
		ID:                   intentID,
		ActorDID:             action.actorDID,
		AdminActing:          auth.InstanceAdmin,
		CommunityDID:         action.communityDID,
		TargetURI:            action.removalURI,
		PairedTargetURI:      action.acceptanceURI,
		GuardCommitCID:       head.CID,
		GuardRev:             head.Rev,
		IdempotencyKey:       action.key,
		Fingerprint:          action.fingerprint,
		State:                CommunityIntentOpen,
		IdempotencyRetention: s.cfg.IdempotencyTTL,
		CreatedAt:            now,
		ExpiresAt:            now.Add(s.cfg.IntentTTL),
	}
	createdAt := postIntentCreatedAt(now)
	if removal != nil {
		intent.ReplacedCID = removal.CID
	}
	if acceptance != nil {
		intent.PairedReplacedCID = acceptance.CID
	}
	if action.removing() {
		intent.Action = ActionRemove
		intent.Op = string(pds.WriteOpCreate)
		if removal != nil {
			intent.Op = string(pds.WriteOpUpdate)
		}
		body := map[string]any{
			"$type":     posts.RemovalCollection,
			"subject":   map[string]any{"uri": action.postURI, "cid": state.CID},
			"code":      action.code,
			"createdAt": removalCreatedAt,
		}
		if action.reason != "" {
			body["reason"] = action.reason
		}
		intent.Record, err = json.Marshal(body)
	} else {
		intent.Action = ActionRestore
		intent.Op = string(pds.WriteOpDelete)
		if acceptance != nil {
			createdAt = standingCreatedAt(acceptance, createdAt)
		}
		intent.PairedRecord, err = json.Marshal(map[string]any{
			"$type":     posts.AcceptanceCollection,
			"subject":   map[string]any{"uri": action.postURI, "cid": state.CID},
			"createdAt": createdAt,
		})
	}
	if err != nil {
		return CommunityIntent{}, fmt.Errorf("%w: encoding the intent's stand record: %w", ErrModerationUnavailable, err)
	}
	return intent, nil
}

// postIntentCreatedAt is the createdAt a stand record the intent creates
// carries: the intent's own time.
func postIntentCreatedAt(now time.Time) string {
	return now.UTC().Format(time.RFC3339)
}

// removalCreatedAt is the createdAt a removal intent's record carries: the
// intent's own time for a new record, and over a standing removal the
// standing record's, which an update keeps, unless the instance removal
// outbox (migration 080) wrote that record. A removal the outbox wrote is
// taken over with the intent's time, distinct from the outbox's createdAt, so
// it is no longer the outbox's by CID or by content and the instance restore
// leaves the community's removal standing (an identical record would be a
// skip, answered as already applied, and stay the outbox's).
func (s *CommunityActionService) removalCreatedAt(ctx context.Context, tx CommunityActionTx, communityDID, subjectURI string, removal *pds.RecordResponse, now time.Time) (string, error) {
	createdAt := postIntentCreatedAt(now)
	if removal == nil {
		return createdAt, nil
	}
	standing := posts.StandingRemoval{
		CID: removal.CID, SubjectURI: subjectURI, SubjectCID: recordSubjectCID(removal),
		Code: recordString(removal, "code"), Reason: recordString(removal, "reason"),
		CreatedAt: recordString(removal, "createdAt"),
	}
	outboxWrote, err := tx.InstanceOutboxWroteRemoval(ctx, communityDID, standing)
	if err != nil {
		return "", fmt.Errorf("%w: reading whether the instance outbox wrote the removal: %w", ErrModerationUnavailable, err)
	}
	if !outboxWrote {
		return standingCreatedAt(removal, createdAt), nil
	}
	if createdAt == standing.CreatedAt {
		createdAt = postIntentCreatedAt(now.Add(time.Second))
	}
	return createdAt, nil
}

// standingCreatedAt is a standing record's createdAt, which an update keeps,
// or fallback when it has none.
func standingCreatedAt(record *pds.RecordResponse, fallback string) string {
	if createdAt := recordString(record, "createdAt"); createdAt != "" {
		return createdAt
	}
	return fallback
}

// isPostIntent reports whether the intent is a community post action's: its
// target is a removal record and it is not a comment action's.
func isPostIntent(intent CommunityIntent) bool {
	uri, err := syntax.ParseATURI(intent.TargetURI)
	return err == nil && uri.Collection().String() == posts.RemovalCollection && !isCommentIntent(intent)
}

// postIntentStand is what a post intent makes stand, read back from the
// intent: the stand record's collection and key, its exact body, and the
// post, CID, code and reason the body names. A remove makes its removal
// record stand (its target), a restore its acceptance (the paired target).
type postIntentStand struct {
	removing         bool
	collection, rkey string
	body             json.RawMessage
	postURI, postCID string
	code, reason     string
	createdAt        string
}

func readPostIntentStand(intent CommunityIntent) (postIntentStand, error) {
	stand := postIntentStand{removing: intent.Op != string(pds.WriteOpDelete)}
	uri := intent.PairedTargetURI
	stand.collection, stand.body = posts.AcceptanceCollection, intent.PairedRecord
	if stand.removing {
		uri = intent.TargetURI
		stand.collection, stand.body = posts.RemovalCollection, intent.Record
	}
	parsed, err := syntax.ParseATURI(uri)
	if err != nil || parsed.Collection().String() != stand.collection || parsed.RecordKey() == "" {
		return postIntentStand{}, fmt.Errorf("%w: post intent %s names no %s record", ErrModerationUnavailable, intent.ID, stand.collection)
	}
	stand.rkey = parsed.RecordKey().String()
	var record struct {
		Subject struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"subject"`
		Code      string `json:"code"`
		Reason    string `json:"reason"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(stand.body, &record); err != nil {
		return postIntentStand{}, fmt.Errorf("%w: decoding post intent %s's stand record: %w", ErrModerationUnavailable, intent.ID, err)
	}
	if record.Subject.URI == "" || record.Subject.CID == "" {
		return postIntentStand{}, fmt.Errorf("%w: post intent %s's stand record names no subject", ErrModerationUnavailable, intent.ID)
	}
	stand.postURI, stand.postCID = record.Subject.URI, record.Subject.CID
	stand.code, stand.reason, stand.createdAt = record.Code, record.Reason, record.CreatedAt
	return stand, nil
}

// latestAttempt is the latest batch attempt recorded for the intent, nil when
// none was recorded or the store records none.
func (s *CommunityActionService) latestAttempt(ctx context.Context, intentID string) (*posts.CommunityCommitAttempt, error) {
	if s.attempts == nil {
		return nil, nil
	}
	attempt, err := s.attempts.LatestIntentAttempt(ctx, intentID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the community write attempt: %w", ErrModerationUnavailable, err)
	}
	return attempt, nil
}

// writePostIntent writes the intent through the community record writer,
// recording each batch's attempt before it is sent, then applies the local
// effect. state is the post's state read in this transaction, nil for a
// retry, which reads it only if it needs it. A writer error leaves the outcome
// unknown: the action transaction rolls back and the intent stays open for
// reconciliation.
func (s *CommunityActionService) writePostIntent(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, state *CommunityPostState) (*CommunityActionResult, error) {
	stand, err := readPostIntentStand(intent)
	if err != nil {
		return nil, err
	}
	if s.cfg.Hooks.BeforeWrite != nil {
		s.cfg.Hooks.BeforeWrite(ctx)
	}
	var lastAttempt *posts.CommunityCommitAttempt
	beforeCommit := func(ctx context.Context, attempt posts.CommunityCommitAttempt) error {
		if s.attempts != nil {
			if err := s.attempts.RecordIntentAttempt(ctx, intent.ID, attempt); err != nil {
				return fmt.Errorf("%w: recording the community write attempt: %w", ErrModerationUnavailable, err)
			}
		}
		lastAttempt = &attempt
		return nil
	}
	createdAt := postIntentCreatedAt(intent.CreatedAt)
	var written posts.CommunityWriteResult
	if stand.removing {
		written, err = s.writer.WriteRemoval(ctx, posts.CommunityRemovalCommand{
			CommunityDID: intent.CommunityDID, PostURI: stand.postURI, PostCID: stand.postCID,
			Code: posts.DecisionCode(stand.code), Reason: stand.reason,
			CreatedAt: stand.createdAt, ReplaceCreatedAtOf: intent.ReplacedCID, BeforeCommit: beforeCommit,
		})
	} else {
		written, err = s.writer.RestoreAcceptance(ctx, posts.CommunityWriteCommand{
			CommunityDID: intent.CommunityDID, PostURI: stand.postURI, PostCID: stand.postCID,
			CreatedAt: createdAt, BeforeCommit: beforeCommit,
		})
	}
	if err != nil {
		if errors.Is(err, ErrModerationUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: writing the community's stand records: %w", ErrModerationUnavailable, err)
	}

	commit := CommunityCommit{Rev: written.Rev, GateRev: written.Rev, CompletedAt: s.cfg.Now()}
	if written.Skipped {
		// An earlier request on this intent may have sent the batch whose
		// record the writer found standing: the durable attempts say.
		if lastAttempt == nil {
			if lastAttempt, err = s.latestAttempt(ctx, intent.ID); err != nil {
				return nil, err
			}
		}
		if lastAttempt == nil || written.CID == lastAttempt.StandCID {
			return s.alreadyApplied(ctx, tx, intent, stand, state, written.URI)
		}
		// A batch of this intent committed but its response was lost; the
		// writer's re-read found the record it wrote. Its commit rev is
		// unknown, and the skip's head rev must not stand for it: the gate
		// takes the attempt's guard rev, so the firehose still applies any
		// later change.
		commit.Rev, commit.GateRev = lastAttempt.GuardRev, lastAttempt.GuardRev
	}
	if stand.removing {
		commit.RecordCID = written.CID
	} else {
		commit.PairedRecordCID = written.CID
	}
	return s.complete(ctx, tx, intent, commit)
}

// alreadyApplied answers a request whose decision was already in place when
// the writer found nothing to write: the result is stored under the key, the
// intent closes without an action, and nothing in the index changes.
func (s *CommunityActionService) alreadyApplied(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, stand postIntentStand, state *CommunityPostState, recordURI string) (*CommunityActionResult, error) {
	if state == nil {
		current, err := tx.CommunityPost(ctx, stand.postURI)
		if err != nil {
			if errors.Is(err, ErrPostNotFound) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: reading the post's community state: %w", ErrModerationUnavailable, err)
		}
		state = &current
	}
	return s.alreadyAppliedAt(ctx, tx, intent, recordURI, state.Version)
}

// alreadyAppliedAt stores the already-applied result at the subject's current
// version under the intent's key and closes the intent without an action.
func (s *CommunityActionService) alreadyAppliedAt(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, recordURI string, version int64) (*CommunityActionResult, error) {
	result := CommunityActionResult{
		CommunityDID:   intent.CommunityDID,
		RecordURI:      recordURI,
		Version:        version,
		AlreadyApplied: true,
	}
	record := CommunityIdempotencyRecord{Fingerprint: intent.Fingerprint, Result: result}
	if err := tx.SaveIdempotencyRecord(ctx, intent.ActorDID, intent.CommunityDID, intent.IdempotencyKey, record, s.cfg.IdempotencyTTL); err != nil {
		return nil, fmt.Errorf("%w: storing the idempotency record: %w", ErrModerationUnavailable, err)
	}
	// The intent wrote nothing, so it closes as failed: no commit of its own
	// exists for anything to complete with.
	if err := s.finish(ctx, tx, intent, CommunityIntentFailed); err != nil {
		return nil, err
	}
	return &result, nil
}

// reconcilePostIntent settles an open post intent from the community repo,
// judged against the latest batch attempt recorded for it (its guard, the
// stand record's CID before the batch and the body the batch wrote). While
// the head is still at that guard nothing of the intent's committed: a
// same-key retry writes again, another key's request is refused while the
// intent is unexpired, and an expired intent expires (restart reports that a
// retry must replace it). Once the head has moved, the intent committed when
// its stand record holds the attempt's body under a CID other than the one
// before the batch, and completes with its actor at the attempt's guard rev;
// it did not commit when no batch was attempted or the stand record is as the
// attempt found it, and fails (a retry gets StateConflict); otherwise the
// record changed again after the write, and the intent stays open for its
// firehose copy (ModerationUnavailable) until postIntentFirehoseLag past its
// expiry, when it fails (a retry gets StateConflict).
func (s *CommunityActionService) reconcilePostIntent(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, retry bool) (*CommunityActionResult, bool, error) {
	stand, err := readPostIntentStand(intent)
	if err != nil {
		return nil, false, err
	}
	repo, err := s.cfg.Repos(ctx, intent.CommunityDID)
	if err != nil {
		return nil, false, fmt.Errorf("%w: opening the community repo: %w", ErrModerationUnavailable, err)
	}
	head, err := repo.GetLatestCommit(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("%w: reading the community repo head: %w", ErrModerationUnavailable, err)
	}
	attempt, err := s.latestAttempt(ctx, intent.ID)
	if err != nil {
		return nil, false, err
	}
	guard := intent.GuardCommitCID
	if attempt != nil {
		guard = attempt.GuardCommitCID
	}
	if head.CID == guard {
		if s.cfg.Now().Before(intent.ExpiresAt) {
			if retry {
				result, err := s.writePostIntent(ctx, tx, intent, nil)
				return result, false, err
			}
			// Another key's unexpired write may still commit on the PDS:
			// expiring it now would orphan that commit from its actor.
			return nil, false, fmt.Errorf("%w: another request's write to the post's stand records has an unknown outcome", ErrStateConflict)
		}
		if err := s.finish(ctx, tx, intent, CommunityIntentExpired); err != nil {
			return nil, false, err
		}
		return nil, retry, nil
	}

	committed, unwritten := false, attempt == nil
	var record *pds.RecordResponse
	if attempt != nil {
		if record, err = readStandRecord(ctx, repo, stand.collection, stand.rkey); err != nil {
			return nil, false, err
		}
		if record != nil && record.CID != attempt.StandCID {
			if committed, err = sameRecordBody(record.Value, attempt.StandRecord); err != nil {
				return nil, false, err
			}
		}
		unwritten = (record == nil && attempt.StandCID == "") || (record != nil && record.CID == attempt.StandCID)
	}
	switch {
	case committed:
		commit := CommunityCommit{Rev: attempt.GuardRev, GateRev: attempt.GuardRev, CompletedAt: s.cfg.Now()}
		if stand.removing {
			commit.RecordCID = record.CID
		} else {
			commit.PairedRecordCID = record.CID
		}
		result, err := s.complete(ctx, tx, intent, commit)
		return result, false, err
	case !unwritten:
		return nil, false, s.awaitFirehoseCopy(ctx, tx, intent, retry, attempt, record)
	}
	return nil, false, s.failUnwritten(ctx, tx, intent, retry)
}

// awaitFirehoseCopy settles a post or comment intent whose stand record
// changed after its batch: whether the batch committed is unknown until its
// firehose copy arrives (ModerationUnavailable), and once postIntentFirehoseLag
// past its expiry has passed without one the intent fails (a retry gets
// StateConflict). record is the stand record as read now, nil when absent.
func (s *CommunityActionService) awaitFirehoseCopy(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, retry bool, attempt *posts.CommunityCommitAttempt, record *pds.RecordResponse) error {
	if s.cfg.Now().Before(intent.ExpiresAt.Add(postIntentFirehoseLag)) {
		return fmt.Errorf("%w: the stand record changed after the write, so whether it committed is unknown until its firehose copy arrives", ErrModerationUnavailable)
	}
	// No firehose copy matched within the lag bound: close the intent so it
	// stops blocking the subject. A copy arriving later is logged as an
	// external change, without the actor.
	standCID := ""
	if record != nil {
		standCID = record.CID
	}
	slog.Error("community intent closed with an unknown outcome: no firehose copy matched its batch within the lag bound",
		"intent_id", intent.ID, "community_did", intent.CommunityDID, "target_uri", intent.TargetURI,
		"guard_rev", attempt.GuardRev, "stand_cid", standCID)
	if err := s.finish(ctx, tx, intent, CommunityIntentFailed); err != nil {
		return err
	}
	if !retry {
		return nil
	}
	return fmt.Errorf("%w: no firehose copy of the write arrived in time, so it is treated as not written", ErrStateConflict)
}

// failUnwritten fails an intent whose batch the moved repo shows was not
// written; a retry gets StateConflict.
func (s *CommunityActionService) failUnwritten(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, retry bool) error {
	if err := s.finish(ctx, tx, intent, CommunityIntentFailed); err != nil {
		return err
	}
	if !retry {
		return nil
	}
	return fmt.Errorf("%w: the community repo moved and the intent's records were not written", ErrStateConflict)
}
