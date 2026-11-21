//go:build integration

package postgres

import (
	"context"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
)

// discoverHotTestRequest is one Discover Hot page request as the Discover Hot
// repository tests read pages.
type discoverHotTestRequest struct {
	Cursor    *string
	ViewerDID string // "" for an anonymous viewer
	Limit     int
}

// discoverHotTestItem is one post of a Discover Hot page as those tests read it.
type discoverHotTestItem struct {
	Post *posts.PostView
}

// GetPost satisfies feedItem, so discoverURIs reads these pages.
func (item *discoverHotTestItem) GetPost() *posts.PostView {
	return item.Post
}

// getDiscoverHotViaGenerator reads one Discover Hot page through the
// generator entry point (HydratedPage, scope all) and maps it back to the
// GetDiscover return shape, so the Discover Hot repository tests exercise the
// generator path with their original assertions. Errors pass through
// unchanged: generator errors keep matching discover.ErrInvalidCursor and
// discover.ErrDiscoverUnavailable.
func getDiscoverHotViaGenerator(ctx context.Context, repo *postgresDiscoverRepo, req discoverHotTestRequest) ([]*discoverHotTestItem, *string, error) {
	cursor := ""
	if req.Cursor != nil {
		cursor = *req.Cursor
	}
	page, err := repo.HydratedPage(ctx, feedgen.SkeletonRequest{
		ViewerDID: req.ViewerDID,
		Scope:     feedgen.Scope{Name: feedgen.ScopeAll},
		Limit:     req.Limit,
		Cursor:    cursor,
	})
	if err != nil {
		return nil, nil, err
	}
	feed := make([]*discoverHotTestItem, 0, len(page.Feed))
	for _, item := range page.Feed {
		feed = append(feed, &discoverHotTestItem{Post: item.Post})
	}
	if page.Cursor == "" {
		return feed, nil, nil
	}
	next := page.Cursor
	return feed, &next, nil
}
