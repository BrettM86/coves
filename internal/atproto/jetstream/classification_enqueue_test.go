//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"Coves/internal/classifier/cloudflare"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/classification"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Classification enqueue through the post consumer and the worker that drains
// the queue (docs/PRD_CONTENT_CLASSIFICATION.md R2-R4): a post indexed through
// the consumer gets a job in the indexing transaction, the worker defers it
// until the community admits the CID, and an edit queues the new CID while
// the old one is superseded.

const clsInstanceDID = "did:web:classification-jetstream.test"

// clsCanned is the canned provider answer, one literal per v1 policy key.
var clsCanned = map[string]float64{
	"sexual_explicit":   0.11,
	"sexual_suggestive": 0.12,
	"sexual_minors":     0.13,
	"graphic_violence":  0.14,
	"hate_harassment":   0.15,
	"spam_scam":         0.16,
}

// clsFixture is the acceptance fixture's consumer with the classification
// enqueuer installed, plus a worker over the same queue and the real Workers
// AI client on a recording in-process transport.
type clsFixture struct {
	accFixture
	// comments is a comment consumer on the same database with the comment
	// classification enqueuer installed.
	comments *CommentEventConsumer
	// communities is a gated community consumer on the same database whose
	// repository has the community classification option installed.
	communities *CommunityEventConsumer
	transport   *cloudflaretest.Transport
	worker      *classification.Worker
	// newWorker builds a worker like worker on another clock.
	newWorker func(now func() time.Time) *classification.Worker
	revs      []string
	nextRev   int
	timeUS    int64
}

func newClsFixture(t *testing.T) *clsFixture {
	t.Helper()
	return newClsFixtureWith(t, newAccFixture)
}

// newClsFixtureWithoutErasurePreCheck builds the fixture on a consumer with
// no deleted-accounts lookup, so an event from an erased author reaches the
// indexing transaction: the state a pre-check that lost a race with erasure
// leaves behind.
func newClsFixtureWithoutErasurePreCheck(t *testing.T) *clsFixture {
	t.Helper()
	return newClsFixtureWith(t, func(t *testing.T, db *sql.DB, opts ...PostEventConsumerOption) accFixture {
		insertBridgedUser(t, db, accAuthor, "accauthor.test")
		insertBridgedCommunity(t, db, accCommunity, "acccommunity.test", accAuthor)
		userService := newMockUserService()
		userService.users[accAuthor] = &users.User{DID: accAuthor, Handle: "accauthor.test"}
		admissions := postgres.NewAdmissionRepository(db)
		return accFixture{
			consumer: NewPostEventConsumer(
				postgres.NewPostRepository(db),
				postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
				userService,
				db,
				append([]PostEventConsumerOption{WithAdmissions(admissions)}, opts...)...,
			),
			admissions: admissions,
			db:         db,
		}
	})
}

