package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// The Postgres side of the community comment actions (remove and restore):
// the comment's community resolved through its parent chain, its removal
// state read under the action's locks, and the local effect of a completed
// comment intent on the comment removal index (migrations 067, 089).

// CommunityComment reads the comment's community removal state in the action
// transaction: the community its parent chain reaches, its CID, the removal
// record the comment removal index holds under the community's derived key,
// whether that removal is applied, and the comment subject's community
// version. A comment not indexed, deleted, or whose chain does not resolve is
// ErrCommentNotFound.
func (t *communityActionTransaction) CommunityComment(ctx context.Context, commentURI string) (moderation.CommunityCommentState, error) {
	communityDID, err := commentCommunity(ctx, t.tx, commentURI)
	if err != nil {
		return moderation.CommunityCommentState{}, err
	}
	state := moderation.CommunityCommentState{CommunityDID: communityDID}
	err = t.tx.QueryRowContext(ctx, `
		SELECT c.cid, COALESCE(r.record_cid, ''), COALESCE(r.code, ''), COALESCE(r.reason, ''),
		       COALESCE(r.state = 'applied', FALSE), COALESCE(v.version, 0)
		FROM comments c
		LEFT JOIN community_comment_removals r
		       ON r.repository_did = $2 AND r.record_key = $3 AND r.subject_uri = c.uri
		LEFT JOIN community_subject_versions v ON v.community_did = $2 AND v.subject = c.uri
		WHERE c.uri = $1
	`, commentURI, communityDID, posts.SubjectRkey(commentURI)).Scan(&state.CID, &state.RemovalCID,
		&state.RemovalCode, &state.RemovalReason, &state.Removed, &state.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return moderation.CommunityCommentState{}, fmt.Errorf("%w: %s", moderation.ErrCommentNotFound, commentURI)
	}
	if err != nil {
		return moderation.CommunityCommentState{}, fmt.Errorf("read the community state of comment %s: %w", commentURI, err)
	}
	return state, nil
}

// communityCommentResolver is the comment-to-community index read the
// community comment actions authorize against.
type communityCommentResolver struct {
	db *sql.DB
}

// NewCommunityCommentResolver builds the resolver over db.
func NewCommunityCommentResolver(db *sql.DB) moderation.CommunityCommentResolver {
	return &communityCommentResolver{db: db}
}

// CommentCommunity returns the community of the indexed thread root post the
// comment's parent chain reaches, or moderation.ErrCommentNotFound.
func (r *communityCommentResolver) CommentCommunity(ctx context.Context, commentURI string) (string, error) {
	return commentCommunity(ctx, r.db, commentURI)
}

// commentCommunity is the community of the post the indexed, undeleted
// comment's parent chain reaches (threadRootCommunity, never the comment's
// root_uri), or ErrCommentNotFound when the comment is not indexed, is
// deleted, or its chain does not resolve.
func commentCommunity(ctx context.Context, querier threadQuerier, commentURI string) (string, error) {
	var live bool
	err := querier.QueryRowContext(ctx, `SELECT deleted_at IS NULL FROM comments WHERE uri = $1`, commentURI).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !live) {
		return "", fmt.Errorf("%w: %s", moderation.ErrCommentNotFound, commentURI)
	}
	if err != nil {
		return "", fmt.Errorf("read comment %s: %w", commentURI, err)
	}
	_, communityDID, resolved, err := threadRootCommunity(ctx, querier, commentURI)
	if err != nil {
		return "", err
	}
	if !resolved {
		return "", fmt.Errorf("%w: %s's thread does not reach an indexed community post", moderation.ErrCommentNotFound, commentURI)
	}
	return communityDID, nil
}

// isCommentIntent reports whether the intent is a community comment action's:
// its subject is a comment.
func isCommentIntent(intent moderation.CommunityIntent) bool {
	if intent.SubjectURI == "" {
		return false
	}
	uri, err := syntax.ParseATURI(intent.SubjectURI)
	return err == nil && uri.Collection().String() == moderation.CommentCollection
}

// localCommentRemoval is what a comment intent's completion changes: the
// removal row under the community's derived key, whether the record's rev
// gate held no change after the commit (current), and the actor its action
// names.
type localCommentRemoval struct {
	intentID              string
	communityDID          string
	recordKey, subjectURI string
	current               bool
	actor                 commentRemovalActor
}

