package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
)

// CommunityCommentRemovalRepository is the Postgres
// moderation.CommunityCommentRemovalStore.
//
// Each entry point commits its own write (the removal row, or, for the
// comment and root paths, the consumer's index write before the call) and only
// then reconciles the removals it may affect. A removal record also reconciles
// once inside its own transaction, holding a lock on its subject that the
// comment and root paths wait for before finding removals, so the record's
// row is visible to them. Of the removal, the comment and
// the thread root, the one committed last therefore sees the other two.
// reconcile locks the removal row before resolving the comment's thread, so
// concurrent reconciles of one removal run one after the other, each against
// the state and thread the previous one committed: an application cannot
// happen twice, and one racing a re-creation of the comment elsewhere is
// demoted by the re-creation's reconcile. Each change to the effective
// decision (apply, demote, retract) advances the comment's community version
// in the transaction that makes it.
type CommunityCommentRemovalRepository struct {
	db *sql.DB
}

// NewCommunityCommentRemovalRepository builds the repository.
func NewCommunityCommentRemovalRepository(db *sql.DB) *CommunityCommentRemovalRepository {
	return &CommunityCommentRemovalRepository{db: db}
}

var _ moderation.CommunityCommentRemovalStore = (*CommunityCommentRemovalRepository)(nil)

