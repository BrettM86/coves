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

	"Coves/internal/core/comments"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
)

// The comment repository's write half — Update, the two delete forms and the
// permalink lookup that has to keep serving what they leave behind.
//
// These four are the moderation and privacy surface of the comments table, and
// none of them had a line of coverage. What they get wrong is not visible from
// the read side unless you know what to look for:
//
//   - Update is the firehose's edit path. It must rewrite content without
//     touching the vote aggregates a completely separate consumer maintains,
//     and it arbitrates bridged (origin-platform) vote counts against an
//     out-of-order asOf stamp INSIDE the UPDATE, because doing it as
//     read-check-write in the consumer would race. A regression here does not
//     fail: it silently rolls a comment's score back to whatever a stale
//     Lemmy sample said.
//
//   - SoftDeleteWithReason is what "delete my comment" actually does. The row
//     survives so the thread keeps its shape, which means the ONLY thing
//     standing between a deleted comment and a permanent public copy of its
//     text is the blanking in that one UPDATE. Every read path in this
//     repository deliberately serves deleted rows.
//
//   - Delete — the deprecated sibling — does not blank anything, and that is
//     the hole task 12 found one table over. See
//     TestCommentRepo_DeleteLeavesTheTextReadable.
//
//   - GetByRootAndRkey resolves a comment permalink. It is the one read that
//     must still answer for a deleted comment (so its children render under a
//     "[deleted]" placeholder) while still honouring a viewer's blocks.
//
// Everything is written through the repository rather than through the
// Jetstream consumer on purpose: what is under test is the SQL, and going in
// through the consumer would make these assertions fail whenever the consumer
// broke. Where a test needs a state the repository cannot produce (native vote
// counts, which a different consumer owns) it writes the column directly and
// says so.

// commentEnv is one isolated database with the foreign-key context a comment
// needs: an indexed author, a community, and a real post row to hang a thread
// off.
//
// Comments themselves carry no foreign keys — migration 016 removed them so
// out-of-order Jetstream events cannot deadlock — but the community filter in
// the commenter feed joins comments to posts to communities, so a fixture that
// skipped those rows could not exercise it.
//
// Shared by comment_repo_write_test.go, comment_repo_batch_test.go,
// comment_repo_commenter_test.go and comment_repo_cursor_test.go.
type commentEnv struct {
	t         *testing.T
	ctx       context.Context
	db        *sql.DB
	repo      comments.Repository
	impl      *postgresCommentRepo
	author    string // commenter DID that HAS a users row, so handles hydrate
	community string
	root      string // the post every seeded comment hangs under
	id        string
}

func commentEnvFor(t *testing.T) *commentEnv {
	t.Helper()

	db := testkit.DB(t)
	ctx := context.Background()
	id := testkit.UniqueID(t)

	author := "did:plc:cmt" + id
	createTestUser(t, db, "cmt-"+id+".test", author)

	community, err := fixtures.Community(ctx, db, "cmt"+id, "cmtown"+id)
	require.NoError(t, err, "seeding the community the thread lives in")
	root := fixtures.Post(t, db, community, author, "thread "+id, 0, time.Now().UTC())

	repo := NewCommentRepository(db)
	return &commentEnv{
		t:         t,
		ctx:       ctx,
		db:        db,
		repo:      repo,
		impl:      repo.(*postgresCommentRepo),
		author:    author,
		community: community,
		root:      root,
		id:        id,
	}
}

// commentSpec is the handful of fields a seeded comment ever needs to vary.
// Zero values mean "author, directly under the root post, at the base time".
type commentSpec struct {
	rkey      string
	author    string
	parent    string
	createdAt time.Time
	content   string
	score     int
}

// commentBaseTime is a fixed point far enough in the past that "top over the
// last hour" excludes it and recent enough to be a plausible comment.
var commentBaseTime = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// seed writes one comment and returns its AT-URI.
//
// A non-zero score is applied with a direct UPDATE afterwards rather than
// through the insert: upvote_count/score are owned by the vote consumer, the
// repository's Create deliberately does not accept them, and a fixture that
// pretended otherwise would be testing a row shape no consumer can produce.
func (e *commentEnv) seed(spec commentSpec) string {
	e.t.Helper()

	author := spec.author
	if author == "" {
		author = e.author
	}
	parent := spec.parent
	if parent == "" {
		parent = e.root
	}
	createdAt := spec.createdAt
	if createdAt.IsZero() {
		createdAt = commentBaseTime
	}
	content := spec.content
	if content == "" {
		content = "body of " + spec.rkey
	}

	uri := "at://" + author + "/social.coves.community.comment/" + spec.rkey
	require.NoError(e.t, e.repo.Create(e.ctx, &comments.Comment{
		URI:          uri,
		CID:          "bafy" + spec.rkey,
		RKey:         spec.rkey,
		CommenterDID: author,
		RootURI:      e.root,
		RootCID:      "bafyroot",
		ParentURI:    parent,
		ParentCID:    "bafyparent",
		Content:      content,
		Langs:        []string{"en"},
		CreatedAt:    createdAt,
	}), "seeding comment %s", spec.rkey)

	if spec.score != 0 {
		e.setNativeVotes(uri, spec.score, 0)
	}
	return uri
}

