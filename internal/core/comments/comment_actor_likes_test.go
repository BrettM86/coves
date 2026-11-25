package comments

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	likesViewerDID      = "did:plc:likesviewer"
	likesCommenterDID   = "did:plc:likescommenter"
	likesPostAuthorDID  = "did:plc:likespostauthor"
	likesBlockedDID     = "did:plc:likesblocked"
	likesRootA          = "at://did:plc:likespostauthor/social.coves.community.postv2/roota"
	likesRootB          = "at://did:plc:likespostauthor/social.coves.community.postv2/rootb"
	likesRootX          = "at://did:plc:likespostauthor/social.coves.community.postv2/rootx"
	likesCommentA       = "at://did:plc:likescommenter/social.coves.community.comment/a"
	likesParentB        = "at://did:plc:likescommenter/social.coves.community.comment/parentb"
	likesCommentB       = "at://did:plc:likescommenter/social.coves.community.comment/b"
	likesCommentX       = "at://did:plc:likescommenter/social.coves.community.comment/x"
	likesPostP1         = "at://did:plc:likespostauthor/social.coves.community.postv2/p1"
	likesPostP2         = "at://did:plc:likespostauthor/social.coves.community.postv2/p2"
	likesNextPageCursor = "next-page"
	likesPageCommunity  = "did:plc:likespagecommunity"
)

// likesAuthorBlocks is a posts.BlockChecker answering from a fixed set of
// blocked author DIDs.
type likesAuthorBlocks struct {
	blocked map[string]bool
	err     error
}

func (b *likesAuthorBlocks) AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error) {
	if b.err != nil {
		return nil, b.err
	}
	result := map[string]bool{}
	for _, did := range blockedDIDs {
		if b.blocked[did] {
			result[did] = true
		}
	}
	return result, nil
}

// likedCommentsFixture is, unless a row replaces page, a comments-only page
// of three comments in the order B, X, A, which is neither URI order nor
// insertion order:
//   - B is a NESTED reply (its parent is comment parentB) in the thread of rootB;
//   - X is a top-level comment on rootX, the comment each drop row changes;
//   - A is a top-level comment on rootA.
//
// Liked posts P1 and P2 are also indexed for the mixed-page rows. Every
// comment and post is indexed and visible, so with no change the service
// serves the whole page.
type likedCommentsFixture struct {
	commentRepo *mockCommentRepo
	postRepo    *mockPostRepo
	blocks      *likesAuthorBlocks
	filter      LikesFilter
	page        []LikedSubjectRef
	pageRequest *GetActorLikesRequest
	// withoutBlockChecker builds the service with no block checker installed.
	withoutBlockChecker bool
}

// likedPostsPage is a posts-only page of liked posts P1 and P2.
var likedPostsPage = []LikedSubjectRef{
	{URI: likesPostP1, Kind: LikedSubjectPost},
	{URI: likesPostP2, Kind: LikedSubjectPost},
}

// mixedLikesPage is a page of liked post P1, liked nested reply B, liked post P2.
var mixedLikesPage = []LikedSubjectRef{
	{URI: likesPostP1, Kind: LikedSubjectPost},
	{URI: likesCommentB, Kind: LikedSubjectComment, RootURI: likesRootB},
	{URI: likesPostP2, Kind: LikedSubjectPost},
}

func newLikedCommentsFixture() *likedCommentsFixture {
	f := &likedCommentsFixture{
		commentRepo: newMockCommentRepo(),
		postRepo:    newMockPostRepo(),
		blocks:      &likesAuthorBlocks{},
		filter:      LikesFilterComments,
		page: []LikedSubjectRef{
			{URI: likesCommentB, Kind: LikedSubjectComment, RootURI: likesRootB},
			{URI: likesCommentX, Kind: LikedSubjectComment, RootURI: likesRootX},
			{URI: likesCommentA, Kind: LikedSubjectComment, RootURI: likesRootA},
		},
	}
	for _, comment := range []*Comment{
		createTestComment(likesCommentA, likesCommenterDID, "commenter.test", likesRootA, likesRootA, 0),
		createTestComment(likesParentB, likesCommenterDID, "commenter.test", likesRootB, likesRootB, 1),
		createTestComment(likesCommentB, likesCommenterDID, "commenter.test", likesRootB, likesParentB, 0),
		createTestComment(likesCommentX, likesCommenterDID, "commenter.test", likesRootX, likesRootX, 0),
	} {
		f.commentRepo.comments[comment.URI] = comment
	}
	f.postRepo.views = map[string]*posts.PostView{}
	for _, uri := range []string{likesRootA, likesRootB, likesRootX, likesPostP1, likesPostP2} {
		f.postRepo.views[uri] = &posts.PostView{URI: uri, Author: &posts.AuthorView{DID: likesPostAuthorDID}}
	}
	f.commentRepo.getLikedSubjectRefsFunc = func(ctx context.Context, req GetActorLikesRequest) ([]LikedSubjectRef, *string, error) {
		captured := req
		f.pageRequest = &captured
		cursor := likesNextPageCursor
		return f.page, &cursor, nil
	}
	return f
}

