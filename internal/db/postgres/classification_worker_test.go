//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/classifier/cloudflare"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/classification"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The classification worker suite (docs/PRD_CONTENT_CLASSIFICATION.md R2-R5):
// the worker, the Postgres queue and the real Workers AI client, with the
// provider answered in-process by the recording transport.

// classificationInstanceDID is the instance authority whose removals and
// account bans hide a subject from the worker.
const classificationInstanceDID = "did:web:classification-instance.test"

// classificationT0 is the fixed worker clock every suite starts from; jobs
// are seeded due at it.
var classificationT0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// classificationCanned is the canned provider answer: one literal probability
// per v1 policy key.
var classificationCanned = map[string]float64{
	"sexual_explicit":   0.01,
	"sexual_suggestive": 0.02,
	"sexual_minors":     0.03,
	"graphic_violence":  0.04,
	"hate_harassment":   0.05,
	"spam_scam":         0.06,
}

// classificationCannedInputTokens is the canned response's usage.input_tokens.
const classificationCannedInputTokens = 321

// classificationProviderFailure is a Workers AI 500 body.
var classificationProviderFailure = []byte(`{"result":null,"success":false,"errors":[{"code":7000,"message":"internal error"}],"messages":[]}`)

type classificationClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *classificationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *classificationClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// classificationLogCapture records every slog record's attributes, including
// attributes bound with Logger.With.
type classificationLogCapture struct {
	mu      sync.Mutex
	records [][]slog.Attr
}

type classificationLogHandler struct {
	capture *classificationLogCapture
	attrs   []slog.Attr
}

func (h classificationLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h classificationLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	h.capture.mu.Lock()
	defer h.capture.mu.Unlock()
	h.capture.records = append(h.capture.records, attrs)
	return nil
}

func (h classificationLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return classificationLogHandler{capture: h.capture, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h classificationLogHandler) WithGroup(string) slog.Handler { return h }

// countCode counts the records carrying the attribute code=<code>.
func (c *classificationLogCapture) countCode(code string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, attrs := range c.records {
		for _, attr := range attrs {
			if attr.Key == "code" && attr.Value.String() == code {
				count++
				break
			}
		}
	}
	return count
}

type classificationFixture struct {
	db        *sql.DB
	repo      *ClassificationRepository
	transport *cloudflaretest.Transport
	clock     *classificationClock
	logs      *classificationLogCapture
	community string
}

func newClassificationFixture(t *testing.T, respond cloudflaretest.Responder) *classificationFixture {
	t.Helper()
	db := testkit.DB(t)
	return &classificationFixture{
		db:        db,
		repo:      NewClassificationRepository(db, classificationInstanceDID),
		transport: cloudflaretest.NewTransport(respond),
		clock:     &classificationClock{now: classificationT0},
		logs:      &classificationLogCapture{},
		community: visibilityCommunity(t, db, "cls"),
	}
}

// worker builds a worker over the fixture's queue and the real Workers AI
// client on the recording transport. adjust changes the default config.
func (f *classificationFixture) worker(t *testing.T, adjust func(*classification.WorkerConfig)) *classification.Worker {
	t.Helper()
	return f.workerOn(t, f.transport, adjust)
}

// workerOn builds a worker like worker, with its client on transport and
// the worker options opts.
func (f *classificationFixture) workerOn(t *testing.T, transport *cloudflaretest.Transport, adjust func(*classification.WorkerConfig), opts ...classification.WorkerOption) *classification.Worker {
	t.Helper()
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:    "acct-classify",
		APIToken:     "token-classify",
		TextModelID:  "@cf/cloudflare/clef-flash",
		ImageModelID: "@cf/cloudflare/clef",
	}, transport.Client())
	cfg := classification.WorkerConfig{
		Evaluation: classification.Evaluation{
			PolicyVersion: "v1",
			TextModelID:   "@cf/cloudflare/clef-flash",
			ImageModelID:  "@cf/cloudflare/clef",
		},
		Concurrency:     1,
		BatchSize:       10,
		TextCharLimit:   8000,
		AttemptLimit:    5,
		RetryBaseDelay:  time.Minute,
		PendingDeferral: 10 * time.Minute,
		DailyRequestCap: 100,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	logger := slog.New(classificationLogHandler{capture: f.logs})
	return classification.NewWorker(f.repo, client, policy, cfg, f.clock.Now, logger, opts...)
}

