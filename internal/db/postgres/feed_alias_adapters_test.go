//go:build integration

package postgres

import (
	"context"
	"database/sql"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
)

// getTimeline, getCommunity and getDiscover are aliases of getFeed. The
// adapters below let the repository suites that pinned those feeds keep their
// assertions while reading through the real getFeed service: the production
// post built-ins (hot, new, top, discover-hot), the real PostHydrator and a
// cursor signer. Each takes a request struct with the old request's field
// names and returns the items and *string cursor the call sites consume.
//
// Mapping (as the alias handler maps it): timeline -> scope home, community
// -> scope community with the community DID, discover -> scope all; sort hot,
// new and top name the built-in of that rkey, except Discover hot, which is
// discover-hot. An empty sort is the old default, hot. Top with no timeframe
// ranks within day, the old services' default.

// feedAliasCursorSecret signs the adapters' getFeed cursors.
const feedAliasCursorSecret = "feed-alias-adapter-cursor-secret"

// builtinFeeds is the getFeed service over the production post built-ins,
// with handles on the generators so tests can lower their tunables before the
// first request.
type builtinFeeds struct {
	service      feedgen.Service
	registry     *feedgen.Registry
	hotGenerator *HotGenerator
	newGenerator *SortGenerator
	topGenerator *SortGenerator
	// discoverRepo backs the discover-hot built-in.
	discoverRepo *postgresDiscoverRepo
}

// newBuiltinFeeds wires the getFeed service as production does
// (productionBuiltins), every generator and the hydrator sharing options.
func newBuiltinFeeds(db *sql.DB, cursorSecret string, options ...AccountRestrictionOption) *builtinFeeds {
	feeds := &builtinFeeds{
		hotGenerator: NewHotGenerator(db, options...),
		newGenerator: NewNewGenerator(db, options...),
		topGenerator: NewTopGenerator(db, options...),
		discoverRepo: &postgresDiscoverRepo{feedRepoBase: newFeedRepoBase(db, cursorSecret, options)},
	}
	feeds.registry = feedgen.NewRegistry(fixtures.InstanceDID(),
		feedgen.HotBuiltin(feeds.hotGenerator), feedgen.NewBuiltin(feeds.newGenerator),
		feedgen.TopBuiltin(feeds.topGenerator), feedgen.DiscoverHotBuiltin(feeds.discoverRepo))
	feeds.service = feedgen.NewService(feeds.registry, NewPostHydrator(db, options...), NewCursorSigner(cursorSecret))
	return feeds
}

// setHotCandidateLimit lowers the hot candidate bound identically on the hot,
// new and top generators (new and top must ignore it).
func (f *builtinFeeds) setHotCandidateLimit(limit int) {
	f.hotGenerator.hotCandidateLimit = limit
	f.newGenerator.hotCandidateLimit = limit
	f.topGenerator.hotCandidateLimit = limit
}

// read serves one getFeed page of the built-in rkey in scope.
func (f *builtinFeeds) read(ctx context.Context, viewerDID string, scope feedgen.Scope, rkey, timeframe string, limit int, cursor *string) ([]*feedgen.FeedViewPost, *string, error) {
	if rkey == "top" && timeframe == "" {
		timeframe = "day"
	}
	req := feedgen.GetFeedRequest{
		ViewerDID: viewerDID,
		Scope:     scope,
		Feed:      f.registry.URI(rkey),
		Limit:     limit,
		Timeframe: timeframe,
	}
	if cursor != nil {
		req.Cursor = *cursor
	}
	page, err := f.service.GetFeed(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if page.Cursor == "" {
		return page.Feed, nil, nil
	}
	next := page.Cursor
	return page.Feed, &next, nil
}

// timelineViaFeedRequest mirrors the old getTimeline repository request.
type timelineViaFeedRequest struct {
	UserDID   string
	Sort      string
	Timeframe string
	Limit     int
	Cursor    *string
}

// timelineViaFeed reads getTimeline as getFeed scope home.
type timelineViaFeed struct{ *builtinFeeds }

func newTimelineViaFeed(db *sql.DB, options ...AccountRestrictionOption) *timelineViaFeed {
	return &timelineViaFeed{newBuiltinFeeds(db, feedAliasCursorSecret, options...)}
}

func (a *timelineViaFeed) GetTimeline(ctx context.Context, req timelineViaFeedRequest) ([]*feedgen.FeedViewPost, *string, error) {
	return a.read(ctx, req.UserDID, feedgen.Scope{Name: feedgen.ScopeHome}, sortRkey(req.Sort), req.Timeframe, req.Limit, req.Cursor)
}

// communityFeedViaFeedRequest mirrors the old getCommunity repository request;
// Community is the community DID.
type communityFeedViaFeedRequest struct {
	Community string
	ViewerDID string
	Sort      string
	Timeframe string
	Limit     int
	Cursor    *string
}

// communityFeedViaFeed reads getCommunity as getFeed scope community.
type communityFeedViaFeed struct{ *builtinFeeds }

func newCommunityFeedViaFeed(db *sql.DB, options ...AccountRestrictionOption) *communityFeedViaFeed {
	return &communityFeedViaFeed{newBuiltinFeeds(db, feedAliasCursorSecret, options...)}
}

func (a *communityFeedViaFeed) GetCommunityFeedViaFeed(ctx context.Context, req communityFeedViaFeedRequest) ([]*feedgen.FeedViewPost, *string, error) {
	scope := feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: req.Community}
	return a.read(ctx, req.ViewerDID, scope, sortRkey(req.Sort), req.Timeframe, req.Limit, req.Cursor)
}

