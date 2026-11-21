//go:build integration

package moderation_test

import (
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community removal and an instance removal of the same post are
// independent decisions. The community's moderators act through the community
// action service; the instance admin acts with instance authority through the
// instance moderation service, whose migration-080 outbox mirrors the removal
// into the community's repo. Both write into one fake community repo over one
// database; outcomes are what the author is served, the community feed, the
// admission row and the records standing in the community repo.

const (
	coexistInstanceRemovalCID  = "bafyreicoexistinstanceremovalaaaaaaaaaaaaaaaaaaaaaaaaaa"
	coexistRewrittenRemovalCID = "bafyreicoexistrewrittenremovalaaaaaaaaaaaaaaaaaaaaaaaaa"
	coexistOffTopicReason      = "Self-promotion outside the weekly thread"
)

// Heads the fake community repo's successive batches commit at.
var (
	coexistHeadOne   = pds.LatestCommit{CID: "bafyreicoexistheadoneaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: "3krosterrev11"}
	coexistHeadTwo   = pds.LatestCommit{CID: "bafyreicoexistheadtwoaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: "3krosterrev12"}
	coexistHeadThree = pds.LatestCommit{CID: "bafyreicoexistheadthreeaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: "3krosterrev13"}
	coexistHeadFour  = pds.LatestCommit{CID: "bafyreicoexistheadfouraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: "3krosterrev14"}
)

// assertAuthorSeesInstanceRemoval: while the instance removal is active the
// author's post.get is the #moderatedPost tombstone, and the community feed
// does not list the post.
func (f *communityPostDBFixture) assertAuthorSeesInstanceRemoval(t *testing.T, post seededCommunityPost, when string) {
	t.Helper()
	result := f.authorPostGet(t, post.URI)
	assert.Nilf(t, result.Post, "%s: the author is not served the post while the instance removal is active", when)
	assert.NotNilf(t, result.Moderated, "%s: the author's post.get is #moderatedPost while the instance removal is active", when)
	assert.NotContainsf(t, f.feedURIs(t), post.URI, "%s: the community feed excludes the post", when)
}

// assertServedAfterBothRestores: neither removal applies; the author is served
// the accepted post and the community feed lists it.
func (f *communityPostDBFixture) assertServedAfterBothRestores(t *testing.T, post seededCommunityPost) {
	t.Helper()
	result := f.authorPostGet(t, post.URI)
	assert.Nil(t, result.Moderated, "the instance removal no longer applies")
	if assert.NotNil(t, result.Post, "the post is served again") {
		assert.Equal(t, string(posts.AdmissionStatusAccepted), result.Post.Status)
		assert.Nil(t, result.Post.CommunityRemoval)
	}
	assert.Contains(t, f.feedURIs(t), post.URI, "the community feed serves the post again")
	assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status)
}

// assertAcceptancePinsCurrentCID: an acceptance written by the community
// restore stands in the community repo, pinned to the post's current CID.
func (f *communityPostDBFixture) assertAcceptancePinsCurrentCID(t *testing.T, post seededCommunityPost) {
	t.Helper()
	acceptance := f.standRecord(posts.AcceptanceCollection, post.SubjectRkey)
	if assert.NotNil(t, acceptance, "an acceptance stands in the community repo") {
		assert.Equal(t, postAcceptanceWrittenCID, acceptance.CID, "the acceptance the restore wrote")
		assert.Equal(t, map[string]any{"uri": post.URI, "cid": postAlphaCID}, acceptance.Value["subject"],
			"pinned to the post's current CID")
	}
}

// I12: community removal and instance removal of one post, in either order,
// then one of them restored. Restoring either leaves the other in force: an
// instance restore leaves the community's removal standing (admission removed,
// the moderator's removal record in the community repo, the author's view
// carrying the community's code and reason); a community restore leaves the
// post hidden by the instance decision until the admin restores it too, and
// that instance restore writes nothing into the community repo.
func TestCommunityPostActionsDB_CommunityAndInstanceRemovalsAreIndependent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		instanceRemovesFirst bool
		communityRestores    bool // the community restores first; else only the instance restores
		// sameCode: over the outbox's record, the moderator removes with its
		// code (spam) and no reason instead of rewriting to off-topic.
		sameCode bool
	}{
		{name: "community removes first, instance restores", instanceRemovesFirst: false, communityRestores: false},
		{name: "instance removes first, moderator rewrites, community restores first", instanceRemovesFirst: true, communityRestores: true},
		{name: "community removes first, community restores first", instanceRemovesFirst: false, communityRestores: true},
		{name: "instance removes first, moderator rewrites, instance restores", instanceRemovesFirst: true, communityRestores: false},
		{name: "instance removes first, moderator removes with its code and no reason, instance restores", instanceRemovesFirst: true, sameCode: true},
		{name: "instance removes first, moderator removes with its code and no reason, community restores first", instanceRemovesFirst: true, sameCode: true, communityRestores: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityPostDBFixture(t)
			post := f.seedAcceptedPost(t, postDBAlpha)
			instance := f.instanceModeration(t)

			// The community's removal: spam with a reason on the accepted post,
			// or, over the outbox's removal record, a rewrite to off-topic with
			// a reason.
			communityCode, communityReason, communityRemovalCID := "spam", postDBRemovalReason, postRemovalWrittenCID
			switch {
			case tt.instanceRemovesFirst && tt.sameCode:
				communityReason, communityRemovalCID = "", coexistRewrittenRemovalCID
			case tt.instanceRemovesFirst:
				communityCode, communityReason, communityRemovalCID = "off-topic", coexistOffTopicReason, coexistRewrittenRemovalCID
			}
			communityRemoves := func(head pds.LatestCommit) {
				t.Helper()
				f.setCommitHead(head)
				f.setRemovalWriteCID(post, communityRemovalCID)
				versionBefore := f.postVersion(t, post.URI)
				result, err := f.removePost(rosterModerator, post.URI, communityCode, communityReason,
					versionBefore, "coexist-community-remove")
				require.NoError(t, err)
				require.NotNil(t, result, "the moderator's removal returns its result")
				assert.False(t, result.AlreadyApplied, "the moderator's removal is the community's own decision")
				assert.Contains(t, f.postActions(t),
					localPostAction("remove", rosterModerator, false, post.URI, moderation.CommunityRemovalActionReason(communityCode)),
					"the moderator's removal is logged")
				removal := f.standRecord(posts.RemovalCollection, post.SubjectRkey)
				require.NotNil(t, removal, "the moderator's removal record stands in the community repo")
				require.Equal(t, communityRemovalCID, removal.CID, "the standing removal is the moderator's")
				assert.Equal(t, communityCode, removal.Value["code"], "the removal record carries the moderator's code")
				if communityReason == "" {
					assert.NotContains(t, removal.Value, "reason", "the removal record carries no reason")
				} else {
					assert.Equal(t, communityReason, removal.Value["reason"], "the removal record carries the moderator's reason")
				}
				assert.Equal(t, versionBefore+1, f.postVersion(t, post.URI),
					"the moderator's removal record advances the post's version once")
				if tt.sameCode {
					// The takeover's firehose copy is the change already counted.
					f.deliverRemovalCommit(t, post, head.Rev, removal.CID, removal.Value)
					assert.Equal(t, versionBefore+1, f.postVersion(t, post.URI),
						"the firehose copy of the moderator's removal does not advance the version again")
				}
			}

			var adminRemoval *moderation.Action
			if tt.instanceRemovesFirst {
				f.setCommitHead(coexistHeadOne)
				f.setRemovalWriteCID(post, coexistInstanceRemovalCID)
				adminRemoval = instance.removeContent(t, post, "coexist-instance-remove")
				outboxRemoval := f.standRecord(posts.RemovalCollection, post.SubjectRkey)
				require.NotNil(t, outboxRemoval, "fixture: the outbox wrote the community's removal record")
				require.Equal(t, coexistInstanceRemovalCID, outboxRemoval.CID, "fixture: the outbox's removal record")
				require.Nil(t, f.standRecord(posts.AcceptanceCollection, post.SubjectRkey), "fixture: the outbox withdrew the acceptance")
				communityRemoves(coexistHeadTwo)
			} else {
				communityRemoves(coexistHeadOne)
				f.setCommitHead(coexistHeadTwo)
				adminRemoval = instance.removeContent(t, post, "coexist-instance-remove")
				removal := f.standRecord(posts.RemovalCollection, post.SubjectRkey)
				require.NotNil(t, removal)
				require.Equal(t, communityRemovalCID, removal.CID, "the outbox leaves the moderator's removal alone")
			}
			f.assertAuthorSeesInstanceRemoval(t, post, "both removals active")

			if !tt.communityRestores {
				instance.restoreContent(t, post, adminRemoval, "coexist-instance-restore")
				instance.drain(t)

				admission := f.admission(t, post.URI)
				assert.Equal(t, posts.AdmissionStatusRemoved, admission.Status, "the community's removal still stands")
				if assert.NotNil(t, admission.DecisionCode) {
					assert.Equal(t, communityCode, *admission.DecisionCode, "with the moderator's code")
				}
				removal := f.standRecord(posts.RemovalCollection, post.SubjectRkey)
				if assert.NotNil(t, removal, "the moderator's removal record still stands in the community repo") {
					assert.Equal(t, communityRemovalCID, removal.CID)
				}
				assert.Nil(t, f.standRecord(posts.AcceptanceCollection, post.SubjectRkey), "no acceptance was written back")
				result := f.authorPostGet(t, post.URI)
				assert.Nil(t, result.Moderated, "the instance removal no longer applies")
				if assert.NotNil(t, result.Post, "the author reads their community-removed post") {
					assert.Equal(t, string(posts.AdmissionStatusRemoved), result.Post.Status)
					assert.Equal(t, &posts.CommunityRemoval{Code: communityCode, Reason: communityReason}, result.Post.CommunityRemoval,
						"the author's view carries the community's removal")
				}
				assert.NotContains(t, f.feedURIs(t), post.URI, "the community feed still excludes the post")
				return
			}

			f.setCommitHead(coexistHeadThree)
			f.setAcceptanceWriteCID(post, postAcceptanceWrittenCID)
			restored, err := f.restorePost(rosterModerator, post.URI, f.postVersion(t, post.URI), "coexist-community-restore")
			require.NoError(t, err)
			require.NotNil(t, restored, "the moderator's restore returns its result")
			assert.Nil(t, f.standRecord(posts.RemovalCollection, post.SubjectRkey), "the community's removal record is gone")
			f.assertAcceptancePinsCurrentCID(t, post)
			assert.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "accepted at the community level")
			f.assertAuthorSeesInstanceRemoval(t, post, "community restored, instance removal active")

			batchesBefore := len(f.repo.batchesSnapshot())
			f.setCommitHead(coexistHeadFour)
			instance.restoreContent(t, post, adminRemoval, "coexist-instance-restore")
			instance.drain(t)

			assert.Len(t, f.repo.batchesSnapshot(), batchesBefore, "the instance restore writes nothing into the community repo")
			f.assertAcceptancePinsCurrentCID(t, post)
			f.assertServedAfterBothRestores(t, post)
		})
	}
}

