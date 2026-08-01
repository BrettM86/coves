//go:build integration

package postgres

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/comments"
	"Coves/tests/fixtures"
)

// ListByCommenterWithCursor and the cursor codec it owns.
//
// This is the query behind a profile's comment history
// (social.coves.actor.getComments). It is the only comment read that FILTERS
// deleted rows rather than serving them as placeholders — a thread needs the
// tombstone to keep its shape, a person's own profile does not, and a retracted
// comment listed under its author's name is the retraction failing.
//
// It is also the only comment query that builds its parameter numbers by
// arithmetic. The cursor filter hardcodes $3 and $4, the community filter
// computes its own index from how many cursor values there happen to be, and
// the two are only ever exercised together on page two of a
// community-filtered profile — the exact combination no unit test of either
// half would reach. A mistake there is not a wrong answer, it is a
// "there is no parameter $5" from Postgres in production only.
//
// The pagination assertions here are all of the form "page through the whole
// set and prove every row was visited exactly once", because the two ways a
// cursor fails — skipping a row and repeating one — both look like a working
// feed from any single page.
//
// See comment_repo_write_test.go for commentEnv and the seeding helpers.

// commentEncodeCursor builds the wire form of a cursor from its plaintext, so a
// test can hand the parser something buildCommenterCursor would never emit.
func commentEncodeCursor(plaintext string) string {
	return base64.URLEncoding.EncodeToString([]byte(plaintext))
}

// commentDecodeCursor reads a cursor back to its plaintext.
func commentDecodeCursor(t *testing.T, cursor string) string {
	t.Helper()
	decoded, err := base64.URLEncoding.DecodeString(cursor)
	require.NoError(t, err, "a cursor the repository emitted must decode as base64url")
	return string(decoded)
}

// commentPageAll walks the whole feed with the given page size and returns
// every URI in visit order plus the number of requests it took.
func commentPageAll(t *testing.T, env *commentEnv, req comments.ListByCommenterRequest) ([]string, int) {
	t.Helper()

	var visited []string
	pages := 0
	for {
		page, next, err := env.repo.ListByCommenterWithCursor(env.ctx, req)
		require.NoError(t, err, "paging must not start failing partway through a feed")
		pages++
		visited = append(visited, commentURIs(page)...)
		if next == nil {
			return visited, pages
		}
		require.Less(t, pages, 20, "the cursor stopped advancing: this feed is an infinite scroll "+
			"that never reaches the end")
		req.Cursor = next
	}
}

func TestCommentRepo_ListByCommenterWithCursor(t *testing.T) {
	t.Parallel()

	t.Run("lists the author's comments newest first", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		oldest := env.seed(commentSpec{rkey: "old", createdAt: commentBaseTime})
		middle := env.seed(commentSpec{rkey: "mid", createdAt: commentBaseTime.Add(time.Hour)})
		newest := env.seed(commentSpec{rkey: "new", createdAt: commentBaseTime.Add(2 * time.Hour)})

		page, next, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{newest, middle, oldest}, commentURIs(page),
			"a profile reads as a chronology; oldest-first would bury a decade of history above "+
				"today's comment")
		assert.Nil(t, next, "a page that fit entirely must not offer a cursor to nowhere")
	})

	t.Run("shows nobody else's comments", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		mine := env.seed(commentSpec{rkey: "mine"})
		stranger := "did:plc:cmtcstranger" + env.id
		env.seed(commentSpec{rkey: "theirs", author: stranger, createdAt: commentBaseTime.Add(time.Hour)})

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{mine}, commentURIs(page))

		empty, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: "did:plc:cmtnobody" + env.id, Limit: 10,
		})
		require.NoError(t, err)
		assert.Empty(t, empty, "an actor with no comments must be an empty history, not an error "+
			"and not somebody else's")
	})

	// The other comment reads serve deleted rows on purpose. This one must not:
	// a profile is where a person's own retracted words would be republished
	// under their name.
	t.Run("omits deleted comments however they were deleted", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		kept := env.seed(commentSpec{rkey: "kept", createdAt: commentBaseTime})
		byAuthor := env.seed(commentSpec{rkey: "byauthor", createdAt: commentBaseTime.Add(time.Hour)})
		byModerator := env.seed(commentSpec{rkey: "bymod", createdAt: commentBaseTime.Add(2 * time.Hour)})
		bareDelete := env.seed(commentSpec{rkey: "bare", createdAt: commentBaseTime.Add(3 * time.Hour)})

		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, byAuthor, comments.DeletionReasonAuthor, env.author))
		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, byModerator, comments.DeletionReasonModerator, "did:plc:cmtcmod"))
		require.NoError(t, env.repo.Delete(env.ctx, bareDelete))

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{kept}, commentURIs(page),
			"the deleted_at IS NULL filter is the whole difference between this query and the thread "+
				"reads; without it a moderator removal would still be on the author's public profile")
	})

	t.Run("hydrates the author handle and falls back to the DID", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		env.seed(commentSpec{rkey: "known"})

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, "cmt-"+env.id+".test", page[0].CommenterHandle)

		stranger := "did:plc:cmtcunindexed" + env.id
		env.seed(commentSpec{rkey: "unindexed", author: stranger})
		strangerPage, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: stranger, Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, strangerPage, 1, "a comment from an author the firehose has not delivered yet "+
			"must still be listed; an INNER JOIN would drop the whole history")
		assert.Equal(t, stranger, strangerPage[0].CommenterHandle)
	})

	t.Run("folds bridged votes into the displayed counts", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "bridged"})
		env.setNativeVotes(uri, 2, 1)
		sampledAt := commentBaseTime.Add(time.Hour)
		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "c", Content: "body",
			BridgedUpvoteCount: 30, BridgedDownvoteCount: 4, BridgedStatsAsOf: &sampledAt,
		}))

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, 32, page[0].UpvoteCount)
		assert.Equal(t, 5, page[0].DownvoteCount)
	})
}

