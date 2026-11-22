package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"Coves/internal/core/classification"

	"github.com/lib/pq"
)

// defaultReenqueueBatchSize is the batch size used when the constructor is
// given none.
const defaultReenqueueBatchSize = 500

// ClassificationReenqueueRepository queues classification jobs for existing
// posts, comments and communities in Postgres, in bounded batches.
type ClassificationReenqueueRepository struct {
	db        *sql.DB
	batchSize int
}

var _ classification.ReenqueueRepository = (*ClassificationReenqueueRepository)(nil)

// NewClassificationReenqueueRepository returns the re-enqueue repository.
// batchSize is the number of subjects each statement covers; zero or less
// means 500.
func NewClassificationReenqueueRepository(db *sql.DB, batchSize int) *ClassificationReenqueueRepository {
	if batchSize <= 0 {
		batchSize = defaultReenqueueBatchSize
	}
	return &ClassificationReenqueueRepository{db: db, batchSize: batchSize}
}

// reenqueueInsertSQL inserts a queued job for each row of the batch CTE
// (subject_uri, record_cid, author_did, community_did). A failed job with the
// same identity is revived: queued again, due now, with its attempts, error
// code and deferral backoff reset. A job with the same identity in any other
// status, closed included, is left as it is. The written CTE has one row per
// job created or revived, inserted telling them apart (xmax is 0 only on a
// newly inserted row). $1..$4 are the subject kind and the evaluation.
const reenqueueInsertSQL = `
	written AS (
		INSERT INTO classification_jobs
		    (subject_uri, subject_kind, record_cid, policy_version, text_model_id, image_model_id,
		     status, next_attempt_at, author_did, community_did)
		SELECT subject_uri, $1, record_cid, $2, $3, $4, '` + classification.StatusQueued + `', NOW(), author_did, community_did
		FROM batch
		ON CONFLICT (subject_uri, record_cid, policy_version, text_model_id, image_model_id) DO UPDATE
		SET status = '` + classification.StatusQueued + `', attempt_count = 0, last_error_code = NULL,
		    deferral_count = 0, next_attempt_at = NOW(), updated_at = NOW()
		WHERE classification_jobs.status = '` + classification.StatusFailed + `'
		RETURNING (xmax = 0) AS inserted
	)`

// reenqueueCountsSQL selects how many jobs a batch created and revived.
const reenqueueCountsSQL = `(SELECT COUNT(*) FILTER (WHERE inserted) FROM written),
		       (SELECT COUNT(*) FILTER (WHERE NOT inserted) FROM written)`

// EnqueueCommunities queues a job for every community at its record CID. The
// community is its own subject, author and community.
func (r *ClassificationReenqueueRepository) EnqueueCommunities(ctx context.Context, evaluation classification.Evaluation) (classification.ReenqueueReport, error) {
	query := `
		WITH batch AS (
			SELECT did AS subject_uri, record_cid, did AS author_did, did AS community_did
			FROM communities
			WHERE record_cid IS NOT NULL AND record_cid <> '' AND did > $5
			ORDER BY did
			LIMIT $6
		),` + reenqueueInsertSQL + `
		SELECT (SELECT COUNT(*) FROM batch), ` + reenqueueCountsSQL + `,
		       COALESCE((SELECT MAX(subject_uri) FROM batch), '')`

	var report classification.ReenqueueReport
	after := ""
	for {
		var scanned, created, revived int
		var last string
		err := r.db.QueryRowContext(ctx, query,
			string(classification.SubjectKindCommunity),
			evaluation.PolicyVersion, evaluation.TextModelID, evaluation.ImageModelID,
			after, r.batchSize,
		).Scan(&scanned, &created, &revived, &last)
		if err != nil {
			return classification.ReenqueueReport{}, fmt.Errorf("enqueue community classification batch: %w", err)
		}
		report.Created += created
		report.Revived += revived
		report.AlreadyPresent += scanned - created - revived
		if scanned < r.batchSize {
			return report, nil
		}
		after = last
	}
}

