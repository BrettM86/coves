//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community's removal record whose subject is a COMMENT in a thread rooted in
// that community. The removal arrives on the post consumer (the removal
// collection is a community decision record), the comment on the comment
// consumer, and the root post on the post consumer, in any order relative to
// each other. Whatever the order, a removal written by the community that hosts
// the thread root must end as one active community-scope removal decision on
// the comment, and a removal from any other repo must leave nothing.

const (
	ccrOtherCommunity = "did:plc:ccrothercommunity"
	ccrUser           = "did:plc:ccruser"
	ccrCommenter      = "did:plc:ccrcommenter"
)

// ccrFixture wires both consumers to the same community comment removal store,
// the way cmd/server/consumers.go wires production.
type ccrFixture struct {
	posts       accFixture
	comments    *CommentEventConsumer
	votes       *VoteEventConsumer
	db          *sql.DB
	rootFetcher *ccrRootFetcher
}

// ccrConsumerOptions are extra consumer options a ccrOption adds on top of the
// fixture's production-shaped wiring.
type ccrConsumerOptions struct {
	posts    []PostEventConsumerOption
	comments []CommentEventConsumerOption
	votes    []VoteEventConsumerOption
}

// ccrOption extends newCCRFixture's wiring; it receives the fixture's database
// so it can build stores on it.
type ccrOption func(db *sql.DB, o *ccrConsumerOptions)

// withCCRCommunityBans installs the real community ban index on the post
// consumer, so ban records are indexed exactly as production indexes them.
func withCCRCommunityBans() ccrOption {
	return func(db *sql.DB, o *ccrConsumerOptions) {
		o.posts = append(o.posts, WithPostCommunityBans(postgres.NewCommunityBanRepository(db)))
	}
}

// withCCRCommentOptions adds comment consumer options.
func withCCRCommentOptions(opts ...CommentEventConsumerOption) ccrOption {
	return func(_ *sql.DB, o *ccrConsumerOptions) { o.comments = append(o.comments, opts...) }
}

// withCCRVoteOptions adds vote consumer options.
func withCCRVoteOptions(opts ...VoteEventConsumerOption) ccrOption {
	return func(_ *sql.DB, o *ccrConsumerOptions) { o.votes = append(o.votes, opts...) }
}

// ccrRootFetcher stands in for the author's PDS on the acceptance direct-fetch
// path: it serves the one post a step registers and nothing else.
type ccrRootFetcher struct {
	served *FetchedPost
}

func (f *ccrRootFetcher) FetchPost(_ context.Context, postURI string) (*FetchedPost, error) {
	if f.served == nil || f.served.URI != postURI {
		return nil, fmt.Errorf("ccrRootFetcher: no record served for %s", postURI)
	}
	return f.served, nil
}

func newCCRFixture(t *testing.T, opts ...ccrOption) ccrFixture {
	t.Helper()
	db := testkit.DB(t)
	removals := postgres.NewCommunityCommentRemovalRepository(db)
	rootFetcher := &ccrRootFetcher{}
	var extra ccrConsumerOptions
	for _, opt := range opts {
		opt(db, &extra)
	}

	f := newAccFixture(t, db, append([]PostEventConsumerOption{
		WithPostCommunityCommentRemovals(removals), WithPostRecordFetcher(rootFetcher),
	}, extra.posts...)...)
	insertBridgedCommunity(t, db, ccrOtherCommunity, "ccrothercommunity.test", accAuthor)
	insertBridgedUser(t, db, ccrUser, "ccruser.test")
	insertBridgedUser(t, db, ccrCommenter, "ccrcommenter.test")

	return ccrFixture{
		posts: f,
		comments: newCommentConsumer(db, append([]CommentEventConsumerOption{
			WithCommentNotifications(postgres.NewNotificationRepository(db)),
			WithCommentCommunityRemovals(removals),
		}, extra.comments...)...),
		votes:       newVoteConsumer(db, extra.votes...),
		db:          db,
		rootFetcher: rootFetcher,
	}
}

func ccrCommentURI(rkey string) string {
	return "at://" + ccrCommenter + "/" + CommentCollection + "/" + rkey
}