func (f *likedCommentsFixture) service() Service {
	userRepo := newMockUserRepo()
	userRepo.users[likesCommenterDID] = createTestUser(likesCommenterDID, "commenter.test")
	if f.withoutBlockChecker {
		return NewCommentService(f.commentRepo, userRepo, f.postRepo, newMockCommunityRepo(), nil, nil, nil)
	}
	return NewCommentService(f.commentRepo, userRepo, f.postRepo, newMockCommunityRepo(), nil, nil, nil,
		WithBlockChecker(f.blocks))
}

func (f *likedCommentsFixture) get(t *testing.T, limit int) (*ActorLikesResponse, error) {
	t.Helper()
	cursor := "this-page"
	return f.service().GetActorLikes(context.Background(), &GetActorLikesRequest{
		ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: f.filter, Limit: limit, Cursor: &cursor,
	})
}

// likedPairs renders a served liked comment as "comment URI @ post URI" and
// a served liked post as "post URI".
func likedPairs(response *ActorLikesResponse) []string {
	pairs := make([]string, 0, len(response.Feed))
	for _, item := range response.Feed {
		var postURI string
		if item.Post != nil {
			postURI = item.Post.URI
		}
		if item.Comment == nil {
			pairs = append(pairs, "post "+postURI)
			continue
		}
		pairs = append(pairs, item.Comment.URI+" @ "+postURI)
	}
	return pairs
}

