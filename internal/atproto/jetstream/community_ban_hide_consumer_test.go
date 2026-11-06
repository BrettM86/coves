//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community ban (social.coves.moderation.ban in the community's repo) also
// covers what the banned account writes straight to its own PDS. A comment or
// vote by A whose subject is in community C is recorded hidden when a ban(C, A)
// was indexed by Coves before the record, was not lifted (deleted or
// overwritten) before the record, and had not expired on the Coves clock when
// the record was indexed. The record's createdAt and the event's time_us never
// matter. The decision is made once, at index time; a comment indexed before
// its thread root is classified when the root arrives, as of the comment's own
// index order and index time.

const (
	bhAuthor = "did:plc:bhauthor" // A, the banned account
	bhOther  = "did:plc:bhother"  // B, never banned

	bhRootRkey         = "bhroot"
	bhRootCID          = "bafyreibhroot"
	bhOtherRootRkey    = "bhotherroot"
	bhOtherRootCID     = "bafyreibhotherroot"
	bhCommentRkey      = "bhcomment"
	bhCommentCID       = "bafyreibhcomment"
	bhReplyRkey        = "bhreply"
	bhReplyCID         = "bafyreibhreply"
	bhOtherCommentRkey = "bhothercomment"
	bhOtherCommentCID  = "bafyreibhothercomment"
	bhOtherVoteRkey    = "bhothervote"
	bhAuthorVoteRkey   = "bhauthorvote"
	// bhAuthorReplacementVoteRkey is A's second vote on the same subject, the
	// new-rkey record that replaces the vote at bhAuthorVoteRkey.
	bhAuthorReplacementVoteRkey = "bhauthorvote2"
	// bhOtherReplacementVoteRkey is B's second vote on A's comment, the
	// new-rkey record that replaces the vote at bhOtherVoteRkey.
	bhOtherReplacementVoteRkey = "bhothervote2"

	// Ban expiries relative to the comment/vote clock bhClockStart. Both are
	// in the real past, so a consumer reading the host clock instead of the
	// injected one sees every expiring ban as expired.
	bhExpiresBeforeClock = "2026-02-01T00:00:00Z"
	bhExpiresAfterClock  = "2026-03-02T00:00:00Z"
)

var (
	// bhClockStart is the Coves clock the comment and vote consumers start at.
	bhClockStart = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	// bhClockAdvanced is past every expiry the scenarios use.
	bhClockAdvanced = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	bhHidden    = sql.NullString{String: "community_ban", Valid: true}
	bhNotHidden = sql.NullString{}
)

// bhStep names one event delivered through a real HandleEvent call.
type bhStep int

const (
	bhStepRoot                     bhStep = iota // root post P in C
	bhStepOtherRoot                              // root post P2 in C2
	bhStepBan                                    // ban(C, A) create, expiresAt = scenario.banExpiresAt
	bhStepBanDelete                              // C deletes the ban record
	bhStepBanOverwrite                           // ban(C, A) update at the same rkey, expiresAt = scenario.overwriteExpiresAt
	bhStepComment                                // A's top-level comment on P
	bhStepCommentOtherRoot                       // A's top-level comment on P2 (same URI as bhStepComment)
	bhStepForgedRoot                             // A's comment whose parent is P but whose declared root is P2
	bhStepOtherComment                           // B's top-level comment on P
	bhStepReply                                  // A's reply to B's comment
	bhStepOtherUpvote                            // B upvotes A's comment
	bhStepAuthorUpvote                           // A upvotes B's comment
	bhStepAcceptRoot                             // C accepts the unindexed P; the direct fetch indexes it
	bhStepAdvanceClock                           // the comment and vote clocks move to bhClockAdvanced
	bhStepCommentReplay                          // the earlier bhStepComment event redelivered unchanged (same rev)
	bhStepCommentReplayNewerRev                  // A's comment create, same content, at a newer rev
	bhStepCommentDelete                          // A deletes the comment at bhCommentRkey
	bhStepCommentRecreate                        // A re-creates bhCommentRkey on P with new content, at a newer rev
	bhStepOtherUpvoteDelete                      // B deletes the upvote at bhOtherVoteRkey
	bhStepOtherReplacementDownvote               // B downvotes A's comment under a NEW rkey (bhOtherReplacementVoteRkey)
)

// bhRecord names a record whose hide_reason (and, for comments, counts) a row
// asserts.
type bhRecord int

const (
	bhRecAuthorComment bhRecord = iota
	bhRecAuthorReply
	bhRecOtherComment
	bhRecOtherVote
	bhRecAuthorVote
	bhRecAuthorReplacementVote
)

func bhCommentURI(did, rkey string) string {
	return "at://" + did + "/" + CommentCollection + "/" + rkey
}
func bhVoteURI(did, rkey string) string { return "at://" + did + "/social.coves.feed.vote/" + rkey }

