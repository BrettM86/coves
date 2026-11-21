package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"Coves/internal/core/feedgen"
	"Coves/internal/core/posts"

	"github.com/lib/pq"
)

// Querier is the read half of *sql.DB and *sql.Tx that PostHydrator needs.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// PostHydrator turns a generator's post URIs into the PostViews the old feed of
// the same scope would serve. It re-checks visibility at hydration time: the
// shared read-path predicate (visiblePostsPredicate) with the request's
// viewer, the viewer's blocks (community blocks on the aggregate home and all
// surfaces, as on the timeline and Discover, not on a community's own feed),
// the account restriction, and scope membership (home: a community the viewer
// subscribes to; community: that community; all: any community not removed by
// the instance, as on Discover).
type PostHydrator struct {
	db Querier
	accountRestriction
}

// NewPostHydrator returns a hydrator reading through q. Pass
// WithAccountRestrictionAuthority so restricted accounts' posts stay hidden,
// as on the old feeds.
func NewPostHydrator(q Querier, options ...AccountRestrictionOption) *PostHydrator {
	return &PostHydrator{db: q, accountRestriction: newAccountRestriction(options)}
}

// hydrateQuery returns the SQL and arguments HydratePosts runs for req.
func (h *PostHydrator) hydrateQuery(req feedgen.HydrateRequest) (string, []any, error) {
	// $1 uris, $2 viewer ("" for anonymous), $3 account-ban authority,
	// $4 community DID for scope community.
	args := []any{pq.Array(req.URIs), req.ViewerDID, h.authorityDID}
	var scopeFilter, blockFilter string
	switch req.Scope.Name {
	case feedgen.ScopeHome:
		scopeFilter = `AND p.community_did IN (SELECT community_did FROM community_subscriptions WHERE user_did = $2)`
		if req.ViewerDID != "" {
			blockFilter = viewerBlockFilters(2, aggregateSurface)
		}
	case feedgen.ScopeCommunity:
		scopeFilter = `AND p.community_did = $4`
		args = append(args, req.Scope.CommunityDID)
		if req.ViewerDID != "" {
			blockFilter = viewerBlockFilters(2, explicitSurface)
		}
	case feedgen.ScopeAll:
		scopeFilter = `AND NOT ` + communityRemovedSQL("p.community_did", "$3")
		if req.ViewerDID != "" {
			blockFilter = viewerBlockFilters(2, aggregateSurface)
		}
	default:
		return "", nil, fmt.Errorf("hydrate posts: scope %q: %w", req.Scope.Name, feedgen.ErrUnsupportedFeedScope)
	}

	visJoin, visWhere := visiblePostsJoin(2, 3)
	return `
		SELECT` + postViewSelectColumns + `
		FROM posts p
		LEFT JOIN users u ON p.author_did = u.did
		INNER JOIN communities c ON p.community_did = c.did` + visJoin + `
		WHERE p.uri = ANY($1)
			AND p.deleted_at IS NULL
			AND ` + visWhere + `
			` + blockFilter + `
			` + scopeFilter, args, nil
}

// HydratePosts returns the visible posts among req.URIs in input order. Posts
// that are missing, deleted, hidden from the viewer, or outside the scope are
// dropped.
func (h *PostHydrator) HydratePosts(ctx context.Context, req feedgen.HydrateRequest) ([]*posts.PostView, error) {
	if len(req.URIs) == 0 {
		return []*posts.PostView{}, nil
	}

	query, args, err := h.hydrateQuery(req)
	if err != nil {
		return nil, err
	}

	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("hydrate posts: query: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.Warn("failed to close rows", "error", err)
		}
	}()

	byURI := make(map[string]*posts.PostView, len(req.URIs))
	for rows.Next() {
		view, err := scanPostView(rows)
		if err != nil {
			return nil, fmt.Errorf("hydrate posts: scan: %w", err)
		}
		byURI[view.URI] = view
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hydrate posts: iterate: %w", err)
	}

	views := make([]*posts.PostView, 0, len(byURI))
	for _, uri := range req.URIs {
		if view, ok := byURI[uri]; ok {
			views = append(views, view)
		}
	}
	return views, nil
}
