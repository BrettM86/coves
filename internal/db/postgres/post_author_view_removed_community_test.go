//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Author X's own view of X's post in community C carries C's avatar. Once the
// instance removes C, X still reads the post, with status "removed", but no
// longer receives the removed community's avatar.
func TestPostRepository_AuthorViewDropsRemovedCommunityAvatar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	label := testkit.UniqueID(t)
	community := visibilityCommunity(t, db, label)
	_, err := db.ExecContext(ctx, `UPDATE communities SET avatar_cid = 'bafkreiauthorviewavatar', pds_url = 'https://pds.authorview.test'
		WHERE did = $1`, community)
	require.NoError(t, err)
	author := "did:plc:authorview" + label
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "X's post in C", time.Now().Add(-time.Hour))
	seedVisibilityAdmission(t, db, community, post, posts.AdmissionStatusAccepted, "", "")
	repository := NewPostRepository(db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	authorView := func(t *testing.T) *posts.PostView {
		t.Helper()
		views, _, err := repository.GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: author, ViewerDID: author, Limit: 10})
		require.NoError(t, err)
		require.Len(t, views, 1, "X reads X's own post")
		require.Equal(t, post, views[0].URI)
		require.NotNil(t, views[0].Community)
		return views[0]
	}

	t.Run("before removal", func(t *testing.T) {
		view := authorView(t)
		assert.Equal(t, string(posts.AdmissionStatusAccepted), view.Status)
		if assert.NotNil(t, view.Community.Avatar, "the live community's avatar is served") {
			assert.Equal(t, "https://pds.authorview.test/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Avis"+label+"community&cid=bafkreiauthorviewavatar",
				*view.Community.Avatar)
		}
	})
	removeNotificationCommunity(t, &unreadVisibilityFixture{db: db}, community)
	t.Run("while the community is removed", func(t *testing.T) {
		view := authorView(t)
		assert.Equal(t, string(posts.AdmissionStatusRemoved), view.Status, "X still reads the post, as removed")
		assert.Nil(t, view.Community.Avatar, "the removed community's avatar is not served")
	})
}

