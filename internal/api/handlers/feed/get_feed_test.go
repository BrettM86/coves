package feed

import (
	"net/http"
	"strings"
	"testing"

	"Coves/internal/core/moderation"
)

const page1InnerCursor = "2026-01-02T03:04:05.123456789Z::at://did:plc:author/social.coves.community.post/p2::2026-01-02T03:04:05.123456789Z"

func twoPostGenerator() *fakeGenerator {
	return &fakeGenerator{
		uris: []string{
			"at://did:plc:author/social.coves.community.post/p1",
			"at://did:plc:author/social.coves.community.post/p2",
			"at://did:plc:author/social.coves.community.post/p3",
		},
		pageCursor: page1InnerCursor,
	}
}

// TestGetFeed_CursorEnvelope: a getFeed cursor is bound to the feed, scope,
// community and viewer that produced it and is tamper-evident; an unchanged
// replay hands the generator back exactly the inner cursor it issued.
func TestGetFeed_CursorEnvelope(t *testing.T) {
	page1 := func(t *testing.T, f *feedFixture) string {
		t.Helper()
		body := decodeFeed(t, f.get(t, values(
			"scope", "community", "community", communityOneDID, "feed", hotFeedURI, "limit", "2"), viewerOneDID))
		if body.Cursor == "" {
			t.Fatalf("page 1 returned no cursor although the generator returned %q", page1InnerCursor)
		}
		return body.Cursor
	}

	t.Run("unchanged replay pages and forwards the inner cursor", func(t *testing.T) {
		f := newFeedFixture(t, twoPostGenerator(), nil)
		cursor := page1(t, f)

		decodeFeed(t, f.get(t, values(
			"scope", "community", "community", communityOneDID, "feed", hotFeedURI, "limit", "2", "cursor", cursor), viewerOneDID))

		requests := f.hot.received()
		if len(requests) != 2 {
			t.Fatalf("hot generator received %d requests, want 2 (page 1, page 2)", len(requests))
		}
		if requests[1].Cursor != page1InnerCursor {
			t.Fatalf("page-2 generator cursor = %q, want %q", requests[1].Cursor, page1InnerCursor)
		}
	})

	flipOneCharacter := func(cursor string) string {
		middle := len(cursor) / 2
		replacement := byte('A')
		if cursor[middle] == 'A' {
			replacement = 'B'
		}
		return cursor[:middle] + string(replacement) + cursor[middle+1:]
	}

	for _, tc := range []struct {
		name   string
		replay func(cursor string) (params []string, viewer string)
	}{
		{"different feed", func(c string) ([]string, string) {
			return []string{"scope", "community", "community", communityOneDID, "feed", otherFeedURI, "limit", "2", "cursor", c}, viewerOneDID
		}},
		{"different scope", func(c string) ([]string, string) {
			return []string{"scope", "home", "feed", hotFeedURI, "limit", "2", "cursor", c}, viewerOneDID
		}},
		{"different community", func(c string) ([]string, string) {
			return []string{"scope", "community", "community", communityTwoDID, "feed", hotFeedURI, "limit", "2", "cursor", c}, viewerOneDID
		}},
		{"different viewer", func(c string) ([]string, string) {
			return []string{"scope", "community", "community", communityOneDID, "feed", hotFeedURI, "limit", "2", "cursor", c}, viewerTwoDID
		}},
		{"one flipped character", func(c string) ([]string, string) {
			return []string{"scope", "community", "community", communityOneDID, "feed", hotFeedURI, "limit", "2", "cursor", flipOneCharacter(c)}, viewerOneDID
		}},
	} {
		t.Run(tc.name+" is InvalidCursor", func(t *testing.T) {
			f := newFeedFixture(t, twoPostGenerator(), nil)
			cursor := page1(t, f)
			params, viewer := tc.replay(cursor)
			requireXRPCError(t, f.get(t, values(params...), viewer), http.StatusBadRequest, "InvalidCursor")
		})
	}

	t.Run("envelope around a 200-char post URI hot cursor stays within 500 chars and replays", func(t *testing.T) {
		postURI := "at://did:plc:abcdefghijklmnopqrstuvwx/social.coves.community.post/" + strings.Repeat("r", 134)
		if len(postURI) != 200 {
			t.Fatalf("fixture post URI is %d chars, want 200", len(postURI))
		}
		innerCursor := "2026-01-02T03:04:05.123456789Z::" + postURI + "::2026-01-02T03:04:05.123456789Z"
		longViewerDID := "did:web:" + strings.Repeat("v", 100) + ".viewer.test"
		longCommunityDID := "did:web:" + strings.Repeat("c", 100) + ".community.test"

		generator := &fakeGenerator{
			uris:       []string{"at://did:plc:author/social.coves.community.post/p1", postURI},
			pageCursor: innerCursor,
		}
		f := newFeedFixture(t, generator, nil, longCommunityDID)
		query := []string{"scope", "community", "community", longCommunityDID, "feed", hotFeedURI, "limit", "2"}

		body := decodeFeed(t, f.get(t, values(query...), longViewerDID))
		if body.Cursor == "" {
			t.Fatal("page 1 returned no cursor")
		}
		if len(body.Cursor) > 500 {
			t.Fatalf("cursor is %d chars, want <= 500 (lexicon maxLength)", len(body.Cursor))
		}

		decodeFeed(t, f.get(t, values(append(query, "cursor", body.Cursor)...), longViewerDID))
		requests := generator.received()
		if len(requests) != 2 || requests[1].Cursor != innerCursor {
			t.Fatalf("replay did not forward the inner cursor; generator requests: %+v", requests)
		}
	})
}