// run sets the clock to at and runs the worker once.
func (f *classificationFixture) run(t *testing.T, worker *classification.Worker, at time.Time) {
	t.Helper()
	f.clock.Set(at)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, worker.RunOnce(ctx))
}

// author creates a users row with handle <label>.test and DID did:plc:<label>.
func (f *classificationFixture) author(t *testing.T, label string) string {
	t.Helper()
	did := "did:plc:" + label
	createTestUser(t, f.db, label+".test", did)
	return did
}

// visiblePost seeds a postv2 post with an acceptance pinned to its CID.
func (f *classificationFixture) visiblePost(t *testing.T, communityDID, authorDID, rkey, title string) string {
	t.Helper()
	uri := seedVisibilityPost(t, f.db, communityDID, authorDID, rkey, title, classificationT0.Add(-time.Hour))
	seedVisibilityAdmission(t, f.db, communityDID, uri, posts.AdmissionStatusAccepted, "", "")
	return uri
}

// visiblePostWithEmbed seeds a postv2 post like visiblePost, carrying
// embedJSON as its embed and body as its content.
func (f *classificationFixture) visiblePostWithEmbed(t *testing.T, communityDID, authorDID, rkey, title, body, embedJSON string) string {
	t.Helper()
	uri := seedVisibilityPostWithEmbedJSON(t, f.db, communityDID, authorDID, rkey, title, embedJSON, classificationT0.Add(-time.Hour))
	if body != "" {
		_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET content = $2 WHERE uri = $1`, uri, body)
		require.NoError(t, err)
	}
	seedVisibilityAdmission(t, f.db, communityDID, uri, posts.AdmissionStatusAccepted, "", "")
	return uri
}

// enqueue inserts the post's job for its current CID through the indexing
// API, then makes it due at due.
func (f *classificationFixture) enqueue(t *testing.T, postURI string, due time.Time) {
	t.Helper()
	var cid, authorDID, communityDID string
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT cid, author_did, community_did FROM posts WHERE uri = $1`, postURI).Scan(&cid, &authorDID, &communityDID))
	f.enqueueJob(t, classification.SubjectKindPost, postURI, cid, authorDID, communityDID, due)
}

// enqueueComment inserts the comment's job for its current CID, with the
// commenter as author and its declared root post's community, then makes it
// due at due.
func (f *classificationFixture) enqueueComment(t *testing.T, commentURI string, due time.Time) {
	t.Helper()
	var cid, commenterDID, communityDID string
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT c.cid, c.commenter_did, COALESCE(p.community_did, '')
		FROM comments c LEFT JOIN posts p ON p.uri = c.root_uri
		WHERE c.uri = $1
	`, commentURI).Scan(&cid, &commenterDID, &communityDID))
	f.enqueueJob(t, classification.SubjectKindComment, commentURI, cid, commenterDID, communityDID, due)
}

// enqueueCommunity inserts the community's job for recordCID, whose subject,
// author and community are the community DID (Q5), then makes it due at due.
// recordCID is passed so a job can name a community that has no row.
func (f *classificationFixture) enqueueCommunity(t *testing.T, communityDID, recordCID string, due time.Time) {
	t.Helper()
	f.enqueueJob(t, classification.SubjectKindCommunity, communityDID, recordCID, communityDID, communityDID, due)
}

// enqueueJob inserts one job through the indexing API, then makes it due at
// due.
func (f *classificationFixture) enqueueJob(t *testing.T, kind classification.SubjectKind, subjectURI, cid, authorDID, communityDID string, due time.Time) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	err = f.repo.EnqueueInTx(ctx, tx, classification.NewJob{
		SubjectURI:   subjectURI,
		SubjectKind:  kind,
		RecordCID:    cid,
		AuthorDID:    authorDID,
		CommunityDID: communityDID,
		Evaluation: classification.Evaluation{
			PolicyVersion: "v1",
			TextModelID:   "@cf/cloudflare/clef-flash",
			ImageModelID:  "@cf/cloudflare/clef",
		},
	}, true)
	if err != nil {
		_ = tx.Rollback()
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	result, err := f.db.ExecContext(ctx,
		`UPDATE classification_jobs SET next_attempt_at = $3 WHERE subject_uri = $1 AND record_cid = $2`,
		subjectURI, cid, due)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "exactly one job is enqueued for %s", subjectURI)
}

type classificationJobRow struct {
	Status        string
	AttemptCount  int
	LastErrorCode sql.NullString
	CloseReason   sql.NullString
	NextAttemptAt time.Time
}

// job reads the single job of postURI.
func (f *classificationFixture) job(t *testing.T, postURI string) classificationJobRow {
	t.Helper()
	var row classificationJobRow
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT status, attempt_count, last_error_code, close_reason, next_attempt_at
		FROM classification_jobs WHERE subject_uri = $1
	`, postURI).Scan(&row.Status, &row.AttemptCount, &row.LastErrorCode, &row.CloseReason, &row.NextAttemptAt))
	row.NextAttemptAt = row.NextAttemptAt.UTC()
	return row
}

