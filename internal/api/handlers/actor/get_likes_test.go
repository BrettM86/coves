package actor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/moderation/moderationtest"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
)

// likesAdminAuthority treats exactly one DID as an instance admin.
type likesAdminAuthority string

func (a likesAdminAuthority) IsInstanceAdmin(did string) bool { return did == string(a) }

const (
	likesViewerDID = "did:plc:liker"
	likesBlobCID   = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	likesPostURI   = "at://did:plc:likedpostauthor/social.coves.community.postv2/abc"
)

// postViewWithImage is a postv2 view by authorDID whose image embed still
// carries a blob ref.
func postViewWithImage(uri, authorDID string) *posts.PostView {
	return &posts.PostView{
		URI:    uri,
		Author: &posts.AuthorView{DID: authorDID, PDSURL: "http://pds.example"},
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.images",
			"images": []interface{}{map[string]interface{}{
				"alt": "a",
				"image": map[string]interface{}{
					"$type": "blob", "ref": map[string]interface{}{"$link": likesBlobCID},
					"mimeType": "image/png", "size": 10,
				},
			}},
		},
	}
}

// likedPostPage is one liked postv2 view by likedPostAuthorDID, in
// likesCommunityDID, whose image embed still carries a blob ref.
func likedPostPage() *comments.ActorLikesResponse {
	view := postViewWithImage(likesPostURI, likedPostAuthorDID)
	view.CID = likesCID
	view.Status = "accepted"
	view.Community = &posts.CommunityRef{DID: likesCommunityDID, Handle: "community.test", Name: "community"}
	return &comments.ActorLikesResponse{Feed: []*comments.LikedItem{{Post: view}}}
}

const (
	likesCommunityDID       = "did:plc:likescommunity"
	likesAuthorDID          = "did:plc:likesauthor"
	likedCommentURI         = "at://did:plc:likesauthor/social.coves.community.comment/reply"
	likedCommentParentURI   = "at://did:plc:likesauthor/social.coves.community.comment/parent"
	likedCommentRootURI     = "at://did:plc:likesauthor/social.coves.community.postv2/root"
	likedCommentVoteURI     = "at://did:plc:liker/social.coves.feed.vote/comment"
	likedCommentRootVoteURI = "at://did:plc:liker/social.coves.feed.vote/root"
	quotedCommentURI        = "at://did:plc:likesauthor/social.coves.community.comment/quoted"
	quotedCommentRootURI    = "at://did:plc:likesauthor/social.coves.community.postv2/quote"
	likesCID                = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
	likedPostVoteURI        = "at://did:plc:liker/social.coves.feed.vote/post"
	likedPostAuthorDID      = "did:plc:likedpostauthor"
)

// likedCommentsPage is two liked comments in likesCommunityDID: a nested
// reply whose root post has an image embed with a blob ref, and a comment
// whose root post quotes a Bluesky post.
func likedCommentsPage() *comments.ActorLikesResponse {
	comment := func(uri, rootURI, parentURI string) *comments.CommentView {
		return &comments.CommentView{
			URI: uri, CID: likesCID,
			Author:       &posts.AuthorView{DID: likesAuthorDID, Handle: "author.test"},
			Post:         &comments.CommentRef{URI: rootURI, CID: likesCID},
			Parent:       &comments.CommentRef{URI: parentURI, CID: likesCID},
			Stats:        &comments.CommentStats{Upvotes: 1, Score: 1},
			CommunityDID: likesCommunityDID,
		}
	}
	root := func(view *posts.PostView) *posts.PostView {
		view.CID = likesCID
		view.Status = "accepted"
		view.Community = &posts.CommunityRef{DID: likesCommunityDID, Handle: "community.test", Name: "community"}
		return view
	}
	quoteRoot := root(&posts.PostView{
		URI:    quotedCommentRootURI,
		Author: &posts.AuthorView{DID: likesAuthorDID, PDSURL: "http://pds.example"},
		Embed: map[string]interface{}{
			"$type": "social.coves.embed.post",
			"post":  map[string]interface{}{"uri": "at://did:plc:bskyauthor/app.bsky.feed.post/quoted", "cid": likesCID},
		},
	})
	return &comments.ActorLikesResponse{
		Feed: []*comments.LikedItem{
			{
				Comment: comment(likedCommentURI, likedCommentRootURI, likedCommentParentURI),
				Post:    root(postViewWithImage(likedCommentRootURI, likesAuthorDID)),
			},
			{
				Comment: comment(quotedCommentURI, quotedCommentRootURI, quotedCommentRootURI),
				Post:    quoteRoot,
			},
		},
		Cursor: strPtr("c3"),
	}
}

