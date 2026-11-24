//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/classification"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin review of classification results over Postgres
// (docs/PRD_CONTENT_CLASSIFICATION.md R6, D18): the result listing, verdicts
// and precision through the real review service and repository.

const (
	reviewModelOne = "@cf/test/model-one"
	reviewModelTwo = "@cf/test/model-two"
	// reviewAdminZ records first; reviewAdminA, earlier in DID order,
	// records later, so "latest verdict wins" is not "greatest admin DID".
	reviewAdminZ = "did:plc:clsreviewadminz"
	reviewAdminA = "did:plc:clsreviewadmina"
	// reviewThumbCID and reviewAvatarCID are valid raw-leaf CIDs, so the
	// subject loaders count them as images.
	reviewThumbCID  = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	reviewAvatarCID = "bafkreihrj4ia2upinbp7ej75dvxs4zdhpninlziyxbqzkyq4spauoxa6ha"
)

// classificationReviewT0 is the review clock's start: verdicts are recorded
// at it unless a test moves the clock.
var classificationReviewT0 = time.Date(2026, 11, 12, 15, 30, 0, 250000000, time.UTC)

// classificationResultSeed is one stored classification result and the
// classified job it belongs to. modelID and policyVersion default to
// reviewModelOne and v1; label names the seed in expected lists.
type classificationResultSeed struct {
	label         string
	kind          classification.SubjectKind
	subjectURI    string
	scoredCID     string
	communityDID  string
	authorDID     string
	modelID       string
	policyVersion string
	probabilities map[string]float64
	imagesSent    int
	imagesSkipped int
	textTruncated bool
	partial       bool
	createdAt     time.Time
}

// seedClassificationResult inserts a classified job and its result with the
// seed's created_at, and returns the result id. The job's image model is
// unique per label, so one subject can carry several results at one CID.
func seedClassificationResult(t *testing.T, db *sql.DB, seed classificationResultSeed) int64 {
	t.Helper()
	modelID := seed.modelID
	if modelID == "" {
		modelID = reviewModelOne
	}
	policyVersion := seed.policyVersion
	if policyVersion == "" {
		policyVersion = "v1"
	}
	probabilities, err := json.Marshal(seed.probabilities)
	require.NoError(t, err)
	ctx := context.Background()
	var jobID int64
	require.NoErrorf(t, db.QueryRowContext(ctx, `
		INSERT INTO classification_jobs
		    (subject_uri, subject_kind, record_cid, policy_version, text_model_id, image_model_id, status,
		     author_did, community_did, created_at, updated_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'classified', $7, $8, $9, $9, $9)
		RETURNING id
	`, seed.subjectURI, string(seed.kind), seed.scoredCID, policyVersion, modelID, "@cf/test/image-"+seed.label,
		seed.authorDID, seed.communityDID, seed.createdAt).Scan(&jobID), "seeding the job of result %s", seed.label)
	var resultID int64
	require.NoErrorf(t, db.QueryRowContext(ctx, `
		INSERT INTO classification_results
		    (job_id, subject_uri, subject_cid, subject_kind, community_did, author_did, model_id, policy_version,
		     probabilities, images_sent, images_skipped, text_truncated, created_at, partial)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id
	`, jobID, seed.subjectURI, seed.scoredCID, string(seed.kind), seed.communityDID, seed.authorDID, modelID, policyVersion,
		string(probabilities), seed.imagesSent, seed.imagesSkipped, seed.textTruncated, seed.createdAt, seed.partial).Scan(&resultID),
		"seeding result %s", seed.label)
	return resultID
}

// reviewSubject is a seeded classified subject at its current CID.
type reviewSubject struct {
	kind         classification.SubjectKind
	uri          string
	cid          string
	authorDID    string
	communityDID string
}

// result is a seed for a result scored at the subject's current CID.
func (s reviewSubject) result(label string, probabilities map[string]float64, createdAt time.Time) classificationResultSeed {
	return classificationResultSeed{
		label:         label,
		kind:          s.kind,
		subjectURI:    s.uri,
		scoredCID:     s.cid,
		communityDID:  s.communityDID,
		authorDID:     s.authorDID,
		probabilities: probabilities,
		createdAt:     createdAt,
	}
}

// seedReviewPost seeds a postv2 post by authorDID with an acceptance pinned
// to its CID.
func seedReviewPost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey, title string) reviewSubject {
	t.Helper()
	uri := seedVisibilityPost(t, db, communityDID, authorDID, rkey, title, classificationT0.Add(-time.Hour))
	seedVisibilityAdmission(t, db, communityDID, uri, posts.AdmissionStatusAccepted, "", "")
	return reviewSubject{kind: classification.SubjectKindPost, uri: uri, cid: "bafypostv2" + rkey, authorDID: authorDID, communityDID: communityDID}
}

// seedReviewComment seeds a comment by commenterDID on root.
func seedReviewComment(t *testing.T, db *sql.DB, root reviewSubject, commenterDID, rkey, body string) reviewSubject {
	t.Helper()
	uri := seedClassificationComment(t, db, commenterDID, rkey, root.uri, root.uri, body)
	return reviewSubject{kind: classification.SubjectKindComment, uri: uri, cid: "bafycmt" + rkey, authorDID: commenterDID, communityDID: root.communityDID}
}

// seedReviewCommunity seeds a community subject created by creatorDID; its
// result's author and community are the community itself.
func seedReviewCommunity(t *testing.T, db *sql.DB, label, creatorDID, name, displayName, description string) reviewSubject {
	t.Helper()
	did := "did:plc:clsreview" + label + "community"
	cid := "bafyclsreview" + label + "community"
	seedClassificationCommunity(t, db, did, "clsreview"+label+".coves.social", creatorDID, name, displayName, description, cid)
	return reviewSubject{kind: classification.SubjectKindCommunity, uri: did, cid: cid, authorDID: did, communityDID: did}
}

// editReviewSubject moves the subject's current CID to newCID, as indexing an
// edit does.
func editReviewSubject(t *testing.T, db *sql.DB, subject reviewSubject, newCID string) {
	t.Helper()
	query := map[classification.SubjectKind]string{
		classification.SubjectKindPost:      `UPDATE posts SET cid = $2 WHERE uri = $1`,
		classification.SubjectKindComment:   `UPDATE comments SET cid = $2 WHERE uri = $1`,
		classification.SubjectKindCommunity: `UPDATE communities SET record_cid = $2 WHERE did = $1`,
	}[subject.kind]
	result, err := db.ExecContext(context.Background(), query, subject.uri, newCID)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "editing %s", subject.uri)
}

// softDeleteReviewPost marks the post deleted as the post delete path does:
// deleted_at set, CID kept.
func softDeleteReviewPost(t *testing.T, db *sql.DB, postURI string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, postURI)
	require.NoError(t, err)
}

