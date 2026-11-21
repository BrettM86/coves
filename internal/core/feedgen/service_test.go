package feedgen_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"Coves/internal/core/feedgen"
)

const (
	testViewerDID     = "did:plc:viewer"
	testCommunityDID  = "did:plc:community1"
	otherCommunityDID = "did:plc:community2"
	hotFeedURI        = "at://did:web:instance.test/social.coves.feed.generator/hot"
)

func postURI(rkey string) string {
	return "at://did:plc:author/social.coves.community.post/" + rkey
}

// B3: feed resolution — instance defaults, unknown feeds, unsupported scopes,
// scope/kind mismatches on registered builtins.
func TestService_FeedResolution(t *testing.T) {
	visiblePost := postURI("p1")

	tests := []struct {
		name    string
		req     feedgen.GetFeedRequest
		wantErr error
		// wantHotScope is the scope the hot generator must receive when no error is expected.
		wantHotScope feedgen.Scope
	}{
		{
			name:         "empty feed with scope home uses hot",
			req:          feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Limit: 10},
			wantHotScope: feedgen.Scope{Name: "home"},
		},
		{
			name:         "empty feed with scope community uses hot",
			req:          feedgen.GetFeedRequest{Scope: feedgen.Scope{Name: "community", CommunityDID: testCommunityDID}, Limit: 10},
			wantHotScope: feedgen.Scope{Name: "community", CommunityDID: testCommunityDID},
		},
		{
			name:    "unregistered at-uri is an unknown feed",
			req:     feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Feed: "at://did:web:instance.test/social.coves.feed.generator/nope", Limit: 10},
			wantErr: feedgen.ErrUnknownFeed,
		},
		{
			name:    "scope all has no registered default",
			req:     feedgen.GetFeedRequest{Scope: feedgen.Scope{Name: "all"}, Limit: 10},
			wantErr: feedgen.ErrUnsupportedFeedScope,
		},
		{
			name:    "scope thread has no registered default",
			req:     feedgen.GetFeedRequest{Scope: feedgen.Scope{Name: "thread"}, Limit: 10},
			wantErr: feedgen.ErrUnsupportedFeedScope,
		},
		{
			name:    "registered builtin whose scopes exclude the request scope",
			req:     feedgen.GetFeedRequest{Scope: feedgen.Scope{Name: "community", CommunityDID: testCommunityDID}, Feed: "at://did:web:instance.test/social.coves.feed.generator/home-only", Limit: 10},
			wantErr: feedgen.ErrUnsupportedFeedScope,
		},
		{
			name:    "comments-hot builtin in scope home",
			req:     feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Feed: "at://did:web:instance.test/social.coves.feed.generator/comments-hot", Limit: 10},
			wantErr: feedgen.ErrUnsupportedFeedScope,
		},
		{
			name:    "registered comments builtin",
			req:     feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Feed: "at://did:web:instance.test/social.coves.feed.generator/thread-comments", Limit: 10},
			wantErr: feedgen.ErrUnsupportedFeedScope,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hot := &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
				"": {Items: skeletonItems(visiblePost)},
			}}
			other := &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
				"": {Items: skeletonItems(visiblePost)},
			}}
			registry := feedgen.NewRegistry(testInstanceDID,
				feedgen.HotBuiltin(hot),
				feedgen.Builtin{RKey: "home-only", DisplayName: "Home Only", Kind: feedgen.KindPosts, Scopes: []string{"home"}, Generator: other},
				feedgen.CommentsHotBuiltin(&stubCommentGenerator{name: "hot"}),
				feedgen.Builtin{RKey: "thread-comments", DisplayName: "Thread Comments", Kind: feedgen.KindComments, Scopes: []string{"home", "community", "thread"}, Generator: other},
			)
			hydrator := newVisibleHydrator(map[string]string{visiblePost: testCommunityDID})
			service := feedgen.NewService(registry, hydrator, suffixSigner{})

			resp, err := service.GetFeed(context.Background(), tc.req)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("GetFeed error = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetFeed returned error %v, want nil", err)
			}
			if resp == nil {
				t.Fatalf("GetFeed returned nil response")
			}
			if len(hot.requests) == 0 {
				t.Fatalf("hot generator was not called")
			}
			if got := hot.requests[0].Scope; got != tc.wantHotScope {
				t.Errorf("hot generator scope = %+v, want %+v", got, tc.wantHotScope)
			}
			if got, want := feedURIs(resp), []string{postURI("p1")}; !reflect.DeepEqual(got, want) {
				t.Errorf("feed URIs = %v, want %v", got, want)
			}
		})
	}
}

