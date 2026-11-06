//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/stretchr/testify/require"
)

const (
	removeCommunityMethod  = "social.coves.moderation.removeCommunity"
	restoreCommunityMethod = "social.coves.moderation.restoreCommunity"
)

// communityRemovalMutation is the part of a removeCommunity/restoreCommunity
// mutation result the contract reads.
type communityRemovalMutation struct {
	Outcome string `json:"outcome"`
	State   struct {
		Version string `json:"version"`
	} `json:"state"`
}

// TestCommunityRemovalContract follows an instance community removal from a
// real admin OAuth session to a member's next writes and to an independent
// label consumer:
//
//	remove   → the admin removes the community; community.get serves
//	           status "removed"; the member's post.create in it, and their
//	           comment.create and vote.create on a post in it, all answer
//	           403 CommunityRemoved; queryLabels on the community's bare DID
//	           serves a !takedown from the instance that verifies with the
//	           resolved #atproto_label key
//	restore  → the admin restores it; community.get serves no status,
//	           post.create is accepted, and queryLabels serves the negation
//
// The comment and vote refusals pin the production wiring of the community
// removal reader into the comment and vote services in cmd/server: only a
// request to the running router sees whether it was handed over.
func TestCommunityRemovalContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "crc")
	community := indexedCommunity(t, p, "crm", owner.DID)
	member := p.IndexedAccount(t, "crb")
	memberView := p.AppView.As(p.AppView.SignIn(t, member))
	target := indexedPost(t, p, community, member, "a post in a community the instance will remove")
	admin := testkit.ModerationAdmin(t, 1)
	adminView := p.AppView.As(p.AppView.SignIn(t, admin))

	communityStatus := func() (any, bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		var response map[string]any
		require.NoError(t, p.AppView.Query(ctx, "social.coves.community.get",
			url.Values{"community": {community.DID}}, &response),
			"community.get must answer for an indexed community")
		require.Equal(t, community.DID, response["did"])
		status, present := response["status"]
		return status, present
	}

	_, present := communityStatus()
	require.False(t, present, "a community in good standing serves no status")
	require.NoError(t, submitPost(memberView, community.DID, "a post before the removal "+testkit.UniqueID(t)),
		"the member posts in the community before the removal")

	var removed communityRemovalMutation
	err := adminView.Procedure(t.Context(), removeCommunityMethod, map[string]any{
		"subject":         community.DID,
		"expectedVersion": "v0",
		"idempotencyKey":  testkit.UniqueID(t),
		"reason":          "social.coves.moderation.defs#reasonSpam",
	}, &removed)
	require.NoError(t, err, "the instance admin removes the community")
	require.Equal(t, "applied", removed.Outcome)
	require.Equal(t, "v1", removed.State.Version)

	status, present := communityStatus()
	require.True(t, present, "a removed community's community.get serves a status")
	require.Equal(t, "removed", status)

	err = submitPost(memberView, community.DID, "a post in a removed community "+testkit.UniqueID(t))
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "CommunityRemoved", "post.create in a removed community")

	err = memberView.Procedure(t.Context(), "social.coves.community.comment.create", map[string]any{
		"reply": map[string]any{
			"root":   map[string]any{"uri": target.URI, "cid": target.CID},
			"parent": map[string]any{"uri": target.URI, "cid": target.CID},
		},
		"content": "a reply in a removed community",
	}, nil)
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "CommunityRemoved", "comment.create on a post in a removed community")

	err = memberView.Procedure(t.Context(), "social.coves.feed.vote.create", map[string]any{
		"subject":   map[string]any{"uri": target.URI, "cid": target.CID},
		"direction": "up",
	}, nil)
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "CommunityRemoved", "vote.create on a post in a removed community")

	labelKey, endpoint := resolveInstanceLabeler(t)
	communityTakedowns := func(negation bool) []*comatproto.LabelDefs_Label {
		t.Helper()
		var matching []*comatproto.LabelDefs_Label
		for _, label := range queryLabels(t, endpoint, community.DID) {
			isNegation := label.Neg != nil && *label.Neg
			if label.Val == "!takedown" && isNegation == negation {
				matching = append(matching, label)
			}
		}
		return matching
	}

	takedowns := communityTakedowns(false)
	require.Len(t, takedowns, 1, "exactly one !takedown label for the removed community")
	require.Equal(t, communityInstanceDID, takedowns[0].Src)
	require.Equal(t, community.DID, takedowns[0].Uri, "a community label's uri is the bare DID")
	require.Nil(t, takedowns[0].Cid, "a community label is not record-scoped")
	takedown := labeling.FromLexicon(takedowns[0])
	require.NoError(t, takedown.VerifySignature(labelKey),
		"the community label must verify with the advertised #atproto_label key")

	var restored communityRemovalMutation
	err = adminView.Procedure(t.Context(), restoreCommunityMethod, map[string]any{
		"subject":         community.DID,
		"expectedVersion": removed.State.Version,
		"idempotencyKey":  testkit.UniqueID(t) + "-restore",
		"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
	}, &restored)
	require.NoError(t, err, "the instance admin restores the community")
	require.Equal(t, "applied", restored.Outcome)
	require.Equal(t, "v2", restored.State.Version)

	_, present = communityStatus()
	require.False(t, present, "a restored community serves no status")

	require.NoError(t, submitPost(memberView, community.DID, "a post after the restore "+testkit.UniqueID(t)),
		"post.create must be accepted once the community is restored")

	negations := communityTakedowns(true)
	require.Len(t, negations, 1, "exactly one !takedown negation for the restored community")
	require.Equal(t, communityInstanceDID, negations[0].Src)
	require.Equal(t, community.DID, negations[0].Uri)
	negation := labeling.FromLexicon(negations[0])
	require.NoError(t, negation.VerifySignature(labelKey),
		"the negation must verify with the advertised #atproto_label key")
}
