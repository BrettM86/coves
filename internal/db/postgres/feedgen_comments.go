package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/feedgen"

	"github.com/lib/pq"
)

// CommentSortGenerator is a built-in comment generator (comments-hot,
// comments-top or comments-new). It returns comment URIs only; the comment
// repository's HydrateComments turns them into comments.
type CommentSortGenerator struct {
	db   *sql.DB
	sort string
	// sortSQL owns the sort, timeframe and cursor SQL helpers shared with the
	// comment repository. They read no repository state.
	sortSQL *postgresCommentRepo
}

func newCommentSortGenerator(db *sql.DB, sort string) *CommentSortGenerator {
	return &CommentSortGenerator{db: db, sort: sort, sortSQL: &postgresCommentRepo{db: db}}
}

// NewCommentsHotGenerator returns the comments-hot generator over db.
func NewCommentsHotGenerator(db *sql.DB) *CommentSortGenerator {
	return newCommentSortGenerator(db, "hot")
}

// NewCommentsTopGenerator returns the comments-top generator over db.
func NewCommentsTopGenerator(db *sql.DB) *CommentSortGenerator {
	return newCommentSortGenerator(db, "top")
}

// NewCommentsNewGenerator returns the comments-new generator over db.
func NewCommentsNewGenerator(db *sql.DB) *CommentSortGenerator {
	return newCommentSortGenerator(db, "new")
}

// CommentSkeleton orders the direct replies of each requested parent. A
// single parent is paged: it fetches one reply past LimitPerParent to decide
// whether to return a cursor. Several parents are read in one query, unpaged,
// LimitPerParent replies each. Deleted replies are included (they keep thread
// structure); replies by authors the viewer blocks are not.
func (g *CommentSortGenerator) CommentSkeleton(ctx context.Context, req feedgen.CommentSkeletonRequest) (feedgen.CommentSkeletonPage, error) {
	if len(req.Parents) == 0 {
		return feedgen.CommentSkeletonPage{Replies: []feedgen.CommentReplies{}}, nil
	}
	query, args, rankedAt, err := g.buildSkeletonQuery(req)
	if err != nil {
		return feedgen.CommentSkeletonPage{}, err
	}
	rows, err := g.db.QueryContext(ctx, query, args...)
	if err != nil {
		return feedgen.CommentSkeletonPage{}, fmt.Errorf("failed to rank comments: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("Failed to close rows: %v", err)
		}
	}()

	if len(req.Parents) == 1 {
		return g.scanParentPage(rows, req, rankedAt)
	}
	return scanParentsBatch(rows, req.Parents)
}

// scanParentPage reads a single parent's page and builds its next cursor from
// the last reply kept.
func (g *CommentSortGenerator) scanParentPage(rows *sql.Rows, req feedgen.CommentSkeletonRequest, rankedAt time.Time) (feedgen.CommentSkeletonPage, error) {
	var ranked []*comments.Comment
	for rows.Next() {
		var reply comments.Comment
		var hotRank sql.NullFloat64 // the ORDER BY key; the cursor does not carry it
		if err := rows.Scan(&reply.URI, &reply.Score, &reply.CreatedAt, &hotRank); err != nil {
			return feedgen.CommentSkeletonPage{}, fmt.Errorf("failed to scan ranked comment: %w", err)
		}
		ranked = append(ranked, &reply)
	}
	if err := rows.Err(); err != nil {
		return feedgen.CommentSkeletonPage{}, fmt.Errorf("error iterating ranked comments: %w", err)
	}

	replies := feedgen.CommentReplies{Parent: req.Parents[0], URIs: []string{}}
	limit := req.LimitPerParent
	if len(ranked) > limit && limit > 0 {
		ranked = ranked[:limit]
		replies.Cursor = g.sortSQL.buildCommentCursor(ranked[len(ranked)-1], g.sort, rankedAt)
	}
	for _, reply := range ranked {
		replies.URIs = append(replies.URIs, reply.URI)
	}
	return feedgen.CommentSkeletonPage{Replies: []feedgen.CommentReplies{replies}}, nil
}

// scanParentsBatch groups a batch's rows, which arrive in each parent's rank
// order, into one CommentReplies per requested parent in request order.
func scanParentsBatch(rows *sql.Rows, parents []string) (feedgen.CommentSkeletonPage, error) {
	byParent := make(map[string][]string, len(parents))
	for rows.Next() {
		var parentURI, uri string
		if err := rows.Scan(&parentURI, &uri); err != nil {
			return feedgen.CommentSkeletonPage{}, fmt.Errorf("failed to scan ranked comment: %w", err)
		}
		byParent[parentURI] = append(byParent[parentURI], uri)
	}
	if err := rows.Err(); err != nil {
		return feedgen.CommentSkeletonPage{}, fmt.Errorf("error iterating ranked comments: %w", err)
	}

	page := feedgen.CommentSkeletonPage{Replies: make([]feedgen.CommentReplies, 0, len(parents))}
	for _, parent := range parents {
		uris := byParent[parent]
		if uris == nil {
			uris = []string{}
		}
		page.Replies = append(page.Replies, feedgen.CommentReplies{Parent: parent, URIs: uris})
	}
	return page, nil
}

// skeletonQuery builds the rank SQL and arguments CommentSkeleton runs for req.
// The cost test EXPLAINs it.
func (g *CommentSortGenerator) skeletonQuery(req feedgen.CommentSkeletonRequest) (string, []any, error) {
	query, args, _, err := g.buildSkeletonQuery(req)
	return query, args, err
}

