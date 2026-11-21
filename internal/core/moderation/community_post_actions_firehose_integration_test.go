//go:build integration

package moderation_test

import (
	"context"
	"errors"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Community post removals and restores arriving through the post consumer:
// records written to a community repo outside Coves, the firehose copies of a
// local restore whose response was lost, and removals indexed before the
// removal identity index existed. The index, intents and actions are real;
// the community repo is faked.

const (
	postFirehoseExternalRemovalCID    = "bafyreiaremovalexternalfirehoseaaaaaaaaaaaaaaaaaaaaaaaa"
	postFirehoseExternalAcceptanceCID = "bafyreiaacceptanceexternalfirehoseaaaaaaaaaaaaaaaaaaaaa"
	postFirehoseExternalCreatedAt     = "2026-11-12T09:00:00Z"

	postFirehoseHeadThreeCID = "bafyreiaheadthreeaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	postFirehoseHeadFourRev  = "3krosterrev04"
	postFirehoseHeadFourCID  = "bafyreiaheadfouraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	postDBReasonModeratorDiscretion = "social.coves.moderation.defs#reasonModeratorDiscretion"
	postDBReasonDoxing              = "social.coves.moderation.defs#reasonDoxing"
)

// intentStates is the state of each of rosteralpha's intents, oldest first.
func (f *communityPostDBFixture) intentStates(t *testing.T) []string {
	t.Helper()
	states := []string{}
	for _, intent := range f.intents(t) {
		states = append(states, intent.State)
	}
	return states
}

// I11: a removal written straight to the community repo outside Coves arrives
// through the consumer as its commit's two events (removal create, acceptance
// delete), in the hosted community and in an indexed community this instance
// does not host. One actorless, inherited remove action with the mapped reason,
// one version advance. The removal is then withdrawn outside Coves (its delete
// alone): the post returns to pending, one actorless restore action carries the
// deleted removal's mapped code, one more version advance.
func TestCommunityPostFirehoseDB_ExternalRemovalThenWithdrawal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hosted     bool
		code       string
		wantReason string
	}{
		{name: "hosted community, code spam", hosted: true, code: "spam", wantReason: postDBReasonSpam},
		{name: "indexed community this instance does not host, code spam", code: "spam", wantReason: postDBReasonSpam},
		{name: "hosted community, unknown code foo", hosted: true, code: "foo", wantReason: postDBReasonModeratorDiscretion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			community := rosterCommunity
			var post seededCommunityPost
			if tt.hosted {
				post = f.seedAcceptedPost(t, postDBAlpha)
			} else {
				community = postDBUnhostedCommunity
				post = f.seedUnhostedCommunityPost(t, postDBAlpha)
			}
			removal := externalPostActionIn(community, "remove", post.URI, tt.wantReason)

			require.NoError(t, f.deliverPost(postStandEventIn(community, "create", posts.RemovalCollection, post.SubjectRkey,
				rosterHeadOneRev, postFirehoseExternalRemovalCID,
				removalRecordBody(post, tt.code, postDBRemovalReason, postFirehoseExternalCreatedAt))))
			require.NoError(t, f.deliverPost(postStandEventIn(community, "delete", posts.AcceptanceCollection, post.SubjectRkey,
				rosterHeadOneRev, "", nil)))

			admission := f.admissionIn(t, community, post.URI)
			assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status, "the external removal is indexed")
			if assert.NotNil(t, admission.DecisionCode, "the removal's code is stored") {
				assert.Equal(t, tt.code, *admission.DecisionCode)
			}
			assert.Equal(t, []storedPostAction{removal}, f.postActionsIn(t, community),
				"one actorless remove action with the mapped reason")
			assert.EqualValues(t, 2, f.postVersionIn(t, community, post.URI), "the removal advanced the version once")

			require.NoError(t, f.deliverPost(postStandEventIn(community, "delete", posts.RemovalCollection, post.SubjectRkey,
				rosterHeadTwoRev, "", nil)))

			assert.Equal(t, posts.AdmissionStatusPending, f.admissionIn(t, community, post.URI).Status,
				"the withdrawn removal no longer stands")
			assert.Equal(t, []storedPostAction{
				removal,
				externalPostActionIn(community, "restore", post.URI, tt.wantReason),
			}, f.postActionsIn(t, community), "one actorless restore action with the deleted removal's mapped code")
			assert.EqualValues(t, 3, f.postVersionIn(t, community, post.URI), "the withdrawal advanced the version once")
		})
	}
}