// discoverViaFeedRequest mirrors the old getDiscover repository request.
type discoverViaFeedRequest struct {
	ViewerDID string
	Sort      string
	Timeframe string
	Limit     int
	Cursor    *string
}

// discoverViaFeed reads getDiscover as getFeed scope all.
type discoverViaFeed struct{ *builtinFeeds }

func newDiscoverViaFeed(db *sql.DB, options ...AccountRestrictionOption) *discoverViaFeed {
	return &discoverViaFeed{newBuiltinFeeds(db, feedAliasCursorSecret, options...)}
}

func (a *discoverViaFeed) GetDiscoverViaFeed(ctx context.Context, req discoverViaFeedRequest) ([]*feedgen.FeedViewPost, *string, error) {
	rkey := sortRkey(req.Sort)
	if rkey == "hot" {
		rkey = "discover-hot"
	}
	return a.read(ctx, req.ViewerDID, feedgen.Scope{Name: feedgen.ScopeAll}, rkey, req.Timeframe, req.Limit, req.Cursor)
}

// sortRkey is the built-in rkey for an old sort; empty is the old default, hot.
func sortRkey(sort string) string {
	if sort == "" {
		return "hot"
	}
	return sort
}

// feedItem is any feed item carrying a post view: getFeed's and the old
// feeds' item types alike.
type feedItem interface {
	GetPost() *posts.PostView
}

// feedItemURIs renders a feed as the URIs it returned.
func feedItemURIs[T feedItem](feed []T) []string {
	uris := make([]string, 0, len(feed))
	for _, item := range feed {
		if post := item.GetPost(); post != nil {
			uris = append(uris, post.URI)
		}
	}
	return uris
}

// Exported names of the adapters for the package postgres_test suites.
type (
	TimelineViaFeedRequest      = timelineViaFeedRequest
	CommunityFeedViaFeedRequest = communityFeedViaFeedRequest
	DiscoverViaFeedRequest      = discoverViaFeedRequest
	TimelineViaFeed             = timelineViaFeed
	CommunityFeedViaFeed        = communityFeedViaFeed
	DiscoverViaFeed             = discoverViaFeed
)

// NewTimelineViaFeed is newTimelineViaFeed for package postgres_test.
func NewTimelineViaFeed(db *sql.DB, options ...AccountRestrictionOption) *TimelineViaFeed {
	return newTimelineViaFeed(db, options...)
}

// NewCommunityFeedViaFeed is newCommunityFeedViaFeed for package postgres_test.
func NewCommunityFeedViaFeed(db *sql.DB, options ...AccountRestrictionOption) *CommunityFeedViaFeed {
	return newCommunityFeedViaFeed(db, options...)
}

// NewDiscoverViaFeed is newDiscoverViaFeed for package postgres_test.
func NewDiscoverViaFeed(db *sql.DB, options ...AccountRestrictionOption) *DiscoverViaFeed {
	return newDiscoverViaFeed(db, options...)
}
