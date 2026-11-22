package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"Coves/internal/core/classification"
	"Coves/internal/core/posts"

	"github.com/lib/pq"
)

// classificationNonTerminalSQL is the SQL list of the non-terminal job
// statuses: the ones a worker may claim and change.
var classificationNonTerminalSQL = fmt.Sprintf(`('%s', '%s', '%s')`,
	classification.StatusQueued, classification.StatusDeferred, classification.StatusRetrying)

// ClassificationRepository is the classification queue in Postgres.
type ClassificationRepository struct {
	db          *sql.DB
	instanceDID string
}

var _ classification.Repository = (*ClassificationRepository)(nil)

// NewClassificationRepository returns the classification queue repository.
// instanceDID is the authority whose removals and account bans hide subjects.
func NewClassificationRepository(db *sql.DB, instanceDID string) *ClassificationRepository {
	return &ClassificationRepository{db: db, instanceDID: instanceDID}
}

// EnqueueInTx enqueues a classification job inside the indexing
// transaction, due now on the database clock. A create or an update that
// changed a classified field always enqueues; any other update enqueues only
// while the subject has a non-terminal job under the same evaluation, so a
// job the new CID supersedes is replaced rather than lost. An existing job
// with the same identity is left as it is.
func (r *ClassificationRepository) EnqueueInTx(ctx context.Context, tx *sql.Tx, job classification.NewJob, classifiedFieldsChanged bool) error {
	return enqueueClassificationJobTx(ctx, tx, job, classifiedFieldsChanged)
}

// enqueueClassificationJobTx is EnqueueInTx for writers that hold no
// ClassificationRepository, such as the community repository.
func enqueueClassificationJobTx(ctx context.Context, tx *sql.Tx, job classification.NewJob, classifiedFieldsChanged bool) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO classification_jobs
		    (subject_uri, subject_kind, record_cid, policy_version, text_model_id, image_model_id,
		     status, next_attempt_at, author_did, community_did)
		SELECT $1, $2, $3, $4, $5, $6, '`+classification.StatusQueued+`', NOW(), $7, $8
		WHERE $9::boolean OR EXISTS (
			SELECT 1 FROM classification_jobs
			WHERE subject_uri = $1
				AND policy_version = $4 AND text_model_id = $5 AND image_model_id = $6
				AND status IN `+classificationNonTerminalSQL+`
		)
		ON CONFLICT (subject_uri, record_cid, policy_version, text_model_id, image_model_id) DO NOTHING
	`, job.SubjectURI, string(job.SubjectKind), job.RecordCID,
		job.Evaluation.PolicyVersion, job.Evaluation.TextModelID, job.Evaluation.ImageModelID,
		job.AuthorDID, job.CommunityDID, classifiedFieldsChanged)
	if err != nil {
		return fmt.Errorf("enqueueing classification job: %w", err)
	}
	return nil
}

// ErasureGateTx takes authorDID's shared erasure lock in tx, then checks the
// erasure marker in its own statement, so at READ COMMITTED it sees an
// erasure committed while the lock was awaited. The isolation level is
// checked before the lock, so a wrong-isolation transaction fails without
// waiting on an erasure in progress.
func (r *ClassificationRepository) ErasureGateTx(ctx context.Context, tx *sql.Tx, authorDID string) (bool, error) {
	var isolation string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&isolation); err != nil {
		return false, fmt.Errorf("check classification erasure gate isolation: %w", err)
	}
	if isolation != "read committed" {
		return false, fmt.Errorf("classification erasure gate requires read committed isolation, got %s", isolation)
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock_shared("+ErasureLockKeySQL+")", authorDID); err != nil {
		return false, fmt.Errorf("lock classification author against erasure: %w", err)
	}
	var erased bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM deleted_accounts WHERE did = $1)`, authorDID).Scan(&erased); err != nil {
		return false, fmt.Errorf("check classification author erasure marker: %w", err)
	}
	return erased, nil
}

