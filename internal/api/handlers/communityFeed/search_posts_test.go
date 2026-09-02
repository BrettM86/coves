package communityFeed

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
)

// searchPosts drives the handler over a raw query string and returns the
// recorder plus the service that captured the request.
func searchPosts(t *testing.T, query string, ctxDID string) (*httptest.ResponseRecorder, *fakeCommunityFeedService) {
	t.Helper()

	svc := &fakeCommunityFeedService{}
	return runSearchPosts(t, svc, http.MethodGet, query, ctxDID), svc
}

func runSearchPosts(t *testing.T, svc *fakeCommunityFeedService, method, query, ctxDID string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/xrpc/social.coves.feed.searchPosts?"+query, nil)
	if ctxDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), ctxDID))
	}
	NewSearchPostsHandler(svc, nil, nil).HandleSearchPosts(rec, req)
	return rec
}

func TestSearchPosts_ParsesTheQueryString(t *testing.T) {
	t.Parallel()

	t.Run("explicit values are forwarded", func(t *testing.T) {
		t.Parallel()
		rec, svc := searchPosts(t, "q=rust&community=gaming&sort=new&timeframe=week&limit=20&cursor=abc", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.searchGot == nil {
			t.Fatal("the service was never called")
		}
		got := svc.searchGot
		if got.Query != "rust" {
			t.Errorf("Query = %q, want %q", got.Query, "rust")
		}
		if got.Community != "gaming" {
			t.Errorf("Community = %q, want %q", got.Community, "gaming")
		}
		if got.Sort != "new" {
			t.Errorf("Sort = %q, want %q", got.Sort, "new")
		}
		if got.Timeframe != "week" {
			t.Errorf("Timeframe = %q, want %q", got.Timeframe, "week")
		}
		if got.Limit != 20 {
			t.Errorf("Limit = %d, want 20", got.Limit)
		}
		if got.Cursor == nil || *got.Cursor != "abc" {
			t.Errorf("Cursor = %v, want %q", got.Cursor, "abc")
		}
	})

	t.Run("absent optional values reach the service as zero values", func(t *testing.T) {
		t.Parallel()
		rec, svc := searchPosts(t, "q=rust", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.searchGot == nil {
			t.Fatal("the service was never called")
		}
		got := svc.searchGot
		if got.Query != "rust" {
			t.Errorf("Query = %q, want %q", got.Query, "rust")
		}
		if got.Community != "" || got.Sort != "" || got.Timeframe != "" {
			t.Errorf("optional strings reached the service as Community=%q Sort=%q Timeframe=%q, want all empty so the service owns defaults", got.Community, got.Sort, got.Timeframe)
		}
		if got.Limit != 0 {
			t.Errorf("Limit = %d, want 0. The handler must not duplicate the service's default of 15", got.Limit)
		}
		if got.Cursor != nil {
			t.Errorf("Cursor = %v, want nil for an absent page token", got.Cursor)
		}
	})
}

func TestSearchPosts_RejectsMalformedInput(t *testing.T) {
	t.Parallel()

	t.Run("missing q", func(t *testing.T) {
		t.Parallel()
		rec, svc := searchPosts(t, "community=gaming", "")
		assertSearchPostsError(t, rec, http.StatusBadRequest, "InvalidRequest")
		if svc.searchGot != nil {
			t.Errorf("the service was called for a request with no q: %+v", *svc.searchGot)
		}
	})

	t.Run("non-numeric limit", func(t *testing.T) {
		t.Parallel()
		rec, svc := searchPosts(t, "q=rust&limit=abc", "")
		assertSearchPostsError(t, rec, http.StatusBadRequest, "InvalidRequest")
		if svc.searchGot != nil {
			t.Errorf("the service was called after a non-numeric limit: %+v", *svc.searchGot)
		}
	})

	for _, limit := range []string{"0", "-1"} {
		limit := limit
		t.Run("explicit limit "+limit+" below minimum", func(t *testing.T) {
			t.Parallel()
			rec, svc := searchPosts(t, "q=x&limit="+limit, "")
			assertSearchPostsError(t, rec, http.StatusBadRequest, "InvalidRequest")
			if svc.searchGot != nil {
				t.Errorf("the service was called after an explicit limit below 1: limit=%s request=%+v", limit, *svc.searchGot)
			}
		})
	}

	t.Run("POST", func(t *testing.T) {
		t.Parallel()
		svc := &fakeCommunityFeedService{}
		rec := runSearchPosts(t, svc, http.MethodPost, "q=rust", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
		if svc.searchGot != nil {
			t.Error("the service was called for a POST to an XRPC query endpoint")
		}
	})
}

func TestSearchPosts_MapsServiceErrors(t *testing.T) {
	t.Parallel()

	internalFailure := errors.New("postgres password leaked from driver")
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "community not found", err: communityFeeds.ErrCommunityNotFound, wantStatus: http.StatusNotFound, wantCode: "CommunityNotFound"},
		{name: "validation error", err: communityFeeds.NewValidationError("q", "query is required"), wantStatus: http.StatusBadRequest, wantCode: "InvalidRequest"},
		{name: "invalid cursor", err: communityFeeds.ErrInvalidCursor, wantStatus: http.StatusBadRequest, wantCode: "InvalidCursor"},
		{name: "internal error", err: internalFailure, wantStatus: http.StatusInternalServerError, wantCode: "InternalServerError"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			svc := &fakeCommunityFeedService{searchErr: test.err}
			rec := runSearchPosts(t, svc, http.MethodGet, "q=rust", "")
			assertSearchPostsError(t, rec, test.wantStatus, test.wantCode)
			if test.err == internalFailure && strings.Contains(rec.Body.String(), internalFailure.Error()) {
				t.Errorf("the 500 response leaked internal detail: %s", rec.Body.String())
			}
		})
	}
}

