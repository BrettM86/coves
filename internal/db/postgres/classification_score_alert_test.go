//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Classifier score alerts (docs/PRD_CONTENT_CLASSIFICATION.md, chunk 07):
// the worker stores an alert row per met (result, question) pair with its
// result, and the alert service delivers it after commit and from its sweep,
// through the real Postgres repositories.

// scoreAlertMinorsAndSpam are the thresholds the suites run with.
var scoreAlertMinorsAndSpam = []classification.ScoreAlertThreshold{
	{QuestionKey: "sexual_minors", Threshold: 0.3},
	{QuestionKey: "spam_scam", Threshold: 0.5},
}

// scoreAlertProbabilities is a full answer whose sexual_minors and
// spam_scam probabilities are minors and spam; every other key is low.
func scoreAlertProbabilities(minors, spam float64) map[string]float64 {
	return map[string]float64{
		"sexual_explicit":   0.01,
		"sexual_suggestive": 0.02,
		"sexual_minors":     minors,
		"graphic_violence":  0.04,
		"hate_harassment":   0.05,
		"spam_scam":         spam,
	}
}

type scoreAlertRow struct {
	ResultID      int64
	QuestionKey   string
	SubjectKind   string
	DeliveredAt   sql.NullTime
	Attempts      int
	LastErrorCode sql.NullString
	NextAttemptAt sql.NullTime
}