// Author X's post in community C, removed by C with code "spam" and a reason.
// X's own view of the post, on each read path that serves it (profile list,
// post.get, the getComments thread header), carries C's code and reason. Any
// other viewer, signed in or not, receives no view of the post at all, so the
// reason never reaches them.
func TestPostRepository_AuthorViewCarriesTheCommunityRemovalReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	label := testkit.UniqueID(t)
	community := visibilityCommunity(t, db, label)
	author := "did:plc:removalreason" + label
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "X's post C removed", time.Now().Add(-time.Hour))
	result, err := NewAdmissionRepository(db).ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: community, PostURI: post, DecisionCode: "spam",
		Reason:    "Posting the same link in every thread",
		Watermark: posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: C's removal stands")
	// X's second post, which C removed with code "spam" and no reason.
	reasonlessPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "X's post C removed without a reason", time.Now().Add(-2*time.Hour))
	result, err = NewAdmissionRepository(db).ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: community, PostURI: reasonlessPost, DecisionCode: "spam",
		Watermark: posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: C's reason-less removal stands")
	// X's third post, which C rejected: decision_code holds the rejection code,
	// which is not a removal.
	rejectedPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "X's post C rejected", time.Now().Add(-3*time.Hour))
	seedVisibilityAdmission(t, db, community, rejectedPost, posts.AdmissionStatusRejected, "", "rate-limit-exceeded")
	repository := NewPostRepository(db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))

	readPaths := map[string]func(t *testing.T, post, viewerDID string) *posts.PostView{
		"profile list (GetByAuthor)": func(t *testing.T, post, viewerDID string) *posts.PostView {
			views, _, err := repository.GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: author, ViewerDID: viewerDID, Limit: 10})
			require.NoError(t, err)
			for _, view := range views {
				if view.URI == post {
					return view
				}
			}
			return nil
		},
		"post.get (GetViewsByURIs)": func(t *testing.T, post, viewerDID string) *posts.PostView {
			views, err := repository.GetViewsByURIs(ctx, []string{post}, viewerDID)
			require.NoError(t, err)
			return views[post]
		},
		"getComments header (VisibleHeaderView)": func(t *testing.T, post, viewerDID string) *posts.PostView {
			view, err := repository.VisibleHeaderView(ctx, post, viewerDID)
			require.NoError(t, err)
			return view
		},
	}
	reason := &posts.CommunityRemoval{Code: "spam", Reason: "Posting the same link in every thread"}
	other := "did:plc:removalreasonother" + label
	removed := string(posts.AdmissionStatusRemoved)
	tests := []struct {
		name       string
		readPath   string
		post       string
		viewerDID  string
		wantStatus string                  // empty: the viewer receives no view of the post
		want       *posts.CommunityRemoval // the author's view's communityRemoval
	}{
		{"author on the profile list", "profile list (GetByAuthor)", post, author, removed, reason},
		{"author on post.get", "post.get (GetViewsByURIs)", post, author, removed, reason},
		{"author on the getComments header", "getComments header (VisibleHeaderView)", post, author, removed, reason},
		{"another viewer on the profile list", "profile list (GetByAuthor)", post, other, "", nil},
		{"another viewer on post.get", "post.get (GetViewsByURIs)", post, other, "", nil},
		{"signed-out viewer on the profile list", "profile list (GetByAuthor)", post, "", "", nil},
		{"signed-out viewer on post.get", "post.get (GetViewsByURIs)", post, "", "", nil},
		{"author on the profile list, reason-less removal", "profile list (GetByAuthor)", reasonlessPost, author, removed, &posts.CommunityRemoval{Code: "spam"}},
		{"author on post.get, reason-less removal", "post.get (GetViewsByURIs)", reasonlessPost, author, removed, &posts.CommunityRemoval{Code: "spam"}},
		{"author on the profile list, rejected post", "profile list (GetByAuthor)", rejectedPost, author, string(posts.AdmissionStatusRejected), nil},
		{"author on post.get, rejected post", "post.get (GetViewsByURIs)", rejectedPost, author, string(posts.AdmissionStatusRejected), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := readPaths[tt.readPath](t, tt.post, tt.viewerDID)
			if tt.wantStatus == "" {
				assert.Nil(t, view, "only the author can see C's removed post")
				return
			}
			require.NotNil(t, view, "X reads X's own post")
			assert.Equal(t, tt.wantStatus, view.Status)
			assert.Equal(t, tt.want, view.CommunityRemoval)
		})
	}
}

// Author X's post in community C, removed by C with code "spam" and a reason.
// Once the instance removes C, X still reads the post with status "removed",
// but without C's code and reason: the post reads as removed because C is
// gone, not because of C's decision.
func TestPostRepository_AuthorViewDropsRemovalReasonInRemovedCommunity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	label := testkit.UniqueID(t)
	community := visibilityCommunity(t, db, label)
	author := "did:plc:removedreason" + label
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "X's post C removed", time.Now().Add(-time.Hour))
	result, err := NewAdmissionRepository(db).ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: community, PostURI: post, DecisionCode: "spam",
		Reason:    "Posting the same link in every thread",
		Watermark: posts.CommunityWatermark{Rev: testkit.TID(), OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: C's removal stands")
	repository := NewPostRepository(db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	authorView := func(t *testing.T) *posts.PostView {
		t.Helper()
		views, _, err := repository.GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: author, ViewerDID: author, Limit: 10})
		require.NoError(t, err)
		require.Len(t, views, 1, "X reads X's own post")
		require.Equal(t, post, views[0].URI)
		return views[0]
	}

	t.Run("before the community is removed", func(t *testing.T) {
		view := authorView(t)
		assert.Equal(t, string(posts.AdmissionStatusRemoved), view.Status)
		assert.Equal(t, &posts.CommunityRemoval{Code: "spam", Reason: "Posting the same link in every thread"}, view.CommunityRemoval,
			"fixture: C's removal reason is served while C is live")
	})
	removeNotificationCommunity(t, &unreadVisibilityFixture{db: db}, community)
	t.Run("while the community is removed", func(t *testing.T) {
		view := authorView(t)
		assert.Equal(t, string(posts.AdmissionStatusRemoved), view.Status, "X still reads the post, as removed")
		assert.Nil(t, view.CommunityRemoval, "the removed community's code and reason are not served")
	})
}
