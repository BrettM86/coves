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
// then reconciles the removals it may affect. Of the removal, the comment and
// the thread root, the one committed last therefore sees the other two.
// reconcile locks the removal row before resolving the comment's thread, so
// concurrent reconciles of one removal run one after the other, each against
// the state and thread the previous one committed: an application cannot
// happen twice, and one racing a re-creation of the comment elsewhere is
// demoted by the re-creation's reconcile.
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
		// An update under the same key keeps its state; one naming another
		// subject is refused, since the record key is derived from the subject.
		result, err := tx.ExecContext(ctx, `
			INSERT INTO community_comment_removals
			    (repository_did, record_key, record_cid, subject_uri, subject_cid, state)
			VALUES ($1, $2, $3, $4, $5, 'held')
			ON CONFLICT (repository_did, record_key) DO UPDATE SET
			    record_cid = EXCLUDED.record_cid, subject_cid = EXCLUDED.subject_cid, updated_at = NOW()
			WHERE community_comment_removals.subject_uri = EXCLUDED.subject_uri
		`, removal.RepositoryDID, removal.RecordKey, removal.RecordCID, removal.SubjectURI, removal.SubjectCID)
		if err != nil {
			return fmt.Errorf("index community comment removal: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check community comment removal index: %w", err)
		}
		if count == 0 {
			outcome = moderation.CommunityCommentRemovalRejected
		}
		return nil
	})
	if err != nil || outcome == moderation.CommunityCommentRemovalRejected {
		return outcome, err
	}
	// A stale replay still reconciles, so a crash between the index commit and
	// the application, or between a comment's re-creation and its reconcile,
	// is repaired by redelivery.
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
		var subjectURI, state string
		var applyActionID sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT subject_uri, state, apply_action_id FROM community_comment_removals
			WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
		`, repositoryDID, recordKey).Scan(&subjectURI, &state, &applyActionID)
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
		_, err = retractCommunityRemoval(ctx, tx, repositoryDID, subjectURI, applyActionID.String, repositoryDID, "")
		return err
	})
	return outcome, err
}

// retractCommunityRemoval logs a retract-removal of the apply-removal
// applyActionID, attributed to actorDID with reason, ends the decision that
// apply-removal made active, and returns the retract-removal's ID. A decision
// another action has since taken over is left alone.
func retractCommunityRemoval(
	ctx context.Context, tx *sql.Tx, communityDID, subjectURI, applyActionID, actorDID, reason string,
) (string, error) {
	moderationTx := &moderationTransaction{tx: tx}
	applied, err := moderationTx.GetAction(ctx, applyActionID)
	if err != nil {
		return "", fmt.Errorf("read applied community removal: %w", err)
	}
	retract, err := moderationTx.InsertAction(ctx, moderation.Action{
		ActorDID: actorDID, AuthorityDID: communityDID,
		ScopeKind: moderation.ScopeCommunity, ScopeCommunityDID: communityDID,
		SubjectURI: subjectURI, SubjectCollection: applied.SubjectCollection,
		SubjectCommunityDID: applied.SubjectCommunityDID, SubjectRootURI: applied.SubjectRootURI,
		Action: moderation.ActionRetractRemoval, Reason: reason, ReversesActionID: applyActionID,
		Origin: moderation.OriginInherited, CreatedAt: time.Now(),
	})
	if err != nil {
		return "", fmt.Errorf("log community retract-removal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE moderation_decisions SET active = FALSE
		WHERE authority_did = $1 AND scope_kind = 'community' AND scope_community_did = $1
		  AND subject_uri = $2 AND kind = 'removal' AND active_action_id = $3 AND active
	`, communityDID, subjectURI, applyActionID); err != nil {
		return "", fmt.Errorf("deactivate community removal decision: %w", err)
	}
	return retract.ID, nil
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

