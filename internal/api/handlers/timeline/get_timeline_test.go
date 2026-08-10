package timeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/timeline"
)

// fakeTimelineService captures the request the handler built. No infrastructure,
// so this file is T0.
type fakeTimelineService struct {
	got *timeline.GetTimelineRequest
}

func (f *fakeTimelineService) GetTimeline(ctx context.Context, req timeline.GetTimelineRequest) (*timeline.TimelineResponse, error) {
	captured := req
	f.got = &captured
	return &timeline.TimelineResponse{Feed: []*timeline.FeedViewPost{}}, nil
}

func getTimeline(t *testing.T, query string, ctxDID string) (*httptest.ResponseRecorder, *fakeTimelineService) {
	t.Helper()

	svc := &fakeTimelineService{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?"+query, nil)
	if ctxDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), ctxDID))
	}

	NewGetTimelineHandler(svc, nil, nil).HandleGetTimeline(rec, req)
	return rec, svc
}

// TestGetTimeline_UserDIDComesFromTheAuthMiddleware is the provenance pin for the
// subscribed feed.
//
// The timeline's UserDID is both the subscription key AND the viewer identity the
// repository binds into the read-path visibility predicate, where it unlocks the
// author carve-out (`p.author_did = $viewer`). A query-supplied value would
// therefore do two things at once: read a stranger's subscriptions, and surface
// that stranger's pending/rejected/removed posts. The endpoint is RequireAuth, so
// the pin has a third obligation the other feeds do not — an unauthenticated call
// must be refused outright rather than served with an empty viewer.
func TestGetTimeline_UserDIDComesFromTheAuthMiddleware(t *testing.T) {
	const adversarialQuery = "sort=new" +
		"&viewer=did:plc:victimauthor" +
		"&viewerDid=did:plc:victimauthor" +
		"&viewer_did=did:plc:victimauthor" +
		"&user=did:plc:victimauthor" +
		"&userDid=did:plc:victimauthor" +
		"&actor=did:plc:victimauthor" +
		"&as=did:plc:victimauthor"

	t.Run("an unauthenticated request is refused, whatever it asks for", func(t *testing.T) {
		t.Parallel()

		rec, svc := getTimeline(t, adversarialQuery, "")

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got != nil {
			t.Errorf("the service ran for an unauthenticated request with UserDID = %q; a query parameter became an "+
				"identity, which reads a stranger's subscriptions AND their unadmitted posts", svc.got.UserDID)
		}
	})

	t.Run("an authenticated request carries the context DID, and no parameter displaces it", func(t *testing.T) {
		t.Parallel()

		const authenticated = "did:plc:realsessionviewer"
		rec, svc := getTimeline(t, adversarialQuery, authenticated)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got == nil {
			t.Fatal("the service was never called")
		}
		if svc.got.UserDID != authenticated {
			t.Errorf("UserDID = %q, want %q — the identity must come from the auth middleware and from nowhere else",
				svc.got.UserDID, authenticated)
		}
	})

	t.Run("a non-DID context value is refused", func(t *testing.T) {
		t.Parallel()

		// The handler requires a did: prefix, so a context value that is not a DID
		// (a handle, a truncated token) cannot become a viewer identity.
		rec, svc := getTimeline(t, "sort=new", "not-a-did")

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got != nil {
			t.Errorf("the service ran with a non-DID identity %q", svc.got.UserDID)
		}
	})
}

// TestGetTimeline_ParsesQueryParameters pins the defaults next to the provenance
// rule so the parameter surface is described in one place.
func TestGetTimeline_ParsesQueryParameters(t *testing.T) {
	t.Parallel()

	const viewer = "did:plc:timelineviewer"

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		_, svc := getTimeline(t, "", viewer)
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
		_, svc := getTimeline(t, "sort=top", viewer)
		if svc.got.Timeframe != "day" {
			t.Errorf("Timeframe = %q, want %q", svc.got.Timeframe, "day")
		}
	})

	t.Run("explicit values are forwarded", func(t *testing.T) {
		t.Parallel()
		_, svc := getTimeline(t, "sort=new&limit=9&cursor=pqr", viewer)
		if svc.got.Sort != "new" {
			t.Errorf("Sort = %q, want %q", svc.got.Sort, "new")
		}
		if svc.got.Limit != 9 {
			t.Errorf("Limit = %d, want 9", svc.got.Limit)
		}
		if svc.got.Cursor == nil || *svc.got.Cursor != "pqr" {
			t.Errorf("Cursor = %v, want %q", svc.got.Cursor, "pqr")
		}
	})
}

// TestGetTimeline_RejectsNonGET keeps the query surface a query.
func TestGetTimeline_RejectsNonGET(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			svc := &fakeTimelineService{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/xrpc/social.coves.feed.getTimeline", nil)
			req = req.WithContext(middleware.SetTestUserDID(req.Context(), "did:plc:timelineviewer"))

			NewGetTimelineHandler(svc, nil, nil).HandleGetTimeline(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if svc.got != nil {
				t.Error("the service was called for a non-GET request")
			}
		})
	}
}

var _ timeline.Service = (*fakeTimelineService)(nil)