func (r bhRecord) tableAndURI() (table, uri string) {
	switch r {
	case bhRecAuthorComment:
		return "comments", bhCommentURI(bhAuthor, bhCommentRkey)
	case bhRecAuthorReply:
		return "comments", bhCommentURI(bhAuthor, bhReplyRkey)
	case bhRecOtherComment:
		return "comments", bhCommentURI(bhOther, bhOtherCommentRkey)
	case bhRecOtherVote:
		return "votes", bhVoteURI(bhOther, bhOtherVoteRkey)
	case bhRecAuthorVote:
		return "votes", bhVoteURI(bhAuthor, bhAuthorVoteRkey)
	case bhRecAuthorReplacementVote:
		return "votes", bhVoteURI(bhAuthor, bhAuthorReplacementVoteRkey)
	}
	panic(fmt.Sprintf("unknown bhRecord %d", r))
}

// bhScenario is an ordered event sequence plus the record fields that vary.
type bhScenario struct {
	steps              []bhStep
	banExpiresAt       string // "" = no expiry
	overwriteExpiresAt string // "" = no expiry
	commentCreatedAt   string // "" = 2026-03-01T02:00:00Z
	// laterTimeUS, when set, is the step whose event carries a Jetstream time_us
	// later than every other step's, whatever its delivery position.
	laterTimeUS *bhStep
}

// bhClock is the injected Coves clock shared by the comment and vote consumers.
type bhClock struct{ now time.Time }

func (c *bhClock) Now() time.Time { return c.now }

func newBanHideFixture(t *testing.T, clock *bhClock) ccrFixture {
	t.Helper()
	f := newCCRFixture(t,
		withCCRCommunityBans(),
		withCCRCommentOptions(WithCommentClock(clock.Now)),
		withCCRVoteOptions(WithVoteClock(clock.Now)))
	insertBridgedUser(t, f.db, bhAuthor, "bhauthor.test")
	insertBridgedUser(t, f.db, bhOther, "bhother.test")
	return f
}

func bhCommentEvent(did, rkey, cid, rev string, timeUS int64, createdAt, rootURI, rootCID, parentURI, parentCID string) *JetstreamEvent {
	record := revCommentRecord("a comment by "+did, rootURI, rootCID, parentURI, parentCID)
	if createdAt != "" {
		record["createdAt"] = createdAt
	}
	return revCommitEvent(did, CommentCollection, "create", rkey, rev, cid, timeUS, record)
}

func bhUpvoteEvent(voter, rkey, rev string, timeUS int64, subjectURI, subjectCID string) *JetstreamEvent {
	return bhVoteEvent(voter, rkey, rev, timeUS, "up", subjectURI, subjectCID)
}

func bhVoteEvent(voter, rkey, rev string, timeUS int64, direction, subjectURI, subjectCID string) *JetstreamEvent {
	return revCommitEvent(voter, "social.coves.feed.vote", "create", rkey, rev, "bafyreivote"+rkey, timeUS,
		map[string]interface{}{
			"$type":     "social.coves.feed.vote",
			"subject":   map[string]interface{}{"uri": subjectURI, "cid": subjectCID},
			"direction": direction,
			"createdAt": "2026-03-01T03:00:00Z",
		})
}

