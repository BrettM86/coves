package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Fingerprint operations of the community comment actions.
const (
	removeCommentOperation  = "remove-comment"
	restoreCommentOperation = "restore-comment"
)

// RemoveCommentRequest asks to remove a comment from its community. Code is
// one of the removal codes; Reason is optional.
type RemoveCommentRequest struct {
	ActorDID, CommentURI, Code, Reason string
	ExpectedVersion                    int64
	IdempotencyKey                     string
}

// RestoreCommentRequest asks to restore a community-removed comment.
type RestoreCommentRequest struct {
	ActorDID, CommentURI string
	ExpectedVersion      int64
	IdempotencyKey       string
}

// CommunityCommentResolver resolves the community of the indexed thread root
// post a comment's parent chain reaches. It is the one read a comment action
// makes before authorization.
type CommunityCommentResolver interface {
	// CommentCommunity returns the community DID, or ErrCommentNotFound.
	CommentCommunity(ctx context.Context, commentURI string) (string, error)
}

// CommunityCommentState is the index view of a comment's community removal:
// the community its parent chain reaches, the comment's CID, the standing
// community removal record's CID, code and reason, whether that removal is
// applied, and the comment subject's community record version.
type CommunityCommentState struct {
	CommunityDID, CID                      string
	RemovalCID, RemovalCode, RemovalReason string
	Removed                                bool
	Version                                int64
}

// commentAction is one validated comment remove or restore request, with the
// community its comment's parent chain reached.
type commentAction struct {
	operation       string
	actorDID        string
	commentURI      string
	code, reason    string // remove only
	expectedVersion int64
	key             string

	communityDID string
	fingerprint  string
	rkey         string
	removalURI   string
}

func (a commentAction) removing() bool { return a.operation == removeCommentOperation }

func (a commentAction) scope() idempotencyScope {
	return idempotencyScope{actorDID: a.actorDID, communityDID: a.communityDID, key: a.key, fingerprint: a.fingerprint}
}

// RemoveComment removes a comment from its community: one commit writes the
// community's removal record about it, touching no acceptance, and the
// removal is applied before it returns. The caller must be a moderator of the
// community the comment's parent chain reaches or an instance admin.
func (s *CommunityActionService) RemoveComment(ctx context.Context, req RemoveCommentRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if err := validateRemovalCodeAndReason(req.Code, req.Reason); err != nil {
		return nil, err
	}
	action := commentAction{
		operation: removeCommentOperation, actorDID: req.ActorDID, commentURI: req.CommentURI,
		code: req.Code, reason: req.Reason, expectedVersion: req.ExpectedVersion, key: req.IdempotencyKey,
	}
	if err := validateCommentAction(action); err != nil {
		return nil, err
	}
	return s.runCommentAction(ctx, action)
}

// RestoreComment restores a comment its community removed: one commit
// deletes the community's removal record about it. With no removal standing
// the restore is already applied. The caller must be a moderator of the
// comment's community or an instance admin.
func (s *CommunityActionService) RestoreComment(ctx context.Context, req RestoreCommentRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	action := commentAction{
		operation: restoreCommentOperation, actorDID: req.ActorDID, commentURI: req.CommentURI,
		expectedVersion: req.ExpectedVersion, key: req.IdempotencyKey,
	}
	if err := validateCommentAction(action); err != nil {
		return nil, err
	}
	return s.runCommentAction(ctx, action)
}

func validateCommentAction(action commentAction) error {
	uri, err := syntax.ParseATURI(action.commentURI)
	if err != nil || !uri.Authority().IsDID() || uri.Collection().String() != CommentCollection || uri.RecordKey().String() == "" {
		return fmt.Errorf("%w: subject must be a %s record URI with a DID authority", ErrInvalidRequest, CommentCollection)
	}
	return validateKeyAndVersion(action.key, action.expectedVersion)
}

// isCommentIntent reports whether the intent is a community comment action's:
// its subject is a comment.
func isCommentIntent(intent CommunityIntent) bool {
	if intent.SubjectURI == "" {
		return false
	}
	uri, err := syntax.ParseATURI(intent.SubjectURI)
	return err == nil && uri.Collection().String() == CommentCollection
}