// B4: a short hydrated page is refilled from the generator for at most two
// extra rounds, never repeating a URI; the response cursor carries the last
// generator cursor.
func TestService_RefillShortPage(t *testing.T) {
	hot := &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
		"":   {Items: skeletonItems(postURI("p1"), postURI("p2"), postURI("p3"), postURI("p4")), Cursor: "c1"},
		"c1": {Items: skeletonItems(postURI("p3"), postURI("p5")), Cursor: "c2"},
		"c2": {Items: skeletonItems(postURI("p6"), postURI("p7")), Cursor: "c3"},
		"c3": {Items: skeletonItems(postURI("p8")), Cursor: ""},
	}}
	// p2, p4, p5 and p7 are invisible and dropped by hydration; p3 comes back in round 2 and must not repeat.
	hydrator := newVisibleHydrator(map[string]string{
		postURI("p1"): testCommunityDID,
		postURI("p3"): testCommunityDID,
		postURI("p6"): testCommunityDID,
		postURI("p8"): testCommunityDID,
	})
	registry := feedgen.NewRegistry(testInstanceDID, feedgen.HotBuiltin(hot))
	service := feedgen.NewService(registry, hydrator, suffixSigner{})
	req := feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Feed: hotFeedURI, Limit: 4}

	first, err := service.GetFeed(context.Background(), req)
	if err != nil {
		t.Fatalf("first GetFeed: %v", err)
	}
	if first == nil {
		t.Fatalf("first GetFeed returned nil response")
	}
	if got, want := feedURIs(first), []string{postURI("p1"), postURI("p3"), postURI("p6")}; !reflect.DeepEqual(got, want) {
		t.Errorf("first page URIs = %v, want %v", got, want)
	}
	if got, want := hot.requestCursors(), []string{"", "c1", "c2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("generator cursors on first page = %q, want %q (initial call plus two refill rounds)", got, want)
	}
	gotLimits := make([]int, 0, len(hot.requests))
	for _, r := range hot.requests {
		gotLimits = append(gotLimits, r.Limit)
	}
	if want := []int{4, 2, 2}; !reflect.DeepEqual(gotLimits, want) {
		t.Errorf("generator limits on first page = %v, want %v (refills ask for the remaining count)", gotLimits, want)
	}
	if first.Cursor == "" {
		t.Fatalf("first page cursor is empty, want an envelope around generator cursor c3")
	}

	hot.requests = nil
	req.Cursor = first.Cursor
	second, err := service.GetFeed(context.Background(), req)
	if err != nil {
		t.Fatalf("second GetFeed with the first page's cursor: %v", err)
	}
	if second == nil {
		t.Fatalf("second GetFeed returned nil response")
	}
	if got, want := hot.requestCursors(), []string{"c3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("generator cursors on second page = %q, want %q", got, want)
	}
	if got, want := feedURIs(second), []string{postURI("p8")}; !reflect.DeepEqual(got, want) {
		t.Errorf("second page URIs = %v, want %v", got, want)
	}
	if second.Cursor != "" {
		t.Errorf("second page cursor = %q, want empty (generator reached the end)", second.Cursor)
	}
}