// setNativeVotes writes the aggregate columns the vote consumer owns.
func (e *commentEnv) setNativeVotes(uri string, upvotes, downvotes int) {
	e.t.Helper()
	_, err := e.db.ExecContext(e.ctx, `
		UPDATE comments SET upvote_count = $2, downvote_count = $3, score = $2::int - $3::int
		WHERE uri = $1
	`, uri, upvotes, downvotes)
	require.NoError(e.t, err, "seeding native vote aggregates")
}

// rawComment reads the columns no repository method exposes unfolded, so a
// test can tell a native vote from a bridged one.
func (e *commentEnv) rawComment(uri string) (nativeUp, nativeDown, bridgedUp, bridgedDown, score, replies int, content string, deletedAt *time.Time) {
	e.t.Helper()
	err := e.db.QueryRowContext(e.ctx, `
		SELECT upvote_count, downvote_count, bridged_upvote_count, bridged_downvote_count,
		       score, reply_count, content, deleted_at
		FROM comments WHERE uri = $1
	`, uri).Scan(&nativeUp, &nativeDown, &bridgedUp, &bridgedDown, &score, &replies, &content, &deletedAt)
	require.NoError(e.t, err, "reading raw comment columns")
	return
}

// TestCommentRepo_CreateRejectsAMalformedRecord covers the one failure branch
// of Create that a real firehose can produce.
//
// content_facets, embed and content_labels are JSONB columns fed straight from
// a record on somebody else's PDS. Nothing between the wire and this INSERT
// re-serialises them, so a peer that writes a malformed facets blob reaches the
// column as text Postgres cannot cast. It must fail here rather than be stored
// as something no read path can parse.
//
// The other branch — mapping "duplicate key" to ErrCommentAlreadyExists — is
// unreachable: uri is the only unique constraint on the table and the INSERT
// carries ON CONFLICT (uri) DO NOTHING, so a duplicate returns no rows and is
// swallowed as an idempotent replay two lines earlier. It is left uncovered on
// purpose rather than reached through a contrived schema change.
func TestCommentRepo_CreateRejectsAMalformedRecord(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	notJSON := "definitely not json"
	uri := "at://" + env.author + "/social.coves.community.comment/malformed"

	err := env.repo.Create(env.ctx, &comments.Comment{
		URI: uri, CID: "bafymal", RKey: "malformed", CommenterDID: env.author,
		RootURI: env.root, RootCID: "bafyroot", ParentURI: env.root, ParentCID: "bafyroot",
		Content: "body", ContentFacets: &notJSON, CreatedAt: commentBaseTime,
	})
	require.Error(t, err, "a JSONB column fed from a peer's record must reject text that is not JSON")
	assert.NotErrorIs(t, err, comments.ErrCommentAlreadyExists,
		"a type error is not a duplicate; reporting it as one would make the consumer treat a "+
			"malformed record as successfully indexed and never retry or dead-letter it")

	_, err = env.repo.GetByURI(env.ctx, uri)
	assert.ErrorIs(t, err, comments.ErrCommentNotFound, "and no partial row was left behind")
}

// A replayed create is not an error: the firehose is at-least-once, and the
// consumer would otherwise dead-letter every redelivery.
func TestCommentRepo_CreateIsIdempotentOnReplay(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	uri := env.seed(commentSpec{rkey: "replayed", content: "the original text"})

	replay := &comments.Comment{
		URI: uri, CID: "bafydifferent", RKey: "replayed", CommenterDID: env.author,
		RootURI: env.root, RootCID: "bafyroot", ParentURI: env.root, ParentCID: "bafyroot",
		Content: "a different body", Langs: []string{"en"}, CreatedAt: commentBaseTime,
	}
	require.NoError(t, env.repo.Create(env.ctx, replay))

	got, err := env.repo.GetByURI(env.ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "the original text", got.Content,
		"ON CONFLICT DO NOTHING means a replay must not rewrite the row; a create that behaved "+
			"like an upsert would let a stale redelivery revert an edit that arrived after it")
	assert.Zero(t, replay.ID,
		"and the caller gets no id back, because no row was written — the RETURNING clause "+
			"produced nothing")
}