// TestGetFeed_ErrorMapping pins the XRPC errors for feed, scope, auth and
// community resolution failures.
func TestGetFeed_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		params     []string
		viewer     string
		wantStatus int
		wantError  string
	}{
		{"unregistered feed", []string{"scope", "community", "community", communityOneDID, "feed", unknownFeedURI}, viewerOneDID, http.StatusBadRequest, "UnknownFeed"},
		{"hot at scope all", []string{"scope", "all", "feed", hotFeedURI}, viewerOneDID, http.StatusBadRequest, "UnsupportedFeedScope"},
		{"scope thread", []string{"scope", "thread"}, viewerOneDID, http.StatusBadRequest, "UnsupportedFeedScope"},
		{"scope home anonymous", []string{"scope", "home"}, "", http.StatusUnauthorized, "AuthenticationRequired"},
		{"unknown community", []string{"scope", "community", "community", "did:plc:nosuchcommunity"}, viewerOneDID, http.StatusNotFound, "CommunityNotFound"},
		{"ambiguous community", []string{"scope", "community", "community", ambiguousCommunityIdentifier}, viewerOneDID, http.StatusConflict, "AmbiguousCommunity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeedFixture(t, twoPostGenerator(), nil)
			requireXRPCError(t, f.get(t, values(tc.params...), tc.viewer), tc.wantStatus, tc.wantError)
		})
	}
}

// TestGetFeed_Limit: above 50 is refused; zero, absent and non-numeric fall
// back to the default page of 15.
func TestGetFeed_Limit(t *testing.T) {
	twentyPosts := func() *fakeGenerator {
		uris := make([]string, 0, 20)
		for _, rkey := range []string{
			"a01", "a02", "a03", "a04", "a05", "a06", "a07", "a08", "a09", "a10",
			"a11", "a12", "a13", "a14", "a15", "a16", "a17", "a18", "a19", "a20",
		} {
			uris = append(uris, "at://did:plc:author/social.coves.community.post/"+rkey)
		}
		return &fakeGenerator{uris: uris}
	}

	t.Run("limit 51 is InvalidRequest", func(t *testing.T) {
		f := newFeedFixture(t, twentyPosts(), nil)
		requireXRPCError(t, f.get(t, values("scope", "community", "community", communityOneDID, "limit", "51"), ""),
			http.StatusBadRequest, "InvalidRequest")
	})

	for _, tc := range []struct {
		name   string
		params []string
	}{
		{"limit 0", []string{"scope", "community", "community", communityOneDID, "limit", "0"}},
		{"limit absent", []string{"scope", "community", "community", communityOneDID}},
		{"limit abc", []string{"scope", "community", "community", communityOneDID, "limit", "abc"}},
	} {
		t.Run(tc.name+" serves the default 15", func(t *testing.T) {
			f := newFeedFixture(t, twentyPosts(), nil)
			body := decodeFeed(t, f.get(t, values(tc.params...), ""))
			// The generator offers 20 visible posts, so the default page is full.
			if len(body.Feed) != 15 {
				t.Fatalf("feed has %d items, want 15", len(body.Feed))
			}
		})
	}
}

