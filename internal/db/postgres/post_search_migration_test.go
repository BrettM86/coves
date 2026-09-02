//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostSearchMigration(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "searchmigration")
	author := "did:plc:searchmigrationauthor"
	createTestUser(t, db, "searchmigrationauthor.test", author)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	titleMatchURI := seedVisibilityPost(t, db, community, author, "searchtitle", "Rust borrow checker tips", createdAt)
	contentMatchURI := seedPostWithContentOnly(t, db, community, author, "searchcontent", "notes on the rust borrow checker", createdAt)
	nonMatchURI := seedVisibilityPost(t, db, community, author, "searchcontrol", "Go generics", createdAt)

	searchesForRust := func(t *testing.T, uri string) bool {
		t.Helper()
		var matches bool
		err := db.QueryRowContext(ctx, `
			SELECT search_vector @@ websearch_to_tsquery('english', 'rust')
			FROM posts
			WHERE uri = $1
		`, uri).Scan(&matches)
		require.NoErrorf(t, err, "evaluating the generated search vector for %s", uri)
		return matches
	}

	assert.True(t, searchesForRust(t, titleMatchURI),
		"a title containing 'Rust' must match the generated search vector; omitting title makes title-only posts unsearchable")
	assert.True(t, searchesForRust(t, contentMatchURI),
		"content containing 'rust' must match when title is NULL; omitting content makes body-only posts unsearchable")
	assert.False(t, searchesForRust(t, nonMatchURI),
		"a post containing neither title nor content matching 'rust' must not satisfy the search query")

	var indexed int
	err := db.QueryRowContext(ctx, `
		SELECT 1
		FROM pg_indexes
		WHERE tablename = 'posts' AND indexname = 'idx_posts_search_vector'
	`).Scan(&indexed)
	require.NoError(t, err,
		"the posts search-vector GIN index must exist or full-text search will degrade to a table scan")
}

func seedPostWithContentOnly(t *testing.T, db *sql.DB, communityDID, authorDID, rkey, content string, createdAt time.Time) string {
	t.Helper()

	uri := postV2URI(authorDID, rkey)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, NULL, $6, $7, $8, $9, 0)
	`, uri, "bafypostv2"+rkey, rkey, authorDID, communityDID, content, createdAt, 1, 1)
	require.NoErrorf(t, err, "seeding content-only postv2 %s", rkey)
	return uri
}

func TestPostSearchMigration_BoundsGeneratedVectorInput(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "searchmigrationbounds")
	author := "did:plc:searchmigrationboundsauthor"
	createTestUser(t, db, "searchmigrationboundsauthor.test", author)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const (
		earlyContentMarker = "earlymarkerzq"
		lateContentMarker  = "latemarkerzq"
		earlyTitleMarker   = "earlytitlemarkerzq"
		lateTitleMarker    = "latetitlemarkerzq"
	)

	var oversizedContent strings.Builder
	oversizedContent.Grow(1_200_000)
	oversizedContent.WriteString(earlyContentMarker)
	oversizedContent.WriteByte(' ')
	for token := 0; token < 150_000; token++ {
		if token == 20_000 {
			require.Greater(t, oversizedContent.Len(), 100_000,
				"the late content marker fixture must begin after the 100,000-character indexing boundary")
			oversizedContent.WriteString(lateContentMarker)
			oversizedContent.WriteByte(' ')
		}
		oversizedContent.WriteByte('w')
		oversizedContent.WriteString(strconv.Itoa(token))
		oversizedContent.WriteByte(' ')
	}
	content := oversizedContent.String()
	require.Greater(t, len(content), 1_000_000,
		"150,000 distinct alphanumeric lexemes must exceed the tsvector size ceiling before the generated expression truncates them")

	contentURI := seedPostWithContentOnly(t, db, community, author, "searchboundedcontent", content, createdAt)

	searchVectorMatches := func(t *testing.T, uri, query string) bool {
		t.Helper()
		var matches bool
		err := db.QueryRowContext(ctx, `
			SELECT search_vector @@ websearch_to_tsquery('english', $2)
			FROM posts
			WHERE uri = $1
		`, uri, query).Scan(&matches)
		require.NoErrorf(t, err, "searching the generated vector for %q in %s", query, uri)
		return matches
	}

	assert.True(t, searchVectorMatches(t, contentURI, earlyContentMarker),
		"content inside the first 100,000 characters must remain searchable after bounding the generated tsvector input")
	assert.False(t, searchVectorMatches(t, contentURI, lateContentMarker),
		"content after character 100,000 must not enter the generated search vector; indexing it would restore the oversized-input failure")

	title := earlyTitleMarker + " " + strings.Repeat("padding ", 500) + lateTitleMarker
	require.Greater(t, strings.Index(title, lateTitleMarker), 3_000,
		"the late title marker fixture must begin after the 3,000-character indexing boundary")
	titleURI := seedVisibilityPost(t, db, community, author, "searchboundedtitle", title, createdAt.Add(time.Hour))

	assert.True(t, searchVectorMatches(t, titleURI, earlyTitleMarker),
		"a title marker inside the first 3,000 characters must remain searchable")
	assert.False(t, searchVectorMatches(t, titleURI, lateTitleMarker),
		"a title marker after character 3,000 must not enter the generated search vector")
}

func TestPostSearchMigration_SearchVectorIndexIsGINAndPartial(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	var indexDefinition string
	err := db.QueryRowContext(context.Background(), `
		SELECT indexdef
		FROM pg_indexes
		WHERE tablename = 'posts' AND indexname = 'idx_posts_search_vector'
	`).Scan(&indexDefinition)
	require.NoError(t, err,
		"the posts search-vector index must exist before its access method and predicate can be verified")
	assert.Contains(t, indexDefinition, "USING gin",
		"idx_posts_search_vector must be a GIN index or full-text matching cannot use the intended inverted-index access path")
	assert.Contains(t, indexDefinition, "WHERE (deleted_at IS NULL)",
		"idx_posts_search_vector must remain partial so deleted posts do not consume search-index space")
}