func (f *classificationFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(t.Context(), query, args...).Scan(&count))
	return count
}

// states decodes the state of every recorded request, in order.
func (f *classificationFixture) states(t *testing.T) []string {
	t.Helper()
	requests := f.transport.Requests()
	states := make([]string, 0, len(requests))
	for _, request := range requests {
		var body struct {
			State string `json:"state"`
		}
		require.NoError(t, json.Unmarshal(request.Body, &body))
		states = append(states, body.State)
	}
	return states
}

// requestsContaining counts recorded requests whose body contains fragment.
func (f *classificationFixture) requestsContaining(fragment string) int {
	count := 0
	for _, request := range f.transport.Requests() {
		if strings.Contains(string(request.Body), fragment) {
			count++
		}
	}
	return count
}

// TestClassificationWorkerClosesHiddenSubjects: a job whose subject is hidden
// from an anonymous viewer under the instance authority, or whose CID moved,
// is closed with no outbound request, and the checks run again before a retry
// (B6).
func TestClassificationWorkerClosesHiddenSubjects(t *testing.T) {
	const (
		controlTitle = "Control harbor seals"
		subjectTitle = "Hidden walrus colony"
		subjectRkey  = "3lclssubject01"
	)
	for _, row := range []struct {
		name string
		// subjectAdmission is the subject's admission status; accepted pins
		// its current CID.
		subjectAdmission posts.AdmissionStatus
		decisionCode     string
		// ownCommunity puts the subject in a second community.
		ownCommunity bool
		// hide makes the subject hidden: before the first run, or, for a
		// retry row, after the first request failed.
		hide            func(t *testing.T, f *classificationFixture, subjectURI, authorDID, communityDID string)
		retry           bool
		wantCloseReason string
	}{
		{
			name:             "soft-deleted",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, subjectURI, _, _ string) {
				_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subjectURI)
				require.NoError(t, err)
			},
			wantCloseReason: "not_visible",
		},
		{
			name:             "instance-removed",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, subjectURI, _, _ string) {
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, subjectURI, "social.coves.moderation.defs#reasonSpam")
			},
			wantCloseReason: "not_visible",
		},
		{
			name:             "instance removal for illegal content",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, subjectURI, _, _ string) {
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, subjectURI, "social.coves.moderation.defs#reasonIllegalContent")
			},
			wantCloseReason: "not_visible",
		},
		{
			name:             "community decision illegal-content",
			subjectAdmission: posts.AdmissionStatusRemoved,
			decisionCode:     "illegal-content",
			wantCloseReason:  "not_visible",
		},
		{
			name:             "community removal hide",
			subjectAdmission: posts.AdmissionStatusRemoved,
			wantCloseReason:  "not_visible",
		},
		{
			name:             "author account ban",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, _, authorDID, _ string) {
				seedAccountBan(t, f.db, classificationInstanceDID, authorDID)
			},
			wantCloseReason: "not_visible",
		},
		{
			name:             "author PDS account takendown",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, _, authorDID, _ string) {
				accountstatusfixture.Set(t, f.db, authorDID, accountstatus.Takendown)
			},
			wantCloseReason: "not_visible",
		},
		{
			name:             "community instance-removed",
			subjectAdmission: posts.AdmissionStatusAccepted,
			ownCommunity:     true,
			hide: func(t *testing.T, f *classificationFixture, _, _, communityDID string) {
				seedCommunityRemoval(t, f.db, classificationInstanceDID, communityDID)
			},
			wantCloseReason: "not_visible",
		},
		{
			name:             "admission rejected",
			subjectAdmission: posts.AdmissionStatusRejected,
			wantCloseReason:  "not_visible",
		},
		{
			name:             "instance-removed before the retry",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, subjectURI, _, _ string) {
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, subjectURI, "social.coves.moderation.defs#reasonSpam")
			},
			retry:           true,
			wantCloseReason: "not_visible",
		},
		{
			name:             "CID changed before the retry",
			subjectAdmission: posts.AdmissionStatusAccepted,
			hide: func(t *testing.T, f *classificationFixture, subjectURI, _, _ string) {
				_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET cid = 'bafyclseditedcid' WHERE uri = $1`, subjectURI)
				require.NoError(t, err)
			},
			retry:           true,
			wantCloseReason: "superseded",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The subject's requests fail, everything else is answered. With
			// one request in flight, the request being answered is the last
			// one recorded.
			var f *classificationFixture
			f = newClassificationFixture(t, func(req *http.Request) (*http.Response, error) {
				recorded := f.transport.Requests()
				if strings.Contains(string(recorded[len(recorded)-1].Body), "walrus") {
					return cloudflaretest.Response(req, http.StatusInternalServerError, classificationProviderFailure), nil
				}
				return cloudflaretest.Response(req, http.StatusOK, cloudflaretest.Envelope(classificationCanned, classificationCannedInputTokens)), nil
			})
			worker := f.worker(t, nil)

			controlAuthor := f.author(t, "clscontrol")
			controlURI := f.visiblePost(t, f.community, controlAuthor, "3lclscontrol01", controlTitle)

			subjectAuthor := f.author(t, "clssubject")
			subjectCommunity := f.community
			if row.ownCommunity {
				subjectCommunity = visibilityCommunity(t, f.db, "clsother")
			}
			subjectURI := seedVisibilityPost(t, f.db, subjectCommunity, subjectAuthor, subjectRkey, subjectTitle, classificationT0.Add(-time.Hour))
			seedVisibilityAdmission(t, f.db, subjectCommunity, subjectURI, row.subjectAdmission, "", row.decisionCode)

			f.enqueue(t, controlURI, classificationT0)
			f.enqueue(t, subjectURI, classificationT0)

			if row.retry {
				f.run(t, worker, classificationT0)
				failed := f.job(t, subjectURI)
				require.Equal(t, "retrying", failed.Status, "the subject's first request failed")
				require.Equal(t, 1, f.requestsContaining("walrus"))

				row.hide(t, f, subjectURI, subjectAuthor, subjectCommunity)
				f.run(t, worker, classificationT0.Add(time.Minute))

				closed := f.job(t, subjectURI)
				assert.Equal(t, "closed", closed.Status)
				assert.Equal(t, row.wantCloseReason, closed.CloseReason.String)
				assert.Equal(t, 1, closed.AttemptCount, "the closure is not an attempt")
				assert.Equal(t, 1, f.requestsContaining("walrus"), "no request is sent for the retry of a hidden or superseded subject")
				assert.Equal(t, "classified", f.job(t, controlURI).Status)
				return
			}

			if row.hide != nil {
				row.hide(t, f, subjectURI, subjectAuthor, subjectCommunity)
			}
			f.run(t, worker, classificationT0)

			closed := f.job(t, subjectURI)
			assert.Equal(t, "closed", closed.Status)
			assert.Equal(t, row.wantCloseReason, closed.CloseReason.String)
			assert.Equal(t, 0, closed.AttemptCount)
			assert.Equal(t, "classified", f.job(t, controlURI).Status, "the visible control in the same run is classified")
			assert.Equal(t, []string{"Control harbor seals"}, f.states(t), "only the control's text is sent")
		})
	}
}

// TestClassificationWorkerStoresResultWithoutContent: a truncated post's
// result row carries its identity, the model, the policy and the literal
// probabilities of the questions a text-only request is asked, holds no
// content, and the request carries no identity (B7).
func TestClassificationWorkerStoresResultWithoutContent(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	worker := f.worker(t, func(cfg *classification.WorkerConfig) { cfg.TextCharLimit = 20 })

	authorDID := f.author(t, "puffinwatcher")
	postURI := f.visiblePost(t, f.community, authorDID, "3lclstrunc0001", "Zanzibar lighthouse keepers")
	_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET content = 'Marmalade pelicans nest offshore' WHERE uri = $1`, postURI)
	require.NoError(t, err)
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)

	assert.Equal(t, []string{"Zanzibar lighthouse "}, f.states(t), "the first 20 runes are sent")

	var (
		subjectURI, subjectCID, subjectKind, communityDID, resultAuthor, modelID, policyVersion string
		probabilitiesJSON                                                                       []byte
		imagesSent, imagesSkipped                                                               int
		textTruncated                                                                           bool
	)
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT subject_uri, subject_cid, subject_kind, community_did, author_did, model_id, policy_version,
		       probabilities, images_sent, images_skipped, text_truncated
		FROM classification_results WHERE subject_uri = $1
	`, postURI).Scan(&subjectURI, &subjectCID, &subjectKind, &communityDID, &resultAuthor, &modelID, &policyVersion,
		&probabilitiesJSON, &imagesSent, &imagesSkipped, &textTruncated))
	assert.Equal(t, "at://did:plc:puffinwatcher/social.coves.community.postv2/3lclstrunc0001", subjectURI)
	assert.Equal(t, "bafypostv23lclstrunc0001", subjectCID)
	assert.Equal(t, "post", subjectKind)
	assert.Equal(t, "did:plc:visclscommunity", communityDID)
	assert.Equal(t, "did:plc:puffinwatcher", resultAuthor)
	assert.Equal(t, "@cf/cloudflare/clef-flash", modelID)
	assert.Equal(t, "v1", policyVersion)
	assert.Equal(t, 0, imagesSent)
	assert.Equal(t, 0, imagesSkipped)
	assert.True(t, textTruncated)
	var probabilities map[string]float64
	require.NoError(t, json.Unmarshal(probabilitiesJSON, &probabilities))
	assert.Equal(t, map[string]float64{
		"sexual_explicit":   0.01,
		"sexual_suggestive": 0.02,
		"graphic_violence":  0.04,
		"hate_harassment":   0.05,
		"spam_scam":         0.06,
	}, probabilities, "a text-only request is not asked sexual_minors")

	var stored string
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT row_to_json(r)::text FROM classification_results r WHERE subject_uri = $1`, postURI).Scan(&stored))
	for _, fragment := range []string{"Zanzibar lighthouse", "Zanzibar lighthouse keepers", "Marmalade pelicans nest offshore", "Marmalade"} {
		assert.NotContains(t, stored, fragment, "the stored result holds no content")
	}
	assert.Equal(t, 0, f.count(t, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'classification_results' AND column_name IN ('title', 'body', 'content', 'text', 'state')
	`))

	requests := f.transport.Requests()
	require.Len(t, requests, 1)
	for _, request := range requests {
		var sent strings.Builder
		sent.WriteString(request.URL)
		for name, values := range request.Header {
			sent.WriteString(name + ": " + strings.Join(values, ",") + "\n")
		}
		sent.Write(request.Body)
		for _, identity := range []string{
			"did:plc:puffinwatcher",
			"did:plc:visclscommunity",
			"at://did:plc:puffinwatcher/social.coves.community.postv2/3lclstrunc0001",
			"puffinwatcher.test",
		} {
			assert.NotContains(t, sent.String(), identity, "the request carries no identity")
		}
	}
}

// TestClassificationWorkerRetriesWithBackoffUntilTheAttemptLimit (B8).
func TestClassificationWorkerRetriesWithBackoffUntilTheAttemptLimit(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Status(http.StatusInternalServerError, classificationProviderFailure))
	worker := f.worker(t, func(cfg *classification.WorkerConfig) {
		cfg.AttemptLimit = 3
		cfg.RetryBaseDelay = time.Minute
	})
	authorDID := f.author(t, "clsretry")
	postURI := f.visiblePost(t, f.community, authorDID, "3lclsretry0001", "Retry otter raft")
	f.enqueue(t, postURI, classificationT0)

	f.run(t, worker, classificationT0)
	job := f.job(t, postURI)
	assert.Equal(t, "retrying", job.Status)
	assert.Equal(t, 1, job.AttemptCount)
	assert.Equal(t, "provider_error", job.LastErrorCode.String)
	assert.Equal(t, time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC), job.NextAttemptAt)

	f.run(t, worker, time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC))
	job = f.job(t, postURI)
	assert.Equal(t, "retrying", job.Status)
	assert.Equal(t, 2, job.AttemptCount)
	assert.Equal(t, "provider_error", job.LastErrorCode.String)
	assert.Equal(t, time.Date(2026, 11, 12, 12, 3, 0, 0, time.UTC), job.NextAttemptAt)

	f.run(t, worker, time.Date(2026, 11, 12, 12, 3, 0, 0, time.UTC))
	job = f.job(t, postURI)
	assert.Equal(t, "failed", job.Status)
	assert.Equal(t, 3, job.AttemptCount)
	assert.Equal(t, "provider_error", job.LastErrorCode.String)
	require.Len(t, f.transport.Requests(), 3)

	f.run(t, worker, time.Date(2026, 11, 12, 13, 3, 0, 0, time.UTC))
	assert.Len(t, f.transport.Requests(), 3, "a failed job is not requested again")
	assert.Equal(t, "failed", f.job(t, postURI).Status)
}

// TestClassificationWorkerBacksOffDeferralUntilRearmed: each deferral of a
// job awaiting admission doubles its wait from CLASSIFIER_PENDING_DEFERRAL
// and costs no attempt; accepting the post re-arms the job and resets the
// backoff, so the next deferral waits the base delay again (R2).
func TestClassificationWorkerBacksOffDeferralUntilRearmed(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	worker := f.worker(t, nil)
	authorDID := f.author(t, "clsdeferral")
	postURI := seedVisibilityPost(t, f.db, f.community, authorDID, "3lclsdeferral1", "Pending tide pool", classificationT0.Add(-time.Hour))
	f.enqueue(t, postURI, classificationT0)
	deferralCount := func() int {
		t.Helper()
		return f.count(t, `SELECT deferral_count FROM classification_jobs WHERE subject_uri = $1`, postURI)
	}

	runAt := classificationT0
	for n, wait := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute} {
		f.run(t, worker, runAt)
		job := f.job(t, postURI)
		assert.Equal(t, "deferred", job.Status, "deferral %d", n+1)
		assert.Equal(t, 0, job.AttemptCount, "a deferral costs no attempt")
		assert.Equal(t, runAt.Add(wait), job.NextAttemptAt, "deferral %d waits %s", n+1, wait)
		assert.Equal(t, n+1, deferralCount())
		runAt = job.NextAttemptAt
	}
	assert.Empty(t, f.transport.Requests(), "a deferred job makes no request")

	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	if err := rearmClassificationJobs(t.Context(), tx, &posts.Admission{
		CommunityDID: f.community, PostURI: postURI, Status: posts.AdmissionStatusAccepted,
	}); err != nil {
		_ = tx.Rollback()
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	assert.Equal(t, 0, deferralCount(), "re-arming resets the deferral backoff")

	// Re-arming makes the job due at the database's NOW(), so the worker runs
	// after it.
	runAt = time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	f.run(t, worker, runAt)
	job := f.job(t, postURI)
	assert.Equal(t, "deferred", job.Status, "the post still awaits admission")
	assert.Equal(t, runAt.Add(10*time.Minute), job.NextAttemptAt, "the next deferral waits the base delay")
	assert.Equal(t, 1, deferralCount())
}

// TestClassificationAuthorEventConvergingOnAcceptanceRearmsJobs: an
// acceptance that arrives before the author event carrying the content it
// pins leaves the post pending_reacceptance; the author event that converges
// it to accepted re-arms the post's comments' jobs and resets their deferral
// backoff, as an acceptance landing directly does (R2).
func TestClassificationAuthorEventConvergingOnAcceptanceRearmsJobs(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	worker := f.worker(t, nil)
	repo := NewAdmissionRepository(f.db)
	ctx := t.Context()
	authorDID := f.author(t, "clsconverge")
	postURI := seedVisibilityPost(t, f.db, f.community, authorDID, "3lclsconverge1", "Converging tide pool", classificationT0.Add(-time.Hour))
	commentURI := seedClassificationComment(t, f.db, authorDID, "3lclsconvcmt1", postURI, postURI, "Converging comment")

	_, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: f.community, PostURI: postURI, EvaluatedCID: "bafypostv23lclsconverge1",
	})
	require.NoError(t, err)

	f.enqueueComment(t, commentURI, classificationT0)
	f.run(t, worker, classificationT0)
	f.run(t, worker, classificationT0.Add(10*time.Minute))
	deferralCount := func() int {
		t.Helper()
		return f.count(t, `SELECT deferral_count FROM classification_jobs WHERE subject_uri = $1`, commentURI)
	}
	require.Equal(t, 2, deferralCount(), "the comment job is backing off while its post awaits admission")

	acceptanceURI, acceptanceRkey := acceptanceRecord(t, f.community)
	accepted, err := repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID: f.community, PostURI: postURI,
		AcceptanceURI: acceptanceURI, AcceptanceRkey: acceptanceRkey, PinnedCID: "bafyclsconvergeedit",
		Watermark: posts.CommunityWatermark{Rev: "3lclsconvrev01", OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusPendingReacceptance, accepted.Admission.Status,
		"the acceptance pins content the AppView has not seen yet")
	require.Equal(t, 2, deferralCount(), "an acceptance that does not admit the post does not re-arm")

	converged, err := repo.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: f.community, PostURI: postURI, EvaluatedCID: "bafyclsconvergeedit",
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusAccepted, converged.Admission.Status)

	assert.Equal(t, 0, deferralCount(), "converging on the acceptance resets the deferral backoff")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_jobs
		WHERE subject_uri = $1 AND next_attempt_at BETWEEN NOW() - INTERVAL '1 minute' AND NOW()`, commentURI),
		"converging on the acceptance makes the comment job due now")
}