func TestCommentRepo_Update(t *testing.T) {
	t.Parallel()

	t.Run("rewrites the record fields and leaves the aggregates alone", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "edited"})
		env.setNativeVotes(uri, 9, 2)
		_, err := env.db.ExecContext(env.ctx, `UPDATE comments SET reply_count = 4 WHERE uri = $1`, uri)
		require.NoError(t, err)

		facets := `[{"index":{"byteStart":0,"byteEnd":4}}]`
		embed := `{"$type":"social.coves.embed.images"}`
		labels := `{"values":[{"val":"nsfw"}]}`
		edit := &comments.Comment{
			URI:           uri,
			CID:           "bafyedited",
			Content:       "the corrected text",
			ContentFacets: &facets,
			Embed:         &embed,
			ContentLabels: &labels,
			Langs:         []string{"en", "fr"},
		}
		require.NoError(t, env.repo.Update(env.ctx, edit))

		got, err := env.repo.GetByURI(env.ctx, uri)
		require.NoError(t, err)
		assert.Equal(t, "the corrected text", got.Content)
		assert.Equal(t, "bafyedited", got.CID, "the CID pins the version; an edit that kept the old one "+
			"would make every strong reference to this comment point at text that no longer exists")
		require.NotNil(t, got.ContentFacets)
		assert.JSONEq(t, facets, *got.ContentFacets)
		require.NotNil(t, got.Embed)
		assert.JSONEq(t, embed, *got.Embed)
		require.NotNil(t, got.ContentLabels)
		assert.JSONEq(t, labels, *got.ContentLabels)
		assert.Equal(t, []string{"en", "fr"}, []string(got.Langs))

		assert.Equal(t, 9, got.UpvoteCount, "the vote consumer owns upvote_count and runs concurrently "+
			"with this UPDATE; an edit that reset it would delete every vote cast between the two events")
		assert.Equal(t, 2, got.DownvoteCount)
		assert.Equal(t, 4, got.ReplyCount, "reply_count is maintained by the comment consumer's insert "+
			"path; an edit that zeroed it would collapse every visible thread")
		assert.WithinDuration(t, commentBaseTime, got.CreatedAt, time.Second,
			"created_at is the commenter's own timestamp and an edit may not move it — it is the sort "+
				"key of every chronological view")
	})

	// The RETURNING clause is how the consumer learns the post-update state
	// without a second round trip. It hands back the RAW native counts (not the
	// display-folded ones GetByURI computes), because the consumer's regression
	// guard has to reason about native and bridged separately.
	t.Run("hands the caller back the row it did not send", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "returning"})
		env.setNativeVotes(uri, 6, 1)

		edit := &comments.Comment{URI: uri, CID: "bafyr", Content: "v2"}
		require.NoError(t, env.repo.Update(env.ctx, edit))

		assert.NotZero(t, edit.ID, "the caller had no id before the call and needs one after it")
		assert.Equal(t, 6, edit.UpvoteCount)
		assert.Equal(t, 1, edit.DownvoteCount)
		assert.Equal(t, 5, edit.Score)
		assert.WithinDuration(t, commentBaseTime, edit.CreatedAt, time.Second)
		assert.False(t, edit.IndexedAt.IsZero())
	})

	// bridgedStats are an origin platform's vote counts, asserted by the bridge
	// and sampled at some instant. Jetstream can deliver two samples out of
	// order, so the UPDATE compares the incoming asOf against the stored one and
	// applies all three bridged columns atomically or none of them.
	t.Run("bridged vote stats apply, then refuse to regress", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "bridged"})
		env.setNativeVotes(uri, 3, 0)

		fresh := commentBaseTime.Add(2 * time.Hour)
		stale := commentBaseTime.Add(1 * time.Hour)

		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "b1", Content: "v1",
			BridgedUpvoteCount: 10, BridgedDownvoteCount: 2, BridgedStatsAsOf: &fresh,
		}))
		_, _, bridgedUp, bridgedDown, score, _, _, _ := env.rawComment(uri)
		assert.Equal(t, 10, bridgedUp)
		assert.Equal(t, 2, bridgedDown)
		assert.Equal(t, 11, score, "score must fold native and bridged together (3-0+10-2); a score "+
			"that counted only native votes would rank bridged content as if nobody had voted on it")

		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "b2", Content: "v2",
			BridgedUpvoteCount: 999, BridgedDownvoteCount: 0, BridgedStatsAsOf: &stale,
		}))
		_, _, bridgedUp, bridgedDown, score, _, content, _ := env.rawComment(uri)
		assert.Equal(t, 10, bridgedUp, "an older sample overwrote a newer one: replaying an out-of-order "+
			"Jetstream event would rewrite a comment's score to whatever the origin platform said hours ago")
		assert.Equal(t, 2, bridgedDown)
		assert.Equal(t, 11, score)
		assert.Equal(t, "v2", content, "the content edit in the same UPDATE must still land — the "+
			"regression guard covers the bridged columns only")

		// A record carrying no applicable bridgedStats leaves the stored ones
		// standing rather than clearing them to zero.
		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "b3", Content: "v3",
			BridgedUpvoteCount: 77, BridgedDownvoteCount: 0, BridgedStatsAsOf: nil,
		}))
		_, _, bridgedUp, _, score, _, _, _ = env.rawComment(uri)
		assert.Equal(t, 10, bridgedUp, "an edit with no bridgedStats must not adopt whatever happened to "+
			"be in the struct's bridged fields, and must not wipe the counts already applied")
		assert.Equal(t, 11, score)
	})

	// Equal asOf re-applies rather than being rejected. The comparison is
	// >= rather than >, which matters twice: an at-least-once replay carries an
	// identical sample and must not be treated as a regression, and two
	// genuinely different samples can collide on asOf when the origin platform
	// truncates its timestamp — in which case the later delivery must win.
	t.Run("an identical asOf re-applies rather than being refused", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "replay"})
		sampledAt := commentBaseTime.Add(time.Hour)

		for i := 0; i < 2; i++ {
			require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
				URI: uri, CID: "r", Content: "same",
				BridgedUpvoteCount: 8, BridgedDownvoteCount: 1, BridgedStatsAsOf: &sampledAt,
			}))
		}
		_, _, bridgedUp, bridgedDown, score, _, _, _ := env.rawComment(uri)
		assert.Equal(t, 8, bridgedUp, "a replayed sample must not be discarded as a regression")
		assert.Equal(t, 1, bridgedDown)
		assert.Equal(t, 7, score, "and must not be double counted")

		// Same stamp, different counts: the correction has to land.
		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "r2", Content: "corrected",
			BridgedUpvoteCount: 12, BridgedDownvoteCount: 3, BridgedStatsAsOf: &sampledAt,
		}))
		_, _, bridgedUp, bridgedDown, score, _, _, _ = env.rawComment(uri)
		assert.Equal(t, 12, bridgedUp, "asOf is compared with >= precisely so a corrected sample "+
			"whose timestamp collided with the previous one is not silently thrown away; with a "+
			"strict > the first delivery would win permanently")
		assert.Equal(t, 3, bridgedDown)
		assert.Equal(t, 9, score)
	})

	t.Run("reports a comment that was never indexed", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)

		err := env.repo.Update(env.ctx, &comments.Comment{
			URI: "at://" + env.author + "/social.coves.community.comment/neverseen", Content: "x",
		})
		require.ErrorIs(t, err, comments.ErrCommentNotFound,
			"an update for a comment the AppView never indexed is a gap in the firehose, and the "+
				"consumer backfills on this specific error; a nil return would leave the comment missing forever")
	})

	// The UPDATE carries "AND deleted_at IS NULL". An edit that arrived after a
	// delete must not resurrect the text: the PDS record is gone, and the
	// AppView is not entitled to a copy of it.
	t.Run("refuses to edit a comment that has been deleted", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "tombstone", content: "original"})
		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, uri, comments.DeletionReasonAuthor, env.author))

		err := env.repo.Update(env.ctx, &comments.Comment{URI: uri, CID: "resurrect", Content: "back from the dead"})
		require.ErrorIs(t, err, comments.ErrCommentNotFound,
			"an out-of-order edit must not un-delete a comment")

		_, _, _, _, _, _, content, deletedAt := env.rawComment(uri)
		assert.Empty(t, content, "and must not put text back into a blanked row")
		assert.NotNil(t, deletedAt)
	})

	t.Run("a malformed facets payload fails loudly and changes nothing", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "badjson", content: "intact"})
		notJSON := "this is not json"

		err := env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "bad", Content: "replacement", ContentFacets: &notJSON,
		})
		require.Error(t, err, "content_facets is JSONB; a record whose facets are not JSON must be "+
			"rejected rather than stored as a string the read path cannot parse")
		assert.NotErrorIs(t, err, comments.ErrCommentNotFound,
			"a type error is not a missing comment; reporting it as one would make the consumer "+
				"try to backfill a comment that is already there")

		_, _, _, _, _, _, content, _ := env.rawComment(uri)
		assert.Equal(t, "intact", content, "a failed UPDATE is a single statement and must be all-or-nothing")
	})
}

