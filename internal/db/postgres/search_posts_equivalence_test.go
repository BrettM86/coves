//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SearchPosts must serve exactly what it served at 6cab19ce, when one query
// both ranked and hydrated the matches: the same items on every page and the
// same cursor strings. The reference is the verbatim oracle
// (search_posts_oracle_test.go). Each path is paged at limit 2 with its own
// cursors, over one corpus, with the same cursor secret and account-restriction
// authority.

const searchEquivalenceCursorSecret = "search-equivalence-cursor-secret"

// searchEquivalenceCorpus names the seeded posts. Relevance tiers by title:
// "<token> <token> gamma" (high), "<token> beta" (middle, a rank tie), and a
// content-only match (low). In relevance order the hidden high-tier matches
// sit between A and C, and the blocked author's X sits between C and D2, so a
// filter applied only after the LIMIT would shorten those pages.
type searchEquivalenceCorpus struct {
	token string
	// Viewers: blocker authored the pending match and blocks the author of X
	// and community two; restricted is the restricted author.
	blocker, restricted string
	// communityOne holds most posts; communityTwo is blocked by blocker.
	communityOne, communityTwo string

	// Visible to everyone (subject to the viewer's blocks).
	a, c, x, bc, d, d2, e string
	// Visible only to their author.
	pending, restrictedPost string
}

func seedSearchEquivalenceCorpus(t *testing.T, db *sql.DB) searchEquivalenceCorpus {
	t.Helper()
	ctx := context.Background()

	label := testkit.UniqueID(t)
	f := searchEquivalenceCorpus{token: testkit.UniqueID(t)}
	author := "did:plc:" + label + "author"
	f.blocker = "did:plc:" + label + "blocker"
	f.restricted = "did:plc:" + label + "restricted"
	blockedAuthor := "did:plc:" + label + "blocked"
	for _, did := range []string{author, f.blocker, f.restricted, blockedAuthor} {
		createTestUser(t, db, did[len("did:plc:"):]+".test", did)
	}
	f.communityOne = visibilityCommunity(t, db, label+"one")
	f.communityTwo = visibilityCommunity(t, db, label+"two")
	removedCommunity := visibilityCommunity(t, db, label+"three")
	seedCommunityRemoval(t, db, fixtures.InstanceDID(), removedCommunity)
	insertUserBlock(t, db, f.blocker, blockedAuthor)
	insertCommunityBlock(t, db, f.blocker, f.communityTwo)
	accountstatusfixture.Set(t, db, f.restricted, accountstatus.Takendown)

	now := time.Now().UTC().Truncate(time.Microsecond)
	ago := func(age time.Duration) time.Time { return now.Add(-age) }
	high := f.token + " " + f.token + " gamma"
	middle := f.token + " beta"

	seed := func(communityDID, authorDID, rkey, title string, createdAt time.Time, score int, status posts.AdmissionStatus) string {
		uri := seedVisibilityPost(t, db, communityDID, authorDID, rkey, title, createdAt)
		acceptedCID := ""
		if status == posts.AdmissionStatusAccepted {
			acceptedCID = "bafypostv2" + rkey
		}
		seedVisibilityAdmission(t, db, communityDID, uri, status, acceptedCID, "")
		setSearchPostScore(t, db, uri, score)
		return uri
	}
	accepted := posts.AdmissionStatusAccepted

	// High tier, newest first.
	f.a = seed(f.communityOne, author, "alpha1", high, ago(time.Hour), 10, accepted)
	f.pending = seed(f.communityOne, f.blocker, "pend01", high, ago(time.Hour+10*time.Minute), 100, posts.AdmissionStatusPending)
	f.restrictedPost = seed(f.communityOne, f.restricted, "restr1", high, ago(time.Hour+20*time.Minute), 100, accepted)
	removed := seed(f.communityOne, author, "remov1", high, ago(time.Hour+30*time.Minute), 1000, accepted)
	insertDiscoverHotRemoval(t, db, removed)
	seed(removedCommunity, author, "rmcom1", high, ago(time.Hour+40*time.Minute), 1000, accepted)
	deleted := seed(f.communityOne, author, "delet1", high, ago(time.Hour+50*time.Minute), 1000, accepted)
	_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deleted)
	require.NoError(t, err, "soft-deleting the deleted match")

	// Middle tier: one rank, C shares A's created_at, D and D2 tie on rank and
	// created_at and are ordered by URI.
	f.c = seed(f.communityOne, author, "charl1", middle, ago(time.Hour), 10, accepted)
	f.x = seed(f.communityOne, blockedAuthor, "xblok1", middle, ago(time.Hour+30*time.Minute), 30, accepted)
	f.bc = seed(f.communityTwo, author, "bcomm1", middle, ago(2*time.Hour), 40, accepted)
	f.d = seed(f.communityOne, author, "delta1", middle, ago(3*24*time.Hour), 5, accepted)
	f.d2 = seed(f.communityOne, author, "delta2", middle, ago(3*24*time.Hour), 5, accepted)

	// Low tier: the token only in the body.
	f.e = seed(f.communityOne, author, "echo01", "echo notes", ago(time.Hour), 50, accepted)
	_, err = db.ExecContext(ctx, `UPDATE posts SET content = $2 WHERE uri = $1`, f.e, "body mentions "+f.token)
	require.NoError(t, err, "giving the low-tier match its body")

	// A non-match that would top every sort if the text predicate leaked.
	seed(f.communityOne, author, "nonmt1", "unrelated words", ago(time.Hour), 5000, accepted)
	return f
}