// TestGetFeed_ViewerModerationWiring: an instance-admin viewer gets
// viewer.moderation on items, as the old feed handlers produce for a live,
// unmoderated post. One wiring check; the action algorithm is tested in common.
func TestGetFeed_ViewerModerationWiring(t *testing.T) {
	authority := moderation.NewAllowlistAuthority([]string{viewerOneDID})
	f := newFeedFixture(t, twoPostGenerator(), authority)

	body := decodeFeed(t, f.get(t, values("scope", "community", "community", communityOneDID, "limit", "1"), viewerOneDID))
	if len(body.Feed) != 1 {
		t.Fatalf("feed has %d items, want 1", len(body.Feed))
	}
	viewer := body.Feed[0].Post.Viewer
	if viewer == nil {
		t.Fatal("post.viewer is absent for an instance-admin viewer")
	}
	want := []string{"remove", "label"}
	if strings.Join(viewer.Moderation, ",") != strings.Join(want, ",") {
		t.Fatalf("post.viewer.moderation = %v, want %v", viewer.Moderation, want)
	}
}

// TestGetFeed_Timeframe: the timeframe parameter is validated for every feed,
// as the old feeds validate it for every sort; an empty one becomes "day"; the
// generator receives the validated value.
func TestGetFeed_Timeframe(t *testing.T) {
	community := []string{"scope", "community", "community", communityOneDID}

	for _, tc := range []struct {
		name   string
		params []string
	}{
		{"feed top, timeframe decade", []string{"feed", topFeedURI, "timeframe", "decade"}},
		{"feed hot, timeframe decade", []string{"feed", hotFeedURI, "timeframe", "decade"}},
		{"feed omitted, timeframe decade", []string{"timeframe", "decade"}},
	} {
		t.Run(tc.name+" is InvalidRequest", func(t *testing.T) {
			f := newFeedFixture(t, twoPostGenerator(), nil)
			requireXRPCError(t, f.get(t, values(append(community, tc.params...)...), viewerOneDID),
				http.StatusBadRequest, "InvalidRequest")
			if n := len(f.top.received()) + len(f.hot.received()); n != 0 {
				t.Fatalf("generators received %d requests for an invalid timeframe, want 0", n)
			}
		})
	}

	for _, tc := range []struct {
		name          string
		params        []string
		generator     func(f *feedFixture) *fakeGenerator
		wantTimeframe string
	}{
		{"feed hot, timeframe week", []string{"feed", hotFeedURI, "timeframe", "week"}, func(f *feedFixture) *fakeGenerator { return f.hot }, "week"},
		{"feed top, timeframe omitted", []string{"feed", topFeedURI}, func(f *feedFixture) *fakeGenerator { return f.top }, "day"},
		{"feed top, timeframe month", []string{"feed", topFeedURI, "timeframe", "month"}, func(f *feedFixture) *fakeGenerator { return f.top }, "month"},
	} {
		t.Run(tc.name+" reaches the generator as "+tc.wantTimeframe, func(t *testing.T) {
			f := newFeedFixture(t, twoPostGenerator(), nil)
			decodeFeed(t, f.get(t, values(append(community, tc.params...)...), viewerOneDID))
			requests := tc.generator(f).received()
			if len(requests) == 0 {
				t.Fatal("the generator received no request")
			}
			for i, req := range requests {
				if req.Timeframe != tc.wantTimeframe {
					t.Fatalf("generator request %d timeframe = %q, want %q", i+1, req.Timeframe, tc.wantTimeframe)
				}
			}
		})
	}
}
