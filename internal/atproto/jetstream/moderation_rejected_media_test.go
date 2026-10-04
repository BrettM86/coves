//go:build integration

package jetstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rejectedMediaCase seeds a subject, optionally removes it, then delivers an
// event whose content the consumer refuses to index. The author's repository
// still serves the images the refused record names, so a removed subject must
// block them for the author only (PRD Q-I6), while the refused content itself
// stays unindexed.
type rejectedMediaCase struct {
	h                *editMediaHarness
	revs             []string
	started          time.Time
	postRkey         string
	post, otherPost  moderation.StrongRef
	commentRkey      string
	subject          moderation.StrongRef
	imageA, imageB   string
	removedActionID  string
	subjectIsComment bool
}

func newRejectedMediaCase(t *testing.T, name string, comment, removed bool) *rejectedMediaCase {
	t.Helper()
	h := newEditMediaHarness(t)
	c := &rejectedMediaCase{
		h: h, revs: increasingTIDs(t, 4), started: time.Now().Add(-time.Minute),
		imageA: editMediaCID(name + " image A"), imageB: editMediaCID(name + " image B"),
		subjectIsComment: comment,
	}
	for _, owner := range []string{pv2Author, h.otherOwner} {
		h.pds.know(owner, c.imageA, c.imageB)
	}
	c.postRkey = testkit.TID()
	c.post = moderation.StrongRef{URI: pv2URI(pv2Author, c.postRkey), CID: editMediaCID(name + " post record")}
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", c.postRkey, c.revs[0],
		c.post.CID, c.started.UnixMicro(), postModerationRecord(c.imageA))))
	otherRkey := testkit.TID()
	c.otherPost = moderation.StrongRef{URI: pv2URI(pv2Author, otherRkey), CID: editMediaCID(name + " other post record")}
	require.NoError(t, h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "create", otherRkey, c.revs[0],
		c.otherPost.CID, c.started.UnixMicro(), postModerationRecord())))
	c.subject = c.post
	if comment {
		c.commentRkey = testkit.TID()
		c.subject = moderation.StrongRef{
			URI: "at://" + pv2Author + "/" + moderation.CommentCollection + "/" + c.commentRkey,
			CID: editMediaCID(name + " comment record"),
		}
		require.NoError(t, h.comments.HandleEvent(t.Context(), editMediaCommentEvent(c.post,
			"create", c.commentRkey, c.revs[0], c.subject.CID, c.started, c.imageA)))
	}
	if removed {
		c.removedActionID = h.remove(t, c.subject, "v0").Action.ID
	}
	return c
}

// postCommunityChange is a postv2 update that retargets the community, which
// the consumer ignores whole.
func (c *rejectedMediaCase) postCommunityChange(rev string, at time.Time) *JetstreamEvent {
	record := postModerationRecord(c.imageA, c.imageB)
	record["community"] = pv2Prefix + "community2"
	return pv2Event(pv2Author, "update", c.postRkey, rev, editMediaCID("retargeted post record "+rev), at.UnixMicro(), record)
}

// assertContentUnchanged proves the refused event indexed nothing.
func (c *rejectedMediaCase) assertContentUnchanged(t *testing.T) {
	t.Helper()
	table := "posts"
	if c.subjectIsComment {
		table = "comments"
	}
	var storedCID, storedEmbed string
	require.NoError(t, c.h.db.QueryRowContext(t.Context(),
		`SELECT cid, embed::text FROM `+table+` WHERE uri = $1`, c.subject.URI).Scan(&storedCID, &storedEmbed))
	assert.Equal(t, c.subject.CID, storedCID, "the refused event must not replace the indexed record")
	assert.NotContains(t, storedEmbed, c.imageB, "the refused event's image must not be indexed")
	if c.subjectIsComment {
		var parentURI string
		require.NoError(t, c.h.db.QueryRowContext(t.Context(),
			`SELECT parent_uri FROM comments WHERE uri = $1`, c.subject.URI).Scan(&parentURI))
		assert.Equal(t, c.post.URI, parentURI, "threading references must stay immutable")
	} else {
		var communityDID string
		require.NoError(t, c.h.db.QueryRowContext(t.Context(),
			`SELECT community_did FROM posts WHERE uri = $1`, c.subject.URI).Scan(&communityDID))
		assert.Equal(t, pv2Community, communityDID, "the community must stay immutable")
	}
}