// softDeleteReviewComment marks the comment deleted as the comment delete
// path (postgresCommentRepo.Delete) does: deleted_at set, body and CID kept.
func softDeleteReviewComment(t *testing.T, db *sql.DB, commentURI string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE comments SET deleted_at = NOW() WHERE uri = $1`, commentURI)
	require.NoError(t, err)
}

// reviewDoxingReason is a hidden reason: the admin moderation log withholds
// the current text of a subject with such an action.
const reviewDoxingReason = "social.coves.moderation.defs#reasonDoxing"

type classificationReview struct {
	db      *sql.DB
	clock   *classificationClock
	service *classification.ReviewService
}

func newClassificationReview(t *testing.T) *classificationReview {
	t.Helper()
	db := testkit.DB(t)
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)
	clock := &classificationClock{now: classificationReviewT0}
	return &classificationReview{
		db:      db,
		clock:   clock,
		service: classification.NewReviewService(NewClassificationReviewRepository(db, classificationInstanceDID), policy, clock.Now),
	}
}

func (r *classificationReview) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	require.NoError(t, r.db.QueryRowContext(context.Background(), query, args...).Scan(&count))
	return count
}

// listByID lists every result in one page, keyed by result id.
func (r *classificationReview) listByID(t *testing.T) map[int64]classification.ResultView {
	t.Helper()
	page, err := r.service.ListResults(context.Background(), classification.ResultFilter{Limit: 100})
	require.NoError(t, err)
	views := make(map[int64]classification.ResultView, len(page.Results))
	for _, view := range page.Results {
		views[view.ID] = view
	}
	return views
}

// classificationVerdictRow is one stored classification_verdicts row.
type classificationVerdictRow struct {
	present    string
	recordedAt time.Time
}

// verdictRows reads a result's stored verdicts, keyed by admin DID.
func (r *classificationReview) verdictRows(t *testing.T, resultID int64) map[string]classificationVerdictRow {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(),
		`SELECT admin_did, present::text, recorded_at FROM classification_verdicts WHERE result_id = $1`, resultID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	stored := make(map[string]classificationVerdictRow)
	for rows.Next() {
		var (
			adminDID string
			row      classificationVerdictRow
		)
		require.NoError(t, rows.Scan(&adminDID, &row.present, &row.recordedAt))
		row.recordedAt = row.recordedAt.UTC()
		stored[adminDID] = row
	}
	require.NoError(t, rows.Err())
	return stored
}

// TestClassificationReviewListFilters: each filter returns only its matching
// rows newest first, boundaries are inclusive at minProbability and since and
// exclusive at until, a combined filter excludes decoys that each fail one
// criterion, and limit-2 paging over created_at ties visits every row once
// (B5).
func TestClassificationReviewListFilters(t *testing.T) {
	r := newClassificationReview(t)
	db := r.db
	communityA := visibilityCommunity(t, db, "clsreviewa")
	communityB := visibilityCommunity(t, db, "clsreviewb")
	authorDID := "did:plc:clsreviewauthor"
	createTestUser(t, db, "clsreviewauthor.test", authorDID)
	postA1 := seedReviewPost(t, db, communityA, authorDID, "3lclsrvwpa001", "Filter heron one")
	postA2 := seedReviewPost(t, db, communityA, authorDID, "3lclsrvwpa002", "Filter heron two")
	postB1 := seedReviewPost(t, db, communityB, authorDID, "3lclsrvwpb001", "Filter egret")
	comment := seedReviewComment(t, db, postA1, authorDID, "3lclsrvwca001", "Filter heron reply")
	community := seedReviewCommunity(t, db, "filter", authorDID, "Heron colony", "Heron Watch", "A community about herons")

	base := time.Date(2026, 11, 10, 9, 0, 0, 123456000, time.UTC)
	since := base.Add(10 * time.Minute)
	until := base.Add(20 * time.Minute)
	withModel := func(seed classificationResultSeed, modelID, policyVersion string) classificationResultSeed {
		seed.modelID, seed.policyVersion = modelID, policyVersion
		return seed
	}
	seeds := []classificationResultSeed{
		postA1.result("r01", map[string]float64{"spam_scam": 0.5}, since),
		postA2.result("r02", map[string]float64{"spam_scam": 0.9, "sexual_minors": 0.1}, since.Add(5*time.Minute)),
		postA1.result("r03", map[string]float64{"hate_harassment": 0.9}, since.Add(time.Minute)),
		postA2.result("r04", map[string]float64{"spam_scam": 0.499999}, since.Add(2*time.Minute)),
		comment.result("r05", map[string]float64{"spam_scam": 0.8}, since.Add(3*time.Minute)),
		postB1.result("r06", map[string]float64{"spam_scam": 0.8}, since.Add(4*time.Minute)),
		withModel(postA1.result("r07", map[string]float64{"spam_scam": 0.8}, since.Add(5*time.Minute)), reviewModelTwo, "v1"),
		withModel(postA1.result("r08", map[string]float64{"spam_scam": 0.8}, since.Add(5*time.Minute)), reviewModelOne, "v0"),
		postA2.result("r09", map[string]float64{"spam_scam": 0.8}, since.Add(-time.Microsecond)),
		postA2.result("r10", map[string]float64{"spam_scam": 0.8}, until),
		community.result("r11", map[string]float64{"sexual_minors": 0.2, "spam_scam": 0.3}, base.Add(time.Minute)),
		withModel(postB1.result("r12", map[string]float64{"sexual_minors": 0.7}, base.Add(2*time.Minute)), reviewModelTwo, "v0"),
		withModel(comment.result("r13", map[string]float64{"sexual_minors": 0.05, "spam_scam": 0.6}, base.Add(2*time.Minute)), reviewModelOne, "v0"),
	}
	labels := make(map[int64]string, len(seeds))
	for _, seed := range seeds {
		labels[seedClassificationResult(t, db, seed)] = seed.label
	}
	labelsOf := func(views []classification.ResultView) []string {
		got := make([]string, 0, len(views))
		for _, view := range views {
			label, ok := labels[view.ID]
			if !ok {
				label = fmt.Sprintf("unknown result %d", view.ID)
			}
			got = append(got, label)
		}
		return got
	}
	half := 0.5

	for _, row := range []struct {
		name   string
		filter classification.ResultFilter
		want   []string
	}{
		{
			name:   "question alone",
			filter: classification.ResultFilter{Question: "sexual_minors"},
			want:   []string{"r02", "r13", "r12", "r11"},
		},
		{
			name:   "question with minProbability includes the boundary row",
			filter: classification.ResultFilter{Question: "spam_scam", MinProbability: &half},
			want:   []string{"r10", "r08", "r07", "r02", "r06", "r05", "r01", "r09", "r13"},
		},
		{
			name:   "subject kind comment",
			filter: classification.ResultFilter{SubjectKind: classification.SubjectKindComment},
			want:   []string{"r05", "r13"},
		},
		{
			name:   "subject kind community",
			filter: classification.ResultFilter{SubjectKind: classification.SubjectKindCommunity},
			want:   []string{"r11"},
		},
		{
			name:   "community",
			filter: classification.ResultFilter{CommunityDID: communityB},
			want:   []string{"r06", "r12"},
		},
		{
			name:   "model",
			filter: classification.ResultFilter{ModelID: reviewModelTwo},
			want:   []string{"r07", "r12"},
		},
		{
			name:   "policy version",
			filter: classification.ResultFilter{PolicyVersion: "v0"},
			want:   []string{"r08", "r13", "r12"},
		},
		{
			name:   "since is inclusive",
			filter: classification.ResultFilter{Since: since},
			want:   []string{"r10", "r08", "r07", "r02", "r06", "r05", "r04", "r03", "r01"},
		},
		{
			name:   "until is exclusive",
			filter: classification.ResultFilter{Until: until},
			want:   []string{"r08", "r07", "r02", "r06", "r05", "r04", "r03", "r01", "r09", "r13", "r12", "r11"},
		},
		{
			name:   "since and until",
			filter: classification.ResultFilter{Since: since, Until: until},
			want:   []string{"r08", "r07", "r02", "r06", "r05", "r04", "r03", "r01"},
		},
		{
			// Decoys r03 (no spam_scam), r04 (below min), r05 (comment), r06
			// (community B), r07 (model two), r08 (policy v0), r09 (before
			// since) and r10 (at until) each fail exactly one criterion.
			name: "every filter combined",
			filter: classification.ResultFilter{
				Question:       "spam_scam",
				MinProbability: &half,
				SubjectKind:    classification.SubjectKindPost,
				CommunityDID:   communityA,
				ModelID:        reviewModelOne,
				PolicyVersion:  "v1",
				Since:          since,
				Until:          until,
			},
			want: []string{"r02", "r01"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			page, err := r.service.ListResults(context.Background(), row.filter)
			require.NoError(t, err)
			assert.Equal(t, row.want, labelsOf(page.Results))
			assert.Empty(t, page.Cursor, "a page holding every match has no cursor")
		})
	}

	t.Run("limit 2 pages visit every row once across created_at ties", func(t *testing.T) {
		var (
			visited   []string
			pageSizes []int
			cursor    string
		)
		for range 20 {
			page, err := r.service.ListResults(context.Background(), classification.ResultFilter{Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			visited = append(visited, labelsOf(page.Results)...)
			pageSizes = append(pageSizes, len(page.Results))
			cursor = page.Cursor
			if cursor == "" {
				break
			}
		}
		assert.Equal(t, []string{"r10", "r08", "r07", "r02", "r06", "r05", "r04", "r03", "r01", "r09", "r13", "r12", "r11"}, visited,
			"newest first, ties by id descending, no gaps or duplicates")
		assert.Equal(t, []int{2, 2, 2, 2, 2, 2, 1}, pageSizes, "the final page has one row and no cursor")
	})
}

// assertReviewSubjectView compares a subject view field by field; an empty
// want.Images accepts nil or empty.
func assertReviewSubjectView(t *testing.T, want, got classification.SubjectView) {
	t.Helper()
	assert.Equal(t, want.Visibility, got.Visibility, "visibility")
	assert.Equal(t, want.Deleted, got.Deleted, "deleted")
	assert.Equal(t, want.CurrentCID, got.CurrentCID, "current CID")
	assert.Equal(t, want.Post, got.Post, "post fields")
	assert.Equal(t, want.Comment, got.Comment, "comment fields")
	assert.Equal(t, want.Community, got.Community, "community fields")
	if len(want.Images) == 0 {
		assert.Empty(t, got.Images, "images")
	} else {
		assert.Equal(t, want.Images, got.Images, "images")
	}
}

// TestClassificationReviewListViews: post, comment and community results
// carry their subject's current view and a current scored CID; an edit makes
// the scored CID stale and the view shows the new CID; a soft-deleted post or
// comment is deleted and stale with no current CID and no content; a
// moderation-hidden unchanged post stays current; a doxing-removed post, and a
// comment whose root or chain post is doxing-removed, has its content withheld
// (B6, R2).
func TestClassificationReviewListViews(t *testing.T) {
	r := newClassificationReview(t)
	db := r.db
	communityDID := visibilityCommunity(t, db, "clsreviewviews")
	authorDID := "did:plc:clsreviewviewer"
	createTestUser(t, db, "clsreviewviewer.test", authorDID)
	commenterDID := "did:plc:clsreviewcommenter"
	createTestUser(t, db, "clsreviewcommenter.test", commenterDID)
	creatorDID := "did:plc:clsreviewcreator"
	createTestUser(t, db, "clsreviewcreator.test", creatorDID)

	postURI := seedVisibilityPostWithEmbed(t, db, communityDID, authorDID, "3lclsrvwview1", "Reviewed gannet survey",
		reviewThumbCID, classificationT0.Add(-time.Hour))
	_, err := db.ExecContext(context.Background(), `UPDATE posts SET content = $2 WHERE uri = $1`, postURI, "Counting gannets on the cliffs")
	require.NoError(t, err)
	seedVisibilityAdmission(t, db, communityDID, postURI, posts.AdmissionStatusAccepted, "", "")
	post := reviewSubject{kind: classification.SubjectKindPost, uri: postURI, cid: "bafypostv23lclsrvwview1", authorDID: authorDID, communityDID: communityDID}
	comment := seedReviewComment(t, db, post, commenterDID, "3lclsrvwvcmt1", "A reviewed reply about gannets")
	community := seedReviewCommunity(t, db, "views", creatorDID, "Puffin colony", "Puffin Watch", "A community about puffins")
	setClassificationCommunityImages(t, db, community.uri, reviewAvatarCID, "")
	deleted := seedReviewPost(t, db, communityDID, authorDID, "3lclsrvwdel01", "Deleted tern")
	softDeleteReviewPost(t, db, deleted.uri)
	hidden := seedReviewPost(t, db, communityDID, authorDID, "3lclsrvwhid01", "Removed tern")
	seedPostInstanceRemoval(t, db, classificationInstanceDID, hidden.uri, "social.coves.moderation.defs#reasonSpam")
	deletedComment := seedReviewComment(t, db, post, commenterDID, "3lclsrvwdcmt1", "A deleted reply about gannets")
	softDeleteReviewComment(t, db, deletedComment.uri)
	doxed := seedReviewPost(t, db, communityDID, authorDID, "3lclsrvwdox01", "Doxed tern")
	seedPostInstanceRemoval(t, db, classificationInstanceDID, doxed.uri, reviewDoxingReason)
	// One reply declares the doxing-removed post its root while its parent
	// chain reaches the visible post; the other declares the visible post its
	// root while its chain reaches the doxing-removed post.
	doxedRootReplyURI := seedClassificationComment(t, db, commenterDID, "3lclsrvwdxrt1", doxed.uri, post.uri, "A reply under the doxing post")
	doxedRootReply := reviewSubject{kind: classification.SubjectKindComment, uri: doxedRootReplyURI, cid: "bafycmt3lclsrvwdxrt1", authorDID: commenterDID, communityDID: communityDID}
	doxedChainReplyURI := seedClassificationComment(t, db, commenterDID, "3lclsrvwdxch1", post.uri, doxed.uri, "A reply whose chain reaches the doxing post")
	doxedChainReply := reviewSubject{kind: classification.SubjectKindComment, uri: doxedChainReplyURI, cid: "bafycmt3lclsrvwdxch1", authorDID: commenterDID, communityDID: communityDID}

	createdAt := time.Date(2026, 11, 11, 8, 0, 0, 654321000, time.UTC)
	postSeed := post.result("post", map[string]float64{"spam_scam": 0.123456, "hate_harassment": 0.7}, createdAt)
	postSeed.imagesSent, postSeed.imagesSkipped, postSeed.textTruncated = 1, 2, true
	postID := seedClassificationResult(t, db, postSeed)
	commentID := seedClassificationResult(t, db, comment.result("comment", map[string]float64{"spam_scam": 0.2}, createdAt.Add(time.Minute)))
	communityID := seedClassificationResult(t, db, community.result("community", map[string]float64{"spam_scam": 0.3}, createdAt.Add(2*time.Minute)))
	deletedID := seedClassificationResult(t, db, deleted.result("deleted", map[string]float64{"spam_scam": 0.4}, createdAt.Add(3*time.Minute)))
	hiddenID := seedClassificationResult(t, db, hidden.result("hidden", map[string]float64{"spam_scam": 0.5}, createdAt.Add(4*time.Minute)))
	deletedCommentID := seedClassificationResult(t, db, deletedComment.result("deleted-comment", map[string]float64{"spam_scam": 0.4}, createdAt.Add(5*time.Minute)))
	doxedID := seedClassificationResult(t, db, doxed.result("doxed", map[string]float64{"spam_scam": 0.5}, createdAt.Add(6*time.Minute)))
	doxedRootReplyID := seedClassificationResult(t, db, doxedRootReply.result("doxed-root-reply", map[string]float64{"spam_scam": 0.5}, createdAt.Add(7*time.Minute)))
	doxedChainReplyID := seedClassificationResult(t, db, doxedChainReply.result("doxed-chain-reply", map[string]float64{"spam_scam": 0.5}, createdAt.Add(8*time.Minute)))

	views := r.listByID(t)
	require.Len(t, views, 9, "every seeded result is listed")

	postView := views[postID]
	assert.Equal(t, classification.SubjectKindPost, postView.SubjectKind)
	assert.Equal(t, postURI, postView.SubjectURI)
	assert.Equal(t, "bafypostv23lclsrvwview1", postView.ScoredCID)
	assert.Equal(t, communityDID, postView.CommunityDID)
	assert.Equal(t, authorDID, postView.AuthorDID)
	assert.Equal(t, reviewModelOne, postView.ModelID)
	assert.Equal(t, "v1", postView.PolicyVersion)
	assert.Equal(t, map[string]float64{"spam_scam": 0.123456, "hate_harassment": 0.7}, postView.Probabilities)
	assert.Equal(t, 1, postView.ImagesSent)
	assert.Equal(t, 2, postView.ImagesSkipped)
	assert.True(t, postView.TextTruncated)
	assert.True(t, createdAt.Equal(postView.CreatedAt), "created_at %v, want %v", postView.CreatedAt, createdAt)

	for _, row := range []struct {
		name        string
		id          int64
		wantCurrent bool
		want        classification.SubjectView
	}{
		{
			name:        "post",
			id:          postID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityVisible,
				CurrentCID: "bafypostv23lclsrvwview1",
				Post: &classification.PostFields{
					Title:         "Reviewed gannet survey",
					Body:          "Counting gannets on the cliffs",
					ExternalURI:   "https://example.com/article",
					ExternalTitle: "Example Article",
				},
				Images: []classification.ImageRef{{OwnerDID: authorDID, CID: reviewThumbCID}},
			},
		},
		{
			name:        "comment",
			id:          commentID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityVisible,
				CurrentCID: "bafycmt3lclsrvwvcmt1",
				Comment:    &classification.CommentFields{Body: "A reviewed reply about gannets"},
			},
		},
		{
			name:        "community",
			id:          communityID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityVisible,
				CurrentCID: "bafyclsreviewviewscommunity",
				Community: &classification.CommunityFields{
					Name:        "Puffin colony",
					DisplayName: "Puffin Watch",
					Description: "A community about puffins",
				},
				Images: []classification.ImageRef{{OwnerDID: "did:plc:clsreviewviewscommunity", CID: reviewAvatarCID}},
			},
		},
		{
			name:        "moderation-hidden post at the scored CID",
			id:          hiddenID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityHidden,
				CurrentCID: "bafypostv23lclsrvwhid01",
				Post:       &classification.PostFields{Title: "Removed tern"},
			},
		},
		{
			// A hidden-reason removal withholds the subject's content, as the
			// admin moderation log does.
			name:        "doxing-removed post at the scored CID",
			id:          doxedID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityHidden,
				CurrentCID: "bafypostv23lclsrvwdox01",
			},
		},
		{
			name:        "comment whose declared root is a doxing-removed post",
			id:          doxedRootReplyID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityHidden,
				CurrentCID: "bafycmt3lclsrvwdxrt1",
			},
		},
		{
			name:        "comment whose chain reaches a doxing-removed post",
			id:          doxedChainReplyID,
			wantCurrent: true,
			want: classification.SubjectView{
				Visibility: classification.VisibilityHidden,
				CurrentCID: "bafycmt3lclsrvwdxch1",
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			view, ok := views[row.id]
			require.True(t, ok, "result %d is listed", row.id)
			assert.Equal(t, row.wantCurrent, view.ScoredCIDIsCurrent, "scored CID is current")
			assertReviewSubjectView(t, row.want, view.Subject)
		})
	}

	for _, row := range []struct {
		name string
		id   int64
	}{
		{name: "soft-deleted post", id: deletedID},
		{name: "soft-deleted comment", id: deletedCommentID},
	} {
		t.Run(row.name, func(t *testing.T) {
			view, ok := views[row.id]
			require.True(t, ok, "the deleted subject's result is listed")
			assert.True(t, view.Subject.Deleted, "a soft-deleted subject is deleted")
			assert.False(t, view.ScoredCIDIsCurrent, "a deleted subject's scored CID is not current")
			assert.Empty(t, view.Subject.CurrentCID, "a deleted subject has no current CID")
			assert.Nil(t, view.Subject.Post, "a deleted subject's post text is withheld")
			assert.Nil(t, view.Subject.Comment, "a deleted subject's comment text is withheld")
			assert.Nil(t, view.Subject.Community, "a deleted subject's community fields are withheld")
			assert.Empty(t, view.Subject.Images, "a deleted subject's images are withheld")
		})
	}

	editReviewSubject(t, db, post, "bafyclsreviewpostedited")
	editReviewSubject(t, db, comment, "bafyclsreviewcommentedited")
	editReviewSubject(t, db, community, "bafyclsreviewcommunityedited")
	edited := r.listByID(t)
	for _, row := range []struct {
		name    string
		id      int64
		wantCID string
	}{
		{name: "edited post", id: postID, wantCID: "bafyclsreviewpostedited"},
		{name: "edited comment", id: commentID, wantCID: "bafyclsreviewcommentedited"},
		{name: "edited community", id: communityID, wantCID: "bafyclsreviewcommunityedited"},
	} {
		t.Run(row.name, func(t *testing.T) {
			view, ok := edited[row.id]
			require.True(t, ok, "result %d is listed after the edit", row.id)
			assert.False(t, view.ScoredCIDIsCurrent, "an edit makes the scored CID stale")
			assert.Equal(t, row.wantCID, view.Subject.CurrentCID)
			assert.False(t, view.Subject.Deleted)
		})
	}
}

// TestClassificationReviewVerdicts: a verdict on a result whose subject was
// edited, or whose post or comment was soft-deleted, is refused and stores nothing; one at the current CID
// is stored, a re-record by the same admin replaces it and a second admin
// adds a row; an unknown result is not found; erasing the author removes the
// result and its verdicts (B7, R2).
func TestClassificationReviewVerdicts(t *testing.T) {
	ctx := context.Background()
	for _, row := range []struct {
		name string
		seed func(t *testing.T, db *sql.DB) reviewSubject
	}{
		{
			name: "post",
			seed: func(t *testing.T, db *sql.DB) reviewSubject {
				communityDID := visibilityCommunity(t, db, "clsverdictpost")
				createTestUser(t, db, "clsverdictauthor.test", "did:plc:clsverdictauthor")
				return seedReviewPost(t, db, communityDID, "did:plc:clsverdictauthor", "3lclsvrdpost1", "Verdict gannet")
			},
		},
		{
			name: "comment",
			seed: func(t *testing.T, db *sql.DB) reviewSubject {
				communityDID := visibilityCommunity(t, db, "clsverdictcmt")
				createTestUser(t, db, "clsverdictauthor.test", "did:plc:clsverdictauthor")
				root := seedReviewPost(t, db, communityDID, "did:plc:clsverdictauthor", "3lclsvrdroot1", "Verdict root")
				return seedReviewComment(t, db, root, "did:plc:clsverdictauthor", "3lclsvrdcmt01", "Verdict reply")
			},
		},
		{
			name: "community",
			seed: func(t *testing.T, db *sql.DB) reviewSubject {
				createTestUser(t, db, "clsverdictauthor.test", "did:plc:clsverdictauthor")
				return seedReviewCommunity(t, db, "verdict", "did:plc:clsverdictauthor", "Gannet colony", "Gannet Watch", "A community about gannets")
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newClassificationReview(t)
			subject := row.seed(t, r.db)
			probabilities := map[string]float64{"spam_scam": 0.8, "hate_harassment": 0.1}
			createdAt := time.Date(2026, 11, 11, 10, 0, 0, 111111000, time.UTC)
			staleID := seedClassificationResult(t, r.db, subject.result("stale", probabilities, createdAt))
			editReviewSubject(t, r.db, subject, "bafyclsverdictedited")
			subject.cid = "bafyclsverdictedited"
			currentID := seedClassificationResult(t, r.db, subject.result("current", probabilities, createdAt.Add(time.Minute)))

			_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
				ResultID: staleID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true, "hate_harassment": false},
			})
			require.ErrorIs(t, err, classification.ErrUnreviewable, "a result scored before an edit is not reviewable")
			assert.Empty(t, r.verdictRows(t, staleID), "a refused verdict stores nothing")

			verdict, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
				ResultID: currentID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true, "hate_harassment": false},
			})
			require.NoError(t, err)
			assert.Equal(t, currentID, verdict.ResultID)
			assert.Equal(t, reviewAdminZ, verdict.AdminDID)
			assert.Equal(t, map[string]bool{"spam_scam": true, "hate_harassment": false}, verdict.Present)
			assert.True(t, classificationReviewT0.Equal(verdict.RecordedAt), "recorded at %v, want the clock %v", verdict.RecordedAt, classificationReviewT0)
			stored := r.verdictRows(t, currentID)
			require.Len(t, stored, 1)
			assert.JSONEq(t, `{"spam_scam": true, "hate_harassment": false}`, stored[reviewAdminZ].present)
			assert.True(t, classificationReviewT0.Equal(stored[reviewAdminZ].recordedAt))

			rerecordedAt := classificationReviewT0.Add(time.Hour)
			r.clock.Set(rerecordedAt)
			_, err = r.service.RecordVerdict(ctx, classification.VerdictRequest{
				ResultID: currentID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": false, "hate_harassment": true},
			})
			require.NoError(t, err)
			stored = r.verdictRows(t, currentID)
			require.Len(t, stored, 1, "a re-record by the same admin replaces the verdict")
			assert.JSONEq(t, `{"spam_scam": false, "hate_harassment": true}`, stored[reviewAdminZ].present)
			assert.True(t, rerecordedAt.Equal(stored[reviewAdminZ].recordedAt), "recorded at %v, want %v", stored[reviewAdminZ].recordedAt, rerecordedAt)

			_, err = r.service.RecordVerdict(ctx, classification.VerdictRequest{
				ResultID: currentID, AdminDID: reviewAdminA, Present: map[string]bool{"spam_scam": true, "hate_harassment": true},
			})
			require.NoError(t, err)
			stored = r.verdictRows(t, currentID)
			require.Len(t, stored, 2, "a second admin's verdict is a separate row")
			assert.JSONEq(t, `{"spam_scam": false, "hate_harassment": true}`, stored[reviewAdminZ].present)
			assert.JSONEq(t, `{"spam_scam": true, "hate_harassment": true}`, stored[reviewAdminA].present)
		})
	}

	t.Run("soft-deleted post", func(t *testing.T) {
		r := newClassificationReview(t)
		communityDID := visibilityCommunity(t, r.db, "clsverdictdel")
		createTestUser(t, r.db, "clsverdictauthor.test", "did:plc:clsverdictauthor")
		post := seedReviewPost(t, r.db, communityDID, "did:plc:clsverdictauthor", "3lclsvrddel01", "Deleted verdict tern")
		resultID := seedClassificationResult(t, r.db, post.result("deleted", map[string]float64{"spam_scam": 0.8},
			time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)))
		softDeleteReviewPost(t, r.db, post.uri)

		_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
			ResultID: resultID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true},
		})
		require.ErrorIs(t, err, classification.ErrUnreviewable, "a soft-deleted subject is not reviewable")
		assert.Empty(t, r.verdictRows(t, resultID), "a refused verdict stores nothing")
	})

	t.Run("soft-deleted comment", func(t *testing.T) {
		r := newClassificationReview(t)
		communityDID := visibilityCommunity(t, r.db, "clsverdictdelcmt")
		createTestUser(t, r.db, "clsverdictauthor.test", "did:plc:clsverdictauthor")
		root := seedReviewPost(t, r.db, communityDID, "did:plc:clsverdictauthor", "3lclsvrddrt01", "Deleted verdict root")
		comment := seedReviewComment(t, r.db, root, "did:plc:clsverdictauthor", "3lclsvrddcm01", "Deleted verdict reply")
		resultID := seedClassificationResult(t, r.db, comment.result("deleted-comment", map[string]float64{"spam_scam": 0.8},
			time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)))
		softDeleteReviewComment(t, r.db, comment.uri)

		_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
			ResultID: resultID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true},
		})
		require.ErrorIs(t, err, classification.ErrUnreviewable, "a soft-deleted comment at the scored CID is not reviewable")
		assert.Empty(t, r.verdictRows(t, resultID), "a refused verdict stores nothing")
	})

	t.Run("unknown result", func(t *testing.T) {
		r := newClassificationReview(t)
		_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
			ResultID: 999999999, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true},
		})
		require.ErrorIs(t, err, classification.ErrResultNotFound)
	})

	t.Run("author erasure removes the result and its verdicts", func(t *testing.T) {
		r := newClassificationReview(t)
		communityDID := visibilityCommunity(t, r.db, "clsverdicterase")
		erasedDID := "did:plc:clsverdicterased"
		createTestUser(t, r.db, "clsverdicterased.test", erasedDID)
		post := seedReviewPost(t, r.db, communityDID, erasedDID, "3lclsvrderas1", "Erased verdict gannet")
		resultID := seedClassificationResult(t, r.db, post.result("erased", map[string]float64{"spam_scam": 0.8},
			time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)))
		for _, adminDID := range []string{reviewAdminZ, reviewAdminA} {
			_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
				ResultID: resultID, AdminDID: adminDID, Present: map[string]bool{"spam_scam": true},
			})
			require.NoError(t, err)
		}
		require.Equal(t, 2, r.count(t, `SELECT count(*) FROM classification_verdicts WHERE result_id = $1`, resultID),
			"both admins' verdicts are stored before erasure")

		eraseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		require.NoError(t, NewUserRepository(r.db).Delete(eraseCtx, erasedDID))

		assert.Equal(t, 0, r.count(t, `SELECT count(*) FROM classification_results WHERE id = $1`, resultID))
		assert.Equal(t, 0, r.count(t, `SELECT count(*) FROM classification_verdicts WHERE result_id = $1`, resultID))
	})
}

// TestClassificationReviewVerdictWaitsForConcurrentEdit: a verdict locks the
// subject row before comparing CIDs, so a verdict started while an edit is
// uncommitted waits for it and is then refused against the edited CID (B11).
func TestClassificationReviewVerdictWaitsForConcurrentEdit(t *testing.T) {
	ctx := context.Background()
	r := newClassificationReview(t)
	communityDID := visibilityCommunity(t, r.db, "clsverdictrace")
	createTestUser(t, r.db, "clsverdictrace.test", "did:plc:clsverdictrace")
	post := seedReviewPost(t, r.db, communityDID, "did:plc:clsverdictrace", "3lclsvrdrace1", "Racing verdict gannet")
	resultID := seedClassificationResult(t, r.db, post.result("race", map[string]float64{"spam_scam": 0.8, "hate_harassment": 0.1},
		time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)))

	edit, err := r.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	committed := false
	t.Cleanup(func() {
		if !committed {
			assert.NoError(t, edit.Rollback())
		}
	})
	var editPID int
	require.NoError(t, edit.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&editPID))
	updated, err := edit.ExecContext(ctx, `UPDATE posts SET cid = $2 WHERE uri = $1`, post.uri, "bafyclsverdictraceedited")
	require.NoError(t, err)
	affected, err := updated.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "the edit moves the post's CID")

	verdictCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	verdictDone := make(chan error, 1)
	go func() {
		_, err := r.service.RecordVerdict(verdictCtx, classification.VerdictRequest{
			ResultID: resultID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true, "hate_harassment": false},
		})
		verdictDone <- err
	}()

	testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
		select {
		case err := <-verdictDone:
			return false, fmt.Errorf("the verdict returned (%v) while the edit was uncommitted", err)
		default:
		}
		var waiting bool
		err := r.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND $1 = ANY(pg_blocking_pids(pid))
			)
		`, editPID).Scan(&waiting)
		return waiting, err
	}, testkit.WithDescription("the verdict's backend waiting on the uncommitted edit's row lock"))
	select {
	case err := <-verdictDone:
		require.Failf(t, "the verdict returned while the edit was uncommitted", "error: %v", err)
	default:
	}

	require.NoError(t, edit.Commit())
	committed = true

	select {
	case err := <-verdictDone:
		require.ErrorIs(t, err, classification.ErrUnreviewable, "the verdict compares against the committed edit's CID")
	case <-verdictCtx.Done():
		require.Fail(t, "the verdict did not return after the edit committed")
	}
	assert.Empty(t, r.verdictRows(t, resultID), "a refused verdict stores nothing")
}

