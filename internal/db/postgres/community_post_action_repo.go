package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// The Postgres side of the community post actions (remove and restore): the
// post's stand state read under the action's locks, the removal limits'
// count, each batch's attempt record, and the local effect of a completed
// post intent (migration 088).

var _ moderation.CommunityIntentAttempts = (*CommunityActionRepository)(nil)

// RecordIntentAttempt records, in its own transaction on the request pool,
// the batch a post or comment intent's writer is about to send (a comment
// restore's stands no record). The attempts table has
// no foreign key to the intent, so the insert never waits on the intent row
// the action transaction may hold.
func (r *CommunityActionRepository) RecordIntentAttempt(ctx context.Context, intentID string, attempt posts.CommunityCommitAttempt) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO community_write_intent_attempts (intent_id, guard_commit_cid, guard_rev, stand_cid, stand_record, clear_cid, clear_code)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, ''))
	`, intentID, attempt.GuardCommitCID, attempt.GuardRev, attempt.StandCID, jsonColumn(attempt.StandRecord),
		attempt.ClearCID, attempt.ClearCode); err != nil {
		return fmt.Errorf("record community write intent %s's attempt: %w", intentID, err)
	}
	return nil
}

// LatestIntentAttempt reads the intent's most recently recorded attempt on
// the request pool; attempts commit in their own transactions, so an action
// transaction's retry or sweep sees every one recorded before it.
func (r *CommunityActionRepository) LatestIntentAttempt(ctx context.Context, intentID string) (*posts.CommunityCommitAttempt, error) {
	var (
		attempt posts.CommunityCommitAttempt
		record  []byte
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT guard_commit_cid, guard_rev, COALESCE(stand_cid, ''), stand_record, COALESCE(clear_cid, ''), COALESCE(clear_code, '')
		FROM community_write_intent_attempts
		WHERE intent_id = $1
		ORDER BY id DESC
		LIMIT 1
	`, intentID).Scan(&attempt.GuardCommitCID, &attempt.GuardRev, &attempt.StandCID, &record, &attempt.ClearCID, &attempt.ClearCode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read community write intent %s's latest attempt: %w", intentID, err)
	}
	attempt.StandRecord = record
	return &attempt, nil
}

// CommunityPost reads the post's community stand state in the action
// transaction: its community, current CID and admission status, the
// acceptance the admission holds, the removal record the index holds
// (community_post_removal_records) and the post subject's community version.
// A post not indexed in a community, or deleted, is ErrPostNotFound.
func (t *communityActionTransaction) CommunityPost(ctx context.Context, postURI string) (moderation.CommunityPostState, error) {
	var state moderation.CommunityPostState
	err := t.tx.QueryRowContext(ctx, `
		SELECT p.community_did, p.cid, a.status, COALESCE(a.accepted_cid, ''), a.acceptance_uri IS NOT NULL,
		       COALESCE(r.record_cid, ''), COALESCE(r.code, ''), COALESCE(r.reason, ''), COALESCE(v.version, 0)
		FROM posts p
		JOIN community_post_admissions a ON a.community_did = p.community_did AND a.post_uri = p.uri
		LEFT JOIN community_post_removal_records r ON r.community_did = p.community_did AND r.post_uri = p.uri
		LEFT JOIN community_subject_versions v ON v.community_did = p.community_did AND v.subject = p.uri
		WHERE p.uri = $1 AND p.deleted_at IS NULL
	`, postURI).Scan(&state.CommunityDID, &state.CID, &state.Status, &state.PinnedCID, &state.HasAcceptance,
		&state.RemovalCID, &state.RemovalCode, &state.RemovalReason, &state.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return moderation.CommunityPostState{}, fmt.Errorf("%w: %s", moderation.ErrPostNotFound, postURI)
	}
	if err != nil {
		return moderation.CommunityPostState{}, fmt.Errorf("read the community state of post %s: %w", postURI, err)
	}
	return state, nil
}

// RecentCommunityRemovals counts the community's non-admin local post and
// comment removals after since: those logged (a post's remove, a comment's
// apply-removal) and those whose intent is still open (not a ban's), by
// actorDID and by anyone. The caller holds the community roster lock, so the count and the
// intent a removal then stores serialize per community.
func (t *communityActionTransaction) RecentCommunityRemovals(ctx context.Context, communityDID, actorDID string, since time.Time) (int, int, error) {
	var byActor, byCommunity int
	err := t.tx.QueryRowContext(ctx, `
		WITH removals AS (
			SELECT actor_did FROM moderation_actions
			WHERE scope_kind = 'community' AND scope_community_did = $1
			  AND (action = 'remove' OR (action = 'apply-removal' AND subject_collection = $4))
			  AND origin = 'local' AND NOT admin_acting AND subject_collection IS NOT NULL
			  AND created_at > $3
			UNION ALL
			SELECT actor_did FROM community_write_intents
			WHERE community_did = $1 AND state = 'open' AND NOT admin_acting
			  AND (action = 'remove' OR (action = 'apply-removal' AND subject_uri IS NOT NULL))
			  AND target_uri NOT LIKE $5
			  AND created_at > $3
		)
		SELECT COUNT(*) FILTER (WHERE actor_did = $2), COUNT(*) FROM removals
	`, communityDID, actorDID, since.UTC(), moderation.CommentCollection, banTargetURIPattern).Scan(&byActor, &byCommunity)
	if err != nil {
		return 0, 0, fmt.Errorf("count recent community removals in %s: %w", communityDID, err)
	}
	return byActor, byCommunity, nil
}

// isPostIntent reports whether the intent is a community post action's: its
// target is a removal record and it names no subject (isCommentIntent).
func isPostIntent(intent moderation.CommunityIntent) bool {
	uri, err := syntax.ParseATURI(intent.TargetURI)
	return err == nil && intent.SubjectURI == "" && uri.Collection().String() == posts.RemovalCollection
}

