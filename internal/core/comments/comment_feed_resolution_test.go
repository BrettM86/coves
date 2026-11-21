package comments

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"Coves/internal/core/feedgen"
)

// recordingCommentGenerator answers each requested parent with a fixed
// literal reply order and records every request it is handed.
type recordingCommentGenerator struct {
	orders   map[string][]string
	cursors  map[string]string
	requests []feedgen.CommentSkeletonRequest
}

func (g *recordingCommentGenerator) CommentSkeleton(_ context.Context, req feedgen.CommentSkeletonRequest) (feedgen.CommentSkeletonPage, error) {
	g.requests = append(g.requests, req)
	page := feedgen.CommentSkeletonPage{}
	for _, parent := range req.Parents {
		page.Replies = append(page.Replies, feedgen.CommentReplies{
			Parent: parent,
			URIs:   append([]string(nil), g.orders[parent]...),
			Cursor: g.cursors[parent],
		})
	}
	return page, nil
}

// B7: GetComments resolves its comment generator from feed, then sort, then
// comments-hot, and serves the resolved generator's order at the top level,
// the parentRkey subtree level and every reply-batch level. timeframe is only
// accepted when the resolved feed is comments-top.
func TestCommentService_GetComments_FeedResolution(t *testing.T) {
	const (
		postURI  = "at://did:plc:post123/social.coves.community.post/feedresolution"
		viewer   = "did:plc:viewer123"
		commentA = "at://did:plc:commenter123/social.coves.community.comment/a"
		commentB = "at://did:plc:commenter123/social.coves.community.comment/b"
		commentC = "at://did:plc:commenter123/social.coves.community.comment/c"
		replyA   = "at://did:plc:commenter123/social.coves.community.comment/a1"
		replyB   = "at://did:plc:commenter123/social.coves.community.comment/b1"
		replyC   = "at://did:plc:commenter123/social.coves.community.comment/c1"
		hotURI   = "at://did:web:comments.test/social.coves.feed.generator/comments-hot"
		topURI   = "at://did:web:comments.test/social.coves.feed.generator/comments-top"
		newURI   = "at://did:web:comments.test/social.coves.feed.generator/comments-new"
		postsHot = "at://did:web:comments.test/social.coves.feed.generator/hot"
		unknown  = "at://did:web:comments.test/social.coves.feed.generator/comments-nope"
	)
	viewerDID := viewer
	cursor := "subtree-page-2"

	type want struct {
		generator string // "hot", "top" or "new"
		topLevel  []string
		requests  []feedgen.CommentSkeletonRequest
		cursor    string
	}
	batchRequest := func(parents ...string) feedgen.CommentSkeletonRequest {
		return feedgen.CommentSkeletonRequest{ViewerDID: viewer, Parents: parents, LimitPerParent: 5, Timeframe: ""}
	}

	tests := []struct {
		name       string
		req        GetCommentsRequest
		wantErr    error
		wantValErr bool
		want       want
	}{
		{
			name: "no feed and no sort serves comments-hot",
			req:  GetCommentsRequest{PostURI: postURI, ViewerDID: &viewerDID, Depth: 2, Limit: 10},
			want: want{
				generator: "hot",
				topLevel:  []string{commentA, commentB, commentC},
				requests: []feedgen.CommentSkeletonRequest{
					{ViewerDID: viewer, Parents: []string{postURI}, LimitPerParent: 10},
					batchRequest(commentA, commentB, commentC),
				},
				cursor: "hot-page-2",
			},
		},
		{
			name: "feed wins over sort",
			req:  GetCommentsRequest{PostURI: postURI, ViewerDID: &viewerDID, Feed: newURI, Sort: "top", Depth: 2, Limit: 10},
			want: want{
				generator: "new",
				topLevel:  []string{commentB, commentC, commentA},
				requests: []feedgen.CommentSkeletonRequest{
					{ViewerDID: viewer, Parents: []string{postURI}, LimitPerParent: 10},
					batchRequest(commentB, commentC, commentA),
				},
				cursor: "new-page-2",
			},
		},
		{
			name: "sort=top serves comments-top",
			req:  GetCommentsRequest{PostURI: postURI, ViewerDID: &viewerDID, Sort: "top", Depth: 2, Limit: 10},
			want: want{
				generator: "top",
				topLevel:  []string{commentC, commentA, commentB},
				requests: []feedgen.CommentSkeletonRequest{
					{ViewerDID: viewer, Parents: []string{postURI}, LimitPerParent: 10},
					batchRequest(commentC, commentA, commentB),
				},
				cursor: "top-page-2",
			},
		},
		{
			name: "sort=top with timeframe passes the timeframe to the top level only",
			req:  GetCommentsRequest{PostURI: postURI, ViewerDID: &viewerDID, Sort: "top", Timeframe: "week", Depth: 2, Limit: 10},
			want: want{
				generator: "top",
				topLevel:  []string{commentC, commentA, commentB},
				requests: []feedgen.CommentSkeletonRequest{
					{ViewerDID: viewer, Parents: []string{postURI}, LimitPerParent: 10, Timeframe: "week"},
					batchRequest(commentC, commentA, commentB),
				},
				cursor: "top-page-2",
			},
		},
		{
			name: "feed=comments-top with timeframe and no sort is accepted",
			req:  GetCommentsRequest{PostURI: postURI, ViewerDID: &viewerDID, Feed: topURI, Timeframe: "week", Depth: 2, Limit: 10},
			want: want{
				generator: "top",
				topLevel:  []string{commentC, commentA, commentB},
				requests: []feedgen.CommentSkeletonRequest{
					{ViewerDID: viewer, Parents: []string{postURI}, LimitPerParent: 10, Timeframe: "week"},
					batchRequest(commentC, commentA, commentB),
				},
				cursor: "top-page-2",
			},
		},
		{
			name: "parentRkey subtree level carries the timeframe and the cursor",
			req: GetCommentsRequest{
				PostURI: postURI, ViewerDID: &viewerDID, ParentRkey: "a", Feed: topURI, Timeframe: "week",
				Cursor: &cursor, Depth: 2, Limit: 10,
			},
			want: want{
				generator: "top",
				topLevel:  []string{commentA},
				requests: []feedgen.CommentSkeletonRequest{
					{ViewerDID: viewer, Parents: []string{commentA}, LimitPerParent: 10, Timeframe: "week", Cursor: "subtree-page-2"},
				},
			},
		},
		{
			name:    "an unregistered feed is an unknown feed",
			req:     GetCommentsRequest{PostURI: postURI, Feed: unknown, Depth: 2, Limit: 10},
			wantErr: feedgen.ErrUnknownFeed,
		},
		{
			name:    "a posts generator is an unsupported feed scope",
			req:     GetCommentsRequest{PostURI: postURI, Feed: postsHot, Depth: 2, Limit: 10},
			wantErr: feedgen.ErrUnsupportedFeedScope,
		},
		{
			name:       "timeframe with feed=comments-hot is a validation error",
			req:        GetCommentsRequest{PostURI: postURI, Feed: hotURI, Timeframe: "week", Depth: 2, Limit: 10},
			wantValErr: true,
		},
		{
			name:       "timeframe with feed=comments-new and sort=top is a validation error",
			req:        GetCommentsRequest{PostURI: postURI, Feed: newURI, Sort: "top", Timeframe: "week", Depth: 2, Limit: 10},
			wantValErr: true,
		},
		{
			name:       "timeframe with sort=hot is a validation error",
			req:        GetCommentsRequest{PostURI: postURI, Sort: "hot", Timeframe: "week", Depth: 2, Limit: 10},
			wantErr:    ErrTimeframeNotSupported,
			wantValErr: true,
		},
		{
			name:       "timeframe with sort=new is a validation error",
			req:        GetCommentsRequest{PostURI: postURI, Sort: "new", Timeframe: "week", Depth: 2, Limit: 10},
			wantErr:    ErrTimeframeNotSupported,
			wantValErr: true,
		},
		{
			name:       "timeframe with no sort and no feed is a validation error",
			req:        GetCommentsRequest{PostURI: postURI, Timeframe: "week", Depth: 2, Limit: 10},
			wantErr:    ErrTimeframeNotSupported,
			wantValErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			commentRepo := newMockCommentRepo()
			for _, seed := range []struct {
				uri, rkey, parent string
				replyCount        int
			}{
				{commentA, "a", postURI, 1},
				{commentB, "b", postURI, 1},
				{commentC, "c", postURI, 1},
				{replyA, "a1", commentA, 0},
				{replyB, "b1", commentB, 0},
				{replyC, "c1", commentC, 0},
			} {
				c := createTestComment(seed.uri, "did:plc:commenter123", "commenter.test", postURI, seed.parent, seed.replyCount)
				c.RKey = seed.rkey
				if err := commentRepo.Create(context.Background(), c); err != nil {
					t.Fatalf("seeding %s: %v", c.URI, err)
				}
			}
			postRepo := newMockPostRepo()
			if err := postRepo.Create(context.Background(), createTestPost(postURI, "did:plc:author123", "did:plc:community123")); err != nil {
				t.Fatalf("seeding post: %v", err)
			}

			replies := map[string][]string{commentA: {replyA}, commentB: {replyB}, commentC: {replyC}}
			generator := func(topLevel []string, nextCursor string) *recordingCommentGenerator {
				orders := map[string][]string{postURI: topLevel}
				for parent, uris := range replies {
					orders[parent] = uris
				}
				return &recordingCommentGenerator{orders: orders, cursors: map[string]string{postURI: nextCursor}}
			}
			generators := map[string]*recordingCommentGenerator{
				"hot": generator([]string{commentA, commentB, commentC}, "hot-page-2"),
				"top": generator([]string{commentC, commentA, commentB}, "top-page-2"),
				"new": generator([]string{commentB, commentC, commentA}, "new-page-2"),
			}
			postsGenerator := &stubPostGenerator{}
			registry := feedgen.NewRegistry("did:web:comments.test",
				feedgen.HotBuiltin(postsGenerator),
				feedgen.CommentsHotBuiltin(generators["hot"]),
				feedgen.CommentsTopBuiltin(generators["top"]),
				feedgen.CommentsNewBuiltin(generators["new"]),
			)
			service := NewCommentService(commentRepo, newMockUserRepo(), postRepo, newMockCommunityRepo(), nil, nil, nil, WithFeedRegistry(registry))

			req := tc.req
			resp, err := service.GetComments(context.Background(), &req)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("GetComments error = %v, want errors.Is %v", err, tc.wantErr)
				}
				if tc.wantValErr && !IsValidationError(err) {
					t.Fatalf("GetComments error = %v, want a validation error (IsValidationError)", err)
				}
				return
			}
			if tc.wantValErr {
				if err == nil || !IsValidationError(err) {
					t.Fatalf("GetComments error = %v, want a validation error (IsValidationError)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetComments returned error %v, want nil", err)
			}

			var gotTop []string
			for _, view := range resp.Comments {
				gotTop = append(gotTop, view.Comment.URI)
			}
			if tc.req.ParentRkey != "" {
				// The subtree's sole top-level entry is the parent; its replies
				// are the generator's order for it.
				if len(resp.Comments) != 1 {
					t.Fatalf("subtree response has %d top-level comments, want 1", len(resp.Comments))
				}
				gotTop = nil
				for _, view := range resp.Comments[0].Replies {
					gotTop = append(gotTop, view.Comment.URI)
				}
				if want := []string{replyA}; !reflect.DeepEqual(gotTop, want) {
					t.Errorf("subtree replies = %v, want %v", gotTop, want)
				}
			} else {
				if !reflect.DeepEqual(gotTop, tc.want.topLevel) {
					t.Errorf("top-level order = %v, want %v", gotTop, tc.want.topLevel)
				}
				for _, view := range resp.Comments {
					if len(view.Replies) != 1 || view.Replies[0].Comment.URI != replies[view.Comment.URI][0] {
						t.Errorf("replies of %s = %v, want [%s]", view.Comment.URI, threadURIs(view.Replies), replies[view.Comment.URI][0])
					}
				}
				gotCursor := ""
				if resp.Cursor != nil {
					gotCursor = *resp.Cursor
				}
				if gotCursor != tc.want.cursor {
					t.Errorf("response cursor = %q, want %q", gotCursor, tc.want.cursor)
				}
			}

			if got := generators[tc.want.generator].requests; !reflect.DeepEqual(got, tc.want.requests) {
				t.Errorf("comments-%s generator requests =\n  %+v\nwant\n  %+v", tc.want.generator, got, tc.want.requests)
			}
		})
	}
}

func threadURIs(views []*ThreadViewComment) []string {
	uris := make([]string, 0, len(views))
	for _, view := range views {
		if view != nil && view.Comment != nil {
			uris = append(uris, view.Comment.URI)
		}
	}
	return uris
}

// stubPostGenerator is a posts generator that is never meant to be called by
// the comment service.
type stubPostGenerator struct{}

func (*stubPostGenerator) Skeleton(context.Context, feedgen.SkeletonRequest) (feedgen.SkeletonPage, error) {
	return feedgen.SkeletonPage{}, nil
}