// threadRootCommunity resolves the post a comment is shown under: the post
// its parent chain reaches, since threads are built from parent_uri. The
// comment's own root_uri is commenter-chosen and never checked against the
// chain, so it is not used. resolved is false while any link of the chain (a
// parent comment or the post) is unindexed, or the chain never reaches a post
// (a cycle).
func threadRootCommunity(ctx context.Context, tx *sql.Tx, subjectURI string) (rootURI, communityDID string, resolved bool, err error) {
	var root, community sql.NullString
	err = tx.QueryRowContext(ctx, `
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
		var subjectURI, subjectCID, state string
		var applyActionID, movedActionID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT subject_uri, subject_cid, state, apply_action_id, moved_action_id
			FROM community_comment_removals
			WHERE repository_did = $1 AND record_key = $2 FOR UPDATE
		`, repositoryDID, recordKey).Scan(&subjectURI, &subjectCID, &state, &applyActionID, &movedActionID)
		if errors.Is(err, sql.ErrNoRows) {
			outcome = moderation.CommunityCommentRemovalNotFound
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock community comment removal: %w", err)
		}
		rootURI, rootCommunityDID, resolved, err := threadRootCommunity(ctx, tx, subjectURI)
		if err != nil {
			return err
		}
		switch {
		case state == "applied" && resolved && rootCommunityDID != repositoryDID:
			outcome = moderation.CommunityCommentRemovalDemoted
			return demoteRemoval(ctx, tx, repositoryDID, recordKey, subjectURI, applyActionID.String)
		case state == "applied":
			outcome = moderation.CommunityCommentRemovalApplied
			return nil
		case !resolved:
			outcome = moderation.CommunityCommentRemovalHeld
			return nil
		case rootCommunityDID != repositoryDID:
			outcome = moderation.CommunityCommentRemovalRejected
			return nil
		}
		outcome = moderation.CommunityCommentRemovalApplied
		return applyRemoval(ctx, tx, repositoryDID, recordKey, subjectURI, subjectCID, rootURI, movedActionID.String)
	})
	return outcome, err
}

// demoteRemoval retracts an applied removal whose comment moved to another
// community's thread and returns it to held. The retract-removal is a system
// action: no actor, reason reasonSubjectMoved.
func demoteRemoval(ctx context.Context, tx *sql.Tx, repositoryDID, recordKey, subjectURI, applyActionID string) error {
	retractID, err := retractCommunityRemoval(ctx, tx, repositoryDID, subjectURI, applyActionID,
		"", moderation.ReasonSubjectMoved)
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
	return nil
}

// applyRemoval logs the apply-removal, activates the community-scope decision
// and marks the removal applied. The first application is the community's
// act. A re-application after the comment came back (movedActionID set) is a
// system action reversing the demotion: no actor, reason reasonSubjectMoved.
func applyRemoval(
	ctx context.Context, tx *sql.Tx, repositoryDID, recordKey, subjectURI, subjectCID, rootURI, movedActionID string,
) error {
	actorDID, reason := repositoryDID, ""
	if movedActionID != "" {
		actorDID, reason = "", moderation.ReasonSubjectMoved
	}
	action, err := (&moderationTransaction{tx: tx}).InsertAction(ctx, moderation.Action{
		ActorDID: actorDID, AuthorityDID: repositoryDID,
		ScopeKind: moderation.ScopeCommunity, ScopeCommunityDID: repositoryDID,
		SubjectURI: subjectURI, SubjectCollection: moderation.CommentCollection,
		SubjectCommunityDID: repositoryDID, SubjectRootURI: rootURI, ObservedCID: subjectCID,
		Action: moderation.ActionApplyRemoval, Reason: reason, ReversesActionID: movedActionID,
		Origin: moderation.OriginInherited, CreatedAt: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("log community apply-removal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'community', $1, $2, 'removal', NULL, $3, TRUE)
		ON CONFLICT ON CONSTRAINT moderation_decisions_key DO UPDATE SET
		    active_action_id = EXCLUDED.active_action_id, active = TRUE
	`, repositoryDID, subjectURI, action.ID); err != nil {
		return fmt.Errorf("activate community removal decision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE community_comment_removals
		SET state = 'applied', apply_action_id = $3, moved_action_id = NULL, updated_at = NOW()
		WHERE repository_did = $1 AND record_key = $2
	`, repositoryDID, recordKey, action.ID); err != nil {
		return fmt.Errorf("mark community comment removal applied: %w", err)
	}
	return nil
}
