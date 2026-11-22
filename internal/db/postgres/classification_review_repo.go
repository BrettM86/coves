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
	"Coves/internal/core/moderation"

	"github.com/lib/pq"
)

// ClassificationReviewRepository stores classification results and admin
// verdicts for review in Postgres.
type ClassificationReviewRepository struct {
	db *sql.DB
	// subjects loads a result's subject as the classification worker does.
	subjects *ClassificationRepository
}

var _ classification.ReviewRepository = (*ClassificationReviewRepository)(nil)

// NewClassificationReviewRepository returns the classification review
// repository. instanceDID is the authority whose removals and account bans
// hide subjects.
func NewClassificationReviewRepository(db *sql.DB, instanceDID string) *ClassificationReviewRepository {
	return &ClassificationReviewRepository{db: db, subjects: NewClassificationRepository(db, instanceDID)}
}

// classificationSubjectDeletedSQL is whether the subject of result r is
// deleted: a post or comment with no undeleted row, or a community with no
// row.
var classificationSubjectDeletedSQL = fmt.Sprintf(`
	CASE r.subject_kind
		WHEN '%s' THEN NOT EXISTS (SELECT 1 FROM posts p WHERE p.uri = r.subject_uri AND p.deleted_at IS NULL)
		WHEN '%s' THEN NOT EXISTS (SELECT 1 FROM comments c WHERE c.uri = r.subject_uri AND c.deleted_at IS NULL)
		ELSE NOT EXISTS (SELECT 1 FROM communities cm WHERE cm.did = r.subject_uri)
	END`, classification.SubjectKindPost, classification.SubjectKindComment)

// classificationScoredCIDCurrentSQL is whether the subject of result r is
// undeleted and still at the CID the result scored.
var classificationScoredCIDCurrentSQL = fmt.Sprintf(`
	CASE r.subject_kind
		WHEN '%s' THEN EXISTS (SELECT 1 FROM posts p
			WHERE p.uri = r.subject_uri AND p.deleted_at IS NULL AND p.cid = r.subject_cid)
		WHEN '%s' THEN EXISTS (SELECT 1 FROM comments c
			WHERE c.uri = r.subject_uri AND c.deleted_at IS NULL AND c.cid = r.subject_cid)
		ELSE EXISTS (SELECT 1 FROM communities cm WHERE cm.did = r.subject_uri AND cm.record_cid = r.subject_cid)
	END`, classification.SubjectKindPost, classification.SubjectKindComment)

// classificationSubjectWithheldSQL is whether the content of result r's
// subject is withheld from admins, by the admin moderation log's rule: any
// action on the subject, or on a comment's declared root post or the post its
// parent chain reaches, has a hidden reason (the text array $12).
var classificationSubjectWithheldSQL = hiddenReasonActionExistsSQL("r.subject_uri", "$12") + fmt.Sprintf(`
	OR (r.subject_kind = '%s' AND EXISTS (SELECT 1 FROM comments c WHERE c.uri = r.subject_uri
		AND (%s OR %s)))`, classification.SubjectKindComment,
	hiddenReasonActionExistsSQL("c.root_uri", "$12"), hiddenReasonActionExistsSQL(commentChainPostSQL("c", "uri"), "$12"))

// classificationResultListQuery lists results matching every given filter,
// newest first by (created_at, id). An empty text filter and a NULL bound
// match every row.
var classificationResultListQuery = `
	SELECT r.id, r.subject_kind, r.subject_uri, r.subject_cid, r.community_did, r.author_did,
	       r.model_id, r.policy_version, r.probabilities, r.images_sent, r.images_skipped,
	       r.text_truncated, r.partial, r.created_at, ` + classificationSubjectDeletedSQL + `, ` + classificationSubjectWithheldSQL + `
	FROM classification_results r
	WHERE ($1 = '' OR r.probabilities ->> $1 IS NOT NULL)
		AND ($2::double precision IS NULL OR (r.probabilities ->> $1)::double precision >= $2)
		AND ($3 = '' OR r.subject_kind = $3)
		AND ($4 = '' OR r.community_did = $4)
		AND ($5 = '' OR r.model_id = $5)
		AND ($6 = '' OR r.policy_version = $6)
		AND ($7::timestamptz IS NULL OR r.created_at >= $7)
		AND ($8::timestamptz IS NULL OR r.created_at < $8)
		AND ($9::timestamptz IS NULL OR (r.created_at, r.id) < ($9, $10))
	ORDER BY r.created_at DESC, r.id DESC
	LIMIT $11`