// assertCommunityCommentRemoval checks the "applied" / "nothing applied"
// definitions for one comment URI, with the active apply-removal the
// community's own.
func assertCommunityCommentRemoval(t *testing.T, db *sql.DB, commentURI string, applied bool) {
	t.Helper()
	assertCommunityCommentRemovalBy(t, db, commentURI, applied, "did:plc:acccommunity", "")
}

// assertCommunityCommentRemovalBy is assertCommunityCommentRemoval with the
// active apply-removal's expected actor and reason: a re-application after the
// comment moved back is a system action with no actor.
func assertCommunityCommentRemovalBy(t *testing.T, db *sql.DB, commentURI string, applied bool, actor, reason string) {
	t.Helper()

	if !applied {
		assert.Equal(t, 0, countRows(t, db,
			`SELECT COUNT(*) FROM moderation_decisions WHERE subject_uri = $1`, commentURI),
			"no moderation decision may exist for %s", commentURI)
		assert.Equal(t, 0, countRows(t, db,
			`SELECT COUNT(*) FROM moderation_actions WHERE subject_uri = $1`, commentURI),
			"no moderation action may exist for %s", commentURI)
		return
	}

	rows, err := db.Query(`
		SELECT d.authority_did, d.scope_kind, d.scope_community_did, d.kind, d.active,
		       a.action, COALESCE(a.actor_did, ''), a.authority_did, a.scope_kind, a.scope_community_did, a.subject_uri, a.origin,
		       COALESCE(a.reason, '')
		FROM moderation_decisions d
		JOIN moderation_actions a ON a.id = d.active_action_id
		WHERE d.subject_uri = $1`, commentURI)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	type decisionRow struct {
		authority, scopeKind           string
		scopeCommunity                 sql.NullString
		kind                           string
		active                         bool
		action, actor, actionAuthority string
		actionScopeKind                string
		actionScopeCommunity           sql.NullString
		actionSubject, origin          string
		reason                         string
	}
	var got []decisionRow
	for rows.Next() {
		var r decisionRow
		require.NoError(t, rows.Scan(&r.authority, &r.scopeKind, &r.scopeCommunity, &r.kind, &r.active,
			&r.action, &r.actor, &r.actionAuthority, &r.actionScopeKind, &r.actionScopeCommunity,
			&r.actionSubject, &r.origin, &r.reason))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 1, "exactly one moderation decision must exist for %s", commentURI)
	want := decisionRow{
		authority:            "did:plc:acccommunity",
		scopeKind:            "community",
		scopeCommunity:       sql.NullString{String: "did:plc:acccommunity", Valid: true},
		kind:                 "removal",
		active:               true,
		action:               "apply-removal",
		actor:                actor,
		actionAuthority:      "did:plc:acccommunity",
		actionScopeKind:      "community",
		actionScopeCommunity: sql.NullString{String: "did:plc:acccommunity", Valid: true},
		actionSubject:        commentURI,
		origin:               "inherited",
		reason:               reason,
	}
	assert.Equal(t, want, got[0])
	assert.Equal(t, 1, countRows(t, db, `
		SELECT COUNT(*) FROM moderation_decisions d
		WHERE d.subject_uri = $1 AND d.active_action_id = (
		    SELECT id FROM moderation_actions WHERE subject_uri = $1 AND action = 'apply-removal'
		    ORDER BY created_at DESC, id DESC LIMIT 1)`, commentURI),
		"the decision must point at the newest apply-removal for %s", commentURI)
}

// ccrStep names one event delivered through a real HandleEvent call.
type ccrStep int

const (
	stepRoot                      ccrStep = iota // postv2 root in C, author repo
	stepComment                                  // comment replying to the root
	stepReply                                    // reply whose parent is the comment
	stepRemovalC                                 // removal of the subject from C
	stepRemovalC2                                // removal of the subject from indexed community C2
	stepRemovalUser                              // removal of the subject from user U
	stepRemovalDeleteC                           // C deletes its removal record
	stepRemovalReplayC                           // stale cross-feed replay of C's earlier removal create, with its original rev
	stepAcceptRoot                               // C accepts the unindexed root, which the direct fetch then indexes
	stepRemovalLateC                             // C's removal create delivered late, with a rev older than every later step's
	stepOtherRoot                                // postv2 root in C2, author repo
	stepForgedComment                            // comment shown under the C root (parent) that names the C2 root as its root
	stepCommentDelete                            // the commenter deletes the comment
	stepCommentUnderOtherRoot                    // the comment re-created at the same URI replying to the C2 root
	stepCommentUnderUnindexedRoot                // the comment re-created at the same URI replying to a post that is never indexed
)

