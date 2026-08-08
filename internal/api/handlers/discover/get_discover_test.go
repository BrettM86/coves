package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/discover"
)

// fakeDiscoverService captures the request the handler built. Nothing here
// reaches a database, so the file is T0.
type fakeDiscoverService struct {
	got *discover.GetDiscoverRequest
}

func (f *fakeDiscoverService) GetDiscover(ctx context.Context, req discover.GetDiscoverRequest) (*discover.DiscoverResponse, error) {
	captured := req
	f.got = &captured
	return &discover.DiscoverResponse{Feed: []*discover.FeedViewPost{}}, nil
}

func getDiscover(t *testing.T, query string, ctxDID string) (*httptest.ResponseRecorder, *fakeDiscoverService) {
	t.Helper()

	svc := &fakeDiscoverService{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?"+query, nil)
	if ctxDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), ctxDID))
	}

	NewGetDiscoverHandler(svc, nil, nil).HandleGetDiscover(rec, req)
	return rec, svc
}

// TestGetDiscover_ViewerDIDComesFromTheAuthMiddleware is the provenance pin for
// the public feed, and it is the one that matters most: getDiscover is the
// UNAUTHENTICATED surface, spanning every community.
//
// ViewerDID now unlocks the author carve-out inside visiblePostsJoin
// (`p.author_did = $viewer`), so a viewer string taken from the query string
// would let an anonymous caller enumerate any author's pending, rejected and
// removed posts across the whole network from one endpoint. The assignment lives
// inside parseRequest — the same function that reads the query — so nothing but
// this test stands between the current correct code and the "preview as user"
// mutation a reviewer demonstrated.
func TestGetDiscover_ViewerDIDComesFromTheAuthMiddleware(t *testing.T) {
	const adversarialQuery = "sort=new" +
		"&viewer=did:plc:victimauthor" +
		"&viewerDid=did:plc:victimauthor" +
		"&viewer_did=did:plc:victimauthor" +
		"&actor=did:plc:victimauthor" +
		"&author=did:plc:victimauthor" +
		"&as=did:plc:victimauthor"

	t.Run("an unauthenticated request carries no viewer, whatever it asks for", func(t *testing.T) {
		t.Parallel()

		rec, svc := getDiscover(t, adversarialQuery, "")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got == nil {
			t.Fatal("the service was never called")
		}
		if svc.got.ViewerDID != "" {
			t.Errorf("ViewerDID = %q for an UNAUTHENTICATED request, want \"\". A query parameter reached the viewer "+
				"identity, which means the anonymous internet can name any author and read that author's unadmitted "+
				"posts through the visibility predicate's author branch", svc.got.ViewerDID)
		}
	})

	t.Run("an authenticated request carries the context DID, and no parameter displaces it", func(t *testing.T) {
		t.Parallel()

		const authenticated = "did:plc:realsessionviewer"
		rec, svc := getDiscover(t, adversarialQuery, authenticated)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got == nil {
			t.Fatal("the service was never called")
		}
		if svc.got.ViewerDID != authenticated {
			t.Errorf("ViewerDID = %q, want %q — the viewer identity must come from the auth middleware and from "+
				"nowhere else", svc.got.ViewerDID, authenticated)
		}
	})
}

// TestGetDiscover_ParsesQueryParameters pins the parameter defaults alongside the
// provenance rule.
func TestGetDiscover_ParsesQueryParameters(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		_, svc := getDiscover(t, "", "")
		if svc.got.Sort != "hot" {
			t.Errorf("Sort = %q, want %q", svc.got.Sort, "hot")
		}
		if svc.got.Limit != 15 {
			t.Errorf("Limit = %d, want 15", svc.got.Limit)
		}
		if svc.got.Cursor != nil {
			t.Errorf("Cursor = %q, want nil", *svc.got.Cursor)
		}
	})

	t.Run("top sort defaults the timeframe", func(t *testing.T) {
		t.Parallel()
		_, svc := getDiscover(t, "sort=top", "")
		if svc.got.Timeframe != "day" {
			t.Errorf("Timeframe = %q, want %q", svc.got.Timeframe, "day")
		}
	})

	t.Run("explicit values are forwarded", func(t *testing.T) {
		t.Parallel()
		_, svc := getDiscover(t, "sort=new&limit=7&cursor=xyz", "")
		if svc.got.Sort != "new" {
			t.Errorf("Sort = %q, want %q", svc.got.Sort, "new")
		}
		if svc.got.Limit != 7 {
			t.Errorf("Limit = %d, want 7", svc.got.Limit)
		}
		if svc.got.Cursor == nil || *svc.got.Cursor != "xyz" {
			t.Errorf("Cursor = %v, want %q", svc.got.Cursor, "xyz")
		}
	})
}

// TestGetDiscover_RejectsNonGET keeps the query surface a query.
func TestGetDiscover_RejectsNonGET(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			svc := &fakeDiscoverService{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/xrpc/social.coves.feed.getDiscover", nil)

			NewGetDiscoverHandler(svc, nil, nil).HandleGetDiscover(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if svc.got != nil {
				t.Error("the service was called for a non-GET request")
			}
		})
	}
}

var _ discover.Service = (*fakeDiscoverService)(nil)
