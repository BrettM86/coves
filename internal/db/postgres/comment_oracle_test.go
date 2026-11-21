//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"Coves/internal/core/comments"

	"github.com/lib/pq"
)

// The comment query oracle: a verbatim copy of the SQL and row scan of the two
// repository methods the comment generators replace, ListByParentWithHotRank
// and ListByParentsBatch, as they stood at 533cbb10.
//
// It is the reference the equivalence and cost tests compare the generators
// against, so it must keep compiling and behaving exactly the same after those
// methods are deleted from comment_repo.go. It calls only helpers that stay in
// production code (commentHotRankSQL, buildCommentSortClause,
// parseCommentCursor, buildCommentCursor, accountRestrictedSQL); everything
// that lived only inside the two methods is copied here. Do not "fix" it: a
// change here changes what equivalence means.

// oracleListByParentQuery builds the SQL and arguments of today's
// ListByParentWithHotRank. rankedAt pins the hot rank clock of page one; zero
// means "now", truncated to microseconds as the method does. A cursor's own
// rankedAt always wins, as it does in the method. The returned time is the rank
// clock the query used (zero for top and new), which the next cursor carries.
func oracleListByParentQuery(
	r *postgresCommentRepo,
	parentURI string,
	sort string,
	timeframe string,
	limit int,
	cursor *string,
	viewerDID string,
	rankedAt time.Time,
) (string, []any, time.Time, error) {
	orderBy, timeFilter := r.buildCommentSortClause(sort, timeframe)

	cursorFilter, cursorValues, err := r.parseCommentCursor(cursor, sort)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("invalid cursor: %w", err)
	}

	args := []any{parentURI, limit + 1} // +1 to detect next page

	if sort == "hot" {
		if len(cursorValues) == 0 {
			if rankedAt.IsZero() {
				// Microseconds, the precision of timestamptz.
				rankedAt = time.Now().UTC().Truncate(time.Microsecond)
			}
			args = append(args, rankedAt.Format(time.RFC3339Nano))
		} else {
			rankedAtText, isString := cursorValues[0].(string)
			if !isString {
				return "", nil, time.Time{}, fmt.Errorf("invalid cursor: %w: rankedAt is not a string", comments.ErrInvalidCursor)
			}
			rankedAt, err = time.Parse(time.RFC3339Nano, rankedAtText)
			if err != nil {
				return "", nil, time.Time{}, fmt.Errorf("invalid cursor: %w: rankedAt is not a timestamp", comments.ErrInvalidCursor)
			}
		}
	} else {
		rankedAt = time.Time{}
	}
	args = append(args, cursorValues...)
	args = append(args, r.authorityDID)
	authorityExpr := fmt.Sprintf("$%d", len(args))

	var selectClause string
	if sort == "hot" {
		selectClause = fmt.Sprintf(`
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by, c.hide_reason,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			%s as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle,
			%s AS commenter_restricted
		FROM comments c`, commentHotRankSQL("c", "$3::timestamptz"), accountRestrictedSQL("c.commenter_did", authorityExpr))
	} else {
		selectClause = `
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by, c.hide_reason,
			c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
			NULL::numeric as hot_rank,
			COALESCE(u.handle, c.commenter_did) as author_handle,
			` + accountRestrictedSQL("c.commenter_did", authorityExpr) + ` AS commenter_restricted
		FROM comments c`
	}

	var viewerFilter string
	if viewerDID != "" {
		args = append(args, viewerDID)
		viewerFilter = fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = c.commenter_did)", len(args))
	}

	query := fmt.Sprintf(`
		%s
		LEFT JOIN users u ON c.commenter_did = u.did
		WHERE c.parent_uri = $1
			%s
			%s
			%s
		ORDER BY %s
		LIMIT $2
	`, selectClause, timeFilter, cursorFilter, viewerFilter, orderBy)

	return query, args, rankedAt, nil
}

// oracleListByParentWithHotRank is today's ListByParentWithHotRank.
func oracleListByParentWithHotRank(
	ctx context.Context,
	r *postgresCommentRepo,
	parentURI string,
	sort string,
	timeframe string,
	limit int,
	cursor *string,
	viewerDID string,
) ([]*comments.Comment, *string, error) {
	return oracleListByParentWithHotRankAt(ctx, r, parentURI, sort, timeframe, limit, cursor, viewerDID, time.Time{})
}