func TestSearchPosts_WritesTheFeed(t *testing.T) {
	t.Parallel()

	t.Run("rows and cursor are encoded", func(t *testing.T) {
		t.Parallel()
		cursor := "next-search-page"
		response := &communityFeeds.FeedResponse{
			Feed: []*communityFeeds.FeedViewPost{
				{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.postv2/1"}},
				{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.postv2/2"}},
			},
			Cursor: &cursor,
		}
		svc := &fakeCommunityFeedService{searchResponse: response}
		rec := runSearchPosts(t, svc, http.MethodGet, "q=rust", "")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body communityFeeds.FeedResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
		}
		if len(body.Feed) != 2 {
			t.Errorf("response feed has %d items, want 2", len(body.Feed))
		}
		if body.Cursor == nil || *body.Cursor != cursor {
			t.Errorf("response cursor = %v, want %q", body.Cursor, cursor)
		}
	})

	t.Run("an empty feed is an array with no cursor key", func(t *testing.T) {
		t.Parallel()
		svc := &fakeCommunityFeedService{searchResponse: &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{}}}
		rec := runSearchPosts(t, svc, http.MethodGet, "q=rust", "")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
		}
		feed, ok := envelope["feed"]
		if !ok || string(feed) != "[]" {
			t.Errorf("feed JSON = %s, want [] so clients never receive null", feed)
		}
		if cursor, ok := envelope["cursor"]; ok {
			t.Errorf("empty response contains cursor key %s, want it omitted", cursor)
		}
	})
}

// TestSearchPosts_ViewerDIDComesFromTheAuthMiddleware is a PROVENANCE pin, and
// under author-owned posts it is a security pin rather than a correctness one.
// Search spans every community when no community filter is supplied, so a
// query-controlled viewer is even more dangerous here than on getCommunity.
//
// ViewerDID used to drive block filtering only, so a wrong value was a
// self-inflicted wound. It now also unlocks the author carve-out inside the
// read-path visibility predicate (`p.author_did = $viewer` in visiblePostsJoin):
// whoever controls that string sees that author's PENDING, REJECTED and REMOVED
// posts — content no community has agreed to carry. The assignment belongs next
// to query parsing, so the only thing standing between an unauthenticated caller
// and any author's unadmitted posts is that viewer identity comes exclusively
// from authentication middleware.
//
// This test is that tripwire: no auth context plus adversarial query parameters
// must produce an EMPTY viewer, and an authenticated request must carry the
// context's DID with no query parameter able to displace it.
func TestSearchPosts_ViewerDIDComesFromTheAuthMiddleware(t *testing.T) {
	const adversarialQuery = "q=rust" +
		"&viewer=did:plc:evil" +
		"&viewerDid=did:plc:evil" +
		"&actor=did:plc:evil" +
		"&author=did:plc:evil" +
		"&as=did:plc:evil" +
		"&did=did:plc:evil"

	t.Run("an unauthenticated request carries no viewer, whatever it asks for", func(t *testing.T) {
		t.Parallel()
		rec, svc := searchPosts(t, adversarialQuery, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.searchGot == nil {
			t.Fatal("the service was never called")
		}
		if svc.searchGot.ViewerDID != "" {
			t.Errorf("ViewerDID = %q for an UNAUTHENTICATED request, want \"\". A query parameter reached the viewer identity: the anonymous internet can now name any author and search that author's unadmitted posts across communities", svc.searchGot.ViewerDID)
		}
	})

	t.Run("an authenticated request carries the context DID, and no parameter displaces it", func(t *testing.T) {
		t.Parallel()
		const authenticated = "did:plc:me"
		rec, svc := searchPosts(t, adversarialQuery, authenticated)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.searchGot == nil {
			t.Fatal("the service was never called")
		}
		if svc.searchGot.ViewerDID != authenticated {
			t.Errorf("ViewerDID = %q, want %q — the viewer identity must come from the auth middleware's context value and from nowhere else", svc.searchGot.ViewerDID, authenticated)
		}
	})
}

func assertSearchPostsError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("decode error response: %v (body: %q)", err, rec.Body.String())
		return
	}
	if body.Error != wantCode {
		t.Errorf("error = %q, want %q (body: %s)", body.Error, wantCode, rec.Body.String())
	}
}
