//go:build integration

package postgres_test

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// likedSubjectsBase is the fixed instant every liked-subjects fixture times its
// votes, posts and comments against.
var likedSubjectsBase = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// likedSubjectsFixture is one community, one author and one voter in a fresh
// database, read through a comment repository bound to the configured instance
// authority.
type likedSubjectsFixture struct {
	db           *sql.DB
	communityDID string
	authorDID    string
	voterDID     string
	repository   comments.Repository
}

func newLikedSubjectsFixture(t *testing.T) *likedSubjectsFixture {
	t.Helper()
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	return &likedSubjectsFixture{
		db:           db,
		communityDID: communityDID,
		authorDID:    authorDID,
		voterDID:     likedSubjectsUser(t, db, "liker"),
		repository:   postgres.NewCommentRepository(db, postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID())),
	}
}

// likedSubjectsUser indexes a user and returns its DID.
func likedSubjectsUser(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	name := testkit.UniqueIDWithPrefix(t, prefix)
	did := fixtures.DID(name)
	fixtures.User(t, db, name+".test", did)
	return did
}

// post indexes a postv2 by authorDID with record key rkey, accepted by
// communityDID, created at createdAt.
func (f *likedSubjectsFixture) post(t *testing.T, communityDID, authorDID, rkey string, createdAt time.Time) moderation.StrongRef {
	t.Helper()
	subject := indexedModerationPostWithRecordKey(t, f.db, moderation.PostV2Collection, communityDID, authorDID, "liked post "+rkey, "", rkey)
	_, err := f.db.ExecContext(t.Context(), `UPDATE posts SET created_at = $2 WHERE uri = $1`, subject.URI, createdAt)
	require.NoError(t, err)
	return subject
}

// pendingPost is post in the fixture's community with the community's decision
// still pending, so only its author may see it.
func (f *likedSubjectsFixture) pendingPost(t *testing.T, authorDID, rkey string, createdAt time.Time) moderation.StrongRef {
	t.Helper()
	subject := f.post(t, f.communityDID, authorDID, rkey, createdAt)
	_, err := f.db.ExecContext(t.Context(), `
		UPDATE community_post_admissions
		SET status = 'pending', accepted_cid = NULL, evaluated_cid = NULL,
			last_community_rev = NULL, last_community_op_rank = NULL
		WHERE community_did = $1 AND post_uri = $2
	`, f.communityDID, subject.URI)
	require.NoError(t, err)
	return subject
}

// comment indexes a comment by commenterDID with record key rkey, declaring
// root as its thread root and replying to parent, created at createdAt.
func (f *likedSubjectsFixture) comment(t *testing.T, commenterDID, rkey string, root, parent moderation.StrongRef, createdAt time.Time) moderation.StrongRef {
	t.Helper()
	subject := moderation.StrongRef{
		URI: "at://" + commenterDID + "/" + moderation.CommentCollection + "/" + rkey,
		CID: moderationCommentCID,
	}
	_, err := f.db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'a liked comment', $9)
	`, subject.URI, subject.CID, rkey, commenterDID, root.URI, root.CID, parent.URI, parent.CID, createdAt)
	require.NoError(t, err)
	return subject
}

// vote indexes voterDID's vote with record key rkey on subjectURI, cast at
// createdAt, and returns the vote's URI.
func (f *likedSubjectsFixture) vote(t *testing.T, voterDID, rkey, subjectURI, direction string, createdAt time.Time) string {
	t.Helper()
	uri := "at://" + voterDID + "/social.coves.feed.vote/" + rkey
	_, err := f.db.ExecContext(t.Context(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
	`, uri, "bafyreilikevote"+rkey, rkey, voterDID, subjectURI, moderationPostCID, direction, createdAt)
	require.NoError(t, err)
	return uri
}

