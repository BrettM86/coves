package comments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
)

// The parameter surface of social.coves.community.comment.getComments.
//
// It is a query endpoint, so every one of these values arrives as a string from
// an untrusted client and the handler is the only thing standing between them
// and the service. That makes this the right tier for the breadth: the pipeline
// contract in tests/e2e proves the endpoint is routed, guarded and serving real
// data (§3.4 rule 3 keeps behavioural breadth out of it), while the matrix of
// what a client may and may not ask for belongs here, where it costs no
// infrastructure and every case is one function call.
//
// Two things this file is careful about, both learned from the endpoint's
// shape:
//
//   - Rejections and CLAMPS are different answers, and the handler chooses
//     differently per parameter. depth=-1 is a 400 while depth=500 is passed
//     through for the service to clamp; limit=0 is a 400 while limit=101 is a
//     400. Asserting only "it did not crash" would let any of those flip.
//   - The request the service receives is asserted, not just the status code. A
//     handler that answers 200 while dropping parentRkey is invisible to a
//     status-only test and would silently serve whole threads where a client
//     asked for one subtree.

// fakeCommentService records the request it was handed and answers with a
// canned response, so a test can assert on what the handler PARSED rather than
// only on what it returned.
type fakeCommentService struct {
	got  *GetCommentsRequest
	resp *comments.GetCommentsResponse
	err  error
}

func (f *fakeCommentService) GetComments(_ *http.Request, req *GetCommentsRequest) (*comments.GetCommentsResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &comments.GetCommentsResponse{Comments: []*comments.ThreadViewComment{}}, nil
}

// getComments issues a GET with the given raw query string and returns the
// recorder plus the service's view of the request.
func getComments(t *testing.T, query string) (*httptest.ResponseRecorder, *fakeCommentService) {
	t.Helper()
	svc := &fakeCommentService{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.comment.getComments?"+query, nil)
	NewGetCommentsHandler(svc).HandleGetComments(rec, req)
	return rec, svc
}

const testPostURI = "at://did:plc:aaaaaaaacommenthandlertst/social.coves.community.post/3lbtestpost01"

func TestGetComments_RejectsBadParameters(t *testing.T) {
	// Every case here is a 400 InvalidRequest, and every one of them must reach
	// the client BEFORE the service is called — an out-of-range limit that got
	// as far as the query is an unbounded read (§ "Limit resource access").
	tests := []struct {
		name  string
		query string
	}{
		{"post is required", "depth=1"},
		{"post cannot be empty", "post="},
		{"depth must be a number", "post=" + testPostURI + "&depth=deep"},
		{"depth cannot be negative", "post=" + testPostURI + "&depth=-1"},
		{"limit must be a number", "post=" + testPostURI + "&limit=many"},
		{"limit cannot be zero", "post=" + testPostURI + "&limit=0"},
		{"limit cannot be negative", "post=" + testPostURI + "&limit=-5"},
		{"limit cannot exceed 100", "post=" + testPostURI + "&limit=101"},
		{"sort must be a known value", "post=" + testPostURI + "&sort=controversial"},
		{"timeframe requires sort=top", "post=" + testPostURI + "&timeframe=day"},
		{"timeframe requires sort=top, not sort=new", "post=" + testPostURI + "&sort=new&timeframe=day"},
		{"timeframe must be a known value", "post=" + testPostURI + "&sort=top&timeframe=fortnight"},
		{"parentRkey must be a valid record key", "post=" + testPostURI + "&parentRkey=not/a/key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, svc := getComments(t, tt.query)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if body.Error != "InvalidRequest" {
				t.Errorf("error code = %q, want %q", body.Error, "InvalidRequest")
			}
			if svc.got != nil {
				t.Errorf("the service was called with %+v; a rejected request must not reach it", svc.got)
			}
		})
	}
}