// runCommentAction resolves the comment's community through its parent chain
// (the one read before authorization), authorizes without locks, then
// attempts the action under the community's roster lock, the actor's lock and
// the removal record's lock.
func (s *CommunityActionService) runCommentAction(ctx context.Context, action commentAction) (*CommunityActionResult, error) {
	if s.cfg.Comments == nil {
		return nil, fmt.Errorf("%w: community comment actions are not configured", ErrModerationUnavailable)
	}
	communityDID, err := s.cfg.Comments.CommentCommunity(ctx, action.commentURI)
	if err != nil {
		if errors.Is(err, ErrCommentNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: resolving the comment's community: %w", ErrModerationUnavailable, err)
	}
	if _, err := authorizeRole(ctx, s.cfg.Authorizer, communityDID, action.actorDID, communities.RoleModerator); err != nil {
		return nil, err
	}
	if s.cfg.Hooks.AfterAuthorize != nil {
		s.cfg.Hooks.AfterAuthorize(ctx)
	}

	action.communityDID = communityDID
	if action.fingerprint, err = removalActionFingerprint(action.operation, communityDID, action.commentURI,
		action.expectedVersion, action.code, action.reason); err != nil {
		return nil, err
	}
	action.rkey = posts.SubjectRkey(action.commentURI)
	action.removalURI = "at://" + communityDID + "/" + posts.RemovalCollection + "/" + action.rkey
	// The roster lock serializes the per-community removal count with the
	// intent that reserves a removal against it.
	lock := newCommunityActionLock(communityDID, []string{action.actorDID}, []string{action.removalURI}, nil)
	return s.runAttempts(lock, action.removalURI, func(lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
		return s.attemptCommentAction(ctx, action, lock)
	})
}

// attemptCommentAction runs the comment action in one action transaction.
func (s *CommunityActionService) attemptCommentAction(ctx context.Context, action commentAction, lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
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

	state, err := s.commentState(ctx, tx, action.commentURI)
	if err != nil {
		return nil, nil, err
	}
	if state.CommunityDID != action.communityDID {
		return nil, nil, fmt.Errorf("%w: the comment's thread moved to another community while the request was authorized", ErrStateConflict)
	}
	if state.Version != action.expectedVersion {
		return nil, nil, fmt.Errorf("%w: the comment's community version is %d, not %d", ErrStateConflict, state.Version, action.expectedVersion)
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
	removal, err := readStandRecord(ctx, repo, posts.RemovalCollection, action.rkey)
	if err != nil {
		return nil, nil, err
	}
	standingCID := ""
	if removal != nil {
		standingCID = removal.CID
	}
	if standingCID != state.RemovalCID {
		return nil, nil, fmt.Errorf("%w: the indexed comment removal is behind the community repo", ErrStateConflict)
	}

	removalCreatedAt := ""
	if action.removing() {
		if removalCreatedAt, err = s.removalCreatedAt(ctx, tx, action.communityDID, action.commentURI, removal, now); err != nil {
			return nil, nil, err
		}
	}
	intent, err := s.newCommentIntent(action, auth, state, head, removal, removalCreatedAt, now)
	if err != nil {
		return nil, nil, err
	}
	if err := s.cfg.Store.SaveIntent(ctx, intent); err != nil {
		if errors.Is(err, ErrStateConflict) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: storing the community write intent: %w", ErrModerationUnavailable, err)
	}
	result, err := s.writeCommentIntent(ctx, tx, intent, &state)
	return result, nil, err
}

// commentState reads the comment's community removal state in the action
// transaction.
func (s *CommunityActionService) commentState(ctx context.Context, tx CommunityActionTx, commentURI string) (CommunityCommentState, error) {
	state, err := tx.CommunityComment(ctx, commentURI)
	if err != nil {
		if errors.Is(err, ErrCommentNotFound) {
			return CommunityCommentState{}, err
		}
		return CommunityCommentState{}, fmt.Errorf("%w: reading the comment's community state: %w", ErrModerationUnavailable, err)
	}
	return state, nil
}

// newCommentIntent is the action's intent. Its target is the removal record
// and its subject the comment: a remove creates or updates the record,
// carrying removalCreatedAt, and a restore deletes it.
func (s *CommunityActionService) newCommentIntent(action commentAction, auth communities.RoleAuthorization, state CommunityCommentState, head *pds.LatestCommit, removal *pds.RecordResponse, removalCreatedAt string, now time.Time) (CommunityIntent, error) {
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
		SubjectURI:           action.commentURI,
		GuardCommitCID:       head.CID,
		GuardRev:             head.Rev,
		IdempotencyKey:       action.key,
		Fingerprint:          action.fingerprint,
		State:                CommunityIntentOpen,
		IdempotencyRetention: s.cfg.IdempotencyTTL,
		CreatedAt:            now,
		ExpiresAt:            now.Add(s.cfg.IntentTTL),
	}
	if removal != nil {
		intent.ReplacedCID = removal.CID
	}
	if !action.removing() {
		intent.Action = ActionRetractRemoval
		intent.Op = string(pds.WriteOpDelete)
		return intent, nil
	}
	intent.Action = ActionApplyRemoval
	intent.Op = string(pds.WriteOpCreate)
	if removal != nil {
		intent.Op = string(pds.WriteOpUpdate)
	}
	body := map[string]any{
		"$type":     posts.RemovalCollection,
		"subject":   map[string]any{"uri": action.commentURI, "cid": state.CID},
		"code":      action.code,
		"createdAt": removalCreatedAt,
	}
	if action.reason != "" {
		body["reason"] = action.reason
	}
	if intent.Record, err = json.Marshal(body); err != nil {
		return CommunityIntent{}, fmt.Errorf("%w: encoding the intent's removal record: %w", ErrModerationUnavailable, err)
	}
	return intent, nil
}

// commentIntentRemoval is what a comment intent writes, read back from the
// intent: the removal record's key and, for a remove, the comment CID, code
// and reason its body names.
type commentIntentRemoval struct {
	removing         bool
	commentURI, rkey string
	commentCID       string
	code, reason     string
	createdAt        string
}

func readCommentIntentRemoval(intent CommunityIntent) (commentIntentRemoval, error) {
	removal := commentIntentRemoval{removing: intent.Op != string(pds.WriteOpDelete), commentURI: intent.SubjectURI}
	target, err := syntax.ParseATURI(intent.TargetURI)
	if err != nil || target.Collection().String() != posts.RemovalCollection || target.RecordKey() == "" {
		return commentIntentRemoval{}, fmt.Errorf("%w: comment intent %s names no %s record", ErrModerationUnavailable, intent.ID, posts.RemovalCollection)
	}
	removal.rkey = target.RecordKey().String()
	if !removal.removing {
		return removal, nil
	}
	var record struct {
		Subject struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"subject"`
		Code      string `json:"code"`
		Reason    string `json:"reason"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(intent.Record, &record); err != nil {
		return commentIntentRemoval{}, fmt.Errorf("%w: decoding comment intent %s's removal record: %w", ErrModerationUnavailable, intent.ID, err)
	}
	if record.Subject.URI != intent.SubjectURI || record.Subject.CID == "" {
		return commentIntentRemoval{}, fmt.Errorf("%w: comment intent %s's removal record names another subject", ErrModerationUnavailable, intent.ID)
	}
	removal.commentCID, removal.code, removal.reason = record.Subject.CID, record.Code, record.Reason
	removal.createdAt = record.CreatedAt
	return removal, nil
}

// writeCommentIntent writes the intent's removal record (or its deletion)
// through the removal-only writer, recording each batch's attempt before it is
// sent, then applies the local effect. state is the comment's state read in
// this transaction, nil for a retry. A writer error leaves the outcome
// unknown: the action transaction rolls back and the intent stays open for
// reconciliation.
func (s *CommunityActionService) writeCommentIntent(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, state *CommunityCommentState) (*CommunityActionResult, error) {
	removal, err := readCommentIntentRemoval(intent)
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
	var written posts.CommunityWriteResult
	if removal.removing {
		written, err = s.removals.WriteRemovalRecord(ctx, posts.CommunityRemovalCommand{
			CommunityDID: intent.CommunityDID, PostURI: removal.commentURI, PostCID: removal.commentCID,
			Code: posts.DecisionCode(removal.code), Reason: removal.reason,
			CreatedAt: removal.createdAt, ReplaceCreatedAtOf: intent.ReplacedCID, BeforeCommit: beforeCommit,
		})
	} else {
		written, err = s.removals.DeleteRemovalRecord(ctx, posts.CommunityRemovalDeleteCommand{
			CommunityDID: intent.CommunityDID, SubjectURI: removal.commentURI, BeforeCommit: beforeCommit,
		})
	}
	if err != nil {
		if errors.Is(err, ErrModerationUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: writing the community's comment removal record: %w", ErrModerationUnavailable, err)
	}

	commit := CommunityCommit{Rev: written.Rev, GateRev: written.Rev, CompletedAt: s.cfg.Now()}
	if written.Skipped {
		// An earlier request on this intent may have sent the batch whose
		// result the writer found in place: the durable attempts say.
		if lastAttempt == nil {
			if lastAttempt, err = s.latestAttempt(ctx, intent.ID); err != nil {
				return nil, err
			}
		}
		if lastAttempt == nil || (removal.removing && written.CID == lastAttempt.StandCID) {
			return s.commentAlreadyApplied(ctx, tx, intent, removal, state, written.URI)
		}
		// A batch of this intent committed but its response was lost. Its
		// commit rev is unknown, so the gate takes the attempt's guard rev and
		// the firehose still applies any later change.
		commit.Rev, commit.GateRev = lastAttempt.GuardRev, lastAttempt.GuardRev
	}
	if removal.removing {
		commit.RecordCID = written.CID
	}
	return s.complete(ctx, tx, intent, commit)
}

// commentAlreadyApplied answers a comment request whose decision was already
// in place: an identical removal standing, or no removal to delete.
func (s *CommunityActionService) commentAlreadyApplied(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, removal commentIntentRemoval, state *CommunityCommentState, recordURI string) (*CommunityActionResult, error) {
	if state == nil {
		current, err := s.commentState(ctx, tx, removal.commentURI)
		if err != nil {
			return nil, err
		}
		state = &current
	}
	return s.alreadyAppliedAt(ctx, tx, intent, recordURI, state.Version)
}

// reconcileCommentIntent settles an open comment intent from the community
// repo, judged against the latest batch attempt recorded for it, as
// reconcilePostIntent settles a post intent. Once the head has moved past the
// attempt's guard, a remove committed when the removal record holds the
// attempt's body under a CID other than the one before the batch, and a
// restore committed when the removal record is gone; either completes with its
// actor at the attempt's guard rev. A remove whose record is as the attempt
// found it, or a restore whose record still has the CID the batch deleted, was
// not written and fails; any other record waits for its firehose copy.
func (s *CommunityActionService) reconcileCommentIntent(ctx context.Context, tx CommunityActionTx, intent CommunityIntent, retry bool) (*CommunityActionResult, bool, error) {
	removal, err := readCommentIntentRemoval(intent)
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
				result, err := s.writeCommentIntent(ctx, tx, intent, nil)
				return result, false, err
			}
			// Another key's unexpired write may still commit on the PDS:
			// expiring it now would orphan that commit from its actor.
			return nil, false, fmt.Errorf("%w: another request's write to the comment's removal record has an unknown outcome", ErrStateConflict)
		}
		if err := s.finish(ctx, tx, intent, CommunityIntentExpired); err != nil {
			return nil, false, err
		}
		return nil, retry, nil
	}

	committed, unwritten := false, attempt == nil
	var record *pds.RecordResponse
	if attempt != nil {
		if record, err = readStandRecord(ctx, repo, posts.RemovalCollection, removal.rkey); err != nil {
			return nil, false, err
		}
		if removal.removing {
			if record != nil && record.CID != attempt.StandCID {
				if committed, err = sameRecordBody(record.Value, attempt.StandRecord); err != nil {
					return nil, false, err
				}
			}
			unwritten = (record == nil && attempt.StandCID == "") || (record != nil && record.CID == attempt.StandCID)
		} else {
			committed = record == nil
			unwritten = record != nil && record.CID == attempt.ClearCID
		}
	}
	switch {
	case committed:
		commit := CommunityCommit{Rev: attempt.GuardRev, GateRev: attempt.GuardRev, CompletedAt: s.cfg.Now()}
		if removal.removing {
			commit.RecordCID = record.CID
		}
		result, err := s.complete(ctx, tx, intent, commit)
		return result, false, err
	case !unwritten:
		return nil, false, s.awaitFirehoseCopy(ctx, tx, intent, retry, attempt, record)
	}
	return nil, false, s.failUnwritten(ctx, tx, intent, retry)
}