// exec runs a fixture statement.
func (f *likedSubjectsFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := f.db.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

// removeComment applies an instance moderation removal to an indexed comment.
func (f *likedSubjectsFixture) removeComment(t *testing.T, subject moderation.StrongRef) {
	t.Helper()
	result, err := newPostgresModerationService(f.db).RemoveContent(t.Context(), fixtures.DID("commentremovaladmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-liked-comment",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err, "an indexed comment must be readable inside the moderation transaction")
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
}

// removedCommunity indexes a second community and removes it at the instance,
// returning its DID.
func (f *likedSubjectsFixture) removedCommunity(t *testing.T) string {
	t.Helper()
	name := testkit.UniqueIDWithPrefix(t, "removedplace")
	communityDID, err := fixtures.Community(t.Context(), f.db, name, "owner"+name)
	require.NoError(t, err)
	communityremovalfixture.New(f.db).Remove(t, communityDID)
	return communityDID
}

// likedPages reads the voter's likes as the voter under filter, limit per
// page, following each returned cursor until none is returned, and returns
// every page in order.
func (f *likedSubjectsFixture) likedPages(t *testing.T, filter comments.LikesFilter, limit int) [][]comments.LikedSubjectRef {
	t.Helper()
	var pages [][]comments.LikedSubjectRef
	var cursor *string
	for range 10 {
		refs, next, err := f.repository.GetLikedSubjectRefs(t.Context(), comments.GetActorLikesRequest{
			ActorDID: f.voterDID, ViewerDID: f.voterDID, Filter: filter, Limit: limit, Cursor: cursor,
		})
		require.NoError(t, err)
		pages = append(pages, refs)
		if next == nil {
			return pages
		}
		cursor = next
	}
	t.Fatalf("likes did not finish within 10 pages of %d; read so far: %v", limit, pages)
	return nil
}

func likedPostRef(subject moderation.StrongRef) comments.LikedSubjectRef {
	return comments.LikedSubjectRef{URI: subject.URI, Kind: comments.LikedSubjectPost}
}

// likedCommentRef is the ref of a liked comment on root whose parent chain
// reaches a post in communityDID.
func likedCommentRef(subject, root moderation.StrongRef, communityDID string) comments.LikedSubjectRef {
	return comments.LikedSubjectRef{URI: subject.URI, Kind: comments.LikedSubjectComment, RootURI: root.URI, CommunityDID: communityDID}
}

// TestCommentRepository_GetLikedSubjectRefsListsOnlyActiveUpvotes: with filter
// all, the voter's likes are its own active upvotes whose subject is an
// indexed, undeleted post or comment; a vote hidden by a community ban still
// counts. Each row adds one candidate vote, cast after the voter's upvote on a
// baseline post, which is always listed.
func TestCommentRepository_GetLikedSubjectRefsListsOnlyActiveUpvotes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// seed indexes the candidate and returns the ref it would be listed
		// under, or nil when it must not be listed.
		seed func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) *comments.LikedSubjectRef
	}{
		{
			name: "another voter's upvote on a post",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.post(t, f.communityDID, f.authorDID, "3kcandidate22", likedSubjectsBase.Add(-48*time.Hour))
				f.vote(t, likedSubjectsUser(t, f.db, "otherliker"), "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "another voter's upvote on a comment",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, likedSubjectsBase.Add(-48*time.Hour))
				f.vote(t, likedSubjectsUser(t, f.db, "otherliker"), "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "downvote on a post",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.post(t, f.communityDID, f.authorDID, "3kcandidate22", likedSubjectsBase.Add(-48*time.Hour))
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "down", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "downvote on a comment",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, likedSubjectsBase.Add(-48*time.Hour))
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "down", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "soft-deleted upvote",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, likedSubjectsBase.Add(-48*time.Hour))
				vote := f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				f.exec(t, `UPDATE votes SET deleted_at = $2 WHERE uri = $1`, vote, likedSubjectsBase.Add(2*time.Hour))
				return nil
			},
		},
		{
			name: "upvote on a soft-deleted post",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.post(t, f.communityDID, f.authorDID, "3kcandidate22", likedSubjectsBase.Add(-48*time.Hour))
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				f.exec(t, `UPDATE posts SET deleted_at = $2 WHERE uri = $1`, candidate.URI, likedSubjectsBase.Add(2*time.Hour))
				return nil
			},
		},
		{
			name: "upvote on a soft-deleted comment",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, likedSubjectsBase.Add(-48*time.Hour))
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				f.exec(t, `UPDATE comments SET deleted_at = $2 WHERE uri = $1`, candidate.URI, likedSubjectsBase.Add(2*time.Hour))
				return nil
			},
		},
		{
			name: "upvote on a subject indexed nowhere",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) *comments.LikedSubjectRef {
				f.vote(t, f.voterDID, "3kcandvote222", "at://"+f.authorDID+"/"+moderation.CommentCollection+"/3knotindexed2", "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "post upvote hidden by a community ban",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.post(t, f.communityDID, f.authorDID, "3kcandidate22", likedSubjectsBase.Add(-48*time.Hour))
				vote := f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				f.exec(t, `UPDATE votes SET hide_reason = 'community_ban' WHERE uri = $1`, vote)
				ref := likedPostRef(candidate)
				return &ref
			},
		},
		{
			name: "comment upvote hidden by a community ban",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) *comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, likedSubjectsBase.Add(-48*time.Hour))
				vote := f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				f.exec(t, `UPDATE votes SET hide_reason = 'community_ban' WHERE uri = $1`, vote)
				ref := likedCommentRef(candidate, baseline, f.communityDID)
				return &ref
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLikedSubjectsFixture(t)
			baseline := f.post(t, f.communityDID, f.authorDID, "3kbaseline222", likedSubjectsBase.Add(-72*time.Hour))
			f.vote(t, f.voterDID, "3kbasevote222", baseline.URI, "up", likedSubjectsBase)
			candidate := tt.seed(t, f, baseline)

			want := []comments.LikedSubjectRef{likedPostRef(baseline)}
			if candidate != nil {
				want = []comments.LikedSubjectRef{*candidate, likedPostRef(baseline)}
			}
			assert.Equal(t, [][]comments.LikedSubjectRef{want}, f.likedPages(t, comments.LikesFilterAll, 50))
		})
	}
}

