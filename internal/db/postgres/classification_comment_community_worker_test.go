//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/atproto/embedblobs"
	"Coves/internal/classifier/cloudflare/cloudflaretest"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/classification"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Comment and community subjects through the classification worker and the
// Postgres queue (docs/PRD_CONTENT_CLASSIFICATION.md, chunk 03).

const classificationCommentCollection = "social.coves.community.comment"

// seedClassificationComment inserts one comment by commenterDID replying to
// parentURI in the thread declared by rootURI, with CID "bafycmt"+rkey, and
// returns its URI.
func seedClassificationComment(t *testing.T, db *sql.DB, commenterDID, rkey, rootURI, parentURI, body string) string {
	t.Helper()
	uri := "at://" + commenterDID + "/" + classificationCommentCollection + "/" + rkey
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, 'bafyclsroot', $6, 'bafyclsparent', $7, $8)
	`, uri, "bafycmt"+rkey, rkey, commenterDID, rootURI, parentURI, body, classificationT0.Add(-30*time.Minute))
	require.NoErrorf(t, err, "seeding comment %s", rkey)
	return uri
}

// seedClassificationCommentWithEmbed is seedClassificationComment carrying
// embedJSON as the comment's embed.
func seedClassificationCommentWithEmbed(t *testing.T, db *sql.DB, commenterDID, rkey, rootURI, parentURI, body, embedJSON string) string {
	t.Helper()
	uri := seedClassificationComment(t, db, commenterDID, rkey, rootURI, parentURI, body)
	_, err := db.ExecContext(context.Background(), `UPDATE comments SET embed = $2 WHERE uri = $1`, uri, embedJSON)
	require.NoErrorf(t, err, "setting the embed of comment %s", rkey)
	return uri
}

// seedClassificationCommunity inserts a community created by creatorDID with
// its profile text and record CID.
func seedClassificationCommunity(t *testing.T, db *sql.DB, did, handle, creatorDID, name, displayName, description, recordCID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO communities (did, handle, name, display_name, description, owner_did, created_by_did, hosted_by_did, record_cid, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $6, $7, NOW())
	`, did, handle, name, displayName, description, creatorDID, recordCID)
	require.NoErrorf(t, err, "seeding community %s", did)
}

// setClassificationCommunityImages sets a seeded community's avatar and banner
// CIDs and writes their blob references as the community repository does:
// owned by the community, under its profile subject.
func setClassificationCommunityImages(t *testing.T, db *sql.DB, did, avatarCID, bannerCID string) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `UPDATE communities SET avatar_cid = $2, banner_cid = $3 WHERE did = $1`, did, avatarCID, bannerCID)
	require.NoErrorf(t, err, "setting the images of community %s", did)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	if err := NewBlobReferenceRepository(db).ReplaceSubjectTx(ctx, tx, did, communityProfileSubjectURI(did),
		embedblobs.ProfileReferences(avatarCID, bannerCID)); err != nil {
		_ = tx.Rollback()
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// seedCommunityCommentRemoval writes an active community-scope removal of
// commentURI whose authority is communityDID.
func seedCommunityCommentRemoval(t *testing.T, db *sql.DB, communityDID, commentURI string) {
	t.Helper()
	actionID := "cls-community-comment-removal-" + testkit.UniqueID(t)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, subject_collection, action, origin, created_at)
		VALUES ($1, 'did:plc:clscommunitymoderator', $2, 'community', $2, $3, $4, 'remove', 'local', NOW())
	`, actionID, communityDID, commentURI, classificationCommentCollection)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'community', $1, $2, 'removal', NULL, $3, TRUE)
	`, communityDID, commentURI, actionID)
	require.NoError(t, err)
}

// deleteClassificationCommunity deletes communityDID through the community
// repository, which keeps its posts' comments and records the posts as deleted
// roots while the posts cascade away.
func deleteClassificationCommunity(t *testing.T, db *sql.DB, communityDID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, NewCommunityRepository(db, credentialciphertest.Fixed()).Delete(ctx, communityDID))
}

// runReporting runs worker once at at, reporting a run error and going on so
// the job states the run left are asserted too.
func (f *classificationFixture) runReporting(t *testing.T, worker *classification.Worker, at time.Time) {
	t.Helper()
	f.clock.Set(at)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	assert.NoError(t, worker.RunOnce(ctx))
}

