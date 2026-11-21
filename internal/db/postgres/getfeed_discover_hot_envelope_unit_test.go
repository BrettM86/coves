package postgres

import (
	"context"
	"errors"
	"math"
	"testing"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
)

// envelopeSizeDiscoverHotGenerator serves one pre-hydrated page carrying a
// fixed inner cursor.
type envelopeSizeDiscoverHotGenerator struct{ innerCursor string }

func (g envelopeSizeDiscoverHotGenerator) HydratedPage(context.Context, feedgen.SkeletonRequest) (feedgen.HydratedPostPage, error) {
	return feedgen.HydratedPostPage{
		Feed:   []*feedgen.FeedViewPost{{Post: &posts.PostView{URI: "at://did:plc:author/social.coves.community.post/p1"}}},
		Cursor: g.innerCursor,
	}, nil
}

type envelopeSizeForbiddenHydrator struct{ t *testing.T }

func (h envelopeSizeForbiddenHydrator) HydratePosts(context.Context, feedgen.HydrateRequest) ([]*posts.PostView, error) {
	h.t.Error("hydrator called for a pre-hydrated discover-hot page")
	return nil, errors.New("must not be called")
}

// B6: the getFeed envelope around the largest Discover Hot inner cursor stays
// within the getFeed lexicon cursor bound of 500 characters.
func TestGetFeedDiscoverHotEnvelope_LargestInnerCursorFitsLexiconBound(t *testing.T) {
	const (
		secret    = "test-cursor-secret-for-envelope-size"
		viewerDID = "did:plc:abcdefghijklmnopqrstuvwx"
	)
	repo := &postgresDiscoverRepo{feedRepoBase: newFeedRepoBase(nil, secret, nil)}
	innerCursor, err := repo.buildDiscoverHotCursor(math.MaxInt64, math.MaxInt64, viewerDID)
	if err != nil {
		t.Fatalf("buildDiscoverHotCursor(MaxInt64, MaxInt64): %v", err)
	}

	registry := feedgen.NewRegistry("did:web:instance.test",
		feedgen.DiscoverHotBuiltin(envelopeSizeDiscoverHotGenerator{innerCursor: innerCursor}))
	service := feedgen.NewService(registry, envelopeSizeForbiddenHydrator{t: t}, NewCursorSigner(secret))

	resp, err := service.GetFeed(context.Background(), feedgen.GetFeedRequest{
		ViewerDID: viewerDID,
		Scope:     feedgen.Scope{Name: feedgen.ScopeAll},
		Feed:      "at://did:web:instance.test/social.coves.feed.generator/discover-hot",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("GetFeed returned error %v, want a discover-hot page", err)
	}
	if resp.Cursor == "" {
		t.Fatal("response cursor is empty, want an envelope around the inner cursor")
	}
	if len(resp.Cursor) > 500 {
		t.Fatalf("response cursor is %d characters, want at most 500", len(resp.Cursor))
	}
}