// searchEquivalencePage is one page of a search: its items and cursor ("" for
// none).
type searchEquivalencePage struct {
	items  []*communityFeeds.FeedViewPost
	cursor string
}

func (p searchEquivalencePage) uris() []string { return feedURIs(p.items) }

type searchPostsFunc func(context.Context, communityFeeds.SearchPostsRequest) ([]*communityFeeds.FeedViewPost, *string, error)

// readSearchPages pages one search path at limit 2 with its own cursors until
// the cursor runs out.
func readSearchPages(t *testing.T, search searchPostsFunc, req communityFeeds.SearchPostsRequest, label string) []searchEquivalencePage {
	t.Helper()

	const maxPages = 20
	var pages []searchEquivalencePage
	req.Limit = 2
	req.Cursor = nil
	for range maxPages {
		items, cursor, err := search(context.Background(), req)
		require.NoErrorf(t, err, "%s: page %d", label, len(pages)+1)
		page := searchEquivalencePage{items: items}
		if cursor != nil {
			page.cursor = *cursor
		}
		pages = append(pages, page)
		if page.cursor == "" {
			return pages
		}
		req.Cursor = cursor
	}
	require.FailNowf(t, "search did not end", "%s still had a cursor after %d pages", label, maxPages)
	return nil
}

func searchPagesURIs(pages []searchEquivalencePage) [][]string {
	uris := make([][]string, 0, len(pages))
	for _, page := range pages {
		uris = append(uris, page.uris())
	}
	return uris
}

// requireSameSearchPages requires the production pages to equal the oracle's:
// the same URIs per page, the same full items (as JSON) and the same cursor
// string on every page.
func requireSameSearchPages(t *testing.T, oracle, production []searchEquivalencePage, label string) {
	t.Helper()

	require.Equalf(t, searchPagesURIs(oracle), searchPagesURIs(production), "%s: URIs per page", label)
	require.Lenf(t, production, len(oracle), "%s: page count", label)
	for i := range oracle {
		wantJSON, err := json.Marshal(oracle[i].items)
		require.NoError(t, err)
		gotJSON, err := json.Marshal(production[i].items)
		require.NoError(t, err)
		require.JSONEqf(t, string(wantJSON), string(gotJSON), "%s: page %d items", label, i+1)
		require.Equalf(t, oracle[i].cursor, production[i].cursor, "%s: page %d cursor", label, i+1)
	}
}