// TestGetActorLikes_ServesPageOrderAndDropsUnservableItems proves the service
// keeps the page's vote order across posts and comments, pairs each comment
// (a nested reply too) with its thread's ROOT post, drops an item it cannot
// serve whole, and returns the page cursor unchanged when it drops one.
func TestGetActorLikes_ServesPageOrderAndDropsUnservableItems(t *testing.T) {
	withoutX := []string{likesCommentB + " @ " + likesRootB, likesCommentA + " @ " + likesRootA}
	mixedWithoutP2 := []string{"post " + likesPostP1, likesCommentB + " @ " + likesRootB}
	tests := []struct {
		name   string
		change func(f *likedCommentsFixture)
		want   []string
		// alsoAssert, when set, checks the served response further.
		alsoAssert func(t *testing.T, response *ActorLikesResponse)
	}{
		{
			name:   "nothing to drop: page order, each comment with its root post",
			change: func(*likedCommentsFixture) {},
			want: []string{
				likesCommentB + " @ " + likesRootB,
				likesCommentX + " @ " + likesRootX,
				likesCommentA + " @ " + likesRootA,
			},
		},
		{
			name: "the viewer's own comment held for a ban check is served to the viewer",
			change: func(f *likedCommentsFixture) {
				hide := "pending_ban_check"
				f.commentRepo.comments[likesCommentX].HideReason = &hide
				f.commentRepo.comments[likesCommentX].CommenterDID = likesViewerDID
			},
			want: []string{
				likesCommentB + " @ " + likesRootB,
				likesCommentX + " @ " + likesRootX,
				likesCommentA + " @ " + likesRootA,
			},
		},
		{
			name: "a served comment carries the community its page row resolved",
			change: func(f *likedCommentsFixture) {
				f.page = []LikedSubjectRef{
					{URI: likesCommentB, Kind: LikedSubjectComment, RootURI: likesRootB, CommunityDID: likesPageCommunity},
				}
			},
			want: []string{likesCommentB + " @ " + likesRootB},
			alsoAssert: func(t *testing.T, response *ActorLikesResponse) {
				require.Len(t, response.Feed, 1)
				require.NotNil(t, response.Feed[0].Comment)
				assert.Equal(t, likesPageCommunity, response.Feed[0].Comment.CommunityDID,
					"the comment view must carry the page row's community, which hydration does not resolve")
			},
		},
		{
			name: "the viewer's own comment hidden by a community ban",
			change: func(f *likedCommentsFixture) {
				hide := HideReasonCommunityBan
				f.commentRepo.comments[likesCommentX].HideReason = &hide
				f.commentRepo.comments[likesCommentX].CommenterDID = likesViewerDID
			},
			want: withoutX,
		},
		{
			name: "the viewer's own comment hidden by an account ban",
			change: func(f *likedCommentsFixture) {
				hide := HideReasonAccountBan
				f.commentRepo.comments[likesCommentX].HideReason = &hide
				f.commentRepo.comments[likesCommentX].CommenterDID = likesViewerDID
			},
			want: withoutX,
		},
		{
			name:   "comment with no indexed row",
			change: func(f *likedCommentsFixture) { delete(f.commentRepo.comments, likesCommentX) },
			want:   withoutX,
		},
		{
			name: "comment its author deleted",
			change: func(f *likedCommentsFixture) {
				deletedAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
				reason := DeletionReasonAuthor
				f.commentRepo.comments[likesCommentX].DeletedAt = &deletedAt
				f.commentRepo.comments[likesCommentX].DeletionReason = &reason
			},
			want: withoutX,
		},
		{
			name: "comment under an active removal",
			change: func(f *likedCommentsFixture) {
				f.commentRepo.activeRemovalsByURI = map[string][]RemovalSource{
					likesCommentX: {{AuthorityDID: "did:web:coves.social", ScopeKind: "instance"}},
				}
			},
			want: withoutX,
		},
		{
			name: "comment hidden from the viewer",
			change: func(f *likedCommentsFixture) {
				hide := HideReasonCommunityBan
				f.commentRepo.comments[likesCommentX].HideReason = &hide
			},
			want: withoutX,
		},
		{
			name: "comment whose author the viewer blocked",
			change: func(f *likedCommentsFixture) {
				f.commentRepo.comments[likesCommentX].CommenterDID = likesBlockedDID
				f.commentRepo.blockedCommentURIs = map[string]bool{likesCommentX: true}
			},
			want: withoutX,
		},
		{
			name:   "comment whose root post has no view",
			change: func(f *likedCommentsFixture) { delete(f.postRepo.views, likesRootX) },
			want:   withoutX,
		},
		{
			name: "comment whose root post's author the viewer blocked",
			change: func(f *likedCommentsFixture) {
				f.postRepo.views[likesRootX].Author.DID = likesBlockedDID
				f.blocks.blocked = map[string]bool{likesBlockedDID: true}
			},
			want: withoutX,
		},
		{
			name: "mixed page: order kept across posts and comments",
			change: func(f *likedCommentsFixture) {
				f.filter, f.page = LikesFilterAll, mixedLikesPage
			},
			want: []string{"post " + likesPostP1, likesCommentB + " @ " + likesRootB, "post " + likesPostP2},
		},
		{
			name: "mixed page: liked post with no view",
			change: func(f *likedCommentsFixture) {
				f.filter, f.page = LikesFilterAll, mixedLikesPage
				delete(f.postRepo.views, likesPostP2)
			},
			want: mixedWithoutP2,
		},
		{
			name: "mixed page: liked post whose author the viewer blocked",
			change: func(f *likedCommentsFixture) {
				f.filter, f.page = LikesFilterAll, mixedLikesPage
				f.postRepo.views[likesPostP2].Author.DID = likesBlockedDID
				f.blocks.blocked = map[string]bool{likesBlockedDID: true}
			},
			want: mixedWithoutP2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLikedCommentsFixture()
			tt.change(f)

			response, err := f.get(t, 0)

			require.NoError(t, err)
			require.NotNil(t, response)
			assert.Equal(t, tt.want, likedPairs(response))
			require.NotNil(t, response.Cursor, "the page cursor must survive dropped items")
			assert.Equal(t, likesNextPageCursor, *response.Cursor)
			assert.Equal(t, likesViewerDID, f.postRepo.viewsViewerDID, "the root posts must be hydrated for the viewer")
			if tt.alsoAssert != nil {
				tt.alsoAssert(t, response)
			}
		})
	}
}

// TestGetActorLikes_PageRequest proves what reaches the page query: the
// actor, the viewer, the filter, the cursor, and the limit defaulted to 50
// and clamped to 100.
func TestGetActorLikes_PageRequest(t *testing.T) {
	tests := []struct {
		name      string
		filter    LikesFilter
		limit     int
		wantLimit int
	}{
		{name: "omitted limit defaults to 50", filter: LikesFilterAll, limit: 0, wantLimit: 50},
		{name: "limit within bounds passes through", filter: LikesFilterComments, limit: 25, wantLimit: 25},
		{name: "posts filter passes through", filter: LikesFilterPosts, limit: 25, wantLimit: 25},
		{name: "limit above 100 is clamped to 100", filter: LikesFilterAll, limit: 101, wantLimit: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLikedCommentsFixture()
			f.filter = tt.filter

			_, err := f.get(t, tt.limit)

			require.NoError(t, err)
			require.NotNil(t, f.pageRequest, "the page query was not asked")
			cursor := "this-page"
			assert.Equal(t, GetActorLikesRequest{
				ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: tt.filter, Limit: tt.wantLimit, Cursor: &cursor,
			}, *f.pageRequest)
		})
	}
}