// TestCommentRepository_GetLikedSubjectRefsFiltersByKind: filter posts lists
// only liked posts, filter comments only liked comments, and filter all both,
// in vote order. A liked nested reply carries its thread's root post, not its
// parent comment, as RootURI, and the community its parent chain reaches as
// CommunityDID; a liked post carries neither.
func TestCommentRepository_GetLikedSubjectRefsFiltersByKind(t *testing.T) {
	t.Parallel()
	f := newLikedSubjectsFixture(t)
	root := f.post(t, f.communityDID, f.authorDID, "3kpost1111112", likedSubjectsBase.Add(-72*time.Hour))
	topLevel := f.comment(t, f.authorDID, "3kcomm1111112", root, root, likedSubjectsBase.Add(-48*time.Hour))
	nestedReply := f.comment(t, f.authorDID, "3kcomm2222222", root, topLevel, likedSubjectsBase.Add(-24*time.Hour))
	otherPost := f.post(t, f.communityDID, f.authorDID, "3kpost2222222", likedSubjectsBase.Add(-72*time.Hour))
	f.vote(t, f.voterDID, "3kvote4444442", nestedReply.URI, "up", likedSubjectsBase)
	f.vote(t, f.voterDID, "3kvote3333332", root.URI, "up", likedSubjectsBase.Add(-1*time.Hour))
	f.vote(t, f.voterDID, "3kvote2222222", topLevel.URI, "up", likedSubjectsBase.Add(-2*time.Hour))
	f.vote(t, f.voterDID, "3kvote1111112", otherPost.URI, "up", likedSubjectsBase.Add(-3*time.Hour))

	tests := []struct {
		filter comments.LikesFilter
		want   []comments.LikedSubjectRef
	}{
		{comments.LikesFilterAll, []comments.LikedSubjectRef{
			likedCommentRef(nestedReply, root, f.communityDID), likedPostRef(root), likedCommentRef(topLevel, root, f.communityDID), likedPostRef(otherPost),
		}},
		{comments.LikesFilterPosts, []comments.LikedSubjectRef{likedPostRef(root), likedPostRef(otherPost)}},
		{comments.LikesFilterComments, []comments.LikedSubjectRef{likedCommentRef(nestedReply, root, f.communityDID), likedCommentRef(topLevel, root, f.communityDID)}},
	}
	for _, tt := range tests {
		t.Run(string(tt.filter), func(t *testing.T) {
			assert.Equal(t, [][]comments.LikedSubjectRef{tt.want}, f.likedPages(t, tt.filter, 50))
		})
	}
}

