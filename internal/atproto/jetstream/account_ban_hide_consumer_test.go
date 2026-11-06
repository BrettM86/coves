//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An instance account ban (Task 06) also covers what the banned account writes
// straight to its own PDS. A comment or vote by A that Coves indexes while an
// instance ban of A is in force is recorded hidden with reason account_ban, in
// every community; "while banned" is judged by Coves' index time only, never
// by the record's createdAt or the event's time_us. The value is permanent:
// after an unban, or after a temporary ban lapses at its expiresAt, only what
// was indexed before the ban (or after the lapse) is served and counted, and
// redelivering the during-ban events changes nothing. No community ban exists
// in these scenarios; the thread roots are in C (accCommunity), which never
// bans A.

var abhHidden = sql.NullString{String: "account_ban", Valid: true}

// abhKind is what A writes: a comment on a thread root, or an upvote on B's
// comment on a thread root.
type abhKind int

const (
	abhComment abhKind = iota
	abhVote
)

// abhStage is when Coves indexes A's record.
type abhStage int

const (
	abhBeforeBan abhStage = iota
	abhWhileBanned
	// abhWhileBannedRootAfterEnd is indexed while banned with its thread root
	// not yet indexed; the root is indexed only after the ban ended.
	abhWhileBannedRootAfterEnd
	abhAfterLapse
)

// abhEnd is how the ban ends.
type abhEnd int

const (
	abhEndUnban abhEnd = iota // a permanent ban, lifted by unbanAccount
	abhEndLapse               // a temporary ban whose expiresAt passes with no action
)

// abhRecord is one record A writes, with its own thread root (and, for a
// vote, its own subject), so no record replaces another.
type abhRecord struct {
	stage abhStage
	// wantPlaceholder: another viewer reads A's comment as a removed
	// placeholder; otherwise with its content.
	wantPlaceholder bool
	// wantUpvotes is the upvote tally of the vote's subject.
	wantUpvotes    int
	wantHideReason sql.NullString
}

func abhRootURI(i int) string    { return accPostURI(fmt.Sprintf("abhroot%d", i)) }
func abhRootCID(i int) string    { return fmt.Sprintf("bafyreiabhroot%d", i) }
func abhSubjectURI(i int) string { return bhCommentURI(bhOther, fmt.Sprintf("abhsubject%d", i)) }
func abhSubjectCID(i int) string { return fmt.Sprintf("bafyreiabhsubject%d", i) }
func abhCommentURI(i int) string { return bhCommentURI(bhAuthor, fmt.Sprintf("abhcomment%d", i)) }
func abhVoteURI(i int) string    { return bhVoteURI(bhAuthor, fmt.Sprintf("abhvote%d", i)) }
func abhCommentText(i int) string {
	return fmt.Sprintf("account ban hide comment %d by the banned account", i)
}

// abhCommentEvent is A's comment on root i with the given createdAt.
func abhCommentEvent(i int, rev string, timeUS int64, createdAt string) *JetstreamEvent {
	record := revCommentRecord(abhCommentText(i), abhRootURI(i), abhRootCID(i), abhRootURI(i), abhRootCID(i))
	record["createdAt"] = createdAt
	return revCommitEvent(bhAuthor, CommentCollection, "create", fmt.Sprintf("abhcomment%d", i), rev,
		fmt.Sprintf("bafyreiabhcomment%d", i), timeUS, record)
}

// abhUpvoteEvent is A's upvote on B's comment i with the given createdAt.
func abhUpvoteEvent(i int, rev string, timeUS int64, createdAt string) *JetstreamEvent {
	event := bhUpvoteEvent(bhAuthor, fmt.Sprintf("abhvote%d", i), rev, timeUS, abhSubjectURI(i), abhSubjectCID(i))
	event.Commit.Record["createdAt"] = createdAt
	return event
}