// standRecordBody is the part of a removal or acceptance body a post intent
// writes that its completion reads.
type standRecordBody struct {
	Subject struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	} `json:"subject"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// completePostIntent applies a post intent's local effect in the action
// transaction. A remove (create or update of the removal record) withdraws
// the acceptance and applies the removal with its code and reason, and
// indexes the removal record at the CID written (else the CID the PDS assigns
// its body); a restore (delete of the removal record) withdraws the removal,
// applies the acceptance pinned to the post's CID, and drops the indexed
// removal record. Both changes take the admission watermark at
// commit.GateRev, so the commit's firehose copy is no new change; the
// removal record's identity is indexed or dropped only over an identity
// indexed at a rev not after commit.GateRev, and indexed only when no delete
// of the removal record after commit.GateRev was indexed, so a delayed
// completion never replaces a newer record's or revives a deleted one. A
// restore whose removal delete was indexed first as an inherited restore
// awaiting it (awaitRestoreIntent) completes at that delete's rev: a
// reconciliation knowing only the guard rev then still applies the commit's
// acceptance, counted once with the delete. The post's community version
// advances once when the admission's decision changed, not when a firehose
// copy of the commit
// already applied it: with eventRev (commit.Rev is the rev of the firehose
// event completing the intent), a watermark already at that rev's delete is
// the commit's other half, which counted the commit. One action is logged
// naming the intent's actor, with the removal's mapped code as its reason
// (for a restore, the code of the removal its latest batch deleted), or, when
// the removal's delete was logged first as an inherited restore that awaited
// this intent (the intent's action_id) at the completing commit's rev, that
// action is attributed to the actor; then the intent completes and its
// idempotency record is saved.
func (t *communityActionTransaction) completePostIntent(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration, eventRev bool) (moderation.CommunityActionResult, error) {
	if commit.GateRev == "" {
		return moderation.CommunityActionResult{}, fmt.Errorf("complete community post intent %s: no gate rev", intent.ID)
	}
	removing := intent.Op != "delete"
	body := intent.PairedRecord
	if removing {
		body = intent.Record
	}
	var record standRecordBody
	if err := json.Unmarshal(body, &record); err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("decode community post intent %s's stand record: %w", intent.ID, err)
	}
	postURI, postCID := record.Subject.URI, record.Subject.CID
	if postURI == "" || postCID == "" {
		return moderation.CommunityActionResult{}, fmt.Errorf("community post intent %s's stand record names no subject", intent.ID)
	}
	rkey := intentRecordKey(intent.TargetURI)
	awaiting := ""
	if !removing {
		awaitingAction, awaitingRev, err := awaitingRestoreDelete(ctx, t.tx, intent.ID)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
		switch {
		case awaitingAction == "":
		case !eventRev:
			// The commit is proven and its removal delete is the awaiting
			// one: the commit's rev is that delete's.
			commit.Rev, commit.GateRev, eventRev = awaitingRev, awaitingRev, true
			awaiting = awaitingAction
		case awaitingRev == commit.Rev:
			awaiting = awaitingAction
		}
	}
	watermark := posts.CommunityWatermark{Rev: commit.GateRev}
	withdrawal := posts.CommunityDeleteCommand{CommunityDID: intent.CommunityDID, PostURI: postURI, Watermark: watermark}

	before, err := admissionDecisionTx(ctx, t.tx, intent.CommunityDID, postURI)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	counted := false
	if eventRev {
		if counted, err = otherHalfApplied(ctx, t.tx, intent.CommunityDID, postURI, commit.Rev); err != nil {
			return moderation.CommunityActionResult{}, err
		}
	}
	var code string
	if removing {
		code = record.Code
		removalCID := commit.RecordCID
		if removalCID == "" {
			if removalCID, err = recordBodyCID(intent.Record); err != nil {
				return moderation.CommunityActionResult{}, fmt.Errorf("the CID of community post intent %s's removal: %w", intent.ID, err)
			}
		}
		if _, err = applyAcceptanceDeleteInTx(ctx, t.tx, withdrawal); err != nil {
			return moderation.CommunityActionResult{}, err
		}
		if _, err = applyRemovalInTx(ctx, t.tx, posts.ApplyRemovalCommand{
			CommunityDID: intent.CommunityDID, PostURI: postURI, DecisionCode: record.Code, Reason: record.Reason,
			Watermark: watermark,
		}); err != nil {
			return moderation.CommunityActionResult{}, err
		}
		deletedAfter, err := removalDeletedAfter(ctx, t.tx, intent.TargetURI, commit.GateRev)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
		if !deletedAfter {
			if err := indexCommunityPostRemovalRecord(ctx, t.tx, communityPostRemovalRecord{
				communityDID: intent.CommunityDID, postURI: postURI, recordKey: rkey, recordCID: removalCID,
				code: record.Code, reason: record.Reason, rev: commit.GateRev,
			}); err != nil {
				return moderation.CommunityActionResult{}, err
			}
		}
	} else {
		if code, err = restoredRemovalCode(ctx, t.tx, intent.ID, intent.CommunityDID, postURI); err != nil {
			return moderation.CommunityActionResult{}, err
		}
		if _, err = applyRemovalDeleteInTx(ctx, t.tx, withdrawal); err != nil {
			return moderation.CommunityActionResult{}, err
		}
		if _, err = applyAcceptanceInTx(ctx, t.tx, posts.ApplyAcceptanceCommand{
			CommunityDID: intent.CommunityDID, PostURI: postURI,
			AcceptanceURI: intent.PairedTargetURI, AcceptanceRkey: intentRecordKey(intent.PairedTargetURI),
			PinnedCID: postCID, Watermark: watermark,
		}); err != nil {
			return moderation.CommunityActionResult{}, err
		}
		if err := dropCommunityPostRemovalRecord(ctx, t.tx, intent.CommunityDID, postURI, commit.GateRev); err != nil {
			return moderation.CommunityActionResult{}, err
		}
	}
	after, err := admissionDecisionTx(ctx, t.tx, intent.CommunityDID, postURI)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	if after.changedFrom(before) && !counted {
		if err := advanceCommunitySubjectVersion(ctx, t.tx, intent.CommunityDID, postURI); err != nil {
			return moderation.CommunityActionResult{}, err
		}
	}

	actionID := awaiting
	if awaiting != "" {
		err = attributeCommunityPostAction(ctx, t.tx, awaiting, intent.ActorDID, intent.AdminActing)
	} else {
		actionID, err = insertCommunityPostAction(ctx, t.tx, communityPostAction{
			action: intent.Action, actorDID: intent.ActorDID, adminActing: intent.AdminActing,
			communityDID: intent.CommunityDID, postURI: postURI, postCID: postCID,
			reason: moderation.CommunityRemovalActionReason(code), origin: moderation.OriginLocal,
			createdAt: commit.CompletedAt,
		})
	}
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	version, err := communityPostVersion(ctx, t.tx, intent.CommunityDID, postURI)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	recordURI := intent.TargetURI
	if !removing {
		recordURI = intent.PairedTargetURI
	}
	return saveCompletedIntentTx(ctx, t.tx, intent, commit, moderation.CommunityActionResult{
		CommunityDID: intent.CommunityDID,
		RecordURI:    recordURI,
		ActionID:     actionID,
		Version:      version,
	}, commit.CompletedAt, idempotencyTTL)
}

// admissionDecision is what a post's admission row says the community
// decided: its status, the acceptance it holds and the removal's code and
// reason, with the identity (key and CID) of the removal record the index
// holds for it. A completion that leaves it as it was changed nothing a
// reader sees (the commit's firehose copy already applied it), so the
// version stays.
type admissionDecision struct {
	exists                                                  bool
	status, acceptanceURI, acceptedCID, code, removalReason string
	removalRecordKey, removalRecordCID                      string
}

// changedFrom reports whether d differs from before as a version change. A
// new removal record identity counts only while the admission holds a
// removal: a moderator's record replacing the instance outbox's with the same
// code and reason is a change, but a record indexed beside an admission
// change refused as older (a completion recovered at its guard rev after the
// commit's acceptance delete counted the commit) is not.
func (d admissionDecision) changedFrom(before admissionDecision) bool {
	if d.exists != before.exists || d.status != before.status || d.acceptanceURI != before.acceptanceURI ||
		d.acceptedCID != before.acceptedCID || d.code != before.code || d.removalReason != before.removalReason {
		return true
	}
	return d.status == string(posts.AdmissionStatusRemoved) &&
		(d.removalRecordKey != before.removalRecordKey || d.removalRecordCID != before.removalRecordCID)
}

// admissionDecisionTx reads the post's admission decision in tx, locking the
// admission row.
func admissionDecisionTx(ctx context.Context, tx *sql.Tx, communityDID, postURI string) (admissionDecision, error) {
	decision := admissionDecision{exists: true}
	err := tx.QueryRowContext(ctx, `
		SELECT a.status, COALESCE(a.acceptance_uri, ''), COALESCE(a.accepted_cid, ''), COALESCE(a.decision_code, ''), COALESCE(a.removal_reason, ''),
		       COALESCE(r.record_key, ''), COALESCE(r.record_cid, '')
		FROM community_post_admissions a
		LEFT JOIN community_post_removal_records r ON r.community_did = a.community_did AND r.post_uri = a.post_uri
		WHERE a.community_did = $1 AND a.post_uri = $2
		FOR UPDATE OF a
	`, communityDID, postURI).Scan(&decision.status, &decision.acceptanceURI, &decision.acceptedCID, &decision.code, &decision.removalReason,
		&decision.removalRecordKey, &decision.removalRecordCID)
	if errors.Is(err, sql.ErrNoRows) {
		return admissionDecision{}, nil
	}
	if err != nil {
		return admissionDecision{}, fmt.Errorf("read the admission of %s in %s: %w", postURI, communityDID, err)
	}
	return decision, nil
}

// communityPostRemovalRecord is one community_post_removal_records row: the
// removal record a community holds for a post.
type communityPostRemovalRecord struct {
	communityDID, postURI string
	recordKey, recordCID  string
	code, reason          string
	rev                   string
}

// indexCommunityPostRemovalRecord indexes the removal record the community
// holds for the post, replacing any it held before at a rev not after the
// record's (or at no rev).
func indexCommunityPostRemovalRecord(ctx context.Context, tx *sql.Tx, record communityPostRemovalRecord) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO community_post_removal_records (community_did, post_uri, record_key, record_cid, code, reason, rev, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
		    record_key = EXCLUDED.record_key, record_cid = EXCLUDED.record_cid, code = EXCLUDED.code,
		    reason = EXCLUDED.reason, rev = EXCLUDED.rev, updated_at = NOW()
		WHERE community_post_removal_records.rev IS NULL
		   OR community_post_removal_records.rev COLLATE "C" <= EXCLUDED.rev COLLATE "C"
	`, record.communityDID, record.postURI, record.recordKey, record.recordCID, record.code,
		strings.ReplaceAll(record.reason, "\x00", ""), record.rev); err != nil {
		return fmt.Errorf("index the removal record of %s in %s: %w", record.postURI, record.communityDID, err)
	}
	return nil
}