// TestGetActorLikes_EmptyPageServesEmptyFeed proves an empty page is a
// non-nil empty feed with no cursor.
func TestGetActorLikes_EmptyPageServesEmptyFeed(t *testing.T) {
	f := newLikedCommentsFixture()
	f.commentRepo.getLikedSubjectRefsFunc = func(context.Context, GetActorLikesRequest) ([]LikedSubjectRef, *string, error) {
		return []LikedSubjectRef{}, nil, nil
	}

	response, err := f.get(t, 0)

	require.NoError(t, err)
	require.NotNil(t, response)
	assert.NotNil(t, response.Feed, "an empty page must serialize as an empty feed, not null")
	assert.Empty(t, response.Feed)
	assert.Nil(t, response.Cursor)
}

// TestGetActorLikes_FailsClosed proves each lookup failure, a signed-in
// viewer with no block checker installed, and a page row the service cannot
// interpret fail the request rather than serving a partial or unfiltered
// page, and that an invalid cursor stays matchable. A row with no wantErr
// expects any error.
func TestGetActorLikes_FailsClosed(t *testing.T) {
	lookupFailed := errors.New("lookup failed")
	tests := []struct {
		name    string
		change  func(f *likedCommentsFixture)
		wantErr error
	}{
		{
			name: "page query rejects the cursor",
			change: func(f *likedCommentsFixture) {
				f.commentRepo.getLikedSubjectRefsFunc = func(context.Context, GetActorLikesRequest) ([]LikedSubjectRef, *string, error) {
					return nil, nil, fmt.Errorf("parse liked comments cursor: %w", ErrInvalidCursor)
				}
			},
			wantErr: ErrInvalidCursor,
		},
		{
			name:   "signed-in viewer on a service with no block checker",
			change: func(f *likedCommentsFixture) { f.withoutBlockChecker = true },
		},
		{
			name: "page row of an unknown kind",
			change: func(f *likedCommentsFixture) {
				f.page = []LikedSubjectRef{
					{URI: likesCommentA, Kind: LikedSubjectComment, RootURI: likesRootA},
					{URI: likesPostP1, Kind: LikedSubjectKind("repost")},
				}
			},
		},
		{
			name: "comment page row with no root post",
			change: func(f *likedCommentsFixture) {
				f.page = []LikedSubjectRef{
					{URI: likesCommentA, Kind: LikedSubjectComment, RootURI: likesRootA},
					{URI: likesCommentX, Kind: LikedSubjectComment, RootURI: ""},
				}
			},
		},
		{
			name:    "comment hydration fails",
			change:  func(f *likedCommentsFixture) { f.commentRepo.getByURIsBatchErr = lookupFailed },
			wantErr: lookupFailed,
		},
		{
			name:    "comment author block check fails",
			change:  func(f *likedCommentsFixture) { f.commentRepo.blockedCommentURIsErr = lookupFailed },
			wantErr: lookupFailed,
		},
		{
			name:    "root post hydration fails",
			change:  func(f *likedCommentsFixture) { f.postRepo.viewsErr = lookupFailed },
			wantErr: lookupFailed,
		},
		{
			name:    "root post author block check fails",
			change:  func(f *likedCommentsFixture) { f.blocks.err = lookupFailed },
			wantErr: lookupFailed,
		},
		{
			name: "liked post hydration fails",
			change: func(f *likedCommentsFixture) {
				f.filter, f.page = LikesFilterPosts, likedPostsPage
				f.postRepo.viewsErr = lookupFailed
			},
			wantErr: lookupFailed,
		},
		{
			name: "liked post author block check fails",
			change: func(f *likedCommentsFixture) {
				f.filter, f.page = LikesFilterPosts, likedPostsPage
				f.blocks.err = lookupFailed
			},
			wantErr: lookupFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLikedCommentsFixture()
			tt.change(f)

			response, err := f.get(t, 0)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				assert.Error(t, err)
			}
			assert.Nil(t, response)
		})
	}
}
