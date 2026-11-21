package feedgen_test

import (
	"context"
	"errors"
	"strings"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"
)

const testInstanceDID = "did:web:instance.test"

// scriptedGenerator answers each Skeleton call with the page scripted for the
// incoming inner cursor and records every request it receives.
type scriptedGenerator struct {
	pages    map[string]feedgen.SkeletonPage
	requests []feedgen.SkeletonRequest
}

func (g *scriptedGenerator) Skeleton(_ context.Context, req feedgen.SkeletonRequest) (feedgen.SkeletonPage, error) {
	g.requests = append(g.requests, req)
	page, ok := g.pages[req.Cursor]
	if !ok {
		return feedgen.SkeletonPage{}, errors.New("scriptedGenerator: no page scripted for cursor " + req.Cursor)
	}
	return page, nil
}

func (g *scriptedGenerator) requestCursors() []string {
	cursors := make([]string, 0, len(g.requests))
	for _, req := range g.requests {
		cursors = append(cursors, req.Cursor)
	}
	return cursors
}

func skeletonItems(uris ...string) []feedgen.SkeletonItem {
	items := make([]feedgen.SkeletonItem, 0, len(uris))
	for _, uri := range uris {
		items = append(items, feedgen.SkeletonItem{URI: uri})
	}
	return items
}

// visibleHydrator returns the posts it knows, in input order, and drops the
// rest (as the real hydrator drops invisible posts).
type visibleHydrator struct {
	visible map[string]*posts.PostView
}

func newVisibleHydrator(communityByURI map[string]string) *visibleHydrator {
	h := &visibleHydrator{visible: map[string]*posts.PostView{}}
	for uri, communityDID := range communityByURI {
		h.visible[uri] = &posts.PostView{URI: uri, Community: &posts.CommunityRef{DID: communityDID}}
	}
	return h
}

func (h *visibleHydrator) HydratePosts(_ context.Context, req feedgen.HydrateRequest) ([]*posts.PostView, error) {
	var out []*posts.PostView
	for _, uri := range req.URIs {
		if view, ok := h.visible[uri]; ok {
			out = append(out, view)
		}
	}
	return out, nil
}

// suffixSigner is a trivial CursorSigner: Sign appends a fixed suffix and
// Verify accepts only cursors carrying it.
type suffixSigner struct{}

const signerSuffix = ".test-signature"

func (suffixSigner) Sign(payload string) string { return payload + signerSuffix }

func (suffixSigner) Verify(cursor string) (string, error) {
	if !strings.HasSuffix(cursor, signerSuffix) {
		return "", errors.New("suffixSigner: bad signature")
	}
	return strings.TrimSuffix(cursor, signerSuffix), nil
}

func feedURIs(resp *feedgen.GetFeedResponse) []string {
	uris := make([]string, 0, len(resp.Feed))
	for _, item := range resp.Feed {
		if item == nil || item.Post == nil {
			uris = append(uris, "<nil>")
			continue
		}
		uris = append(uris, item.Post.URI)
	}
	return uris
}

// scriptedHydratedGenerator answers each HydratedPage call with the page
// scripted for the incoming inner cursor and records every request.
type scriptedHydratedGenerator struct {
	pages    map[string]feedgen.HydratedPostPage
	requests []feedgen.SkeletonRequest
}

func (g *scriptedHydratedGenerator) HydratedPage(_ context.Context, req feedgen.SkeletonRequest) (feedgen.HydratedPostPage, error) {
	g.requests = append(g.requests, req)
	page, ok := g.pages[req.Cursor]
	if !ok {
		return feedgen.HydratedPostPage{}, errors.New("scriptedHydratedGenerator: no page scripted for cursor " + req.Cursor)
	}
	return page, nil
}

func (g *scriptedHydratedGenerator) requestCursors() []string {
	cursors := make([]string, 0, len(g.requests))
	for _, req := range g.requests {
		cursors = append(cursors, req.Cursor)
	}
	return cursors
}

func hydratedItems(uris ...string) []*feedgen.FeedViewPost {
	items := make([]*feedgen.FeedViewPost, 0, len(uris))
	for _, uri := range uris {
		items = append(items, &feedgen.FeedViewPost{Post: &posts.PostView{URI: uri}})
	}
	return items
}

// forbiddenHydrator fails the test if the service hydrates at all: a
// pre-hydrated page must be served as the generator returned it.
type forbiddenHydrator struct {
	t interface{ Errorf(string, ...any) }
}

func (h forbiddenHydrator) HydratePosts(_ context.Context, req feedgen.HydrateRequest) ([]*posts.PostView, error) {
	h.t.Errorf("hydrator called with %v; a pre-hydrated page must not be hydrated again", req.URIs)
	return nil, errors.New("forbiddenHydrator: must not be called")
}
