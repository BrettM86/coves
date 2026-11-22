//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/classification"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin classification operations over Postgres
// (docs/PRD_CONTENT_CLASSIFICATION.md R6 bullets 4-5): the per-community
// summary and re-enqueueing existing content through the real services and
// repositories.

// reenqueueEvaluation is the evaluation f.enqueueJob queues under, so a job
// the fixture seeds is the identical job a re-enqueue would create.
var reenqueueEvaluation = classification.Evaluation{
	PolicyVersion: "v1",
	TextModelID:   "@cf/cloudflare/clef-flash",
	ImageModelID:  "@cf/cloudflare/clef",
}

// setClassificationIndexedAt sets a post's or comment's indexed_at, the
// re-enqueue range column.
func setClassificationIndexedAt(t *testing.T, db *sql.DB, kind classification.SubjectKind, uri string, at time.Time) {
	t.Helper()
	query := map[classification.SubjectKind]string{
		classification.SubjectKindPost:    `UPDATE posts SET indexed_at = $2 WHERE uri = $1`,
		classification.SubjectKindComment: `UPDATE comments SET indexed_at = $2 WHERE uri = $1`,
	}[kind]
	result, err := db.ExecContext(context.Background(), query, uri, at)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "setting indexed_at of %s", uri)
}

// seedFlagEvent inserts one community_nsfw_flag_events row; delivered sets
// delivered_at to occurredAt.
func seedFlagEvent(t *testing.T, db *sql.DB, communityDID string, oldNSFW, newNSFW bool, occurredAt time.Time, delivered bool) {
	t.Helper()
	var deliveredAt sql.NullTime
	if delivered {
		deliveredAt = sql.NullTime{Time: occurredAt, Valid: true}
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO community_nsfw_flag_events (community_did, origin_host, old_nsfw, new_nsfw, occurred_at, delivered_at)
		VALUES ($1, NULL, $2, $3, $4, $5)
	`, communityDID, oldNSFW, newNSFW, occurredAt, deliveredAt)
	require.NoErrorf(t, err, "seeding a flag event of %s", communityDID)
}

// reenqueueJobRow is one stored classification_jobs row's identity, owner
// fields and status.
type reenqueueJobRow struct {
	SubjectURI    string
	SubjectKind   string
	RecordCID     string
	PolicyVersion string
	TextModelID   string
	ImageModelID  string
	AuthorDID     string
	CommunityDID  string
	Status        string
}

// reenqueueJobs reads every stored job.
func reenqueueJobs(t *testing.T, db *sql.DB) []reenqueueJobRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT subject_uri, subject_kind, record_cid, policy_version, text_model_id, image_model_id,
		       author_did, community_did, status
		FROM classification_jobs
		ORDER BY subject_uri, record_cid, policy_version
	`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var jobs []reenqueueJobRow
	for rows.Next() {
		var row reenqueueJobRow
		require.NoError(t, rows.Scan(&row.SubjectURI, &row.SubjectKind, &row.RecordCID, &row.PolicyVersion,
			&row.TextModelID, &row.ImageModelID, &row.AuthorDID, &row.CommunityDID, &row.Status))
		jobs = append(jobs, row)
	}
	require.NoError(t, rows.Err())
	return jobs
}

// queuedJob is the job a re-enqueue under reenqueueEvaluation creates.
func queuedJob(kind classification.SubjectKind, uri, cid, authorDID, communityDID string) reenqueueJobRow {
	return reenqueueJobRow{
		SubjectURI:    uri,
		SubjectKind:   string(kind),
		RecordCID:     cid,
		PolicyVersion: "v1",
		TextModelID:   "@cf/cloudflare/clef-flash",
		ImageModelID:  "@cf/cloudflare/clef",
		AuthorDID:     authorDID,
		CommunityDID:  communityDID,
		Status:        "queued",
	}
}

func newReenqueueService(db *sql.DB, evaluation classification.Evaluation) *classification.ReenqueueService {
	return classification.NewReenqueueService(NewClassificationReenqueueRepository(db, 2), &evaluation)
}

// TestClassificationSummaryWindowFilteringAndEvents: the summary counts one
// community's posts and comments scored in [now-168h, now), each by its most
// recent result in the window that answers the question, counts a
// probability equal to the threshold as above, excludes community-kind
// results and other communities, and lists the window's flag events newest
// first (B6).
func TestClassificationSummaryWindowFilteringAndEvents(t *testing.T) {
	db := testkit.DB(t)
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)
	clock := &classificationClock{now: classificationT0}
	service := classification.NewSummaryService(NewClassificationSummaryRepository(db), policy, clock.Now)

	communityA := visibilityCommunity(t, db, "clssuma")
	communityB := visibilityCommunity(t, db, "clssumb")
	authorDID := "did:plc:clssumauthor"
	createTestUser(t, db, "clssumauthor.test", authorDID)
	p1 := seedReviewPost(t, db, communityA, authorDID, "3lclssump001", "Summary heron one")
	p2 := seedReviewPost(t, db, communityA, authorDID, "3lclssump002", "Summary heron two")
	p3 := seedReviewPost(t, db, communityA, authorDID, "3lclssump003", "Summary heron three")
	p4 := seedReviewPost(t, db, communityA, authorDID, "3lclssump004", "Summary heron four")
	p5 := seedReviewPost(t, db, communityA, authorDID, "3lclssump005", "Summary heron five")
	p6 := seedReviewPost(t, db, communityA, authorDID, "3lclssump006", "Summary heron six")
	p7 := seedReviewPost(t, db, communityA, authorDID, "3lclssump007", "Summary heron seven")
	c1 := seedReviewComment(t, db, p1, authorDID, "3lclssumc001", "Summary reply one")
	c2 := seedReviewComment(t, db, p1, authorDID, "3lclssumc002", "Summary reply two")
	c3 := seedReviewComment(t, db, p2, authorDID, "3lclssumc003", "Summary reply three")
	c4 := seedReviewComment(t, db, p2, authorDID, "3lclssumc004", "Summary reply four")
	postB := seedReviewPost(t, db, communityB, authorDID, "3lclssumpb01", "Summary egret")
	commentB := seedReviewComment(t, db, postB, authorDID, "3lclssumcb01", "Summary egret reply")
	communityKind := reviewSubject{
		kind: classification.SubjectKindCommunity, uri: communityA, cid: "bafyclssumacommunity",
		authorDID: communityA, communityDID: communityA,
	}

	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	above := map[string]float64{"spam_scam": 0.9}
	below := map[string]float64{"spam_scam": 0.1}
	for _, seed := range []classificationResultSeed{
		p1.result("p1", above, now.Add(-2*day)),
		p2.result("p2", below, now.Add(-3*day)),
		p3.result("p3", above, now.Add(-8*day)),
		p4.result("p4old", above, now.Add(-5*day)),
		p4.result("p4new", below, now.Add(-4*day)),
		p5.result("p5", below, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)),
		p6.result("p6", above, now),
		// p7's latest result, under an older policy, lacks the question; its
		// earlier result answers it.
		p7.result("p7answer", above, now.Add(-6*day)),
		p7.result("p7nokey", map[string]float64{"hate_harassment": 0.2}, now.Add(-day)),
		c1.result("c1", map[string]float64{"spam_scam": 0.8}, now.Add(-day)),
		c2.result("c2", map[string]float64{"hate_harassment": 0.9}, now.Add(-day)),
		c3.result("c3", above, now.Add(-9*day)),
		c4.result("c4", map[string]float64{"spam_scam": 0.5}, now.Add(-day)),
		communityKind.result("community", above, now.Add(-day)),
		postB.result("pb", above, now.Add(-day)),
		commentB.result("cb", above, now.Add(-day)),
	} {
		seedClassificationResult(t, db, seed)
	}
	seedFlagEvent(t, db, communityA, false, true, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), true)
	seedFlagEvent(t, db, communityA, true, false, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), false)
	seedFlagEvent(t, db, communityA, false, true, time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), true)
	seedFlagEvent(t, db, communityB, false, true, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), true)

	summary, err := service.Summary(context.Background(), classification.SummaryRequest{
		CommunityDID: communityA, Question: "spam_scam", MinProbability: 0.5,
	})
	require.NoError(t, err)

	assert.Equal(t, communityA, summary.CommunityDID)
	assert.Equal(t, "spam_scam", summary.Question)
	assert.Equal(t, 0.5, summary.MinProbability)
	assert.Equal(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), summary.Since.UTC())
	assert.Equal(t, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), summary.Until.UTC())
	assert.Equal(t, classification.KindSummary{Scored: 5, Above: 2}, summary.Posts,
		"p1, p2, p4, p5 and p7 are scored; p1 and p7 (by its earlier result answering the question) are above")
	assert.Equal(t, classification.KindSummary{Scored: 2, Above: 2}, summary.Comments,
		"c1 and c4 are scored and above, c4 at exactly the threshold; c2's only result lacks the question")
	share, defined := summary.Share()
	assert.True(t, defined, "share is defined when something was scored")
	assert.InDelta(t, 4.0/7.0, share, 1e-9)
	events := make([]classification.FlagEvent, 0, len(summary.FlagEvents))
	for _, event := range summary.FlagEvents {
		event.OccurredAt = event.OccurredAt.UTC()
		events = append(events, event)
	}
	assert.Equal(t, []classification.FlagEvent{
		{OldNSFW: true, NewNSFW: false, OccurredAt: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), Delivered: false},
		{OldNSFW: false, NewNSFW: true, OccurredAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), Delivered: true},
	}, events)

	t.Run("unknown community", func(t *testing.T) {
		unknownDID := "did:plc:clssumunknowncommunity"
		_, err := service.Summary(context.Background(), classification.SummaryRequest{
			CommunityDID: unknownDID, Question: "spam_scam", MinProbability: 0.5,
		})
		require.ErrorIs(t, err, classification.ErrCommunityNotFound)
		assert.NotContains(t, err.Error(), unknownDID, "a logged summary error never carries the community DID")
	})
}