// EnqueuePostsAndComments queues a job for every undeleted post and comment
// indexed in [since, until) at its current CID, except those whose author has
// been erased; those count as neither created nor present. A comment's
// community is its root post's community, empty when the root post is not
// indexed.
func (r *ClassificationReenqueueRepository) EnqueuePostsAndComments(ctx context.Context, evaluation classification.Evaluation, since, until time.Time) (classification.ReenqueueReport, error) {
	posts, err := r.enqueueIndexedRange(ctx, evaluation, classification.SubjectKindPost, `
		SELECT p.uri, p.indexed_at, p.author_did
		FROM posts p
		WHERE p.deleted_at IS NULL AND p.indexed_at >= $1 AND p.indexed_at < $2
			AND ($3::timestamptz IS NULL OR (p.indexed_at, p.uri) > ($3, $4))
		ORDER BY p.indexed_at, p.uri
		LIMIT $5`, `
		SELECT p.uri AS subject_uri, p.cid AS record_cid, p.author_did, p.community_did
		FROM posts p
		WHERE p.uri = ANY($5::text[]) AND p.deleted_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM deleted_accounts d WHERE d.did = p.author_did)`, since, until)
	if err != nil {
		return classification.ReenqueueReport{}, err
	}
	comments, err := r.enqueueIndexedRange(ctx, evaluation, classification.SubjectKindComment, `
		SELECT c.uri, c.indexed_at, c.commenter_did
		FROM comments c
		WHERE c.deleted_at IS NULL AND c.indexed_at >= $1 AND c.indexed_at < $2
			AND ($3::timestamptz IS NULL OR (c.indexed_at, c.uri) > ($3, $4))
		ORDER BY c.indexed_at, c.uri
		LIMIT $5`, `
		SELECT c.uri AS subject_uri, c.cid AS record_cid, c.commenter_did AS author_did,
		       COALESCE(root.community_did, '') AS community_did
		FROM comments c
		LEFT JOIN posts root ON root.uri = c.root_uri
		WHERE c.uri = ANY($5::text[]) AND c.deleted_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM deleted_accounts d WHERE d.did = c.commenter_did)`, since, until)
	if err != nil {
		return classification.ReenqueueReport{}, err
	}
	return classification.ReenqueueReport{
		Created:        posts.Created + comments.Created,
		Revived:        posts.Revived + comments.Revived,
		AlreadyPresent: posts.AlreadyPresent + comments.AlreadyPresent,
	}, nil
}

// enqueueIndexedRange walks selectSQL's rows by the keyset (indexed_at, uri),
// one READ COMMITTED transaction per batch. selectSQL returns (uri,
// indexed_at, author_did) and takes the range as $1, $2, the cursor as $3, $4
// (NULL on the first batch) and the batch size as $5. Each batch takes its
// authors' shared erasure locks in DID order, then inserts in a new statement,
// so it sees any erasure committed while it waited: eligibleSQL, the batch CTE
// of reenqueueInsertSQL, takes the batch's URIs as $5 and drops subjects
// deleted or erased since they were selected.
func (r *ClassificationReenqueueRepository) enqueueIndexedRange(ctx context.Context, evaluation classification.Evaluation, kind classification.SubjectKind, selectSQL, eligibleSQL string, since, until time.Time) (classification.ReenqueueReport, error) {
	insertQuery := `WITH batch AS (` + eligibleSQL + `),` + reenqueueInsertSQL + `
		SELECT (SELECT COUNT(*) FROM batch), ` + reenqueueCountsSQL

	var report classification.ReenqueueReport
	var afterIndexedAt sql.NullTime
	var afterURI sql.NullString
	for {
		scanned, eligible, batchReport, lastIndexedAt, lastURI, err := r.enqueueIndexedBatch(ctx, evaluation, kind,
			selectSQL, insertQuery, since, until, afterIndexedAt, afterURI)
		if err != nil {
			return classification.ReenqueueReport{}, fmt.Errorf("enqueue %s classification batch: %w", kind, err)
		}
		report.Created += batchReport.Created
		report.Revived += batchReport.Revived
		report.AlreadyPresent += eligible - batchReport.Created - batchReport.Revived
		if scanned < r.batchSize {
			return report, nil
		}
		afterIndexedAt, afterURI = lastIndexedAt, lastURI
	}
}

// enqueueIndexedBatch runs one batch of enqueueIndexedRange in its own
// transaction and reports the rows selected, the rows still eligible after
// the erasure locks, the jobs created and revived (AlreadyPresent unset) and
// the batch's last keyset.
func (r *ClassificationReenqueueRepository) enqueueIndexedBatch(ctx context.Context, evaluation classification.Evaluation, kind classification.SubjectKind, selectSQL, insertQuery string, since, until time.Time, afterIndexedAt sql.NullTime, afterURI sql.NullString) (scanned, eligible int, written classification.ReenqueueReport, lastIndexedAt sql.NullTime, lastURI sql.NullString, err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, selectSQL, since, until, afterIndexedAt, afterURI, r.batchSize)
	if err != nil {
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("select batch: %w", err)
	}
	var uris []string
	authors := map[string]struct{}{}
	for rows.Next() {
		var uri, authorDID string
		var indexedAt time.Time
		if err := rows.Scan(&uri, &indexedAt, &authorDID); err != nil {
			_ = rows.Close()
			return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("scan batch: %w", err)
		}
		uris = append(uris, uri)
		authors[authorDID] = struct{}{}
		lastIndexedAt = sql.NullTime{Time: indexedAt, Valid: true}
		lastURI = sql.NullString{String: uri, Valid: true}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("read batch: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("close batch: %w", err)
	}
	if len(uris) == 0 {
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, nil
	}

	authorDIDs := make([]string, 0, len(authors))
	for authorDID := range authors {
		authorDIDs = append(authorDIDs, authorDID)
	}
	sort.Strings(authorDIDs)
	for _, authorDID := range authorDIDs {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock_shared("+ErasureLockKeySQL+")", authorDID); err != nil {
			return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("lock author against erasure: %w", err)
		}
	}

	if err := tx.QueryRowContext(ctx, insertQuery,
		string(kind), evaluation.PolicyVersion, evaluation.TextModelID, evaluation.ImageModelID, pq.Array(uris),
	).Scan(&eligible, &written.Created, &written.Revived); err != nil {
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("insert jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, classification.ReenqueueReport{}, sql.NullTime{}, sql.NullString{}, fmt.Errorf("commit: %w", err)
	}
	return len(uris), eligible, written, lastIndexedAt, lastURI, nil
}