// scoreAlerts reads every classification_score_alerts row, ordered by
// question key, with its times in UTC.
func (f *classificationFixture) scoreAlerts(t *testing.T) []scoreAlertRow {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(), `
		SELECT result_id, question_key, subject_kind, delivered_at, attempts, last_error_code, next_attempt_at
		FROM classification_score_alerts
		ORDER BY question_key, id
	`)
	require.NoError(t, err, "reading classification_score_alerts")
	defer func() { _ = rows.Close() }()
	var out []scoreAlertRow
	for rows.Next() {
		var row scoreAlertRow
		require.NoError(t, rows.Scan(&row.ResultID, &row.QuestionKey, &row.SubjectKind, &row.DeliveredAt,
			&row.Attempts, &row.LastErrorCode, &row.NextAttemptAt))
		row.DeliveredAt.Time = row.DeliveredAt.Time.UTC()
		row.NextAttemptAt.Time = row.NextAttemptAt.Time.UTC()
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// resultID reads the id of the single result of subjectURI.
func (f *classificationFixture) resultID(t *testing.T, subjectURI string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT id FROM classification_results WHERE subject_uri = $1`, subjectURI).Scan(&id))
	return id
}

// scoreAlertWorker builds the alert service over the real repository with
// sender and cfg, on the fixture's clock, and a worker that reads subject
// images from pds, raises alerts against thresholds and notifies that
// service.
func (f *classificationFixture) scoreAlertWorker(t *testing.T, pds *classificationImagePDS, thresholds []classification.ScoreAlertThreshold, sender classification.MessageSender, cfg classification.ScoreAlertDeliveryConfig) (*classification.Worker, *classification.ScoreAlertService) {
	t.Helper()
	service := classification.NewScoreAlertService(NewClassificationScoreAlertRepository(f.db), sender, cfg, f.clock.Now, signalLogger())
	worker := f.imageWorker(t, pds, nil, classification.WithScoreAlerts(thresholds, service))
	return worker, service
}

// imagePost seeds a visible post carrying one fetchable, referenced image
// from pds, so its request is asked every question, sexual_minors included.
// It returns the post URI.
func (f *classificationFixture) imagePost(t *testing.T, pds *classificationImagePDS, authorDID, rkey, title string) string {
	t.Helper()
	postURI := postV2URI(authorDID, rkey)
	cid := pds.addImage(t, 701)
	f.referenceBlobs(t, authorDID, postURI, postImageReferences(cid))
	require.Equal(t, postURI, f.visiblePostWithEmbed(t, f.community, authorDID, rkey, title, "", classificationGalleryJSON(cid)))
	return postURI
}

// sweepAt sets the clock to at and runs one alert sweep.
func (f *classificationFixture) sweepAt(t *testing.T, service *classification.ScoreAlertService, at time.Time) {
	t.Helper()
	f.clock.Set(at)
	require.NoError(t, service.Sweep(t.Context()))
}

// scoreAlertFailed is the send_failed error code column value.
func scoreAlertFailed() sql.NullString {
	return sql.NullString{String: "send_failed", Valid: true}
}

// scoreAlertTime is a set timestamp column value.
func scoreAlertTime(at time.Time) sql.NullTime { return sql.NullTime{Time: at, Valid: true} }

// TestClassificationScoreAlertRetriesThenDeliversOnce: a post whose
// sexual_minors probability meets its threshold stores one alert with its
// result; the after-commit send fails and is retried by the sweep after the
// base delay, which delivers it once (B4).
func TestClassificationScoreAlertRetriesThenDeliversOnce(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(scoreAlertProbabilities(0.31, 0.2), classificationCannedInputTokens))
	sender := &signalSender{err: errors.New("telegram unavailable")}
	pds := &classificationImagePDS{}
	worker, service := f.scoreAlertWorker(t, pds, scoreAlertMinorsAndSpam, sender,
		classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute})

	postURI := f.imagePost(t, pds, f.author(t, "clsalertpost"), "3lclsalert0001", "Alert lighthouse gulls")
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	assert.Equal(t, "classified", f.job(t, postURI).Status)
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, postURI))
	resultID := f.resultID(t, postURI)
	assert.Equal(t, []scoreAlertRow{{
		ResultID:      resultID,
		QuestionKey:   "sexual_minors",
		SubjectKind:   "post",
		Attempts:      1,
		LastErrorCode: scoreAlertFailed(),
		NextAttemptAt: scoreAlertTime(time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC)),
	}}, f.scoreAlerts(t), "one alert for the met question; the failed after-commit send is retried after the base delay")
	require.Len(t, sender.Attempts(), 1, "the after-commit delivery sends once")

	f.sweepAt(t, service, time.Date(2026, 11, 12, 12, 0, 30, 0, time.UTC))
	assert.Len(t, sender.Attempts(), 1, "an alert before its next attempt is not sent")

	sender.SetErr(nil)
	f.sweepAt(t, service, time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC))
	assert.Equal(t, []string{fmt.Sprintf("Classifier score alert: result %d, question sexual_minors, subject post", resultID)},
		sender.Delivered(), "the due alert is delivered once, naming its result")
	alerts := f.scoreAlerts(t)
	require.Len(t, alerts, 1)
	assert.Equal(t, scoreAlertTime(time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC)), alerts[0].DeliveredAt)

	f.sweepAt(t, service, time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC))
	assert.Len(t, sender.Attempts(), 2, "a delivered alert is not sent again")
}

// TestClassificationScoreAlertOnePerMetPair: the worker stores one alert per
// met question with the result, carrying the subject kind, and none when no
// question is met (B4b).
func TestClassificationScoreAlertOnePerMetPair(t *testing.T) {
	for _, row := range []struct {
		name          string
		probabilities map[string]float64
		seed          func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string
		// want lists the expected alerts' (question, subject kind).
		want [][2]string
	}{
		{
			name:          "comment meeting both thresholds",
			probabilities: scoreAlertProbabilities(0.5, 0.9),
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string {
				root := f.visiblePost(t, f.community, f.author(t, "clsalertroot"), "3lclsalertrt01", "Alert root heron")
				commenterDID := f.author(t, "clsalertcmt")
				cid := pds.addImage(t, 702)
				comment := seedClassificationCommentWithEmbed(t, f.db, commenterDID, "3lclsalertcm01", root, root, "Alert reply heron",
					classificationGalleryJSON(cid))
				f.referenceBlobs(t, commenterDID, comment, []embedblobs.Reference{{CID: cid, Kind: embedblobs.KindCommentImage}})
				f.enqueueComment(t, comment, classificationT0)
				return comment
			},
			want: [][2]string{{"sexual_minors", "comment"}, {"spam_scam", "comment"}},
		},
		{
			name:          "post meeting no threshold",
			probabilities: scoreAlertProbabilities(0.29, 0.49),
			seed: func(t *testing.T, f *classificationFixture, pds *classificationImagePDS) string {
				post := f.imagePost(t, pds, f.author(t, "clsalertnone"), "3lclsalertno01", "Quiet harbour terns")
				f.enqueue(t, post, classificationT0)
				return post
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(row.probabilities, classificationCannedInputTokens))
			sender := &signalSender{}
			pds := &classificationImagePDS{}
			worker, _ := f.scoreAlertWorker(t, pds, scoreAlertMinorsAndSpam, sender,
				classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute})
			subjectURI := row.seed(t, f, pds)

			f.run(t, worker, classificationT0)

			assert.Equal(t, "classified", f.job(t, subjectURI).Status)
			assert.Equal(t, row.probabilities["sexual_minors"], f.result(t, subjectURI).Probabilities["sexual_minors"],
				"the subject carries an image, so its sexual_minors score is stored")
			resultID := f.resultID(t, subjectURI)
			got := [][2]string{}
			for _, alert := range f.scoreAlerts(t) {
				assert.Equal(t, resultID, alert.ResultID, "each alert names the stored result")
				got = append(got, [2]string{alert.QuestionKey, alert.SubjectKind})
			}
			want := row.want
			if want == nil {
				want = [][2]string{}
			}
			assert.Equal(t, want, got)
			assert.Len(t, sender.Delivered(), len(row.want), "each stored alert is delivered after commit, and nothing else is sent")
		})
	}
}

// TestClassificationScoreAlertStopsAtTheAttemptLimit: an alert whose sends
// always fail is sent AttemptLimit times with backoff, then left undelivered
// (B5).
func TestClassificationScoreAlertStopsAtTheAttemptLimit(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(scoreAlertProbabilities(0.9, 0.1), classificationCannedInputTokens))
	sender := &signalSender{err: errors.New("telegram unavailable")}
	pds := &classificationImagePDS{}
	worker, service := f.scoreAlertWorker(t, pds, scoreAlertMinorsAndSpam, sender,
		classification.ScoreAlertDeliveryConfig{AttemptLimit: 2, RetryBaseDelay: time.Minute})

	postURI := f.imagePost(t, pds, f.author(t, "clsalertlimit"), "3lclsalertlm01", "Limit puffin burrow")
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)
	require.Len(t, sender.Attempts(), 1, "the after-commit send is the first attempt")

	f.sweepAt(t, service, time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC))
	require.Len(t, sender.Attempts(), 2, "the sweep at the retry time is the second attempt")
	assert.Equal(t, []scoreAlertRow{{
		ResultID:      f.resultID(t, postURI),
		QuestionKey:   "sexual_minors",
		SubjectKind:   "post",
		Attempts:      2,
		LastErrorCode: scoreAlertFailed(),
		NextAttemptAt: scoreAlertTime(time.Date(2026, 11, 12, 12, 3, 0, 0, time.UTC)),
	}}, f.scoreAlerts(t), "the second failure backs off twice the base delay")

	f.sweepAt(t, service, time.Date(2026, 11, 12, 13, 1, 0, 0, time.UTC))
	assert.Len(t, sender.Attempts(), 2, "an alert at the attempt limit is not sent again")
	alerts := f.scoreAlerts(t)
	require.Len(t, alerts, 1)
	assert.Equal(t, 2, alerts[0].Attempts)
	assert.False(t, alerts[0].DeliveredAt.Valid, "an abandoned alert stays undelivered")
}

// TestClassificationScoreAlertNotRaisedForADiscardedResult: a result whose
// job was closed while its request was in flight is discarded with no alert
// row and no send (B6).
func TestClassificationScoreAlertNotRaisedForADiscardedResult(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(scoreAlertProbabilities(0.9, 0.9), classificationCannedInputTokens))
	sender := &signalSender{}
	worker, service := f.scoreAlertWorker(t, &classificationImagePDS{}, scoreAlertMinorsAndSpam, sender,
		classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute})

	postURI := f.visiblePost(t, f.community, f.author(t, "clsalertgone"), "3lclsalertgn01", "Discarded cormorant roost")
	f.enqueue(t, postURI, classificationT0)
	var (
		once     sync.Once
		closeErr error
	)
	f.transport.SetBeforeAnswer(func(*http.Request) {
		once.Do(func() {
			_, closeErr = f.db.ExecContext(t.Context(),
				`UPDATE classification_jobs SET status = 'closed', close_reason = 'superseded' WHERE subject_uri = $1`, postURI)
		})
	})

	f.run(t, worker, classificationT0)

	require.NoError(t, closeErr)
	require.Len(t, f.transport.Requests(), 1)
	assert.Equal(t, "closed", f.job(t, postURI).Status)
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, postURI))
	assert.Empty(t, f.scoreAlerts(t), "a discarded result raises no alert")
	f.sweepAt(t, service, time.Date(2026, 11, 12, 12, 5, 0, 0, time.UTC))
	assert.Empty(t, sender.Attempts(), "nothing is sent for a discarded result")
}

// TestClassificationScoreAlertDeliveryIsExactlyOnce: two due alerts of one
// stored result are each sent once when an after-commit delivery and a sweep
// overlap, and when a sweep's run outlives its first claim's lease; an
// outcome written under an expired lease is refused (B7).
func TestClassificationScoreAlertDeliveryIsExactlyOnce(t *testing.T) {
	// seedTwoAlerts stores one post result meeting both thresholds with no
	// notifier, so its two alerts (sexual_minors, spam_scam) are due and
	// unsent. It returns the result's job ID and the two alert messages.
	seedTwoAlerts := func(t *testing.T, f *classificationFixture, label string) (jobID int64, minorsText, spamText string) {
		t.Helper()
		pds := &classificationImagePDS{}
		worker := f.imageWorker(t, pds, nil, classification.WithScoreAlerts(scoreAlertMinorsAndSpam, nil))
		postURI := f.imagePost(t, pds, f.author(t, label), "3lclsalertxo01", "Exactly once kittiwake ledge")
		f.enqueue(t, postURI, classificationT0)
		f.run(t, worker, classificationT0)
		require.Len(t, f.scoreAlerts(t), 2, "both thresholds are met, so two alerts are stored")
		require.NoError(t, f.db.QueryRowContext(t.Context(),
			`SELECT id FROM classification_jobs WHERE subject_uri = $1`, postURI).Scan(&jobID))
		resultID := f.resultID(t, postURI)
		return jobID,
			fmt.Sprintf("Classifier score alert: result %d, question sexual_minors, subject post", resultID),
			fmt.Sprintf("Classifier score alert: result %d, question spam_scam, subject post", resultID)
	}

	t.Run("sweep during a blocked after-commit send", func(t *testing.T) {
		f := newClassificationFixture(t, cloudflaretest.Canned(scoreAlertProbabilities(0.5, 0.9), classificationCannedInputTokens))
		jobID, minorsText, spamText := seedTwoAlerts(t, f, "clsalertxoa")
		sender := &blockingSignalSender{entered: make(chan struct{}), release: make(chan struct{})}
		repo := NewClassificationScoreAlertRepository(f.db)
		cfg := classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute}
		afterCommit := classification.NewScoreAlertService(repo, sender, cfg, f.clock.Now, signalLogger())
		sweeper := classification.NewScoreAlertService(repo, sender, cfg, f.clock.Now, signalLogger())

		delivered := make(chan struct{})
		go func() {
			defer close(delivered)
			afterCommit.DeliverPending(t.Context(), jobID)
		}()
		select {
		case <-sender.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("DeliverPending never sent a due alert")
		}
		require.NoError(t, sweeper.Sweep(t.Context()), "sweep while the after-commit send is in flight")
		close(sender.release)
		<-delivered

		assert.ElementsMatch(t, []string{minorsText, spamText}, sender.Attempts(), "each alert is sent exactly once")
		for _, alert := range f.scoreAlerts(t) {
			assert.Equal(t, scoreAlertTime(classificationT0), alert.DeliveredAt, "%s is delivered", alert.QuestionKey)
			assert.Equal(t, 0, alert.Attempts, "%s records no failed attempt", alert.QuestionKey)
		}
	})

	t.Run("sweep outliving its first lease", func(t *testing.T) {
		f := newClassificationFixture(t, cloudflaretest.Canned(scoreAlertProbabilities(0.5, 0.9), classificationCannedInputTokens))
		_, minorsText, spamText := seedTwoAlerts(t, f, "clsalertxob")
		sender := newGatedFailingSender(1, 2)
		repo := NewClassificationScoreAlertRepository(f.db)
		// A ten-minute backoff keeps a failed alert out of the second sweep.
		cfg := classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: 10 * time.Minute}
		slow := classification.NewScoreAlertService(repo, sender, cfg, f.clock.Now, signalLogger())
		other := classification.NewScoreAlertService(repo, sender, cfg, f.clock.Now, signalLogger())

		swept := make(chan error, 1)
		go func() { swept <- slow.Sweep(t.Context()) }()

		// The first send takes four minutes, inside the lease taken at T0.
		sender.waitEntered(t, 1)
		f.clock.Set(time.Date(2026, 11, 12, 12, 4, 0, 0, time.UTC))
		close(sender.release[1])

		// The second send is still running at T0+6m, past a lease taken at T0.
		sender.waitEntered(t, 2)
		f.clock.Set(time.Date(2026, 11, 12, 12, 6, 0, 0, time.UTC))
		require.NoError(t, other.Sweep(t.Context()), "a second sweep while the slow sweep sends")
		close(sender.release[2])
		require.NoError(t, <-swept)

		assert.Equal(t, 1, sender.sendsContaining(minorsText), "the first alert is sent once")
		assert.Equal(t, 1, sender.sendsContaining(spamText), "the second alert is sent once")
		alerts := f.scoreAlerts(t)
		require.Len(t, alerts, 2)
		assert.Equal(t, []scoreAlertRow{
			{
				ResultID:      alerts[0].ResultID,
				QuestionKey:   "sexual_minors",
				SubjectKind:   "post",
				Attempts:      1,
				LastErrorCode: scoreAlertFailed(),
				NextAttemptAt: scoreAlertTime(time.Date(2026, 11, 12, 12, 14, 0, 0, time.UTC)),
			},
			{
				ResultID:      alerts[0].ResultID,
				QuestionKey:   "spam_scam",
				SubjectKind:   "post",
				Attempts:      1,
				LastErrorCode: scoreAlertFailed(),
				NextAttemptAt: scoreAlertTime(time.Date(2026, 11, 12, 12, 16, 0, 0, time.UTC)),
			},
		}, alerts, "each failed send counts one attempt and backs off from the slow sweep's clock")
	})

	t.Run("outcome under an expired lease", func(t *testing.T) {
		f := newClassificationFixture(t, cloudflaretest.Canned(scoreAlertProbabilities(0.5, 0.9), classificationCannedInputTokens))
		jobID, _, _ := seedTwoAlerts(t, f, "clsalertxoc")
		repo := NewClassificationScoreAlertRepository(f.db)
		ctx := t.Context()
		firstLease := time.Date(2026, 11, 12, 12, 5, 0, 0, time.UTC)
		secondLease := time.Date(2026, 11, 12, 12, 11, 0, 0, time.UTC)

		stale, ok, err := repo.ClaimDueAlert(ctx, jobID, classificationT0, firstLease, 8)
		require.NoError(t, err)
		require.True(t, ok)
		other, ok, err := repo.ClaimDueAlert(ctx, jobID, classificationT0, firstLease, 8)
		require.NoError(t, err)
		require.True(t, ok)
		require.NotEqual(t, stale.ID, other.ID)
		require.NoError(t, repo.MarkAlertDelivered(ctx, other.ID, firstLease, classificationT0),
			"an outcome under a held lease is recorded")

		// Past the first lease, another claim takes the stale claim's alert.
		reclaimed, ok, err := repo.ClaimDueAlert(ctx, jobID, time.Date(2026, 11, 12, 12, 6, 0, 0, time.UTC), secondLease, 8)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, stale.ID, reclaimed.ID)

		assert.ErrorIs(t, repo.MarkAlertDelivered(ctx, stale.ID, firstLease, time.Date(2026, 11, 12, 12, 7, 0, 0, time.UTC)),
			classification.ErrScoreAlertLeaseLost, "delivery under the expired lease is refused")
		assert.ErrorIs(t, repo.MarkAlertFailed(ctx, stale.ID, firstLease, "send_failed", time.Date(2026, 11, 12, 12, 20, 0, 0, time.UTC)),
			classification.ErrScoreAlertLeaseLost, "failure under the expired lease is refused")
		assert.ErrorIs(t, repo.MarkAlertDelivered(ctx, other.ID, firstLease, time.Date(2026, 11, 12, 12, 7, 0, 0, time.UTC)),
			classification.ErrScoreAlertLeaseLost, "a delivered alert's lease is no longer held")

		for _, alert := range f.scoreAlerts(t) {
			if alert.DeliveredAt.Valid {
				assert.Equal(t, scoreAlertTime(classificationT0), alert.DeliveredAt, "the delivered alert keeps its first delivery time")
				continue
			}
			assert.Equal(t, 0, alert.Attempts, "the refused failure counts no attempt")
			assert.False(t, alert.LastErrorCode.Valid, "the refused failure records no error code")
			assert.Equal(t, scoreAlertTime(secondLease), alert.NextAttemptAt, "the newer claim's lease stands")
		}
	})
}

// TestClassificationSexualMinorsIsAskedOnlyWithAnImage: with a provider
// that answers 0.9 to every question and a sexual_minors alert at 0.3, a
// text-only post is not asked sexual_minors, so its result has no
// sexual_minors score and raises no alert; a post with a fetched image is
// asked it, stores its score and raises its alert.
func TestClassificationSexualMinorsIsAskedOnlyWithAnImage(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(map[string]float64{
		"sexual_explicit":   0.9,
		"sexual_suggestive": 0.9,
		"sexual_minors":     0.9,
		"graphic_violence":  0.9,
		"hate_harassment":   0.9,
		"spam_scam":         0.9,
	}, classificationCannedInputTokens))
	sender := &signalSender{}
	pds := &classificationImagePDS{}
	worker, _ := f.scoreAlertWorker(t, pds, []classification.ScoreAlertThreshold{{QuestionKey: "sexual_minors", Threshold: 0.3}},
		sender, classification.ScoreAlertDeliveryConfig{AttemptLimit: 8, RetryBaseDelay: time.Minute})

	textPost := f.visiblePost(t, f.community, f.author(t, "clsminorstext"), "3lclsminorstx1", "Court report on an abuse trial")
	imagePost := f.imagePost(t, pds, f.author(t, "clsminorsimage"), "3lclsminorsim1", "Beach day photos")
	f.enqueue(t, textPost, classificationT0)
	f.enqueue(t, imagePost, classificationT0)

	f.run(t, worker, classificationT0)

	assert.Equal(t, "classified", f.job(t, textPost).Status)
	assert.Equal(t, "classified", f.job(t, imagePost).Status)
	assert.Equal(t, map[string]float64{
		"sexual_explicit":   0.9,
		"sexual_suggestive": 0.9,
		"graphic_violence":  0.9,
		"hate_harassment":   0.9,
		"spam_scam":         0.9,
	}, f.result(t, textPost).Probabilities, "the text-only post is not asked sexual_minors")
	assert.Equal(t, map[string]float64{
		"sexual_explicit":   0.9,
		"sexual_suggestive": 0.9,
		"sexual_minors":     0.9,
		"graphic_violence":  0.9,
		"hate_harassment":   0.9,
		"spam_scam":         0.9,
	}, f.result(t, imagePost).Probabilities, "the image post is asked every question")
	alerts := f.scoreAlerts(t)
	require.Len(t, alerts, 1, "only the image post raises a sexual_minors alert")
	assert.Equal(t, f.resultID(t, imagePost), alerts[0].ResultID)
	assert.Equal(t, "sexual_minors", alerts[0].QuestionKey)
}