// completeCommentIntent applies a comment intent's local effect in the action
// transaction. The removal record's rev gate is stamped at commit.GateRev, so
// the commit's firehose copy is no new change; when the gate already held a
// later change of the record, that change stands and only the action is
// logged. Otherwise a remove (create or update of the removal record) indexes
// the record with its code and reason and applies it, or, for an applied
// removal rewritten, moves the active decision to the new action; a restore
// (delete of the removal record) drops the row and retracts an applied
// removal. Each changed decision advances the comment's community version
// once. Exactly one action names the intent's actor, with the removal's
// mapped code as its reason (a restore's, the deleted removal's code); then
// the intent completes and its idempotency record is saved.
func (t *communityActionTransaction) completeCommentIntent(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	if commit.GateRev == "" {
		return moderation.CommunityActionResult{}, fmt.Errorf("complete community comment intent %s: no gate rev", intent.ID)
	}
	current, err := stampIntentRecordGate(ctx, t.tx, intent.TargetURI, commit.GateRev)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	if err := lockRemovalSubject(ctx, t.tx, intent.SubjectURI); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	removal := localCommentRemoval{
		intentID: intent.ID, communityDID: intent.CommunityDID,
		recordKey: intentRecordKey(intent.TargetURI), subjectURI: intent.SubjectURI, current: current,
		actor: commentRemovalActor{
			actorDID: intent.ActorDID, adminActing: intent.AdminActing,
			origin: moderation.OriginLocal, createdAt: commit.CompletedAt,
		},
	}
	var actionID string
	if intent.Op == "delete" {
		actionID, err = retractLocalCommentRemoval(ctx, t.tx, removal, intent.CreatedAt)
	} else {
		var body standRecordBody
		if err := json.Unmarshal(intent.Record, &body); err != nil {
			return moderation.CommunityActionResult{}, fmt.Errorf("decode community comment intent %s's removal record: %w", intent.ID, err)
		}
		if body.Subject.URI != intent.SubjectURI || body.Subject.CID == "" {
			return moderation.CommunityActionResult{}, fmt.Errorf("community comment intent %s's removal record names another subject", intent.ID)
		}
		if commit.RecordCID == "" {
			if commit.RecordCID, err = recordBodyCID(intent.Record); err != nil {
				return moderation.CommunityActionResult{}, fmt.Errorf("the CID of community comment intent %s's removal: %w", intent.ID, err)
			}
		}
		actionID, err = applyLocalCommentRemoval(ctx, t.tx, removal, commit.RecordCID, body)
	}
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	version, err := communityPostVersion(ctx, t.tx, intent.CommunityDID, intent.SubjectURI)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	return saveCompletedIntentTx(ctx, t.tx, intent, commit, moderation.CommunityActionResult{
		CommunityDID: intent.CommunityDID,
		RecordURI:    intent.TargetURI,
		ActionID:     actionID,
		Version:      version,
	}, commit.CompletedAt, idempotencyTTL)
}