// B4: a full first page needs no refill.
func TestService_FullPageNoRefill(t *testing.T) {
	hot := &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
		"":   {Items: skeletonItems(postURI("p1"), postURI("p2")), Cursor: "c1"},
		"c1": {Items: skeletonItems(postURI("p3")), Cursor: ""},
	}}
	hydrator := newVisibleHydrator(map[string]string{
		postURI("p1"): testCommunityDID,
		postURI("p2"): testCommunityDID,
		postURI("p3"): testCommunityDID,
	})
	service := feedgen.NewService(feedgen.NewRegistry(testInstanceDID, feedgen.HotBuiltin(hot)), hydrator, suffixSigner{})

	resp, err := service.GetFeed(context.Background(), feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Limit: 2})
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if resp == nil {
		t.Fatalf("GetFeed returned nil response")
	}
	if got, want := feedURIs(resp), []string{postURI("p1"), postURI("p2")}; !reflect.DeepEqual(got, want) {
		t.Errorf("feed URIs = %v, want %v", got, want)
	}
	if got, want := hot.requestCursors(), []string{""}; !reflect.DeepEqual(got, want) {
		t.Errorf("generator cursors = %q, want %q", got, want)
	}
	if resp.Cursor == "" {
		t.Errorf("cursor is empty, want an envelope around generator cursor c1")
	}
}

// B4: the response ReqID is the generator's, or a generated id of at most 100 chars.
func TestService_ReqID(t *testing.T) {
	tests := []struct {
		name         string
		generatorReq string
		want         string
	}{
		{name: "generator reqId is passed through", generatorReq: "abc", want: "abc"},
		{name: "missing generator reqId is generated", generatorReq: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hot := &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
				"": {Items: skeletonItems(postURI("p1")), ReqID: tc.generatorReq},
			}}
			hydrator := newVisibleHydrator(map[string]string{postURI("p1"): testCommunityDID})
			service := feedgen.NewService(feedgen.NewRegistry(testInstanceDID, feedgen.HotBuiltin(hot)), hydrator, suffixSigner{})

			resp, err := service.GetFeed(context.Background(), feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Limit: 5})
			if err != nil {
				t.Fatalf("GetFeed: %v", err)
			}
			if resp == nil {
				t.Fatalf("GetFeed returned nil response")
			}
			if tc.want != "" {
				if resp.ReqID != tc.want {
					t.Errorf("reqId = %q, want %q", resp.ReqID, tc.want)
				}
				return
			}
			if resp.ReqID == "" || len(resp.ReqID) > 100 {
				t.Errorf("reqId = %q (len %d), want non-empty and at most 100 chars", resp.ReqID, len(resp.ReqID))
			}
		})
	}
}