func newClsFixtureWith(t *testing.T, newConsumerFixture func(*testing.T, *sql.DB, ...PostEventConsumerOption) accFixture) *clsFixture {
	t.Helper()
	db := testkit.DB(t)
	evaluation := classification.Evaluation{
		PolicyVersion: "v1",
		TextModelID:   "@cf/cloudflare/clef-flash",
		ImageModelID:  "@cf/cloudflare/clef",
	}
	repo := postgres.NewClassificationRepository(db, clsInstanceDID)
	acc := newConsumerFixture(t, db, WithClassificationEnqueuer(repo, evaluation))

	policy, err := classification.LoadPolicy()
	require.NoError(t, err)
	transport := cloudflaretest.NewTransport(cloudflaretest.Canned(clsCanned, 77))
	client := cloudflare.NewClient(cloudflare.Config{
		AccountID:   "acct-jetstream",
		APIToken:    "token-jetstream",
		TextModelID: "@cf/cloudflare/clef-flash",
	}, transport.Client())
	newWorker := func(now func() time.Time) *classification.Worker {
		return classification.NewWorker(repo, client, policy, classification.WorkerConfig{
			Evaluation:      evaluation,
			Concurrency:     1,
			BatchSize:       10,
			TextCharLimit:   8000,
			AttemptLimit:    5,
			RetryBaseDelay:  time.Minute,
			PendingDeferral: 10 * time.Minute,
			DailyRequestCap: 100,
		}, now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	return &clsFixture{
		accFixture: acc,
		comments:   newCommentConsumer(db, WithCommentClassificationEnqueuer(repo, evaluation)),
		communities: NewCommunityEventConsumer(
			postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithCommunityClassification(evaluation)),
			"did:web:test.local", true, nil, WithCommunityRevGate(NewRevGate(db))),
		transport: transport,
		worker:    newWorker(time.Now),
		newWorker: newWorker,
		revs:      increasingTIDs(t, 8),
		timeUS:    time.Now().UnixMicro(),
	}
}

// commit consumes an author-repo postv2 create or update for rkey at cid.
func (f *clsFixture) commit(t *testing.T, op, rkey, cid string, record map[string]interface{}) string {
	t.Helper()
	f.timeUS += 1_000_000
	rev := f.revs[f.nextRev]
	f.nextRev++
	require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(accAuthor, op, rkey, rev, cid, f.timeUS, record)))
	return accPostURI(rkey)
}

// accept consumes the community's acceptance of uri at cid (operation create
// for a first decision, update for a re-acceptance) and requires it applied.
func (f *clsFixture) accept(t *testing.T, operation, uri, cid string) {
	t.Helper()
	f.timeUS += 1_000_000
	require.NoError(t, f.consumer.HandleEvent(t.Context(),
		acceptanceEventAt(accCommunity, operation, posts.SubjectRkey(uri), uri, cid, testkit.TID(), f.timeUS)))
	admission, err := f.admissions.Get(t.Context(), accCommunity, uri)
	require.NoError(t, err)
	require.NotNil(t, admission)
	require.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "fixture: the acceptance of %s must apply", cid)
	assertNullableStringPV2(t, cid, admission.AcceptedCID, "fixture: the acceptance pins the CID")
}

func (f *clsFixture) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, f.worker.RunOnce(ctx))
}

type clsJob struct {
	Status        string
	AttemptCount  int
	CloseReason   sql.NullString
	NextAttemptAt time.Time
}