func (c *rejectedMediaCase) assertImageBBlockedForAuthorOnly(t *testing.T) {
	t.Helper()
	assert.Equal(t, 1, countRows(t, c.h.db, `SELECT count(*) FROM moderation_media_blocks
		WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active`, c.removedActionID, pv2Author, c.imageB),
		"the refused event's new image needs an active author-owned block on the removal")
	assert.Zero(t, countRows(t, c.h.db, `SELECT count(*) FROM moderation_media_blocks
		WHERE blob_cid = $1 AND owner_did IS NULL AND active`, c.imageB),
		"an image added after removal must never get an every-owner block")
	assert.Equal(t, http.StatusNotFound, c.h.imageStatus(t, pv2Author, c.imageB), "the author's new image must not be served")
	assert.Equal(t, http.StatusOK, c.h.imageStatus(t, c.h.otherOwner, c.imageB), "another owner's copy must stay available")
}

func (c *rejectedMediaCase) assertImageBNotBlocked(t *testing.T) {
	t.Helper()
	assert.Zero(t, countRows(t, c.h.db, `SELECT count(*) FROM moderation_media_blocks WHERE blob_cid = $1`, c.imageB),
		"no block may be added for this event")
	assert.Equal(t, http.StatusOK, c.h.imageStatus(t, pv2Author, c.imageB), "the author's image must stay available")
}

func TestModerationRejectedEventBlocksIncomingImagesForAuthor(t *testing.T) {
	for _, variant := range []struct {
		name    string
		comment bool
		deliver func(t *testing.T, c *rejectedMediaCase)
	}{
		{
			name: "postv2 update that changes community",
			deliver: func(t *testing.T, c *rejectedMediaCase) {
				require.NoError(t, c.h.posts.HandleEvent(t.Context(), c.postCommunityChange(c.revs[1], c.started.Add(time.Second))),
					"an illegal retarget is ignored, not dead-lettered")
			},
		},
		{
			name: "comment update that changes threading references", comment: true,
			deliver: func(t *testing.T, c *rejectedMediaCase) {
				err := c.h.comments.HandleEvent(t.Context(), editMediaCommentEvent(c.otherPost, "update", c.commentRkey, c.revs[1],
					editMediaCID("rethreaded comment record"), c.started.Add(time.Second), c.imageA, c.imageB))
				require.ErrorIs(t, err, ErrPermanentEvent, "the threading change itself must still be rejected")
			},
		},
		{
			name: "same-rkey comment re-create with a changed parent", comment: true,
			deliver: func(t *testing.T, c *rejectedMediaCase) {
				require.NoError(t, c.h.comments.HandleEvent(t.Context(), editMediaCommentEvent(c.otherPost, "create", c.commentRkey, c.revs[1],
					editMediaCID("recreated comment record"), c.started.Add(time.Second), c.imageB)),
					"a re-create with a changed parent is kept as a duplicate")
			},
		},
	} {
		t.Run(variant.name+" on a removed subject", func(t *testing.T) {
			c := newRejectedMediaCase(t, variant.name, variant.comment, true)
			variant.deliver(t, c)
			c.assertContentUnchanged(t)
			c.assertImageBBlockedForAuthorOnly(t)
		})
		t.Run(variant.name+" on a subject that is not removed", func(t *testing.T) {
			c := newRejectedMediaCase(t, variant.name, variant.comment, false)
			variant.deliver(t, c)
			c.assertContentUnchanged(t)
			c.assertImageBNotBlocked(t)
		})
	}
}

// A refused event that is older than state already applied must not block its
// images: the newer state supersedes it.
func TestModerationStaleRejectedEventAddsNoBlocks(t *testing.T) {
	t.Run("postv2 community change older by rev", func(t *testing.T) {
		c := newRejectedMediaCase(t, "stale post retarget", false, true)
		require.NoError(t, c.h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "update", c.postRkey, c.revs[2],
			editMediaCID("newer post record"), c.started.Add(2*time.Second).UnixMicro(), postModerationRecord(c.imageA))))
		// A cross-feed copy carries a newer emission time, so only rev orders it.
		require.NoError(t, c.h.posts.HandleEvent(t.Context(), c.postCommunityChange(c.revs[1], c.started.Add(3*time.Second))))
		c.assertImageBNotBlocked(t)
	})
	t.Run("postv2 community change older by event time", func(t *testing.T) {
		c := newRejectedMediaCase(t, "late post retarget", false, true)
		require.NoError(t, c.h.posts.HandleEvent(t.Context(), pv2Event(pv2Author, "update", c.postRkey, c.revs[1],
			editMediaCID("newer post record"), c.started.Add(2*time.Second).UnixMicro(), postModerationRecord(c.imageA))))
		require.NoError(t, c.h.posts.HandleEvent(t.Context(), c.postCommunityChange(c.revs[2], c.started.Add(time.Second))))
		c.assertImageBNotBlocked(t)
	})
	t.Run("comment threading change older by rev", func(t *testing.T) {
		c := newRejectedMediaCase(t, "stale comment rethread", true, true)
		require.NoError(t, c.h.comments.HandleEvent(t.Context(), editMediaCommentEvent(c.post, "update", c.commentRkey, c.revs[2],
			editMediaCID("newer comment record"), c.started.Add(2*time.Second), c.imageA)))
		err := c.h.comments.HandleEvent(t.Context(), editMediaCommentEvent(c.otherPost, "update", c.commentRkey, c.revs[1],
			editMediaCID("rethreaded comment record"), c.started.Add(3*time.Second), c.imageA, c.imageB))
		require.ErrorIs(t, err, ErrPermanentEvent)
		c.assertImageBNotBlocked(t)
	})
}