func TestSearchPosts_MatchesOracle(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	f := seedSearchEquivalenceCorpus(t, db)
	restriction := WithAccountRestrictionAuthority(fixtures.InstanceDID())
	repo := NewCommunityFeedRepository(db, searchEquivalenceCursorSecret, restriction)
	oracle := newFeedOracle(db, searchEquivalenceCursorSecret, restriction)
	oracleSearch := func(ctx context.Context, req communityFeeds.SearchPostsRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
		return oracleSearchPosts(ctx, oracle, req)
	}

	read := func(t *testing.T, req communityFeeds.SearchPostsRequest, label string) []searchEquivalencePage {
		t.Helper()
		want := readSearchPages(t, oracleSearch, req, label+" oracle")
		got := readSearchPages(t, repo.SearchPosts, req, label+" production")
		requireSameSearchPages(t, want, got, label)
		return got
	}

	type sortCase struct{ sort, timeframe string }
	sorts := []sortCase{{"relevance", "all"}, {"new", "all"}, {"top", "all"}, {"top", "day"}}
	type namedValue struct{ name, value string }
	communityScopes := []namedValue{{"all-communities", ""}, {"community-one", f.communityOne}}
	viewers := []namedValue{{"anonymous", ""}, {"blocker", f.blocker}, {"restricted-author", f.restricted}}

	type row struct {
		name string
		req  communityFeeds.SearchPostsRequest
	}
	var rows []row
	for _, s := range sorts {
		for _, scope := range communityScopes {
			for _, viewer := range viewers {
				rows = append(rows, row{
					name: fmt.Sprintf("%s-%s/%s/%s", s.sort, s.timeframe, scope.name, viewer.name),
					req: communityFeeds.SearchPostsRequest{
						Query: f.token, Community: scope.value, ViewerDID: viewer.value,
						Sort: s.sort, Timeframe: s.timeframe,
					},
				})
			}
		}
		// The blocker scoping search to the community they block: the explicit
		// surface overrides the community mute.
		rows = append(rows, row{
			name: fmt.Sprintf("%s-%s/community-two/blocker", s.sort, s.timeframe),
			req: communityFeeds.SearchPostsRequest{
				Query: f.token, Community: f.communityTwo, ViewerDID: f.blocker,
				Sort: s.sort, Timeframe: s.timeframe,
			},
		})
	}

	multiPage := false
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			pages := read(t, r.req, r.name)
			total := 0
			for _, page := range pages {
				total += len(page.items)
			}
			require.Positivef(t, total, "%s: the row compared no items", r.name)
			if len(pages) >= 2 {
				multiPage = true
			}
		})
	}
	require.True(t, multiPage, "at least one row must span two or more pages so cursors are compared")

	// Hand-written orders for the rows that place hidden matches between
	// visible ones. Anonymous: the pending, restricted, removed,
	// removed-community and deleted high-tier matches sit between A and C.
	t.Run("relevance literal order anonymous", func(t *testing.T) {
		pages := read(t, communityFeeds.SearchPostsRequest{Query: f.token, Sort: "relevance", Timeframe: "all"}, "relevance anonymous literal")
		assert.Equal(t, [][]string{{f.a, f.c}, {f.x, f.bc}, {f.d2, f.d}, {f.e}}, searchPagesURIs(pages),
			"anonymous relevance search: hidden matches filtered before the limit, rank ties by created_at then URI")
	})
	// The blocker: their own pending post is visible; the blocked author's X
	// sits between C and D2, and community two's BC is muted.
	t.Run("relevance literal order blocker", func(t *testing.T) {
		pages := read(t, communityFeeds.SearchPostsRequest{Query: f.token, ViewerDID: f.blocker, Sort: "relevance", Timeframe: "all"}, "relevance blocker literal")
		assert.Equal(t, [][]string{{f.a, f.pending}, {f.c, f.d2}, {f.d, f.e}}, searchPagesURIs(pages),
			"blocker relevance search: own pending post shown, blocked author and blocked community filtered before the limit")
	})
}