func TestCommentRepo_Delete(t *testing.T) {
	t.Parallel()

	t.Run("marks the row deleted and is idempotent", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "gone"})

		require.NoError(t, env.repo.Delete(env.ctx, uri))
		_, _, _, _, _, _, _, deletedAt := env.rawComment(uri)
		require.NotNil(t, deletedAt, "the delete did not mark the row")

		firstStamp := *deletedAt
		require.NoError(t, env.repo.Delete(env.ctx, uri), "Jetstream is at-least-once; a redelivered "+
			"delete must not be an error")
		_, _, _, _, _, _, _, deletedAt = env.rawComment(uri)
		require.NotNil(t, deletedAt)
		assert.Equal(t, firstStamp, *deletedAt, "the WHERE deleted_at IS NULL guard is what keeps a "+
			"redelivered delete from moving the deletion time; a moving timestamp would reorder the "+
			"moderation log every time the firehose replayed")
	})

	t.Run("a delete for a comment nobody indexed is silent", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)

		assert.NoError(t, env.repo.Delete(env.ctx, "at://"+env.author+"/social.coves.community.comment/never"),
			"a delete that arrives before the create it refers to is normal on an out-of-order "+
				"firehose and must not stall the consumer")
	})

	t.Run("the deleted comment leaves the author's own history", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		kept := env.seed(commentSpec{rkey: "kept"})
		removed := env.seed(commentSpec{rkey: "removed", createdAt: commentBaseTime.Add(time.Minute)})
		require.NoError(t, env.repo.Delete(env.ctx, removed))

		list, _, err := env.repo.ListByCommenterWithCursor(env.ctx, comments.ListByCommenterRequest{
			CommenterDID: env.author, Limit: 10,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{kept}, commentURIs(list),
			"the profile comment history filters deleted_at IS NULL; a deleted comment listed there "+
				"would be a user's own retracted words served back on their public profile")
	})
}

