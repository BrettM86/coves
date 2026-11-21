//go:build integration

package moderation_test

import (
	"context"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// modlogInvitee is I, who accepts O's moderator invitation.
	modlogInvitee           = "did:plc:modloginvitee"
	modlogInviteeWrittenCID = "bafyreiamodloginviteewrittenaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	modlogHeadSevenRev = "3krosterrev07"
	modlogHeadEightRev = "3krosterrev08"
)

// modlogEntry is one expected public listActions entry, matched by action and
// subject URI.
type modlogEntry struct {
	action, subjectURI string
	actor              map[string]any
	scope              map[string]any
	role               string // "" for a non-roster entry
}

// Chunk 13 B4: in the hosted community C (rosteralpha), every kind of
// community authority action recorded through its real path, plus one
// instance admin removal of a post in C, lists exactly once in the public log
// filtered to C, with its actor, action, scope kind and, on roster entries,
// the role. None of them is admin-acting.
func TestCommunityModlog_ListsEveryCommunityAuthorityActionOnce(t *testing.T) {
	t.Parallel()
	f := newCommunityBanDBFixture(t)
	ctx := context.Background()

	removedPost := f.seedAcceptedPost(t, postDBSeedFor("3kmodlogpost1"))
	instanceRemovedPost := f.seedAcceptedPost(t, postDBSeedFor("3kmodloginst1"))
	thread := f.seedThread(t)
	// R (M2)'s role record stands in the community repo with its rev gate at
	// head zero, so O can remove it.
	f.repo.mu.Lock()
	f.repo.setRecord(communities.RoleCollection, rosterSecondModeratorKey, rosterSecondModeratorCID)
	f.repo.writeCIDs[communities.RoleCollection+"/"+communities.RoleRecordKey(modlogInvitee)] = modlogInviteeWrittenCID
	f.repo.mu.Unlock()
	_, err := f.db.ExecContext(ctx, `INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)`,
		rosterSecondModeratorURI, rosterHeadZeroRev)
	require.NoError(t, err)

	// M removes a post.
	_, err = f.removePost(rosterModerator, removedPost.URI, "spam", postDBRemovalReason, postDBSeededVersion, "modlog-remove-post")
	require.NoError(t, err, "M removes the post")

	// M removes, then restores, a comment.
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	_, err = f.removeComment(rosterModerator, thread.Comment.URI, "spam", commentRemovalReason, 0, "modlog-remove-comment")
	require.NoError(t, err, "M removes the comment")
	f.setCommitHead(pds.LatestCommit{CID: commentDBHeadThreeCID, Rev: rosterHeadThreeRev})
	_, err = f.restoreComment(rosterModerator, thread.Comment.URI, 1, "modlog-restore-comment")
	require.NoError(t, err, "M restores the comment")

	// M bans B.
	f.setCommitHead(pds.LatestCommit{CID: postFirehoseHeadFourCID, Rev: postFirehoseHeadFourRev})
	expiry := banDBFarExpiry
	_, err = f.ban(rosterModerator, banSubject, banReason, &expiry, 0, "modlog-ban")
	require.NoError(t, err, "M bans B")

	// I accepts O's moderator invitation.
	f.setCommitHead(pds.LatestCommit{CID: "bafyreiamodlogheadfiveaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: rosterHeadFiveRev})
	f.seedInvitation(t, rosterInvitation(rosterModeratorInvitationID, modlogInvitee, communities.RoleModerator, rosterOwner, false))
	_, err = f.acceptInvitation(modlogInvitee, rosterModeratorInvitationID, rosterStartVersion, "modlog-accept")
	require.NoError(t, err, "I accepts the moderator invitation")

	// O removes R.
	f.setCommitHead(pds.LatestCommit{CID: "bafyreiamodlogheadsixaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: rosterHeadSixRev})
	_, err = f.service.RemoveModerator(ctx, moderation.RemoveModeratorRequest{
		ActorDID: rosterOwner, CommunityDID: rosterCommunity, SubjectDID: rosterSecondModerator,
		ExpectedVersion: rosterStartVersion + 1, IdempotencyKey: "modlog-remove-role",
	})
	require.NoError(t, err, "O removes R")

	// O transfers the ownership to T (M): O's offer, accepted by M.
	f.setCommitHead(pds.LatestCommit{CID: "bafyreiamodlogheadsevenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: modlogHeadSevenRev})
	f.seedInvitation(t, rosterInvitation(rosterOwnershipOfferID, rosterModerator, communities.RoleOwner, rosterOwner, false))
	_, err = f.acceptInvitation(rosterModerator, rosterOwnershipOfferID, rosterStartVersion+2, "modlog-transfer")
	require.NoError(t, err, "M accepts O's ownership offer")

	// An instance admin removes another post in C with instance authority.
	f.setCommitHead(pds.LatestCommit{CID: "bafyreiamodlogheadeightaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: modlogHeadEightRev})
	f.instanceModeration(t).removeContent(t, instanceRemovedPost, "modlog-instance-remove")

	entries := publicModlog(t, f.db, moderation.ListActionsParams{Community: "did:plc:rosteralpha"})

	moderatorM := map[string]any{"did": "did:plc:troublemaker456"}
	ownerO := map[string]any{"did": "did:plc:rosterowner"}
	communityScope := map[string]any{"kind": "community", "communityDid": "did:plc:rosteralpha"}
	want := []modlogEntry{
		{
			action: "remove", subjectURI: "at://did:plc:rosterauthor/social.coves.community.postv2/3kmodlogpost1",
			actor: moderatorM, scope: communityScope,
		},
		{
			action: "apply-removal", subjectURI: "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentalpha1",
			actor: moderatorM, scope: communityScope,
		},
		{
			action: "retract-removal", subjectURI: "at://did:plc:rosterauthor/social.coves.community.comment/3kcommentalpha1",
			actor: moderatorM, scope: communityScope,
		},
		{action: "remove", subjectURI: "at://did:plc:rosteroutsider", actor: moderatorM, scope: communityScope},
		// An accepted invitation's grant names the inviter, O.
		{
			action: "grant-role", subjectURI: "at://did:plc:modloginvitee", actor: ownerO, scope: communityScope,
			role: "moderator",
		},
		{
			action: "remove-role", subjectURI: "at://did:plc:rostermodtwo", actor: ownerO, scope: communityScope,
			role: "moderator",
		},
		{
			action: "transfer-ownership", subjectURI: "at://did:plc:troublemaker456", actor: ownerO, scope: communityScope,
			role: "owner",
		},
		{
			action: "remove", subjectURI: "at://did:plc:rosterauthor/social.coves.community.postv2/3kmodloginst1",
			actor: map[string]any{"did": "did:plc:rosteradmin"}, scope: map[string]any{"kind": "instance"},
		},
	}

	assert.Len(t, entries, len(want), "the public log lists each action once and nothing else")
	for _, expected := range want {
		matches := []map[string]any{}
		for _, entry := range entries {
			subject, _ := entry["subject"].(map[string]any)
			if entry["action"] == expected.action && subject != nil && subject["uri"] == expected.subjectURI {
				matches = append(matches, entry)
			}
		}
		if !assert.Lenf(t, matches, 1, "%s of %s is listed exactly once", expected.action, expected.subjectURI) {
			continue
		}
		entry := matches[0]
		assert.Equalf(t, expected.actor, entry["actor"], "%s of %s names its actor", expected.action, expected.subjectURI)
		assert.Equalf(t, expected.scope, entry["scope"], "%s of %s scope", expected.action, expected.subjectURI)
		if expected.role != "" {
			assert.Equalf(t, expected.role, entry["role"], "%s of %s names the role", expected.action, expected.subjectURI)
		} else {
			assert.NotContainsf(t, entry, "role", "%s of %s has no role", expected.action, expected.subjectURI)
		}
		assert.NotContainsf(t, entry, "adminActing", "%s of %s is not admin-acting", expected.action, expected.subjectURI)
	}
}
