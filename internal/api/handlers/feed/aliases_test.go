package feed

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/discover"
	"Coves/internal/core/feedgen"
)

// The old feed endpoints (getTimeline, getDiscover, getCommunity) are aliases
// of getFeed: each maps sort to a built-in feed and runs the getFeed path over
// the real feedgen service. T0.

type aliasEndpoint string

const (
	aliasTimeline  aliasEndpoint = "social.coves.feed.getTimeline"
	aliasDiscover  aliasEndpoint = "social.coves.feed.getDiscover"
	aliasCommunity aliasEndpoint = "social.coves.communityFeed.getCommunity"
)

const (
	aliasHotItemURI = "at://did:plc:author/social.coves.community.post/hot1"
	aliasNewItemURI = "at://did:plc:author/social.coves.community.post/new1"
	aliasTopItemURI = "at://did:plc:author/social.coves.community.post/top1"
)

// newAliasFixture builds the getFeed fixture with a one-post hot generator.
func newAliasFixture(t *testing.T) *feedFixture {
	t.Helper()
	return newFeedFixture(t, &fakeGenerator{uris: []string{aliasHotItemURI}}, nil)
}

// alias issues GET on the alias endpoint with the given query, as viewerDID
// ("" = anonymous).
func (f *feedFixture) alias(t *testing.T, endpoint aliasEndpoint, query url.Values, viewerDID string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewFeedAliasHandler(f.handler, f.registry)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/"+string(endpoint)+"?"+query.Encode(), nil)
	if viewerDID != "" {
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), viewerDID))
	}
	switch endpoint {
	case aliasTimeline:
		handler.HandleGetTimeline(rec, req)
	case aliasDiscover:
		handler.HandleGetDiscover(rec, req)
	case aliasCommunity:
		handler.HandleGetCommunity(rec, req)
	default:
		t.Fatalf("unknown alias endpoint %q", endpoint)
	}
	return rec
}

// aliasViewerLikeParams names another DID under every parameter a "preview as
// user" feature might use; the viewer must still come only from the auth
// context.
var aliasViewerLikeParams = []string{
	"viewer", viewerTwoDID,
	"viewerDid", viewerTwoDID,
	"viewer_did", viewerTwoDID,
	"user", viewerTwoDID,
	"userDid", viewerTwoDID,
	"actor", viewerTwoDID,
	"author", viewerTwoDID,
	"as", viewerTwoDID,
}

// requestsByRKey is every generator request the fixture's built-ins received,
// keyed by built-in rkey.
func (f *feedFixture) requestsByRKey() map[string][]feedgen.SkeletonRequest {
	return map[string][]feedgen.SkeletonRequest{
		"hot":          f.hot.received(),
		"new":          f.newFeed.received(),
		"top":          f.top.received(),
		"discover-hot": f.discoverHot.received(),
		"other":        f.other.received(),
	}
}

