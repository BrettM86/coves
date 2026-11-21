package feedgen

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	coreerrors "Coves/internal/core/errors"
)

const (
	defaultLimit = 15
	maxLimit     = 50

	// maxRefillRounds is how many extra Skeleton calls a short page may make.
	maxRefillRounds = 2

	// envelopeSeparator splits the binding digest from the inner cursor.
	envelopeSeparator = "::"
	// bindingDigestBytes is the digest length; hex-encoded it is 16 chars.
	bindingDigestBytes = 8
)

type service struct {
	registry *Registry
	hydrator PostHydrator
	signer   CursorSigner
}

// NewService builds the getFeed service over the registry's builtins.
func NewService(registry *Registry, hydrator PostHydrator, signer CursorSigner) Service {
	return &service{registry: registry, hydrator: hydrator, signer: signer}
}

func (s *service) GetFeed(ctx context.Context, req GetFeedRequest) (*GetFeedResponse, error) {
	limit, err := normalizeLimit(req.Limit)
	if err != nil {
		return nil, err
	}
	timeframe, err := normalizeTimeframe(req.Timeframe)
	if err != nil {
		return nil, err
	}
	req.Timeframe = timeframe
	if req.Scope.Name == ScopeCommunity && req.Scope.CommunityDID == "" {
		return nil, coreerrors.NewValidationError("community", "community is required for scope community")
	}

	feedURI, builtin, err := s.resolveFeed(req.Feed, req.Scope.Name)
	if err != nil {
		return nil, err
	}

	binding := bindingDigest(feedURI, req.Scope, req.ViewerDID)
	innerCursor, err := s.unwrapCursor(req.Cursor, binding)
	if err != nil {
		return nil, err
	}

	var page filledPage
	if builtin.HydratedGenerator != nil {
		page, err = hydratedPage(ctx, builtin.HydratedGenerator, req, limit, innerCursor)
	} else {
		page, err = s.fill(ctx, builtin.Generator, req, limit, innerCursor)
	}
	if err != nil {
		return nil, err
	}

	resp := &GetFeedResponse{Feed: page.items, ReqID: page.reqID}
	if resp.Feed == nil {
		resp.Feed = []*FeedViewPost{}
	}
	if page.cursor != "" {
		resp.Cursor = s.signer.Sign(binding + envelopeSeparator + page.cursor)
	}
	if resp.ReqID == "" {
		reqID, err := newReqID()
		if err != nil {
			return nil, err
		}
		resp.ReqID = reqID
	}
	return resp, nil
}

// ValidatePageParameters returns the validation error GetFeed would return
// for limit or timeframe, so a caller can reject them before resolving a
// community.
func ValidatePageParameters(limit int, timeframe string) error {
	if _, err := normalizeLimit(limit); err != nil {
		return err
	}
	_, err := normalizeTimeframe(timeframe)
	return err
}

func normalizeLimit(limit int) (int, error) {
	if limit <= 0 {
		return defaultLimit, nil
	}
	if limit > maxLimit {
		return 0, coreerrors.NewValidationError("limit", fmt.Sprintf("limit must be at most %d", maxLimit))
	}
	return limit, nil
}

// validTimeframes are the lexicon's timeframe values.
var validTimeframes = map[string]bool{
	"hour": true, "day": true, "week": true, "month": true, "year": true, "all": true,
}

// normalizeTimeframe validates timeframe for every feed, as today's feeds do
// for every sort, and defaults it to the lexicon default "day". Top ranks
// within it; Hot and New ignore it.
func normalizeTimeframe(timeframe string) (string, error) {
	if timeframe == "" {
		return "day", nil
	}
	if !validTimeframes[timeframe] {
		return "", coreerrors.NewValidationError("timeframe", "timeframe must be one of: hour, day, week, month, year, all")
	}
	return timeframe, nil
}

// resolveFeed maps the requested feed (or the scope's instance default) to a
// builtin usable in scope.
func (s *service) resolveFeed(feed, scope string) (string, Builtin, error) {
	if feed == "" {
		defaultURI, ok := s.registry.defaultURI(scope)
		if !ok {
			return "", Builtin{}, fmt.Errorf("%w: no default feed for scope %q", ErrUnsupportedFeedScope, scope)
		}
		feed = defaultURI
	}
	builtin, ok := s.registry.Lookup(feed)
	if !ok {
		return "", Builtin{}, fmt.Errorf("%w: %s", ErrUnknownFeed, feed)
	}
	if builtin.Kind != KindPosts || (builtin.Generator == nil && builtin.HydratedGenerator == nil) {
		return "", Builtin{}, fmt.Errorf("%w: feed %s does not serve posts", ErrUnsupportedFeedScope, feed)
	}
	if !slices.Contains(builtin.Scopes, scope) {
		return "", Builtin{}, fmt.Errorf("%w: feed %s does not support scope %q", ErrUnsupportedFeedScope, feed, scope)
	}
	return feed, builtin, nil
}