// TestClassificationReenqueuePostsAndCommentsRange: re-enqueueing a range
// gives every undeleted post and comment indexed in [since, until) exactly
// one job at its current CID under the evaluation, with the consumers' author
// and community fields, across timestamp ties at a batch boundary; jobs that
// already exist count as present, and a second run creates nothing (B7).
func TestClassificationReenqueuePostsAndCommentsRange(t *testing.T) {
	f := newClassificationFixture(t, nil)
	db := f.db
	authorDID := f.author(t, "clsrqauthor")
	commenterDID := f.author(t, "clsrqcommenter")
	community := f.community

	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	post := func(rkey string, at time.Time) reviewSubject {
		subject := seedReviewPost(t, db, community, authorDID, rkey, "Re-enqueue "+rkey)
		setClassificationIndexedAt(t, db, classification.SubjectKindPost, subject.uri, at)
		return subject
	}
	comment := func(rkey, rootURI, parentURI string, at time.Time) reviewSubject {
		uri := seedClassificationComment(t, db, commenterDID, rkey, rootURI, parentURI, "Re-enqueue "+rkey)
		setClassificationIndexedAt(t, db, classification.SubjectKindComment, uri, at)
		return reviewSubject{kind: classification.SubjectKindComment, uri: uri, cid: "bafycmt" + rkey, authorDID: commenterDID}
	}

	postBefore := post("3lclsrqp0bef", time.Date(2026, 10, 31, 23, 59, 59, 999999000, time.UTC))
	postAtSince := post("3lclsrqp0sin", since)
	// Three posts share one indexed_at; with batch size 2 the first batch
	// ends between them.
	tieAt := time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC)
	postTie1 := post("3lclsrqptie1", tieAt)
	postTie2 := post("3lclsrqptie2", tieAt)
	postTie3 := post("3lclsrqptie3", tieAt)
	postInside := post("3lclsrqp0ins", time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))
	postQueued := post("3lclsrqp0que", time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	f.enqueue(t, postQueued.uri, since)
	postEdited := post("3lclsrqp0edi", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	f.enqueue(t, postEdited.uri, since)
	editReviewSubject(t, db, postEdited, "bafypostv2edited")
	postDeleted := post("3lclsrqp0del", time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC))
	softDeleteReviewPost(t, db, postDeleted.uri)
	postAtUntil := post("3lclsrqp0unt", until)
	postAfter := post("3lclsrqp0aft", time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC))

	commentBefore := comment("3lclsrqc0bef", postInside.uri, postInside.uri, time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	commentAtSince := comment("3lclsrqc0sin", postInside.uri, postInside.uri, since)
	commentClassified := comment("3lclsrqc0cls", postInside.uri, postInside.uri, time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))
	f.enqueueComment(t, commentClassified.uri, since)
	_, err := db.ExecContext(context.Background(),
		`UPDATE classification_jobs SET status = 'classified' WHERE subject_uri = $1`, commentClassified.uri)
	require.NoError(t, err)
	orphanRootURI := postV2URI("did:plc:clsrqgoneauthor", "3lclsrqgone1")
	commentOrphan := comment("3lclsrqc0orp", orphanRootURI, orphanRootURI, time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC))
	commentNested := comment("3lclsrqc0nes", postInside.uri, commentAtSince.uri, time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	commentDeleted := comment("3lclsrqc0del", postInside.uri, postInside.uri, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	softDeleteReviewComment(t, db, commentDeleted.uri)
	commentAtUntil := comment("3lclsrqc0unt", postInside.uri, postInside.uri, until)

	service := newReenqueueService(db, reenqueueEvaluation)
	request := classification.ReenqueueRequest{Target: classification.ReenqueuePostsAndComments, Since: since, Until: until}

	report, err := service.Reenqueue(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, classification.ReenqueueReport{Created: 9, AlreadyPresent: 2}, report,
		"six posts and three comments are created; the queued post and the classified comment are present")

	postJob := func(subject reviewSubject, cid string) reenqueueJobRow {
		return queuedJob(classification.SubjectKindPost, subject.uri, cid, authorDID, community)
	}
	commentJob := func(subject reviewSubject, communityDID string) reenqueueJobRow {
		return queuedJob(classification.SubjectKindComment, subject.uri, subject.cid, commenterDID, communityDID)
	}
	classifiedComment := commentJob(commentClassified, community)
	classifiedComment.Status = "classified"
	want := []reenqueueJobRow{
		postJob(postAtSince, "bafypostv23lclsrqp0sin"),
		postJob(postTie1, "bafypostv23lclsrqptie1"),
		postJob(postTie2, "bafypostv23lclsrqptie2"),
		postJob(postTie3, "bafypostv23lclsrqptie3"),
		postJob(postInside, "bafypostv23lclsrqp0ins"),
		postJob(postQueued, "bafypostv23lclsrqp0que"),
		postJob(postEdited, "bafypostv23lclsrqp0edi"),
		postJob(postEdited, "bafypostv2edited"),
		commentJob(commentAtSince, community),
		classifiedComment,
		commentJob(commentOrphan, ""),
		commentJob(commentNested, community),
	}
	jobs := reenqueueJobs(t, db)
	assert.ElementsMatch(t, want, jobs)
	for _, outside := range []reviewSubject{postBefore, postAtUntil, postAfter, postDeleted, commentBefore, commentAtUntil, commentDeleted} {
		assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM classification_jobs WHERE subject_uri = $1`, outside.uri),
			"%s is out of range or deleted and gets no job", outside.uri)
	}

	again, err := service.Reenqueue(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, classification.ReenqueueReport{Created: 0, AlreadyPresent: 11}, again, "a repeat creates nothing")
	assert.Equal(t, 12, f.count(t, `SELECT COUNT(*) FROM classification_jobs`), "a repeat leaves the job count unchanged")
}

// TestClassificationReenqueueSkipsErasedAuthors: a post or comment whose
// author has an erasure marker gets no job and counts as neither created nor
// present; other authors' subjects are queued.
func TestClassificationReenqueueSkipsErasedAuthors(t *testing.T) {
	f := newClassificationFixture(t, nil)
	db := f.db
	liveDID := f.author(t, "clsrqliveauthor")
	erasedDID := f.author(t, "clsrqerasedauthor")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	livePost := seedReviewPost(t, db, f.community, liveDID, "3lclsrqlive01", "Live author post")
	erasedPost := seedReviewPost(t, db, f.community, erasedDID, "3lclsrqgone01", "Erased author post")
	erasedComment := seedReviewComment(t, db, livePost, erasedDID, "3lclsrqgonc01", "Erased author reply")
	setClassificationIndexedAt(t, db, classification.SubjectKindPost, livePost.uri, time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))
	setClassificationIndexedAt(t, db, classification.SubjectKindPost, erasedPost.uri, time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC))
	setClassificationIndexedAt(t, db, classification.SubjectKindComment, erasedComment.uri, time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	_, err := db.ExecContext(context.Background(), `INSERT INTO deleted_accounts (did) VALUES ($1)`, erasedDID)
	require.NoError(t, err)

	report, err := newReenqueueService(db, reenqueueEvaluation).Reenqueue(context.Background(), classification.ReenqueueRequest{
		Target: classification.ReenqueuePostsAndComments, Since: since, Until: until,
	})
	require.NoError(t, err)

	assert.Equal(t, classification.ReenqueueReport{Created: 1, AlreadyPresent: 0}, report)
	assert.Equal(t, []reenqueueJobRow{
		queuedJob(classification.SubjectKindPost, livePost.uri, livePost.cid, liveDID, f.community),
	}, reenqueueJobs(t, db), "the erased author's post and comment get no job")
}

// TestClassificationReenqueueWaitsForAuthorErasure: a re-enqueue batch waits
// for an author's erasure in progress, then sees its committed marker and
// queues no job for that author, so no job outlives the erasure.
func TestClassificationReenqueueWaitsForAuthorErasure(t *testing.T) {
	f := newClassificationFixture(t, nil)
	db := f.db
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	authorDID := f.author(t, "clsrqraceauthor")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	post := seedReviewPost(t, db, f.community, authorDID, "3lclsrqrace01", "Racing erasure post")
	setClassificationIndexedAt(t, db, classification.SubjectKindPost, post.uri, time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))

	// The fixture transaction stands in for account erasure: it holds the
	// author's exclusive erasure lock and writes the marker.
	erasure, erasureProcessID := notificationRaceTransaction(t, db, ctx)
	_, err := erasure.ExecContext(ctx, "SELECT pg_advisory_xact_lock("+ErasureLockKeySQL+")", authorDID)
	require.NoError(t, err)

	type outcome struct {
		report classification.ReenqueueReport
		err    error
	}
	results := make(chan outcome, 1)
	go func() {
		report, err := newReenqueueService(db, reenqueueEvaluation).Reenqueue(ctx, classification.ReenqueueRequest{
			Target: classification.ReenqueuePostsAndComments, Since: since, Until: until,
		})
		results <- outcome{report: report, err: err}
	}()

	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		var waiting bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks waiter
				WHERE waiter.locktype = 'advisory' AND NOT waiter.granted AND waiter.mode = 'ShareLock'
					AND waiter.database = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND $1 = ANY(pg_blocking_pids(waiter.pid)))`, erasureProcessID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("re-enqueue waiting for the author's shared erasure lock"))
	select {
	case result := <-results:
		t.Fatalf("re-enqueue returned before the erasure committed: %+v", result)
	default:
	}

	_, err = erasure.ExecContext(ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, authorDID)
	require.NoError(t, err)
	require.NoError(t, erasure.Commit())

	result := <-results
	require.NoError(t, result.err)
	assert.Equal(t, classification.ReenqueueReport{}, result.report)
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM classification_jobs WHERE author_did = $1`, authorDID),
		"no job for the erased author outlives the erasure")
}

// TestClassificationReenqueueAllCommunities: re-enqueueing all communities
// gives each community with a record one job at its record CID, counting an
// existing identical job as present, across batches of 2, and queues no
// post (B8).
func TestClassificationReenqueueAllCommunities(t *testing.T) {
	f := newClassificationFixture(t, nil)
	db := f.db
	creatorDID := f.author(t, "clsrqcreator")
	seedReviewCommunity(t, db, "rqone", creatorDID, "Heron colony", "Heron Watch", "Herons")
	second := seedReviewCommunity(t, db, "rqtwo", creatorDID, "Egret colony", "Egret Watch", "Egrets")
	seedReviewCommunity(t, db, "rqthree", creatorDID, "Ibis colony", "Ibis Watch", "Ibises")
	f.enqueueCommunity(t, second.uri, second.cid, classificationT0)
	postURI := f.visiblePost(t, f.community, creatorDID, "3lclsrqcomp01", "A post in a community")

	report, err := newReenqueueService(db, reenqueueEvaluation).Reenqueue(context.Background(),
		classification.ReenqueueRequest{Target: classification.ReenqueueAllCommunities})
	require.NoError(t, err)

	assert.Equal(t, classification.ReenqueueReport{Created: 2, AlreadyPresent: 1}, report)
	assert.ElementsMatch(t, []reenqueueJobRow{
		queuedJob(classification.SubjectKindCommunity, "did:plc:clsreviewrqonecommunity", "bafyclsreviewrqonecommunity",
			"did:plc:clsreviewrqonecommunity", "did:plc:clsreviewrqonecommunity"),
		queuedJob(classification.SubjectKindCommunity, "did:plc:clsreviewrqtwocommunity", "bafyclsreviewrqtwocommunity",
			"did:plc:clsreviewrqtwocommunity", "did:plc:clsreviewrqtwocommunity"),
		queuedJob(classification.SubjectKindCommunity, "did:plc:clsreviewrqthreecommunity", "bafyclsreviewrqthreecommunity",
			"did:plc:clsreviewrqthreecommunity", "did:plc:clsreviewrqthreecommunity"),
	}, reenqueueJobs(t, db))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM classification_jobs WHERE subject_uri = $1`, postURI),
		"an allCommunities re-enqueue queues no post")
}