func TestCommentRepo_ListByCommenterCommunityFilter(t *testing.T) {
	t.Parallel()

	// The filter walks comments → their root post → that post's community, so it
	// needs real post rows in two different communities.
	seed := func(t *testing.T) (env *commentEnv, otherCommunity string, here, there []string) {
		t.Helper()
		env = commentEnvFor(t)

		otherCommunity, err := fixtures.Community(env.ctx, env.db, "cmtalt"+env.id, "cmtaltown"+env.id)
		require.NoError(t, err)
		otherRoot := fixtures.Post(t, env.db, otherCommunity, env.author, "elsewhere", 0, time.Now().UTC())

		for i, rkey := range []string{"here1", "here2", "here3"} {
			here = append(here, env.seed(commentSpec{
				rkey: rkey, createdAt: commentBaseTime.Add(time.Duration(i) * time.Hour),
			}))
		}
		for i, rkey := range []string{"there1", "there2"} {
			uri := "at://" + env.author + "/social.coves.community.comment/" + rkey
			require.NoError(t, env.repo.Create(env.ctx, &comments.Comment{
				URI: uri, CID: "bafy" + rkey, RKey: rkey, CommenterDID: env.author,
				RootURI: otherRoot, RootCID: "bafyroot", ParentURI: otherRoot, ParentCID: "bafyroot",
				Content: "in the other community", Langs: []string{"en"},
				CreatedAt: commentBaseTime.Add(time.Duration(i) * time.Hour),
			}))
			there = append(there, uri)
		}
		return env, otherCommunity, here, there
	}

	t.Run("restricts the history to one community", func(t *testing.T) {
		t.Parallel()
		env, otherCommunity, here, there := seed(t)

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10, CommunityDID: &env.community,
		})
		require.NoError(t, err)
		assert.ElementsMatch(t, here, commentURIs(page),
			"filtering by community is how a moderator reviews one person's history in the place "+
				"they moderate; leaking comments from elsewhere shows them content they have no "+
				"authority over")

		page, _, err = env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10, CommunityDID: &otherCommunity,
		})
		require.NoError(t, err)
		assert.ElementsMatch(t, there, commentURIs(page))
	})

	t.Run("an empty community string is no filter at all", func(t *testing.T) {
		t.Parallel()
		env, _, here, there := seed(t)
		blank := ""

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10, CommunityDID: &blank,
		})
		require.NoError(t, err)
		assert.Len(t, commentURIs(page), len(here)+len(there),
			"an unset filter arriving as a pointer to \"\" must behave like no filter; building the "+
				"SQL anyway would compare community_did against the empty string and return nothing")
	})

	t.Run("a community nobody has posted in is empty, not everything", func(t *testing.T) {
		t.Parallel()
		env, _, _, _ := seed(t)
		absent := "did:plc:cmtnosuchcommunity"

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10, CommunityDID: &absent,
		})
		require.NoError(t, err)
		assert.Empty(t, page)
	})

	// The combination that no half of this can reach on its own: page two of a
	// community-filtered profile puts the cursor at $3/$4 and the community DID
	// at $5. Get the arithmetic wrong and this is a runtime SQL error.
	t.Run("paginates while filtered, with the parameters in the right slots", func(t *testing.T) {
		t.Parallel()
		env, _, here, _ := seed(t)

		visited, pages := commentPageAll(t, env, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 2, CommunityDID: &env.community,
		})
		assert.Equal(t, 2, pages, "three comments at two per page is two requests")
		assert.ElementsMatch(t, here, visited,
			"the cursor filter hardcodes $3/$4 and the community filter computes its index from "+
				"how many cursor values exist; a mismatch is a bind-parameter error that only "+
				"appears on page two of a filtered profile")
		assert.Len(t, visited, len(here), "and no comment was visited twice")
	})
}