// TestCommentRepository_GetLikedSubjectRefsAppliesVisibility: a liked subject
// the viewer may not see is left out of the page itself. Each row adds one
// candidate liked subject, voted after the voter's upvote on a visible
// baseline post that is the candidate comments' thread root.
func TestCommentRepository_GetLikedSubjectRefsAppliesVisibility(t *testing.T) {
	t.Parallel()
	candidateCreated := likedSubjectsBase.Add(-48 * time.Hour)
	tests := []struct {
		name string
		// seed indexes the candidate, votes for it, and returns the refs listed
		// ahead of the baseline, newest vote first, or nil when nothing is.
		seed func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef
	}{
		{
			name: "post the instance removed",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.post(t, f.communityDID, f.authorDID, "3kcandidate22", candidateCreated)
				removeIndexedModerationPostWithKey(t, newPostgresModerationService(f.db), candidate, "remove-liked-post")
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "another author's post pending in its community",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.pendingPost(t, f.authorDID, "3kcandidate22", candidateCreated)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "the viewer's own pending post is listed",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.pendingPost(t, f.voterDID, "3kcandidate22", candidateCreated)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return []comments.LikedSubjectRef{likedPostRef(candidate)}
			},
		},
		{
			name: "post by an author banned under the configured authority",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				bannedAuthorDID := likedSubjectsUser(t, f.db, "bannedauthor")
				candidate := f.post(t, f.communityDID, bannedAuthorDID, "3kcandidate22", candidateCreated)
				accountbanfixture.New(f.db).Ban(t, bannedAuthorDID, nil)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "comment the instance removed",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, candidateCreated)
				f.removeComment(t, candidate)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "another author's comment hidden by a community ban",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, candidateCreated)
				f.exec(t, `UPDATE comments SET hide_reason = 'community_ban' WHERE uri = $1`, candidate.URI)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "comment by a commenter banned under the configured authority",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef {
				bannedCommenterDID := likedSubjectsUser(t, f.db, "bannedcommenter")
				candidate := f.comment(t, bannedCommenterDID, "3kcomment2222", baseline, baseline, candidateCreated)
				accountbanfixture.New(f.db).Ban(t, bannedCommenterDID, nil)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "the viewer's own comment pending a ban check is listed",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.comment(t, f.voterDID, "3kcomment2222", baseline, baseline, candidateCreated)
				f.exec(t, `UPDATE comments SET hide_reason = 'pending_ban_check' WHERE uri = $1`, candidate.URI)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return []comments.LikedSubjectRef{likedCommentRef(candidate, baseline, f.communityDID)}
			},
		},
		{
			name: "comment whose root post the instance removed",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				root := f.post(t, f.communityDID, f.authorDID, "3kcandroot222", candidateCreated)
				candidate := f.comment(t, f.authorDID, "3kcomment2222", root, root, candidateCreated)
				removeIndexedModerationPostWithKey(t, newPostgresModerationService(f.db), root, "remove-liked-root")
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "comment whose root post is another author's pending post",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				root := f.pendingPost(t, f.authorDID, "3kcandroot222", candidateCreated)
				candidate := f.comment(t, f.authorDID, "3kcomment2222", root, root, candidateCreated)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "comment whose root post is soft-deleted",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				root := f.post(t, f.communityDID, f.authorDID, "3kcandroot222", candidateCreated)
				candidate := f.comment(t, f.authorDID, "3kcomment2222", root, root, candidateCreated)
				f.exec(t, `UPDATE posts SET deleted_at = $2 WHERE uri = $1`, root.URI, likedSubjectsBase)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "comment by another member, under the viewer's own post, in a community the instance removed",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				removedCommunityDID := f.removedCommunity(t)
				root := f.post(t, removedCommunityDID, f.voterDID, "3kcandroot222", candidateCreated)
				candidate := f.comment(t, f.authorDID, "3kcomment2222", root, root, candidateCreated)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(2*time.Hour))
				f.vote(t, f.voterDID, "3krootvote222", root.URI, "up", likedSubjectsBase.Add(time.Hour))
				// The viewer's own post in the removed community stays listed
				// to the viewer; the other member's comment under it does not.
				return []comments.LikedSubjectRef{likedPostRef(root)}
			},
		},
		{
			name: "the viewer's own comment, under the viewer's own post, in a community the instance removed is listed",
			seed: func(t *testing.T, f *likedSubjectsFixture, _ moderation.StrongRef) []comments.LikedSubjectRef {
				removedCommunityDID := f.removedCommunity(t)
				root := f.post(t, removedCommunityDID, f.voterDID, "3kcandroot222", candidateCreated)
				candidate := f.comment(t, f.voterDID, "3kcomment2222", root, root, candidateCreated)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(2*time.Hour))
				f.vote(t, f.voterDID, "3krootvote222", root.URI, "up", likedSubjectsBase.Add(time.Hour))
				return []comments.LikedSubjectRef{likedCommentRef(candidate, root, removedCommunityDID), likedPostRef(root)}
			},
		},
		{
			name: "comment its community removed",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef {
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, baseline, candidateCreated)
				outcome, err := postgres.NewCommunityCommentRemovalRepository(f.db).RecordRemoval(t.Context(), moderation.CommunityCommentRemoval{
					RepositoryDID: f.communityDID, RecordKey: "3kremoval2222", RecordCID: "bafyreilikedcommentremoval",
					Rev: testkit.TID(), SubjectURI: candidate.URI, SubjectCID: candidate.CID, Code: "spam",
				})
				require.NoError(t, err)
				require.Equal(t, moderation.CommunityCommentRemovalApplied, outcome, "fixture: the community's removal is active")
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
		{
			name: "comment whose parent chain reaches another community than its declared root",
			seed: func(t *testing.T, f *likedSubjectsFixture, baseline moderation.StrongRef) []comments.LikedSubjectRef {
				otherCommunityName := testkit.UniqueIDWithPrefix(t, "otherplace")
				otherCommunityDID, err := fixtures.Community(t.Context(), f.db, otherCommunityName, "owner"+otherCommunityName)
				require.NoError(t, err)
				otherParent := f.post(t, otherCommunityDID, f.authorDID, "3kotherpost22", candidateCreated)
				candidate := f.comment(t, f.authorDID, "3kcomment2222", baseline, otherParent, candidateCreated)
				f.vote(t, f.voterDID, "3kcandvote222", candidate.URI, "up", likedSubjectsBase.Add(time.Hour))
				return nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLikedSubjectsFixture(t)
			baseline := f.post(t, f.communityDID, f.authorDID, "3kbaseline222", likedSubjectsBase.Add(-72*time.Hour))
			f.vote(t, f.voterDID, "3kbasevote222", baseline.URI, "up", likedSubjectsBase)
			want := append(tt.seed(t, f, baseline), likedPostRef(baseline))
			assert.Equal(t, [][]comments.LikedSubjectRef{want}, f.likedPages(t, comments.LikesFilterAll, 50))
		})
	}
}