// Chunk 13 B7: a removal written to the hosted community's repo outside Coves
// with a code Coves does not know lists publicly, filtered to the community,
// as moderator discretion and names the post.
func TestCommunityModlog_ExternalUnknownCodeRemovalListsPublicly(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.standExternalRemoval(t, post, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
		postFirehoseExternalRemovalCID, removalRecordBody(post, "made-up-code", postDBRemovalReason, postFirehoseExternalCreatedAt))

	entries := publicModlog(t, f.db, moderation.ListActionsParams{Community: "did:plc:rosteralpha"})

	require.Len(t, entries, 1, "the external removal is listed once")
	entry := entries[0]
	assert.Equal(t, "remove", entry["action"])
	assert.Equal(t, "social.coves.moderation.defs#reasonModeratorDiscretion", entry["reason"])
	assert.NotContains(t, entry, "actor", "an external removal has no actor")
	if assert.Contains(t, entry, "subject", "the post is named") {
		assert.Equal(t, "at://did:plc:rosterauthor/social.coves.community.postv2/3kpostalpha1",
			modlogObject(t, entry["subject"])["uri"])
	}
	assert.Equal(t, map[string]any{"kind": "community", "communityDid": "did:plc:rosteralpha"}, entry["scope"])
}

// R1-1: an externally written off-topic removal stands; an external restore
// commit {removal delete, acceptance create pinned to the post's CID} arrives
// in either event order. Either way the post is accepted and one actorless
// restore action carries the removal's mapped code; the version advances per
// applied record change (chunk 04).
func TestCommunityPostFirehoseDB_ExternalRestoreInEitherOrder(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		acceptanceFirst bool
		// wantVersion: each applied record change advances the version (04).
		wantVersion int64
	}{
		{name: "acceptance first", acceptanceFirst: true, wantVersion: 3},
		{name: "removal delete first", wantVersion: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			f.standExternalRemoval(t, post, pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadOneRev},
				postFirehoseExternalRemovalCID,
				removalRecordBody(post, "off-topic", postDBRemovalReason, postFirehoseExternalCreatedAt))
			removal := externalPostAction("remove", post.URI, postDBReasonRuleViolation)
			assert.Equal(t, []storedPostAction{removal}, f.postActions(t), "the external removal's action")
			assert.EqualValues(t, 2, f.postVersion(t, post.URI), "the external removal advanced the version")

			f.deliverRestoreCommitIn(t, rosterCommunity, post, rosterHeadTwoRev, postFirehoseExternalAcceptanceCID, tt.acceptanceFirst)

			admission := f.admission(t, post.URI)
			assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "the post is accepted again")
			if assert.NotNil(t, admission.AcceptedCID, "the acceptance pins a CID") {
				assert.Equal(t, postAlphaCID, *admission.AcceptedCID)
			}
			assert.Equal(t, []storedPostAction{
				removal,
				externalPostAction("restore", post.URI, postDBReasonRuleViolation),
			}, f.postActions(t), "one actorless restore action with the removal's mapped code")
			assert.EqualValues(t, tt.wantVersion, f.postVersion(t, post.URI), "one advance per applied record change")
		})
	}
}