// B5: for scope community, hydrated posts from another community are dropped.
func TestService_CommunityScopeDropsOtherCommunities(t *testing.T) {
	hot := &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
		"": {Items: skeletonItems(postURI("p1"), postURI("p2"), postURI("p3"))},
	}}
	hydrator := newVisibleHydrator(map[string]string{
		postURI("p1"): testCommunityDID,
		postURI("p2"): otherCommunityDID,
		postURI("p3"): testCommunityDID,
	})
	service := feedgen.NewService(feedgen.NewRegistry(testInstanceDID, feedgen.HotBuiltin(hot)), hydrator, suffixSigner{})

	resp, err := service.GetFeed(context.Background(), feedgen.GetFeedRequest{
		ViewerDID: testViewerDID,
		Scope:     feedgen.Scope{Name: "community", CommunityDID: testCommunityDID},
		Feed:      hotFeedURI,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if resp == nil {
		t.Fatalf("GetFeed returned nil response")
	}
	if got, want := feedURIs(resp), []string{postURI("p1"), postURI("p3")}; !reflect.DeepEqual(got, want) {
		t.Errorf("feed URIs = %v, want %v", got, want)
	}
}

const (
	newFeedURI         = "at://did:web:instance.test/social.coves.feed.generator/new"
	topFeedURI         = "at://did:web:instance.test/social.coves.feed.generator/top"
	discoverHotFeedURI = "at://did:web:instance.test/social.coves.feed.generator/discover-hot"
)

// scopeAllFixture is a registry of the four production builtins over fakes.
type scopeAllFixture struct {
	hot, newFeed, top *scriptedGenerator
	discoverHot       *scriptedHydratedGenerator
	service           feedgen.Service
}

func newScopeAllFixture(t *testing.T) *scopeAllFixture {
	t.Helper()
	page := func(rkey string) *scriptedGenerator {
		return &scriptedGenerator{pages: map[string]feedgen.SkeletonPage{
			"": {Items: skeletonItems(postURI(rkey + "1"))},
		}}
	}
	f := &scopeAllFixture{
		hot:     page("hot"),
		newFeed: page("new"),
		top:     page("top"),
		discoverHot: &scriptedHydratedGenerator{pages: map[string]feedgen.HydratedPostPage{
			"": {Feed: hydratedItems(postURI("discover1"))},
		}},
	}
	registry := feedgen.NewRegistry(testInstanceDID,
		feedgen.HotBuiltin(f.hot), feedgen.NewBuiltin(f.newFeed), feedgen.TopBuiltin(f.top),
		feedgen.DiscoverHotBuiltin(f.discoverHot))
	hydrator := newVisibleHydrator(map[string]string{
		postURI("hot1"): testCommunityDID,
		postURI("new1"): testCommunityDID,
		postURI("top1"): testCommunityDID,
	})
	f.service = feedgen.NewService(registry, hydrator, suffixSigner{})
	return f
}

// B2: scope all defaults to discover-hot; hot is not offered at scope all;
// discover-hot is offered only at scope all; new and top serve scope all.
func TestService_ScopeAllResolution(t *testing.T) {
	t.Run("scope all with feed omitted is served by discover-hot", func(t *testing.T) {
		f := newScopeAllFixture(t)
		resp, err := f.service.GetFeed(context.Background(), feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "all"}, Limit: 10})
		if err != nil {
			t.Fatalf("GetFeed returned error %v, want the discover-hot page", err)
		}
		if got, want := len(f.discoverHot.requests), 1; got != want {
			t.Fatalf("discover-hot generator called %d times, want %d", got, want)
		}
		if got, want := f.discoverHot.requests[0].Scope, (feedgen.Scope{Name: "all"}); got != want {
			t.Errorf("discover-hot generator scope = %+v, want %+v", got, want)
		}
		if got, want := feedURIs(resp), []string{postURI("discover1")}; !reflect.DeepEqual(got, want) {
			t.Errorf("feed URIs = %v, want %v", got, want)
		}
	})

	for _, tc := range []struct {
		name string
		req  feedgen.GetFeedRequest
	}{
		{"hot at scope all", feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "all"}, Feed: hotFeedURI, Limit: 10}},
		{"discover-hot at scope home", feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "home"}, Feed: discoverHotFeedURI, Limit: 10}},
		{"discover-hot at scope community", feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "community", CommunityDID: testCommunityDID}, Feed: discoverHotFeedURI, Limit: 10}},
	} {
		t.Run(tc.name+" is an unsupported feed scope", func(t *testing.T) {
			f := newScopeAllFixture(t)
			_, err := f.service.GetFeed(context.Background(), tc.req)
			if !errors.Is(err, feedgen.ErrUnsupportedFeedScope) {
				t.Fatalf("GetFeed error = %v, want errors.Is ErrUnsupportedFeedScope", err)
			}
			if len(f.hot.requests) != 0 || len(f.discoverHot.requests) != 0 {
				t.Errorf("a generator was called (hot %d, discover-hot %d), want none", len(f.hot.requests), len(f.discoverHot.requests))
			}
		})
	}

	for _, tc := range []struct {
		name      string
		feed      string
		timeframe string
		generator func(f *scopeAllFixture) *scriptedGenerator
		wantURI   string
	}{
		{"new at scope all", newFeedURI, "week", func(f *scopeAllFixture) *scriptedGenerator { return f.newFeed }, postURI("new1")},
		{"top at scope all", topFeedURI, "month", func(f *scopeAllFixture) *scriptedGenerator { return f.top }, postURI("top1")},
	} {
		t.Run(tc.name+" reaches its generator with scope all and the timeframe", func(t *testing.T) {
			f := newScopeAllFixture(t)
			resp, err := f.service.GetFeed(context.Background(), feedgen.GetFeedRequest{
				ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "all"}, Feed: tc.feed, Limit: 10, Timeframe: tc.timeframe,
			})
			if err != nil {
				t.Fatalf("GetFeed returned error %v, want a page", err)
			}
			generator := tc.generator(f)
			if len(generator.requests) == 0 {
				t.Fatalf("generator was not called")
			}
			if got, want := generator.requests[0].Scope, (feedgen.Scope{Name: "all"}); got != want {
				t.Errorf("generator scope = %+v, want %+v", got, want)
			}
			if got := generator.requests[0].Timeframe; got != tc.timeframe {
				t.Errorf("generator timeframe = %q, want %q", got, tc.timeframe)
			}
			if got, want := feedURIs(resp), []string{tc.wantURI}; !reflect.DeepEqual(got, want) {
				t.Errorf("feed URIs = %v, want %v", got, want)
			}
		})
	}
}