// runBanHideScenario delivers every step through the real consumers, in order.
func runBanHideScenario(t *testing.T, f ccrFixture, clock *bhClock, s bhScenario) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, len(s.steps))

	rootURI := accPostURI(bhRootRkey)
	otherRootURI := accPostURI(bhOtherRootRkey)
	authorCommentURI := bhCommentURI(bhAuthor, bhCommentRkey)
	otherCommentURI := bhCommentURI(bhOther, bhOtherCommentRkey)

	ban := func(op, rev, expiresAt, cid string, timeUS int64) *JetstreamEvent {
		return banCommitEvent(banStep{
			repo: accCommunity, op: op, subject: bhAuthor, rev: rev,
			cid: cid, reason: "spam", expiresAt: expiresAt,
		}, timeUS)
	}

	var commentEvent *JetstreamEvent
	for i, step := range s.steps {
		rev := revs[i]
		timeUS := base + int64(i+1)*1_000_000
		if s.laterTimeUS != nil && step == *s.laterTimeUS {
			timeUS = base + 1_000_000_000
		}
		switch step {
		case bhStepRoot:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
				accAuthor, "create", bhRootRkey, rev, bhRootCID, timeUS,
				pv2Record(accCommunity, "thread root", "root body"))), "root post in C")
		case bhStepOtherRoot:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
				accAuthor, "create", bhOtherRootRkey, rev, bhOtherRootCID, timeUS,
				pv2Record(ccrOtherCommunity, "other root", "other body"))), "root post in C2")
		case bhStepBan:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx,
				ban("create", rev, s.banExpiresAt, "bafyreibhban1", timeUS)), "ban(C, A)")
		case bhStepBanDelete:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx,
				ban("delete", rev, "", "", timeUS)), "ban(C, A) delete")
		case bhStepBanOverwrite:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx,
				ban("update", rev, s.overwriteExpiresAt, "bafyreibhban2", timeUS)), "ban(C, A) overwrite")
		case bhStepComment:
			commentEvent = bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID, rev, timeUS, s.commentCreatedAt,
				rootURI, bhRootCID, rootURI, bhRootCID)
			require.NoError(t, f.comments.HandleEvent(ctx, commentEvent), "A's comment on P")
		case bhStepCommentOtherRoot:
			require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID, rev, timeUS,
				s.commentCreatedAt, otherRootURI, bhOtherRootCID, otherRootURI, bhOtherRootCID)), "A's comment on P2")
		case bhStepForgedRoot:
			require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID, rev, timeUS,
				s.commentCreatedAt, otherRootURI, bhOtherRootCID, rootURI, bhRootCID)), "A's comment under P naming P2 as root")
		case bhStepOtherComment:
			require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, bhOtherCommentRkey, bhOtherCommentCID, rev, timeUS,
				"", rootURI, bhRootCID, rootURI, bhRootCID)), "B's comment on P")
		case bhStepReply:
			require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhReplyRkey, bhReplyCID, rev, timeUS,
				s.commentCreatedAt, rootURI, bhRootCID, otherCommentURI, bhOtherCommentCID)), "A's reply to B's comment")
		case bhStepOtherUpvote:
			require.NoError(t, f.votes.HandleEvent(ctx, bhUpvoteEvent(bhOther, bhOtherVoteRkey, rev, timeUS,
				authorCommentURI, bhCommentCID)), "B's upvote on A's comment")
		case bhStepAuthorUpvote:
			require.NoError(t, f.votes.HandleEvent(ctx, bhUpvoteEvent(bhAuthor, bhAuthorVoteRkey, rev, timeUS,
				otherCommentURI, bhOtherCommentCID)), "A's upvote on B's comment")
		case bhStepOtherUpvoteDelete:
			require.NoError(t, f.votes.HandleEvent(ctx, revCommitEvent(bhOther, "social.coves.feed.vote",
				"delete", bhOtherVoteRkey, rev, "", timeUS, nil)), "B deletes the upvote")
		case bhStepOtherReplacementDownvote:
			require.NoError(t, f.votes.HandleEvent(ctx, bhVoteEvent(bhOther, bhOtherReplacementVoteRkey, rev, timeUS,
				"down", authorCommentURI, bhCommentCID)), "B's replacement downvote under a new rkey")
		case bhStepAcceptRoot:
			f.rootFetcher.served = &FetchedPost{
				URI: rootURI, CID: bhRootCID,
				Record: pv2Record(accCommunity, "thread root", "root body"),
			}
			require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(
				accCommunity, rootURI, bhRootCID, rev, timeUS)), "acceptance of the unindexed root from C")
		case bhStepAdvanceClock:
			clock.now = bhClockAdvanced
		case bhStepCommentReplay:
			require.NotNil(t, commentEvent, "a replay needs an earlier bhStepComment")
			require.NoError(t, f.comments.HandleEvent(ctx, commentEvent), "A's comment redelivered")
		case bhStepCommentReplayNewerRev:
			require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID, rev, timeUS,
				s.commentCreatedAt, rootURI, bhRootCID, rootURI, bhRootCID)), "A's comment at a newer rev")
		case bhStepCommentDelete:
			require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(bhAuthor, CommentCollection, "delete", bhCommentRkey,
				rev, "", timeUS, nil)), "A deletes the comment")
		case bhStepCommentRecreate:
			record := revCommentRecord("new words written after the ban", rootURI, bhRootCID, rootURI, bhRootCID)
			require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(bhAuthor, CommentCollection, "create", bhCommentRkey,
				rev, "bafyreibhcommentrecreated", timeUS, record)), "A re-creates the comment")
		default:
			t.Fatalf("unknown step %d", step)
		}
	}
}

func readHideReason(t *testing.T, db *sql.DB, rec bhRecord) sql.NullString {
	t.Helper()
	table, uri := rec.tableAndURI()
	return readStoredHideReason(t, db, table, uri)
}

// readStoredHideReason reads the hide_reason of the comment or vote at uri;
// table is "comments" or "votes".
func readStoredHideReason(t *testing.T, db *sql.DB, table, uri string) sql.NullString {
	t.Helper()
	var reason sql.NullString
	require.NoError(t, db.QueryRow(`SELECT hide_reason FROM `+table+` WHERE uri = $1`, uri).Scan(&reason),
		"reading hide_reason of %s", uri)
	return reason
}