// TestClassificationWorkerHonoursTheDailyRequestCap (B9).
func TestClassificationWorkerHonoursTheDailyRequestCap(t *testing.T) {
	var (
		mu       sync.Mutex
		answered int
	)
	f := newClassificationFixture(t, func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		answered++
		first := answered == 1
		mu.Unlock()
		if first {
			return cloudflaretest.Response(req, http.StatusInternalServerError, classificationProviderFailure), nil
		}
		return cloudflaretest.Response(req, http.StatusOK, cloudflaretest.Envelope(classificationCanned, classificationCannedInputTokens)), nil
	})
	worker := f.worker(t, func(cfg *classification.WorkerConfig) { cfg.DailyRequestCap = 2 })
	authorDID := f.author(t, "clscap")
	uris := []string{
		f.visiblePost(t, f.community, authorDID, "3lclscap00001", "Cap heron one"),
		f.visiblePost(t, f.community, authorDID, "3lclscap00002", "Cap heron two"),
		f.visiblePost(t, f.community, authorDID, "3lclscap00003", "Cap heron three"),
	}
	for _, uri := range uris {
		f.enqueue(t, uri, classificationT0)
	}

	f.run(t, worker, classificationT0)
	assert.Len(t, f.transport.Requests(), 2, "the failed request counts against the cap")

	statuses := map[string][]string{}
	for _, uri := range uris {
		job := f.job(t, uri)
		statuses[job.Status] = append(statuses[job.Status], uri)
	}
	require.Len(t, statuses["classified"], 1)
	require.Len(t, statuses["retrying"], 1)
	require.Len(t, statuses["queued"], 1, "the job claimed after the cap is released")
	unsent := statuses["queued"][0]
	held := f.job(t, unsent)
	assert.Equal(t, 0, held.AttemptCount)
	assert.Equal(t, classificationT0, held.NextAttemptAt)

	var requests, inputTokens int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT requests, input_tokens FROM classification_usage
		WHERE model_id = '@cf/cloudflare/clef-flash' AND usage_date = '2026-11-12'
	`).Scan(&requests, &inputTokens))
	assert.Equal(t, 2, requests)
	assert.Equal(t, 321, inputTokens, "a failed request adds no tokens")

	f.run(t, worker, time.Date(2026, 11, 12, 12, 2, 0, 0, time.UTC))
	assert.Len(t, f.transport.Requests(), 2, "a second run the same day makes no request")
	assert.Equal(t, 1, f.logs.countCode("daily_cap_reached"), "the cap is logged once per UTC day")

	f.run(t, worker, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	assert.Equal(t, "classified", f.job(t, unsent).Status, "the next UTC day's budget classifies the held job")
}

// TestClassificationAccountErasure: erasing an author removes their jobs and
// results, including a result whose request was in flight; a hidden post
// keeps its result (B10).
func TestClassificationAccountErasure(t *testing.T) {
	const (
		erasedLabel = "clserased"
		erasedDID   = "did:plc:clserased"
		keptDID     = "did:plc:clskept"
	)
	authorRows := func(f *classificationFixture, t *testing.T, did string) (jobs, results int) {
		jobs = f.count(t, `SELECT count(*) FROM classification_jobs WHERE author_did = $1`, did)
		results = f.count(t, `SELECT count(*) FROM classification_results WHERE author_did = $1`, did)
		return jobs, results
	}
	deleteAccount := func(t *testing.T, db *sql.DB, did string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return NewUserRepository(db).Delete(ctx, did)
	}
	for _, row := range []struct {
		name string
		run  func(t *testing.T, f *classificationFixture, worker *classification.Worker)
	}{
		{
			name: "queued job erased before the run",
			run: func(t *testing.T, f *classificationFixture, worker *classification.Worker) {
				f.enqueue(t, f.visiblePost(t, f.community, f.author(t, erasedLabel), "3lclserase001", "Erased gannet"), classificationT0)

				require.NoError(t, deleteAccount(t, f.db, erasedDID))
				f.run(t, worker, classificationT0)

				jobs, results := authorRows(f, t, erasedDID)
				assert.Equal(t, 0, jobs)
				assert.Equal(t, 0, results)
				assert.Empty(t, f.transport.Requests())
			},
		},
		{
			name: "stored result erased",
			run: func(t *testing.T, f *classificationFixture, worker *classification.Worker) {
				f.enqueue(t, f.visiblePost(t, f.community, f.author(t, erasedLabel), "3lclserase001", "Erased gannet"), classificationT0)
				f.enqueue(t, f.visiblePost(t, f.community, f.author(t, "clskept"), "3lclskept0001", "Kept cormorant"), classificationT0)
				f.run(t, worker, classificationT0)
				_, results := authorRows(f, t, erasedDID)
				require.Equal(t, 1, results)

				require.NoError(t, deleteAccount(t, f.db, erasedDID))

				jobs, results := authorRows(f, t, erasedDID)
				assert.Equal(t, 0, jobs)
				assert.Equal(t, 0, results)
				_, keptResults := authorRows(f, t, keptDID)
				assert.Equal(t, 1, keptResults, "another author's result remains")
			},
		},
		{
			name: "erased during the in-flight request",
			run: func(t *testing.T, f *classificationFixture, worker *classification.Worker) {
				f.enqueue(t, f.visiblePost(t, f.community, f.author(t, erasedLabel), "3lclserase001", "Erased gannet"), classificationT0)
				var (
					once      sync.Once
					deleteErr error
				)
				f.transport.SetBeforeAnswer(func(*http.Request) {
					once.Do(func() { deleteErr = deleteAccount(t, f.db, erasedDID) })
				})

				f.run(t, worker, classificationT0)

				require.NoError(t, deleteErr)
				require.Len(t, f.transport.Requests(), 1)
				jobs, results := authorRows(f, t, erasedDID)
				assert.Equal(t, 0, jobs)
				assert.Equal(t, 0, results, "a result whose job was erased is not stored")
				requests, inputTokens := f.usage(t, "@cf/cloudflare/clef-flash", "2026-11-12")
				assert.Equal(t, 1, requests)
				assert.Equal(t, 321, inputTokens, "the answered request's tokens are counted although its job was erased")
			},
		},
		{
			name: "hidden after classification keeps the result",
			run: func(t *testing.T, f *classificationFixture, worker *classification.Worker) {
				authorDID := f.author(t, "clshidden")
				deleted := f.visiblePost(t, f.community, authorDID, "3lclshide0001", "Deleted tern")
				removed := f.visiblePost(t, f.community, authorDID, "3lclshide0002", "Removed tern")
				f.enqueue(t, deleted, classificationT0)
				f.enqueue(t, removed, classificationT0)
				f.run(t, worker, classificationT0)

				_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deleted)
				require.NoError(t, err)
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, removed, "social.coves.moderation.defs#reasonSpam")
				f.run(t, worker, classificationT0.Add(time.Hour))

				assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, deleted))
				assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, removed))
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
			row.run(t, f, f.worker(t, nil))
		})
	}
}

// TestClassificationWorkersClaimADueJobOnce: two workers running
// concurrently over one due job send one request and store one result; the
// second worker, run while the first worker's request is in flight, sends
// nothing (C1).
func TestClassificationWorkersClaimADueJobOnce(t *testing.T) {
	f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	firstTransport := cloudflaretest.NewTransport(cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	secondTransport := cloudflaretest.NewTransport(cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
	first := f.workerOn(t, firstTransport, nil)
	second := f.workerOn(t, secondTransport, nil)

	authorDID := f.author(t, "clsconcurrent")
	postURI := f.visiblePost(t, f.community, authorDID, "3lclsconc0001", "Concurrent kittiwake ledge")
	f.enqueue(t, postURI, classificationT0)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	firstTransport.SetBeforeAnswer(func(*http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
	})

	f.clock.Set(classificationT0)
	firstDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		firstDone <- first.RunOnce(ctx)
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first worker's request did not arrive within 10s")
	}

	f.run(t, second, classificationT0)
	assert.Empty(t, secondTransport.Requests(), "the second worker sends no request while the first holds the job")

	releaseFirst()
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first worker did not finish within 10s of its release")
	}

	assert.Len(t, firstTransport.Requests(), 1)
	assert.Equal(t, 1, len(firstTransport.Requests())+len(secondTransport.Requests()), "one request across both workers")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, postURI))
	job := f.job(t, postURI)
	assert.Equal(t, "classified", job.Status)
	assert.Equal(t, 0, job.AttemptCount)
}