func TestCommunityCommentRemoval_ArrivalOrders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		steps        []ccrStep
		subjectReply bool // the removal subject is the nested reply, not the comment
		applied      bool
		retracted    bool // a community decision exists for the subject but none is active
		inactive     bool // no moderation decision for the subject is active, whatever its history
		movedBack    bool // the active apply-removal is the system re-application after the comment moved back
	}{
		// B1
		{name: "B1 root, comment, removal from C", steps: []ccrStep{stepRoot, stepComment, stepRemovalC}, applied: true},
		{name: "B1 nested reply removed by C", steps: []ccrStep{stepRoot, stepComment, stepReply, stepRemovalC}, subjectReply: true, applied: true},
		// B2
		{name: "B2 removal from other indexed community", steps: []ccrStep{stepRoot, stepComment, stepRemovalC2}},
		{name: "B2 removal from a user repo", steps: []ccrStep{stepRoot, stepComment, stepRemovalUser}},
		// B3
		{name: "B3a root, removal, comment", steps: []ccrStep{stepRoot, stepRemovalC, stepComment}, applied: true},
		{name: "B3b removal, root, comment", steps: []ccrStep{stepRemovalC, stepRoot, stepComment}, applied: true},
		{name: "B3c removal, comment, root", steps: []ccrStep{stepRemovalC, stepComment, stepRoot}, applied: true},
		{name: "B3d root, removal, removal-delete, comment", steps: []ccrStep{stepRoot, stepRemovalC, stepRemovalDeleteC, stepComment}},
		{name: "B3e removal, comment, removal-delete, root", steps: []ccrStep{stepRemovalC, stepComment, stepRemovalDeleteC, stepRoot}},
		{name: "B3f removal from C2, comment, root", steps: []ccrStep{stepRemovalC2, stepComment, stepRoot}},
		{name: "B3g root, removal from C2, comment", steps: []ccrStep{stepRoot, stepRemovalC2, stepComment}},
		// Multi-feed stale replay: the original create (rev r1) arrives again after its delete (rev r2 > r1).
		{name: "stale replay of removal create after its delete", steps: []ccrStep{stepRoot, stepComment, stepRemovalC, stepRemovalDeleteC, stepRemovalReplayC}, retracted: true},
		{name: "removal, comment, root indexed by acceptance direct fetch", steps: []ccrStep{stepRemovalC, stepComment, stepAcceptRoot}, applied: true},
		// The delete is processed before its older create (redrive or cross-feed reorder).
		{name: "removal-delete before its older create", steps: []ccrStep{stepRoot, stepComment, stepRemovalDeleteC, stepRemovalLateC}},
		{name: "re-removal after retraction", steps: []ccrStep{stepRoot, stepComment, stepRemovalC, stepRemovalDeleteC, stepRemovalC}, applied: true},
		// Authority follows the thread the comment is shown in (its parent chain), not its self-declared root.
		{name: "forged root: removal from the displayed thread's community C", steps: []ccrStep{stepRoot, stepOtherRoot, stepForgedComment, stepRemovalC}, applied: true},
		{name: "forged root: removal from the named root's community C2", steps: []ccrStep{stepRoot, stepOtherRoot, stepForgedComment, stepRemovalC2}},
		{name: "forged root: removal held until the displayed root", steps: []ccrStep{stepOtherRoot, stepForgedComment, stepRemovalC, stepRoot}, applied: true},
		{name: "nested reply removal held until its parent comment", steps: []ccrStep{stepRoot, stepReply, stepRemovalC, stepComment}, subjectReply: true, applied: true},
		// Authority follows a resurrected comment to the thread it is re-created in.
		{name: "removed comment re-created under another community's post", steps: []ccrStep{stepRoot, stepOtherRoot, stepComment, stepRemovalC, stepCommentDelete, stepCommentUnderOtherRoot}, inactive: true},
		{name: "removed comment re-created elsewhere, then back under the removing community's post", steps: []ccrStep{stepRoot, stepOtherRoot, stepComment, stepRemovalC, stepCommentDelete, stepCommentUnderOtherRoot, stepCommentDelete, stepComment}, applied: true, movedBack: true},
		{name: "removed reply's parent re-created under another community's post", steps: []ccrStep{stepRoot, stepOtherRoot, stepComment, stepReply, stepRemovalC, stepCommentDelete, stepCommentUnderOtherRoot}, subjectReply: true, inactive: true},
		{name: "removed comment re-created under another community's post indexed later", steps: []ccrStep{stepRoot, stepComment, stepRemovalC, stepCommentDelete, stepCommentUnderOtherRoot, stepOtherRoot}, inactive: true},
		// An unresolved parent chain must not un-hide a removed comment.
		{name: "removed comment re-created under a never-indexed post stays removed", steps: []ccrStep{stepRoot, stepComment, stepRemovalC, stepCommentDelete, stepCommentUnderUnindexedRoot}, applied: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCCRFixture(t)
			ctx := context.Background()
			base := time.Now().UnixMicro()
			revs := increasingTIDs(t, len(tt.steps))

			const (
				rootRkey      = "ccrroot"
				rootCID       = "bafyreiccrroot"
				commentRkey   = "ccrcomment"
				commentCID    = "bafyreiccrcomment"
				replyRkey     = "ccrreply"
				replyCID      = "bafyreiccrreply"
				otherRootRkey = "ccrotherroot"
				otherRootCID  = "bafyreiccrotherroot"
				// otherCommentCID is the comment re-created under the C2 root.
				otherCommentCID = "bafyreiccrcommentinc2"
			)
			rootURI := accPostURI(rootRkey)
			commentURI := ccrCommentURI(commentRkey)
			replyURI := ccrCommentURI(replyRkey)

			subjectURI, subjectCID := commentURI, commentCID
			if tt.subjectReply {
				subjectURI, subjectCID = replyURI, replyCID
			}
			code := string(posts.DecisionRuleViolation)
			var removalRev string
			var removalTimeUS int64

			for i, step := range tt.steps {
				rev := revs[i]
				timeUS := base + int64(i+1)*1_000_000
				switch step {
				case stepRoot:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
						accAuthor, "create", rootRkey, rev, rootCID, timeUS,
						pv2Record(accCommunity, "thread root", "root body"))), "root post")
				case stepComment:
					require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "create", commentRkey, rev, commentCID, timeUS,
						revCommentRecord("the comment", rootURI, rootCID, rootURI, rootCID))), "comment")
				case stepReply:
					require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "create", replyRkey, rev, replyCID, timeUS,
						revCommentRecord("the reply", rootURI, rootCID, commentURI, commentCID))), "reply")
				case stepRemovalC:
					removalRev, removalTimeUS = rev, timeUS
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
						accCommunity, subjectURI, subjectCID, code, rev, timeUS)), "removal from C")
				case stepRemovalReplayC:
					require.NotEmpty(t, removalRev, "replay needs an earlier removal from C")
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
						accCommunity, subjectURI, subjectCID, code, removalRev, removalTimeUS)), "stale removal replay from C")
				case stepRemovalC2:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
						ccrOtherCommunity, subjectURI, subjectCID, code, rev, timeUS)), "removal from C2")
				case stepRemovalUser:
					err := f.posts.consumer.HandleEvent(ctx, removalEvent(
						ccrUser, subjectURI, subjectCID, code, rev, timeUS))
					if err != nil {
						require.ErrorIs(t, err, ErrUnresolvedReference, "removal from a user repo")
					}
				case stepRemovalDeleteC:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalDeleteEvent(
						accCommunity, subjectURI, rev, timeUS)), "removal delete from C")
				case stepAcceptRoot:
					f.rootFetcher.served = &FetchedPost{
						URI: rootURI, CID: rootCID,
						Record: pv2Record(accCommunity, "thread root", "root body"),
					}
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(
						accCommunity, rootURI, rootCID, rev, timeUS)), "acceptance of the unindexed root from C")
				case stepRemovalLateC:
					require.Positive(t, i, "a late create needs an earlier step's older rev")
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
						accCommunity, subjectURI, subjectCID, code, revs[0], timeUS)), "late removal create from C")
				case stepOtherRoot:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
						accAuthor, "create", otherRootRkey, rev, otherRootCID, timeUS,
						pv2Record(ccrOtherCommunity, "other root", "other body"))), "C2 root post")
				case stepForgedComment:
					require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "create", commentRkey, rev, commentCID, timeUS,
						revCommentRecord("the forged comment", accPostURI(otherRootRkey), otherRootCID, rootURI, rootCID))), "forged comment")
				case stepCommentDelete:
					require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "delete", commentRkey, rev, "", timeUS, nil)), "comment delete")
				case stepCommentUnderOtherRoot:
					otherRootURI := accPostURI(otherRootRkey)
					require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "create", commentRkey, rev, otherCommentCID, timeUS,
						revCommentRecord("the comment, re-created", otherRootURI, otherRootCID, otherRootURI, otherRootCID))), "comment re-created under the C2 root")
				case stepCommentUnderUnindexedRoot:
					unindexedURI := accPostURI("ccrneverindexed")
					err := f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "create", commentRkey, rev, "bafyreiccrcommentunindexed", timeUS,
						revCommentRecord("the comment, re-created", unindexedURI, "bafyreiccrneverindexed", unindexedURI, "bafyreiccrneverindexed")))
					if err != nil {
						require.ErrorIs(t, err, ErrUnresolvedReference, "comment re-created under a never-indexed post")
					}
				}
			}

			if tt.inactive {
				assert.Equal(t, 0, countRows(t, f.db,
					`SELECT COUNT(*) FROM moderation_decisions WHERE subject_uri = $1 AND active`, subjectURI),
					"no active moderation decision may exist for %s", subjectURI)
			} else if tt.retracted {
				assert.Equal(t, 1, countRows(t, f.db, `
					SELECT COUNT(*) FROM moderation_decisions
					WHERE subject_uri = $1 AND authority_did = 'did:plc:acccommunity'
					  AND scope_kind = 'community' AND kind = 'removal' AND NOT active`, subjectURI),
					"the retracted community decision must remain inactive")
				assert.Equal(t, 0, countRows(t, f.db,
					`SELECT COUNT(*) FROM moderation_decisions WHERE subject_uri = $1 AND active`, subjectURI),
					"no active moderation decision may exist for %s", subjectURI)
			} else if tt.movedBack {
				assertCommunityCommentRemovalBy(t, f.db, subjectURI, tt.applied, "", "social.coves.moderation.defs#reasonSubjectMoved")
			} else {
				assertCommunityCommentRemoval(t, f.db, subjectURI, tt.applied)
			}
			if tt.subjectReply {
				assertCommunityCommentRemoval(t, f.db, commentURI, false)
			}
		})
	}
}