// R1-1: M restores a post M removed; the commit lands and the response is
// lost. With no retry, the commit's firehose copies arrive in either order and
// settle the open intent: one restore action naming M, the intent completed,
// one version advance for the restore.
func TestCommunityPostFirehoseDB_LostRestoreResponseSettledByFirehose(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		acceptanceFirst bool
	}{
		{name: "acceptance first", acceptanceFirst: true},
		{name: "removal delete first"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
			assert.NoError(t, err)
			assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status, "M's removal landed")
			f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
			// The PDS assigns the acceptance the CID of the body the restore sent.
			acceptanceCID := recordCIDOf(t, acceptanceRecordBody(post, postDBIntentCreatedAt))
			f.setAcceptanceWriteCID(post, acceptanceCID)
			f.repo.applyThenErr = errors.New("pds: connection reset by peer")

			_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
			assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
			assert.Equal(t, 1, f.openIntents(t), "the restore intent stays open")

			f.deliverRestoreCommitIn(t, rosterCommunity, post, rosterHeadTwoRev, acceptanceCID, tt.acceptanceFirst)

			assert.Equal(t, []storedPostAction{
				localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
				localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
			}, f.postActions(t), "one restore action names M, from the open intent")
			assert.Equal(t, []string{"completed", "completed"}, f.intentStates(t), "the restore intent is completed")
			admission := f.admission(t, post.URI)
			assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "the post is accepted again")
			if assert.NotNil(t, admission.AcceptedCID, "the acceptance pins a CID") {
				assert.Equal(t, postAlphaCID, *admission.AcceptedCID)
			}
			assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the restore advanced the version once")
		})
	}
}

// R2-1 (I7c): M's lost-response restore is recovered by a same-key retry; its
// delayed firehose copies change nothing. A removal then written outside Coves
// is a new change (actorless remove action, version +1), and so is its
// external withdrawal (actorless restore action with its mapped code,
// version +1).
func TestCommunityPostFirehoseDB_RecoveredRestoreThenExternalRemovalAndWithdrawal(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	assert.NoError(t, err)
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	result, err := f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")

	assert.NoError(t, err, "the same-key retry settles the lost restore")
	assert.NotNil(t, result, "the retry returns its result")
	local := []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
	}
	assert.Equal(t, local, f.postActions(t), "the retry records one restore action naming M")
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the restore advanced the version once")
	settled := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusAccepted, settled.Status, "the post is accepted again")

	// The restore commit's delayed firehose copies, at head two's rev.
	f.deliverRestoreCommitIn(t, rosterCommunity, post, rosterHeadTwoRev, postAcceptanceWrittenCID, true)

	assert.Equal(t, local, f.postActions(t), "the copies add no action")
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the copies leave the version")
	afterCopies := f.admission(t, post.URI)
	assert.Equal(t, settled.Status, afterCopies.Status, "the copies leave the status")
	assert.Equal(t, settled.AcceptedCID, afterCopies.AcceptedCID, "the copies leave the pinned CID")
	assert.Equal(t, settled.DecisionCode, afterCopies.DecisionCode, "the copies leave the decision")

	// A removal written outside Coves.
	f.standExternalRemoval(t, post, pds.LatestCommit{CID: postFirehoseHeadThreeCID, Rev: rosterHeadThreeRev},
		postFirehoseExternalRemovalCID, removalRecordBody(post, "spam", postDBRemovalReason, "2026-11-13T14:00:00Z"))

	assert.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status, "the external removal is indexed")
	assert.Equal(t, append(append([]storedPostAction{}, local...), externalPostAction("remove", post.URI, postDBReasonSpam)),
		f.postActions(t), "the external removal records one actorless action")
	assert.EqualValues(t, 4, f.postVersion(t, post.URI), "the external removal advanced the version")

	// Its withdrawal outside Coves.
	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, post.SubjectRkey, postFirehoseHeadFourRev, "", nil)))

	assert.Equal(t, posts.AdmissionStatusPending, f.admission(t, post.URI).Status, "the withdrawn removal no longer stands")
	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
		externalPostAction("remove", post.URI, postDBReasonSpam),
		externalPostAction("restore", post.URI, postDBReasonSpam),
	}, f.postActions(t), "the full action history")
	assert.EqualValues(t, 5, f.postVersion(t, post.URI), "the withdrawal advanced the version")
}

