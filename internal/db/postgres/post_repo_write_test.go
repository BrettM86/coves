//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"
)

// The two writes on the post indexing path: Create, which is how a post enters
// the AppView, and SoftDelete, which is how it leaves.
//
// Neither is called by a request handler. Both are called by the Jetstream
// consumer as it replays a community's repository, which is what makes their
// error behaviour more interesting than their happy path:
//
//   - Create is NOT an upsert. Every other firehose-fed writer in this package
//     resolves a replayed record with ON CONFLICT; this one turns the duplicate
//     into an error string and drops the event. That is a deliberate choice
//     about immutability rather than an oversight, and it is worth having on
//     the record, because it means the second delivery of an EDITED post is
//     discarded rather than applied.
//   - ONE foreign key is left, and it catches a post whose community the AppView
//     has not indexed yet — an ordinary occurrence on a cold start, where
//     records arrive in repository order rather than in dependency order. The
//     repository translates the violation by constraint name. The AUTHOR key
//     used to do the same and is deliberately gone (migration 034, PRD
//     §5.3): under author-owned posts an author's profile may live on a PDS
//     this AppView will never index, so an unknown author is a normal state
//     rather than an ordering artefact. See the subtest below.
//   - SoftDelete is where "deleted" is defined. It sets one column, and what
//     that column means is decided entirely by which read paths filter on it.
//     Two of the three do. The third is pinned below.
//
// Posts hang off users(did) and communities(did), so each test seeds both. That
// is not ceremony: the FKs are the branches under test.

// postAuthorAndCommunity seeds the two rows a post cannot exist without and
// returns their DIDs.
func postAuthorAndCommunity(t *testing.T, db *sql.DB) (authorDID, communityDID string) {
	t.Helper()
	id := testkit.UniqueID(t)
	authorDID = "did:plc:author" + id
	communityDID = "did:plc:comm" + id
	createTestUser(t, db, "author-"+id+".test", authorDID)
	createTestCommunity(t, db, communityDID, "c-"+id+".coves.social", communityDID)
	return authorDID, communityDID
}

// postRecord builds a minimal indexable post. A test that asserts on a field
// sets that field itself, so the fixture can never be the reason an assertion
// passes.
func postRecord(authorDID, communityDID, rkey string) *posts.Post {
	return &posts.Post{
		URI:          "at://" + communityDID + "/social.coves.community.post/" + rkey,
		CID:          "bafypost-" + rkey,
		RKey:         rkey,
		AuthorDID:    authorDID,
		CommunityDID: communityDID,
		CreatedAt:    time.Now().UTC(),
	}
}

// postText is a pointer-to-string helper; the Post struct models every optional
// column as a pointer so that "absent" and "empty" stay distinguishable.
func postText(s string) *string { return &s }