// job reads the single job of (uri, cid).
func (f *clsFixture) job(t *testing.T, uri, cid string) clsJob {
	t.Helper()
	var row clsJob
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT status, attempt_count, close_reason, next_attempt_at
		FROM classification_jobs WHERE subject_uri = $1 AND record_cid = $2
	`, uri, cid).Scan(&row.Status, &row.AttemptCount, &row.CloseReason, &row.NextAttemptAt), "job for %s at %s", uri, cid)
	return row
}

func (f *clsFixture) jobCount(t *testing.T, uri string) int {
	t.Helper()
	return countRows(t, f.db, `SELECT count(*) FROM classification_jobs WHERE subject_uri = $1`, uri)
}

func (f *clsFixture) usageRequests(t *testing.T) int {
	t.Helper()
	return countRows(t, f.db, `SELECT COALESCE(SUM(requests), 0) FROM classification_usage`)
}

// states decodes the state of every recorded request, in order.
func (f *clsFixture) states(t *testing.T) []string {
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

// assertNoIdentifiers checks that no recorded request's URL, headers or body
// names the author, the community, the post or the author's handle, nor any
// of the extra identifiers.
func (f *clsFixture) assertNoIdentifiers(t *testing.T, uri string, extra ...string) {
	t.Helper()
	identifiers := append([]string{accAuthor, accCommunity, uri, "accauthor.test", "acccommunity.test"}, extra...)
	for _, request := range f.transport.Requests() {
		headers, err := json.Marshal(request.Header)
		require.NoError(t, err)
		recorded := request.URL + "\n" + string(headers) + "\n" + string(request.Body)
		for _, identifier := range identifiers {
			assert.NotContains(t, recorded, identifier, "a classifier request must not carry %s", identifier)
		}
	}
}

func clsRecord(title, content string) map[string]interface{} {
	return pv2Record(accCommunity, title, content)
}

func clsExternalRecord(title, content, linkURI, linkTitle string) map[string]interface{} {
	record := clsRecord(title, content)
	record["embed"] = map[string]interface{}{
		"$type":    "social.coves.embed.external",
		"external": map[string]interface{}{"uri": linkURI, "title": linkTitle},
	}
	return record
}

func clsSelfLabelled(record map[string]interface{}) map[string]interface{} {
	record["labels"] = map[string]interface{}{
		"$type":  "com.atproto.label.defs#selfLabels",
		"values": []interface{}{map[string]interface{}{"val": "nsfw"}},
	}
	return record
}

// TestClassificationAcceptance_PostIsDeferredUntilAdmittedThenClassified is
// the chunk's acceptance test (B12).
func TestClassificationAcceptance_PostIsDeferredUntilAdmittedThenClassified(t *testing.T) {
	t.Parallel()

	t.Run("new post", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cid = "bafyclsacceptnew"
		uri := f.commit(t, "create", "clsacceptnew", cid, clsRecord("Otters at dusk", "A raft of otters drifted past the pier"))

		require.Equal(t, 1, f.jobCount(t, uri), "the create enqueues one job")
		assert.Equal(t, "queued", f.job(t, uri, cid).Status)

		runAt := time.Now()
		f.run(t)
		deferred := f.job(t, uri, cid)
		assert.Equal(t, "deferred", deferred.Status, "a post awaiting admission is deferred")
		assert.Equal(t, 0, deferred.AttemptCount)
		assert.False(t, deferred.NextAttemptAt.Before(runAt.Add(10*time.Minute-time.Second)),
			"next_attempt_at %s must be at least run time + PendingDeferral", deferred.NextAttemptAt)
		assert.Empty(t, f.transport.Requests(), "a deferred job makes no request")
		assert.Equal(t, 0, countRows(t, f.db, `SELECT count(*) FROM classification_usage`), "a deferral records no usage")

		f.accept(t, "create", uri, cid)
		f.run(t)
		assert.Equal(t, "classified", f.job(t, uri, cid).Status)
		assert.Equal(t, 1, countRows(t, f.db,
			`SELECT count(*) FROM classification_results WHERE subject_uri = $1 AND subject_cid = $2`, uri, cid))
		assert.Equal(t, []string{"Otters at dusk\n\nA raft of otters drifted past the pier"}, f.states(t))
		f.assertNoIdentifiers(t, uri)
	})

	t.Run("edit of an accepted post", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cidA, cidB = "bafyclsacceptedita", "bafyclsaccepteditb"
		uri := f.commit(t, "create", "clsacceptedit", cidA, clsRecord("Heron nest", "The heron returned to the old nest"))
		f.accept(t, "create", uri, cidA)
		f.run(t)
		require.Equal(t, "classified", f.job(t, uri, cidA).Status, "fixture: A is classified before the edit")

		f.commit(t, "update", "clsacceptedit", cidB, clsRecord("Heron nest", "Two heron chicks hatched this week"))
		admission, err := f.admissions.Get(t.Context(), accCommunity, uri)
		require.NoError(t, err)
		require.Equal(t, posts.AdmissionStatusPendingReacceptance, admission.Status, "fixture: the edit reopens the admission")

		f.run(t)
		edited := f.job(t, uri, cidB)
		assert.Equal(t, "deferred", edited.Status, "the edit awaiting re-acceptance is deferred")
		assert.Equal(t, 0, edited.AttemptCount)
		assert.Len(t, f.transport.Requests(), 1, "no request for the edit before re-acceptance")
		assert.Equal(t, 1, f.usageRequests(t), "usage unchanged by the deferral")

		f.accept(t, "update", uri, cidB)
		f.run(t)
		assert.Equal(t, "classified", f.job(t, uri, cidB).Status)
		assert.Equal(t, []string{
			"Heron nest\n\nThe heron returned to the old nest",
			"Heron nest\n\nTwo heron chicks hatched this week",
		}, f.states(t))
		f.assertNoIdentifiers(t, uri)
	})

	t.Run("edit inside the window before the admission reopens", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cidA, cidB = "bafyclswindowa", "bafyclswindowb"
		uri := f.commit(t, "create", "clswindow", cidA, clsRecord("Kelp forest", "The kelp grew a metre in a month"))
		f.accept(t, "create", uri, cidA)
		f.run(t)
		require.Equal(t, "classified", f.job(t, uri, cidA).Status, "fixture: A is classified before the edit")

		f.commit(t, "update", "clswindow", cidB, clsRecord("Kelp forest", "Urchins cleared the kelp overnight"))
		// The window between the content commit and UpsertPending: the
		// admission still reads accepted, pinned to A, while the post is at B.
		_, err := f.db.ExecContext(t.Context(),
			`UPDATE community_post_admissions SET status = 'accepted', accepted_cid = $2 WHERE post_uri = $1`, uri, cidA)
		require.NoError(t, err)

		f.run(t)
		edited := f.job(t, uri, cidB)
		assert.Equal(t, "deferred", edited.Status, "an acceptance pinned to another CID does not admit the edit")
		assert.Equal(t, 0, edited.AttemptCount)
		assert.Len(t, f.transport.Requests(), 1, "no request for the edit inside the window")
		assert.Equal(t, 1, f.usageRequests(t), "usage unchanged by the deferral")

		f.accept(t, "update", uri, cidB)
		f.run(t)
		assert.Equal(t, "classified", f.job(t, uri, cidB).Status)
		assert.Equal(t, []string{
			"Kelp forest\n\nThe kelp grew a metre in a month",
			"Kelp forest\n\nUrchins cleared the kelp overnight",
		}, f.states(t))
	})
}

// TestClassificationEnqueue_RedeliveryAndNoEnqueuer: a redelivered create
// enqueues once; a consumer without the enqueuer enqueues nothing (B13).
func TestClassificationEnqueue_RedeliveryAndNoEnqueuer(t *testing.T) {
	t.Parallel()

	t.Run("redelivered create", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		event := pv2Event(accAuthor, "create", "clsredeliver", testkit.TID(), "bafyclsredeliver", time.Now().UnixMicro(),
			clsRecord("Tide pools", "Anemones closed as the tide went out"))
		require.NoError(t, f.consumer.HandleEvent(t.Context(), event))
		require.NoError(t, f.consumer.HandleEvent(t.Context(), event))
		assert.Equal(t, 1, f.jobCount(t, accPostURI("clsredeliver")))
	})

	t.Run("no enqueuer installed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		f := newAccFixture(t, db)
		require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(
			accAuthor, "create", "clsnoenqueuer", testkit.TID(), "bafyclsnoenqueuer", time.Now().UnixMicro(),
			clsRecord("Sand dunes", "The dunes shifted after the storm"))))
		assert.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM classification_jobs`))
	})
}