// applyLocalCommentRemoval indexes the removal record a comment remove wrote
// and applies it with the intent's actor, returning the action that records
// it: a new apply-removal, or, when the index already holds this record
// applied, the action that applied it. A removal whose comment's thread does
// not resolve to the community stays held, its action logged.
func applyLocalCommentRemoval(ctx context.Context, tx *sql.Tx, removal localCommentRemoval, recordCID string, body standRecordBody) (string, error) {
	action := commentRemovalAction{
		action: moderation.ActionApplyRemoval, communityDID: removal.communityDID,
		subjectURI: removal.subjectURI, observedCID: body.Subject.CID,
		reason: moderation.CommunityRemovalActionReason(body.Code), actor: removal.actor,
	}
	if !removal.current {
		return insertCommentRemovalAction(ctx, tx, action)
	}
	var priorCID, priorState string
	var priorApplyID, movedID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT record_cid, state, apply_action_id, moved_action_id FROM community_comment_removals
		WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
	`, removal.communityDID, removal.recordKey).Scan(&priorCID, &priorState, &priorApplyID, &movedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read community comment removal: %w", err)
	}
	indexed, err := tx.ExecContext(ctx, `
		INSERT INTO community_comment_removals
		    (repository_did, record_key, record_cid, subject_uri, subject_cid, state, code, reason)
		VALUES ($1, $2, $3, $4, $5, 'held', NULLIF($6, ''), NULLIF($7, ''))
		ON CONFLICT (repository_did, record_key) DO UPDATE SET
		    record_cid = EXCLUDED.record_cid, subject_cid = EXCLUDED.subject_cid,
		    code = EXCLUDED.code, reason = EXCLUDED.reason, updated_at = NOW()
		WHERE community_comment_removals.subject_uri = EXCLUDED.subject_uri
	`, removal.communityDID, removal.recordKey, recordCID, removal.subjectURI, body.Subject.CID, body.Code, body.Reason)
	if err != nil {
		return "", fmt.Errorf("index community comment removal: %w", err)
	}
	if count, err := indexed.RowsAffected(); err != nil || count == 0 {
		return "", errors.Join(fmt.Errorf("index community comment removal %s/%s: the key names another subject",
			removal.communityDID, removal.recordKey), err)
	}
	if priorState == "applied" && priorCID == recordCID {
		return priorApplyID.String, nil
	}
	rootURI, rootCommunityDID, resolved, err := threadRootCommunity(ctx, tx, removal.subjectURI)
	if err != nil {
		return "", err
	}
	action.rootURI = rootURI
	if priorState == "applied" {
		// A rewrite of an applied removal: the decision stays active under
		// the new action.
		actionID, err := insertCommentRemovalAction(ctx, tx, action)
		if err != nil {
			return "", err
		}
		if err := activateCommentRemoval(ctx, tx, removal.communityDID, removal.recordKey, removal.subjectURI, actionID); err != nil {
			return "", err
		}
		return actionID, advanceCommunitySubjectVersion(ctx, tx, removal.communityDID, removal.subjectURI)
	}
	if !resolved || rootCommunityDID != removal.communityDID {
		return insertCommentRemovalAction(ctx, tx, action)
	}
	return applyRemoval(ctx, tx, commentRemovalApplication{
		repositoryDID: removal.communityDID, recordKey: removal.recordKey,
		subjectURI: removal.subjectURI, subjectCID: body.Subject.CID, rootURI: rootURI,
		reason: action.reason, reversesID: movedID.String, actor: removal.actor,
	})
}

// retractLocalCommentRemoval drops the removal row a comment restore deleted
// the record of and retracts it with the intent's actor when it was applied,
// returning the retract-removal. When the index no longer holds the row, or
// the gate already holds a later change (its delete was indexed first with no
// actor, after intentCreatedAt, possibly not matched to this intent because
// the batch deleted a rewrite the index never saw), that retract-removal is
// attributed to the actor with the deleted code instead of logging a second.
func retractLocalCommentRemoval(ctx context.Context, tx *sql.Tx, removal localCommentRemoval, intentCreatedAt time.Time) (string, error) {
	action := commentRemovalAction{
		action: moderation.ActionRetractRemoval, communityDID: removal.communityDID,
		subjectURI: removal.subjectURI, actor: removal.actor,
	}
	var state, code string
	var applyActionID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT state, apply_action_id, COALESCE(code, '') FROM community_comment_removals
		WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
	`, removal.communityDID, removal.recordKey).Scan(&state, &applyActionID, &code)
	indexed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("lock community comment removal: %w", err)
	}
	// The removal the restore's batch deleted is the latest attempt's: an
	// external rewrite not yet indexed may have replaced the indexed one. The
	// indexed code stands in only when no attempt was recorded.
	deletedCode, recorded, err := deletedRemovalCode(ctx, tx, removal.intentID)
	if err != nil {
		return "", err
	}
	if recorded {
		code = deletedCode
	}
	action.reason = moderation.CommunityRemovalActionReason(code)
	if !removal.current || !indexed {
		return attributeOrInsertRetraction(ctx, tx, action, intentCreatedAt)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM community_comment_removals WHERE repository_did = $1 AND record_key = $2
	`, removal.communityDID, removal.recordKey); err != nil {
		return "", fmt.Errorf("delete community comment removal: %w", err)
	}
	if state != "applied" {
		return insertCommentRemovalAction(ctx, tx, action)
	}
	retractID, err := retractCommunityRemoval(ctx, tx, removal.communityDID, removal.subjectURI, applyActionID.String,
		action.reason, removal.actor)
	if err != nil {
		return "", err
	}
	return retractID, advanceCommunitySubjectVersion(ctx, tx, removal.communityDID, removal.subjectURI)
}

// deletedRemovalCode is the code of the removal a restore intent's latest
// recorded batch deleted, and whether any batch was recorded.
func deletedRemovalCode(ctx context.Context, tx *sql.Tx, intentID string) (string, bool, error) {
	var code sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT clear_code FROM community_write_intent_attempts
		WHERE id = (SELECT MAX(id) FROM community_write_intent_attempts WHERE intent_id = $1)
	`, intentID).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the removal community write intent %s deleted: %w", intentID, err)
	}
	return code.String, true, nil
}