// TestCommentRepo_DeleteLeavesTheTextReadable pins the privacy hole in the
// deprecated Delete path.
//
// Delete sets deleted_at and nothing else. Every thread-shaped read in this
// repository deliberately serves rows with deleted_at set — that is how a
// deleted comment keeps its children under a "[deleted]" placeholder — and they
// all select `content` verbatim. So a comment removed through Delete keeps
// serving its full text, to anonymous callers, forever.
//
// It is the same shape as the posts defect task 12 found, and only two things
// keep it from being live: Delete has no production caller (the consumer uses
// SoftDeleteWithReasonTx, internal/atproto/jetstream/comment_consumer.go:478),
// and SoftDeleteWithReason blanks the columns Delete does not. The method is
// still on the exported Repository interface, so the next caller inherits this.
//
// The right behaviour is for Delete to blank content/facets/embed/labels the
// way SoftDeleteWithReasonTx does, or to be removed from the interface.
func TestCommentRepo_DeleteLeavesTheTextReadable(t *testing.T) {
	t.Parallel()
	env := commentEnvFor(t)
	const secret = "text the author asked to have removed"
	uri := env.seed(commentSpec{rkey: "leaky", content: secret})

	require.NoError(t, env.repo.Delete(env.ctx, uri))

	single, err := env.repo.GetByURI(env.ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, secret, single.Content,
		"Delete must blank the content the way SoftDeleteWithReason does. "+
			"IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")

	batch, err := env.repo.GetByURIsBatch(env.ctx, []string{uri})
	require.NoError(t, err)
	require.Contains(t, batch, uri)
	assert.Equal(t, secret, batch[uri].Content,
		"the batch thread fetch serves the deleted text too. "+
			"IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")

	tree, _, err := env.repo.ListByParentWithHotRank(env.ctx, env.root, "new", "", 10, nil, "")
	require.NoError(t, err)
	require.Len(t, tree, 1)
	assert.Equal(t, secret, tree[0].Content,
		"and so does the anonymous thread view. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")

	assert.Nil(t, single.DeletionReason,
		"Delete also records no reason and no actor, so a moderation audit cannot tell why this "+
			"row is a tombstone. IF THIS FAILED (issue 2026-07-31-repo-minor-pins-batch.md) the defect is FIXED — delete this pin")
	assert.Nil(t, single.DeletedBy)
}