// holdNewerEvent writes, in a transaction it leaves open until commit is
// called, what a newer same-record event writes: its rev gate claim, the
// first statement of every consumer write, and the row's indexed_at watermark.
func (c *rejectedMediaCase) holdNewerEvent(t *testing.T, rev string, at time.Time) (commit func()) {
	t.Helper()
	table := "posts"
	if c.subjectIsComment {
		table = "comments"
	}
	tx, err := c.h.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	won, err := tryAdvanceRecordRev(t.Context(), tx, c.subject.URI, rev)
	require.NoError(t, err)
	require.True(t, won, "the held event must be newer than the indexed state")
	_, err = tx.ExecContext(t.Context(), `UPDATE `+table+` SET indexed_at = $2 WHERE uri = $1`, c.subject.URI, at)
	require.NoError(t, err)
	return func() { require.NoError(t, tx.Commit()) }
}

// A refused event must decide that it is current in the transaction that
// inserts its blocks. A newer event that commits after the refused event's
// first read of the indexed state, but before its reconciliation, supersedes
// it, and the refused event must then add no blocks.
func TestModerationRejectedEventRechecksFreshnessUnderLock(t *testing.T) {
	type deliver func(ctx context.Context, c *rejectedMediaCase, rev string, at time.Time) error
	retarget := func(ctx context.Context, c *rejectedMediaCase, rev string, at time.Time) error {
		return c.h.posts.HandleEvent(ctx, c.postCommunityChange(rev, at))
	}
	rethread := func(ctx context.Context, c *rejectedMediaCase, rev string, at time.Time) error {
		err := c.h.comments.HandleEvent(ctx, editMediaCommentEvent(c.otherPost, "update", c.commentRkey, rev,
			editMediaCID("rethreaded comment record"), at, c.imageA, c.imageB))
		if errors.Is(err, ErrPermanentEvent) {
			return nil
		}
		return fmt.Errorf("the threading change must still be rejected, got %v", err)
	}
	for _, path := range []struct {
		name    string
		comment bool
		deliver deliver
	}{
		{name: "postv2 community change", deliver: retarget},
		{name: "comment threading change", comment: true, deliver: rethread},
	} {
		for _, newer := range []struct {
			name string
			// newerRev and newerAt are the held event's; refusedRev and
			// refusedAt the refused event's, as indexes into revs and seconds
			// after the case started.
			newerRev, refusedRev int
			newerAt, refusedAt   time.Duration
		}{
			// A cross-feed copy carries a newer emission time, so only rev orders it.
			{name: "newer by rev", newerRev: 2, refusedRev: 1, newerAt: 2 * time.Second, refusedAt: 3 * time.Second},
			{name: "newer by event time", newerRev: 1, refusedRev: 2, newerAt: 2 * time.Second, refusedAt: time.Second},
		} {
			t.Run(path.name+" superseded "+newer.name, func(t *testing.T) {
				c := newRejectedMediaCase(t, "fresh "+path.name+" "+newer.name, path.comment, true)
				// The pool is three connections: the held event, the refused
				// event and this one.
				observer, err := c.h.db.Conn(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { _ = observer.Close() })
				commit := c.holdNewerEvent(t, c.revs[newer.newerRev], c.started.Add(newer.newerAt))

				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				refusedDone := make(chan error, 1)
				go func() {
					refusedDone <- path.deliver(ctx, c, c.revs[newer.refusedRev], c.started.Add(newer.refusedAt))
				}()
				testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
					if len(refusedDone) > 0 {
						return true, nil
					}
					var waiting int
					err := observer.QueryRowContext(t.Context(), `
						SELECT count(*) FROM pg_stat_activity
						WHERE datname = current_database() AND pid <> pg_backend_pid()
						  AND wait_event_type = 'Lock' AND wait_event <> 'advisory'
					`).Scan(&waiting)
					return waiting > 0, err
				}, testkit.WithDescription("refused event waiting on a lock or finished"))
				assert.Empty(t, refusedDone, "the refused event must not reconcile while a newer event is uncommitted")

				commit()
				require.NoError(t, <-refusedDone)
				c.assertContentUnchanged(t)
				c.assertImageBNotBlocked(t)
			})
		}
	}
}