// ListResults lists the results matching query, newest first, each with its
// subject as the classification worker would load it now. A deleted subject's
// content and images are left out, and so are those of a subject under a
// hidden-reason action (classificationSubjectWithheldSQL), as the admin
// moderation log leaves them out.
func (r *ClassificationReviewRepository) ListResults(ctx context.Context, query classification.ResultQuery) ([]classification.ResultView, error) {
	filter := query.Filter
	var (
		afterCreatedAt sql.NullTime
		afterID        int64
	)
	if query.After != nil {
		afterCreatedAt = sql.NullTime{Time: query.After.CreatedAt, Valid: true}
		afterID = query.After.ID
	}
	var minimumProbability sql.NullFloat64
	if filter.MinProbability != nil {
		minimumProbability = sql.NullFloat64{Float64: *filter.MinProbability, Valid: true}
	}
	rows, err := r.db.QueryContext(ctx, classificationResultListQuery,
		filter.Question, minimumProbability, string(filter.SubjectKind), filter.CommunityDID,
		filter.ModelID, filter.PolicyVersion, optionalTime(filter.Since), optionalTime(filter.Until),
		afterCreatedAt, afterID, query.Limit, pq.Array(moderation.HiddenActionReasons()))
	if err != nil {
		return nil, fmt.Errorf("listing classification results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		views    []classification.ResultView
		deleted  []bool
		withheld []bool
	)
	for rows.Next() {
		var (
			view              classification.ResultView
			subjectKind       string
			probabilitiesJSON []byte
			subjectDeleted    bool
			subjectWithheld   bool
		)
		if err := rows.Scan(&view.ID, &subjectKind, &view.SubjectURI, &view.ScoredCID, &view.CommunityDID, &view.AuthorDID,
			&view.ModelID, &view.PolicyVersion, &probabilitiesJSON, &view.ImagesSent, &view.ImagesSkipped,
			&view.TextTruncated, &view.Partial, &view.CreatedAt, &subjectDeleted, &subjectWithheld); err != nil {
			return nil, fmt.Errorf("scanning classification result: %w", err)
		}
		view.SubjectKind = classification.SubjectKind(subjectKind)
		view.CreatedAt = view.CreatedAt.UTC()
		if err := json.Unmarshal(probabilitiesJSON, &view.Probabilities); err != nil {
			return nil, fmt.Errorf("decoding probabilities of classification result %d: %w", view.ID, err)
		}
		views = append(views, view)
		deleted = append(deleted, subjectDeleted)
		withheld = append(withheld, subjectWithheld)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading classification results: %w", err)
	}
	_ = rows.Close()

	for index := range views {
		view := &views[index]
		subject, err := r.loadSubjectView(ctx, view.SubjectKind, view.SubjectURI)
		if err != nil {
			return nil, fmt.Errorf("loading the subject of classification result %d: %w", view.ID, err)
		}
		if deleted[index] || subject.Visibility == classification.VisibilityMissing {
			subject.Deleted = true
			subject.CurrentCID = ""
		}
		if subject.Deleted || withheld[index] {
			subject.Post, subject.Comment, subject.Community, subject.Images = nil, nil, nil, nil
		}
		view.Subject = subject
		view.ScoredCIDIsCurrent = !subject.Deleted && subject.CurrentCID == view.ScoredCID
	}
	return views, nil
}

func optionalTime(value time.Time) sql.NullTime {
	return sql.NullTime{Time: value, Valid: !value.IsZero()}
}

// loadSubjectView loads a subject through the classification worker's
// subject loaders.
func (r *ClassificationReviewRepository) loadSubjectView(ctx context.Context, kind classification.SubjectKind, subjectURI string) (classification.SubjectView, error) {
	switch kind {
	case classification.SubjectKindPost:
		subject, err := r.subjects.LoadPostSubject(ctx, subjectURI)
		if err != nil {
			return classification.SubjectView{}, err
		}
		view := classification.SubjectView{Visibility: subject.Visibility, CurrentCID: subject.CID, Images: subject.ImageRefs}
		if subject.Visibility != classification.VisibilityMissing {
			fields := subject.Fields
			view.Post = &fields
		}
		return view, nil
	case classification.SubjectKindComment:
		subject, err := r.subjects.LoadCommentSubject(ctx, subjectURI)
		if err != nil {
			return classification.SubjectView{}, err
		}
		view := classification.SubjectView{Visibility: subject.Visibility, CurrentCID: subject.CID, Images: subject.ImageRefs}
		if subject.Visibility != classification.VisibilityMissing {
			view.Comment = &classification.CommentFields{Body: subject.Body}
		}
		return view, nil
	case classification.SubjectKindCommunity:
		subject, err := r.subjects.LoadCommunitySubject(ctx, subjectURI)
		if err != nil {
			return classification.SubjectView{}, err
		}
		view := classification.SubjectView{Visibility: subject.Visibility, CurrentCID: subject.CID, Images: subject.ImageRefs}
		if subject.Visibility != classification.VisibilityMissing {
			fields := subject.Fields
			view.Community = &fields
		}
		return view, nil
	default:
		return classification.SubjectView{}, fmt.Errorf("unknown classification subject kind %q", kind)
	}
}

// ResultQuestions returns the question keys a result scored, sorted.
func (r *ClassificationReviewRepository) ResultQuestions(ctx context.Context, resultID int64) ([]string, error) {
	var probabilitiesJSON []byte
	err := r.db.QueryRowContext(ctx, `SELECT probabilities FROM classification_results WHERE id = $1`, resultID).
		Scan(&probabilitiesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, classification.ErrResultNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading classification result %d: %w", resultID, err)
	}
	var probabilities map[string]float64
	if err := json.Unmarshal(probabilitiesJSON, &probabilities); err != nil {
		return nil, fmt.Errorf("decoding probabilities of classification result %d: %w", resultID, err)
	}
	return sortedKeys(probabilities), nil
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// classificationSubjectLockQueries read and share-lock a subject's row: its
// current CID and whether it is deleted. An edit of the row in progress
// blocks the lock until it commits, and the lock then reads the edited row.
var classificationSubjectLockQueries = map[classification.SubjectKind]string{
	classification.SubjectKindPost:      `SELECT cid, deleted_at IS NOT NULL FROM posts WHERE uri = $1 FOR SHARE`,
	classification.SubjectKindComment:   `SELECT cid, deleted_at IS NOT NULL FROM comments WHERE uri = $1 FOR SHARE`,
	classification.SubjectKindCommunity: `SELECT COALESCE(record_cid, ''), FALSE FROM communities WHERE did = $1 FOR SHARE`,
}

// StoreVerdict stores an admin's verdict on a reviewable result, replacing
// that admin's earlier verdict on it. In one transaction it takes the shared
// erasure lock on the result's author, share-locks the result and its subject
// row, refuses the verdict with ErrUnreviewable when the
// subject is deleted or no longer at the scored CID, and upserts it. A
// missing result is ErrResultNotFound.
func (r *ClassificationReviewRepository) StoreVerdict(ctx context.Context, verdict classification.Verdict) (stored classification.Verdict, err error) {
	present, err := json.Marshal(verdict.Present)
	if err != nil {
		return classification.Verdict{}, fmt.Errorf("encoding classification verdict: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return classification.Verdict{}, fmt.Errorf("beginning the classification verdict: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Account erasure deletes the author's posts and comments, then their
	// results, under the exclusive erasure lock. Taking the shared lock on the
	// result's author before locking the result or its subject keeps a verdict
	// from holding the result while waiting on a subject row the erasure holds,
	// a deadlock; after an erasure the locked read below finds no result.
	var authorDID string
	err = tx.QueryRowContext(ctx, `SELECT author_did FROM classification_results WHERE id = $1`, verdict.ResultID).
		Scan(&authorDID)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.Verdict{}, classification.ErrResultNotFound
	}
	if err != nil {
		return classification.Verdict{}, fmt.Errorf("loading the author of classification result %d: %w", verdict.ResultID, err)
	}
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock_shared("+ErasureLockKeySQL+")", authorDID); err != nil {
		return classification.Verdict{}, fmt.Errorf("taking the shared erasure lock for classification result %d: %w", verdict.ResultID, err)
	}

	var subjectKind, subjectURI, scoredCID string
	err = tx.QueryRowContext(ctx, `
		SELECT subject_kind, subject_uri, subject_cid FROM classification_results WHERE id = $1 FOR SHARE`,
		verdict.ResultID).Scan(&subjectKind, &subjectURI, &scoredCID)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.Verdict{}, classification.ErrResultNotFound
	}
	if err != nil {
		return classification.Verdict{}, fmt.Errorf("loading classification result %d: %w", verdict.ResultID, err)
	}
	lockQuery, known := classificationSubjectLockQueries[classification.SubjectKind(subjectKind)]
	if !known {
		return classification.Verdict{}, fmt.Errorf("unknown classification subject kind %q", subjectKind)
	}
	var (
		currentCID string
		deleted    bool
	)
	err = tx.QueryRowContext(ctx, lockQuery, subjectURI).Scan(&currentCID, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return classification.Verdict{}, classification.ErrUnreviewable
	}
	if err != nil {
		return classification.Verdict{}, fmt.Errorf("locking the subject of classification result %d: %w", verdict.ResultID, err)
	}
	if deleted || currentCID != scoredCID {
		return classification.Verdict{}, classification.ErrUnreviewable
	}

	var (
		presentJSON []byte
		recordedAt  time.Time
	)
	err = tx.QueryRowContext(ctx, `
		INSERT INTO classification_verdicts (result_id, admin_did, present, recorded_at)
		VALUES ($1, $2, $3::jsonb, $4)
		ON CONFLICT (result_id, admin_did) DO UPDATE
		SET present = EXCLUDED.present, recorded_at = EXCLUDED.recorded_at
		RETURNING present, recorded_at
	`, verdict.ResultID, verdict.AdminDID, string(present), verdict.RecordedAt).Scan(&presentJSON, &recordedAt)
	if err != nil {
		return classification.Verdict{}, fmt.Errorf("storing classification verdict: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return classification.Verdict{}, fmt.Errorf("committing the classification verdict: %w", err)
	}

	stored = classification.Verdict{ResultID: verdict.ResultID, AdminDID: verdict.AdminDID, RecordedAt: recordedAt.UTC()}
	if err := json.Unmarshal(presentJSON, &stored.Present); err != nil {
		return classification.Verdict{}, fmt.Errorf("decoding stored classification verdict: %w", err)
	}
	return stored, nil
}

// PrecisionSamples returns an evaluation's verdicted reviewable results, each
// with its most recently recorded verdict, the questions its results scored,
// and how many of its results are unreviewable (subject edited or deleted),
// verdicted or not.
func (r *ClassificationReviewRepository) PrecisionSamples(ctx context.Context, modelID, policyVersion string) (classification.PrecisionSamples, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT r.probabilities, latest.present, `+classificationScoredCIDCurrentSQL+`
		FROM classification_results r
		LEFT JOIN LATERAL (
			SELECT v.present FROM classification_verdicts v
			WHERE v.result_id = r.id
			ORDER BY v.recorded_at DESC, v.admin_did
			LIMIT 1
		) latest ON TRUE
		WHERE r.model_id = $1 AND r.policy_version = $2
	`, modelID, policyVersion)
	if err != nil {
		return classification.PrecisionSamples{}, fmt.Errorf("loading classification precision samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		samples   classification.PrecisionSamples
		questions = make(map[string]struct{})
	)
	for rows.Next() {
		var (
			probabilitiesJSON, presentJSON []byte
			reviewable                     bool
		)
		if err := rows.Scan(&probabilitiesJSON, &presentJSON, &reviewable); err != nil {
			return classification.PrecisionSamples{}, fmt.Errorf("scanning classification precision sample: %w", err)
		}
		var sample classification.VerdictSample
		if err := json.Unmarshal(probabilitiesJSON, &sample.Probabilities); err != nil {
			return classification.PrecisionSamples{}, fmt.Errorf("decoding classification precision probabilities: %w", err)
		}
		for question := range sample.Probabilities {
			questions[question] = struct{}{}
		}
		if !reviewable {
			samples.Unreviewable++
			continue
		}
		if presentJSON == nil {
			continue
		}
		if err := json.Unmarshal(presentJSON, &sample.Present); err != nil {
			return classification.PrecisionSamples{}, fmt.Errorf("decoding classification verdict: %w", err)
		}
		samples.Samples = append(samples.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return classification.PrecisionSamples{}, fmt.Errorf("reading classification precision samples: %w", err)
	}
	samples.Questions = sortedKeys(questions)
	return samples, nil
}