func assertBanHides(t *testing.T, db *sql.DB, want map[bhRecord]sql.NullString, counts map[bhRecord]subjectCounts) {
	t.Helper()
	for rec, wantReason := range want {
		_, uri := rec.tableAndURI()
		assert.Equal(t, wantReason, readHideReason(t, db, rec), "hide_reason of %s", uri)
	}
	for rec, wantCounts := range counts {
		_, uri := rec.tableAndURI()
		assert.Equal(t, wantCounts, readSubjectCounts(t, db,
			`SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`, uri),
			"counts of %s", uri)
	}
}

func bhStepPtr(s bhStep) *bhStep { return &s }

// B1: the root P in C is indexed; the comment is classified when it is indexed.
func TestCommunityBanHide_CommentClassifiedAtIndexTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bhScenario
		want map[bhRecord]sql.NullString
	}{
		{
			name:       "a ban then comment",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepBan, bhStepComment}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name:       "b comment then ban",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepComment, bhStepBan}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name:       "c ban in C, comment on a post in C2",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepOtherRoot, bhStepBan, bhStepCommentOtherRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name: "d ban expired before the comment clock",
			bhScenario: bhScenario{
				steps:        []bhStep{bhStepRoot, bhStepBan, bhStepComment},
				banExpiresAt: bhExpiresBeforeClock,
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name: "e comment createdAt predates the ban",
			bhScenario: bhScenario{
				steps:            []bhStep{bhStepRoot, bhStepBan, bhStepComment},
				commentCreatedAt: "2020-01-01T00:00:00Z",
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name:       "f ban deleted before the comment",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepBan, bhStepBanDelete, bhStepComment}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name:       "g reply to B's comment reaches C through the parent chain",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepOtherComment, bhStepBan, bhStepReply}},
			want: map[bhRecord]sql.NullString{
				bhRecAuthorReply:  bhHidden,
				bhRecOtherComment: bhNotHidden,
			},
		},
		{
			name: "h ban expires after the comment clock",
			bhScenario: bhScenario{
				steps:        []bhStep{bhStepRoot, bhStepBan, bhStepComment},
				banExpiresAt: bhExpiresAfterClock,
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name: "i ban delivered first with a later time_us",
			bhScenario: bhScenario{
				steps:       []bhStep{bhStepRoot, bhStepBan, bhStepComment},
				laterTimeUS: bhStepPtr(bhStepBan),
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name: "j comment delivered first with a later time_us",
			bhScenario: bhScenario{
				steps:       []bhStep{bhStepRoot, bhStepComment, bhStepBan},
				laterTimeUS: bhStepPtr(bhStepComment),
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name:       "k parent chain reaches C, declared root names C2",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepOtherRoot, bhStepBan, bhStepForgedRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name: "l pre-ban comment deleted and re-created at the same rkey after the ban",
			bhScenario: bhScenario{steps: []bhStep{
				bhStepRoot, bhStepComment, bhStepBan, bhStepCommentDelete, bhStepCommentRecreate,
			}},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := &bhClock{now: bhClockStart}
			f := newBanHideFixture(t, clock)
			runBanHideScenario(t, f, clock, tt.bhScenario)
			assertBanHides(t, f.db, tt.want, nil)
		})
	}
}