// indexStampedRemovalRecord indexes a removal the AppView wrote and stamped
// (ApplyRemoval) under the subject's canonical key, with no CID.
func indexStampedRemovalRecord(ctx context.Context, tx *sql.Tx, cmd posts.ApplyRemovalCommand) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO community_post_removal_records (community_did, post_uri, record_key, record_cid, code, reason, rev, updated_at)
		VALUES ($1, $2, $3, NULL, $4, NULLIF($5, ''), $6, NOW())
		ON CONFLICT (community_did, post_uri) DO UPDATE SET
		    record_key = EXCLUDED.record_key, record_cid = NULL, code = EXCLUDED.code,
		    reason = EXCLUDED.reason, rev = EXCLUDED.rev, updated_at = NOW()
	`, cmd.CommunityDID, cmd.PostURI, posts.SubjectRkey(cmd.PostURI), cmd.DecisionCode,
		strings.ReplaceAll(cmd.Reason, "\x00", ""), cmd.Watermark.Rev); err != nil {
		return fmt.Errorf("index the stamped removal record of %s in %s: %w", cmd.PostURI, cmd.CommunityDID, err)
	}
	return nil
}

// dropCommunityPostRemovalRecord drops the removal record the index holds for
// the post, if any, when it was indexed at a rev not after rev (or at no rev).
func dropCommunityPostRemovalRecord(ctx context.Context, tx *sql.Tx, communityDID, postURI, rev string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM community_post_removal_records
		WHERE community_did = $1 AND post_uri = $2 AND (rev IS NULL OR rev COLLATE "C" <= $3 COLLATE "C")
	`, communityDID, postURI, rev); err != nil {
		return fmt.Errorf("drop the removal record of %s in %s: %w", postURI, communityDID, err)
	}
	return nil
}

// standingRemovalCode is the code of the removal the community holds for the
// post: the indexed removal record's, else the admission's decision code
// while it is removed, else "".
func standingRemovalCode(ctx context.Context, tx *sql.Tx, communityDID, postURI string) (string, error) {
	var code string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT code FROM community_post_removal_records WHERE community_did = $1 AND post_uri = $2),
			(SELECT decision_code FROM community_post_admissions
			 WHERE community_did = $1 AND post_uri = $2 AND status = 'removed'),
			'')
	`, communityDID, postURI).Scan(&code)
	if err != nil {
		return "", fmt.Errorf("read the removal code of %s in %s: %w", postURI, communityDID, err)
	}
	return code, nil
}

// restoredRemovalCode is the code of the removal a restore intent deleted:
// the one its latest recorded batch deleted, else, when no batch recorded
// deleting one, the removal the community holds for the post
// (standingRemovalCode).
func restoredRemovalCode(ctx context.Context, tx *sql.Tx, intentID, communityDID, postURI string) (string, error) {
	var code sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT clear_code FROM community_write_intent_attempts
		WHERE intent_id = $1 AND clear_cid IS NOT NULL
		  AND id = (SELECT MAX(id) FROM community_write_intent_attempts WHERE intent_id = $1)
	`, intentID).Scan(&code)
	switch {
	case err == nil:
		return code.String, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("read the removal community write intent %s deleted: %w", intentID, err)
	}
	return standingRemovalCode(ctx, tx, communityDID, postURI)
}