// R2-2: a removal indexed before migration 088 (the 087 consumer stored only
// the admission row: status removed, decision_code spam, removal_reason) is
// back-filled into the removal identity index by 088. Its later external
// withdrawal alone, or an external paired restore in either event order,
// writes one actorless restore action with the removal's mapped code.
func TestCommunityPostFirehoseDB_BackfilledRemoval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		deliver     func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost)
		wantStatus  posts.AdmissionStatus
		wantVersion int64
	}{
		{
			name: "withdrawal alone",
			deliver: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev, "", nil)))
			},
			wantStatus:  posts.AdmissionStatusPending,
			wantVersion: 3,
		},
		{
			name: "paired restore, acceptance first",
			deliver: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				f.deliverRestoreCommitIn(t, rosterCommunity, post, rosterHeadTwoRev, postFirehoseExternalAcceptanceCID, true)
			},
			wantStatus:  posts.AdmissionStatusAccepted,
			wantVersion: 3,
		},
		{
			name: "paired restore, removal delete first",
			deliver: func(t *testing.T, f *communityPostDBFixture, post seededCommunityPost) {
				f.deliverRestoreCommitIn(t, rosterCommunity, post, rosterHeadTwoRev, postFirehoseExternalAcceptanceCID, false)
			},
			wantStatus:  posts.AdmissionStatusAccepted,
			wantVersion: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedBackfilledRemoval(t)

			tt.deliver(t, f, post)

			assert.Equal(t, tt.wantStatus, f.admission(t, post.URI).Status, "the removal no longer stands")
			assert.Equal(t, []storedPostAction{externalPostAction("restore", post.URI, postDBReasonSpam)}, f.postActions(t),
				"one actorless restore action with the back-filled removal's mapped code")
			assert.EqualValues(t, tt.wantVersion, f.postVersion(t, post.URI), "one advance per applied record change")
		})
	}
}