// attributeOrInsertRetraction attributes to action's actor, with action's
// reason, the latest actorless retract-removal of the comment in its
// community logged since since, the firehose's record of the delete the
// restore committed, and returns it; with none, it logs action.
func attributeOrInsertRetraction(ctx context.Context, tx *sql.Tx, action commentRemovalAction, since time.Time) (string, error) {
	var actionID string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM moderation_actions
		WHERE scope_kind = 'community' AND scope_community_did = $1 AND subject_uri = $2
		  AND action = 'retract-removal' AND origin = $3 AND actor_did = ''
		  AND reason IS DISTINCT FROM $4 AND created_at >= $5
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, action.communityDID, action.subjectURI, moderation.OriginInherited, moderation.ReasonSubjectMoved, since.UTC()).Scan(&actionID)
	if errors.Is(err, sql.ErrNoRows) {
		return insertCommentRemovalAction(ctx, tx, action)
	}
	if err != nil {
		return "", fmt.Errorf("read the comment's inherited retract-removal: %w", err)
	}
	// The firehose logged the indexed code; the restore's batch may have
	// deleted a rewrite the index never saw, so action.reason replaces it.
	if _, err := tx.ExecContext(ctx, `
		UPDATE moderation_actions SET actor_did = $2, admin_acting = $3, origin = $4, reason = NULLIF($5, '')
		WHERE id = $1 AND origin = $6
	`, actionID, action.actor.actorDID, action.actor.adminActing, moderation.OriginLocal, action.reason,
		moderation.OriginInherited); err != nil {
		return "", fmt.Errorf("attribute community comment retract-removal %s: %w", actionID, err)
	}
	return actionID, nil
}

// completeCommentRemoveIntentFromEvent completes the open comment remove
// intent whose commit the firehose removal record is (openRemovalIntentFor:
// its latest batch's body has the record's CID, its guard rev is before the
// record's), with its actor at the record's rev, and reports whether one did.
// The caller holds the record's rev gate, advanced to the record's rev.
func completeCommentRemoveIntentFromEvent(ctx context.Context, tx *sql.Tx, removal moderation.CommunityCommentRemoval) (bool, error) {
	if removal.Rev == "" {
		return false, nil
	}
	removalURI := "at://" + removal.RepositoryDID + "/" + posts.RemovalCollection + "/" + removal.RecordKey
	intent, err := openRemovalIntentFor(ctx, tx, removalURI, removal.RecordCID, removal.Rev)
	if err != nil || intent == nil || !isCommentIntent(*intent) || intent.SubjectURI != removal.SubjectURI {
		return false, err
	}
	commit := moderation.CommunityCommit{
		Rev: removal.Rev, GateRev: removal.Rev, RecordCID: removal.RecordCID, CompletedAt: time.Now(),
	}
	if _, err := (&communityActionTransaction{tx: tx}).completeCommentIntent(ctx, *intent, commit, intent.IdempotencyRetention); err != nil {
		return false, err
	}
	return true, nil
}

// completeCommentRestoreIntentFromEvent completes the open comment restore
// intent on the removal record whose commit a firehose delete at rev is: its
// guard rev is before rev and its latest recorded batch deleted the record at
// deletedCID, the one the index holds. It reports whether one did. The caller
// holds the record's rev gate, advanced to rev.
func completeCommentRestoreIntentFromEvent(ctx context.Context, tx *sql.Tx, repositoryDID, recordKey, subjectURI, deletedCID, rev string) (bool, error) {
	removalURI := "at://" + repositoryDID + "/" + posts.RemovalCollection + "/" + recordKey
	intent, err := openRestoreIntentFor(ctx, tx, removalURI, rev)
	if err != nil || intent == nil || !isCommentIntent(*intent) || intent.SubjectURI != subjectURI {
		return false, err
	}
	var deletesIt bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM community_write_intent_attempts
			WHERE id = (SELECT MAX(id) FROM community_write_intent_attempts WHERE intent_id = $1)
			  AND clear_cid = $2
		)
	`, intent.ID, deletedCID).Scan(&deletesIt); err != nil {
		return false, fmt.Errorf("read whether intent %s deletes %s: %w", intent.ID, removalURI, err)
	}
	if !deletesIt {
		return false, nil
	}
	commit := moderation.CommunityCommit{Rev: rev, GateRev: rev, CompletedAt: time.Now()}
	if _, err := (&communityActionTransaction{tx: tx}).completeCommentIntent(ctx, *intent, commit, intent.IdempotencyRetention); err != nil {
		return false, err
	}
	return true, nil
}