func (r *CommunityCommentRemovalRepository) inTransaction(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// advanceRemovalRev is the per-record rev gate (jetstream_record_revs) for a
// removal record: false means an equal or newer event already applied, such as
// a cross-feed copy of a create arriving after its delete.
func advanceRemovalRev(ctx context.Context, tx *sql.Tx, repositoryDID, recordKey, rev string) (bool, error) {
	if rev == "" {
		return true, nil
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)
		ON CONFLICT (record_uri) DO UPDATE SET rev = EXCLUDED.rev, updated_at = NOW()
		WHERE jetstream_record_revs.rev < EXCLUDED.rev
	`, "at://"+repositoryDID+"/"+posts.RemovalCollection+"/"+recordKey, rev)
	if err != nil {
		return false, fmt.Errorf("advance removal record rev: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check removal record rev advance: %w", err)
	}
	return count > 0, nil
}

// RecordRemoval indexes the record as held, then reconciles it.
func (r *CommunityCommentRemovalRepository) RecordRemoval(
	ctx context.Context, removal moderation.CommunityCommentRemoval,
) (moderation.CommunityCommentRemovalOutcome, error) {
	var outcome moderation.CommunityCommentRemovalOutcome
	err := r.inTransaction(ctx, func(tx *sql.Tx) error {
		advanced, err := advanceRemovalRev(ctx, tx, removal.RepositoryDID, removal.RecordKey, removal.Rev)
		if err != nil {
			return err
		}
		if !advanced {
			outcome = moderation.CommunityCommentRemovalStale
			return nil
		}
		// The open remove intent this record may complete resolves the
		// comment's reports as its actor, so the actor's erasure gate comes
		// before the comment rows an erasure locks. The rev gate held above
		// keeps the set of open intents on the record fixed.
		if removal.Rev != "" {
			removalURI := "at://" + removal.RepositoryDID + "/" + posts.RemovalCollection + "/" + removal.RecordKey
			if err := gateRemovalIntentActors(ctx, tx, removalURI); err != nil {
				return err
			}
		}
		if err := lockRemovalSubject(ctx, tx, removal.SubjectURI); err != nil {
			return err
		}
		// A copy of the commit an open comment remove of this instance wrote
		// completes that intent with its actor, as its local completion would.
		completed, err := completeCommentRemoveIntentFromEvent(ctx, tx, removal)
		if err != nil || completed {
			outcome = moderation.CommunityCommentRemovalApplied
			return err
		}
		// The row as it was before this record, to tell a rewrite of an
		// applied removal, which reconcile leaves applied, from no change.
		var priorRecordCID, priorSubjectCID, priorState string
		err = tx.QueryRowContext(ctx, `
			SELECT record_cid, subject_cid, state FROM community_comment_removals
			WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
		`, removal.RepositoryDID, removal.RecordKey).Scan(&priorRecordCID, &priorSubjectCID, &priorState)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read community comment removal: %w", err)
		}
		// An update under the same key keeps its state; one naming another
		// subject is refused, since the record key is derived from the subject.
		result, err := tx.ExecContext(ctx, `
			INSERT INTO community_comment_removals
			    (repository_did, record_key, record_cid, subject_uri, subject_cid, state, code, reason)
			VALUES ($1, $2, $3, $4, $5, 'held', NULLIF($6, ''), NULLIF($7, ''))
			ON CONFLICT (repository_did, record_key) DO UPDATE SET
			    record_cid = EXCLUDED.record_cid, subject_cid = EXCLUDED.subject_cid,
			    code = EXCLUDED.code, reason = EXCLUDED.reason, updated_at = NOW()
			WHERE community_comment_removals.subject_uri = EXCLUDED.subject_uri
		`, removal.RepositoryDID, removal.RecordKey, removal.RecordCID, removal.SubjectURI, removal.SubjectCID,
			removal.Code, removal.Reason)
		if err != nil {
			return fmt.Errorf("index community comment removal: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check community comment removal index: %w", err)
		}
		if count == 0 {
			outcome = moderation.CommunityCommentRemovalRejected
			return nil
		}
		// The first reconcile runs in the record's transaction, so applying
		// the removal to an already-indexed comment commits, and advances the
		// comment's version, together with the record.
		reconciled, err := reconcileTx(ctx, tx, removal.RepositoryDID, removal.RecordKey)
		if err != nil {
			return err
		}
		// A changed record for a removal that was already applied and stays
		// applied is a rewrite, as on the local rewrite path: it logs an
		// actorless apply-removal with the new record's mapped code, moves the
		// active decision to it and advances the version once. A first
		// application or a demotion has already advanced it in reconcileTx.
		if priorState == "applied" && reconciled == moderation.CommunityCommentRemovalApplied &&
			(priorRecordCID != removal.RecordCID || priorSubjectCID != removal.SubjectCID) {
			rootURI, _, _, err := threadRootCommunity(ctx, tx, removal.SubjectURI)
			if err != nil {
				return err
			}
			_, err = applyRemoval(ctx, tx, commentRemovalApplication{
				repositoryDID: removal.RepositoryDID, recordKey: removal.RecordKey,
				subjectURI: removal.SubjectURI, subjectCID: removal.SubjectCID, rootURI: rootURI,
				reason: moderation.CommunityRemovalActionReason(removal.Code), actor: inheritedCommentRemovalActor(),
			})
			return err
		}
		return nil
	})
	if err != nil || outcome == moderation.CommunityCommentRemovalRejected {
		return outcome, err
	}
	// The record is reconciled again after commit: a comment indexed while the
	// record transaction ran is seen here. A stale replay still reconciles, so
	// a crash between a comment's re-creation and its reconcile is repaired by
	// redelivery. A reconcile that changes nothing advances nothing.
	reconciled, err := r.reconcile(ctx, removal.RepositoryDID, removal.RecordKey)
	if err != nil {
		return "", err
	}
	if outcome == moderation.CommunityCommentRemovalStale && reconciled == moderation.CommunityCommentRemovalNotFound {
		return outcome, nil
	}
	return reconciled, nil
}

// DeleteRemoval discards a held removal or retracts an applied one.
func (r *CommunityCommentRemovalRepository) DeleteRemoval(
	ctx context.Context, repositoryDID, recordKey, rev string,
) (moderation.CommunityCommentRemovalOutcome, error) {
	var outcome moderation.CommunityCommentRemovalOutcome
	err := r.inTransaction(ctx, func(tx *sql.Tx) error {
		// The rev gate is taken before the removal row, the same order as
		// RecordRemoval, so a concurrent create and delete of one record
		// cannot deadlock. The delete's rev is recorded even when no removal
		// is indexed, so an older create processed after it (redrive,
		// cross-feed reorder) is refused as stale. The post-removal path
		// gates on admission rows, not this table.
		advanced, err := advanceRemovalRev(ctx, tx, repositoryDID, recordKey, rev)
		if err != nil {
			return err
		}
		var subjectURI, recordCID, state, code string
		var applyActionID sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT subject_uri, record_cid, state, apply_action_id, COALESCE(code, '') FROM community_comment_removals
			WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
		`, repositoryDID, recordKey).Scan(&subjectURI, &recordCID, &state, &applyActionID, &code)
		if errors.Is(err, sql.ErrNoRows) {
			outcome = moderation.CommunityCommentRemovalNotFound
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock community comment removal: %w", err)
		}
		if !advanced {
			outcome = moderation.CommunityCommentRemovalStale
			return nil
		}
		// A copy of the commit an open comment restore of this instance wrote
		// completes that intent with its actor, as its local completion would.
		completed, err := completeCommentRestoreIntentFromEvent(ctx, tx, repositoryDID, recordKey, subjectURI, recordCID, rev)
		if err != nil || completed {
			outcome = moderation.CommunityCommentRemovalRetracted
			if completed && state != "applied" {
				outcome = moderation.CommunityCommentRemovalDiscarded
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM community_comment_removals WHERE repository_did = $1 AND record_key = $2
		`, repositoryDID, recordKey); err != nil {
			return fmt.Errorf("delete community comment removal: %w", err)
		}
		if state != "applied" {
			outcome = moderation.CommunityCommentRemovalDiscarded
			return nil
		}
		outcome = moderation.CommunityCommentRemovalRetracted
		if _, err := retractCommunityRemoval(ctx, tx, repositoryDID, subjectURI, applyActionID.String,
			moderation.CommunityRemovalActionReason(code), inheritedCommentRemovalActor()); err != nil {
			return err
		}
		return advanceCommunitySubjectVersion(ctx, tx, repositoryDID, subjectURI)
	})
	return outcome, err
}

// commentRemovalActor is whom a community comment removal action names: the
// moderator (or acting instance admin) of a local action, or no one for a
// change indexed from the community's repo with no intent of this instance
// behind it, and for the subject-moved system actions.
type commentRemovalActor struct {
	actorDID    string
	adminActing bool
	origin      string
	createdAt   time.Time
}

// inheritedCommentRemovalActor names no actor: a change indexed from the
// community's repo.
func inheritedCommentRemovalActor() commentRemovalActor {
	return commentRemovalActor{origin: moderation.OriginInherited, createdAt: time.Now()}
}

// commentRemovalAction is one apply-removal or retract-removal of a comment
// in its community's log.
type commentRemovalAction struct {
	action                  string
	communityDID            string
	subjectURI, observedCID string
	rootURI                 string
	reason, reversesID      string
	actor                   commentRemovalActor
}

// insertCommentRemovalAction logs one community comment removal action,
// scoped to and authored for its community, and returns its ID.
func insertCommentRemovalAction(ctx context.Context, tx *sql.Tx, action commentRemovalAction) (string, error) {
	actionID := moderationActionClock.Next().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, admin_acting, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, subject_root_uri, observed_cid,
		     action, reason, reverses_action_id, origin, created_at)
		VALUES ($1, $2, $3, $4, $5, $4, $6, $7, $4, NULLIF($8, ''), NULLIF($9, ''),
		        $10, NULLIF($11, ''), NULLIF($12, ''), $13, $14)
	`, actionID, action.actor.actorDID, action.actor.adminActing, action.communityDID, moderation.ScopeCommunity,
		action.subjectURI, moderation.CommentCollection, action.rootURI, action.observedCID,
		action.action, action.reason, action.reversesID, action.actor.origin,
		action.actor.createdAt.UTC().Truncate(time.Microsecond)); err != nil {
		return "", fmt.Errorf("log community %s: %w", action.action, err)
	}
	if action.action == moderation.ActionApplyRemoval {
		if err := resolveCommunityReportsTx(ctx, tx, action.communityDID, action.subjectURI, action.actor.actorDID); err != nil {
			return "", err
		}
	}
	return actionID, nil
}

// retractCommunityRemoval logs a retract-removal of the apply-removal
// applyActionID, attributed to actor with reason, ends the decision that
// apply-removal made active, and returns the retract-removal's ID. A decision
// another action has since taken over is left alone.
func retractCommunityRemoval(
	ctx context.Context, tx *sql.Tx, communityDID, subjectURI, applyActionID, reason string, actor commentRemovalActor,
) (string, error) {
	applied, err := (&moderationTransaction{tx: tx}).GetAction(ctx, applyActionID)
	if err != nil {
		return "", fmt.Errorf("read applied community removal: %w", err)
	}
	retractID, err := insertCommentRemovalAction(ctx, tx, commentRemovalAction{
		action: moderation.ActionRetractRemoval, communityDID: communityDID,
		subjectURI: subjectURI, rootURI: applied.SubjectRootURI,
		reason: reason, reversesID: applyActionID, actor: actor,
	})
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE moderation_decisions SET active = FALSE
		WHERE authority_did = $1 AND scope_kind = 'community' AND scope_community_did = $1
		  AND subject_uri = $2 AND kind = 'removal' AND active_action_id = $3 AND active
	`, communityDID, subjectURI, applyActionID); err != nil {
		return "", fmt.Errorf("deactivate community removal decision: %w", err)
	}
	return retractID, nil
}

// atOrBelowQuery finds removals, held or applied, of $1 and of every comment
// whose parent chain passes through $1: the removals whose thread the arrival
// or re-creation of $1 may have completed or moved.
const atOrBelowQuery = `
	WITH RECURSIVE thread (uri) AS (
	    SELECT $1::text
	    UNION
	    SELECT c.uri FROM comments c JOIN thread ON c.parent_uri = thread.uri
	)
	SELECT r.repository_did, r.record_key FROM community_comment_removals r
	JOIN thread ON thread.uri = r.subject_uri`

// ApplyHeldForComment reconciles every removal of the comment and of comments
// shown beneath it.
func (r *CommunityCommentRemovalRepository) ApplyHeldForComment(ctx context.Context, commentURI string) error {
	return r.reconcileAtOrBelow(ctx, commentURI)
}

// ApplyHeldForRoot reconciles every removal of comments shown under the post.
func (r *CommunityCommentRemovalRepository) ApplyHeldForRoot(ctx context.Context, rootURI string) error {
	return r.reconcileAtOrBelow(ctx, rootURI)
}

func (r *CommunityCommentRemovalRepository) reconcileAtOrBelow(ctx context.Context, uri string) error {
	// Wait for a removal record of uri still being indexed, so its row is
	// visible below and reconciled here against the thread this call follows.
	if err := r.inTransaction(ctx, func(tx *sql.Tx) error {
		return lockRemovalSubject(ctx, tx, uri)
	}); err != nil {
		return err
	}
	keys, err := r.matchingRemovals(ctx, atOrBelowQuery, uri)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := r.reconcile(ctx, key.repositoryDID, key.recordKey); err != nil {
			return err
		}
	}
	return nil
}

// lockRemovalSubject takes the transaction advisory lock a removal record
// holds on its subject while it is indexed and first reconciled.
func lockRemovalSubject(ctx context.Context, tx *sql.Tx, subjectURI string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('community-comment-removal-subject:' || $1))`, subjectURI); err != nil {
		return fmt.Errorf("lock community comment removal subject %s: %w", subjectURI, err)
	}
	return nil
}