// B5: routing a removal by subject must keep post-subject removals on the
// admission path, without a moderation decision.
func TestCommunityCommentRemoval_PostSubjectStaysAdmissionRemoval(t *testing.T) {
	t.Parallel()
	f := newCCRFixture(t)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	const cid = "bafyreiccrpostsubject"
	uri := f.posts.indexPV2(t, "ccrpostsubject", cid, base)

	require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
		accCommunity, uri, cid, string(posts.DecisionRuleViolation), testkit.TID(), base+1_000_000)))

	row := readAdmissionRow(t, f.db, accCommunity, uri)
	assert.Equal(t, "removed", row[0], "a removal of a post subject is an admission removal")
	assert.Equal(t, 0, countRows(t, f.db,
		`SELECT COUNT(*) FROM moderation_decisions WHERE subject_uri = $1`, uri),
		"a post-subject removal must not create a moderation decision")

	// The community restores the post: the comment-removal store does not know
	// the key, so the delete must still reach the admission row.
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalDeleteEvent(
		accCommunity, uri, testkit.TID(), base+2_000_000)))

	row = readAdmissionRow(t, f.db, accCommunity, uri)
	assert.NotEqual(t, "removed", row[0], "deleting a post-subject removal must take the post out of removed")
}

// failingHeldRootStore is the real store with ApplyHeldForRoot failing.
type failingHeldRootStore struct {
	*postgres.CommunityCommentRemovalRepository
	err error
}