// B2: the comment (and votes on it) arrive before the thread root, so the
// community is unknown at index time; classification happens when the root is
// indexed, as of each record's own index order and index time.
func TestCommunityBanHide_PendingClassifiedOnRootArrival(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bhScenario
		want   map[bhRecord]sql.NullString
		counts map[bhRecord]subjectCounts
	}{
		{
			name:       "a ban, comment, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepComment, bhStepRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name:       "b ban, comment, ban deleted, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepComment, bhStepBanDelete, bhStepRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name:       "c ban, comment, ban overwritten by a later ban, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepComment, bhStepBanOverwrite, bhStepRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name: "c2 ban, comment, ban overwritten by one already expired at the comment clock, root",
			bhScenario: bhScenario{
				steps:              []bhStep{bhStepBan, bhStepComment, bhStepBanOverwrite, bhStepRoot},
				banExpiresAt:       bhExpiresAfterClock,
				overwriteExpiresAt: bhExpiresBeforeClock,
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name: "d ban expiring after the comment clock, comment, clocks pass expiry, root",
			bhScenario: bhScenario{
				steps:        []bhStep{bhStepBan, bhStepComment, bhStepAdvanceClock, bhStepRoot},
				banExpiresAt: bhExpiresAfterClock,
			},
			want: map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name:       "e comment, ban, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepComment, bhStepBan, bhStepRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name:       "f ban, ban deleted, comment, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepBanDelete, bhStepComment, bhStepRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
		{
			name:       "g ban, comment, B's upvote on it, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepComment, bhStepOtherUpvote, bhStepRoot}},
			want: map[bhRecord]sql.NullString{
				bhRecAuthorComment: bhHidden,
				bhRecOtherVote:     bhNotHidden,
			},
			counts: map[bhRecord]subjectCounts{bhRecAuthorComment: {Upvotes: 1, Downvotes: 0, Score: 1}},
		},
		{
			name:       "g3 comment, B's upvote on it, B deletes the upvote, root",
			bhScenario: bhScenario{steps: []bhStep{bhStepComment, bhStepOtherUpvote, bhStepOtherUpvoteDelete, bhStepRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
			counts:     map[bhRecord]subjectCounts{bhRecAuthorComment: {Upvotes: 0, Downvotes: 0, Score: 0}},
		},
		{
			name: "g4 comment, B's upvote on it, B's replacement downvote, root",
			bhScenario: bhScenario{steps: []bhStep{
				bhStepComment, bhStepOtherUpvote, bhStepOtherReplacementDownvote, bhStepRoot,
			}},
			want:   map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
			counts: map[bhRecord]subjectCounts{bhRecAuthorComment: {Upvotes: 0, Downvotes: 1, Score: -1}},
		},
		{
			name: "g2 B's comment, ban, A's upvote, ban deleted, root",
			bhScenario: bhScenario{steps: []bhStep{
				bhStepOtherComment, bhStepBan, bhStepAuthorUpvote, bhStepBanDelete, bhStepRoot,
			}},
			want: map[bhRecord]sql.NullString{
				bhRecAuthorVote:   bhHidden,
				bhRecOtherComment: bhNotHidden,
			},
			counts: map[bhRecord]subjectCounts{bhRecOtherComment: {Upvotes: 0, Downvotes: 0, Score: 0}},
		},
		{
			name: "g2 ban, B's comment, ban deleted, A's upvote, root",
			bhScenario: bhScenario{steps: []bhStep{
				bhStepBan, bhStepOtherComment, bhStepBanDelete, bhStepAuthorUpvote, bhStepRoot,
			}},
			want: map[bhRecord]sql.NullString{
				bhRecAuthorVote:   bhNotHidden,
				bhRecOtherComment: bhNotHidden,
			},
			counts: map[bhRecord]subjectCounts{bhRecOtherComment: {Upvotes: 1, Downvotes: 0, Score: 1}},
		},
		{
			name:       "i ban, comment, root indexed by acceptance direct fetch",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepComment, bhStepAcceptRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhHidden},
		},
		{
			name:       "h ban in C, comment whose root is in C2, root in C2",
			bhScenario: bhScenario{steps: []bhStep{bhStepBan, bhStepCommentOtherRoot, bhStepOtherRoot}},
			want:       map[bhRecord]sql.NullString{bhRecAuthorComment: bhNotHidden},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := &bhClock{now: bhClockStart}
			f := newBanHideFixture(t, clock)
			runBanHideScenario(t, f, clock, tt.bhScenario)
			assertBanHides(t, f.db, tt.want, tt.counts)
		})
	}
}

// B3: the recorded value is permanent. Lifting the ban (delete or expiry) and
// redelivering the comment (same rev, or a newer rev with the same content)
// never changes it, in either direction.
func TestCommunityBanHide_RecordedValueIsPermanent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bhScenario
		want sql.NullString
	}{
		{
			name:       "hidden stays hidden after the ban is deleted",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepBan, bhStepComment, bhStepBanDelete}},
			want:       bhHidden,
		},
		{
			name: "hidden stays hidden after expiry and a same-rev redelivery",
			bhScenario: bhScenario{
				steps:        []bhStep{bhStepRoot, bhStepBan, bhStepComment, bhStepAdvanceClock, bhStepCommentReplay},
				banExpiresAt: bhExpiresAfterClock,
			},
			want: bhHidden,
		},
		{
			name: "hidden stays hidden after expiry and a newer-rev redelivery",
			bhScenario: bhScenario{
				steps:        []bhStep{bhStepRoot, bhStepBan, bhStepComment, bhStepAdvanceClock, bhStepCommentReplayNewerRev},
				banExpiresAt: bhExpiresAfterClock,
			},
			want: bhHidden,
		},
		{
			name: "hidden stays hidden after the ban is deleted and a newer-rev redelivery",
			bhScenario: bhScenario{steps: []bhStep{
				bhStepRoot, bhStepBan, bhStepComment, bhStepBanDelete, bhStepCommentReplayNewerRev,
			}},
			want: bhHidden,
		},
		{
			name:       "not hidden stays not hidden on a same-rev redelivery after a later ban",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepComment, bhStepBan, bhStepCommentReplay}},
			want:       bhNotHidden,
		},
		{
			name:       "not hidden stays not hidden on a newer-rev redelivery after a later ban",
			bhScenario: bhScenario{steps: []bhStep{bhStepRoot, bhStepComment, bhStepBan, bhStepCommentReplayNewerRev}},
			want:       bhNotHidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := &bhClock{now: bhClockStart}
			f := newBanHideFixture(t, clock)
			runBanHideScenario(t, f, clock, tt.bhScenario)
			assertBanHides(t, f.db, map[bhRecord]sql.NullString{bhRecAuthorComment: tt.want}, nil)
		})
	}
}