// abhFixture is the consumers on the Coves clock (the host clock, which agrees
// with the DB clock accountbanfixture.Lapse uses), the instance moderator, and
// the comment service other viewers read threads through.
type abhFixture struct {
	ccrFixture
	moderator *accountbanfixture.Moderator
	threads   comments.Service
}

func newAccountBanHideFixture(t *testing.T) abhFixture {
	t.Helper()
	f := newCCRFixture(t, withCCRCommunityBans(),
		withCCRCommentOptions(WithCommentAccountBanAuthority(fixtures.InstanceDID())),
		withCCRVoteOptions(WithVoteAccountBanAuthority(fixtures.InstanceDID())))
	insertBridgedUser(t, f.db, bhAuthor, "bhauthor.test")
	insertBridgedUser(t, f.db, bhOther, "bhother.test")
	return abhFixture{
		ccrFixture: f,
		moderator:  accountbanfixture.New(f.db),
		threads: comments.NewCommentServiceWithPDSFactory(
			postgres.NewCommentRepository(f.db, postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID())),
			postgres.NewUserRepository(f.db),
			postgres.NewPostRepository(f.db, postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID())),
			postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed(),
				postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID())), nil, nil,
			comments.WithInstanceDID(fixtures.InstanceDID())),
	}
}

// runAccountBanHideScenario indexes every record's root (except the late
// ones) and, for votes, B's subject comment; delivers the before-ban records;
// bans A; delivers the while-banned records with createdAt and time_us earlier
// than the ban; ends the ban; indexes the late roots; delivers the after-lapse
// records with createdAt inside the ban window; checks; redelivers every
// while-banned event unchanged; and checks again.
func runAccountBanHideScenario(t *testing.T, f abhFixture, kind abhKind, end abhEnd, records []abhRecord) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	root := func(i int) {
		require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(accAuthor, "create", fmt.Sprintf("abhroot%d", i),
			testkit.TID(), abhRootCID(i), now.UnixMicro(), pv2Record(accCommunity, "thread root", "root body"))),
			"thread root %d in C", i)
		require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, abhRootURI(i), abhRootCID(i),
			testkit.TID(), now.UnixMicro())), "C accepts thread root %d", i)
	}
	deliver := func(i int, event *JetstreamEvent) {
		if kind == abhComment {
			require.NoError(t, f.comments.HandleEvent(ctx, event), "A's comment %d", i)
			return
		}
		require.NoError(t, f.votes.HandleEvent(ctx, event), "A's upvote %d", i)
	}
	record := func(i int, timeUS int64, createdAt string) *JetstreamEvent {
		if kind == abhComment {
			return abhCommentEvent(i, testkit.TID(), timeUS, createdAt)
		}
		return abhUpvoteEvent(i, testkit.TID(), timeUS, createdAt)
	}

	for i, r := range records {
		if r.stage != abhWhileBannedRootAfterEnd {
			root(i)
		}
		if kind == abhVote {
			require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, fmt.Sprintf("abhsubject%d", i),
				abhSubjectCID(i), testkit.TID(), now.UnixMicro(), "", abhRootURI(i), abhRootCID(i), abhRootURI(i), abhRootCID(i))),
				"B's comment %d", i)
		}
	}
	for i, r := range records {
		if r.stage == abhBeforeBan {
			deliver(i, record(i, now.UnixMicro(), "2026-03-01T02:00:00Z"))
		}
	}

	banStart := time.Now()
	if end == abhEndLapse {
		expiresAt := banStart.Add(24 * time.Hour)
		f.moderator.Ban(t, bhAuthor, &expiresAt)
	} else {
		f.moderator.Ban(t, bhAuthor, nil)
	}
	var duringBan []*JetstreamEvent
	duringBanIndex := []int{}
	for i, r := range records {
		if r.stage == abhWhileBanned || r.stage == abhWhileBannedRootAfterEnd {
			// Written (createdAt) and emitted (time_us) before the ban; indexed after it.
			event := record(i, banStart.Add(-time.Hour).UnixMicro(), "2020-01-01T00:00:00Z")
			deliver(i, event)
			duringBan = append(duringBan, event)
			duringBanIndex = append(duringBanIndex, i)
		}
	}
	insideBanWindow := time.Now().UTC().Format(time.RFC3339Nano)

	if end == abhEndLapse {
		f.moderator.Lapse(t, bhAuthor)
	} else {
		f.moderator.Unban(t, bhAuthor)
	}
	for i, r := range records {
		if r.stage == abhWhileBannedRootAfterEnd {
			root(i)
		}
	}
	for i, r := range records {
		if r.stage == abhAfterLapse {
			deliver(i, record(i, time.Now().UnixMicro(), insideBanWindow))
		}
	}

	checkAccountBanHides(t, f, kind, records, "after the ban ended")
	for n, event := range duringBan {
		deliver(duringBanIndex[n], event)
	}
	checkAccountBanHides(t, f, kind, records, "after the during-ban events are redelivered")
}