func (s failingHeldRootStore) ApplyHeldForRoot(context.Context, string) error { return s.err }

// A failure applying held comment removals under a newly indexed root must not
// cost the root its pending admission.
func TestCommunityCommentRemoval_HeldApplyFailureKeepsRootAdmission(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	errHeldApply := errors.New("held apply failed")
	f := newAccFixture(t, db, WithPostCommunityCommentRemovals(failingHeldRootStore{
		CommunityCommentRemovalRepository: postgres.NewCommunityCommentRemovalRepository(db),
		err:                               errHeldApply,
	}))

	const rkey, cid = "ccrheldfailroot", "bafyreiccrheldfailroot"
	err := f.consumer.HandleEvent(context.Background(), pv2Event(
		accAuthor, "create", rkey, testkit.TID(), cid, time.Now().UnixMicro(),
		pv2Record(accCommunity, "thread root", "root body")))
	require.ErrorIs(t, err, errHeldApply, "the held-removal failure must be returned so a replay re-runs it")

	assert.Equal(t, 1, countRows(t, db, `
		SELECT COUNT(*) FROM community_post_admissions
		WHERE community_did = $1 AND post_uri = $2 AND status = 'pending'`, accCommunity, accPostURI(rkey)),
		"the root's pending admission must be recorded before held comment removals are applied")
}

