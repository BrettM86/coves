package post

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/posts"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// mockGetPostService implements posts.Service for testing the get handler.
type mockGetPostService struct {
	getPostsFunc func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error)
}

func (m *mockGetPostService) CreatePost(ctx context.Context, session *oauthlib.ClientSessionData, req posts.CreatePostRequest) (*posts.CreatePostResponse, error) {
	return nil, nil
}

func (m *mockGetPostService) UpdatePost(context.Context, *oauthlib.ClientSessionData, posts.UpdatePostRequest) (*posts.UpdatePostResponse, error) {
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

// TestHandleGet_BlockedUnionMember verifies a blockedPost result from the service is
// emitted as a blockedPost union member (blocked:true, blockedBy:author) rather than a
// postView or notFoundPost.
func TestHandleGet_BlockedUnionMember(t *testing.T) {
	blockedURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/blocked"

	svc := &mockGetPostService{
		getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
			return []*posts.PostResult{
				{Blocked: &posts.BlockedPost{
					URI:       blockedURI,
					Blocked:   true,
					BlockedBy: "author",
					Author:    &posts.BlockedAuthor{DID: "did:plc:blockedauthor"},
				}},
			}, nil
		},
	}
	h := NewGetHandler(svc, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.post.get?uris="+blockedURI, nil)
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
	if len(resp.Posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(resp.Posts))
	}
	if got := resp.Posts[0]["blocked"]; got != true {
		t.Errorf("posts[0].blocked = %v, want true", got)
	}
	if got := resp.Posts[0]["blockedBy"]; got != "author" {
		t.Errorf("posts[0].blockedBy = %v, want author", got)
	}
	if _, hasNotFound := resp.Posts[0]["notFound"]; hasNotFound {
		t.Errorf("posts[0] should be a blockedPost, but has a notFound marker")
	}
}

// TestHandleGet_InvalidResultIs500 verifies that an internally-invalid result (no union
// member set) is reported as a 500 rather than serialized as a null array entry, which
// would violate the lexicon's union.
func TestHandleGet_InvalidResultIs500(t *testing.T) {
	uri := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/x"
	svc := &mockGetPostService{
		getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
			return []*posts.PostResult{{}}, nil // no member set
		},
	}
	h := NewGetHandler(svc, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.post.get?uris="+uri, nil)
	h.HandleGet(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestHandleGet_URITooLong verifies the handler rejects an over-length URI (per-URI
// length cap) with a 400 before reaching the service, bounding query-param abuse.
func TestHandleGet_URITooLong(t *testing.T) {
	called := false
	svc := &mockGetPostService{
		getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
			called = true
			return nil, nil
		},
	}
	h := NewGetHandler(svc, nil, nil)

	// One valid prefix padded past maxURILength.
	longURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/" +
		strings.Repeat("a", maxURILength)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.post.get?uris="+longURI, nil)
	h.HandleGet(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("service should not be called for an over-length URI")
	}
}

// TestHandleGet_ServiceErrorMapping verifies the handler maps service errors to the
// correct HTTP status: a validation error -> 400, any other (internal) error -> 500.
// A regression collapsing these would leak 500s for client mistakes or vice versa.
func TestHandleGet_ServiceErrorMapping(t *testing.T) {
	validURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/x"

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"validation error -> 400", posts.NewValidationError("uris", "bad"), http.StatusBadRequest},
		{"internal error -> 500", errors.New("db exploded"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &mockGetPostService{
				getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
					return nil, tt.err
				},
			}
			h := NewGetHandler(svc, nil, nil)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				"/xrpc/social.coves.community.post.get?uris="+validURI, nil)
			h.HandleGet(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestHandleGet_ViewerDIDPassthrough verifies the authenticated viewer's DID (set on the
// context by OptionalAuth) is forwarded to the service as ViewerDID. Dropping it would
// silently disable viewer block filtering on permalink/cold-load reads.
func TestHandleGet_ViewerDIDPassthrough(t *testing.T) {
	const viewerDID = "did:plc:viewer123"
	validURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/x"

	var gotViewerDID string
	svc := &mockGetPostService{
		getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
			gotViewerDID = req.ViewerDID
			return []*posts.PostResult{{NotFound: &posts.NotFoundPost{URI: validURI, NotFound: true}}}, nil
		},
	}
	h := NewGetHandler(svc, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.post.get?uris="+validURI, nil)
	// Simulate what OptionalAuth does for an authenticated viewer.
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, viewerDID))
	h.HandleGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if gotViewerDID != viewerDID {
		t.Errorf("service got ViewerDID = %q, want %q", gotViewerDID, viewerDID)
	}
}

// TestHandleGet_DuplicateURIs verifies duplicate URIs in the request are forwarded as-is
// (the service owns dedup/ordering) and the handler emits one union member per service
// result, in order.
func TestHandleGet_DuplicateURIs(t *testing.T) {
	dupURI := "at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/social.coves.community.post/dup"

	var gotURIs []string
	svc := &mockGetPostService{
		getPostsFunc: func(ctx context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
			gotURIs = req.URIs
			// Service preserves request order, including duplicates.
			return []*posts.PostResult{
				{Post: &posts.PostView{URI: dupURI, CID: "cid1"}},
				{Post: &posts.PostView{URI: dupURI, CID: "cid1"}},
			}, nil
		},
	}
	h := NewGetHandler(svc, nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.post.get?uris="+dupURI+"&uris="+dupURI, nil)
	h.HandleGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(gotURIs) != 2 || gotURIs[0] != dupURI || gotURIs[1] != dupURI {
		t.Errorf("service got URIs = %v, want both entries = %q", gotURIs, dupURI)
	}

	var resp struct {
		Posts []map[string]interface{} `json:"posts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v (body: %s)", err, rec.Body.String())
	}
	if len(resp.Posts) != 2 {
		t.Fatalf("expected 2 posts (one per result, order preserved), got %d", len(resp.Posts))
	}
	for i, p := range resp.Posts {
		if p["uri"] != dupURI {
			t.Errorf("posts[%d].uri = %v, want %q", i, p["uri"], dupURI)
		}
	}
}