// removalDeletedAfter reports whether a delete of the removal record at
// removalURI at a rev after rev was indexed: the record's rev gate
// (jetstream_record_revs, advanced by every removal delete) is past rev.
func removalDeletedAfter(ctx context.Context, tx *sql.Tx, removalURI, rev string) (bool, error) {
	var after bool
	err := tx.QueryRowContext(ctx, `
		SELECT rev > $2 FROM jetstream_record_revs WHERE record_uri = $1
	`, removalURI, rev).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the rev gate of %s: %w", removalURI, err)
	}
	return after, nil
}

// advanceRemovalDeleteRev advances the removal record's rev gate to a delete's
// rev when it holds an older one, so a delayed completion of an earlier
// removal does not index a record this delete withdrew (removalDeletedAfter).
func advanceRemovalDeleteRev(ctx context.Context, tx *sql.Tx, removalURI, rev string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)
		ON CONFLICT (record_uri) DO UPDATE SET rev = EXCLUDED.rev, updated_at = NOW()
		WHERE jetstream_record_revs.rev < EXCLUDED.rev
	`, removalURI, rev); err != nil {
		return fmt.Errorf("advance the rev gate of %s: %w", removalURI, err)
	}
	return nil
}

// communityPostVersion is the post subject's community version; a missing
// row is 0.
func communityPostVersion(ctx context.Context, tx *sql.Tx, communityDID, postURI string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, `
		SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $2
	`, communityDID, postURI).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the community version of %s in %s: %w", postURI, communityDID, err)
	}
	return version, nil
}

// communityPostAction is one remove or restore of a post in its community's
// log. actorDID is empty for a change with no matching intent of this
// instance (origin inherited). createdAt is the service clock for a local
// action, so the removal limits' window and the service clock agree.
type communityPostAction struct {
	action, actorDID string
	adminActing      bool
	communityDID     string
	postURI, postCID string
	reason, origin   string
	createdAt        time.Time
}

// insertCommunityPostAction logs one community post action, scoped to and
// authored for its community, and returns its ID.
func insertCommunityPostAction(ctx context.Context, tx *sql.Tx, action communityPostAction) (string, error) {
	uri, err := syntax.ParseATURI(action.postURI)
	if err != nil {
		return "", fmt.Errorf("community post action subject %q: %w", action.postURI, err)
	}
	actionID := moderationActionClock.Next().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, admin_acting, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, observed_cid, action, reason, origin, created_at)
		VALUES ($1, $2, $3, $4, $5, $4, $6, $7, $4, NULLIF($8, ''), $9, $10, $11, $12)
	`, actionID, action.actorDID, action.adminActing, action.communityDID, moderation.ScopeCommunity,
		action.postURI, uri.Collection().String(), action.postCID, action.action, action.reason, action.origin,
		action.createdAt.UTC()); err != nil {
		return "", fmt.Errorf("insert community post action: %w", err)
	}
	if action.action == moderation.ActionRemove {
		if err := resolveCommunityReportsTx(ctx, tx, action.communityDID, action.postURI, action.actorDID); err != nil {
			return "", err
		}
	}
	return actionID, nil
}

// ApplyRemovalRecord indexes a community removal record's create or update
// from the firehose in one transaction, holding the rev-gate rows of the
// post's removal and acceptance records (the targets a post action locks, in
// the same order):
//   - the index already holds this record (community_post_removal_records at
//     the event's CID, code and reason, or, for a removal this AppView
//     stamped without knowing its CID, at the event's rev, or, for a removal
//     back-filled with neither, at the event's code and reason): it is a copy
//     of a change already logged; the CID (and a back-filled row's key and
//     rev) is filled in, and the removal applies at the event's watermark
//     only if the admission does not hold it yet (applyIndexedRemovalTx);
//   - an open remove intent of this instance wrote it (the CID of the body
//     its latest batch sent is the event's, its guard rev before the
//     event's): the intent completes with its actor at the event's rev, as
//     its local completion would;
//   - otherwise the removal applies under the watermark rule, advancing the
//     post's community version, and the record is indexed. One remove action
//     with no actor is logged (origin inherited, reason mapped from the code)
//     unless the instance removal outbox wrote the record: that removal's
//     instance action is its audit.
func (r *postgresAdmissionRepo) ApplyRemovalRecord(ctx context.Context, cmd posts.ApplyRemovalRecordCommand) (posts.AdmissionResult, error) {
	if err := validateWatermark("ApplyRemovalRecord", cmd.CommunityDID, cmd.PostURI, cmd.Watermark); err != nil {
		return posts.AdmissionResult{}, err
	}
	if cmd.RecordKey == "" || cmd.RecordCID == "" {
		return posts.AdmissionResult{}, fmt.Errorf("ApplyRemovalRecord for %s in %s: the removal record's key and CID are required", cmd.PostURI, cmd.CommunityDID)
	}
	var result posts.AdmissionResult
	err := inAdmissionTransaction(ctx, r.db, func(tx *sql.Tx) error {
		var err error
		result, err = applyRemovalRecordTx(ctx, tx, cmd)
		return err
	})
	if err != nil {
		return posts.AdmissionResult{}, fmt.Errorf("ApplyRemovalRecord for %s in %s: %w", cmd.PostURI, cmd.CommunityDID, err)
	}
	return result, nil
}