// checkAccountBanHides reads each comment through the comment service as
// another viewer V (ccrUser) and each vote's subject tally at the DB.
func checkAccountBanHides(t *testing.T, f abhFixture, kind abhKind, records []abhRecord, when string) {
	t.Helper()
	viewer := ccrUser
	for i, r := range records {
		if kind == abhVote {
			assert.Equal(t, subjectCounts{Upvotes: r.wantUpvotes, Downvotes: 0, Score: r.wantUpvotes},
				readSubjectCounts(t, f.db, `SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`, abhSubjectURI(i)),
				"tally of B's comment %d (A's upvote stage %d) %s", i, r.stage, when)
			assert.Equal(t, r.wantHideReason, readStoredHideReason(t, f.db, "votes", abhVoteURI(i)),
				"hide_reason of A's upvote %d %s", i, when)
			continue
		}
		assert.Equal(t, r.wantHideReason, readStoredHideReason(t, f.db, "comments", abhCommentURI(i)),
			"hide_reason of A's comment %d %s", i, when)
		thread, err := f.threads.GetComments(context.Background(), &comments.GetCommentsRequest{
			PostURI: abhRootURI(i), ViewerDID: &viewer, Sort: "new", Depth: 10, Limit: 50,
		})
		require.NoError(t, err, "reading thread %d as V %s", i, when)
		var node *comments.ThreadViewComment
		for _, top := range thread.Comments {
			if top.Comment.URI == abhCommentURI(i) {
				node = top
			}
		}
		require.NotNil(t, node, "A's comment %d stays in the thread %s", i, when)
		if r.wantPlaceholder {
			assert.True(t, node.Comment.IsDeleted, "A's comment %d (stage %d) must be a placeholder to V %s", i, r.stage, when)
			if assert.NotNil(t, node.Comment.DeletionReason, "A's comment %d placeholder reason %s", i, when) {
				assert.Equal(t, "moderator", *node.Comment.DeletionReason)
			}
			encoded, err := json.Marshal(node.Comment)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), abhCommentText(i), "a placeholder serves none of the comment's text")
			continue
		}
		assert.False(t, node.Comment.IsDeleted, "A's comment %d (stage %d) must be served to V %s", i, r.stage, when)
		served, ok := node.Comment.Record.(*comments.CommentRecord)
		if assert.True(t, ok, "A's comment %d serves its record, got %T", i, node.Comment.Record) {
			assert.Equal(t, abhCommentText(i), served.Content)
		}
	}
}