// ClaimDue claims up to limit due non-terminal jobs under evaluation,
// skipping rows another claim has locked, and leases them until now+lease.
// The jobs come back in due order.
func (r *ClassificationRepository) ClaimDue(ctx context.Context, evaluation classification.Evaluation, now time.Time, limit int, lease time.Duration) ([]classification.ClaimedJob, error) {
	rows, err := r.db.QueryContext(ctx, `
		WITH due AS (
			SELECT id, next_attempt_at FROM classification_jobs
			WHERE status IN `+classificationNonTerminalSQL+` AND next_attempt_at <= $1
				AND policy_version = $4 AND text_model_id = $5 AND image_model_id = $6
			ORDER BY next_attempt_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE classification_jobs j
		SET next_attempt_at = $3, updated_at = NOW()
		FROM due
		WHERE j.id = due.id
		RETURNING j.id, j.subject_uri, j.subject_kind, j.record_cid, j.author_did, j.community_did,
		          j.policy_version, j.text_model_id, j.image_model_id, j.status, j.attempt_count,
		          j.deferral_count, due.next_attempt_at
	`, now, limit, now.Add(lease), evaluation.PolicyVersion, evaluation.TextModelID, evaluation.ImageModelID)
	if err != nil {
		return nil, fmt.Errorf("claiming classification jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var jobs []classification.ClaimedJob
	for rows.Next() {
		var (
			job         classification.ClaimedJob
			subjectKind string
		)
		if err := rows.Scan(&job.ID, &job.SubjectURI, &subjectKind, &job.RecordCID, &job.AuthorDID, &job.CommunityDID,
			&job.Evaluation.PolicyVersion, &job.Evaluation.TextModelID, &job.Evaluation.ImageModelID,
			&job.Status, &job.AttemptCount, &job.DeferralCount, &job.NextAttemptAt); err != nil {
			return nil, fmt.Errorf("scanning claimed classification job: %w", err)
		}
		job.SubjectKind = classification.SubjectKind(subjectKind)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading claimed classification jobs: %w", err)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].NextAttemptAt.Equal(jobs[j].NextAttemptAt) {
			return jobs[i].NextAttemptAt.Before(jobs[j].NextAttemptAt)
		}
		return jobs[i].ID < jobs[j].ID
	})
	return jobs, nil
}

// classificationPostSubjectQuery reads a post and its visibility to an
// anonymous viewer under the instance authority ($2). The hard hides are the
// read path's own non-admission predicate (postNotHiddenSQL) plus deletion
// and an erasure marker on the author, which a job enqueued after the
// author's erasure would otherwise slip past; a post that passes them and the read path's admission rule
// (admittedPostsPredicate) is visible. Otherwise a rejected or removed
// admission refuses it, and anything else (pending, pending re-acceptance, an
// acceptance pinned to another CID, no admission row for a postv2) awaits
// admission.
var classificationPostSubjectQuery = fmt.Sprintf(`
		SELECT p.cid, p.title, p.content, COALESCE(p.embed::text, ''), p.author_did, p.community_did, %s
		FROM posts p%s
		WHERE p.uri = $1`, classificationPostVisibilitySQL, classificationPostAdmissionJoin)

// classificationPostAdmissionJoin joins a post p to its admission row a, which
// classificationPostVisibilitySQL reads.
var classificationPostAdmissionJoin, _ = admittedPostsPredicate(anonymousViewerSQL)

// classificationPostVisibilitySQL is the SubjectVisibility of post p (joined by
// classificationPostAdmissionJoin) under the instance authority $2.
var classificationPostVisibilitySQL = func() string {
	_, admittedSQL := admittedPostsPredicate(anonymousViewerSQL)
	notHiddenSQL := postNotHiddenSQL(anonymousViewerSQL, "$2::text",
		`NOT `+communityRemovedSQL("p.community_did", "$2::text"))
	return fmt.Sprintf(`
			CASE
				WHEN p.deleted_at IS NOT NULL
					OR EXISTS (SELECT 1 FROM deleted_accounts erased WHERE erased.did = p.author_did)
					OR NOT (%s) THEN '%s'
				WHEN %s THEN '%s'
				WHEN a.status IN ('%s', '%s') THEN '%s'
				WHEN a.status IS NULL OR a.status IN ('%s', '%s', '%s') THEN '%s'
				ELSE '%s'
			END`,
		notHiddenSQL, classification.VisibilityHidden,
		admittedSQL, classification.VisibilityVisible,
		posts.AdmissionStatusRejected, posts.AdmissionStatusRemoved, classification.VisibilityAdmissionRefused,
		posts.AdmissionStatusPending, posts.AdmissionStatusPendingReacceptance, posts.AdmissionStatusAccepted,
		classification.VisibilityAwaitingAdmission,
		classification.VisibilityHidden)
}()