// failFirstHeldRootStore is the real store with only the first ApplyHeldForRoot
// call failing; later calls reach the real repository.
type failFirstHeldRootStore struct {
	*postgres.CommunityCommentRemovalRepository
	err    error
	failed bool
}

func (s *failFirstHeldRootStore) ApplyHeldForRoot(ctx context.Context, rootURI string) error {
	if !s.failed {
		s.failed = true
		return s.err
	}
	return s.CommunityCommentRemovalRepository.ApplyHeldForRoot(ctx, rootURI)
}

// A failure applying held comment removals after the acceptance direct fetch
// indexed the root must be repaired by redelivering the same acceptance: the
// replay finds the root indexed, and the held removal must still be applied.
func TestCommunityCommentRemoval_AcceptanceReplayAppliesHeldRemovalAfterFailure(t *testing.T) {
	t.Parallel()
	errHeldApply := errors.New("held apply failed")
	var store *failFirstHeldRootStore
	f := newCCRFixture(t, func(db *sql.DB, o *ccrConsumerOptions) {
		store = &failFirstHeldRootStore{
			CommunityCommentRemovalRepository: postgres.NewCommunityCommentRemovalRepository(db),
			err:                               errHeldApply,
		}
		o.posts = append(o.posts, WithPostCommunityCommentRemovals(store))
	})
	require.NotNil(t, store)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 3)

	const (
		rootRkey    = "ccraccreplayroot"
		rootCID     = "bafyreiccraccreplayroot"
		commentRkey = "ccraccreplaycomment"
		commentCID  = "bafyreiccraccreplaycomment"
	)
	rootURI := accPostURI(rootRkey)
	commentURI := ccrCommentURI(commentRkey)

	require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
		accCommunity, commentURI, commentCID, string(posts.DecisionRuleViolation), revs[0], base+1_000_000)), "removal from C")
	require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
		ccrCommenter, CommentCollection, "create", commentRkey, revs[1], commentCID, base+2_000_000,
		revCommentRecord("the comment", rootURI, rootCID, rootURI, rootCID))), "comment")
	assertCommunityCommentRemoval(t, f.db, commentURI, false)

	f.rootFetcher.served = &FetchedPost{
		URI: rootURI, CID: rootCID,
		Record: pv2Record(accCommunity, "thread root", "root body"),
	}
	acceptance := acceptanceEvent(accCommunity, rootURI, rootCID, revs[2], base+3_000_000)

	err := f.posts.consumer.HandleEvent(ctx, acceptance)
	require.ErrorIs(t, err, errHeldApply, "the held-removal failure must be returned so a replay re-runs it")

	require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptance), "replay of the same acceptance")
	assertCommunityCommentRemoval(t, f.db, commentURI, true)
}