// oracleListByParentWithHotRankAt is oracleListByParentWithHotRank with an
// explicit page-one rank clock (zero = now).
func oracleListByParentWithHotRankAt(
	ctx context.Context,
	r *postgresCommentRepo,
	parentURI string,
	sort string,
	timeframe string,
	limit int,
	cursor *string,
	viewerDID string,
	rankedAt time.Time,
) ([]*comments.Comment, *string, error) {
	query, args, rankedAt, err := oracleListByParentQuery(r, parentURI, sort, timeframe, limit, cursor, viewerDID, rankedAt)
	if err != nil {
		return nil, nil, err
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query comments with hot rank: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*comments.Comment
	for rows.Next() {
		comment, err := oracleScanCommentRow(rows)
		if err != nil {
			return nil, nil, err
		}
		result = append(result, comment)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating comments: %w", err)
	}

	var nextCursor *string
	if len(result) > limit && limit > 0 {
		result = result[:limit]
		lastComment := result[len(result)-1]
		cursorStr := r.buildCommentCursor(lastComment, sort, rankedAt)
		nextCursor = &cursorStr
	}

	return result, nextCursor, nil
}

// oracleListByParentsBatchQuery builds the SQL and arguments of today's
// ListByParentsBatch (LATERAL per-parent limit; hot ranked at NOW()). Callers
// handle the empty parent list themselves, as the method did.
func oracleListByParentsBatchQuery(
	r *postgresCommentRepo,
	parentURIs []string,
	sort string,
	limitPerParent int,
	viewerDID string,
) (string, []any) {
	var rankColumn, innerOrderBy, outerOrderBy string
	switch sort {
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

	// $1=parentURIs, $2=limitPerParent, $3=the account-ban authority, $4=viewer
	var viewerFilter string
	var viewerArgs []any
	if viewerDID != "" {
		viewerParamIdx := 4
		viewerFilter = fmt.Sprintf("AND NOT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_did = $%d AND blocked_did = c.commenter_did)", viewerParamIdx)
		viewerArgs = append(viewerArgs, viewerDID)
	}

	query := fmt.Sprintf(`
		SELECT
			c.id, c.uri, c.cid, c.rkey, c.commenter_did,
			c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
			c.content, c.content_facets, c.embed, c.content_labels, c.langs,
			c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by, c.hide_reason,
			c.upvote_count, c.downvote_count, c.score, c.reply_count,
			c.hot_rank, COALESCE(u.handle, c.commenter_did) AS author_handle,
			%s AS commenter_restricted
		FROM (SELECT DISTINCT unnest($1::text[]) AS uri) AS parent
		CROSS JOIN LATERAL (
			SELECT
				c.id, c.uri, c.cid, c.rkey, c.commenter_did,
				c.root_uri, c.root_cid, c.parent_uri, c.parent_cid,
				c.content, c.content_facets, c.embed, c.content_labels, c.langs,
				c.created_at, c.indexed_at, c.deleted_at, c.deletion_reason, c.deleted_by, c.hide_reason,
				c.upvote_count + c.bridged_upvote_count AS upvote_count, c.downvote_count + c.bridged_downvote_count AS downvote_count, c.score, c.reply_count,
				%s AS hot_rank
			FROM comments c
			WHERE c.parent_uri = parent.uri
				%s
			ORDER BY %s
			LIMIT $2
		) c
		LEFT JOIN users u ON c.commenter_did = u.did
		ORDER BY c.parent_uri, %s
	`, accountRestrictedSQL("c.commenter_did", "$3"), rankColumn, viewerFilter, innerOrderBy, outerOrderBy)

	args := []any{pq.Array(parentURIs), limitPerParent, r.authorityDID}
	args = append(args, viewerArgs...)
	return query, args
}

// oracleListByParentsBatch is today's ListByParentsBatch.
func oracleListByParentsBatch(
	ctx context.Context,
	r *postgresCommentRepo,
	parentURIs []string,
	sort string,
	limitPerParent int,
	viewerDID string,
) (map[string][]*comments.Comment, error) {
	if len(parentURIs) == 0 {
		return make(map[string][]*comments.Comment), nil
	}

	query, args := oracleListByParentsBatchQuery(r, parentURIs, sort, limitPerParent, viewerDID)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to batch query comments by parents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string][]*comments.Comment)
	for rows.Next() {
		comment, err := oracleScanCommentRow(rows)
		if err != nil {
			return nil, err
		}
		result[comment.ParentURI] = append(result[comment.ParentURI], comment)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating comments: %w", err)
	}

	return result, nil
}

// oracleScanCommentRow is the row scan both methods shared verbatim: the full
// comment columns, the (discarded) hot_rank, the author handle and the
// account-restriction flag.
func oracleScanCommentRow(rows *sql.Rows) (*comments.Comment, error) {
	var comment comments.Comment
	var langs pq.StringArray
	var hotRank sql.NullFloat64
	var authorHandle string

	err := rows.Scan(
		&comment.ID, &comment.URI, &comment.CID, &comment.RKey, &comment.CommenterDID,
		&comment.RootURI, &comment.RootCID, &comment.ParentURI, &comment.ParentCID,
		&comment.Content, &comment.ContentFacets, &comment.Embed, &comment.ContentLabels, &langs,
		&comment.CreatedAt, &comment.IndexedAt, &comment.DeletedAt, &comment.DeletionReason, &comment.DeletedBy, &comment.HideReason,
		&comment.UpvoteCount, &comment.DownvoteCount, &comment.Score, &comment.ReplyCount,
		&hotRank, &authorHandle, &comment.CommenterRestricted,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to scan comment: %w", err)
	}

	comment.Langs = langs
	comment.CommenterHandle = authorHandle
	return &comment, nil
}