func TestGetComments_AcceptsAndParsesParameters(t *testing.T) {
	// The mirror image: what a client is allowed to ask for, and what the
	// service is handed as a result. want is compared field by field so a
	// failure names the parameter that was dropped.
	cursor := "cursor-from-a-previous-page"
	tests := []struct {
		name  string
		query string
		want  GetCommentsRequest
	}{
		{
			// Defaults are part of the contract: a client that sends nothing but
			// the post gets a bounded page and a bounded depth, not everything.
			name:  "defaults when only post is given",
			query: "post=" + testPostURI,
			want:  GetCommentsRequest{PostURI: testPostURI, Depth: 10, Limit: 50},
		},
		{
			name:  "depth zero is a flat list, not a missing value",
			query: "post=" + testPostURI + "&depth=0",
			want:  GetCommentsRequest{PostURI: testPostURI, Depth: 0, Limit: 50},
		},
		{
			// Over-large depth is CLAMPED by the service rather than rejected by
			// the handler, unlike limit. Pinned because the asymmetry is easy to
			// "tidy up" in either direction.
			name:  "large depth is passed through for the service to clamp",
			query: "post=" + testPostURI + "&depth=500",
			want:  GetCommentsRequest{PostURI: testPostURI, Depth: 500, Limit: 50},
		},
		{
			name:  "limit at the maximum is allowed",
			query: "post=" + testPostURI + "&limit=100",
			want:  GetCommentsRequest{PostURI: testPostURI, Depth: 10, Limit: 100},
		},
		{
			name:  "sort=hot",
			query: "post=" + testPostURI + "&sort=hot",
			want:  GetCommentsRequest{PostURI: testPostURI, Sort: "hot", Depth: 10, Limit: 50},
		},
		{
			name:  "sort=new",
			query: "post=" + testPostURI + "&sort=new",
			want:  GetCommentsRequest{PostURI: testPostURI, Sort: "new", Depth: 10, Limit: 50},
		},
		{
			name:  "sort=top with every timeframe the endpoint takes",
			query: "post=" + testPostURI + "&sort=top&timeframe=all",
			want:  GetCommentsRequest{PostURI: testPostURI, Sort: "top", Timeframe: "all", Depth: 10, Limit: 50},
		},
		{
			name:  "a subtree request carries its parentRkey through",
			query: "post=" + testPostURI + "&parentRkey=3lbtestrkey01&depth=2&limit=25",
			want:  GetCommentsRequest{PostURI: testPostURI, ParentRkey: "3lbtestrkey01", Depth: 2, Limit: 25},
		},
		{
			name:  "a cursor is carried through",
			query: "post=" + testPostURI + "&cursor=" + cursor,
			want:  GetCommentsRequest{PostURI: testPostURI, Depth: 10, Limit: 50, Cursor: &cursor},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, svc := getComments(t, tt.query)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			if svc.got == nil {
				t.Fatal("the handler answered 200 without calling the service")
			}
			assertRequest(t, *svc.got, tt.want)
		})
	}

	// Timeframes are enumerated separately rather than as nine near-identical
	// table rows: the point is that the whole set is accepted, not that each one
	// parses differently.
	for _, timeframe := range []string{"hour", "day", "week", "month", "year", "all"} {
		t.Run("timeframe="+timeframe, func(t *testing.T) {
			t.Parallel()
			rec, svc := getComments(t, "post="+testPostURI+"&sort=top&timeframe="+timeframe)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			if svc.got.Timeframe != timeframe {
				t.Errorf("timeframe = %q, want %q", svc.got.Timeframe, timeframe)
			}
		})
	}
}

// assertRequest compares what the service received against what the query
// asked for, field by field.
func assertRequest(t *testing.T, got, want GetCommentsRequest) {
	t.Helper()
	if got.PostURI != want.PostURI {
		t.Errorf("PostURI = %q, want %q", got.PostURI, want.PostURI)
	}
	if got.ParentRkey != want.ParentRkey {
		t.Errorf("ParentRkey = %q, want %q", got.ParentRkey, want.ParentRkey)
	}
	if got.Sort != want.Sort {
		t.Errorf("Sort = %q, want %q", got.Sort, want.Sort)
	}
	if got.Timeframe != want.Timeframe {
		t.Errorf("Timeframe = %q, want %q", got.Timeframe, want.Timeframe)
	}
	if got.Depth != want.Depth {
		t.Errorf("Depth = %d, want %d", got.Depth, want.Depth)
	}
	if got.Limit != want.Limit {
		t.Errorf("Limit = %d, want %d", got.Limit, want.Limit)
	}
	switch {
	case want.Cursor == nil && got.Cursor != nil:
		t.Errorf("Cursor = %q, want nil — an absent cursor must not become an empty-string page token",
			*got.Cursor)
	case want.Cursor != nil && got.Cursor == nil:
		t.Errorf("Cursor = nil, want %q", *want.Cursor)
	case want.Cursor != nil && *got.Cursor != *want.Cursor:
		t.Errorf("Cursor = %q, want %q", *got.Cursor, *want.Cursor)
	}
}