// mixedLikesPage is the liked post of likedPostPage, then the liked nested
// reply of likedCommentsPage with its root post.
func mixedLikesPage() *comments.ActorLikesResponse {
	return &comments.ActorLikesResponse{Feed: []*comments.LikedItem{
		likedPostPage().Feed[0],
		likedCommentsPage().Feed[0],
	}}
}

// likedItem is what a getLikes item identifies: its union member and the
// URIs of its comment ("" for a liked post) and post.
type likedItem struct {
	Type       string
	CommentURI string
	PostURI    string
}

func decodeLikedItems(t *testing.T, body []byte) []likedItem {
	t.Helper()
	var decoded struct {
		Feed []struct {
			Type    string `json:"$type"`
			Comment *struct {
				URI string `json:"uri"`
			} `json:"comment"`
			Post *struct {
				URI string `json:"uri"`
			} `json:"post"`
		} `json:"feed"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body %s: %v", body, err)
	}
	items := make([]likedItem, 0, len(decoded.Feed))
	for _, entry := range decoded.Feed {
		item := likedItem{Type: entry.Type}
		if entry.Comment != nil {
			item.CommentURI = entry.Comment.URI
		}
		if entry.Post != nil {
			item.PostURI = entry.Post.URI
		}
		items = append(items, item)
	}
	return items
}

func TestGetLikesHandler(t *testing.T) {
	resolve := func(ctx context.Context, identifier string) (string, error) {
		switch identifier {
		case "liker.test", "liker@lemmy.example":
			return likesViewerDID, nil
		}
		return "", users.ErrUserNotFound
	}

	const (
		likedPostType    = "social.coves.actor.getLikes#likedPost"
		likedCommentType = "social.coves.actor.getLikes#likedComment"
	)

	allRequest := &comments.GetActorLikesRequest{ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: comments.LikesFilterAll}
	mixedItems := []likedItem{
		{Type: likedPostType, PostURI: likesPostURI},
		{Type: likedCommentType, CommentURI: likedCommentURI, PostURI: likedCommentRootURI},
	}

	tests := []struct {
		name        string
		query       string
		viewerDID   string
		response    *comments.ActorLikesResponse
		serviceErr  error
		wantStatus  int
		wantError   string
		wantRequest *comments.GetActorLikesRequest // nil: the service must not be asked
		wantBody    []string                       // literal substrings of the response body
		wantItems   []likedItem                    // nil: items not checked
	}{
		{
			name:       "no viewer is 401",
			query:      "actor=" + likesViewerDID,
			wantStatus: http.StatusUnauthorized,
			wantError:  "AuthenticationRequired",
		},
		{
			name:       "viewer asking for another actor's DID is 403",
			query:      "actor=did:plc:someoneelse",
			viewerDID:  likesViewerDID,
			wantStatus: http.StatusForbidden,
			wantError:  "NotAllowed",
		},
		{
			name:       "filter=comments for another actor is 403",
			query:      "actor=did:plc:someoneelse&filter=comments",
			viewerDID:  likesViewerDID,
			wantStatus: http.StatusForbidden,
			wantError:  "NotAllowed",
		},
		{
			name:       "unknown handle is 404",
			query:      "actor=nobody.test",
			viewerDID:  likesViewerDID,
			wantStatus: http.StatusNotFound,
			wantError:  "ActorNotFound",
		},
		{
			name:       "non-integer limit is 400",
			query:      "actor=" + likesViewerDID + "&limit=abc",
			viewerDID:  likesViewerDID,
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidRequest",
		},
		{
			name:       "unknown filter is 400 InvalidRequest",
			query:      "actor=" + likesViewerDID + "&filter=bogus",
			viewerDID:  likesViewerDID,
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidRequest",
		},
		{
			name:        "wrapped invalid cursor from the service is 400 InvalidCursor",
			query:       "actor=" + likesViewerDID + "&filter=comments&cursor=garbage",
			viewerDID:   likesViewerDID,
			serviceErr:  fmt.Errorf("liked page: %w", comments.ErrInvalidCursor),
			wantStatus:  http.StatusBadRequest,
			wantError:   "InvalidCursor",
			wantRequest: &comments.GetActorLikesRequest{ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: comments.LikesFilterComments, Cursor: strPtr("garbage")},
		},
		{
			name:        "own handle resolves to the viewer",
			query:       "actor=liker.test",
			viewerDID:   likesViewerDID,
			wantStatus:  http.StatusOK,
			wantRequest: allRequest,
		},
		{
			name:        "own name@host resolves to the viewer",
			query:       "actor=liker@lemmy.example",
			viewerDID:   likesViewerDID,
			wantStatus:  http.StatusOK,
			wantRequest: allRequest,
		},
		{
			name:        "filter=posts with limit and cursor reaches the service; empty feed serializes as [] and the next cursor passes through",
			query:       "actor=" + likesViewerDID + "&filter=posts&limit=25&cursor=next",
			viewerDID:   likesViewerDID,
			response:    &comments.ActorLikesResponse{Feed: []*comments.LikedItem{}, Cursor: strPtr("c2")},
			wantStatus:  http.StatusOK,
			wantRequest: &comments.GetActorLikesRequest{ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: comments.LikesFilterPosts, Limit: 25, Cursor: strPtr("next")},
			wantBody:    []string{`"feed":[]`, `"cursor":"c2"`},
			wantItems:   []likedItem{},
		},
		{
			name:        "filter=comments with limit and cursor reaches the service; empty feed serializes as [] and the next cursor passes through",
			query:       "actor=liker.test&filter=comments&limit=25&cursor=next",
			viewerDID:   likesViewerDID,
			response:    &comments.ActorLikesResponse{Feed: []*comments.LikedItem{}, Cursor: strPtr("c2")},
			wantStatus:  http.StatusOK,
			wantRequest: &comments.GetActorLikesRequest{ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: comments.LikesFilterComments, Limit: 25, Cursor: strPtr("next")},
			wantBody:    []string{`"feed":[]`, `"cursor":"c2"`},
			wantItems:   []likedItem{},
		},
		{
			name:        "filter=all asks for all likes and serves the mixed page in service order",
			query:       "actor=" + likesViewerDID + "&filter=all",
			viewerDID:   likesViewerDID,
			response:    mixedLikesPage(),
			wantStatus:  http.StatusOK,
			wantRequest: allRequest,
			wantItems:   mixedItems,
		},
		{
			name:        "omitted filter asks for all likes; a liked post item carries the viewer's vote, instance and community moderation and its blob ref projected to a URL",
			query:       "actor=" + likesViewerDID,
			viewerDID:   likesViewerDID,
			response:    mixedLikesPage(),
			wantStatus:  http.StatusOK,
			wantRequest: allRequest,
			wantItems:   mixedItems,
			wantBody: []string{
				`"viewer":{"vote":"up","voteUri":"` + likedPostVoteURI + `","moderation":["remove","label"],"communityModeration":["remove","ban-author"],"saved":false}`,
				`"thumb":"http://pds.example/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Alikedpostauthor` + "\\u0026" + `cid=` + likesBlobCID + `"`,
			},
		},
		{
			name:        "filter=posts asks for liked posts only",
			query:       "actor=" + likesViewerDID + "&filter=posts",
			viewerDID:   likesViewerDID,
			response:    likedPostPage(),
			wantStatus:  http.StatusOK,
			wantRequest: &comments.GetActorLikesRequest{ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: comments.LikesFilterPosts},
			wantItems:   []likedItem{{Type: likedPostType, PostURI: likesPostURI}},
		},
		{
			name:        "filter=comments serves likedComment items with viewer state and moderation on the comment and an enriched root post",
			query:       "actor=" + likesViewerDID + "&filter=comments",
			viewerDID:   likesViewerDID,
			response:    likedCommentsPage(),
			wantStatus:  http.StatusOK,
			wantRequest: &comments.GetActorLikesRequest{ActorDID: likesViewerDID, ViewerDID: likesViewerDID, Filter: comments.LikesFilterComments},
			wantItems: []likedItem{
				{Type: likedCommentType, CommentURI: likedCommentURI, PostURI: likedCommentRootURI},
				{Type: likedCommentType, CommentURI: quotedCommentURI, PostURI: quotedCommentRootURI},
			},
			wantBody: []string{
				// the liked comment: the viewer's vote, instance and community moderation
				`"viewer":{"vote":"up","voteUri":"` + likedCommentVoteURI + `","moderation":["remove"],"communityModeration":["remove","ban-author"]}`,
				// its root post: the viewer's vote, instance and community moderation
				`"viewer":{"vote":"up","voteUri":"` + likedCommentRootVoteURI + `","moderation":["remove","label"],"communityModeration":["remove","ban-author"],"saved":false}`,
				// the root post's blob ref projected to a URL
				`"$type":"social.coves.embed.images#view"`,
				`"thumb":"http://pds.example/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Alikesauthor` + "\\u0026" + `cid=` + likesBlobCID + `"`,
				// the other root post's Bluesky quote transformed
				`"$type":"social.coves.embed.post#view"`,
				`"cursor":"c3"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotRequest *comments.GetActorLikesRequest
			commentService := &mockCommentService{
				getActorLikesFunc: func(ctx context.Context, req *comments.GetActorLikesRequest) (*comments.ActorLikesResponse, error) {
					copied := *req
					gotRequest = &copied
					if tt.serviceErr != nil {
						return nil, tt.serviceErr
					}
					if tt.response != nil {
						return tt.response, nil
					}
					return &comments.ActorLikesResponse{Feed: []*comments.LikedItem{}}, nil
				},
			}
			userService := &mockUserService{resolveActorIdentifierToDIDFunc: resolve}
			voteService := &mockVoteServiceForComments{viewerVotes: map[string]votes.ViewerVote{
				likedCommentURI:     {Direction: "up", URI: likedCommentVoteURI},
				likedCommentRootURI: {Direction: "up", URI: likedCommentRootVoteURI},
				likesPostURI:        {Direction: "up", URI: likedPostVoteURI},
			}}
			communityViewer := moderationtest.CommunityViewer{
				Authority: map[string]map[string]moderation.CommunityViewerAuthority{
					likesCommunityDID: {likesViewerDID: moderation.CommunityAuthorityModerator},
				},
				Roles: map[string]map[string]bool{likesCommunityDID: {likesViewerDID: true}},
			}
			handler := NewGetLikesHandler(commentService, userService, voteService, &mockBlueskyService{}).
				WithViewerModeration(likesAdminAuthority(likesViewerDID), "did:web:coves.test").
				WithCommunityViewerModeration(communityViewer)

			req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getLikes?"+tt.query, nil)
			if tt.viewerDID != "" {
				req = req.WithContext(middleware.SetTestUserDID(req.Context(), tt.viewerDID))
			}
			rec := httptest.NewRecorder()
			handler.HandleGetLikes(rec, req)

			body := rec.Body.String()
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, body)
			}
			if tt.wantError != "" {
				var errBody ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
					t.Fatalf("decode error body %q: %v", body, err)
				}
				if errBody.Error != tt.wantError {
					t.Errorf("error = %q, want %q", errBody.Error, tt.wantError)
				}
			}
			if !reflect.DeepEqual(gotRequest, tt.wantRequest) {
				t.Errorf("service request = %s, want %s", describeLikesRequest(gotRequest), describeLikesRequest(tt.wantRequest))
			}
			if tt.wantItems != nil {
				if got := decodeLikedItems(t, rec.Body.Bytes()); !reflect.DeepEqual(got, tt.wantItems) {
					t.Errorf("items = %+v, want %+v\nbody: %s", got, tt.wantItems, body)
				}
			}
			for _, want := range tt.wantBody {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s\nbody: %s", want, body)
				}
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func describeLikesRequest(req *comments.GetActorLikesRequest) string {
	if req == nil {
		return "<not called>"
	}
	cursor := "<nil>"
	if req.Cursor != nil {
		cursor = *req.Cursor
	}
	return fmt.Sprintf("{ActorDID:%q ViewerDID:%q Filter:%q Limit:%d Cursor:%s}", req.ActorDID, req.ViewerDID, req.Filter, req.Limit, cursor)
}