// TestClassificationReviewVerdictWaitsForErasure: a verdict takes the shared
// erasure lock on the result's author before locking the result or its
// subject, so a verdict started while the author's erasure holds the subject
// post's row lock waits for the erasure rather than deadlocking with its
// delete of the result, and then finds the result gone (B7, R2).
func TestClassificationReviewVerdictWaitsForErasure(t *testing.T) {
	ctx := context.Background()
	r := newClassificationReview(t)
	communityDID := visibilityCommunity(t, r.db, "clsverdicterasure")
	erasedDID := "did:plc:clsverdicterasurerace"
	createTestUser(t, r.db, "clsverdicterasurerace.test", erasedDID)
	post := seedReviewPost(t, r.db, communityDID, erasedDID, "3lclsvrderas2", "Erasure race gannet")
	resultID := seedClassificationResult(t, r.db, post.result("erasure-race", map[string]float64{"spam_scam": 0.8},
		time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)))
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind) VALUES ($1, $2, $3, 'post_image')
	`, erasedDID, reviewThumbCID, post.uri)
	require.NoError(t, err)

	// The blocker holds the author's blob reference row, which the erasure
	// locks after it has locked the author's posts and before it deletes the
	// author's classification results, so the erasure pauses holding the
	// subject post's row lock and the exclusive erasure lock.
	blocker, err := r.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	released := false
	t.Cleanup(func() {
		if !released {
			assert.NoError(t, blocker.Rollback())
		}
	})
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.ExecContext(ctx, `SELECT 1 FROM blob_references WHERE owner_did = $1 FOR UPDATE`, erasedDID)
	require.NoError(t, err)

	operationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	erasureDone := make(chan error, 1)
	go func() { erasureDone <- NewUserRepository(r.db).Delete(operationCtx, erasedDID) }()

	// blockedBy returns the backend waiting on a lock that holderPID holds and
	// the kind of lock it waits on.
	// It polls on the blocker's connection, as the erasure and the verdict
	// hold the rest of the test pool, clearing the transaction's cached
	// activity snapshot before each read.
	blockedBy := func(holderPID int, description string) (int, string) {
		t.Helper()
		var (
			waiterPID int
			waitEvent string
		)
		testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
			if _, err := blocker.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
				return false, err
			}
			err := blocker.QueryRowContext(ctx, `
				SELECT pid, wait_event FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND $1 = ANY(pg_blocking_pids(pid))
				LIMIT 1
			`, holderPID).Scan(&waiterPID, &waitEvent)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		}, testkit.WithDescription("%s", description))
		return waiterPID, waitEvent
	}
	erasurePID, _ := blockedBy(blockerPID, "the erasure waiting on the blocker's blob reference lock")

	verdictDone := make(chan error, 1)
	go func() {
		_, err := r.service.RecordVerdict(operationCtx, classification.VerdictRequest{
			ResultID: resultID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true},
		})
		verdictDone <- err
	}()
	_, verdictWaitEvent := blockedBy(erasurePID, "the verdict waiting on a lock the paused erasure holds")
	assert.Equal(t, "advisory", verdictWaitEvent, "the verdict waits on the erasure lock, not on a row the erasure holds")
	select {
	case err := <-verdictDone:
		require.Failf(t, "the verdict returned while the erasure was paused", "error: %v", err)
	default:
	}

	require.NoError(t, blocker.Rollback())
	released = true

	select {
	case err := <-erasureDone:
		require.NoError(t, err, "the erasure completes without a deadlock")
	case <-operationCtx.Done():
		require.Fail(t, "the erasure did not return after the blocker released")
	}
	select {
	case err := <-verdictDone:
		require.ErrorIs(t, err, classification.ErrResultNotFound, "the verdict finds the erased result gone")
	case <-operationCtx.Done():
		require.Fail(t, "the verdict did not return after the erasure committed")
	}
	assert.Equal(t, 0, r.count(t, `SELECT count(*) FROM classification_results WHERE id = $1`, resultID))
	assert.Equal(t, 0, r.count(t, `SELECT count(*) FROM classification_verdicts WHERE result_id = $1`, resultID))
}

// TestClassificationReviewPrecision: precision counts only the evaluation's
// verdicted reviewable results, takes each result's latest verdict across
// admins whichever admin DID is greater, and reports its unreviewable results
// (edited posts, comments and communities, deleted posts) separately (B8, R2).
func TestClassificationReviewPrecision(t *testing.T) {
	ctx := context.Background()
	r := newClassificationReview(t)
	db := r.db
	communityDID := visibilityCommunity(t, db, "clsprecision")
	authorDID := "did:plc:clsprecisionauthor"
	createTestUser(t, db, "clsprecisionauthor.test", authorDID)

	post1 := seedReviewPost(t, db, communityDID, authorDID, "3lclsprcpost1", "Precision post one")
	post2 := seedReviewPost(t, db, communityDID, authorDID, "3lclsprcpost2", "Precision post two")
	post3 := seedReviewPost(t, db, communityDID, authorDID, "3lclsprcpost3", "Precision post three")
	post4 := seedReviewPost(t, db, communityDID, authorDID, "3lclsprcpost4", "Precision post four")
	post5 := seedReviewPost(t, db, communityDID, authorDID, "3lclsprcpost5", "Precision post five")
	post6 := seedReviewPost(t, db, communityDID, authorDID, "3lclsprcpost6", "Precision post six")
	comment := seedReviewComment(t, db, post1, authorDID, "3lclsprccmt01", "Precision reply")
	editedComment := seedReviewComment(t, db, post1, authorDID, "3lclsprccmt02", "Precision reply to edit")
	community := seedReviewCommunity(t, db, "precision", authorDID, "Tern colony", "Tern Watch", "A community about terns")
	editedCommunity := seedReviewCommunity(t, db, "precisionedit", authorDID, "Skua colony", "Skua Watch", "A community about skuas")

	createdAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	seed := func(subject reviewSubject, label, modelID, policyVersion string, spam, hate float64) int64 {
		resultSeed := subject.result(label, map[string]float64{"spam_scam": spam, "hate_harassment": hate}, createdAt)
		resultSeed.modelID, resultSeed.policyVersion = modelID, policyVersion
		return seedClassificationResult(t, db, resultSeed)
	}
	e1 := seed(post1, "e1-post", reviewModelOne, "v3", 0.9, 0.2)
	e2 := seed(comment, "e2-comment", reviewModelOne, "v3", 0.6, 0.7)
	e3 := seed(community, "e3-community", reviewModelOne, "v3", 0.5, 0.1)
	e4 := seed(post2, "e4-two-admins", reviewModelOne, "v3", 0.8, 0.6)
	seed(post3, "e5-unverdicted", reviewModelOne, "v3", 0.95, 0.95)
	e6 := seed(post4, "e6-edited", reviewModelOne, "v3", 0.9, 0.9)
	e7 := seed(post5, "e7-deleted", reviewModelOne, "v3", 0.1, 0.9)
	e8 := seed(post6, "e8-greater-admin-later", reviewModelOne, "v3", 0.7, 0.8)
	e9 := seed(editedComment, "e9-edited-comment", reviewModelOne, "v3", 0.9, 0.9)
	e10 := seed(editedCommunity, "e10-edited-community", reviewModelOne, "v3", 0.9, 0.9)
	otherModel := seed(post1, "decoy-model", reviewModelTwo, "v3", 0.9, 0.9)
	otherPolicy := seed(post1, "decoy-policy", reviewModelOne, "v0", 0.9, 0.9)

	record := func(resultID int64, adminDID string, at time.Time, spam, hate bool) {
		t.Helper()
		r.clock.Set(at)
		_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
			ResultID: resultID, AdminDID: adminDID, Present: map[string]bool{"spam_scam": spam, "hate_harassment": hate},
		})
		require.NoErrorf(t, err, "recording a verdict on result %d", resultID)
	}
	record(e1, reviewAdminZ, classificationReviewT0, true, false)
	record(e2, reviewAdminZ, classificationReviewT0, false, true)
	record(e3, reviewAdminZ, classificationReviewT0, true, true)
	record(e4, reviewAdminZ, classificationReviewT0, false, false)
	record(e4, reviewAdminA, classificationReviewT0.Add(time.Hour), true, true)
	record(e6, reviewAdminZ, classificationReviewT0, true, true)
	record(e7, reviewAdminZ, classificationReviewT0, false, false)
	// On e8 the greater admin DID records later, the reverse of e4, so the
	// latest verdict is not the least admin DID either.
	record(e8, reviewAdminA, classificationReviewT0, false, true)
	record(e8, reviewAdminZ, classificationReviewT0.Add(time.Hour), true, false)
	record(e9, reviewAdminZ, classificationReviewT0, true, true)
	record(e10, reviewAdminZ, classificationReviewT0, true, true)
	record(otherModel, reviewAdminZ, classificationReviewT0, false, false)
	record(otherPolicy, reviewAdminZ, classificationReviewT0, false, false)
	editReviewSubject(t, db, post4, "bafyclsprecisionedited")
	softDeleteReviewPost(t, db, post5.uri)
	editReviewSubject(t, db, editedComment, "bafyclsprecisioncommentedited")
	editReviewSubject(t, db, editedCommunity, "bafyclsprecisioncommunityedited")

	report, err := r.service.Precision(ctx, classification.PrecisionRequest{ModelID: reviewModelOne, PolicyVersion: "v3", Threshold: 0.5})
	require.NoError(t, err)
	assert.Equal(t, reviewModelOne, report.ModelID)
	assert.Equal(t, "v3", report.PolicyVersion)
	assert.InDelta(t, 0.5, report.Threshold, 1e-9)
	assert.Equal(t, 5, report.VerdictedCount, "e1-e4 and e8 are verdicted and reviewable")
	assert.Equal(t, 4, report.UnreviewableCount,
		"e6 (edited post), e7 (deleted post), e9 (edited comment) and e10 (edited community) are unreviewable")

	ratio := func(value float64) *float64 { return &value }
	type questionRow struct {
		question                         string
		atOrAbove, present, truePositive int
		precision, recall                *float64
	}
	// spam_scam: e1 0.9 T, e2 0.6 F, e3 0.5 T (at the threshold), e4 0.8 T
	// (admin A's later verdict), e8 0.7 T (admin Z's later verdict).
	// hate_harassment: e1 0.2 F, e2 0.7 T, e3 0.1 T, e4 0.6 T, e8 0.8 F.
	want := []questionRow{
		{question: "graphic_violence"},
		{question: "hate_harassment", atOrAbove: 3, present: 3, truePositive: 2, precision: ratio(2.0 / 3.0), recall: ratio(2.0 / 3.0)},
		{question: "sexual_explicit"},
		{question: "sexual_minors"},
		{question: "sexual_suggestive"},
		{question: "spam_scam", atOrAbove: 5, present: 4, truePositive: 4, precision: ratio(0.8), recall: ratio(1)},
	}
	require.Len(t, report.Questions, len(want), "the current policy's six questions")
	for index, wantRow := range want {
		got := report.Questions[index]
		assert.Equal(t, wantRow.question, got.Question, "row %d", index)
		assert.Equal(t, wantRow.atOrAbove, got.AtOrAbove, "%s at or above", wantRow.question)
		assert.Equal(t, wantRow.present, got.Present, "%s present", wantRow.question)
		assert.Equal(t, wantRow.truePositive, got.TruePositive, "%s true positives", wantRow.question)
		for _, pair := range []struct {
			name      string
			want, got *float64
		}{{"precision", wantRow.precision, got.Precision}, {"recall", wantRow.recall, got.Recall}} {
			if pair.want == nil {
				assert.Nil(t, pair.got, "%s %s is undefined", wantRow.question, pair.name)
				continue
			}
			if assert.NotNil(t, pair.got, "%s %s is defined", wantRow.question, pair.name) {
				assert.InDelta(t, *pair.want, *pair.got, 1e-6, "%s %s", wantRow.question, pair.name)
			}
		}
	}
}

// TestClassificationReviewPartialResultIsListedAndVerdictable: a partial
// result is listed with partial set, accepts a verdict like any result while
// its subject is at the scored CID, and counts toward precision (R6).
func TestClassificationReviewPartialResultIsListedAndVerdictable(t *testing.T) {
	ctx := context.Background()
	r := newClassificationReview(t)
	db := r.db
	communityDID := visibilityCommunity(t, db, "clspartial")
	authorDID := "did:plc:clspartialauthor"
	createTestUser(t, db, "clspartialauthor.test", authorDID)
	partialPost := seedReviewPost(t, db, communityDID, authorDID, "3lclsprtpost1", "Partial post")
	completePost := seedReviewPost(t, db, communityDID, authorDID, "3lclsprtpost2", "Complete post")

	createdAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	partialSeed := partialPost.result("partial", map[string]float64{"spam_scam": 0.9}, createdAt)
	partialSeed.partial = true
	partialID := seedClassificationResult(t, db, partialSeed)
	completeID := seedClassificationResult(t, db, completePost.result("complete", map[string]float64{"spam_scam": 0.2}, createdAt))

	views := r.listByID(t)
	assert.True(t, views[partialID].Partial, "the partial result is listed as partial")
	assert.False(t, views[completeID].Partial)

	r.clock.Set(classificationReviewT0)
	_, err := r.service.RecordVerdict(ctx, classification.VerdictRequest{
		ResultID: partialID, AdminDID: reviewAdminZ, Present: map[string]bool{"spam_scam": true},
	})
	require.NoError(t, err, "a partial result accepts a verdict")

	report, err := r.service.Precision(ctx, classification.PrecisionRequest{ModelID: reviewModelOne, PolicyVersion: "v1", Threshold: 0.5})
	require.NoError(t, err)
	assert.Equal(t, 1, report.VerdictedCount, "the verdicted partial result counts")
	for _, row := range report.Questions {
		if row.Question == "spam_scam" {
			assert.Equal(t, 1, row.TruePositive)
		}
	}
}