func applyRemovalRecordTx(ctx context.Context, tx *sql.Tx, cmd posts.ApplyRemovalRecordCommand) (posts.AdmissionResult, error) {
	removalURI := "at://" + cmd.CommunityDID + "/" + posts.RemovalCollection + "/" + cmd.RecordKey
	acceptanceURI := "at://" + cmd.CommunityDID + "/" + posts.AcceptanceCollection + "/" + cmd.RecordKey
	if err := lockCommunityAction(ctx, tx, moderation.CommunityActionLock{
		CommunityDID: cmd.CommunityDID, TargetURI: removalURI, OtherTargetURIs: []string{acceptanceURI},
	}); err != nil {
		return posts.AdmissionResult{}, err
	}
	// The open remove intent this record may complete resolves the post's
	// reports as its actor, so the actor's erasure gate comes before the
	// removal record and admission rows an erasure deletes.
	if err := gateRemovalIntentActors(ctx, tx, removalURI); err != nil {
		return posts.AdmissionResult{}, err
	}

	// A record's CID fixes its body, so the code and reason compared with it
	// only guard against an event that reuses a CID for another body. A
	// removal an AppView stamp indexed (ApplyRemoval) has no CID yet: the
	// commit at the stamped rev is the one the stamp indexed. A back-filled
	// removal (migration 088) has neither CID nor rev: the record with its
	// code and reason is the one the index holds.
	var indexed bool
	err := tx.QueryRowContext(ctx, `
		SELECT CASE
		           WHEN record_cid IS NOT NULL THEN record_cid = $3
		           WHEN rev IS NOT NULL THEN rev = $6
		           ELSE TRUE
		       END
		       AND code = $4 AND reason IS NOT DISTINCT FROM NULLIF($5, '')
		FROM community_post_removal_records
		WHERE community_did = $1 AND post_uri = $2
		FOR UPDATE
	`, cmd.CommunityDID, cmd.PostURI, cmd.RecordCID, cmd.DecisionCode, cmd.Reason, cmd.Watermark.Rev).Scan(&indexed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return posts.AdmissionResult{}, fmt.Errorf("read the indexed removal record: %w", err)
	}
	if indexed {
		if _, err := tx.ExecContext(ctx, `
			UPDATE community_post_removal_records
			SET record_key = $3, record_cid = $4, rev = COALESCE(rev, $5), updated_at = NOW()
			WHERE community_did = $1 AND post_uri = $2 AND record_cid IS NULL
		`, cmd.CommunityDID, cmd.PostURI, cmd.RecordKey, cmd.RecordCID, cmd.Watermark.Rev); err != nil {
			return posts.AdmissionResult{}, fmt.Errorf("fill the indexed removal record's CID: %w", err)
		}
		return applyIndexedRemovalTx(ctx, tx, cmd)
	}

	intent, err := openRemovalIntentFor(ctx, tx, removalURI, cmd.RecordCID, cmd.Watermark.Rev)
	if err != nil {
		return posts.AdmissionResult{}, err
	}
	if intent != nil {
		completion := communityActionTransaction{tx: tx}
		commit := moderation.CommunityCommit{
			Rev: cmd.Watermark.Rev, RecordCID: cmd.RecordCID, GateRev: cmd.Watermark.Rev, CompletedAt: time.Now(),
		}
		if _, err := completion.completePostIntent(ctx, *intent, commit, intent.IdempotencyRetention, true); err != nil {
			return posts.AdmissionResult{}, err
		}
		return currentAdmissionTx(ctx, tx, cmd.CommunityDID, cmd.PostURI, posts.AdmissionApplied)
	}

	result, err := compareAndSwapInTx(ctx, tx, "ApplyRemoval", cmd.CommunityDID, cmd.PostURI, communityEventOutcome, rowRequired, communityRecordChange,
		applyRemovalQuery(communityWatermarkGuard), &communityWithdrawal{rev: cmd.Watermark.Rev, removal: true},
		cmd.CommunityDID, cmd.PostURI, cmd.DecisionCode,
		cmd.Watermark.Rev, int16(posts.CommunityOpPut), cmd.Reason)
	if err != nil || result.Outcome != posts.AdmissionApplied {
		return result, err
	}
	if err := indexCommunityPostRemovalRecord(ctx, tx, communityPostRemovalRecord{
		communityDID: cmd.CommunityDID, postURI: cmd.PostURI, recordKey: cmd.RecordKey, recordCID: cmd.RecordCID,
		code: cmd.DecisionCode, reason: cmd.Reason, rev: cmd.Watermark.Rev,
	}); err != nil {
		return posts.AdmissionResult{}, err
	}
	outboxWrote, err := instanceOutboxWroteRemoval(ctx, tx, cmd)
	if err != nil || outboxWrote {
		return result, err
	}
	if _, err := insertCommunityPostAction(ctx, tx, communityPostAction{
		action: moderation.ActionRemove, communityDID: cmd.CommunityDID,
		postURI: cmd.PostURI, postCID: cmd.SubjectCID,
		reason: moderation.CommunityRemovalActionReason(cmd.DecisionCode), origin: moderation.OriginInherited,
		createdAt: time.Now(),
	}); err != nil {
		return posts.AdmissionResult{}, err
	}
	return result, nil
}

// applyIndexedRemovalTx applies a firehose copy of the removal record the
// index already holds. When the admission already holds the removal (its
// code and reason), nothing changes. Otherwise the removal applies under the
// watermark rule with no new action: the record's change was logged when it
// was indexed, but its admission change was refused as older there (a
// completion recovered at its guard rev after the commit's acceptance delete
// was indexed). The version advances when the decision changed, unless the
// commit's acceptance delete at this rev already advanced it.
func applyIndexedRemovalTx(ctx context.Context, tx *sql.Tx, cmd posts.ApplyRemovalRecordCommand) (posts.AdmissionResult, error) {
	before, err := admissionDecisionTx(ctx, tx, cmd.CommunityDID, cmd.PostURI)
	if err != nil {
		return posts.AdmissionResult{}, err
	}
	if before.status == string(posts.AdmissionStatusRemoved) && before.code == cmd.DecisionCode && before.removalReason == cmd.Reason {
		return currentAdmissionTx(ctx, tx, cmd.CommunityDID, cmd.PostURI, posts.AdmissionSkippedStale)
	}
	counted, err := otherHalfApplied(ctx, tx, cmd.CommunityDID, cmd.PostURI, cmd.Watermark.Rev)
	if err != nil {
		return posts.AdmissionResult{}, err
	}
	result, err := compareAndSwapInTx(ctx, tx, "ApplyRemoval", cmd.CommunityDID, cmd.PostURI, communityEventOutcome, rowRequired, versionAdvancedByCaller,
		applyRemovalQuery(communityWatermarkGuard), &communityWithdrawal{rev: cmd.Watermark.Rev, removal: true},
		cmd.CommunityDID, cmd.PostURI, cmd.DecisionCode,
		cmd.Watermark.Rev, int16(posts.CommunityOpPut), cmd.Reason)
	if err != nil || result.Outcome != posts.AdmissionApplied || counted {
		return result, err
	}
	after, err := admissionDecisionTx(ctx, tx, cmd.CommunityDID, cmd.PostURI)
	if err != nil {
		return posts.AdmissionResult{}, err
	}
	if after != before {
		if err := advanceCommunitySubjectVersion(ctx, tx, cmd.CommunityDID, cmd.PostURI); err != nil {
			return posts.AdmissionResult{}, err
		}
	}
	return result, nil
}

// otherHalfApplied reports whether the post's admission watermark is at rev's
// delete: the delete half of the commit at rev already applied, and counted
// the commit's version advance.
func otherHalfApplied(ctx context.Context, tx *sql.Tx, communityDID, postURI, rev string) (bool, error) {
	var applied bool
	err := tx.QueryRowContext(ctx, `
		SELECT last_community_rev IS NOT DISTINCT FROM $3 AND last_community_op_rank IS NOT DISTINCT FROM $4
		FROM community_post_admissions
		WHERE community_did = $1 AND post_uri = $2
	`, communityDID, postURI, rev, int16(posts.CommunityOpDelete)).Scan(&applied)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the community watermark of %s in %s: %w", postURI, communityDID, err)
	}
	return applied, nil
}

