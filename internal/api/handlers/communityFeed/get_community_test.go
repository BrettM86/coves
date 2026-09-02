package communityFeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communityFeeds"
)

// fakeCommunityFeedService captures the request the handler built so a test can
// assert on it. It never reaches a database, so the whole file is T0.
type fakeCommunityFeedService struct {
	got     *communityFeeds.GetCommunityFeedRequest
	callErr error

	searchGot      *communityFeeds.SearchPostsRequest
	searchResponse *communityFeeds.FeedResponse
	searchErr      error
}

func (f *fakeCommunityFeedService) GetCommunityFeed(
	ctx context.Context,
	req communityFeeds.GetCommunityFeedRequest,
) (*communityFeeds.FeedResponse, error) {
	captured := req
	f.got = &captured
	if f.callErr != nil {
		return nil, f.callErr
	}
	return &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{}}, nil
}

func (f *fakeCommunityFeedService) SearchPosts(_ context.Context, req communityFeeds.SearchPostsRequest) (*communityFeeds.FeedResponse, error) {
	captured := req
	f.searchGot = &captured
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.searchResponse != nil {
		return f.searchResponse, nil
	}
	return &communityFeeds.FeedResponse{Feed: []*communityFeeds.FeedViewPost{}}, nil
}

// getCommunity drives the handler over a raw query string and returns the
// recorder plus the service that captured the request.
func getCommunity(t *testing.T, query string, ctxDID string) (*httptest.ResponseRecorder, *fakeCommunityFeedService) {
	t.Helper()

	svc := &fakeCommunityFeedService{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.communityFeed.getCommunity?"+query, nil)
	if ctxDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), ctxDID))
	}

	NewGetCommunityHandler(svc, nil, nil).HandleGetCommunity(rec, req)
	return rec, svc
}

// TestGetCommunity_ViewerDIDComesFromTheAuthMiddleware is a PROVENANCE pin, and
// under author-owned posts it is a security pin rather than a correctness one.
//
// ViewerDID used to drive block filtering only, so a wrong value was a
// self-inflicted wound. It now also unlocks the author carve-out inside the
// read-path visibility predicate (`p.author_did = $viewer` in visiblePostsJoin):
// whoever controls that string sees that author's PENDING, REJECTED and REMOVED
// posts — content no community has agreed to carry. The assignment lives inside
// parseRequest, the same function that reads the query string, so the only thing
// standing between an unauthenticated caller and any author's unadmitted posts is
// that nobody adds `?viewer=` to that function. A reviewer demonstrated the
// mutation: three lines of "preview as user" in parseRequest, whole suite still
// green.
//
// This test is that missing tripwire: no auth context plus adversarial query
// parameters must produce an EMPTY viewer, and an authenticated request must
// carry the context's DID with no query parameter able to displace it.
func TestGetCommunity_ViewerDIDComesFromTheAuthMiddleware(t *testing.T) {
	// Every name a "preview as user" parameter might plausibly be given, plus the
	// ones this endpoint already reads, so a mutation cannot hide behind a
	// spelling this test forgot.
	const adversarialQuery = "community=did:plc:targetcommunity" +
		"&viewer=did:plc:victimauthor" +
		"&viewerDid=did:plc:victimauthor" +
		"&viewer_did=did:plc:victimauthor" +
		"&actor=did:plc:victimauthor" +
		"&author=did:plc:victimauthor" +
		"&as=did:plc:victimauthor"

	t.Run("an unauthenticated request carries no viewer, whatever it asks for", func(t *testing.T) {
		t.Parallel()

		rec, svc := getCommunity(t, adversarialQuery, "")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got == nil {
			t.Fatal("the service was never called")
		}
		if svc.got.ViewerDID != "" {
			t.Errorf("ViewerDID = %q for an UNAUTHENTICATED request, want \"\". A query parameter reached the viewer "+
				"identity: the anonymous internet can now name any author and read that author's pending, rejected and "+
				"removed posts through visiblePostsJoin's author carve-out", svc.got.ViewerDID)
		}
	})

	t.Run("an authenticated request carries the context DID, and no parameter displaces it", func(t *testing.T) {
		t.Parallel()

		const authenticated = "did:plc:realsessionviewer"
		rec, svc := getCommunity(t, adversarialQuery, authenticated)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got == nil {
			t.Fatal("the service was never called")
		}
		if svc.got.ViewerDID != authenticated {
			t.Errorf("ViewerDID = %q, want %q — the viewer identity must come from the auth middleware's context "+
				"value and from nowhere else", svc.got.ViewerDID, authenticated)
		}
	})
}

// TestGetCommunity_ParsesQueryParameters pins the rest of parseRequest so the
// provenance test above is not the only thing describing this handler: a
// defaulted sort/limit is what makes an unbounded feed read impossible.
func TestGetCommunity_ParsesQueryParameters(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		_, svc := getCommunity(t, "community=did:plc:c", "")
		if svc.got.Sort != "hot" {
			t.Errorf("Sort = %q, want %q", svc.got.Sort, "hot")
		}
		if svc.got.Limit != 15 {
			t.Errorf("Limit = %d, want 15", svc.got.Limit)
		}
		if svc.got.Cursor != nil {
			t.Errorf("Cursor = %q, want nil — an absent cursor must not become an empty page token", *svc.got.Cursor)
		}
		if svc.got.Timeframe != "" {
			t.Errorf("Timeframe = %q, want empty for a non-top sort", svc.got.Timeframe)
		}
	})

	t.Run("top sort defaults the timeframe", func(t *testing.T) {
		t.Parallel()
		_, svc := getCommunity(t, "community=did:plc:c&sort=top", "")
		if svc.got.Timeframe != "day" {
			t.Errorf("Timeframe = %q, want %q", svc.got.Timeframe, "day")
		}
	})

	t.Run("explicit values are forwarded", func(t *testing.T) {
		t.Parallel()
		_, svc := getCommunity(t, "community=cats.coves.social&sort=new&limit=42&cursor=abc", "")
		if svc.got.Community != "cats.coves.social" {
			t.Errorf("Community = %q, want %q", svc.got.Community, "cats.coves.social")
		}
		if svc.got.Sort != "new" {
			t.Errorf("Sort = %q, want %q", svc.got.Sort, "new")
		}
		if svc.got.Limit != 42 {
			t.Errorf("Limit = %d, want 42", svc.got.Limit)
		}
		if svc.got.Cursor == nil || *svc.got.Cursor != "abc" {
			t.Errorf("Cursor = %v, want %q", svc.got.Cursor, "abc")
		}
	})
}

// TestGetCommunity_RejectsNonGET keeps the query surface a query: an XRPC query
// answering POST would be a CSRF-shaped write surface.
func TestGetCommunity_RejectsNonGET(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			svc := &fakeCommunityFeedService{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/xrpc/social.coves.communityFeed.getCommunity?community=did:plc:c", nil)

			NewGetCommunityHandler(svc, nil, nil).HandleGetCommunity(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if svc.got != nil {
				t.Error("the service was called for a non-GET request")
			}
		})
	}
}

// compile-time guard: the fake must stay a communityFeeds.Service.
var _ communityFeeds.Service = (*fakeCommunityFeedService)(nil)
