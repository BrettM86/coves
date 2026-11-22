//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/classification"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassificationEnqueueKeepsOneJobPerIdentity: enqueueing a job whose
// identity (subject URI, record CID, policy version, text model, image model)
// already exists leaves the one row and returns no error, as a post deleted
// and re-created with identical content does; a different policy version is a
// different identity and gets its own row.
func TestClassificationEnqueueKeepsOneJobPerIdentity(t *testing.T) {
	db := testkit.DB(t)
	repo := NewClassificationRepository(db, classificationInstanceDID)
	const subjectURI = "at://did:plc:clsidentity/social.coves.community.post/3lclsidentity1"
	job := classification.NewJob{
		SubjectURI:   subjectURI,
		SubjectKind:  classification.SubjectKindPost,
		RecordCID:    "bafyclsidentity",
		AuthorDID:    "did:plc:clsidentity",
		CommunityDID: "did:plc:clsidentitycommunity",
		Evaluation: classification.Evaluation{
			PolicyVersion: "v1",
			TextModelID:   "@cf/cloudflare/clef-flash",
			ImageModelID:  "@cf/cloudflare/clef",
		},
	}
	enqueue := func(t *testing.T, job classification.NewJob) {
		t.Helper()
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		if err := repo.EnqueueInTx(ctx, tx, job, true); err != nil {
			_ = tx.Rollback()
			require.NoError(t, err, "enqueueing %s under policy %s", job.RecordCID, job.Evaluation.PolicyVersion)
		}
		require.NoError(t, tx.Commit())
	}
	jobCount := func(t *testing.T) int {
		t.Helper()
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT count(*) FROM classification_jobs WHERE subject_uri = $1`, subjectURI).Scan(&count))
		return count
	}

	enqueue(t, job)
	enqueue(t, job)
	assert.Equal(t, 1, jobCount(t), "an existing job identity is left as it is")

	nextPolicy := job
	nextPolicy.Evaluation.PolicyVersion = "v2"
	enqueue(t, nextPolicy)
	assert.Equal(t, 2, jobCount(t), "another policy version is another job")
}

// TestClassificationWorkerClosesAnErasedAuthorsJob: a job whose author has an
// erasure marker but whose rows erasure has not removed (a job enqueued after
// the erasure committed) is not visible: the worker closes it with no request
// and stores no result (R5).
func TestClassificationWorkerClosesAnErasedAuthorsJob(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	authorDID := f.author(t, "clsmarked")
	postURI := f.visiblePost(t, f.community, authorDID, "3lclsmarked01", "Marked petrel")
	f.enqueue(t, postURI, classificationT0)
	_, err := f.db.ExecContext(t.Context(),
		`INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, authorDID)
	require.NoError(t, err)

	subject, err := f.repo.LoadPostSubject(t.Context(), postURI)
	require.NoError(t, err)
	assert.Equal(t, classification.VisibilityHidden, subject.Visibility, "an erased author's post is not visible")

	f.run(t, f.worker(t, nil), classificationT0)

	job := f.job(t, postURI)
	assert.Equal(t, classification.StatusClosed, job.Status)
	assert.Equal(t, classification.CloseReasonNotVisible, job.CloseReason.String)
	assert.Equal(t, 0, len(f.transport.Requests()), "no request for an erased author")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_results WHERE author_did = $1`, authorDID))
}

// TestClassificationReserveRequestCapsRequestsAcrossModels: the daily cap
// counts requests across the text and image models together, while each
// model keeps its own usage row; the next UTC day has a fresh budget, and
// concurrent reservations never overshoot the cap (R2).
func TestClassificationReserveRequestCapsRequestsAcrossModels(t *testing.T) {
	const (
		textModelID  = "@cf/cloudflare/clef-flash"
		imageModelID = "@cf/cloudflare/clef"
	)
	dayOne := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	requestsOn := func(t *testing.T, db *sql.DB, modelID, usageDate string) int {
		t.Helper()
		var requests int
		err := db.QueryRowContext(t.Context(), `
			SELECT requests FROM classification_usage WHERE model_id = $1 AND usage_date = $2
		`, modelID, usageDate).Scan(&requests)
		if errors.Is(err, sql.ErrNoRows) {
			return 0
		}
		require.NoError(t, err)
		return requests
	}

	t.Run("text and image requests share one cap", func(t *testing.T) {
		db := testkit.DB(t)
		repo := NewClassificationRepository(db, classificationInstanceDID)
		reserve := func(modelID string, now time.Time) bool {
			t.Helper()
			reserved, err := repo.ReserveRequest(t.Context(), modelID, now, 3)
			require.NoError(t, err)
			return reserved
		}

		assert.True(t, reserve(textModelID, dayOne))
		assert.True(t, reserve(imageModelID, dayOne))
		assert.True(t, reserve(textModelID, dayOne))
		assert.False(t, reserve(imageModelID, dayOne), "a fourth request of the image model is over the cap")
		assert.False(t, reserve(textModelID, dayOne), "a fourth request of the text model is over the cap")

		assert.Equal(t, 2, requestsOn(t, db, textModelID, "2026-11-12"))
		assert.Equal(t, 1, requestsOn(t, db, imageModelID, "2026-11-12"))

		assert.True(t, reserve(imageModelID, dayOne.Add(24*time.Hour)), "the next UTC day has a fresh budget")
		assert.Equal(t, 1, requestsOn(t, db, imageModelID, "2026-11-13"))
	})

	t.Run("concurrent reservations across models stop at the cap", func(t *testing.T) {
		db := testkit.DB(t)
		repo := NewClassificationRepository(db, classificationInstanceDID)
		const attempts = 20
		var (
			wait     sync.WaitGroup
			mu       sync.Mutex
			granted  int
			failures []error
		)
		start := make(chan struct{})
		for attempt := range attempts {
			modelID := textModelID
			if attempt%2 == 1 {
				modelID = imageModelID
			}
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				reserved, err := repo.ReserveRequest(t.Context(), modelID, dayOne, 5)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failures = append(failures, err)
				}
				if reserved {
					granted++
				}
			}()
		}
		close(start)
		wait.Wait()

		require.Empty(t, failures)
		assert.Equal(t, 5, granted)
		assert.Equal(t, 5, requestsOn(t, db, textModelID, "2026-11-12")+requestsOn(t, db, imageModelID, "2026-11-12"))
	})
}

// TestClassificationStoreResultWaitsForErasure: an erasure that has deleted
// a revived job's partial result but not yet its job, and is still open,
// makes the job's completion wait instead of deadlocking with it. The
// erasure commits cleanly, and the completion then finds no job and stores
// nothing.
func TestClassificationStoreResultWaitsForErasure(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	ctx := t.Context()
	authorDID := f.author(t, "clsstoreerase")
	postURI := f.visiblePost(t, f.community, authorDID, "3lclsstoreer1", "Erased shearwater")
	f.enqueue(t, postURI, classificationT0)
	evaluation := classification.Evaluation{
		PolicyVersion: "v1",
		TextModelID:   "@cf/cloudflare/clef-flash",
		ImageModelID:  "@cf/cloudflare/clef",
	}
	claimed, err := f.repo.ClaimDue(ctx, evaluation, classificationT0, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	job := claimed[0]

	stored, err := f.repo.StorePartialResult(ctx, job, classification.Result{
		ModelID:       "@cf/cloudflare/clef",
		Probabilities: map[string]float64{"sexual_minors": 0.01},
		ImagesSent:    4,
		ImagesSkipped: 4,
	}, classification.Failure{ErrorCode: "provider_unavailable", Final: true})
	require.NoError(t, err)
	require.True(t, stored, "the failed job stores its partial result")
	_, err = f.db.ExecContext(ctx,
		`UPDATE classification_jobs SET status = $2, next_attempt_at = $3 WHERE id = $1`,
		job.ID, classification.StatusRetrying, classificationT0)
	require.NoError(t, err, "revive the failed job")

	// The erasure's own order: exclusive erasure lock, results, then jobs.
	erasure, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = erasure.Rollback() }()
	_, err = erasure.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+ErasureLockKeySQL+")", authorDID)
	require.NoError(t, err)
	var erasurePID int
	require.NoError(t, erasure.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&erasurePID))
	_, err = erasure.ExecContext(ctx, `DELETE FROM classification_results WHERE author_did = $1`, authorDID)
	require.NoError(t, err)

	type storeOutcome struct {
		stored bool
		err    error
	}
	completion := make(chan storeOutcome, 1)
	go func() {
		storeContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stored, err := f.repo.StoreResult(storeContext, job, classification.Result{
			ModelID:       "@cf/cloudflare/clef",
			Probabilities: map[string]float64{"sexual_minors": 0.02},
			ImagesSent:    8,
		})
		completion <- storeOutcome{stored: stored, err: err}
	}()

	require.Eventually(t, func() bool {
		var waiting int
		err := f.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, erasurePID).Scan(&waiting)
		return err == nil && waiting > 0
	}, 10*time.Second, 10*time.Millisecond, "the completion waits on the open erasure")

	_, err = erasure.ExecContext(ctx, `DELETE FROM classification_jobs WHERE author_did = $1`, authorDID)
	require.NoError(t, err, "the erasure deletes the job without a deadlock")
	require.NoError(t, erasure.Commit())

	var outcome storeOutcome
	select {
	case outcome = <-completion:
	case <-time.After(10 * time.Second):
		t.Fatal("the completion did not finish within 10s of the erasure's commit")
	}
	require.NoError(t, outcome.err, "the completion fails without a deadlock")
	assert.False(t, outcome.stored, "the completion reports its job gone")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_jobs WHERE author_did = $1`, authorDID))
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_results WHERE author_did = $1`, authorDID))
}