// intentStandBodyCID is the CID of the stand-record body the intent's latest
// recorded batch sent, else of fallback (the intent's own body) when no batch
// was recorded. The writer re-reads the stand record before each batch and
// keeps its createdAt, so the body sent can differ from the intent's.
func intentStandBodyCID(ctx context.Context, tx *sql.Tx, intentID string, fallback json.RawMessage) (string, error) {
	body := fallback
	var sent []byte
	err := tx.QueryRowContext(ctx, `
		SELECT stand_record FROM community_write_intent_attempts
		WHERE intent_id = $1
		ORDER BY id DESC
		LIMIT 1
	`, intentID).Scan(&sent)
	switch {
	case err == nil:
		body = sent
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("read the latest attempt of intent %s: %w", intentID, err)
	}
	bodyCID, err := recordBodyCID(body)
	if err != nil {
		return "", fmt.Errorf("the CID of intent %s's stand record: %w", intentID, err)
	}
	return bodyCID, nil
}

// openRemovalIntentFor is the open remove intent, locked, whose removal
// record body (as its latest batch sent it) has recordCID and whose guard rev
// is before rev: the intent whose commit a firehose removal record at
// (recordCID, rev) is. nil when there is none.
func openRemovalIntentFor(ctx context.Context, tx *sql.Tx, removalURI, recordCID, rev string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE target_uri = $1 AND state = 'open' AND op IN ('create', 'update') AND guard_rev < $2
		FOR UPDATE
	`, removalURI, rev))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the open removal intent on %s: %w", removalURI, err)
	}
	bodyCID, err := intentStandBodyCID(ctx, tx, intent.ID, intent.Record)
	if err != nil {
		return nil, err
	}
	if bodyCID != recordCID {
		return nil, nil
	}
	return &intent, nil
}

// InstanceOutboxWroteRemoval reports, in the action transaction, whether the
// instance removal outbox wrote the removal record standing in the
// community's repo, by the outbox's own ownership rule.
func (t *communityActionTransaction) InstanceOutboxWroteRemoval(ctx context.Context, communityDID string, removal posts.StandingRemoval) (bool, error) {
	return instanceOutboxWroteRemoval(ctx, t.tx, posts.ApplyRemovalRecordCommand{
		ApplyRemovalCommand: posts.ApplyRemovalCommand{
			CommunityDID: communityDID, PostURI: removal.SubjectURI, DecisionCode: removal.Code, Reason: removal.Reason,
		},
		RecordCID: removal.CID, SubjectCID: removal.SubjectCID, CreatedAt: removal.CreatedAt,
	})
}

// instanceOutboxWroteRemoval reports whether the instance removal outbox
// (migration 080) wrote this removal record: the record its row owns by CID,
// or, while a write of the row's may still be landing, a record whose content
// is exactly what that write sent (the outbox's own ownership rule).
func instanceOutboxWroteRemoval(ctx context.Context, tx *sql.Tx, cmd posts.ApplyRemovalRecordCommand) (bool, error) {
	var wrote bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM moderation_community_record_writes
			WHERE community_did = $1 AND subject_uri = $2
			  AND (owned_removal_cid = $3
			       OR (last_write_started_at IS NOT NULL AND $6 = ''
			           AND last_write_subject_cid = $4 AND last_write_code = $5
			           AND last_write_record_created_at = $7))
		)
	`, cmd.CommunityDID, cmd.PostURI, cmd.RecordCID, cmd.SubjectCID, cmd.DecisionCode, cmd.Reason, cmd.CreatedAt).Scan(&wrote)
	if err != nil {
		return false, fmt.Errorf("read whether the instance outbox wrote the removal of %s: %w", cmd.PostURI, err)
	}
	return wrote, nil
}

// ApplyRemovalRecordDelete indexes a community removal record's delete from
// the firehose in one transaction, holding the same rev-gate rows as
// ApplyRemovalRecord. The deleted record's post and code come from
// community_post_removal_records: the row under the record's key, else a row
// back-filled by migration 088 (no key) whose post's subject key is the
// record's. Then:
//   - no indexed record: the index already holds the delete (a local restore
//     dropped it, or the AppView's own restore stamp did); a post whose
//     acceptance stands under the key takes the watermark and nothing else
//     changes. Otherwise there is nothing to withdraw and "" is returned;
//   - an indexed record at a rev not before the delete's: a stale delete,
//     nothing changes;
//   - otherwise the removal's withdrawal applies under the watermark rule,
//     advancing the post's community version when it applies, the record is
//     dropped from the index, and one restore action with no actor is logged
//     with the deleted removal's mapped code (origin inherited), unless the
//     instance removal outbox's restore committed this delete. A delete does
//     not show whose commit it is, so an open restore intent of this
//     instance completes only from its commit's acceptance
//     (completeRestoreByAcceptanceTx); until then the inherited action awaits
//     it (awaitRestoreIntent).
func (r *postgresAdmissionRepo) ApplyRemovalRecordDelete(ctx context.Context, cmd posts.ApplyRemovalRecordDeleteCommand) (string, posts.AdmissionResult, error) {
	if cmd.Watermark.Rev == "" {
		return "", posts.AdmissionResult{}, fmt.Errorf("ApplyRemovalRecordDelete of %s in %s: %w: empty rev", cmd.RecordKey, cmd.CommunityDID, posts.ErrInvalidWatermark)
	}
	if cmd.RecordKey == "" {
		return "", posts.AdmissionResult{}, fmt.Errorf("ApplyRemovalRecordDelete in %s: the removal record's key is required", cmd.CommunityDID)
	}
	var (
		postURI string
		result  posts.AdmissionResult
	)
	err := inAdmissionTransaction(ctx, r.db, func(tx *sql.Tx) error {
		var err error
		postURI, result, err = applyRemovalRecordDeleteTx(ctx, tx, cmd)
		return err
	})
	if err != nil {
		return "", posts.AdmissionResult{}, fmt.Errorf("ApplyRemovalRecordDelete of %s in %s: %w", cmd.RecordKey, cmd.CommunityDID, err)
	}
	return postURI, result, nil
}