func TestCommentRepo_SoftDeleteWithReason(t *testing.T) {
	t.Parallel()

	t.Run("blanks every content column and records who and why", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "moderated", content: "the offending text"})
		facets := `[{"index":{"byteStart":0,"byteEnd":3}}]`
		embed := `{"$type":"social.coves.embed.images"}`
		labels := `{"values":[{"val":"nsfw"}]}`
		require.NoError(t, env.repo.Update(env.ctx, &comments.Comment{
			URI: uri, CID: "c", Content: "the offending text",
			ContentFacets: &facets, Embed: &embed, ContentLabels: &labels,
		}))

		moderator := "did:plc:cmtmod" + env.id
		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, uri, comments.DeletionReasonModerator, moderator))

		got, err := env.repo.GetByURI(env.ctx, uri)
		require.NoError(t, err)
		assert.Empty(t, got.Content, "the row survives so the thread keeps its shape, which makes this "+
			"blanking the only thing that actually removes the words")
		assert.Nil(t, got.ContentFacets, "facets carry the link targets and mentions of the removed text")
		assert.Nil(t, got.Embed, "an embed left behind is the removed image still being served")
		assert.Nil(t, got.ContentLabels)
		require.NotNil(t, got.DeletedAt)
		require.NotNil(t, got.DeletionReason)
		assert.Equal(t, comments.DeletionReasonModerator, *got.DeletionReason,
			"the reason is what tells a client to render 'removed by moderator' rather than "+
				"'deleted by author', and what a moderation audit reads")
		require.NotNil(t, got.DeletedBy)
		assert.Equal(t, moderator, *got.DeletedBy,
			"without the actor there is no accountability for a removal")
	})

	// Structure is the entire reason this is a soft delete. Children must keep
	// resolving through the deleted parent, and the parent must still be
	// fetchable by permalink so the placeholder has something to render.
	t.Run("preserves the thread underneath it", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		parent := env.seed(commentSpec{rkey: "parent"})
		child := env.seed(commentSpec{rkey: "child", parent: parent, createdAt: commentBaseTime.Add(time.Minute)})

		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, parent, comments.DeletionReasonAuthor, env.author))

		replies, _, err := env.repo.ListByParentWithHotRank(env.ctx, parent, "new", "", 10, nil, "")
		require.NoError(t, err)
		assert.Equal(t, []string{child}, commentURIs(replies),
			"a hard delete would take the child with it; the whole point of blanking instead of "+
				"deleting is that the reply stays reachable")

		placeholder, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "parent", "")
		require.NoError(t, err, "the permalink to a deleted comment must still resolve, or its "+
			"children have no node to hang under")
		assert.Empty(t, placeholder.Content)
		assert.NotNil(t, placeholder.DeletedAt)
	})

	t.Run("accepts only the two reasons the domain defines", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "reasons", content: "still here"})

		err := env.repo.SoftDeleteWithReason(env.ctx, uri, "spam", "did:plc:cmtsomeone")
		require.Error(t, err, "deletion_reason is a Postgres enum of exactly ('author','moderator'); "+
			"a third value is a deletion no client knows how to render")
		assert.ErrorContains(t, err, "invalid deletion reason")

		_, _, _, _, _, _, content, deletedAt := env.rawComment(uri)
		assert.Equal(t, "still here", content, "a rejected reason must not have blanked anything on the way out")
		assert.Nil(t, deletedAt)

		for _, reason := range []string{comments.DeletionReasonAuthor, comments.DeletionReasonModerator} {
			fresh := env.seed(commentSpec{rkey: "ok" + reason})
			assert.NoErrorf(t, env.repo.SoftDeleteWithReason(env.ctx, fresh, reason, env.author),
				"%q is a reason the lexicon defines", reason)
		}
	})

	// The first deletion is the one that counts. A moderator removal followed by
	// a replayed author delete must not rewrite the record to say the author did
	// it — that would erase the moderation trail.
	t.Run("a second deletion does not rewrite the first one", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "twice"})
		moderator := "did:plc:cmtmod2" + env.id

		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, uri, comments.DeletionReasonModerator, moderator))
		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, uri, comments.DeletionReasonAuthor, env.author),
			"a replayed delete must not be an error")

		got, err := env.repo.GetByURI(env.ctx, uri)
		require.NoError(t, err)
		require.NotNil(t, got.DeletionReason)
		assert.Equal(t, comments.DeletionReasonModerator, *got.DeletionReason,
			"the WHERE deleted_at IS NULL guard is what protects the audit trail: a later author "+
				"delete overwriting a moderator removal would launder the moderation record")
		require.NotNil(t, got.DeletedBy)
		assert.Equal(t, moderator, *got.DeletedBy)
	})

	// SoftDeleteWithReason reports nothing when it matches no row, by design:
	// the Tx form returns the count for callers that need to tell the difference
	// (the consumer uses it to decide whether to decrement reply counts).
	t.Run("a deletion for an unknown comment is silent", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)

		assert.NoError(t, env.repo.SoftDeleteWithReason(env.ctx,
			"at://"+env.author+"/social.coves.community.comment/phantom",
			comments.DeletionReasonModerator, "did:plc:cmtmod3"),
			"IF THIS FAILED, the non-Tx form learned to report that it matched no rows. That is an "+
				"improvement — assert the new error here rather than reverting. Callers that need the "+
				"count already use SoftDeleteWithReasonTx")
	})
}