// classificationThread is a visible root post in the fixture's community and
// the commenter of the subject comment.
type classificationThread struct {
	rootURI    string
	rootAuthor string
	commenter  string
}

// TestClassificationWorkerClosesHiddenCommentsAndCommunities: a comment or
// community job whose subject an anonymous viewer cannot see under the
// instance authority is closed not_visible with no request carrying its text,
// while a visible comment and a visible community in the same run are
// classified (V1).
func TestClassificationWorkerClosesHiddenCommentsAndCommunities(t *testing.T) {
	const (
		subjectBody          = "Hidden walrus reply"
		subjectRkey          = "3lclscmtsubj1"
		hiddenMarker         = "walrus"
		controlBody          = "Control harbor seal reply"
		controlCommunityText = "Control otters\n\nOtter Control Room\n\nA visible community about river otters"
	)
	// comment seeds the subject comment by the thread's commenter, replying
	// to parentURI under rootURI, and enqueues it.
	comment := func(t *testing.T, f *classificationFixture, s classificationThread, rootURI, parentURI string) string {
		t.Helper()
		uri := seedClassificationComment(t, f.db, s.commenter, subjectRkey, rootURI, parentURI, subjectBody)
		f.enqueueComment(t, uri, classificationT0)
		return uri
	}
	for _, row := range []struct {
		name string
		// seed seeds and enqueues the hidden subject and returns its URI.
		seed func(t *testing.T, f *classificationFixture, s classificationThread) string
	}{
		{
			name: "comment soft-deleted",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				_, err := f.db.ExecContext(t.Context(), `UPDATE comments SET deleted_at = NOW() WHERE uri = $1`, uri)
				require.NoError(t, err)
				return uri
			},
		},
		{
			name: "comment instance-removed",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				seedInstanceRemovalDecision(t, f.db, classificationInstanceDID, uri,
					sql.NullString{String: classificationCommentCollection, Valid: true}, sql.NullString{}, "social.coves.moderation.defs#reasonSpam")
				return uri
			},
		},
		{
			name: "comment removed by its community",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				seedCommunityCommentRemoval(t, f.db, f.community, uri)
				return uri
			},
		},
		{
			name: "under a removed root post",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				removedRoot := f.visiblePost(t, f.community, s.rootAuthor, "3lclsremroot1", "Removed root heron")
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, removedRoot, "social.coves.moderation.defs#reasonSpam")
				return comment(t, f, s, removedRoot, removedRoot)
			},
		},
		{
			name: "under a removed parent comment",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				parent := seedClassificationComment(t, f.db, s.rootAuthor, "3lclsremparnt", s.rootURI, s.rootURI, "Removed parent puffin")
				seedInstanceRemovalDecision(t, f.db, classificationInstanceDID, parent,
					sql.NullString{String: classificationCommentCollection, Valid: true}, sql.NullString{}, "social.coves.moderation.defs#reasonSpam")
				return comment(t, f, s, s.rootURI, parent)
			},
		},
		{
			name: "author account ban in force",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				seedAccountBan(t, f.db, classificationInstanceDID, s.commenter)
				return uri
			},
		},
		{
			name: "author PDS account suspended",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				accountstatusfixture.Set(t, f.db, s.commenter, accountstatus.Suspended)
				return uri
			},
		},
		{
			name: "account_ban hide after the ban ended",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				actionID := seedAccountBan(t, f.db, classificationInstanceDID, s.commenter)
				_, err := f.db.ExecContext(t.Context(), `UPDATE comments SET hide_reason = 'account_ban' WHERE uri = $1`, uri)
				require.NoError(t, err)
				_, err = f.db.ExecContext(t.Context(), `UPDATE moderation_actions SET expires_at = NOW() - interval '1 minute' WHERE id = $1`, actionID)
				require.NoError(t, err)
				return uri
			},
		},
		{
			name: "community_ban hide with no removal decision",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				uri := comment(t, f, s, s.rootURI, s.rootURI)
				_, err := f.db.ExecContext(t.Context(), `UPDATE comments SET hide_reason = 'community_ban' WHERE uri = $1`, uri)
				require.NoError(t, err)
				return uri
			},
		},
		{
			name: "comment in a removed community",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				removedCommunity := visibilityCommunity(t, f.db, "clsremoved")
				root := f.visiblePost(t, removedCommunity, s.rootAuthor, "3lclsremcomm1", "Removed community heron")
				seedCommunityRemoval(t, f.db, classificationInstanceDID, removedCommunity)
				return comment(t, f, s, root, root)
			},
		},
		{
			name: "parent chain reaches a removed post while the declared root is visible",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				removedPost := f.visiblePost(t, f.community, s.rootAuthor, "3lclschainb01", "Removed chain heron")
				seedPostInstanceRemoval(t, f.db, classificationInstanceDID, removedPost, "social.coves.moderation.defs#reasonSpam")
				bridge := seedClassificationComment(t, f.db, s.rootAuthor, "3lclschainp01", removedPost, removedPost, "Bridge puffin")
				return comment(t, f, s, s.rootURI, bridge)
			},
		},
		{
			name: "parent chain reaches a visible post in another community than the visible declared root",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				otherCommunity := visibilityCommunity(t, f.db, "clsforked")
				otherPost := f.visiblePost(t, otherCommunity, s.rootAuthor, "3lclsforkpost", "Forked community heron")
				return comment(t, f, s, s.rootURI, otherPost)
			},
		},
		{
			name: "declared root's community deleted",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				deletedCommunity := visibilityCommunity(t, f.db, "clsdeleted")
				root := f.visiblePost(t, deletedCommunity, s.rootAuthor, "3lclsdelroot1", "Deleted community heron")
				uri := comment(t, f, s, root, root)
				deleteClassificationCommunity(t, f.db, deletedCommunity)
				return uri
			},
		},
		{
			name: "parent chain reaches a deleted community's post while the declared root is visible",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				deletedCommunity := visibilityCommunity(t, f.db, "clsdelchain")
				chainPost := f.visiblePost(t, deletedCommunity, s.rootAuthor, "3lclsdelchain", "Deleted chain heron")
				uri := comment(t, f, s, s.rootURI, chainPost)
				deleteClassificationCommunity(t, f.db, deletedCommunity)
				return uri
			},
		},
		{
			name: "declared root's community deleted while the parent chain reaches a visible post",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				deletedCommunity := visibilityCommunity(t, f.db, "clsdelroot2")
				root := f.visiblePost(t, deletedCommunity, s.rootAuthor, "3lclsdelroot2", "Deleted root heron")
				uri := comment(t, f, s, root, s.rootURI)
				deleteClassificationCommunity(t, f.db, deletedCommunity)
				return uri
			},
		},
		{
			name: "declared root post's admission refused",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				refusedRoot := seedVisibilityPost(t, f.db, f.community, s.rootAuthor, "3lclsrefroot1", "Refused root heron", classificationT0.Add(-time.Hour))
				seedVisibilityAdmission(t, f.db, f.community, refusedRoot, posts.AdmissionStatusRejected, "", "")
				return comment(t, f, s, refusedRoot, refusedRoot)
			},
		},
		{
			name: "community instance-removed",
			seed: func(t *testing.T, f *classificationFixture, s classificationThread) string {
				const did = "did:plc:clsremovedsubject"
				seedClassificationCommunity(t, f.db, did, "clsremovedsubject.coves.social", s.rootAuthor,
					"Walrus haul-out", "Walrus Watch", "A removed walrus community", "bafyclsremovedsubject")
				seedCommunityRemoval(t, f.db, classificationInstanceDID, did)
				f.enqueueCommunity(t, did, "bafyclsremovedsubject", classificationT0)
				return did
			},
		},
		{
			name: "community deleted",
			seed: func(t *testing.T, f *classificationFixture, _ classificationThread) string {
				const did = "did:plc:clsgonesubject"
				f.enqueueCommunity(t, did, "bafyclsgonesubject", classificationT0)
				return did
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
			worker := f.worker(t, nil)

			rootAuthor := f.author(t, "clsrootauthor")
			thread := classificationThread{
				rootURI:    f.visiblePost(t, f.community, rootAuthor, "3lclsroot0001", "Visible root heron"),
				rootAuthor: rootAuthor,
				commenter:  f.author(t, "clscommenter"),
			}

			controlComment := seedClassificationComment(t, f.db, f.author(t, "clscmtcontrol"), "3lclscmtctl01", thread.rootURI, thread.rootURI, controlBody)
			f.enqueueComment(t, controlComment, classificationT0)
			const controlCommunity = "did:plc:clsctlcommunity"
			seedClassificationCommunity(t, f.db, controlCommunity, "clsctl.coves.social", f.author(t, "clsctlowner"),
				"Control otters", "Otter Control Room", "A visible community about river otters", "bafyclsctlcommunity")
			f.enqueueCommunity(t, controlCommunity, "bafyclsctlcommunity", classificationT0)

			subjectURI := row.seed(t, f, thread)
			f.runReporting(t, worker, classificationT0)

			closed := f.job(t, subjectURI)
			assert.Equal(t, "closed", closed.Status)
			assert.Equal(t, "not_visible", closed.CloseReason.String)
			assert.Equal(t, 0, closed.AttemptCount)
			assert.Equal(t, 0, f.requestsContaining(hiddenMarker), "no request carries the hidden subject's text")

			assert.Equal(t, "classified", f.job(t, controlComment).Status, "the visible comment in the same run is classified")
			assert.Equal(t, "classified", f.job(t, controlCommunity).Status, "the visible community in the same run is classified")
			assert.ElementsMatch(t, []string{controlBody, controlCommunityText}, f.states(t), "only the visible comment's and community's text is sent")
		})
	}
}