// B4 and the index-side rows of B6.
func TestAccountBanHide_DirectWritesAfterTheBanEnds(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		kind    abhKind
		end     abhEnd
		records []abhRecord
	}{
		{
			name: "comments, unban",
			kind: abhComment,
			end:  abhEndUnban,
			records: []abhRecord{
				{stage: abhBeforeBan, wantPlaceholder: false, wantHideReason: bhNotHidden},
				{stage: abhWhileBanned, wantPlaceholder: true, wantHideReason: abhHidden},
				{stage: abhWhileBannedRootAfterEnd, wantPlaceholder: true, wantHideReason: abhHidden},
			},
		},
		{
			name: "votes, unban",
			kind: abhVote,
			end:  abhEndUnban,
			records: []abhRecord{
				{stage: abhBeforeBan, wantUpvotes: 1, wantHideReason: bhNotHidden},
				{stage: abhWhileBanned, wantUpvotes: 0, wantHideReason: abhHidden},
				{stage: abhWhileBannedRootAfterEnd, wantUpvotes: 0, wantHideReason: abhHidden},
			},
		},
		{
			name: "comments, temporary ban lapses",
			kind: abhComment,
			end:  abhEndLapse,
			records: []abhRecord{
				{stage: abhBeforeBan, wantPlaceholder: false, wantHideReason: bhNotHidden},
				{stage: abhWhileBanned, wantPlaceholder: true, wantHideReason: abhHidden},
				{stage: abhAfterLapse, wantPlaceholder: false, wantHideReason: bhNotHidden},
			},
		},
		{
			name: "votes, temporary ban lapses",
			kind: abhVote,
			end:  abhEndLapse,
			records: []abhRecord{
				{stage: abhBeforeBan, wantUpvotes: 1, wantHideReason: bhNotHidden},
				{stage: abhWhileBanned, wantUpvotes: 0, wantHideReason: abhHidden},
				{stage: abhAfterLapse, wantUpvotes: 1, wantHideReason: bhNotHidden},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runAccountBanHideScenario(t, newAccountBanHideFixture(t), tt.kind, tt.end, tt.records)
		})
	}
}