func TestCommentRepo_CommenterCursorPagination(t *testing.T) {
	t.Parallel()

	// Two of the five share a created_at exactly. Ties are where a cursor that
	// compares on timestamp alone loses a row: the second of the pair sorts
	// after the boundary but a "created_at < cursor" filter excludes it.
	//
	// The tied pair is INSERTED in the opposite order to the one the ORDER BY
	// asks for — "tiea" first, then "tieb", against a uri DESC tiebreak. Insert
	// them the other way round and Postgres returns them in the wanted order by
	// accident, and a query that had lost its tiebreak would look correct.
	//
	// The pair also straddles a page boundary at limit 2: they are the second
	// and third rows of the feed, so a cursor whose comparison chain stops at
	// the timestamp drops one of them instead of merely reordering them.
	seedFive := func(t *testing.T) (*commentEnv, []string) {
		t.Helper()
		env := commentEnvFor(t)
		var all []string
		for _, spec := range []commentSpec{
			{rkey: "e", createdAt: commentBaseTime},
			{rkey: "d", createdAt: commentBaseTime.Add(time.Hour)},
			{rkey: "tiea", createdAt: commentBaseTime.Add(2 * time.Hour)},
			{rkey: "tieb", createdAt: commentBaseTime.Add(2 * time.Hour)},
			{rkey: "a", createdAt: commentBaseTime.Add(3 * time.Hour)},
		} {
			all = append(all, env.seed(spec))
		}
		return env, all
	}

	t.Run("visits every comment exactly once across ties", func(t *testing.T) {
		t.Parallel()
		env, all := seedFive(t)

		visited, pages := commentPageAll(t, env, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 2,
		})
		assert.Equal(t, 3, pages, "five comments at two per page is three requests")
		assert.ElementsMatch(t, all, visited,
			"a comment that appears on no page has been silently deleted from its author's history, "+
				"and one that appears on two makes the feed look duplicated")
		assert.Len(t, visited, len(all))

		unpaged, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 50,
		})
		require.NoError(t, err)
		assert.Equal(t, commentURIs(unpaged), visited,
			"and paging must visit them in the same order a single large page would; the uri DESC "+
				"tiebreak is what makes the two tied comments come out in a fixed order at all")
	})

	// A set whose size is an exact multiple of the page size is where an
	// off-by-one shows: the last full page must not advertise a cursor to a page
	// that does not exist.
	t.Run("stops at an exact page boundary", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		for i, rkey := range []string{"p1", "p2", "p3", "p4"} {
			env.seed(commentSpec{rkey: rkey, createdAt: commentBaseTime.Add(time.Duration(i) * time.Hour)})
		}

		first, next, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 2,
		})
		require.NoError(t, err)
		require.Len(t, first, 2)
		require.NotNil(t, next)

		second, next, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 2, Cursor: next,
		})
		require.NoError(t, err)
		require.Len(t, second, 2)
		assert.Nil(t, next, "the query asks for limit+1 rows precisely so the last full page can tell "+
			"there is nothing after it; a cursor here sends the client to an empty page and, on a "+
			"client that treats an empty page as a network glitch, into a retry loop")
	})

	t.Run("a cursor past the end is an empty page rather than an error", func(t *testing.T) {
		t.Parallel()
		env, _ := seedFive(t)

		page, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		last := page[len(page)-1]
		cursor := env.impl.buildCommenterCursor(last)

		page, next, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10, Cursor: &cursor,
		})
		require.NoError(t, err)
		assert.Empty(t, page)
		assert.Nil(t, next)
	})

	// The cursor is built from a row that came out of Postgres, so the timestamp
	// it carries has to survive the round trip back into a timestamptz
	// comparison. Microsecond precision is where that breaks: Postgres truncates
	// to microseconds and Go formats to nanoseconds.
	t.Run("round-trips a timestamp with sub-second precision", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		precise := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
		first := env.seed(commentSpec{rkey: "precise1", createdAt: precise})
		second := env.seed(commentSpec{rkey: "precise2", createdAt: precise.Add(-time.Microsecond)})

		page, next, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 1,
		})
		require.NoError(t, err)
		require.Equal(t, []string{first}, commentURIs(page))
		require.NotNil(t, next)
		assert.Contains(t, commentDecodeCursor(t, *next), ".123456",
			"a cursor that dropped the fractional second would compare against the whole second and "+
				"skip or repeat every comment written in the same second")

		page, _, err = env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 1, Cursor: next,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{second}, commentURIs(page),
			"one microsecond of difference must still order these two")
	})

	t.Run("the cursor the repository builds is the one it parses", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "roundtrip"})
		stored, err := env.repo.GetByURI(env.ctx, uri)
		require.NoError(t, err)

		cursor := env.impl.buildCommenterCursor(stored)
		plaintext := commentDecodeCursor(t, cursor)
		assert.True(t, strings.HasSuffix(plaintext, "|"+uri),
			"the cursor is createdAt|uri; the URI half is the tiebreak that makes two comments "+
				"written in the same microsecond paginate deterministically")

		filter, values, err := env.impl.parseCommenterCursor(&cursor)
		require.NoError(t, err)
		assert.Contains(t, filter, "$3")
		assert.Contains(t, filter, "$4")
		require.Len(t, values, 2)
		assert.Equal(t, uri, values[1],
			"the parsed URI must be the one the cursor was built from, or the tiebreak compares "+
				"against the wrong row")
	})
}