// A root indexed by the acceptance direct fetch whose pending ban check fails
// still has its pending comments classified when the acceptance is redelivered,
// with no event of the root's own: the redelivery finds the root indexed.
func TestCommunityBanHide_AcceptanceRedeliveryClassifiesAfterFailedCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &bhClock{now: bhClockStart}
	f := newBanHideFixture(t, clock)
	runBanHideScenario(t, f, clock, bhScenario{steps: []bhStep{bhStepBan, bhStepComment}})
	require.Equal(t, sql.NullString{String: hideReasonPendingBanCheck, Valid: true},
		readHideReason(t, f.db, bhRecAuthorComment), "fixture: the comment waits for its root")

	// The first classification attempt fails inside Postgres.
	_, err := f.db.ExecContext(ctx, `
		CREATE FUNCTION bh_fail_pending_check() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected pending ban check failure'; END $$;
		CREATE TRIGGER bh_fail_pending_check BEFORE UPDATE ON comments FOR EACH ROW
		WHEN (OLD.hide_reason = 'pending_ban_check' AND NEW.hide_reason IS DISTINCT FROM OLD.hide_reason)
		EXECUTE FUNCTION bh_fail_pending_check();`)
	require.NoError(t, err)

	rootURI := accPostURI(bhRootRkey)
	f.rootFetcher.served = &FetchedPost{
		URI: rootURI, CID: bhRootCID,
		Record: pv2Record(accCommunity, "thread root", "root body"),
	}
	acceptance := acceptanceEvent(accCommunity, rootURI, bhRootCID, testkit.TID(), time.Now().UnixMicro())
	require.Error(t, f.posts.consumer.HandleEvent(ctx, acceptance), "the injected failure surfaces")
	require.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM posts WHERE uri = $1`, rootURI),
		"fixture: the direct fetch indexed the root before the check failed")
	require.Equal(t, sql.NullString{String: hideReasonPendingBanCheck, Valid: true},
		readHideReason(t, f.db, bhRecAuthorComment), "fixture: the failed check left the comment pending")

	_, err = f.db.ExecContext(ctx, `DROP TRIGGER bh_fail_pending_check ON comments`)
	require.NoError(t, err)
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptance), "the acceptance redelivered")
	assert.Equal(t, bhHidden, readHideReason(t, f.db, bhRecAuthorComment),
		"the redelivery classifies the comment against the ban in force at its index order")
}

// A ban (or unban) of A and A's comment or vote indexed concurrently commit in
// the order of their Coves index orders, so the recorded hide agrees with the
// ban history at the record's stored order. The ban transaction is held open
// on a row lock after it starts, A's record is delivered while it waits, and
// the row lock is then released.
func TestCommunityBanHide_ConcurrentBanAndRecordAgreeOnIndexOrder(t *testing.T) {
	t.Parallel()

	rootURI := accPostURI(bhRootRkey)
	authorComment := func(f ccrFixture, rev string) error {
		return f.comments.HandleEvent(context.Background(), bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID,
			rev, time.Now().UnixMicro(), "", rootURI, bhRootCID, rootURI, bhRootCID))
	}
	authorVote := func(f ccrFixture, rev string) error {
		return f.votes.HandleEvent(context.Background(), bhUpvoteEvent(bhAuthor, bhAuthorVoteRkey, rev,
			time.Now().UnixMicro(), bhCommentURI(bhOther, bhOtherCommentRkey), bhOtherCommentCID))
	}
	for _, tt := range []struct {
		name    string
		setup   bhScenario
		banOp   string
		record  bhRecord
		deliver func(f ccrFixture, rev string) error
		hidden  sql.NullString
	}{
		{
			// The earlier ban has expired at the comment clock, so only the
			// overwrite can hide the comment.
			name:    "ban overwrite before the comment",
			setup:   bhScenario{steps: []bhStep{bhStepRoot, bhStepBan}, banExpiresAt: bhExpiresBeforeClock},
			banOp:   "update",
			record:  bhRecAuthorComment,
			deliver: authorComment,
			hidden:  bhHidden,
		},
		{
			name:    "unban before the comment",
			setup:   bhScenario{steps: []bhStep{bhStepRoot, bhStepBan}},
			banOp:   "delete",
			record:  bhRecAuthorComment,
			deliver: authorComment,
			hidden:  bhNotHidden,
		},
		{
			name: "ban overwrite before the vote",
			setup: bhScenario{
				steps:        []bhStep{bhStepRoot, bhStepOtherComment, bhStepBan},
				banExpiresAt: bhExpiresBeforeClock,
			},
			banOp:   "update",
			record:  bhRecAuthorVote,
			deliver: authorVote,
			hidden:  bhHidden,
		},
		{
			name:    "unban before the vote",
			setup:   bhScenario{steps: []bhStep{bhStepRoot, bhStepOtherComment, bhStepBan}},
			banOp:   "delete",
			record:  bhRecAuthorVote,
			deliver: authorVote,
			hidden:  bhNotHidden,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			clock := &bhClock{now: bhClockStart}
			f := newBanHideFixture(t, clock)
			// The held row lock, the ban transaction and the comment transaction
			// each hold a connection while the consumers also read outside their
			// transactions; testkit budgets two pools of three per test.
			f.db.SetMaxOpenConns(6)
			runBanHideScenario(t, f, clock, tt.setup)

			holder, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			require.NoError(t, err)
			t.Cleanup(func() { _ = holder.Rollback() })
			_, err = holder.ExecContext(ctx, `SELECT 1 FROM community_ban_history
				WHERE community_did = $1 AND subject_did = $2 AND lifted_order IS NULL FOR UPDATE`, accCommunity, bhAuthor)
			require.NoError(t, err)

			banDone := make(chan error, 1)
			banRev := testkit.TID()
			go func() {
				banDone <- f.posts.consumer.HandleEvent(ctx, banCommitEvent(banStep{
					repo: accCommunity, op: tt.banOp, subject: bhAuthor, rev: banRev,
					cid: "bafyreibhban2", reason: "spam",
				}, time.Now().UnixMicro()))
			}()
			waitForLockWaitOrDone(t, holder, banDone, false, "the ban waits on the held history row")
			require.Empty(t, banDone, "fixture: the ban must be held open, not finished")

			recordDone := make(chan error, 1)
			recordRev := testkit.TID()
			go func() { recordDone <- tt.deliver(f, recordRev) }()
			waitForLockWaitOrDone(t, holder, recordDone, true, "A's record waits on the ban subject lock or finished")

			require.NoError(t, holder.Commit())
			require.NoError(t, <-banDone, "the ban event")
			require.NoError(t, <-recordDone, "A's record")

			table, uri := tt.record.tableAndURI()
			authorColumn := "r.commenter_did"
			if table == "votes" {
				authorColumn = "r.voter_did"
			}
			var agreesWithHistory bool
			require.NoError(t, f.db.QueryRowContext(ctx, `
				SELECT (r.hide_reason IS NOT NULL) = `+banInForceCondition("$2", authorColumn, "r.coves_index_order", "r.coves_index_time")+`
				FROM `+table+` r WHERE r.uri = $1`, uri, accCommunity).Scan(&agreesWithHistory))
			assert.True(t, agreesWithHistory, "the recorded hide agrees with the ban history at the record's index order")
			assert.Equal(t, tt.hidden, readHideReason(t, f.db, tt.record),
				"the ban event, started first, is ordered before the record")
		})
	}
}

// A parent comment by A and the re-creation of A's pending reply beneath it,
// racing, do not deadlock. Both wait on A's ban subject lock, held by the test
// (as a concurrent ban would): the parent first, then the reply's re-creation.
// Each takes that lock before touching a comment row, so the parent, released
// first, classifies the pending reply without waiting on a row the
// re-creation holds.
func TestCommunityBanHide_ParentArrivalRacingPendingReplyRecreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &bhClock{now: bhClockStart}
	f := newBanHideFixture(t, clock)
	// The held lock, the two comment transactions and the lock observer each
	// hold a connection; testkit budgets two pools of three per test.
	f.db.SetMaxOpenConns(6)
	runBanHideScenario(t, f, clock, bhScenario{steps: []bhStep{bhStepRoot}})

	rootURI := accPostURI(bhRootRkey)
	parentURI := bhCommentURI(bhAuthor, bhCommentRkey)
	revs := increasingTIDs(t, 4)
	base := time.Now().UnixMicro()
	reply := func(rev, cid string, timeUS int64) *JetstreamEvent {
		return bhCommentEvent(bhAuthor, bhReplyRkey, cid, rev, timeUS, "", rootURI, bhRootCID, parentURI, bhCommentCID)
	}
	require.NoError(t, f.comments.HandleEvent(ctx, reply(revs[0], bhReplyCID, base)), "A's reply before its parent")
	require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(bhAuthor, CommentCollection, "delete", bhReplyRkey,
		revs[1], "", base+1, nil)), "A deletes the reply")
	require.Equal(t, sql.NullString{String: hideReasonPendingBanCheck, Valid: true},
		readHideReason(t, f.db, bhRecAuthorReply), "fixture: the deleted reply waits for its parent")

	holder, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	require.NoError(t, lockBanSubjectTx(ctx, holder, bhAuthor))

	parentDone := make(chan error, 1)
	go func() {
		parentDone <- f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID, revs[2], base+2,
			"", rootURI, bhRootCID, rootURI, bhRootCID))
	}()
	waitForLockWaitOrDone(t, holder, parentDone, true, "A's parent comment waits on A's ban subject lock")
	require.Empty(t, parentDone, "fixture: the parent must be held, not finished")

	recreateDone := make(chan error, 1)
	go func() { recreateDone <- f.comments.HandleEvent(ctx, reply(revs[3], "bafyreibhreplyrecreated", base+3)) }()
	waitForLockWaitersOrDone(t, holder, recreateDone, true, 2, "the reply's re-creation also waits on A's ban subject lock")
	require.Empty(t, recreateDone, "fixture: the re-creation must be held, not finished")

	require.NoError(t, holder.Commit())
	require.NoError(t, <-parentDone, "A's parent comment")
	require.NoError(t, <-recreateDone, "A's re-created reply")
	assertBanHides(t, f.db, map[bhRecord]sql.NullString{
		bhRecAuthorComment: bhNotHidden,
		bhRecAuthorReply:   bhNotHidden,
	}, nil)
}

// bhHeldCommitReconciler holds a comment transaction open just before it
// commits: the first ReconcileTx call signals reached and waits for release.
type bhHeldCommitReconciler struct {
	reached     chan struct{}
	release     chan struct{}
	reachedOnce sync.Once
}

func (r *bhHeldCommitReconciler) ReconcileTx(ctx context.Context, _ *sql.Tx, _ string) ([]moderation.MediaBlock, error) {
	r.reachedOnce.Do(func() { close(r.reached) })
	select {
	case <-r.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *bhHeldCommitReconciler) ReconcileIncomingTx(context.Context, *sql.Tx, string, string, []string) ([]moderation.MediaBlock, error) {
	return nil, nil
}

func (r *bhHeldCommitReconciler) Purge(context.Context, []moderation.MediaBlock) {}

// B's comment, whose transaction found the root missing and recorded the
// comment pending, is still settled when the root arrives and is classified
// before that transaction commits: the comment transaction is held open just
// before its commit while the root post is indexed, then released.
func TestCommunityBanHide_RootArrivalDuringUncommittedPendingComment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &bhClock{now: bhClockStart}
	held := &bhHeldCommitReconciler{reached: make(chan struct{}), release: make(chan struct{})}
	f := newCCRFixture(t,
		withCCRCommunityBans(),
		withCCRCommentOptions(WithCommentClock(clock.Now), WithCommentMediaReconciler(held)),
		withCCRVoteOptions(WithVoteClock(clock.Now)))
	insertBridgedUser(t, f.db, bhAuthor, "bhauthor.test")
	insertBridgedUser(t, f.db, bhOther, "bhother.test")
	// The held comment transaction, the root's transactions and the lock
	// observer each hold a connection; testkit budgets two pools of three.
	f.db.SetMaxOpenConns(6)
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})

	rootURI := accPostURI(bhRootRkey)
	revs := increasingTIDs(t, 2)
	base := time.Now().UnixMicro()
	commentDone := make(chan error, 1)
	go func() {
		commentDone <- f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, bhOtherCommentRkey, bhOtherCommentCID, revs[0], base,
			"", rootURI, bhRootCID, rootURI, bhRootCID))
	}()
	select {
	case <-held.reached:
	case err := <-commentDone:
		t.Fatalf("fixture: B's comment finished before its commit was held: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: B's comment never reached its commit")
	}

	rootDone := make(chan error, 1)
	go func() {
		rootDone <- f.posts.consumer.HandleEvent(ctx, pv2Event(accAuthor, "create", bhRootRkey, revs[1], bhRootCID, base+1,
			pv2Record(accCommunity, "thread root", "root body")))
	}()
	waitForLockWaitOrDone(t, f.db, rootDone, true, "the root classification waits on the thread root lock or finished")

	close(held.release)
	released = true
	require.NoError(t, <-commentDone, "B's comment")
	require.NoError(t, <-rootDone, "the root post")
	assertBanHides(t, f.db, map[bhRecord]sql.NullString{bhRecOtherComment: bhNotHidden}, nil)
}