func applyRemovalRecordDeleteTx(ctx context.Context, tx *sql.Tx, cmd posts.ApplyRemovalRecordDeleteCommand) (string, posts.AdmissionResult, error) {
	removalURI := "at://" + cmd.CommunityDID + "/" + posts.RemovalCollection + "/" + cmd.RecordKey
	acceptanceURI := "at://" + cmd.CommunityDID + "/" + posts.AcceptanceCollection + "/" + cmd.RecordKey
	if err := lockCommunityAction(ctx, tx, moderation.CommunityActionLock{
		CommunityDID: cmd.CommunityDID, TargetURI: removalURI, OtherTargetURIs: []string{acceptanceURI},
	}); err != nil {
		return "", posts.AdmissionResult{}, err
	}
	rev := cmd.Watermark.Rev
	if err := advanceRemovalDeleteRev(ctx, tx, removalURI, rev); err != nil {
		return "", posts.AdmissionResult{}, err
	}

	record, err := indexedRemovalRecordByKey(ctx, tx, cmd.CommunityDID, cmd.RecordKey)
	if err != nil {
		return "", posts.AdmissionResult{}, err
	}
	if record == nil {
		postURI, err := acceptedPostByKey(ctx, tx, cmd.CommunityDID, cmd.RecordKey)
		if err != nil || postURI == "" {
			return "", posts.AdmissionResult{}, err
		}
		result, err := stampCommunityWatermarkTx(ctx, tx, cmd.CommunityDID, postURI, rev, posts.CommunityOpDelete)
		return postURI, result, err
	}
	postURI := record.postURI
	if record.rev != "" && record.rev >= rev {
		result, err := currentAdmissionTx(ctx, tx, cmd.CommunityDID, postURI, posts.AdmissionSkippedStale)
		return postURI, result, err
	}

	result, err := compareAndSwapInTx(ctx, tx, "ApplyRemovalDelete", cmd.CommunityDID, postURI, communityEventOutcome, rowRequired, communityRecordChange,
		applyRemovalDeleteQuery(communityWatermarkGuard), nil,
		cmd.CommunityDID, postURI, rev, int16(posts.CommunityOpDelete))
	if err != nil {
		return "", posts.AdmissionResult{}, err
	}
	if err := dropCommunityPostRemovalRecord(ctx, tx, cmd.CommunityDID, postURI, rev); err != nil {
		return "", posts.AdmissionResult{}, err
	}
	outboxRestored, err := instanceOutboxRestored(ctx, tx, cmd.CommunityDID, postURI, rev)
	if err != nil || outboxRestored {
		return postURI, result, err
	}
	actionID, err := insertCommunityPostAction(ctx, tx, communityPostAction{
		action: moderation.ActionRestore, communityDID: cmd.CommunityDID,
		postURI: postURI, postCID: record.postCID,
		reason: moderation.CommunityRemovalActionReason(record.code), origin: moderation.OriginInherited,
		createdAt: time.Now(),
	})
	if err != nil {
		return "", posts.AdmissionResult{}, err
	}
	if err := awaitRestoreIntent(ctx, tx, removalURI, rev, record.recordCID, actionID); err != nil {
		return "", posts.AdmissionResult{}, err
	}
	return postURI, result, nil
}

// indexedRemovalRecord is the part of a community_post_removal_records row a
// removal delete reads, with the indexed post's current CID.
type indexedRemovalRecord struct {
	postURI, postCID, code, rev, recordCID string
}

// indexedRemovalRecordByKey is the removal record the community's index holds
// under recordKey: the row carrying that key, else the back-filled row with
// no key whose post's subject key is recordKey. nil when there is none.
func indexedRemovalRecordByKey(ctx context.Context, tx *sql.Tx, communityDID, recordKey string) (*indexedRemovalRecord, error) {
	const columns = `r.post_uri, COALESCE(p.cid, ''), r.code, COALESCE(r.rev, ''), COALESCE(r.record_cid, '')
		FROM community_post_removal_records r
		LEFT JOIN posts p ON p.uri = r.post_uri`
	var record indexedRemovalRecord
	err := tx.QueryRowContext(ctx, `
		SELECT `+columns+`
		WHERE r.community_did = $1 AND r.record_key = $2
		LIMIT 1
	`, communityDID, recordKey).Scan(&record.postURI, &record.postCID, &record.code, &record.rev, &record.recordCID)
	if err == nil {
		return &record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read the indexed removal record %s in %s: %w", recordKey, communityDID, err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+columns+`
		WHERE r.community_did = $1 AND r.record_key IS NULL
	`, communityDID)
	if err != nil {
		return nil, fmt.Errorf("read the back-filled removal records of %s: %w", communityDID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := rows.Scan(&record.postURI, &record.postCID, &record.code, &record.rev, &record.recordCID); err != nil {
			return nil, fmt.Errorf("scan a back-filled removal record of %s: %w", communityDID, err)
		}
		if posts.SubjectRkey(record.postURI) == recordKey {
			return &record, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the back-filled removal records of %s: %w", communityDID, err)
	}
	return nil, nil
}

// acceptedPostByKey is the post whose acceptance stands in the community
// under recordKey (an acceptance and a removal of one post share its subject
// key), "" when there is none.
func acceptedPostByKey(ctx context.Context, tx *sql.Tx, communityDID, recordKey string) (string, error) {
	var postURI string
	err := tx.QueryRowContext(ctx, `
		SELECT post_uri FROM community_post_admissions WHERE community_did = $1 AND acceptance_rkey = $2
	`, communityDID, recordKey).Scan(&postURI)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the post accepted under %s in %s: %w", recordKey, communityDID, err)
	}
	return postURI, nil
}

// stampCommunityWatermarkTx advances the post's admission watermark to
// (rev, op) when that is newer, changing nothing else: a firehose copy of a
// change the index already holds.
func stampCommunityWatermarkTx(ctx context.Context, tx *sql.Tx, communityDID, postURI, rev string, op posts.CommunityOpRank) (posts.AdmissionResult, error) {
	if _, err := tx.ExecContext(ctx, `
		UPDATE community_post_admissions SET last_community_rev = $3, last_community_op_rank = $4, updated_at = NOW()
		WHERE community_did = $1 AND post_uri = $2
		  AND (last_community_rev IS NULL OR (last_community_rev, last_community_op_rank) < ($3::text, $4::smallint))
	`, communityDID, postURI, rev, int16(op)); err != nil {
		return posts.AdmissionResult{}, fmt.Errorf("stamp the community watermark of %s in %s: %w", postURI, communityDID, err)
	}
	return currentAdmissionTx(ctx, tx, communityDID, postURI, posts.AdmissionSkippedStale)
}

// openRestoreIntentFor is the open restore intent, locked, deleting the
// removal record at removalURI whose guard rev is before rev. nil when there
// is none.
func openRestoreIntentFor(ctx context.Context, tx *sql.Tx, removalURI, rev string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE target_uri = $1 AND state = 'open' AND op = 'delete' AND guard_rev < $2
		FOR UPDATE
	`, removalURI, rev))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the open restore intent on %s: %w", removalURI, err)
	}
	return &intent, nil
}