func TestCommentRepo_SoftDeleteWithReasonTx(t *testing.T) {
	t.Parallel()

	// The count is the contract: the consumer uses it to decide whether this
	// delete was the one that took effect, and therefore whether to run the
	// bookkeeping that goes with it.
	t.Run("reports one row the first time and none the second", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "counted"})

		affected, err := env.impl.SoftDeleteWithReasonTx(env.ctx, nil, uri, comments.DeletionReasonAuthor, env.author)
		require.NoError(t, err)
		assert.EqualValues(t, 1, affected)

		affected, err = env.impl.SoftDeleteWithReasonTx(env.ctx, nil, uri, comments.DeletionReasonAuthor, env.author)
		require.NoError(t, err)
		assert.EqualValues(t, 0, affected,
			"a redelivered delete reporting 1 would make the consumer decrement the parent's reply "+
				"count a second time, and reply counts have no floor")

		affected, err = env.impl.SoftDeleteWithReasonTx(env.ctx, nil,
			"at://"+env.author+"/social.coves.community.comment/absent", comments.DeletionReasonAuthor, env.author)
		require.NoError(t, err)
		assert.EqualValues(t, 0, affected, "and a delete for a comment that was never indexed is not an error")
	})

	// The consumer runs the delete inside the same transaction as the reply-count
	// bookkeeping, so the blanking has to be genuinely transactional rather than
	// a separate connection that has already committed.
	t.Run("rolls back with its transaction", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "rolledback", content: "survives"})

		tx, err := env.db.BeginTx(env.ctx, nil)
		require.NoError(t, err)
		affected, err := env.impl.SoftDeleteWithReasonTx(env.ctx, tx, uri, comments.DeletionReasonAuthor, env.author)
		require.NoError(t, err)
		require.EqualValues(t, 1, affected)
		require.NoError(t, tx.Rollback())

		_, _, _, _, _, _, content, deletedAt := env.rawComment(uri)
		assert.Equal(t, "survives", content, "the delete escaped its transaction: a consumer whose "+
			"reply-count update failed would have blanked the comment anyway")
		assert.Nil(t, deletedAt)
	})

	t.Run("commits with its transaction", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "committed"})

		tx, err := env.db.BeginTx(env.ctx, nil)
		require.NoError(t, err)
		_, err = env.impl.SoftDeleteWithReasonTx(env.ctx, tx, uri, comments.DeletionReasonModerator, "did:plc:cmttxmod")
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		got, err := env.repo.GetByURI(env.ctx, uri)
		require.NoError(t, err)
		assert.Empty(t, got.Content)
		require.NotNil(t, got.DeletionReason)
		assert.Equal(t, comments.DeletionReasonModerator, *got.DeletionReason)
	})

	// The Tx form skips the Go-side reason check that its wrapper performs — it
	// is called by the Jetstream consumer with a constant. The enum is the
	// backstop, and this proves the backstop is real rather than assumed.
	t.Run("the database refuses a reason the wrapper would have caught", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "enumguard", content: "untouched"})

		_, err := env.impl.SoftDeleteWithReasonTx(env.ctx, nil, uri, "shadowban", "did:plc:cmtmod4")
		require.Error(t, err, "deletion_reason is an enum; the Tx form does no validation of its own, "+
			"so a caller other than the consumer could otherwise write a reason nothing can render")

		_, _, _, _, _, _, content, deletedAt := env.rawComment(uri)
		assert.Equal(t, "untouched", content)
		assert.Nil(t, deletedAt)
	})
}