// seedBackfilledRemoval is rosteralpha's postDBAlpha removed (code spam,
// reason postDBRemovalReason, at head one's rev, version 2) as the 087
// consumer indexed it, then back-filled into the removal identity index by
// 088: its removal record row has no record key, CID or rev.
func (f *communityPostDBFixture) seedBackfilledRemoval(t *testing.T) seededCommunityPost {
	t.Helper()
	post := f.seedAcceptedPost(t, postDBAlpha)
	require.EqualValues(t, 93, testkit.MigrateDownOne(t, f.db, 93),
		"093 (community report notifications) must be rolled back first")
	require.EqualValues(t, 92, testkit.MigrateDownOne(t, f.db, 92),
		"092 (community report dismissals) must be rolled back first")
	require.EqualValues(t, 91, testkit.MigrateDownOne(t, f.db, 91),
		"091 (moderation action role) must be rolled back first")
	require.EqualValues(t, 90, testkit.MigrateDownOne(t, f.db, 90),
		"090 (community ban actions) must be rolled back first")
	require.EqualValues(t, 89, testkit.MigrateDownOne(t, f.db, 89),
		"089 (community comment actions) must be rolled back first")
	require.EqualValues(t, 88, testkit.MigrateDownOne(t, f.db, 88),
		"088 (community post removal records) must be rolled back to index a removal as 087 did")
	// What the 087 consumer's ApplyRemoval stored for a removal at head
	// one's rev, and the version advance it made.
	_, err := f.db.Exec(`
		UPDATE community_post_admissions SET
			status = 'removed', decision_code = 'spam', decision_at = NOW(), removal_reason = $3,
			acceptance_uri = NULL, acceptance_rkey = NULL, accepted_cid = NULL,
			last_community_rev = $4, last_community_op_rank = $5, updated_at = NOW()
		WHERE community_did = $1 AND post_uri = $2
	`, rosterCommunity, post.URI, postDBRemovalReason, rosterHeadOneRev, int16(posts.CommunityOpPut))
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE community_subject_versions SET version = 2 WHERE community_did = $1 AND subject = $2`,
		rosterCommunity, post.URI)
	require.NoError(t, err)
	testkit.MigrateUp(t, f.db)
	require.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status, "fixture: the legacy removal stands")
	require.EqualValues(t, 2, f.postVersion(t, post.URI), "fixture: the legacy removal's version")
	return post
}

// R2-2 follow-up: a removal record create for a back-filled removal (its
// identity row has no record key, CID or rev) arrives at a rev after the
// admission's watermark — a redelivery after a cursor rewind, or a rewrite.
// With the back-filled code and reason it is the record the index already
// holds: no action, no version advance, the removal stands as it was. With
// another code it is a new change: one actorless inherited remove action with
// the new code's mapped reason, one version advance.
func TestCommunityPostFirehoseDB_BackfilledRemovalRecordRedelivered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		code        string
		wantActions func(post seededCommunityPost) []storedPostAction
		wantVersion int64
		wantCode    string
	}{
		{
			name:        "same code and reason",
			code:        "spam",
			wantActions: func(seededCommunityPost) []storedPostAction { return []storedPostAction{} },
			wantVersion: 2,
			wantCode:    "spam",
		},
		{
			name: "code spam rewritten to off-topic",
			code: "off-topic",
			wantActions: func(post seededCommunityPost) []storedPostAction {
				return []storedPostAction{externalPostAction("remove", post.URI, postDBReasonRuleViolation)}
			},
			wantVersion: 3,
			wantCode:    "off-topic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedBackfilledRemoval(t)

			require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, post.SubjectRkey,
				rosterHeadTwoRev, postFirehoseExternalRemovalCID,
				removalRecordBody(post, tt.code, postDBRemovalReason, postFirehoseExternalCreatedAt))),
				"the removal record event is handled")

			admission := f.admission(t, post.URI)
			assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status, "the post stays removed")
			require.NotNil(t, admission.DecisionCode, "the removal's code")
			assert.Equal(t, tt.wantCode, *admission.DecisionCode, "the removal's code")
			require.NotNil(t, admission.RemovalReason, "the removal's reason")
			assert.Equal(t, postDBRemovalReason, *admission.RemovalReason, "the removal's reason")
			assert.Equal(t, tt.wantActions(post), f.postActions(t), "the actions the record change wrote")
			assert.EqualValues(t, tt.wantVersion, f.postVersion(t, post.URI), "one advance per applied record change")
		})
	}
}

// Review r1 #3: M rewrites a standing spam removal to off-topic. After M's
// intent is stored and before the writer re-reads the stand record, another
// AppView writer (one that takes no community lock) rewrites the removal with
// a new createdAt at head two. The writer keeps that createdAt, so the body it
// sends differs from the intent's. The commit lands at head three, the
// response is lost, and the rewrite's firehose copy (the CID of the body
// sent) settles the open intent: one new action names M, none is inherited.
func TestCommunityPostFirehoseDB_LostRewriteSettledByFirehose(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	const (
		offTopicReason       = "Not about this community"
		interleavedCreatedAt = "2026-11-13T12:30:00Z"
	)
	interleaved := removalRecordBody(post, "spam", postDBRemovalReason, interleavedCreatedAt)
	service := f.serviceWithHooks(moderation.CommunityActionHooks{
		BeforeWrite: func(context.Context) {
			f.repo.setPostRecord(posts.RemovalCollection, post.SubjectRkey, recordCIDOf(t, interleaved), interleaved)
			f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
		},
	})
	sent := removalRecordBody(post, "off-topic", offTopicReason, interleavedCreatedAt)
	sentCID := recordCIDOf(t, sent)
	f.setRemovalWriteCID(post, sentCID)
	f.setCommitHead(pds.LatestCommit{CID: postFirehoseHeadThreeCID, Rev: rosterHeadThreeRev})
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err = removePostWith(service, rosterModerator, post.URI, "off-topic", offTopicReason, 2, "rewrite-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
	written := f.standRecord(posts.RemovalCollection, post.SubjectRkey)
	require.NotNil(t, written)
	require.Equal(t, sentCID, written.CID, "fixture: the repo holds the body the writer sent")

	require.NoError(t, f.deliverPost(postStandEvent("update", posts.RemovalCollection, post.SubjectRkey, rosterHeadThreeRev, sentCID, sent)))

	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonRuleViolation),
	}, f.postActions(t), "the firehose copy completes M's intent; no inherited action")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
	if code := f.admission(t, post.URI).DecisionCode; assert.NotNil(t, code) {
		assert.Equal(t, "off-topic", *code)
	}
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the rewrite advanced the version once")
}

// Review r1 #4: M's restore fails before it commits and its intent stays
// open. The removal record is then deleted on its own outside Coves. That
// delete is an inherited withdrawal: the post goes back to pending with no
// acceptance, one actorless restore action carries the removal's mapped code,
// and no restore names M.
func TestCommunityPostFirehoseDB_RemovalDeleteAloneWhileRestoreIntentOpen(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the failed write reads as unavailable")
	require.Equal(t, 1, f.openIntents(t), "fixture: the restore intent stays open")

	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev, "", nil)))

	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusPending, admission.Status, "the withdrawn removal leaves the post pending")
	assert.Nil(t, admission.AcceptedCID, "no acceptance the repo does not hold is applied")
	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		externalPostAction("restore", post.URI, postDBReasonSpam),
	}, f.postActions(t), "one actorless restore action; none names M")
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the withdrawal advanced the version once")
}

// Review r1 #7(a): M's removal commits at head one and the response is lost.
// A different removal (code off-topic, another CID) written outside Coves at
// head two arrives first. It is not M's commit: one actorless inherited
// action, none names M, and M's intent stays open.
func TestCommunityPostFirehoseDB_ForeignRemovalWhileRemoveIntentOpen(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	require.NoError(t, f.deliverPost(postStandEvent("update", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev,
		postFirehoseExternalRemovalCID, removalRecordBody(post, "off-topic", postDBRemovalReason, postFirehoseExternalCreatedAt))))

	assert.Equal(t, []storedPostAction{externalPostAction("remove", post.URI, postDBReasonRuleViolation)}, f.postActions(t),
		"one actorless action; none names M")
	assert.Equal(t, 1, f.openIntents(t), "M's intent stays open")
	if code := f.admission(t, post.URI).DecisionCode; assert.NotNil(t, code) {
		assert.Equal(t, "off-topic", *code, "the foreign removal's code stands")
	}
}

// Review r1 #4 follow-up: M's restore commits at head two and the response
// is lost; the commit's removal delete is indexed (an inherited restore
// awaiting M's intent), then M's same-key retry finds the commit on the
// community repo before the acceptance copy arrives. The one restore action
// names M, and the acceptance copy then accepts the post with no further
// action.
func TestCommunityPostFirehoseDB_LostRestoreRetryAfterRemovalDelete(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	acceptanceCID := recordCIDOf(t, acceptanceRecordBody(post, postDBIntentCreatedAt))
	f.setAcceptanceWriteCID(post, acceptanceCID)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev, "", nil)))

	result, err := f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")

	require.NoError(t, err, "the same-key retry settles the lost restore")
	require.NotNil(t, result, "the retry returns its result")
	want := []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
	}
	assert.Equal(t, want, f.postActions(t), "the awaiting restore action names M")
	assert.Equal(t, 0, f.openIntents(t), "no intent is left open")
	assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status,
		"the post is accepted before the retry returns")
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the restore commit advanced the version once")
	assert.EqualValues(t, 3, result.Version, "the retry's result carries the version")

	require.NoError(t, f.deliverPost(postStandEvent("create", posts.AcceptanceCollection, post.SubjectRkey, rosterHeadTwoRev,
		acceptanceCID, acceptanceRecordBody(post, postDBIntentCreatedAt))))
	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev, "", nil)))

	assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "the copies leave the post accepted")
	assert.Equal(t, want, f.postActions(t), "the copies add no action")
	assert.EqualValues(t, 3, f.postVersion(t, post.URI), "the copies leave the version")
	replayed, err := f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	require.NoError(t, err, "the same key replays its stored result")
	require.NotNil(t, replayed)
	assert.EqualValues(t, 3, replayed.Version, "the stored result's version is the real version")
}

// Review r2 #4: M restores a doxing removal at head two and the response is
// lost. A newer spam removal written outside Coves at head three is indexed;
// then the acceptance copy of M's restore arrives (before its removal-delete
// copy). M's restore is logged with the reason of the removal it deleted
// (doxing), not the spam removal now indexed, and the spam removal stands.
func TestCommunityPostFirehoseDB_DelayedRestoreCopyAfterNewerRemoval(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "doxing", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	acceptanceCID := recordCIDOf(t, acceptanceRecordBody(post, postDBIntentCreatedAt))
	f.setAcceptanceWriteCID(post, acceptanceCID)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	f.standExternalRemoval(t, post, pds.LatestCommit{CID: postFirehoseHeadThreeCID, Rev: rosterHeadThreeRev},
		postFirehoseExternalRemovalCID, removalRecordBody(post, "spam", postDBRemovalReason, postFirehoseExternalCreatedAt))
	require.NoError(t, f.deliverPost(postStandEvent("create", posts.AcceptanceCollection, post.SubjectRkey, rosterHeadTwoRev,
		acceptanceCID, acceptanceRecordBody(post, postDBIntentCreatedAt))))

	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonDoxing),
		externalPostAction("remove", post.URI, postDBReasonSpam),
		localPostAction("restore", rosterModerator, false, post.URI, postDBReasonDoxing),
	}, f.postActions(t), "M's restore carries the deleted removal's reason")
	assert.Equal(t, 0, f.openIntents(t), "M's restore intent is completed")
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status, "the newer removal stands")
	if assert.NotNil(t, admission.DecisionCode) {
		assert.Equal(t, "spam", *admission.DecisionCode, "the newer removal's code stands")
	}
}

// Review r2 #2: M's removal commits at head one and the response is lost. An
// external rewrite at head two and an external restore at head three are
// indexed; then M's removal copy at head one arrives. M's removal is logged,
// but the index holds no removal the repo does not hold, and a later
// moderator's removal is not refused as a state conflict.
func TestCommunityPostFirehoseDB_LateRemovalCopyAfterExternalRestore(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	f.setRemovalWriteCID(post, postDBAlphaRemovalBodyCID)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	rewrite := removalRecordBody(post, "off-topic", postDBRemovalReason, postFirehoseExternalCreatedAt)
	f.repo.setPostRecord(posts.RemovalCollection, post.SubjectRkey, postFirehoseExternalRemovalCID, rewrite)
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	require.NoError(t, f.deliverPost(postStandEvent("update", posts.RemovalCollection, post.SubjectRkey, rosterHeadTwoRev,
		postFirehoseExternalRemovalCID, rewrite)))
	f.repo.mu.Lock()
	delete(f.repo.records, posts.RemovalCollection+"/"+post.SubjectRkey)
	f.repo.mu.Unlock()
	f.repo.setPostRecord(posts.AcceptanceCollection, post.SubjectRkey, postFirehoseExternalAcceptanceCID,
		acceptanceRecordBody(post, postDBIntentCreatedAt))
	f.repo.landCommit(pds.LatestCommit{CID: postFirehoseHeadThreeCID, Rev: rosterHeadThreeRev})
	f.deliverRestoreCommitIn(t, rosterCommunity, post, rosterHeadThreeRev, postFirehoseExternalAcceptanceCID, false)
	require.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "fixture: the external restore is indexed")

	f.deliverRemovalCommit(t, post, rosterHeadOneRev, postDBAlphaRemovalBodyCID,
		removalRecordBody(post, "spam", postDBRemovalReason, postDBIntentCreatedAt))

	assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "the late copy leaves the post accepted")
	assert.Empty(t, f.indexedRemovalCID(t, post.URI), "the index holds no removal the repo does not hold")
	var indexed int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM community_post_removal_records WHERE community_did = $1 AND post_uri = $2`,
		rosterCommunity, post.URI).Scan(&indexed))
	assert.Zero(t, indexed, "no removal identity is indexed")
	assert.Contains(t, f.postActions(t), localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		"M's removal is logged")

	f.setRemovalWriteCID(post, postDBAlpha.RemovalWrittenCID)
	f.setCommitHead(pds.LatestCommit{CID: postFirehoseHeadFourCID, Rev: postFirehoseHeadFourRev})
	_, err = f.removePost(rosterSecondModerator, post.URI, "spam", postDBRemovalReason, f.postVersion(t, post.URI), "remove-post-2")
	assert.NoError(t, err, "a later moderator's removal is not refused")
}