// TestCommentRepository_GetLikedSubjectRefsPagesStayFull: hidden subjects are
// left out of the page query itself, so a page of two holds two visible
// subjects even when hidden ones sit between them in vote order.
func TestCommentRepository_GetLikedSubjectRefsPagesStayFull(t *testing.T) {
	t.Parallel()
	f := newLikedSubjectsFixture(t)
	visiblePost := f.post(t, f.communityDID, f.authorDID, "3kpost1111112", likedSubjectsBase.Add(-72*time.Hour))
	othersPending := f.pendingPost(t, f.authorDID, "3kpost2222222", likedSubjectsBase.Add(-72*time.Hour))
	hiddenComment := f.comment(t, f.authorDID, "3kcomm1111112", visiblePost, visiblePost, likedSubjectsBase.Add(-48*time.Hour))
	f.exec(t, `UPDATE comments SET hide_reason = 'community_ban' WHERE uri = $1`, hiddenComment.URI)
	visibleComment := f.comment(t, f.authorDID, "3kcomm2222222", visiblePost, visiblePost, likedSubjectsBase.Add(-48*time.Hour))
	f.vote(t, f.voterDID, "3kvote4444442", visiblePost.URI, "up", likedSubjectsBase)
	f.vote(t, f.voterDID, "3kvote3333332", othersPending.URI, "up", likedSubjectsBase.Add(-1*time.Hour))
	f.vote(t, f.voterDID, "3kvote2222222", hiddenComment.URI, "up", likedSubjectsBase.Add(-2*time.Hour))
	f.vote(t, f.voterDID, "3kvote1111112", visibleComment.URI, "up", likedSubjectsBase.Add(-3*time.Hour))

	assert.Equal(t, [][]comments.LikedSubjectRef{
		{likedPostRef(visiblePost), likedCommentRef(visibleComment, visiblePost, f.communityDID)},
	}, f.likedPages(t, comments.LikesFilterAll, 2))
}

