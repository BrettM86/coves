//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityModeratorErasureDeletesTheRoleRecord is the production wiring
// of an erased moderator's record cleanup (B10): a founder creates a community
// the AppView hosts, invites a second account M, and M accepts, so the
// AppView writes M's moderator record to the community's repo. M then deletes
// its Coves account. The record is gone from the community's repo within the
// contract budget (45 s). A periodic sweep tick can land inside that budget,
// so this does not by itself prove the after-commit trigger;
// TestBuildServicesGivesTheUserServiceTheRoleRecordDeleteQueueAsItsErasureFollowUp
// in cmd/server pins that wiring.
//
// The community is created through community.create and its password is never
// reset: the AppView must keep its own credentials to write as the community.
func TestCommunityModeratorErasureDeletesTheRoleRecord(t *testing.T) {
	p := newPipeline(t)
	founder := p.IndexedAccount(t, "efo")
	moderator := p.IndexedAccount(t, "emo")
	founderClient := p.AppView.As(p.AppView.SignIn(t, founder))
	ctx, cancel := context.WithTimeout(context.Background(), 4*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, founderClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "er"), "description": "a community whose moderator leaves Coves",
	}, &created), "the founder creates a community through the AppView, which hosts it")
	require.NotEmpty(t, created.DID)
	communityDID := created.DID

	// moderatorRecordCount is the number of moderator records in the
	// community's repo naming subjectDID, read from the PDS.
	moderatorRecordCount := func(subjectDID string) (int, error) {
		var listing struct {
			Records []struct {
				Value map[string]any `json:"value"`
			} `json:"records"`
		}
		if err := p.PDS.Anon.Query(ctx, "com.atproto.repo.listRecords", url.Values{
			"repo": {communityDID}, "collection": {moderatorCollection}, "limit": {"100"},
		}, &listing); err != nil {
			return 0, err
		}
		count := 0
		for _, record := range listing.Records {
			if record.Value["subject"] == subjectDID {
				count++
			}
		}
		return count, nil
	}

	var state struct {
		Version int64 `json:"version"`
	}
	require.NoError(t, founderClient.Query(ctx, "social.coves.moderation.getCommunityState",
		url.Values{"subject": {communityDID}, "community": {communityDID}}, &state))
	var invited struct {
		Invitation struct {
			ID string `json:"id"`
		} `json:"invitation"`
		Version int64 `json:"version"`
	}
	require.NoError(t, founderClient.Procedure(ctx, "social.coves.community.inviteModerator", map[string]any{
		"community": communityDID, "subject": moderator.DID,
		"expectedVersion": state.Version, "idempotencyKey": testkit.UniqueID(t),
	}, &invited), "the founder invites M")
	require.NotEmpty(t, invited.Invitation.ID)

	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	var inbox struct {
		Invitations []struct {
			Invitation struct {
				ID string `json:"id"`
			} `json:"invitation"`
			RosterVersion int64 `json:"rosterVersion"`
		} `json:"invitations"`
	}
	require.NoError(t, moderatorClient.Query(ctx, "social.coves.community.listViewerInvitations", url.Values{}, &inbox))
	require.Len(t, inbox.Invitations, 1, "M's inbox holds the one pending invitation")
	require.Equal(t, invited.Invitation.ID, inbox.Invitations[0].Invitation.ID)
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.acceptInvitation", map[string]any{
		"invitation": invited.Invitation.ID, "expectedVersion": inbox.Invitations[0].RosterVersion,
		"idempotencyKey": testkit.UniqueID(t),
	}, nil), "M accepts")

	p.Await(t, "M's moderator record to be in the community's repo", func() (bool, error) {
		count, err := moderatorRecordCount(moderator.DID)
		return count == 1, err
	})

	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.actor.deleteAccount", nil, nil),
		"M deletes its Coves account")

	p.Await(t, "M's moderator record to be deleted from the community's repo after the erasure", func() (bool, error) {
		count, err := moderatorRecordCount(moderator.DID)
		return count == 0, err
	})
}