// TestClassificationCommentAndCommunityErasure: erasing a commenter deletes
// that commenter's comment jobs and results and leaves another commenter's;
// erasing a community's creator leaves the community's job and result, whose
// author is the community (E1, Q5).
func TestClassificationCommentAndCommunityErasure(t *testing.T) {
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
			name: "commenter erased",
			run: func(t *testing.T, f *classificationFixture, worker *classification.Worker) {
				const erasedDID, keptDID = "did:plc:clserased", "did:plc:clskept"
				root := f.visiblePost(t, f.community, f.author(t, "clsrootauthor"), "3lclsroot0001", "Visible root heron")
				f.author(t, "clserased")
				f.author(t, "clskept")
				classified := seedClassificationComment(t, f.db, erasedDID, "3lclsercmt001", root, root, "Erased gannet reply")
				queued := seedClassificationComment(t, f.db, erasedDID, "3lclsercmt002", root, root, "Erased gannet second reply")
				kept := seedClassificationComment(t, f.db, keptDID, "3lclskeptcmt1", root, root, "Kept cormorant reply")
				f.enqueueComment(t, classified, classificationT0)
				f.enqueueComment(t, queued, classificationT0.Add(time.Hour))
				f.enqueueComment(t, kept, classificationT0.Add(time.Hour))
				f.runReporting(t, worker, classificationT0)
				require.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, classified),
					"the erased commenter's first comment is classified before erasure")
				require.Equal(t, "queued", f.job(t, queued).Status, "the erased commenter's second comment is still queued")

				require.NoError(t, deleteAccount(t, f.db, erasedDID))

				assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_jobs WHERE author_did = $1`, erasedDID))
				assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM classification_results WHERE author_did = $1`, erasedDID))
				assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_jobs WHERE subject_uri = $1`, kept),
					"another commenter's comment job remains")
			},
		},
		{
			name: "community creator erased",
			run: func(t *testing.T, f *classificationFixture, worker *classification.Worker) {
				const communityDID = "did:plc:clserasedcreatorcommunity"
				creatorDID := f.author(t, "clscreator")
				seedClassificationCommunity(t, f.db, communityDID, "clserasedcreator.coves.social", creatorDID,
					"Gannet colony", "Gannet Watch", "A community about gannets", "bafyclserasedcreator")
				f.enqueueCommunity(t, communityDID, "bafyclserasedcreator", classificationT0)
				f.runReporting(t, worker, classificationT0)
				require.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1`, communityDID),
					"the community is classified before its creator is erased")

				require.NoError(t, deleteAccount(t, f.db, creatorDID))

				assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_jobs WHERE subject_uri = $1`, communityDID),
					"the community's job remains")
				assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM classification_results WHERE subject_uri = $1 AND author_did = $1`, communityDID),
					"the community's result, authored by the community, remains")
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newClassificationFixture(t, cloudflaretest.Canned(classificationCanned, classificationCannedInputTokens))
			row.run(t, f, f.worker(t, nil))
		})
	}
}