// abhSeedForeignAccountRemoval records an active permanent instance-scope
// removal of accountDID's account subject in the shape BanAccount writes, made
// by another instance's authority: not this instance's account ban.
func abhSeedForeignAccountRemoval(t *testing.T, db *sql.DB, accountDID string) {
	t.Helper()
	const foreignAuthority = "did:web:foreign-instance.test"
	actionID := "foreign-account-removal-" + testkit.UniqueID(t)
	_, err := db.ExecContext(context.Background(), `INSERT INTO moderation_actions
		(id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, subject_kind, action, reason, origin, created_at)
		VALUES ($1, 'did:plc:foreignremovaladmin', $2, 'instance', $3, NULL, 'account', 'remove',
		        'social.coves.moderation.defs#reasonSpam', 'local', NOW())`,
		actionID, foreignAuthority, "at://"+accountDID)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO moderation_decisions
		(authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)`,
		foreignAuthority, "at://"+accountDID, actionID)
	require.NoError(t, err)
}

// Another instance's active removal of A's account subject is not this
// instance's account ban: A's comment and vote indexed under it are recorded
// unhidden, the comment is served to V and the vote counts.
func TestAccountBanHide_ForeignAuthorityRemovalHidesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAccountBanHideFixture(t)
	now := time.Now()
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(accAuthor, "create", "abhroot0",
		testkit.TID(), abhRootCID(0), now.UnixMicro(), pv2Record(accCommunity, "thread root", "root body"))))
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, abhRootURI(0), abhRootCID(0),
		testkit.TID(), now.UnixMicro())))
	require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, "abhsubject0", abhSubjectCID(0), testkit.TID(),
		now.UnixMicro(), "", abhRootURI(0), abhRootCID(0), abhRootURI(0), abhRootCID(0))), "B's comment")
	abhSeedForeignAccountRemoval(t, f.db, bhAuthor)

	require.NoError(t, f.comments.HandleEvent(ctx, abhCommentEvent(0, testkit.TID(), now.UnixMicro(), now.UTC().Format(time.RFC3339Nano))))
	require.NoError(t, f.votes.HandleEvent(ctx, abhUpvoteEvent(0, testkit.TID(), now.UnixMicro(), now.UTC().Format(time.RFC3339Nano))))

	checkAccountBanHides(t, f, abhComment, []abhRecord{{stage: abhWhileBanned, wantPlaceholder: false, wantHideReason: bhNotHidden}},
		"under another instance's account removal")
	checkAccountBanHides(t, f, abhVote, []abhRecord{{stage: abhWhileBanned, wantUpvotes: 1, wantHideReason: bhNotHidden}},
		"under another instance's account removal")
}

// abhMentionFixture is the notification mention fixture with its comment
// consumer judging account bans for this instance, and the instance moderator.
func abhMentionFixture(t *testing.T) (mentionEditFixture, *accountbanfixture.Moderator) {
	t.Helper()
	fixture := newMentionEditFixture(t)
	fixture.consumer = fixture.gate.commentConsumer(WithCommentAccountBanAuthority(fixtures.InstanceDID()))
	return fixture, accountbanfixture.New(fixture.gate.db)
}

func abhRecordNotifications(t *testing.T, fixture mentionEditFixture) int {
	t.Helper()
	return countRows(t, fixture.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, fixture.uri)
}

// A comment the banned account writes straight to its PDS while banned, which
// replies to a post and mentions D, notifies no one, before or after the
// unban. The same comment by A once unbanned notifies both.
func TestAccountBanHide_DuringBanCommentNotifiesNoOne(t *testing.T) {
	t.Parallel()
	fixture, moderator := abhMentionFixture(t)
	mentioned := fixture.recipient(t)
	moderator.Ban(t, fixture.gate.commenterDID, nil)

	fixture.create(t, mentioned)
	assert.Equal(t, abhHidden, readStoredHideReason(t, fixture.gate.db, "comments", fixture.uri))
	assert.Equal(t, 0, abhRecordNotifications(t, fixture), "a during-ban comment notifies no one while A is banned")
	moderator.Unban(t, fixture.gate.commenterDID)
	assert.Equal(t, 0, abhRecordNotifications(t, fixture), "a during-ban comment notifies no one after the unban")

	fixture.key = testkit.TID()
	fixture.create(t, mentioned)
	assert.Equal(t, 1, mentionEditRows(t, fixture, mentioned, "mention"), "control: once unbanned, A's comment mentions D")
	assert.Equal(t, 1, mentionEditRows(t, fixture, fixture.postAuthor(t), "postReply"), "control: once unbanned, A's comment replies")
}

// A's comment indexed before the ban, edited while banned to add a mention of
// E, notifies E of nothing.
func TestAccountBanHide_EditWhileBannedNotifiesNoAddedMention(t *testing.T) {
	t.Parallel()
	fixture, moderator := abhMentionFixture(t)
	fixture.create(t)
	added := fixture.recipient(t)
	moderator.Ban(t, fixture.gate.commenterDID, nil)

	require.NoError(t, fixture.update(t, fixture.createdAt, time.Now().UnixMicro(), added))
	assert.Equal(t, 0, mentionEditRows(t, fixture, added, "mention"), "an edit while banned adds no mention notification")
}

// The same for an active same-rkey re-create, which is an edit.
func TestAccountBanHide_ActiveRecreateWhileBannedNotifiesNoAddedMention(t *testing.T) {
	t.Parallel()
	fixture, moderator := abhMentionFixture(t)
	fixture.create(t)
	added := fixture.recipient(t)
	moderator.Ban(t, fixture.gate.commenterDID, nil)

	event, content := activeRecreateEvent(t, fixture, fixture.createdAt, added)
	require.NoError(t, fixture.consumer.HandleEvent(context.Background(), event))
	requireActiveRecreateApplied(t, fixture, content)
	assert.Equal(t, 0, mentionEditRows(t, fixture, added, "mention"), "an active re-create while banned adds no mention notification")
}