func TestCommentRepo_CommenterCursorRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	env.seed(commentSpec{rkey: "target"})

	t.Run("no cursor means no filter", func(t *testing.T) {
		t.Parallel()
		filter, values, err := env.impl.parseCommenterCursor(nil)
		require.NoError(t, err)
		assert.Empty(t, filter, "a first page must add no WHERE clause at all")
		assert.Empty(t, values)

		blank := ""
		filter, values, err = env.impl.parseCommenterCursor(&blank)
		require.NoError(t, err)
		assert.Empty(t, filter, "a client that sends cursor=\"\" means the first page, not a broken one")
		assert.Empty(t, values)
	})

	// Every one of these must be REJECTED rather than quietly turned into "no
	// filter". A cursor that parses to nothing restarts the feed at the top,
	// which a scrolling client experiences as an endless loop over page one.
	for name, cursor := range map[string]string{
		"not base64":           "this is not base64 !!!",
		"no delimiter":         commentEncodeCursor("2026-03-04T05:06:07Z"),
		"too many fields":      commentEncodeCursor("2026-03-04T05:06:07Z|at://x/y/z|extra"),
		"empty":                commentEncodeCursor("|"),
		"uri is not an AT-URI": commentEncodeCursor("2026-03-04T05:06:07Z|https://example.com/evil"),
		"uri half missing":     commentEncodeCursor("2026-03-04T05:06:07Z|"),
	} {
		t.Run("rejects a cursor with "+name, func(t *testing.T) {
			t.Parallel()
			_, _, err := env.impl.parseCommenterCursor(&cursor)
			require.Error(t, err, "this cursor must not be accepted; silently ignoring it would "+
				"serve page one forever")

			_, _, err = env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
				CommenterDID: env.author, Limit: 10, Cursor: &cursor,
			})
			assert.Error(t, err, "and the query must refuse it too rather than falling back to the "+
				"unpaginated feed")
		})
	}

	// The size cap exists so a hostile client cannot make the AppView base64
	// decode a megabyte per request. It is checked before the decode, which is
	// the only order that helps.
	t.Run("rejects a cursor larger than the cap without decoding it", func(t *testing.T) {
		t.Parallel()
		huge := commentEncodeCursor("2026-03-04T05:06:07Z|at://" + strings.Repeat("a", 4096))
		require.Greater(t, len(huge), 1024)

		_, _, err := env.impl.parseCommenterCursor(&huge)
		require.Error(t, err)
		assert.ErrorContains(t, err, "cursor too large",
			"the size check must come before the base64 decode, or the cap protects nothing")
	})

	// A timestamp that is not a timestamp is not caught by the parser — it is
	// passed through as a bind parameter and Postgres rejects the cast. That is
	// still a rejection, which is what matters; the assertion is here so the
	// behaviour is on the record rather than assumed.
	t.Run("a non-timestamp is refused by the database", func(t *testing.T) {
		t.Parallel()
		cursor := commentEncodeCursor("not-a-timestamp|at://did:plc:x/social.coves.community.comment/y")

		filter, _, err := env.impl.parseCommenterCursor(&cursor)
		require.NoError(t, err, "the parser does not validate the timestamp half")
		require.NotEmpty(t, filter)

		_, _, err = env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10, Cursor: &cursor,
		})
		assert.Error(t, err, "the timestamptz cast is the backstop; without it a garbage cursor "+
			"would silently match every row")
	})
}