type removalKey struct{ repositoryDID, recordKey string }

func (r *CommunityCommentRemovalRepository) matchingRemovals(ctx context.Context, query, argument string) ([]removalKey, error) {
	rows, err := r.db.QueryContext(ctx, query, argument)
	if err != nil {
		return nil, fmt.Errorf("find community comment removals: %w", err)
	}
	var keys []removalKey
	for rows.Next() {
		var key removalKey
		if err := rows.Scan(&key.repositoryDID, &key.recordKey); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan community comment removal: %w", err)
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read community comment removals: %w", err)
	}
	return keys, nil
}

// threadQuerier reads a comment's thread from a pool or a transaction.
type threadQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// threadRootCommunity resolves the post a comment is shown under: the post
// its parent chain reaches, since threads are built from parent_uri. The
// comment's own root_uri is commenter-chosen and never checked against the
// chain, so it is not used. resolved is false while any link of the chain (a
// parent comment or the post) is unindexed, or the chain never reaches a post
// (a cycle).
func threadRootCommunity(ctx context.Context, querier threadQuerier, subjectURI string) (rootURI, communityDID string, resolved bool, err error) {
	var root, community sql.NullString
	err = querier.QueryRowContext(ctx, `
		WITH RECURSIVE chain (uri, parent_uri) AS (
		    SELECT uri, parent_uri FROM comments WHERE uri = $1
		    UNION
		    SELECT c.uri, c.parent_uri FROM comments c JOIN chain ON c.uri = chain.parent_uri
		)
		SELECT chain.parent_uri, p.community_did FROM chain
		LEFT JOIN posts p ON p.uri = chain.parent_uri
		WHERE NOT EXISTS (SELECT 1 FROM comments c WHERE c.uri = chain.parent_uri)
	`, subjectURI).Scan(&root, &community)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !community.Valid) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read the thread root of %s: %w", subjectURI, err)
	}
	return root.String, community.String, true, nil
}