// awaitRestoreIntent marks the inherited restore action a removal record's
// delete at rev logged as awaiting the open restore intent on the record,
// with the delete's rev, when that intent's latest batch (a recorded attempt)
// was sent before rev and deletes the removal this delete withdrew (its CID,
// deletedCID, is the one the batch's pre-read found; a removal indexed with
// no CID is not compared). A delete alone does not show whose commit it is:
// only the commit's acceptance, the intent's stand record, does. If that
// acceptance arrives at the delete's rev (completeRestoreByAcceptanceTx), or a
// reconciliation finds the intent committed, the action is attributed to the
// intent's actor; otherwise it stays inherited.
func awaitRestoreIntent(ctx context.Context, tx *sql.Tx, removalURI, rev, deletedCID, actionID string) error {
	intent, err := openRestoreIntentFor(ctx, tx, removalURI, rev)
	if err != nil || intent == nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE community_write_intents SET action_id = $2, awaiting_delete_rev = $3
		WHERE id = $1 AND EXISTS (
			SELECT 1 FROM community_write_intent_attempts
			WHERE id = (SELECT MAX(id) FROM community_write_intent_attempts WHERE intent_id = $1)
			  AND clear_cid IS NOT NULL AND ($4 = '' OR clear_cid = $4)
		)
	`, intent.ID, actionID, rev, deletedCID); err != nil {
		return fmt.Errorf("mark the restore action awaiting intent %s: %w", intent.ID, err)
	}
	return nil
}

// awaitingRestoreDelete is the inherited restore action awaiting the open
// restore intent (awaitRestoreIntent) and the rev of the removal delete that
// logged it, "" when none is.
func awaitingRestoreDelete(ctx context.Context, tx *sql.Tx, intentID string) (string, string, error) {
	var actionID, rev string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(action_id, ''), COALESCE(awaiting_delete_rev, '') FROM community_write_intents WHERE id = $1 AND state = 'open'
	`, intentID).Scan(&actionID, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read the restore action awaiting intent %s: %w", intentID, err)
	}
	if rev == "" {
		return "", "", nil
	}
	return actionID, rev, nil
}

// attributeCommunityPostAction attributes an inherited community post action
// to the actor whose intent's commit it turned out to be.
func attributeCommunityPostAction(ctx context.Context, tx *sql.Tx, actionID, actorDID string, adminActing bool) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE moderation_actions SET actor_did = $2, admin_acting = $3, origin = $4
		WHERE id = $1 AND origin = $5
	`, actionID, actorDID, adminActing, moderation.OriginLocal, moderation.OriginInherited); err != nil {
		return fmt.Errorf("attribute community post action %s: %w", actionID, err)
	}
	return nil
}

// completeRestoreByAcceptanceTx completes the open restore intent whose
// commit a firehose acceptance is: an intent restoring the post under the
// acceptance's key, its guard rev before the acceptance's, whose acceptance
// body (as its latest batch sent it) has the acceptance's CID. The intent
// completes with its actor at the acceptance's rev, as its local completion
// would. nil when no such intent is open; the acceptance then applies as any
// other.
func completeRestoreByAcceptanceTx(ctx context.Context, tx *sql.Tx, cmd posts.ApplyAcceptanceCommand) (*posts.AdmissionResult, error) {
	removalURI := "at://" + cmd.CommunityDID + "/" + posts.RemovalCollection + "/" + cmd.AcceptanceRkey
	rev := cmd.Watermark.Rev
	var open bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM community_write_intents WHERE target_uri = $1 AND state = 'open' AND op = 'delete')
	`, removalURI).Scan(&open); err != nil {
		return nil, fmt.Errorf("read whether a restore intent is open on %s: %w", removalURI, err)
	}
	if !open {
		return nil, nil
	}
	if err := lockCommunityAction(ctx, tx, moderation.CommunityActionLock{
		CommunityDID: cmd.CommunityDID, TargetURI: removalURI, OtherTargetURIs: []string{cmd.AcceptanceURI},
	}); err != nil {
		return nil, err
	}
	intent, err := openRestoreIntentFor(ctx, tx, removalURI, rev)
	if err != nil || intent == nil {
		return nil, err
	}
	bodyCID, err := intentStandBodyCID(ctx, tx, intent.ID, intent.PairedRecord)
	if err != nil {
		return nil, err
	}
	if bodyCID != cmd.RecordCID || intent.PairedTargetURI != cmd.AcceptanceURI {
		return nil, nil
	}
	completion := communityActionTransaction{tx: tx}
	commit := moderation.CommunityCommit{Rev: rev, GateRev: rev, PairedRecordCID: cmd.RecordCID, CompletedAt: time.Now()}
	if _, err := completion.completePostIntent(ctx, *intent, commit, intent.IdempotencyRetention, true); err != nil {
		return nil, err
	}
	result, err := currentAdmissionTx(ctx, tx, cmd.CommunityDID, cmd.PostURI, posts.AdmissionApplied)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// instanceOutboxRestored reports whether the instance removal outbox's
// restore committed the delete at rev: the outbox row's owed restore stamp is
// that commit's.
func instanceOutboxRestored(ctx context.Context, tx *sql.Tx, communityDID, postURI, rev string) (bool, error) {
	var restored bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM moderation_community_record_writes
			WHERE community_did = $1 AND subject_uri = $2 AND restore_stamp_rev = $3
		)
	`, communityDID, postURI, rev).Scan(&restored)
	if err != nil {
		return false, fmt.Errorf("read whether the instance outbox restored %s: %w", postURI, err)
	}
	return restored, nil
}

// ReleaseRemovalRecord drops the removal record the index holds for the post
// when it is older than rev: the AppView's own commit at rev deleted it (the
// instance removal outbox's restore). The record's firehose delete then finds
// the index already holding it.
func (r *postgresAdmissionRepo) ReleaseRemovalRecord(ctx context.Context, communityDID, postURI, rev string) error {
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM community_post_removal_records
		WHERE community_did = $1 AND post_uri = $2 AND (rev IS NULL OR rev COLLATE "C" < $3 COLLATE "C")
	`, communityDID, postURI, rev); err != nil {
		return fmt.Errorf("release the removal record of %s in %s: %w", postURI, communityDID, err)
	}
	return nil
}

// currentAdmissionTx is the post's admission row, read in tx, with outcome.
func currentAdmissionTx(ctx context.Context, tx *sql.Tx, communityDID, postURI string, outcome posts.AdmissionOutcome) (posts.AdmissionResult, error) {
	admission, err := scanAdmission(tx.QueryRowContext(ctx, `
		SELECT `+admissionColumns+`
		FROM community_post_admissions
		WHERE community_did = $1 AND post_uri = $2
	`, communityDID, postURI))
	if errors.Is(err, sql.ErrNoRows) {
		return posts.AdmissionResult{Outcome: outcome}, nil
	}
	if err != nil {
		return posts.AdmissionResult{}, fmt.Errorf("read the admission of %s in %s: %w", postURI, communityDID, err)
	}
	return posts.AdmissionResult{Outcome: outcome, Admission: admission}, nil
}

// inAdmissionTransaction runs apply in one read-committed transaction on db
// and commits it when apply succeeds.
func inAdmissionTransaction(ctx context.Context, db *sql.DB, apply func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := apply(tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