func TestGetComments_ViewerDIDComesFromTheAuthMiddleware(t *testing.T) {
	// The viewer DID is what populates per-comment vote state and applies block
	// filtering, and it is read from the request context rather than the query —
	// so a client cannot ask for another user's viewer state by naming them.
	// Both halves are asserted: present when the middleware set it, absent when
	// it did not.
	t.Run("anonymous requests carry no viewer", func(t *testing.T) {
		t.Parallel()
		_, svc := getComments(t, "post="+testPostURI)
		if svc.got.ViewerDID != nil {
			t.Errorf("ViewerDID = %q for an unauthenticated request, want nil", *svc.got.ViewerDID)
		}
	})

	t.Run("an authenticated request carries the DID from the context", func(t *testing.T) {
		t.Parallel()
		const viewer = "did:plc:aaaaaaaaviewerofthisthread"
		svc := &fakeCommentService{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/xrpc/social.coves.community.comment.getComments?post="+testPostURI, nil)
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, viewer))

		NewGetCommentsHandler(svc).HandleGetComments(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.got.ViewerDID == nil {
			t.Fatal("ViewerDID = nil; the DID the auth middleware put on the context was dropped")
		}
		if *svc.got.ViewerDID != viewer {
			t.Errorf("ViewerDID = %q, want %q", *svc.got.ViewerDID, viewer)
		}
	})
}

func TestGetComments_RejectsNonGET(t *testing.T) {
	// A query endpoint that also answered POST would be a CSRF-shaped surface
	// and a lexicon violation; XRPC queries are GET.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			svc := &fakeCommentService{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method,
				"/xrpc/social.coves.community.comment.getComments?post="+testPostURI, nil)

			NewGetCommentsHandler(svc).HandleGetComments(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if svc.got != nil {
				t.Error("the service was called for a non-GET request")
			}
		})
	}
}

func TestGetComments_ServiceErrorsKeepTheirCodes(t *testing.T) {
	// The two not-founds this endpoint can produce are a client contract: a
	// stale permalink (ParentNotFound) means the thread still exists and the
	// comment does not, while RootNotFound means the whole post is gone. A
	// client that could not tell them apart would send a reader to the wrong
	// page.
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"the post is not indexed", comments.ErrRootNotFound, http.StatusNotFound, "RootNotFound"},
		{"the parentRkey names nothing", comments.ErrParentNotFound, http.StatusNotFound, "ParentNotFound"},
		{"a mismatched cursor", comments.ErrInvalidCursor, http.StatusBadRequest, "InvalidRequest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := &fakeCommentService{err: tt.err}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				"/xrpc/social.coves.community.comment.getComments?post="+testPostURI, nil)

			NewGetCommentsHandler(svc).HandleGetComments(rec, req)
			assertXRPCError(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

// An unrecognised validation error becomes a 500, which is HALF of the
// malformed-URI defect — and this test can only see that half. Read the scope
// note before trusting it.
//
// # WHAT IS ACTUALLY BROKEN
//
// `post` is client-supplied and the handler validates only that it is present.
// The at:// check lives in the service (comments.validateGetCommentsRequest) and
// returns a bare errors.New, which GetComments wraps as "invalid request: ...".
// That value matches neither a sentinel rule nor comments.IsValidationError, so
// errorMapper falls through to its 500 and a client typo is reported as a
// server fault. Filed as 2026-07-29-getcomments-malformed-uri-returns-500.
//
// # WHY THIS TEST CANNOT BE THE ONE THAT CATCHES THE FIX
//
// The defect has two halves — a service that returns an unmapped error, and a
// mapper that answers 500 when it meets one — and this test drives a FAKE
// service with a hand-built error, so it exercises the second half only. The
// likely fix is in the first half (return a sentinel from the service), and
// after it lands this test would still pass: it would still be handing the
// mapper an unrecognised error, and the mapper would still, correctly, answer
// 500 to one. So it must not claim to fail on fix.
//
// It is kept because the behaviour it does pin is worth pinning on its own —
// "an error the mapper does not recognise is a 500, not a 200 or a panic" is a
// real contract, and the errors_test.go catch-all rules are what would change
// if it stopped being true.
//
// THE PIN THAT FIRES ON FIX is the end-to-end one, in
// tests/e2e/comment_contract_test.go's TestCommentAPIContract — it sends a real
// malformed URI to the shipped binary, so it goes red the moment either half is
// corrected. If you are fixing this defect, that is the test to expect.
func TestGetComments_UnmappedValidationErrorIs500(t *testing.T) {
	// The exact string comments.GetComments produces today for a malformed
	// post URI, so the case this stands in for is unambiguous.
	svc := &fakeCommentService{err: errors.New("invalid request: invalid AT-URI format: must start with 'at://'")}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.comment.getComments?post=not-an-at-uri", nil)

	NewGetCommentsHandler(svc).HandleGetComments(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: an error the mapper cannot classify must still produce a "+
			"well-formed server error (body: %s)", rec.Code, rec.Body.String())
	}
}
