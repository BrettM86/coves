package post

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/core/posts"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// mockGetPostService implements posts.Service for testing the get handler.
type mockGetPostService struct {
	getPostsFunc func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error)
}

func (m *mockGetPostService) CreatePost(ctx context.Context, req posts.CreatePostRequest) (*posts.CreatePostResponse, error) {
	return nil, nil
}

func (m *mockGetPostService) GetAuthorPosts(ctx context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
	return nil, nil
}

func (m *mockGetPostService) DeletePost(ctx context.Context, session *oauthlib.ClientSessionData, req posts.DeletePostRequest) error {
	return nil
}

func (m *mockGetPostService) GetPosts(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
	if m.getPostsFunc != nil {
		return m.getPostsFunc(ctx, req)
	}
	return nil, nil
}

func TestHandleGet_MissingURIs(t *testing.T) {
	h := NewGetHandler(&mockGetPostService{}, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.post.get", nil)

	h.HandleGet(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGet_TooManyURIs(t *testing.T) {
	h := NewGetHandler(&mockGetPostService{}, nil, nil)
	rec := httptest.NewRecorder()

	q := ""
	for i := 0; i < posts.MaxGetPostsURIs+1; i++ {
		if i > 0 {
			q += "&"
		}
		q += "uris=at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/r" + string(rune('a'+i%26))
	}
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.post.get?"+q, nil)

	h.HandleGet(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGet_Success_UnionOrder(t *testing.T) {
	foundURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/found"
	missingURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/missing"

	svc := &mockGetPostService{
		getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
			return []*posts.PostResult{
				{Post: &posts.PostView{URI: foundURI, CID: "cid1"}},
				{NotFound: &posts.NotFoundPost{URI: missingURI, NotFound: true}},
			}, nil
		},
	}
	h := NewGetHandler(svc, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.post.get?uris="+foundURI+"&uris="+missingURI, nil)

	h.HandleGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Posts []map[string]interface{} `json:"posts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v (body: %s)", err, rec.Body.String())
	}
	if len(resp.Posts) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(resp.Posts))
	}

	// [0] is a postView (has uri, no notFound marker)
	if got := resp.Posts[0]["uri"]; got != foundURI {
		t.Errorf("posts[0].uri = %v, want %q", got, foundURI)
	}
	if _, hasNotFound := resp.Posts[0]["notFound"]; hasNotFound {
		t.Errorf("posts[0] should be a postView, but has a notFound marker")
	}

	// [1] is a notFoundPost
	if got := resp.Posts[1]["notFound"]; got != true {
		t.Errorf("posts[1].notFound = %v, want true", got)
	}
	if got := resp.Posts[1]["uri"]; got != missingURI {
		t.Errorf("posts[1].uri = %v, want %q", got, missingURI)
	}
}