// TestClassificationEnqueue_ErasedAuthor: a create or an update from an
// author with an erasure marker that reaches the indexing transaction enqueues
// no job, so the worker sends no request and stores no result (R5).
func TestClassificationEnqueue_ErasedAuthor(t *testing.T) {
	t.Parallel()
	markErased := func(t *testing.T, f *clsFixture) {
		t.Helper()
		_, err := f.db.ExecContext(t.Context(),
			`INSERT INTO deleted_accounts (did, deleted_at) VALUES ($1, NOW())`, accAuthor)
		require.NoError(t, err)
	}
	assertNothingClassified := func(t *testing.T, f *clsFixture) {
		t.Helper()
		f.run(t)
		assert.Equal(t, 0, len(f.transport.Requests()), "no request for an erased author")
		assert.Equal(t, 0, countRows(t, f.db,
			`SELECT count(*) FROM classification_results WHERE author_did = $1`, accAuthor), "no result for an erased author")
	}

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		f := newClsFixtureWithoutErasurePreCheck(t)
		markErased(t, f)
		uri := f.commit(t, "create", "clserasedcreate", "bafyclserasedcreate", clsRecord("Gull roost", "Gulls roosted on the breakwater"))

		require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
			"fixture: the create reaches the indexing transaction")
		assert.Equal(t, 0, f.jobCount(t, uri), "an erased author's create enqueues no job")
		assertNothingClassified(t, f)
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		f := newClsFixtureWithoutErasurePreCheck(t)
		uri := f.commit(t, "create", "clserasedupdate", "bafyclserasedupdatea", clsRecord("Seal haul-out", "Seals hauled out at low tide"))
		require.Equal(t, 1, f.jobCount(t, uri), "fixture: the create before erasure enqueues")
		// Erasure removes the author's jobs and writes the marker; the post row
		// stays so the late update reaches the update transaction.
		_, err := f.db.ExecContext(t.Context(), `DELETE FROM classification_jobs WHERE author_did = $1`, accAuthor)
		require.NoError(t, err)
		markErased(t, f)

		const cidB = "bafyclserasedupdateb"
		f.commit(t, "update", "clserasedupdate", cidB, clsRecord("Seal haul-out", "Pups joined the seals at low tide"))

		var storedCID string
		require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT cid FROM posts WHERE uri = $1`, uri).Scan(&storedCID))
		require.Equal(t, cidB, storedCID, "fixture: the update reaches the update transaction")
		assert.Equal(t, 0, f.jobCount(t, uri), "an erased author's update enqueues no job")
		assertNothingClassified(t, f)
	})
}

// TestClassificationEnqueue_EditAfterQueue: an edit queues its CID and
// supersedes the earlier one; a title-only or external-link-only edit of a
// classified post queues the new CID (B14).
func TestClassificationEnqueue_EditAfterQueue(t *testing.T) {
	t.Parallel()

	t.Run("body edit before classification", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cidA, cidB = "bafyclsbodyedita", "bafyclsbodyeditb"
		uri := f.commit(t, "create", "clsbodyedit", cidA, clsRecord("Gull colony", "Original gull census text"))
		f.commit(t, "update", "clsbodyedit", cidB, clsRecord("Gull colony", "Corrected gull census text"))
		f.accept(t, "create", uri, cidB)
		f.run(t)

		superseded := f.job(t, uri, cidA)
		assert.Equal(t, "closed", superseded.Status)
		assert.Equal(t, sql.NullString{String: "superseded", Valid: true}, superseded.CloseReason)
		assert.Equal(t, "classified", f.job(t, uri, cidB).Status)
		assert.Equal(t, []string{"Gull colony\n\nCorrected gull census text"}, f.states(t),
			"the only request carries the edited body")
	})

	for _, row := range []struct {
		name    string
		rkey    string
		created map[string]interface{}
		edited  map[string]interface{}
	}{
		{
			name:    "title-only edit",
			rkey:    "clstitleedit",
			created: clsRecord("Puffin burrows", "Burrows dot the cliff top"),
			edited:  clsRecord("Puffin burrows counted", "Burrows dot the cliff top"),
		},
		{
			name:    "external-link-only edit",
			rkey:    "clslinkedit",
			created: clsExternalRecord("Seal pups", "Pups on the sandbank", "https://example.com/seals-2025", "Seal survey"),
			edited:  clsExternalRecord("Seal pups", "Pups on the sandbank", "https://example.com/seals-2026", "Seal survey"),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newClsFixture(t)
			cidA, cidB := "bafy"+row.rkey+"a", "bafy"+row.rkey+"b"
			uri := f.commit(t, "create", row.rkey, cidA, row.created)
			f.accept(t, "create", uri, cidA)
			f.run(t)
			require.Equal(t, "classified", f.job(t, uri, cidA).Status, "fixture: the predecessor is classified")

			f.commit(t, "update", row.rkey, cidB, row.edited)
			assert.Equal(t, 1, countRows(t, f.db,
				`SELECT count(*) FROM classification_jobs WHERE subject_uri = $1 AND record_cid = $2`, uri, cidB),
				"a classified-field edit queues the new CID")
		})
	}
}

// TestClassificationEnqueue_SelfLabelOnlyEdit: a self-label-only edit queues
// the new CID only when the post still has a non-terminal job (B15).
func TestClassificationEnqueue_SelfLabelOnlyEdit(t *testing.T) {
	t.Parallel()

	t.Run("while the job is queued", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cidA, cidB = "bafyclslabelqueueda", "bafyclslabelqueuedb"
		uri := f.commit(t, "create", "clslabelqueued", cidA, clsRecord("Crab moult", "Shore crabs shed their shells"))
		f.commit(t, "update", "clslabelqueued", cidB, clsSelfLabelled(clsRecord("Crab moult", "Shore crabs shed their shells")))
		f.accept(t, "create", uri, cidB)
		f.run(t)

		assert.Equal(t, "classified", f.job(t, uri, cidB).Status)
		superseded := f.job(t, uri, cidA)
		assert.Equal(t, "closed", superseded.Status)
		assert.Equal(t, sql.NullString{String: "superseded", Valid: true}, superseded.CloseReason)
	})

	t.Run("while the job is deferred", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cidA, cidB = "bafyclslabeldeferreda", "bafyclslabeldeferredb"
		uri := f.commit(t, "create", "clslabeldeferred", cidA, clsRecord("Starfish", "Starfish regrew an arm"))
		f.run(t)
		require.Equal(t, "deferred", f.job(t, uri, cidA).Status, "fixture: A is deferred before the self-label edit")

		f.commit(t, "update", "clslabeldeferred", cidB, clsSelfLabelled(clsRecord("Starfish", "Starfish regrew an arm")))
		assert.Equal(t, 1, countRows(t, f.db,
			`SELECT count(*) FROM classification_jobs WHERE subject_uri = $1 AND record_cid = $2`, uri, cidB),
			"a deferred predecessor carries the queue to the new CID")

		f.accept(t, "create", uri, cidB)
		f.run(t)
		superseded := f.job(t, uri, cidA)
		assert.Equal(t, "closed", superseded.Status)
		assert.Equal(t, sql.NullString{String: "superseded", Valid: true}, superseded.CloseReason)
		assert.Equal(t, "classified", f.job(t, uri, cidB).Status)
	})

	t.Run("after classification", func(t *testing.T) {
		t.Parallel()
		f := newClsFixture(t)
		const cidA, cidB = "bafyclslabeldonea", "bafyclslabeldoneb"
		uri := f.commit(t, "create", "clslabeldone", cidA, clsRecord("Barnacles", "Barnacles crowd the hull"))
		f.accept(t, "create", uri, cidA)
		f.run(t)
		require.Equal(t, "classified", f.job(t, uri, cidA).Status, "fixture: A is classified before the self-label edit")

		f.commit(t, "update", "clslabeldone", cidB, clsSelfLabelled(clsRecord("Barnacles", "Barnacles crowd the hull")))
		assert.Equal(t, 1, f.jobCount(t, uri), "a self-label edit of a classified post queues nothing")
		assert.Equal(t, 0, countRows(t, f.db,
			`SELECT count(*) FROM classification_jobs WHERE subject_uri = $1 AND record_cid = $2`, uri, cidB))
	})
}