// ClassificationSummaryRepository reads per-community classification
// summaries from Postgres.
type ClassificationSummaryRepository struct {
	db *sql.DB
}

var _ classification.SummaryRepository = (*ClassificationSummaryRepository)(nil)

// NewClassificationSummaryRepository returns the summary repository.
func NewClassificationSummaryRepository(db *sql.DB) *ClassificationSummaryRepository {
	return &ClassificationSummaryRepository{db: db}
}

// CommunitySummary reads one community's summary for the query: per kind,
// the subjects with a result created in [Since, Until) that answers the
// question and those whose latest such result has it at or above
// MinProbability, and the community's NSFW flag events in the window, newest
// first. Community-kind results are not counted. It returns classification.ErrCommunityNotFound
// when no such community is indexed.
func (r *ClassificationSummaryRepository) CommunitySummary(ctx context.Context, query classification.SummaryQuery) (classification.CommunitySummary, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM communities WHERE did = $1)`, query.CommunityDID,
	).Scan(&exists); err != nil {
		return classification.CommunitySummary{}, fmt.Errorf("look up community: %w", err)
	}
	if !exists {
		return classification.CommunitySummary{}, classification.ErrCommunityNotFound
	}

	summary := classification.CommunitySummary{
		CommunityDID:   query.CommunityDID,
		Question:       query.Question,
		MinProbability: query.MinProbability,
		Since:          query.Since,
		Until:          query.Until,
		FlagEvents:     []classification.FlagEvent{},
	}

	rows, err := r.db.QueryContext(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (subject_uri) subject_kind, probabilities
			FROM classification_results
			WHERE community_did = $1 AND subject_kind IN ('post', 'comment')
				AND created_at >= $3 AND created_at < $4 AND probabilities ? $2
			ORDER BY subject_uri, created_at DESC, id DESC
		)
		SELECT subject_kind, COUNT(*),
		       COUNT(*) FILTER (WHERE (probabilities ->> $2)::double precision >= $5)
		FROM latest
		GROUP BY subject_kind`,
		query.CommunityDID, query.Question, query.Since, query.Until, query.MinProbability)
	if err != nil {
		return classification.CommunitySummary{}, fmt.Errorf("count classification results: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind string
		var counts classification.KindSummary
		if err := rows.Scan(&kind, &counts.Scored, &counts.Above); err != nil {
			return classification.CommunitySummary{}, fmt.Errorf("scan classification result counts: %w", err)
		}
		switch classification.SubjectKind(kind) {
		case classification.SubjectKindPost:
			summary.Posts = counts
		case classification.SubjectKindComment:
			summary.Comments = counts
		default:
			return classification.CommunitySummary{}, fmt.Errorf("unexpected subject kind %q in classification result counts", kind)
		}
	}
	if err := rows.Err(); err != nil {
		return classification.CommunitySummary{}, fmt.Errorf("read classification result counts: %w", err)
	}

	events, err := r.flagEvents(ctx, query)
	if err != nil {
		return classification.CommunitySummary{}, err
	}
	summary.FlagEvents = events
	return summary, nil
}

func (r *ClassificationSummaryRepository) flagEvents(ctx context.Context, query classification.SummaryQuery) ([]classification.FlagEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT old_nsfw, new_nsfw, occurred_at, delivered_at IS NOT NULL
		FROM community_nsfw_flag_events
		WHERE community_did = $1 AND occurred_at >= $2 AND occurred_at < $3
		ORDER BY occurred_at DESC, id DESC`,
		query.CommunityDID, query.Since, query.Until)
	if err != nil {
		return nil, fmt.Errorf("list NSFW flag events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := []classification.FlagEvent{}
	for rows.Next() {
		var event classification.FlagEvent
		if err := rows.Scan(&event.OldNSFW, &event.NewNSFW, &event.OccurredAt, &event.Delivered); err != nil {
			return nil, fmt.Errorf("scan NSFW flag event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read NSFW flag events: %w", err)
	}
	return events, nil
}