// classificationCommentSubjectQuery reads a comment, the community of the post
// its parent chain reaches, and its visibility to an anonymous viewer under the
// instance authority ($2). It is hidden when deleted, by an erased or
// restricted commenter, recorded hidden by an account or community ban (which
// persists after the ban ends), under an active removal of the comment or any
// ancestor comment, when its declared root post or the post its chain reaches
// is hidden as a post subject, when either of those posts was deleted with its
// community, or when the chain reaches a post in another community than the
// declared root (commentServedUnderDeclaredRootPredicate does not serve it to
// an anonymous viewer); refused when either post's admission was refused. It
// awaits admission while either post awaits it, is not indexed (the chain does
// not resolve), or its ban check is pending.
var classificationCommentSubjectQuery = func() string {
	postVisibility := func(uriExpr string) string {
		return fmt.Sprintf(`(SELECT %s FROM posts p%s WHERE p.uri = %s)`,
			classificationPostVisibilitySQL, classificationPostAdmissionJoin, uriExpr)
	}
	return fmt.Sprintf(`
		SELECT c.cid, c.content, c.commenter_did, COALESCE(c.embed::text, ''), subject_posts.chain_community,
			CASE
				WHEN c.deleted_at IS NOT NULL
					OR EXISTS (SELECT 1 FROM deleted_accounts erased WHERE erased.did = c.commenter_did)
					OR c.hide_reason IN ('%[2]s', '%[3]s')
					OR %[4]s
					OR EXISTS (%[5]s
						SELECT 1 FROM chain
						JOIN moderation_decisions d ON d.subject_uri = chain.uri
						WHERE d.kind = 'removal' AND d.active)
					OR EXISTS (SELECT 1 FROM deleted_community_post_roots deleted_root WHERE deleted_root.root_uri = c.root_uri)
					OR EXISTS (%[5]s
						SELECT 1 FROM chain
						JOIN deleted_community_post_roots deleted_root ON deleted_root.root_uri = chain.parent_uri
						WHERE NOT EXISTS (SELECT 1 FROM comments chain_parent WHERE chain_parent.uri = chain.parent_uri))
					OR '%[6]s' IN (subject_posts.root_visibility, subject_posts.chain_visibility) THEN '%[6]s'
				WHEN '%[7]s' IN (subject_posts.root_visibility, subject_posts.chain_visibility) THEN '%[7]s'
				WHEN subject_posts.root_visibility = '%[8]s' AND subject_posts.chain_visibility = '%[8]s'
					AND c.hide_reason IS NULL THEN
					CASE WHEN subject_posts.chain_community IS NOT DISTINCT FROM subject_posts.root_community
						THEN '%[8]s' ELSE '%[6]s' END
				ELSE '%[9]s'
			END
		FROM comments c
		CROSS JOIN LATERAL (
			SELECT %[10]s AS root_visibility, %[11]s AS chain_visibility, %[1]s AS chain_community,
				(SELECT root_post.community_did FROM posts root_post WHERE root_post.uri = c.root_uri) AS root_community
		) subject_posts
		WHERE c.uri = $1`,
		commentChainCommunitySQL("c"),
		commentHideReasonAccountBan, commentHideReasonCommunityBan,
		accountRestrictedSQL("c.commenter_did", "$2::text"),
		commentChainSQL("c"),
		classification.VisibilityHidden,
		classification.VisibilityAdmissionRefused,
		classification.VisibilityVisible,
		classification.VisibilityAwaitingAdmission,
		postVisibility("c.root_uri"),
		postVisibility(commentChainPostSQL("c", "uri")))
}()

// classificationCommunitySubjectQuery reads a community's classified profile
// fields and its visibility: hidden under an active instance removal by the
// authority $2, the one rule by which the read path hides a community.
var classificationCommunitySubjectQuery = fmt.Sprintf(`
		SELECT cm.record_cid, cm.name, cm.display_name, cm.description,
			COALESCE(cm.avatar_cid, ''), COALESCE(cm.banner_cid, ''),
			CASE WHEN %s THEN '%s' ELSE '%s' END
		FROM communities cm
		WHERE cm.did = $1`,
	communityRemovedSQL("cm.did", "$2::text"), classification.VisibilityHidden, classification.VisibilityVisible)