// B3: a pre-hydrated page is served exactly as the generator returned it, in
// one generator call with no hydration and no refill; its inner cursor
// round-trips inside the bound envelope.
func TestService_HydratedGeneratorPage(t *testing.T) {
	const innerCursor = "discover-hot-v1.inner-cursor-page-1"
	discoverHot := &scriptedHydratedGenerator{pages: map[string]feedgen.HydratedPostPage{
		"":          {Feed: hydratedItems(postURI("d1"), postURI("d2")), Cursor: innerCursor},
		innerCursor: {Feed: hydratedItems(postURI("d3")), Cursor: ""},
	}}
	registry := feedgen.NewRegistry(testInstanceDID, feedgen.HotBuiltin(&scriptedGenerator{}), feedgen.DiscoverHotBuiltin(discoverHot))
	service := feedgen.NewService(registry, forbiddenHydrator{t: t}, suffixSigner{})
	req := feedgen.GetFeedRequest{ViewerDID: testViewerDID, Scope: feedgen.Scope{Name: "all"}, Feed: discoverHotFeedURI, Limit: 5}

	first, err := service.GetFeed(context.Background(), req)
	if err != nil {
		t.Fatalf("first GetFeed returned error %v, want the generator's page", err)
	}
	if got, want := feedURIs(first), []string{postURI("d1"), postURI("d2")}; !reflect.DeepEqual(got, want) {
		t.Errorf("first page URIs = %v, want %v", got, want)
	}
	if got, want := discoverHot.requestCursors(), []string{""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generator cursors on first page = %q, want %q (one call, no refill round)", got, want)
	}
	if got := discoverHot.requests[0].Limit; got != 5 {
		t.Errorf("generator limit = %d, want 5", got)
	}
	if first.Cursor == "" {
		t.Fatalf("first page cursor is empty, want an envelope around %q", innerCursor)
	}

	req.Cursor = first.Cursor
	second, err := service.GetFeed(context.Background(), req)
	if err != nil {
		t.Fatalf("second GetFeed with the first page's cursor: %v", err)
	}
	if got, want := discoverHot.requestCursors(), []string{"", innerCursor}; !reflect.DeepEqual(got, want) {
		t.Errorf("generator cursors = %q, want %q", got, want)
	}
	if got, want := feedURIs(second), []string{postURI("d3")}; !reflect.DeepEqual(got, want) {
		t.Errorf("second page URIs = %v, want %v", got, want)
	}
	if second.Cursor != "" {
		t.Errorf("second page cursor = %q, want empty (generator returned no inner cursor)", second.Cursor)
	}

	otherViewer := req
	otherViewer.ViewerDID = "did:plc:otherviewer"
	if _, err := service.GetFeed(context.Background(), otherViewer); !errors.Is(err, feedgen.ErrInvalidCursor) {
		t.Errorf("another viewer's envelope: GetFeed error = %v, want errors.Is ErrInvalidCursor", err)
	}
	if got := len(discoverHot.requests); got != 2 {
		t.Errorf("generator called %d times after the other viewer's request, want 2 (not called for it)", got)
	}
}
