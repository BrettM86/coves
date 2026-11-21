//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"Coves/internal/core/comments"
	"Coves/internal/core/feedgen"
	"Coves/tests/fixtures"
)

// The thread-listing tests in this package were written against
// ListByParentWithHotRank and ListByParentsBatch. Those repository methods are
// gone: a thread is now ranked by a comment generator (URIs only) and hydrated
// by Repository.HydrateComments. These two adapters keep the old call shapes
// and results so those tests exercise the production replacement unchanged.

// commentGeneratorForSort returns the built-in comment generator behind a
// getComments sort. Any other sort is an error: there is no generator for it.
func commentGeneratorForSort(db *sql.DB, sort string) (feedgen.CommentGenerator, error) {
	switch sort {
	case "hot":
		return NewCommentsHotGenerator(db), nil
	case "top":
		return NewCommentsTopGenerator(db), nil
	case "new":
		return NewCommentsNewGenerator(db), nil
	default:
		return nil, fmt.Errorf("no comment generator for sort %q", sort)
	}
}

// listByParentViaGenerator is ListByParentWithHotRank's shape over the
// generator path: one parent, its page of replies and the next cursor (nil
// when there are no more).
func listByParentViaGenerator(
	ctx context.Context,
	repo comments.Repository,
	db *sql.DB,
	parentURI, sort, timeframe string,
	limit int,
	cursor *string,
	viewerDID string,
) ([]*comments.Comment, *string, error) {
	generator, err := commentGeneratorForSort(db, sort)
	if err != nil {
		return nil, nil, err
	}
	req := feedgen.CommentSkeletonRequest{
		ViewerDID:      viewerDID,
		Parents:        []string{parentURI},
		LimitPerParent: limit,
		Timeframe:      timeframe,
	}
	if cursor != nil {
		req.Cursor = *cursor
	}
	page, err := generator.CommentSkeleton(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var uris []string
	var next *string
	for _, replies := range page.Replies {
		if replies.Parent != parentURI {
			return nil, nil, fmt.Errorf("generator answered for parent %q, asked for %q", replies.Parent, parentURI)
		}
		uris = append(uris, replies.URIs...)
		if replies.Cursor != "" {
			nextCursor := replies.Cursor
			next = &nextCursor
		}
	}
	if len(uris) == 0 {
		return nil, next, nil
	}
	hydrated, err := repo.HydrateComments(ctx, uris, viewerDID)
	if err != nil {
		return nil, nil, err
	}
	return hydrated, next, nil
}

// listByParentsBatchViaGenerator is ListByParentsBatch's shape over the
// generator path: the replies of several parents keyed by parent, a parent
// with no replies absent from the map.
func listByParentsBatchViaGenerator(
	ctx context.Context,
	repo comments.Repository,
	db *sql.DB,
	parentURIs []string,
	sort string,
	limitPerParent int,
	viewerDID string,
) (map[string][]*comments.Comment, error) {
	generator, err := commentGeneratorForSort(db, sort)
	if err != nil {
		return nil, err
	}
	page, err := generator.CommentSkeleton(ctx, feedgen.CommentSkeletonRequest{
		ViewerDID:      viewerDID,
		Parents:        parentURIs,
		LimitPerParent: limitPerParent,
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string][]*comments.Comment)
	for _, replies := range page.Replies {
		if len(replies.URIs) == 0 {
			continue
		}
		hydrated, err := repo.HydrateComments(ctx, replies.URIs, viewerDID)
		if err != nil {
			return nil, err
		}
		if len(hydrated) > 0 {
			result[replies.Parent] = hydrated
		}
	}
	return result, nil
}

// productionCommentFeeds is commentfeedfixture.Registry for this package, which
// that fixture cannot be imported into: the three built-in comment generators
// over db, for a real comment service's WithFeedRegistry.
func productionCommentFeeds(db *sql.DB) *feedgen.Registry {
	return feedgen.NewRegistry(fixtures.InstanceDID(),
		feedgen.CommentsHotBuiltin(NewCommentsHotGenerator(db)),
		feedgen.CommentsTopBuiltin(NewCommentsTopGenerator(db)),
		feedgen.CommentsNewBuiltin(NewCommentsNewGenerator(db)),
	)
}