// I13: the outbox wrote the community's removal record for an instance
// removal (withdrawing the acceptance). A moderator restores the post with
// community authority: the outbox's removal record is deleted and an
// acceptance pinned to the post's current CID stands, the admission is
// accepted, yet the instance decision still hides the post. The admin's later
// instance restore finds no removal it owns: once the outbox has drained the
// community repo received no batch for it, and the post is served.
func TestCommunityPostActionsDB_ModeratorRestoresAnOutboxWrittenRemoval(t *testing.T) {
	t.Parallel()
	f := newCommunityPostDBFixture(t)
	post := f.seedAcceptedPost(t, postDBAlpha)
	instance := f.instanceModeration(t)

	f.setCommitHead(coexistHeadOne)
	f.setRemovalWriteCID(post, coexistInstanceRemovalCID)
	adminRemoval := instance.removeContent(t, post, "outbox-instance-remove")
	outboxRemoval := f.standRecord(posts.RemovalCollection, post.SubjectRkey)
	require.NotNil(t, outboxRemoval, "fixture: the outbox wrote the community's removal record")
	require.Equal(t, coexistInstanceRemovalCID, outboxRemoval.CID, "fixture: the outbox's removal record")
	require.Nil(t, f.standRecord(posts.AcceptanceCollection, post.SubjectRkey), "fixture: the outbox withdrew the acceptance")
	require.Equal(t, posts.AdmissionStatusRemoved, f.admission(t, post.URI).Status, "fixture: the outbox stamped its removal")

	f.setCommitHead(coexistHeadTwo)
	f.setAcceptanceWriteCID(post, postAcceptanceWrittenCID)
	restored, err := f.restorePost(rosterModerator, post.URI, f.postVersion(t, post.URI), "outbox-community-restore")

	require.NoError(t, err)
	require.NotNil(t, restored, "the moderator's restore returns its result")
	assert.Nil(t, f.standRecord(posts.RemovalCollection, post.SubjectRkey), "the outbox-written removal record is deleted")
	f.assertAcceptancePinsCurrentCID(t, post)
	admission := f.admission(t, post.URI)
	assert.Equal(t, posts.AdmissionStatusAccepted, admission.Status, "accepted at the community level")
	if assert.NotNil(t, admission.AcceptedCID) {
		assert.Equal(t, postAlphaCID, *admission.AcceptedCID, "pinned to the post's current CID")
	}
	f.assertAuthorSeesInstanceRemoval(t, post, "community restored, instance removal active")

	batchesBefore := len(f.repo.batchesSnapshot())
	f.setCommitHead(coexistHeadThree)
	instance.restoreContent(t, post, adminRemoval, "outbox-instance-restore")
	instance.drain(t)

	assert.Len(t, f.repo.batchesSnapshot(), batchesBefore, "the outbox writes and deletes nothing for the instance restore")
	assert.Nil(t, f.standRecord(posts.RemovalCollection, post.SubjectRkey), "no removal is recreated")
	f.assertAcceptancePinsCurrentCID(t, post)
	f.assertServedAfterBothRestores(t, post)
}