// reconcile locks one removal row, then resolves the comment's thread under
// that lock and brings the removal in line with it:
//
//   - held, and the thread resolves to the record's repo: apply it.
//   - applied, and the thread resolves to another community (the comment was
//     re-created under that community's post): demote it to held with a
//     system retract-removal, so it re-applies if the comment comes back.
//   - otherwise leave it: held and unresolved reports Held, held under another
//     community reports Rejected, applied with the chain unresolved stays
//     applied (a comment re-created with a dangling parent must not reappear).
//
// Taking the row lock before resolving the thread means a concurrent
// reconcile of the same removal is waited on, and the decision is made
// against the state and thread it committed.
func (r *CommunityCommentRemovalRepository) reconcile(
	ctx context.Context, repositoryDID, recordKey string,
) (moderation.CommunityCommentRemovalOutcome, error) {
	var outcome moderation.CommunityCommentRemovalOutcome
	err := r.inTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		outcome, err = reconcileTx(ctx, tx, repositoryDID, recordKey)
		return err
	})
	return outcome, err
}

// reconcileTx is reconcile inside tx. Applying or demoting the removal
// advances the comment's community version in tx; leaving it as it is
// advances nothing.
func reconcileTx(
	ctx context.Context, tx *sql.Tx, repositoryDID, recordKey string,
) (moderation.CommunityCommentRemovalOutcome, error) {
	var subjectURI, subjectCID, state, code string
	var applyActionID, movedActionID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT subject_uri, subject_cid, state, apply_action_id, moved_action_id, COALESCE(code, '')
		FROM community_comment_removals
		WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
	`, repositoryDID, recordKey).Scan(&subjectURI, &subjectCID, &state, &applyActionID, &movedActionID, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return moderation.CommunityCommentRemovalNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("lock community comment removal: %w", err)
	}
	rootURI, rootCommunityDID, resolved, err := threadRootCommunity(ctx, tx, subjectURI)
	if err != nil {
		return "", err
	}
	switch {
	case state == "applied" && resolved && rootCommunityDID != repositoryDID:
		return moderation.CommunityCommentRemovalDemoted, demoteRemoval(ctx, tx, repositoryDID, recordKey, subjectURI, applyActionID.String)
	case state == "applied":
		return moderation.CommunityCommentRemovalApplied, nil
	case !resolved:
		return moderation.CommunityCommentRemovalHeld, nil
	case rootCommunityDID != repositoryDID:
		return moderation.CommunityCommentRemovalRejected, nil
	}
	// The first application has no actor and the record's mapped code; a
	// re-application after the comment came back (movedActionID set) is a
	// system action reversing the demotion.
	reason := moderation.CommunityRemovalActionReason(code)
	if movedActionID.Valid {
		reason = moderation.ReasonSubjectMoved
	}
	_, err = applyRemoval(ctx, tx, commentRemovalApplication{
		repositoryDID: repositoryDID, recordKey: recordKey, subjectURI: subjectURI, subjectCID: subjectCID,
		rootURI: rootURI, reason: reason, reversesID: movedActionID.String, actor: inheritedCommentRemovalActor(),
	})
	return moderation.CommunityCommentRemovalApplied, err
}

// demoteRemoval retracts an applied removal whose comment moved to another
// community's thread and returns it to held. The retract-removal is a system
// action: no actor, reason reasonSubjectMoved.
func demoteRemoval(ctx context.Context, tx *sql.Tx, repositoryDID, recordKey, subjectURI, applyActionID string) error {
	retractID, err := retractCommunityRemoval(ctx, tx, repositoryDID, subjectURI, applyActionID,
		moderation.ReasonSubjectMoved, inheritedCommentRemovalActor())
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE community_comment_removals
		SET state = 'held', apply_action_id = NULL, moved_action_id = $3, updated_at = NOW()
		WHERE repository_did = $1 AND record_key = $2
	`, repositoryDID, recordKey, retractID); err != nil {
		return fmt.Errorf("mark community comment removal held: %w", err)
	}
	return advanceCommunitySubjectVersion(ctx, tx, repositoryDID, subjectURI)
}