// A removal record's key is derived from its subject
// (posts.SubjectRkey(subject.uri)), so one subject has one removal record per
// community. A create or update at any other key is refused as permanent
// before anything is indexed, whether the subject is a comment or a post:
// otherwise two records could remove one subject, and deleting the canonical
// one would restore it while the other still stands.
func TestCommunityCommentRemoval_NonCanonicalRecordKeyIsRefused(t *testing.T) {
	t.Parallel()

	const (
		rootRkey    = "ccrkeyroot"
		rootCID     = "bafyreiccrkeyroot"
		commentRkey = "ccrkeycomment"
		commentCID  = "bafyreiccrkeycomment"
	)
	rootURI := accPostURI(rootRkey)
	commentURI := ccrCommentURI(commentRkey)
	otherSubjectKey := posts.SubjectRkey(ccrCommentURI("ccrkeyothercomment"))

	tests := []struct {
		name          string
		commentTarget bool   // the removal's subject is the comment, not the root post
		operation     string // create or update
		rkey          string // "" is the subject's canonical key
		refused       bool
	}{
		{name: "comment subject, create at an invented key", commentTarget: true, operation: "create", rkey: "ccrnoncanonical", refused: true},
		{name: "comment subject, update at another subject's key", commentTarget: true, operation: "update", rkey: otherSubjectKey, refused: true},
		{name: "post subject, create at an invented key", operation: "create", rkey: "ccrnoncanonical", refused: true},
		{name: "post subject, update at another subject's key", operation: "update", rkey: otherSubjectKey, refused: true},
		{name: "comment subject at its canonical key", commentTarget: true, operation: "create"},
		{name: "post subject at its canonical key", operation: "create"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCCRFixture(t)
			ctx := context.Background()
			base := time.Now().UnixMicro()
			revs := increasingTIDs(t, 2)

			f.posts.indexPV2(t, rootRkey, rootCID, base)
			require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
				ccrCommenter, CommentCollection, "create", commentRkey, revs[0], commentCID, base+1_000_000,
				revCommentRecord("the comment", rootURI, rootCID, rootURI, rootCID))), "comment")
			admissionBefore := readAdmissionRow(t, f.db, accCommunity, rootURI)

			subjectURI, subjectCID := rootURI, rootCID
			if tt.commentTarget {
				subjectURI, subjectCID = commentURI, commentCID
			}
			rkey := tt.rkey
			if rkey == "" {
				rkey = posts.SubjectRkey(subjectURI)
			}
			err := f.posts.consumer.HandleEvent(ctx, removalEventAt(accCommunity, tt.operation, rkey,
				subjectURI, subjectCID, string(posts.DecisionRuleViolation), revs[1], base+2_000_000))

			if !tt.refused {
				require.NoError(t, err)
				if tt.commentTarget {
					assertCommunityCommentRemoval(t, f.db, commentURI, true)
				} else {
					assert.Equal(t, "removed", readAdmissionRow(t, f.db, accCommunity, rootURI)[0],
						"a removal at the canonical key removes the post")
				}
				return
			}

			require.ErrorIs(t, err, ErrPermanentEvent, "a removal at a key not derived from its subject")
			assert.Equal(t, 0, countRows(t, f.db,
				`SELECT COUNT(*) FROM community_comment_removals`),
				"a refused removal must not be indexed")
			assert.Equal(t, 0, countRows(t, f.db,
				`SELECT COUNT(*) FROM jetstream_record_revs WHERE record_uri = $1`,
				"at://"+accCommunity+"/"+posts.RemovalCollection+"/"+rkey),
				"a refused removal must not advance its rev gate")
			assertCommunityCommentRemoval(t, f.db, commentURI, false)
			assert.Equal(t, admissionBefore, readAdmissionRow(t, f.db, accCommunity, rootURI),
				"a refused removal must leave the post's admission row unchanged")
		})
	}
}