// Review r2 #3: M restores at head two and the response is lost. A removal
// written outside Coves at head three and its withdrawal at head four are
// indexed; then the acceptance copy of M's restore arrives. The withdrawal
// stays actorless, and M's restore is logged as its own action with the
// reason of the removal M deleted.
func TestCommunityPostFirehoseDB_DelayedRestoreCopyAfterExternalRemovalAndWithdrawal(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	_, err := f.removePost(rosterModerator, post.URI, "spam", postDBRemovalReason, postDBSeededVersion, "remove-post-1")
	require.NoError(t, err)
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	acceptanceCID := recordCIDOf(t, acceptanceRecordBody(post, postDBIntentCreatedAt))
	f.setAcceptanceWriteCID(post, acceptanceCID)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err = f.restorePost(rosterModerator, post.URI, 2, "restore-post-1")
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	f.standExternalRemoval(t, post, pds.LatestCommit{CID: postFirehoseHeadThreeCID, Rev: rosterHeadThreeRev},
		postFirehoseExternalRemovalCID, removalRecordBody(post, "off-topic", postDBRemovalReason, postFirehoseExternalCreatedAt))
	f.repo.mu.Lock()
	delete(f.repo.records, posts.RemovalCollection+"/"+post.SubjectRkey)
	f.repo.mu.Unlock()
	f.repo.landCommit(pds.LatestCommit{CID: postFirehoseHeadFourCID, Rev: postFirehoseHeadFourRev})
	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.RemovalCollection, post.SubjectRkey, postFirehoseHeadFourRev, "", nil)))

	require.NoError(t, f.deliverPost(postStandEvent("create", posts.AcceptanceCollection, post.SubjectRkey, rosterHeadTwoRev,
		acceptanceCID, acceptanceRecordBody(post, postDBIntentCreatedAt))))

	assert.Equal(t, []storedPostAction{
		localPostAction("remove", rosterModerator, false, post.URI, postDBReasonSpam),
		externalPostAction("remove", post.URI, postDBReasonRuleViolation),
		externalPostAction("restore", post.URI, postDBReasonRuleViolation),
		localPostAction("restore", rosterModerator, false, post.URI, postDBReasonSpam),
	}, f.postActions(t), "the external withdrawal stays actorless; M's restore is its own action")
	assert.Equal(t, 0, f.openIntents(t), "M's restore intent is completed")
}