// TestCommentRepo_CommenterCursorErrorsAreNotClassifiable pins a mapping gap.
//
// parseCommentCursor — the codec for the thread queries, forty lines further
// down the same file — wraps every failure in comments.ErrInvalidCursor, and
// says in its own doc comment that callers need it "so callers can surface them
// as client input errors (HTTP 400) instead of server faults".
// parseCommenterCursor wraps nothing.
//
// The consequence is concrete. comment_service.go wraps the repository error,
// and internal/api/handlers/actor/get_comments.go:handleCommentServiceError
// tests for comments.IsNotFound, comments.ErrNotAuthorized and the substring
// "invalid request" before falling through to 500. A client that sends a
// malformed cursor to social.coves.actor.getComments therefore gets a 500 and
// an entry in the error log, for input the server correctly rejected.
//
// The fix is to wrap these four returns the way the sibling does.
func TestCommentRepo_CommenterCursorErrorsAreNotClassifiable(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	env.seed(commentSpec{rkey: "target"})

	for name, cursor := range map[string]string{
		"bad encoding": "!!! not base64 !!!",
		"wrong arity":  commentEncodeCursor("2026-03-04T05:06:07Z"),
		"bad uri":      commentEncodeCursor("2026-03-04T05:06:07Z|https://example.com"),
	} {
		cursor := cursor
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
				CommenterDID: env.author, Limit: 10, Cursor: &cursor,
			})
			require.Error(t, err)
			assert.NotErrorIs(t, err, comments.ErrInvalidCursor,
				"parseCommenterCursor should wrap its failures in comments.ErrInvalidCursor as "+
					"parseCommentCursor does, so a malformed cursor answers 400 instead of 500. "+
					"IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin and assert ErrorIs instead")
			assert.False(t, comments.IsValidationError(err),
				"and the domain's own classifier cannot see it either")
		})
	}
}

// TestCommentRepo_CommenterFeedIgnoresAZeroLimit pins an off-by-one.
//
// The query is issued with LIMIT req.Limit+1 so the extra row can signal "there
// is another page". Nothing floors the limit first, so a request for zero
// comments runs LIMIT 1 and returns one comment; the cursor block is guarded by
// req.Limit > 0, so the caller is handed a row it did not ask for and no cursor
// to continue from.
//
// The XRPC layer clamps limit to 1..100 before reaching here, so this is not
// live — but the repository is the layer that promises it, and the clamp is one
// handler away from being the only thing holding.
func TestCommentRepo_CommenterFeedIgnoresAZeroLimit(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	env.seed(commentSpec{rkey: "zero1"})
	env.seed(commentSpec{rkey: "zero2", createdAt: commentBaseTime.Add(time.Hour)})

	page, next, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
		CommenterDID: env.author, Limit: 0,
	})
	require.NoError(t, err)
	assert.Len(t, page, 1,
		"a limit of 0 should return no comments. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")
	assert.Nil(t, next)

	negative, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
		CommenterDID: env.author, Limit: -1,
	})
	require.NoError(t, err, "limit -1 becomes LIMIT 0, which Postgres accepts; a limit of -2 would "+
		"become LIMIT -1 and be a database error")
	assert.Empty(t, negative)
}