// bindingDigest ties a cursor to the feed, scope, community and viewer it was
// issued for.
func bindingDigest(feedURI string, scope Scope, viewerDID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{feedURI, scope.Name, scope.CommunityDID, viewerDID}, "\x00")))
	return hex.EncodeToString(sum[:bindingDigestBytes])
}

// unwrapCursor verifies an envelope cursor and returns the generator cursor it
// carries; "" stays "".
func (s *service) unwrapCursor(cursor, binding string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	payload, err := s.signer.Verify(cursor)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidCursor, err)
	}
	digest, inner, found := strings.Cut(payload, envelopeSeparator)
	if !found || inner == "" {
		return "", fmt.Errorf("%w: malformed envelope", ErrInvalidCursor)
	}
	if digest != binding {
		return "", fmt.Errorf("%w: cursor was issued for a different feed, scope, community, or viewer", ErrInvalidCursor)
	}
	return inner, nil
}

type filledPage struct {
	items  []*FeedViewPost
	cursor string
	reqID  string
}

// fill calls the generator, hydrates, and refills a short page for at most
// maxRefillRounds extra rounds while the generator has more. The returned
// cursor is the last generator cursor.
func (s *service) fill(ctx context.Context, generator PostGenerator, req GetFeedRequest, limit int, cursor string) (filledPage, error) {
	var out filledPage
	seen := map[string]struct{}{}

	for round := 0; round <= maxRefillRounds; round++ {
		remaining := limit - len(out.items)
		page, err := generator.Skeleton(ctx, SkeletonRequest{
			ViewerDID: req.ViewerDID,
			Scope:     req.Scope,
			Limit:     remaining,
			Cursor:    cursor,
			Timeframe: req.Timeframe,
		})
		if err != nil {
			return filledPage{}, fmt.Errorf("feed skeleton: %w", err)
		}
		if out.reqID == "" {
			out.reqID = page.ReqID
		}
		cursor = page.Cursor
		out.cursor = page.Cursor

		uris := make([]string, 0, len(page.Items))
		feedContext := make(map[string]string, len(page.Items))
		for _, item := range page.Items {
			if _, dup := seen[item.URI]; dup {
				continue
			}
			seen[item.URI] = struct{}{}
			uris = append(uris, item.URI)
			feedContext[item.URI] = item.FeedContext
		}

		if len(uris) > 0 {
			views, err := s.hydrator.HydratePosts(ctx, HydrateRequest{ViewerDID: req.ViewerDID, Scope: req.Scope, URIs: uris})
			if err != nil {
				return filledPage{}, fmt.Errorf("hydrate feed posts: %w", err)
			}
			for _, view := range views {
				if view == nil || len(out.items) >= limit {
					continue
				}
				if req.Scope.Name == ScopeCommunity && (view.Community == nil || view.Community.DID != req.Scope.CommunityDID) {
					continue
				}
				out.items = append(out.items, &FeedViewPost{Post: view, FeedContext: feedContext[view.URI]})
			}
		}

		if len(out.items) >= limit || cursor == "" {
			break
		}
	}
	return out, nil
}

// hydratedPage serves one page of a generator that hydrates its own posts, as
// it returned it: one call, no hydration, no refill.
func hydratedPage(ctx context.Context, generator HydratedPostGenerator, req GetFeedRequest, limit int, cursor string) (filledPage, error) {
	page, err := generator.HydratedPage(ctx, SkeletonRequest{
		ViewerDID: req.ViewerDID,
		Scope:     req.Scope,
		Limit:     limit,
		Cursor:    cursor,
		Timeframe: req.Timeframe,
	})
	if err != nil {
		return filledPage{}, fmt.Errorf("hydrated feed page: %w", err)
	}
	items := page.Feed
	if len(items) > limit {
		items = items[:limit]
	}
	return filledPage{items: items, cursor: page.Cursor, reqID: page.ReqID}, nil
}

// newReqID generates a request id for responses whose generator gave none.
func newReqID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