// TestCommentRepository_GetLikedSubjectRefsOrdersByVoteTime: liked posts and
// comments are ordered together by vote time, newest first, then by vote URI
// descending, and following the returned cursors visits each liked subject
// exactly once. Subject creation times run opposite to vote times, and the two
// votes cast at the same instant are on comments whose URIs order opposite to
// the votes' URIs.
func TestCommentRepository_GetLikedSubjectRefsOrdersByVoteTime(t *testing.T) {
	t.Parallel()
	f := newLikedSubjectsFixture(t)
	newestVote := f.post(t, f.communityDID, f.authorDID, "3kpost1111112", likedSubjectsBase.Add(-6*24*time.Hour))
	tieFirst := f.comment(t, f.authorDID, "3kcomm2222222", newestVote, newestVote, likedSubjectsBase.Add(-4*24*time.Hour))
	tieSecond := f.comment(t, f.authorDID, "3kcomm3333332", newestVote, newestVote, likedSubjectsBase.Add(-3*24*time.Hour))
	older := f.post(t, f.communityDID, f.authorDID, "3kpost4444442", likedSubjectsBase.Add(-2*24*time.Hour))
	oldestVote := f.comment(t, f.authorDID, "3kcomm5555552", newestVote, tieFirst, likedSubjectsBase.Add(-1*24*time.Hour))
	f.vote(t, f.voterDID, "3kvote5555552", newestVote.URI, "up", likedSubjectsBase)
	f.vote(t, f.voterDID, "3kvote4444442", tieFirst.URI, "up", likedSubjectsBase.Add(-time.Hour))
	f.vote(t, f.voterDID, "3kvote3333332", tieSecond.URI, "up", likedSubjectsBase.Add(-time.Hour))
	f.vote(t, f.voterDID, "3kvote2222222", older.URI, "up", likedSubjectsBase.Add(-2*time.Hour))
	f.vote(t, f.voterDID, "3kvote1111112", oldestVote.URI, "up", likedSubjectsBase.Add(-3*time.Hour))

	newestRef := likedPostRef(newestVote)
	tieFirstRef := likedCommentRef(tieFirst, newestVote, f.communityDID)
	tieSecondRef := likedCommentRef(tieSecond, newestVote, f.communityDID)
	olderRef := likedPostRef(older)
	oldestRef := likedCommentRef(oldestVote, newestVote, f.communityDID)
	tests := []struct {
		name  string
		limit int
		want  [][]comments.LikedSubjectRef
	}{
		{"one page", 50, [][]comments.LikedSubjectRef{{newestRef, tieFirstRef, tieSecondRef, olderRef, oldestRef}}},
		{"pages of two, the tie split across a page boundary", 2, [][]comments.LikedSubjectRef{
			{newestRef, tieFirstRef}, {tieSecondRef, olderRef}, {oldestRef},
		}},
		{"pages of one", 1, [][]comments.LikedSubjectRef{
			{newestRef}, {tieFirstRef}, {tieSecondRef}, {olderRef}, {oldestRef},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, f.likedPages(t, comments.LikesFilterAll, tt.limit))
		})
	}
}