// LoadPostSubject reads the indexed post at subjectURI, its classified fields,
// its images and its visibility. The images are owned by the repository the
// record lives in: the author's for a postv2 record, the community's for a
// legacy community-repo record.
func (r *ClassificationRepository) LoadPostSubject(ctx context.Context, subjectURI string) (classification.PostSubject, error) {
	var (
		subject                 classification.PostSubject
		title, body             sql.NullString
		embedJSON               string
		authorDID, communityDID string
		visibility              string
	)
	err := r.db.QueryRowContext(ctx, classificationPostSubjectQuery, subjectURI, r.instanceDID).
		Scan(&subject.CID, &title, &body, &embedJSON, &authorDID, &communityDID, &visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.PostSubject{Visibility: classification.VisibilityMissing}, nil
	}
	if err != nil {
		return classification.PostSubject{}, fmt.Errorf("loading classification post subject: %w", err)
	}
	subject.Visibility = classification.SubjectVisibility(visibility)
	subject.Fields = classification.PostFieldsFromRecord(nullStringPointer(title), nullStringPointer(body), embedJSON)
	imageOwnerDID := communityDID
	if posts.CollectionOfPostURI(subjectURI) == posts.PostV2Collection {
		imageOwnerDID = authorDID
	}
	subject.ImageRefs, subject.ImagesSkipped = classification.PostImages(imageOwnerDID, embedJSON)
	return subject, nil
}

// LoadCommentSubject reads the indexed comment at subjectURI, its body, its
// images (owned by the commenter), the community of the post its parent chain
// reaches and its visibility.
func (r *ClassificationRepository) LoadCommentSubject(ctx context.Context, subjectURI string) (classification.CommentSubject, error) {
	var (
		subject                 classification.CommentSubject
		commenterDID, embedJSON string
		communityDID            sql.NullString
		visibility              string
	)
	err := r.db.QueryRowContext(ctx, classificationCommentSubjectQuery, subjectURI, r.instanceDID).
		Scan(&subject.CID, &subject.Body, &commenterDID, &embedJSON, &communityDID, &visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.CommentSubject{Visibility: classification.VisibilityMissing}, nil
	}
	if err != nil {
		return classification.CommentSubject{}, fmt.Errorf("loading classification comment subject: %w", err)
	}
	subject.Visibility = classification.SubjectVisibility(visibility)
	subject.CommunityDID = communityDID.String
	subject.ImageRefs, subject.ImagesSkipped = classification.CommentImages(commenterDID, embedJSON)
	return subject, nil
}

// LoadCommunitySubject reads the indexed community communityDID, its
// classified profile fields, its avatar and banner and its visibility.
func (r *ClassificationRepository) LoadCommunitySubject(ctx context.Context, communityDID string) (classification.CommunitySubject, error) {
	var (
		subject                       classification.CommunitySubject
		cid, displayName, description sql.NullString
		avatarCID, bannerCID          string
		visibility                    string
	)
	err := r.db.QueryRowContext(ctx, classificationCommunitySubjectQuery, communityDID, r.instanceDID).
		Scan(&cid, &subject.Fields.Name, &displayName, &description, &avatarCID, &bannerCID, &visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.CommunitySubject{Visibility: classification.VisibilityMissing}, nil
	}
	if err != nil {
		return classification.CommunitySubject{}, fmt.Errorf("loading classification community subject: %w", err)
	}
	subject.Visibility = classification.SubjectVisibility(visibility)
	subject.CID = cid.String
	subject.Fields.DisplayName = displayName.String
	subject.Fields.Description = description.String
	subject.ImageRefs = classification.CommunityImages(communityDID, avatarCID, bannerCID)
	return subject, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// Close ends a non-terminal job closed with reason and discards its stored
// image batches, in one statement.
func (r *ClassificationRepository) Close(ctx context.Context, jobID int64, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		WITH closed AS (
			UPDATE classification_jobs
			SET status = '`+classification.StatusClosed+`', close_reason = $2, updated_at = NOW()
			WHERE id = $1 AND status IN `+classificationNonTerminalSQL+`
			RETURNING id
		)
		DELETE FROM classification_job_batches WHERE job_id IN (SELECT id FROM closed)`,
		jobID, reason)
	if err != nil {
		return fmt.Errorf("closing classification job: %w", err)
	}
	return nil
}

// Defer makes a non-terminal job deferred, due at until, and counts the
// deferral.
func (r *ClassificationRepository) Defer(ctx context.Context, jobID int64, until time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE classification_jobs
		SET status = '`+classification.StatusDeferred+`', next_attempt_at = $2,
		    deferral_count = deferral_count + 1, updated_at = NOW()
		WHERE id = $1 AND status IN `+classificationNonTerminalSQL,
		jobID, until)
	if err != nil {
		return fmt.Errorf("deferring classification job: %w", err)
	}
	return nil
}