// postDeletedAt reads deleted_at straight from the row. The soft-delete
// timestamp is not on any read path that filters by it, so this is the only way
// to tell "deleted again" from "deleted once".
func postDeletedAt(t *testing.T, db *sql.DB, uri string) *time.Time {
	t.Helper()
	var deletedAt sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM posts WHERE uri = $1`, uri).Scan(&deletedAt))
	if !deletedAt.Valid {
		return nil
	}
	return &deletedAt.Time
}

func TestPostRepo_Create(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("round-trips every column a post record can carry", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		facets := `[{"index":{"byteStart":0,"byteEnd":5},"features":[{"$type":"app.bsky.richtext.facet#link","uri":"https://example.com"}]}]`
		embed := `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com","title":"Example"}}`
		labels := `{"values":[{"val":"nsfw","neg":false}]}`
		// createdAt belongs to the RECORD and indexed_at to the AppView. Setting
		// them a day apart is what proves the column takes the author's
		// timestamp rather than the DEFAULT NOW() the migration also offers.
		authored := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)

		post := postRecord(authorDID, communityDID, "full"+testkit.UniqueID(t))
		post.Title = postText("A post with everything")
		post.Content = postText("Hello, world")
		post.ContentFacets = &facets
		post.Embed = &embed
		post.ContentLabels = &labels
		post.CreatedAt = authored

		require.NoError(t, repo.Create(ctx, post))

		assert.NotZero(t, post.ID, "the serial primary key is written back through RETURNING; without "+
			"it the consumer has no handle on the row it just indexed")
		assert.WithinDuration(t, time.Now(), post.IndexedAt, time.Minute)
		assert.False(t, post.IndexedAt.Equal(authored),
			"indexed_at must be when the AppView saw the record, not when the author wrote it")

		stored, err := repo.GetByURI(ctx, post.URI)
		require.NoError(t, err)
		assert.Equal(t, post.CID, stored.CID)
		assert.Equal(t, post.RKey, stored.RKey)
		assert.Equal(t, authorDID, stored.AuthorDID)
		assert.Equal(t, communityDID, stored.CommunityDID)
		require.NotNil(t, stored.Title)
		assert.Equal(t, "A post with everything", *stored.Title)
		require.NotNil(t, stored.Content)
		assert.Equal(t, "Hello, world", *stored.Content)
		require.NotNil(t, stored.ContentFacets)
		assert.JSONEq(t, facets, *stored.ContentFacets,
			"facets carry the link and mention ranges; losing them renders a post as plain text with "+
				"dead links")
		require.NotNil(t, stored.Embed)
		assert.JSONEq(t, embed, *stored.Embed)
		require.NotNil(t, stored.ContentLabels)
		assert.JSONEq(t, labels, *stored.ContentLabels,
			"the labels blob is stored whole so the 'neg' flag survives; flattening it to a list of "+
				"values would turn a negated content warning into an applied one")
		assert.True(t, stored.CreatedAt.Equal(authored),
			"createdAt = %v, want the record's %v", stored.CreatedAt, authored)
		assert.Nil(t, stored.EditedAt)
		assert.Nil(t, stored.DeletedAt)
	})

	t.Run("a bare post stores NULLs rather than empty strings", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		post := postRecord(authorDID, communityDID, "bare"+testkit.UniqueID(t))
		require.NoError(t, repo.Create(ctx, post))

		stored, err := repo.GetByURI(ctx, post.URI)
		require.NoError(t, err)
		assert.Nil(t, stored.Title, "a post with no title must read back as absent; an empty string "+
			"renders as a blank heading rather than as no heading")
		assert.Nil(t, stored.Content)
		assert.Nil(t, stored.ContentFacets)
		assert.Nil(t, stored.Embed)
		assert.Nil(t, stored.ContentLabels)
	})

	t.Run("starts with no votes and no comments", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		post := postRecord(authorDID, communityDID, "counts"+testkit.UniqueID(t))
		require.NoError(t, repo.Create(ctx, post))

		stored, err := repo.GetByURI(ctx, post.URI)
		require.NoError(t, err)
		assert.Zero(t, stored.UpvoteCount)
		assert.Zero(t, stored.DownvoteCount)
		assert.Zero(t, stored.Score)
		assert.Zero(t, stored.CommentCount,
			"the counters are maintained by the vote and comment consumers; a post that arrived with "+
				"a non-zero score would be ranked on a number nobody earned")
	})

	// The firehose is at-least-once, so the same create arrives twice. Unlike
	// every other indexed record in this package, a post is NOT upserted: the
	// duplicate is refused, and the row already on disk wins.
	t.Run("a replayed create is refused and the indexed row wins", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		rkey := "dupe" + testkit.UniqueID(t)
		first := postRecord(authorDID, communityDID, rkey)
		first.Title = postText("As indexed")
		require.NoError(t, repo.Create(ctx, first))

		replay := postRecord(authorDID, communityDID, rkey)
		replay.Title = postText("As replayed")
		replay.CID = "bafypost-edited"

		err := repo.Create(ctx, replay)
		require.Error(t, err)
		assert.ErrorContains(t, err, "post already indexed",
			"the raw constraint name is not something a consumer can branch on; the repository owns "+
				"the translation, and a consumer that could not recognise a replay would treat it as "+
				"an infrastructure failure and retry forever")

		stored, err := repo.GetByURI(ctx, first.URI)
		require.NoError(t, err)
		require.NotNil(t, stored.Title)
		assert.Equal(t, "As indexed", *stored.Title,
			"IF THIS FAILED, Create learned to upsert. That would be a behaviour change worth "+
				"knowing about: today a post record is treated as immutable, so a re-published "+
				"(edited) record at the same URI is dropped rather than applied, and the AppView "+
				"keeps serving the first version it saw")
		assert.Equal(t, "bafypost-"+rkey, stored.CID)
	})

	// THIS INVARIANT WAS DELIBERATELY INVERTED. Until migration 034 this subtest
	// asserted the opposite — that Create ERRORS with "author DID not found" —
	// and that was correct while posts.author_did carried a hard FK to users.
	//
	// Author-owned posts abolished it (PRD_AUTHOR_OWNED_POSTS §5.3). A post
	// record now lives in its AUTHOR's repo, and open federated posting means
	// the author may be someone this AppView has no users row for and may never
	// get one: the users consumer default-denies identities from untrusted
	// hosts, so treating an unknown author as a write failure made a federated
	// author's post permanently unindexable. Migration 034 drops fk_author (and
	// its ON DELETE CASCADE, so a profile row going away can no longer erase
	// indexed posts). The author reference is soft now; profiles are hydrated
	// opportunistically afterwards, and a post whose author is still a bare DID
	// is a normal indexed row rather than a dead letter.
	t.Run("indexes a post whose author is not yet known", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		_, communityDID := postAuthorAndCommunity(t, db)

		unknownAuthor := "did:plc:unindexed" + testkit.UniqueID(t)
		post := postRecord(unknownAuthor, communityDID, "unknownauthor"+testkit.UniqueID(t))

		require.NoError(t, repo.Create(ctx, post),
			"an author with no users row must index: under author-owned posts that is a federated "+
				"author, not an ordering artefact waiting on a backfill")

		stored, err := repo.GetByURI(ctx, post.URI)
		require.NoError(t, err, "the post must be readable back, not half-written")
		assert.Equal(t, unknownAuthor, stored.AuthorDID,
			"the author DID is carried on the row itself; it is the only identity the AppView has "+
				"until the profile is hydrated")

		var indexedAuthors int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE did = $1`, unknownAuthor).Scan(&indexedAuthors))
		assert.Zero(t, indexedAuthors,
			"IF THIS FAILED, something started bootstrapping a users row as a side effect of indexing "+
				"a post. That would make this test pass for the wrong reason — the point is that the "+
				"post indexes with NO author row at all")
	})

	// The community FK is still real, and is still the thing that catches a post
	// arriving before its community's declaration on a cold start.
	t.Run("names the community it could not find", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		// The author is unknown TOO, which is the case that matters now that
		// fk_author is gone: the community must still be the record the
		// consumer is sent to fetch, and an unknown author must contribute
		// nothing to the diagnosis.
		unknownAuthor := "did:plc:unindexed" + testkit.UniqueID(t)
		orphanCommunity := "did:plc:unindexedcomm" + testkit.UniqueID(t)
		post := postRecord(unknownAuthor, orphanCommunity, "orphancomm"+testkit.UniqueID(t))

		err := repo.Create(ctx, post)
		require.Error(t, err)
		assert.ErrorContains(t, err, "community DID not found")
		assert.ErrorContains(t, err, orphanCommunity)
		assert.NotContains(t, err.Error(), "author DID not found",
			"IF THIS FAILED, the author FK came back: matching it would send the consumer backfilling "+
				"a user when the missing record is a community")
	})

	// embed, content_facets and content_labels are JSONB columns, not text. A
	// post whose embed did not parse must be refused at the write rather than
	// stored and discovered later by a reader — scanPostView logs a parse
	// failure and serves the post without its embed, so a bad blob that got in
	// would be invisible until someone wondered where the images went.
	t.Run("refuses a record whose JSON columns do not parse", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		for _, tc := range []struct {
			name  string
			apply func(*posts.Post)
		}{
			{"embed", func(p *posts.Post) { p.Embed = postText("this is not json") }},
			{"facets", func(p *posts.Post) { p.ContentFacets = postText("{unclosed") }},
			{"labels", func(p *posts.Post) { p.ContentLabels = postText("nsfw") }},
		} {
			post := postRecord(authorDID, communityDID, tc.name+testkit.UniqueID(t))
			tc.apply(post)

			err := repo.Create(ctx, post)
			require.Errorf(t, err, "%s: a JSONB column accepted a value that is not JSON", tc.name)

			_, err = repo.GetByURI(ctx, post.URI)
			assert.ErrorIsf(t, err, posts.ErrNotFound, "%s: the rejected post was indexed anyway", tc.name)
		}
	})

	t.Run("two posts by the same author in the same community coexist", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		first := postRecord(authorDID, communityDID, "one"+testkit.UniqueID(t))
		second := postRecord(authorDID, communityDID, "two"+testkit.UniqueID(t))
		require.NoError(t, repo.Create(ctx, first))
		require.NoError(t, repo.Create(ctx, second))

		assert.NotEqual(t, first.ID, second.ID, "the uniqueness is per URI, not per author")
	})
}

// TestPostRepo_SoftDelete covers what a firehose delete event actually does to
// the post's visibility.
//
// The write itself is one column. Everything that makes it a deletion lives in
// the read paths, so every subtest here goes through a reader rather than
// through deleted_at.
func TestPostRepo_SoftDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// postDeletedFixture indexes two posts by one author in one community and
	// deletes the first, so every assertion has both a subject and a control.
	type postDeletedFixture struct {
		repo         posts.Repository
		authorDID    string
		communityDID string
		deletedURI   string
		survivorURI  string
	}
	seed := func(t *testing.T) postDeletedFixture {
		t.Helper()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		deleted := postRecord(authorDID, communityDID, "deleted"+testkit.UniqueID(t))
		deleted.Title = postText("Withdrawn by its author")
		deleted.Content = postText("the body nobody should still be served")
		survivor := postRecord(authorDID, communityDID, "survivor"+testkit.UniqueID(t))
		survivor.Title = postText("Still here")
		require.NoError(t, repo.Create(context.Background(), deleted))
		require.NoError(t, repo.Create(context.Background(), survivor))
		require.NoError(t, repo.SoftDelete(context.Background(), deleted.URI))

		return postDeletedFixture{
			repo: repo, authorDID: authorDID, communityDID: communityDID,
			deletedURI: deleted.URI, survivorURI: survivor.URI,
		}
	}

	t.Run("the post drops out of the batch hydration path", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		views, err := fixture.repo.GetViewsByURIs(ctx, []string{fixture.deletedURI, fixture.survivorURI})
		require.NoError(t, err)
		assert.NotContains(t, views, fixture.deletedURI,
			"a deleted post must be absent from the map so the caller emits a notFoundPost marker; "+
				"serving it would republish content its author withdrew from their repository")
		assert.Contains(t, views, fixture.survivorURI,
			"deleting one post removed another from the same batch")
	})

	t.Run("the post drops out of the author's feed", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		feed, _, err := fixture.repo.GetByAuthor(ctx, posts.GetAuthorPostsRequest{
			ActorDID: fixture.authorDID, Limit: 50,
		})
		require.NoError(t, err)

		uris := make([]string, 0, len(feed))
		for _, view := range feed {
			uris = append(uris, view.URI)
		}
		assert.NotContains(t, uris, fixture.deletedURI)
		assert.Equal(t, []string{fixture.survivorURI}, uris,
			"the author feed is a public listing; a withdrawn post appearing on it is the deletion "+
				"not having happened as far as any reader is concerned")
	})

	// PINNED DEFECT. GetByURI has no `deleted_at IS NULL` predicate, so it
	// serves a withdrawn post in full — title, body, facets and all. It is not a
	// dead path: comments.GetComments calls it to build the post header for a
	// thread and never inspects DeletedAt, so
	// social.coves.community.comment.getComments still returns the whole of a
	// deleted post to an anonymous caller. This is the same hole task 12 found
	// one table over, in the comment thread reads.
	t.Run("but GetByURI still serves it in full", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		stored, err := fixture.repo.GetByURI(ctx, fixture.deletedURI)
		require.NoError(t, err,
			"IF THIS FAILED (issue 2026-07-29-deleted-posts-still-served-by-getcomments.md) the defect is FIXED — delete this pin. The right behaviour is "+
				"posts.ErrNotFound (or a caller that checks DeletedAt): a post withdrawn from its "+
				"community's repository must not be readable through this path")
		require.NotNil(t, stored.DeletedAt, "the row is marked deleted")
		require.NotNil(t, stored.Content)
		assert.Equal(t, "the body nobody should still be served", *stored.Content,
			"IF THIS FAILED (issue 2026-07-29-deleted-posts-still-served-by-getcomments.md) the defect is FIXED — delete this pin. comments.GetComments builds its "+
				"post header from exactly this value and never looks at DeletedAt, so today a deleted "+
				"post's title and body are still served to anonymous callers through getComments")
	})

	// The WHERE clause carries `AND deleted_at IS NULL`, so a replayed delete
	// event cannot move the deletion timestamp forward. That timestamp is the
	// record of when the author withdrew the post, and the firehose delivers the
	// same event more than once as a matter of course.
	t.Run("a replayed delete does not move the deletion timestamp", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		authorDID, communityDID := postAuthorAndCommunity(t, db)

		post := postRecord(authorDID, communityDID, "replay"+testkit.UniqueID(t))
		require.NoError(t, repo.Create(ctx, post))
		require.NoError(t, repo.SoftDelete(ctx, post.URI))

		first := postDeletedAt(t, db, post.URI)
		require.NotNil(t, first)

		// Backdate, so a second write has something unambiguous to overwrite;
		// two NOW() calls microseconds apart would not distinguish the cases.
		_, err := db.ExecContext(ctx,
			`UPDATE posts SET deleted_at = $2 WHERE uri = $1`, post.URI, first.Add(-72*time.Hour))
		require.NoError(t, err)
		backdated := postDeletedAt(t, db, post.URI)
		require.NotNil(t, backdated)

		require.NoError(t, repo.SoftDelete(ctx, post.URI))

		after := postDeletedAt(t, db, post.URI)
		require.NotNil(t, after)
		assert.True(t, after.Equal(*backdated),
			"a second delete event rewrote when the post was withdrawn; deleted_at = %v, want %v",
			*after, *backdated)
	})

	// Documented as idempotent, and it is — but the same silence covers a URI
	// that was never indexed. Recorded here so the next person to add a caller
	// knows that a nil error from SoftDelete is not evidence anything was
	// deleted.
	t.Run("deleting a URI nothing indexed is silent", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewPostRepository(db)
		_, communityDID := postAuthorAndCommunity(t, db)

		absent := "at://" + communityDID + "/social.coves.community.post/neverindexed"
		assert.NoError(t, repo.SoftDelete(ctx, absent),
			"IF THIS FAILED (issue 2026-07-29-deleted-posts-still-served-by-getcomments.md), SoftDelete learned to report that it matched no rows. That is an "+
				"improvement for a consumer that wants to know a delete arrived before its create — "+
				"assert the new error here rather than reverting")

		_, err := repo.GetByURI(ctx, absent)
		assert.ErrorIs(t, err, posts.ErrNotFound, "and no row was conjured by the delete")
	})

	t.Run("deletes the one URI named", func(t *testing.T) {
		t.Parallel()
		fixture := seed(t)

		survivor, err := fixture.repo.GetByURI(ctx, fixture.survivorURI)
		require.NoError(t, err)
		assert.Nil(t, survivor.DeletedAt,
			"deleting one post marked another as deleted; only the URI predicate separates them")
	})
}