// TestGetLikesHandler_ServesCommunityViewerModeration proves getLikes
// hydrates viewer.communityModeration on a liked post in a community the
// viewer moderates, as getPosts does, and omits it in a community where the
// viewer holds no role. Which actions apply is owned by the moderation rule
// tests.
func TestGetLikesHandler_ServesCommunityViewerModeration(t *testing.T) {
	const (
		moderatedCommunity   = "did:plc:likesmoderatedcommunity"
		unmoderatedCommunity = "did:plc:likesunmoderatedcommunity"
		plainAuthor          = "did:plc:likesplainauthor"
		moderatedPostURI     = "at://did:plc:likesplainauthor/social.coves.community.post/moderated"
		unmoderatedPostURI   = "at://did:plc:likesplainauthor/social.coves.community.post/unmoderated"
	)
	viewer := moderationtest.CommunityViewer{
		Authority: map[string]map[string]moderation.CommunityViewerAuthority{
			moderatedCommunity: {likesViewerDID: moderation.CommunityAuthorityModerator},
		},
		Roles: map[string]map[string]bool{moderatedCommunity: {likesViewerDID: true}},
	}
	post := func(uri, community string) *comments.LikedItem {
		return &comments.LikedItem{Post: &posts.PostView{
			URI: uri, CID: "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi", Status: "accepted",
			Author:    &posts.AuthorView{DID: plainAuthor, Handle: "author.test"},
			Community: &posts.CommunityRef{DID: community, Handle: "community.test", Name: "community"},
		}}
	}
	commentService := &mockCommentService{
		getActorLikesFunc: func(context.Context, *comments.GetActorLikesRequest) (*comments.ActorLikesResponse, error) {
			return &comments.ActorLikesResponse{Feed: []*comments.LikedItem{
				post(moderatedPostURI, moderatedCommunity), post(unmoderatedPostURI, unmoderatedCommunity),
			}}, nil
		},
	}
	handler := NewGetLikesHandler(commentService, &mockUserService{}, &mockVoteService{}, &mockBlueskyService{}).
		WithViewerModeration(likesAdminAuthority("did:plc:someadmin"), "did:web:coves.test").
		WithCommunityViewerModeration(viewer)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getLikes?filter=posts&actor="+likesViewerDID, nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), likesViewerDID))
	rec := httptest.NewRecorder()
	handler.HandleGetLikes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Feed []struct {
			Post struct {
				URI    string         `json:"uri"`
				Viewer map[string]any `json:"viewer"`
			} `json:"post"`
		} `json:"feed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Feed) != 2 {
		t.Fatalf("feed length = %d, want 2 (body: %s)", len(body.Feed), rec.Body.String())
	}
	want := map[string][]any{
		moderatedPostURI:   {"remove", "ban-author"},
		unmoderatedPostURI: nil,
	}
	for _, item := range body.Feed {
		got, present := item.Post.Viewer["communityModeration"]
		expected := want[item.Post.URI]
		if expected == nil {
			if present {
				t.Errorf("%s: viewer.communityModeration = %#v, want absent", item.Post.URI, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("%s: viewer.communityModeration = %#v, want %#v (viewer: %#v)", item.Post.URI, got, expected, item.Post.Viewer)
		}
	}
}