// ReleaseClaim restores a claimed job's status and pre-claim next_attempt_at.
func (r *ClassificationRepository) ReleaseClaim(ctx context.Context, job classification.ClaimedJob) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE classification_jobs
		SET status = $2, next_attempt_at = $3, updated_at = NOW()
		WHERE id = $1 AND status IN `+classificationNonTerminalSQL,
		job.ID, job.Status, job.NextAttemptAt)
	if err != nil {
		return fmt.Errorf("releasing classification job: %w", err)
	}
	return nil
}

// RecordFailure counts a failed request on a non-terminal job and makes it
// retrying at failure.RetryAt, or failed when failure.Final. A final failure
// discards the job's stored image batches in the same statement, so a job
// that ends without a result keeps no partial answers, and reports how many
// it discarded.
func (r *ClassificationRepository) RecordFailure(ctx context.Context, jobID int64, failure classification.Failure) (int, error) {
	var discarded int
	err := r.db.QueryRowContext(ctx, `
		WITH recorded AS (
			UPDATE classification_jobs
			SET attempt_count = attempt_count + 1,
			    last_error_code = $2,
			    status = CASE WHEN $3::boolean THEN '`+classification.StatusFailed+`' ELSE '`+classification.StatusRetrying+`' END,
			    next_attempt_at = CASE WHEN $3::boolean THEN next_attempt_at ELSE $4 END,
			    updated_at = NOW()
			WHERE id = $1 AND status IN `+classificationNonTerminalSQL+`
			RETURNING id
		), discarded AS (
			DELETE FROM classification_job_batches
			WHERE $3::boolean AND job_id IN (SELECT id FROM recorded)
			RETURNING 1
		)
		SELECT COUNT(*) FROM discarded`,
		jobID, failure.ErrorCode, failure.Final, failure.RetryAt).Scan(&discarded)
	if err != nil {
		return 0, fmt.Errorf("recording classification failure: %w", err)
	}
	return discarded, nil
}

// classificationUsageDate is the UTC date of now, as the usage row key.
func classificationUsageDate(now time.Time) string {
	return now.UTC().Format(time.DateOnly)
}

// classificationUsageLockKeySQL is the transaction-scoped advisory lock
// serializing request reservations for one UTC usage date, namespaced away
// from other advisory lock users.
const classificationUsageLockKeySQL = "hashtext('classification-usage:' || $1)"

// ReserveRequest counts one request for modelID on now's UTC date unless the
// day already has dailyCap requests across all models. Each model keeps its
// own usage row; the cap is on their sum. Reservations for a date hold that
// date's advisory lock while they sum and increment, so concurrent
// reservations cannot overshoot the cap.
func (r *ClassificationRepository) ReserveRequest(ctx context.Context, modelID string, now time.Time, dailyCap int) (reserved bool, err error) {
	if dailyCap <= 0 {
		return false, nil
	}
	usageDate := classificationUsageDate(now)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning the classification reservation: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+classificationUsageLockKeySQL+`)`, usageDate); err != nil {
		return false, fmt.Errorf("locking the classification usage date: %w", err)
	}
	// The statement's snapshot is taken after the lock is held, so the sum
	// includes every reservation committed before it.
	var requests int
	err = tx.QueryRowContext(ctx, `
		INSERT INTO classification_usage (model_id, usage_date, requests, input_tokens)
		SELECT $1, $2::date, 1, 0
		WHERE (SELECT COALESCE(SUM(requests), 0) FROM classification_usage WHERE usage_date = $2::date) < $3
		ON CONFLICT (model_id, usage_date) DO UPDATE
		SET requests = classification_usage.requests + 1
		RETURNING requests
	`, modelID, usageDate, dailyCap).Scan(&requests)
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.Commit(); err != nil {
			return false, fmt.Errorf("committing the classification reservation: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reserving classification request: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("committing the classification reservation: %w", err)
	}
	return true, nil
}

// StoreResult marks the job classified, inserts its result with one score
// alert per result.AlertQuestions key, and discards its stored image batches
// in one statement, so all happen or none does. A job that no longer exists
// (erased with its author) or is already terminal stores nothing and reports
// false. The claimed job's CommunityDID, which the worker sets to the
// community a comment's chain resolves to, is written to the job and the
// result. An erasure that deletes the job after this commits removes the
// result through the job_id foreign key's cascade. A revived job's partial
// result is replaced in place: same ID, so its verdicts stay, partial false,
// and an alert already raised for a question is not raised again. The store
// holds the author's shared erasure lock to commit: an erasure deletes the
// partial result before the job, so without the lock the store could hold
// the job row while waiting on that result row, a deadlock.
func (r *ClassificationRepository) StoreResult(ctx context.Context, job classification.ClaimedJob, result classification.Result) (bool, error) {
	return r.storeResult(ctx, job, result, nil)
}

// StorePartialResult records the job's final failure, as RecordFailure does
// (attempt counted, last_error_code set, status failed), and stores result
// marked partial, under StoreResult's rules and in one statement with it.
func (r *ClassificationRepository) StorePartialResult(ctx context.Context, job classification.ClaimedJob, result classification.Result, failure classification.Failure) (bool, error) {
	result.Partial = true
	return r.storeResult(ctx, job, result, &failure)
}

// storeResult is StoreResult, or with failure set StorePartialResult.
func (r *ClassificationRepository) storeResult(ctx context.Context, job classification.ClaimedJob, result classification.Result, failure *classification.Failure) (bool, error) {
	probabilities, err := json.Marshal(result.Probabilities)
	if err != nil {
		return false, fmt.Errorf("encoding classification probabilities: %w", err)
	}
	alertQuestions := result.AlertQuestions
	if alertQuestions == nil {
		alertQuestions = []string{}
	}
	status := classification.StatusClassified
	var errorCode sql.NullString
	if failure != nil {
		status = classification.StatusFailed
		errorCode = sql.NullString{String: failure.ErrorCode, Valid: true}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning the classification result store: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock_shared("+ErasureLockKeySQL+")", job.AuthorDID); err != nil {
		return false, fmt.Errorf("taking the shared erasure lock for classification job %d: %w", job.ID, err)
	}
	var stored int
	err = tx.QueryRowContext(ctx, `
		WITH ended AS (
			UPDATE classification_jobs
			SET status = $9, community_did = $7,
			    attempt_count = attempt_count + CASE WHEN $10::text IS NULL THEN 0 ELSE 1 END,
			    last_error_code = COALESCE($10::text, last_error_code),
			    updated_at = NOW()
			WHERE id = $1 AND status IN `+classificationNonTerminalSQL+`
			RETURNING id, subject_uri, record_cid, subject_kind, community_did, author_did, policy_version
		), discarded AS (
			DELETE FROM classification_job_batches WHERE job_id IN (SELECT id FROM ended)
		), inserted AS (
			INSERT INTO classification_results
			    (job_id, subject_uri, subject_cid, subject_kind, community_did, author_did, model_id,
			     policy_version, probabilities, images_sent, images_skipped, text_truncated, partial)
			SELECT id, subject_uri, record_cid, subject_kind, community_did, author_did, $2,
			       policy_version, $3::jsonb, $4, $5, $6, $11
			FROM ended
			ON CONFLICT (job_id) DO UPDATE
			SET community_did = EXCLUDED.community_did, model_id = EXCLUDED.model_id,
			    probabilities = EXCLUDED.probabilities, images_sent = EXCLUDED.images_sent,
			    images_skipped = EXCLUDED.images_skipped, text_truncated = EXCLUDED.text_truncated,
			    partial = EXCLUDED.partial, created_at = NOW()
			RETURNING id, subject_kind
		), alerts AS (
			INSERT INTO classification_score_alerts (result_id, question_key, subject_kind)
			SELECT inserted.id, question.key, inserted.subject_kind
			FROM inserted CROSS JOIN unnest($8::text[]) AS question(key)
			ON CONFLICT (result_id, question_key) DO NOTHING
		)
		SELECT count(*) FROM inserted
	`, job.ID, result.ModelID, string(probabilities), result.ImagesSent, result.ImagesSkipped, result.TextTruncated,
		job.CommunityDID, pq.Array(alertQuestions), status, errorCode, result.Partial).Scan(&stored)
	if err != nil {
		return false, fmt.Errorf("storing classification result: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("committing the classification result store: %w", err)
	}
	return stored == 1, nil
}

// WithCommunityClassification makes the community repository enqueue a
// classification job under evaluation in each community write transaction.
func WithCommunityClassification(evaluation classification.Evaluation) AccountRestrictionOption {
	return func(r *accountRestriction) { r.communityClassification = &evaluation }
}

// enqueueCommunityClassificationTx enqueues a classification job for the
// community did at recordCID in tx. The community is its own subject, author
// and community. Without WithCommunityClassification it does nothing.
func (r accountRestriction) enqueueCommunityClassificationTx(ctx context.Context, tx *sql.Tx, did, recordCID string, classifiedFieldsChanged bool) error {
	if r.communityClassification == nil {
		return nil
	}
	err := enqueueClassificationJobTx(ctx, tx, classification.NewJob{
		SubjectURI:   did,
		SubjectKind:  classification.SubjectKindCommunity,
		RecordCID:    recordCID,
		AuthorDID:    did,
		CommunityDID: did,
		Evaluation:   *r.communityClassification,
	}, classifiedFieldsChanged)
	if err != nil {
		return fmt.Errorf("enqueue classification for community %s: %w", did, err)
	}
	return nil
}

// LoadBatchProgress reads the job's stored image batches, ordered by index.
// InputTokens is not stored with a batch: its tokens are in the usage row.
func (r *ClassificationRepository) LoadBatchProgress(ctx context.Context, jobID int64) ([]classification.BatchProgress, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT batch_index, model_id, probabilities, images_sent, images_skipped
		FROM classification_job_batches
		WHERE job_id = $1
		ORDER BY batch_index
	`, jobID)
	if err != nil {
		return nil, fmt.Errorf("loading classification batch progress: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var progress []classification.BatchProgress
	for rows.Next() {
		var (
			batch             classification.BatchProgress
			probabilitiesJSON []byte
		)
		if err := rows.Scan(&batch.Index, &batch.ModelID, &probabilitiesJSON, &batch.ImagesSent, &batch.ImagesSkipped); err != nil {
			return nil, fmt.Errorf("scanning classification batch progress: %w", err)
		}
		if err := json.Unmarshal(probabilitiesJSON, &batch.Probabilities); err != nil {
			return nil, fmt.Errorf("decoding classification batch probabilities: %w", err)
		}
		progress = append(progress, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading classification batch progress: %w", err)
	}
	return progress, nil
}

// StoreBatchProgress stores one finished batch of a non-terminal job and adds
// its input tokens to its model's usage on now's UTC date, in one statement.
// A batch without input tokens, such as a skipped-only batch that sent no
// request and has no model, adds no usage row.
// The tokens are added even when the job no longer exists or is terminal
// (erased, closed or superseded while its request was in flight), because
// the request was sent; the batch is then not stored and it reports false.
func (r *ClassificationRepository) StoreBatchProgress(ctx context.Context, jobID int64, progress classification.BatchProgress, now time.Time) (bool, error) {
	probabilities, err := json.Marshal(progress.Probabilities)
	if err != nil {
		return false, fmt.Errorf("encoding classification batch probabilities: %w", err)
	}
	// A data-modifying CTE runs whether or not the outer query reads it, so
	// the usage upsert does not depend on the batch insert.
	var stored int
	err = r.db.QueryRowContext(ctx, `
		WITH pending AS (
			SELECT id FROM classification_jobs
			WHERE id = $1 AND status IN `+classificationNonTerminalSQL+`
			FOR UPDATE
		), batch AS (
			INSERT INTO classification_job_batches
			    (job_id, batch_index, model_id, probabilities, images_sent, images_skipped)
			SELECT id, $2, $3, $4::jsonb, $5, $6 FROM pending
			RETURNING job_id
		), tokens AS (
			INSERT INTO classification_usage (model_id, usage_date, requests, input_tokens)
			SELECT $3, $7::date, 0, $8::bigint WHERE $8::bigint > 0
			ON CONFLICT (model_id, usage_date) DO UPDATE
			SET input_tokens = classification_usage.input_tokens + EXCLUDED.input_tokens
		)
		SELECT count(*) FROM batch
	`, jobID, progress.Index, progress.ModelID, string(probabilities), progress.ImagesSent, progress.ImagesSkipped,
		classificationUsageDate(now), progress.InputTokens).Scan(&stored)
	if err != nil {
		return false, fmt.Errorf("storing classification batch progress: %w", err)
	}
	return stored == 1, nil
}