// buildSkeletonQuery is skeletonQuery plus the hot rank clock a single-parent
// page ranks at (zero for other sorts and for batches).
func (g *CommentSortGenerator) buildSkeletonQuery(req feedgen.CommentSkeletonRequest) (string, []any, time.Time, error) {
	switch len(req.Parents) {
	case 0:
		return "", nil, time.Time{}, fmt.Errorf("comment skeleton request has no parents")
	case 1:
		return g.parentPageQuery(req)
	default:
		if req.Cursor != "" {
			return "", nil, time.Time{}, fmt.Errorf("%w: a cursor pages a single parent, got %d parents",
				comments.ErrInvalidCursor, len(req.Parents))
		}
		query, args := g.parentsBatchQuery(req)
		return query, args, time.Time{}, nil
	}
}

// parentPageQuery ranks one parent's direct replies, one past the limit.
//
// Hot pins its rank clock, bound as $3, for the whole walk: page one ranks as
// of its own request time, and every later page ranks as of the rankedAt its
// cursor carries. Ranks decay at different speeds, so two replies can swap
// places between requests; a page ranked at its own request time would then
// skip a reply or serve one twice. Scores are read live.
func (g *CommentSortGenerator) parentPageQuery(req feedgen.CommentSkeletonRequest) (string, []any, time.Time, error) {
	orderBy, timeFilter := g.sortSQL.buildCommentSortClause(g.sort, req.Timeframe)

	var cursor *string
	if req.Cursor != "" {
		cursor = &req.Cursor
	}
	cursorFilter, cursorValues, err := g.sortSQL.parseCommentCursor(cursor, g.sort)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("invalid cursor: %w", err)
	}

	args := []any{req.Parents[0], req.LimitPerParent + 1}

	var rankedAt time.Time
	// The ORDER BY sorts on the hot_rank alias; other sorts leave it NULL.
	rankColumn := "NULL::float8"
	if g.sort == "hot" {
		if len(cursorValues) == 0 {
			// Microseconds, the precision of timestamptz.
			rankedAt = time.Now().UTC().Truncate(time.Microsecond)
			args = append(args, rankedAt.Format(time.RFC3339Nano))
		} else {
			// parseCommentCursor has validated this field and rewritten it in UTC; it
			// is read again as a time so the next cursor carries the same instant.
			rankedAtText, isString := cursorValues[0].(string)
			if !isString {
				return "", nil, time.Time{}, fmt.Errorf("invalid cursor: %w: rankedAt is not a string", comments.ErrInvalidCursor)
			}
			rankedAt, err = time.Parse(time.RFC3339Nano, rankedAtText)
			if err != nil {
				return "", nil, time.Time{}, fmt.Errorf("invalid cursor: %w: rankedAt is not a timestamp", comments.ErrInvalidCursor)
			}
		}
		rankColumn = commentHotRankSQL("c", "$3::timestamptz")
	}
	args = append(args, cursorValues...)

	var viewerFilter string
	if req.ViewerDID != "" {
		args = append(args, req.ViewerDID)
		viewerFilter = fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = c.commenter_did)", len(args))
	}

	query := fmt.Sprintf(`
		SELECT c.uri, c.score, c.created_at, %s AS hot_rank
		FROM comments c
		WHERE c.parent_uri = $1
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, rankColumn, timeFilter, cursorFilter, viewerFilter, orderBy)
	return query, args, rankedAt, nil
}

// parentsBatchQuery ranks the direct replies of several parents in one query.
// Each parent's replies are ranked and limited on their own (LATERAL ...
// LIMIT), so top and new read just LimitPerParent rows per parent off
// idx_comments_parent_score / idx_comments_parent. Hot ranks at NOW(); a batch
// is not paged.
func (g *CommentSortGenerator) parentsBatchQuery(req feedgen.CommentSkeletonRequest) (string, []any) {
	var rankColumn, innerOrderBy, outerOrderBy string
	switch g.sort {
	case "top":
		rankColumn = `NULL::numeric`
		innerOrderBy = `c.score DESC, c.created_at DESC, c.uri DESC`
		outerOrderBy = `c.score DESC, c.created_at DESC, c.uri DESC`
	case "new":
		rankColumn = `NULL::numeric`
		innerOrderBy = `c.created_at DESC, c.uri DESC`
		outerOrderBy = `c.created_at DESC, c.uri DESC`
	default:
		rankColumn = commentHotRankSQL("c", "NOW()")
		innerOrderBy = `hot_rank DESC, c.score DESC, c.created_at DESC, c.uri DESC`
		outerOrderBy = `c.hot_rank DESC, c.score DESC, c.created_at DESC, c.uri DESC`
	}
	var timeFilter string
	if g.sort == "top" {
		timeFilter = g.sortSQL.buildCommentTimeFilter(req.Timeframe)
	}

	args := []any{pq.Array(req.Parents), req.LimitPerParent}
	var viewerFilter string
	if req.ViewerDID != "" {
		args = append(args, req.ViewerDID)
		viewerFilter = fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = c.commenter_did)", len(args))
	}

	// DISTINCT keeps a parent listed twice from returning its replies twice.
	query := fmt.Sprintf(`
		SELECT c.parent_uri, c.uri
		FROM (SELECT DISTINCT unnest($1::text[]) AS uri) AS parent
		CROSS JOIN LATERAL (
			SELECT c.uri, c.parent_uri, c.score, c.created_at, %s AS hot_rank
			FROM comments c
			WHERE c.parent_uri = parent.uri
				%s
				%s
			ORDER BY %s
			LIMIT $2
		) c
		ORDER BY c.parent_uri, %s
	`, rankColumn, timeFilter, viewerFilter, innerOrderBy, outerOrderBy)
	return query, args
}