// TestClassificationReenqueueAfterPolicyBump: re-enqueueing under a new
// policy version queues a new job for each subject at its current CID and
// leaves the classified jobs and results of the old version unchanged (B9).
func TestClassificationReenqueueAfterPolicyBump(t *testing.T) {
	f := newClassificationFixture(t, nil)
	db := f.db
	authorDID := f.author(t, "clsrqbumpauthor")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	post := seedReviewPost(t, db, f.community, authorDID, "3lclsrqbump01", "Policy bump post")
	comment := seedReviewComment(t, db, post, authorDID, "3lclsrqbumc01", "Policy bump reply")
	setClassificationIndexedAt(t, db, classification.SubjectKindPost, post.uri, time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC))
	setClassificationIndexedAt(t, db, classification.SubjectKindComment, comment.uri, time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC))
	seedClassificationResult(t, db, post.result("bumppost", map[string]float64{"spam_scam": 0.7}, time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)))
	seedClassificationResult(t, db, comment.result("bumpcomment", map[string]float64{"spam_scam": 0.2}, time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)))
	// Both v1 jobs share one image model, so the v2 evaluation differs from
	// them only in its policy version.
	_, err := db.ExecContext(context.Background(), `UPDATE classification_jobs SET image_model_id = '@cf/test/image-bump'`)
	require.NoError(t, err)

	evaluation := classification.Evaluation{PolicyVersion: "v2", TextModelID: reviewModelOne, ImageModelID: "@cf/test/image-bump"}
	report, err := newReenqueueService(db, evaluation).Reenqueue(context.Background(), classification.ReenqueueRequest{
		Target: classification.ReenqueuePostsAndComments, Since: since, Until: until,
	})
	require.NoError(t, err)

	assert.Equal(t, classification.ReenqueueReport{Created: 2, AlreadyPresent: 0}, report)
	job := func(subject reviewSubject, policyVersion, status string) reenqueueJobRow {
		return reenqueueJobRow{
			SubjectURI: subject.uri, SubjectKind: string(subject.kind), RecordCID: subject.cid,
			PolicyVersion: policyVersion, TextModelID: reviewModelOne, ImageModelID: "@cf/test/image-bump",
			AuthorDID: authorDID, CommunityDID: f.community, Status: status,
		}
	}
	assert.ElementsMatch(t, []reenqueueJobRow{
		job(post, "v1", "classified"),
		job(post, "v2", "queued"),
		job(comment, "v1", "classified"),
		job(comment, "v2", "queued"),
	}, reenqueueJobs(t, db))

	rows, err := db.QueryContext(context.Background(),
		`SELECT subject_uri, policy_version, probabilities::text FROM classification_results ORDER BY subject_uri`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var results [][3]string
	for rows.Next() {
		var row [3]string
		require.NoError(t, rows.Scan(&row[0], &row[1], &row[2]))
		results = append(results, row)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, [][3]string{
		{post.uri, "v1", `{"spam_scam": 0.7}`},
		{comment.uri, "v1", `{"spam_scam": 0.2}`},
	}, results, "the v1 results are kept unchanged")
}