func TestCommentRepo_GetByRootAndRkey(t *testing.T) {
	t.Parallel()

	t.Run("resolves a permalink within its thread and hydrates the author", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "permalink", content: "findable"})

		got, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "permalink", "")
		require.NoError(t, err)
		assert.Equal(t, uri, got.URI)
		assert.Equal(t, "findable", got.Content)
		assert.Equal(t, "cmt-"+env.id+".test", got.CommenterHandle,
			"the permalink view renders the author; leaving the handle empty would show a raw DID "+
				"to every reader of a linked comment")
	})

	// The users row may not exist yet: comment and user events arrive on the
	// same firehose in no particular order, and migration 016 removed the
	// foreign key precisely so the comment is indexed anyway.
	t.Run("falls back to the DID when the author is not indexed yet", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		stranger := "did:plc:cmtstranger" + env.id
		env.seed(commentSpec{rkey: "unindexed", author: stranger})

		got, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "unindexed", "")
		require.NoError(t, err)
		assert.Equal(t, stranger, got.CommenterHandle,
			"the LEFT JOIN plus COALESCE is what keeps an out-of-order firehose from dropping the "+
				"comment; an INNER JOIN here would return 'not found' for a comment that exists")
	})

	// rkey is unique per repository, not per thread: the same commenter can hold
	// one rkey and a different commenter another, and two different threads can
	// each contain a comment with rkey "abc".
	t.Run("is scoped to the thread asked for", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)

		// The decoy is indexed FIRST, so a query that lost its root scope would
		// return it rather than the one under test: the fallback ordering is
		// indexed_at ASC.
		otherRoot := fixtures.Post(t, env.db, env.community, env.author, "other thread", 0, time.Now().UTC())
		otherURI := "at://did:plc:cmtother" + env.id + "/social.coves.community.comment/shared"
		require.NoError(t, env.repo.Create(env.ctx, &comments.Comment{
			URI: otherURI, CID: "bafyother", RKey: "shared", CommenterDID: "did:plc:cmtother" + env.id,
			RootURI: otherRoot, RootCID: "bafyroot", ParentURI: otherRoot, ParentCID: "bafyroot",
			Content: "a different thread's comment", CreatedAt: commentBaseTime,
		}))
		mine := env.seed(commentSpec{rkey: "shared"})

		got, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "shared", "")
		require.NoError(t, err)
		assert.Equal(t, mine, got.URI, "an rkey lookup that ignored the root would serve one thread's "+
			"comment under another thread's permalink")
	})

	t.Run("reports an rkey nobody in the thread holds", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		env.seed(commentSpec{rkey: "present"})

		_, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "absent", "")
		require.ErrorIs(t, err, comments.ErrCommentNotFound,
			"sql.ErrNoRows leaking out would make the handler answer 500 to a stale permalink")
	})

	t.Run("still resolves a deleted comment", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		uri := env.seed(commentSpec{rkey: "removed"})
		require.NoError(t, env.repo.SoftDeleteWithReason(env.ctx, uri, comments.DeletionReasonAuthor, env.author))

		got, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "removed", "")
		require.NoError(t, err, "a deleted comment must keep resolving: getComments uses this to anchor "+
			"a subtree, and a 404 here would hide every surviving reply below it")
		assert.Empty(t, got.Content)
		assert.NotNil(t, got.DeletedAt)
	})

	// A blocked author's comment is reported as absent rather than as blocked:
	// telling the viewer that a comment exists but is hidden is itself a signal
	// about the blocked account.
	t.Run("hides a blocked author's comment from that viewer only", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		viewer := "did:plc:cmtblkviewer" + env.id
		blocked := "did:plc:cmtblkauthor" + env.id
		createTestUser(t, env.db, "cmtblkv-"+env.id+".test", viewer)
		createTestUser(t, env.db, "cmtblka-"+env.id+".test", blocked)
		env.seed(commentSpec{rkey: "blockable", author: blocked})

		before, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "blockable", viewer)
		require.NoError(t, err, "the fixture is visible before any block exists")
		require.NotNil(t, before)

		insertUserBlock(t, env.db, viewer, blocked)

		_, err = env.repo.GetByRootAndRkey(env.ctx, env.root, "blockable", viewer)
		assert.ErrorIs(t, err, comments.ErrCommentNotFound,
			"the permalink must honour the same block the thread listing does, or a blocked author "+
				"reaches the viewer through a direct link")

		stillThere, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "blockable", "")
		require.NoError(t, err, "one viewer's block must not remove the comment for everyone else")
		assert.Equal(t, blocked, stillThere.CommenterDID)
	})

	// rkeys are TIDs, so a collision inside one thread is astronomically
	// unlikely — but "unlikely" is not "impossible", and an unordered query
	// would serve a different comment on different page loads.
	t.Run("breaks an rkey collision deterministically", func(t *testing.T) {
		t.Parallel()
		env := commentEnvFor(t)
		first := "at://did:plc:cmtcolfirst" + env.id + "/social.coves.community.comment/collide"
		second := "at://did:plc:cmtcolsecond" + env.id + "/social.coves.community.comment/collide"

		for i, uri := range []string{first, second} {
			require.NoError(t, env.repo.Create(env.ctx, &comments.Comment{
				URI: uri, CID: "bafycol", RKey: "collide",
				CommenterDID: []string{"did:plc:cmtcolfirst" + env.id, "did:plc:cmtcolsecond" + env.id}[i],
				RootURI:      env.root, RootCID: "bafyroot", ParentURI: env.root, ParentCID: "bafyroot",
				Content: "collision " + uri, CreatedAt: commentBaseTime,
			}))
		}

		for i := 0; i < 3; i++ {
			got, err := env.repo.GetByRootAndRkey(env.ctx, env.root, "collide", "")
			require.NoError(t, err)
			assert.Equal(t, first, got.URI,
				"indexed_at ASC, id ASC makes the earliest-indexed comment win every time; without "+
					"the ORDER BY the permalink would flip between two different people's comments")
		}
	})
}