// TestFeedAliases_Mapping: each alias maps sort to a built-in feed and scope
// and passes viewer, community, timeframe and limit through to the generator.
func TestFeedAliases_Mapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint aliasEndpoint
		params   []string
		viewer   string

		wantRKey      string
		wantURIs      []string
		wantScope     string
		wantCommunity string
		wantTimeframe string
		wantLimit     int
	}{
		// getTimeline: scope home.
		{
			"getTimeline sort absent", aliasTimeline, nil, viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline sort hot", aliasTimeline,
			[]string{"sort", "hot"},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline sort new", aliasTimeline,
			[]string{"sort", "new"},
			viewerOneDID,
			"new",
			[]string{aliasNewItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline sort top, timeframe absent", aliasTimeline,
			[]string{"sort", "top"},
			viewerOneDID,
			"top",
			[]string{aliasTopItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline sort top, timeframe week", aliasTimeline,
			[]string{"sort", "top", "timeframe", "week"},
			viewerOneDID,
			"top",
			[]string{aliasTopItemURI},
			"home", "", "week", 15,
		},
		{
			"getTimeline viewer-like parameters, signed in", aliasTimeline,
			append([]string{"sort", "new"}, aliasViewerLikeParams...),
			viewerOneDID,
			"new",
			[]string{aliasNewItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline limit 50", aliasTimeline,
			[]string{"limit", "50"},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"home", "", "day", 50,
		},
		{
			"getTimeline limit 0", aliasTimeline,
			[]string{"limit", "0"},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline limit -3", aliasTimeline,
			[]string{"limit", "-3"},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"home", "", "day", 15,
		},
		{
			"getTimeline limit abc", aliasTimeline,
			[]string{"limit", "abc"},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"home", "", "day", 15,
		},

		// getDiscover: scope all; hot is discover-hot.
		{
			"getDiscover sort absent", aliasDiscover, nil, viewerOneDID,
			"discover-hot",
			[]string{discoverHotItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover sort absent, anonymous", aliasDiscover, nil, "",
			"discover-hot",
			[]string{discoverHotItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover sort hot", aliasDiscover,
			[]string{"sort", "hot"},
			viewerOneDID,
			"discover-hot",
			[]string{discoverHotItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover sort new, anonymous", aliasDiscover,
			[]string{"sort", "new"},
			"",
			"new",
			[]string{aliasNewItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover sort top", aliasDiscover,
			[]string{"sort", "top"},
			viewerOneDID,
			"top",
			[]string{aliasTopItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover sort top, timeframe month, limit 50", aliasDiscover,
			[]string{"sort", "top", "timeframe", "month", "limit", "50"},
			"",
			"top",
			[]string{aliasTopItemURI},
			"all", "", "month", 50,
		},
		{
			"getDiscover viewer-like parameters, anonymous", aliasDiscover,
			append([]string{"sort", "new"}, aliasViewerLikeParams...),
			"",
			"new",
			[]string{aliasNewItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover viewer-like parameters, signed in", aliasDiscover,
			append([]string{"sort", "new"}, aliasViewerLikeParams...),
			viewerOneDID,
			"new",
			[]string{aliasNewItemURI},
			"all", "", "day", 15,
		},
		{
			"getDiscover limit -3", aliasDiscover,
			[]string{"sort", "new", "limit", "-3"},
			"",
			"new",
			[]string{aliasNewItemURI},
			"all", "", "day", 15,
		},

		// getCommunity: scope community with the resolved DID.
		{
			"getCommunity sort absent", aliasCommunity,
			[]string{"community", communityOneDID},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity by handle", aliasCommunity,
			[]string{"community", communityOneHandle},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity by !name@instance", aliasCommunity,
			[]string{"community", communityOneScopedIdentifier},
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity sort absent, anonymous", aliasCommunity,
			[]string{"community", communityTwoDID},
			"",
			"hot",
			[]string{aliasHotItemURI},
			"community", communityTwoDID, "day", 15,
		},
		{
			"getCommunity sort new", aliasCommunity,
			[]string{"community", communityOneDID, "sort", "new"},
			viewerOneDID,
			"new",
			[]string{aliasNewItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity sort top, timeframe absent", aliasCommunity,
			[]string{"community", communityOneDID, "sort", "top"},
			viewerOneDID,
			"top",
			[]string{aliasTopItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity sort top, timeframe year, limit 50", aliasCommunity,
			[]string{"community", communityOneDID, "sort", "top", "timeframe", "year", "limit", "50"},
			"",
			"top",
			[]string{aliasTopItemURI},
			"community", communityOneDID, "year", 50,
		},
		{
			"getCommunity limit 0", aliasCommunity,
			[]string{"community", communityOneDID, "limit", "0"},
			"",
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity viewer-like parameters, anonymous", aliasCommunity,
			append([]string{"community", communityOneDID}, aliasViewerLikeParams...),
			"",
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity viewer-like parameters, signed in", aliasCommunity,
			append([]string{"community", communityOneDID}, aliasViewerLikeParams...),
			viewerOneDID,
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
		{
			"getCommunity limit abc", aliasCommunity,
			[]string{"community", communityOneDID, "limit", "abc"},
			"",
			"hot",
			[]string{aliasHotItemURI},
			"community", communityOneDID, "day", 15,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAliasFixture(t)

			body := decodeFeed(t, f.alias(t, tc.endpoint, values(tc.params...), tc.viewer))

			uris := make([]string, 0, len(body.Feed))
			for _, item := range body.Feed {
				uris = append(uris, item.Post.URI)
			}
			if !reflect.DeepEqual(uris, tc.wantURIs) {
				t.Errorf("feed URIs = %v, want %v", uris, tc.wantURIs)
			}

			want := feedgen.SkeletonRequest{
				ViewerDID: tc.viewer,
				Scope:     feedgen.Scope{Name: tc.wantScope, CommunityDID: tc.wantCommunity},
				Limit:     tc.wantLimit,
				Timeframe: tc.wantTimeframe,
			}
			for rkey, requests := range f.requestsByRKey() {
				if rkey != tc.wantRKey {
					if len(requests) != 0 {
						t.Errorf("built-in %q received %+v, want no request (expected feed %q)", rkey, requests, tc.wantRKey)
					}
					continue
				}
				if len(requests) != 1 {
					t.Fatalf("built-in %q received %d requests, want 1: %+v", rkey, len(requests), requests)
				}
				if requests[0] != want {
					t.Errorf("built-in %q request = %+v, want %+v", rkey, requests[0], want)
				}
			}
		})
	}
}

// TestFeedAliases_Errors: the aliases keep today's parsing and error codes.
// For getCommunity, sort, limit and timeframe are validated before the
// community is resolved, and the community parameter has no length cap.
func TestFeedAliases_Errors(t *testing.T) {
	const unknownCommunityDID = "did:plc:nosuchcommunity"
	longUnknownCommunityDID := "did:plc:" + strings.Repeat("a", 400)

	type errorRow struct {
		name       string
		endpoint   aliasEndpoint
		params     []string
		viewer     string
		wantStatus int
		wantError  string
	}
	rows := []errorRow{
		{"getTimeline no viewer", aliasTimeline, nil, "", http.StatusUnauthorized, "AuthenticationRequired"},
		{"getTimeline no viewer, viewer-like parameters", aliasTimeline, aliasViewerLikeParams, "", http.StatusUnauthorized, "AuthenticationRequired"},
		{"getTimeline non-DID context viewer", aliasTimeline, []string{"sort", "new"}, "not-a-did", http.StatusUnauthorized, "AuthenticationRequired"},
		{"getCommunity ambiguous community", aliasCommunity, []string{"community", ambiguousCommunityIdentifier}, viewerOneDID, http.StatusConflict, "AmbiguousCommunity"},
		{"getCommunity malformed community", aliasCommunity, []string{"community", malformedCommunityIdentifier}, viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
		{"getCommunity resolver failure", aliasCommunity, []string{"community", unresolvableCommunityIdentifier}, viewerOneDID, http.StatusInternalServerError, "InternalServerError"},
		{"getCommunity no community", aliasCommunity, nil, viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
		{"getCommunity unknown community", aliasCommunity, []string{"community", unknownCommunityDID}, viewerOneDID, http.StatusNotFound, "CommunityNotFound"},
		{"getCommunity unknown community, limit 51", aliasCommunity, []string{"community", unknownCommunityDID, "limit", "51"}, viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
		{"getCommunity unknown community, sort best", aliasCommunity, []string{"community", unknownCommunityDID, "sort", "best"}, viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
		{"getCommunity unknown community, timeframe fortnight", aliasCommunity, []string{"community", unknownCommunityDID, "timeframe", "fortnight"}, viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
		{"getCommunity unknown 408-char community", aliasCommunity, []string{"community", longUnknownCommunityDID}, viewerOneDID, http.StatusNotFound, "CommunityNotFound"},
	}
	for _, endpoint := range []struct {
		endpoint aliasEndpoint
		base     []string
	}{
		{aliasTimeline, nil},
		{aliasDiscover, nil},
		{aliasCommunity, []string{"community", communityOneDID}},
	} {
		with := func(params ...string) []string {
			return append(append([]string(nil), endpoint.base...), params...)
		}
		rows = append(rows,
			errorRow{string(endpoint.endpoint) + " sort best", endpoint.endpoint, with("sort", "best"), viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
			errorRow{string(endpoint.endpoint) + " sort Hot", endpoint.endpoint, with("sort", "Hot"), viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
			errorRow{string(endpoint.endpoint) + " sort ' hot'", endpoint.endpoint, with("sort", " hot"), viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
			errorRow{string(endpoint.endpoint) + " limit 51", endpoint.endpoint, with("limit", "51"), viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
			errorRow{string(endpoint.endpoint) + " sort hot, timeframe fortnight", endpoint.endpoint, with("sort", "hot", "timeframe", "fortnight"), viewerOneDID, http.StatusBadRequest, "InvalidRequest"},
			errorRow{string(endpoint.endpoint) + " bad cursor", endpoint.endpoint, with("cursor", "not-a-cursor"), viewerOneDID, http.StatusBadRequest, "InvalidCursor"},
		)
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newAliasFixture(t)
			rec := f.alias(t, tc.endpoint, values(tc.params...), tc.viewer)
			requireXRPCError(t, rec, tc.wantStatus, tc.wantError)
			if strings.Contains(rec.Body.String(), resolverFailureDetail) {
				t.Errorf("error body leaks the resolver failure detail: %s", rec.Body.String())
			}
		})
	}
}

// TestFeedAliases_DiscoverHotFailures: getDiscover hot reports a discover-hot
// capacity failure as 503 DiscoverUnavailable with Retry-After 30, and any
// other generator failure as a 500, neither leaking the internal detail.
func TestFeedAliases_DiscoverHotFailures(t *testing.T) {
	const internalDetail = "snapshot reservation query failed"

	for _, tc := range []struct {
		name           string
		err            error
		wantStatus     int
		wantError      string
		wantRetryAfter string
	}{
		{"capacity unavailable", fmt.Errorf("%s: %w", internalDetail, discover.ErrDiscoverUnavailable), http.StatusServiceUnavailable, "DiscoverUnavailable", "30"},
		{"other failure", errors.New(internalDetail), http.StatusInternalServerError, "InternalServerError", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAliasFixture(t)
			f.discoverHot.err = tc.err

			rec := f.alias(t, aliasDiscover, values("sort", "hot"), viewerOneDID)

			requireXRPCError(t, rec, tc.wantStatus, tc.wantError)
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
			if strings.Contains(rec.Body.String(), internalDetail) {
				t.Errorf("error body leaks the internal detail: %s", rec.Body.String())
			}
		})
	}
}

// TestFeedAliases_EmptyFeedIsAnArray: an alias page with no posts is a 200
// with "feed": [] and no cursor, never a null feed.
func TestFeedAliases_EmptyFeedIsAnArray(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint aliasEndpoint
		params   []string
		viewer   string
	}{
		{"getTimeline sort hot", aliasTimeline, []string{"sort", "hot"}, viewerOneDID},
		{"getCommunity sort new", aliasCommunity, []string{"community", communityOneDID, "sort", "new"}, ""},
		{"getDiscover sort top", aliasDiscover, []string{"sort", "top"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeedFixture(t, &fakeGenerator{}, nil)
			f.newFeed.uris = nil
			f.top.uris = nil

			rec := f.alias(t, tc.endpoint, values(tc.params...), tc.viewer)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not a JSON object: %v (body: %q)", err, rec.Body.String())
			}
			if got := string(body["feed"]); got != "[]" {
				t.Errorf("feed = %s, want [] (body: %s)", got, rec.Body.String())
			}
			if _, ok := body["cursor"]; ok {
				t.Errorf("empty page carries a cursor: %s", rec.Body.String())
			}
		})
	}
}

// TestFeedAliases_CursorReplay: an alias's page-1 cursor, replayed to the same
// alias with the same parameters, hands the generator back its inner cursor.
func TestFeedAliases_CursorReplay(t *testing.T) {
	const innerCursor = "2026-01-02T03:04:05.123456789Z::at://did:plc:author/social.coves.community.post/x1"

	for _, tc := range []struct {
		name      string
		endpoint  aliasEndpoint
		params    []string
		viewer    string
		generator func(f *feedFixture) *fakeGenerator
	}{
		{"getTimeline sort hot", aliasTimeline, []string{"sort", "hot"}, viewerOneDID, func(f *feedFixture) *fakeGenerator { return f.hot }},
		{"getCommunity sort top", aliasCommunity, []string{"community", communityOneDID, "sort", "top"}, viewerOneDID, func(f *feedFixture) *fakeGenerator { return f.top }},
		{"getDiscover sort new", aliasDiscover, []string{"sort", "new"}, "", func(f *feedFixture) *fakeGenerator { return f.newFeed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAliasFixture(t)
			generator := tc.generator(f)
			generator.pageCursor = innerCursor

			page1 := decodeFeed(t, f.alias(t, tc.endpoint, values(append(tc.params, "limit", "1")...), tc.viewer))
			if page1.Cursor == "" {
				t.Fatalf("page 1 returned no cursor although the generator returned %q", innerCursor)
			}
			decodeFeed(t, f.alias(t, tc.endpoint, values(append(tc.params, "limit", "1", "cursor", page1.Cursor)...), tc.viewer))

			requests := generator.received()
			if len(requests) != 2 {
				t.Fatalf("generator received %d requests, want 2 (page 1, page 2): %+v", len(requests), requests)
			}
			if requests[1].Cursor != innerCursor {
				t.Fatalf("page-2 generator cursor = %q, want %q", requests[1].Cursor, innerCursor)
			}
		})
	}
}