// TestCommentRepository_GetLikedSubjectRefsRejectsAMalformedCursor: a cursor
// the repository did not issue is refused as comments.ErrInvalidCursor.
func TestCommentRepository_GetLikedSubjectRefsRejectsAMalformedCursor(t *testing.T) {
	t.Parallel()
	f := newLikedSubjectsFixture(t)
	liked := f.post(t, f.communityDID, f.authorDID, "3kpost1111112", likedSubjectsBase.Add(-24*time.Hour))
	f.vote(t, f.voterDID, "3kvote1111112", liked.URI, "up", likedSubjectsBase)

	for name, cursor := range map[string]string{
		"not base64":               "not a cursor!",
		"base64 of unrelated text": base64.URLEncoding.EncodeToString([]byte("unrelated text")),
	} {
		t.Run(name, func(t *testing.T) {
			refs, next, err := f.repository.GetLikedSubjectRefs(t.Context(), comments.GetActorLikesRequest{
				ActorDID: f.voterDID, ViewerDID: f.voterDID, Filter: comments.LikesFilterAll, Limit: 50, Cursor: &cursor,
			})
			assert.Truef(t, errors.Is(err, comments.ErrInvalidCursor), "want comments.ErrInvalidCursor, got %v", err)
			assert.Nil(t, refs)
			assert.Nil(t, next)
		})
	}
}

// TestCommentRepository_GetLikedSubjectRefsRejectsAnUnknownFilter: a filter
// other than all, posts or comments is refused rather than read as all.
func TestCommentRepository_GetLikedSubjectRefsRejectsAnUnknownFilter(t *testing.T) {
	t.Parallel()
	f := newLikedSubjectsFixture(t)
	liked := f.post(t, f.communityDID, f.authorDID, "3kpost1111112", likedSubjectsBase.Add(-24*time.Hour))
	f.vote(t, f.voterDID, "3kvote1111112", liked.URI, "up", likedSubjectsBase)

	refs, next, err := f.repository.GetLikedSubjectRefs(t.Context(), comments.GetActorLikesRequest{
		ActorDID: f.voterDID, ViewerDID: f.voterDID, Filter: comments.LikesFilter("bogus"), Limit: 50,
	})
	assert.Error(t, err)
	assert.Nil(t, refs)
	assert.Nil(t, next)
}