// TestClassificationReenqueueRevivesFailedJobs: a re-enqueue re-queues a job
// that ended failed under the same evaluation, with its attempts, error code
// and deferral backoff reset, and counts it revived; a closed job stays
// closed and counts present, and a queued one is left as it is (R6).
func TestClassificationReenqueueRevivesFailedJobs(t *testing.T) {
	f := newClassificationFixture(t, nil)
	db := f.db
	authorDID := f.author(t, "clsrqreviveauthor")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	post := func(rkey string, at time.Time) reviewSubject {
		subject := seedReviewPost(t, db, f.community, authorDID, rkey, "Revive "+rkey)
		setClassificationIndexedAt(t, db, classification.SubjectKindPost, subject.uri, at)
		return subject
	}
	failedPost := post("3lclsrqrvfai1", time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))
	closedPost := post("3lclsrqrvclo1", time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC))
	queuedPost := post("3lclsrqrvque1", time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	newPost := post("3lclsrqrvnew1", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	for _, subject := range []reviewSubject{failedPost, closedPost, queuedPost} {
		f.enqueue(t, subject.uri, since)
	}
	_, err := db.ExecContext(context.Background(), `
		UPDATE classification_jobs
		SET status = 'failed', attempt_count = 5, last_error_code = 'provider_error', deferral_count = 3,
		    next_attempt_at = '2026-10-01T00:00:00Z'
		WHERE subject_uri = $1`, failedPost.uri)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		UPDATE classification_jobs SET status = 'closed', close_reason = 'not_visible', attempt_count = 2
		WHERE subject_uri = $1`, closedPost.uri)
	require.NoError(t, err)

	service := newReenqueueService(db, reenqueueEvaluation)
	request := classification.ReenqueueRequest{Target: classification.ReenqueuePostsAndComments, Since: since, Until: until}
	before := time.Now()
	report, err := service.Reenqueue(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, classification.ReenqueueReport{Created: 1, Revived: 1, AlreadyPresent: 2}, report,
		"the new post is created, the failed one revived, the closed and queued ones present")

	var (
		status, lastErrorCode, closeReason sql.NullString
		attempts, deferrals                int
		nextAttemptAt                      time.Time
	)
	readJob := func(uri string) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT status, attempt_count, last_error_code, close_reason, deferral_count, next_attempt_at
			FROM classification_jobs WHERE subject_uri = $1`, uri,
		).Scan(&status, &attempts, &lastErrorCode, &closeReason, &deferrals, &nextAttemptAt))
	}
	readJob(failedPost.uri)
	assert.Equal(t, "queued", status.String, "the failed job is re-queued")
	assert.Zero(t, attempts, "its attempts are reset")
	assert.False(t, lastErrorCode.Valid, "its error code is cleared")
	assert.Zero(t, deferrals, "its deferral backoff is reset")
	assert.False(t, nextAttemptAt.Before(before.Add(-time.Minute)), "it is due now, not at its old next_attempt_at")
	readJob(closedPost.uri)
	assert.Equal(t, "closed", status.String, "a closed job is not revived")
	assert.Equal(t, "not_visible", closeReason.String)
	assert.Equal(t, 2, attempts)
	readJob(newPost.uri)
	assert.Equal(t, "queued", status.String, "the new post's job is created queued")
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM classification_jobs`), "one job per subject")

	again, err := service.Reenqueue(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, classification.ReenqueueReport{Created: 0, Revived: 0, AlreadyPresent: 4}, again,
		"a revived job is present on a repeat")

	communityCreator := f.author(t, "clsrqrevivecreator")
	community := seedReviewCommunity(t, db, "rqrevive", communityCreator, "Plover colony", "Plover Watch", "Plovers")
	f.enqueueCommunity(t, community.uri, community.cid, classificationT0)
	_, err = db.ExecContext(context.Background(),
		`UPDATE classification_jobs SET status = 'failed', attempt_count = 5 WHERE subject_uri = $1`, community.uri)
	require.NoError(t, err)
	communities, err := service.Reenqueue(context.Background(),
		classification.ReenqueueRequest{Target: classification.ReenqueueAllCommunities})
	require.NoError(t, err)
	assert.Equal(t, 1, communities.Revived, "a failed community job is revived")
	assert.Equal(t, "queued", reenqueueJobStatus(t, db, community.uri))
}

// reenqueueJobStatus reads the status of subjectURI's single job.
func reenqueueJobStatus(t *testing.T, db *sql.DB, subjectURI string) string {
	t.Helper()
	var status string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status FROM classification_jobs WHERE subject_uri = $1`, subjectURI).Scan(&status))
	return status
}