// commentRemovalApplication is one application of a held comment removal:
// the removal row, the thread root its comment resolved to, and the
// apply-removal's reason, reversed action and actor.
type commentRemovalApplication struct {
	repositoryDID, recordKey string
	subjectURI, subjectCID   string
	rootURI                  string
	reason, reversesID       string
	actor                    commentRemovalActor
}

// applyRemoval logs the apply-removal, activates the community-scope decision,
// marks the removal applied and advances the comment's community version. It
// returns the apply-removal's ID.
func applyRemoval(ctx context.Context, tx *sql.Tx, application commentRemovalApplication) (string, error) {
	actionID, err := insertCommentRemovalAction(ctx, tx, commentRemovalAction{
		action: moderation.ActionApplyRemoval, communityDID: application.repositoryDID,
		subjectURI: application.subjectURI, observedCID: application.subjectCID, rootURI: application.rootURI,
		reason: application.reason, reversesID: application.reversesID, actor: application.actor,
	})
	if err != nil {
		return "", err
	}
	if err := activateCommentRemoval(ctx, tx, application.repositoryDID, application.recordKey, application.subjectURI, actionID); err != nil {
		return "", err
	}
	return actionID, advanceCommunitySubjectVersion(ctx, tx, application.repositoryDID, application.subjectURI)
}

// activateCommentRemoval points the community-scope removal decision and the
// removal row at the apply-removal actionID, active.
func activateCommentRemoval(ctx context.Context, tx *sql.Tx, repositoryDID, recordKey, subjectURI, actionID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'community', $1, $2, 'removal', NULL, $3, TRUE)
		ON CONFLICT ON CONSTRAINT moderation_decisions_key DO UPDATE SET
		    active_action_id = EXCLUDED.active_action_id, active = TRUE
	`, repositoryDID, subjectURI, actionID); err != nil {
		return fmt.Errorf("activate community removal decision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE community_comment_removals
		SET state = 'applied', apply_action_id = $3, moved_action_id = NULL, updated_at = NOW()
		WHERE repository_did = $1 AND record_key = $2
	`, repositoryDID, recordKey, actionID); err != nil {
		return fmt.Errorf("mark community comment removal applied: %w", err)
	}
	return nil
}
