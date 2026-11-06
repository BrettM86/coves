//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"Coves/tests/testkit"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/stretchr/testify/require"
)

const (
	banAccountMethod   = "social.coves.moderation.banAccount"
	unbanAccountMethod = "social.coves.moderation.unbanAccount"
)

// accountBanMutation is the part of a banAccount/unbanAccount mutation result
// the contract reads.
type accountBanMutation struct {
	Outcome string `json:"outcome"`
	State   struct {
		Version string `json:"version"`
	} `json:"state"`
}

// TestAccountBanContract follows an instance account ban from a real admin
// OAuth session to the banned account's next post and to an independent label
// consumer:
//
//	ban    → the admin bans the account; its post.create, comment.create (on
//	         another author's post) and vote.create (on that post) all answer
//	         403 Banned with the instance account-ban message, and queryLabels
//	         on the bare DID serves a !takedown from the instance that verifies
//	         with the resolved #atproto_label key
//	unban  → the admin lifts the ban; post.create is accepted and queryLabels
//	         serves the negation, also verifying
//
// The three refusals are the production wiring of each write path's account
// ban gate: cmd/server must hand the account-ban read to the post, comment and
// vote services, and only a request to the running router sees whether it did.
func TestAccountBanContract(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "abn")
	community := indexedCommunity(t, p, "abn", account.DID)
	other := p.IndexedAccount(t, "abo")
	accountView := p.AppView.As(p.AppView.SignIn(t, account))
	target := indexedPost(t, p, community, other, "a post the banned account will try to join in on")
	admin := testkit.ModerationAdmin(t, 1)
	adminView := p.AppView.As(p.AppView.SignIn(t, admin))

	require.NoError(t, submitPost(accountView, community.DID, "a post before the ban "+testkit.UniqueID(t)),
		"the account posts in good standing before the ban")

	var banned accountBanMutation
	err := adminView.Procedure(t.Context(), banAccountMethod, map[string]any{
		"subject":         account.DID,
		"expectedVersion": "v0",
		"idempotencyKey":  testkit.UniqueID(t),
		"reason":          "social.coves.moderation.defs#reasonSpam",
	}, &banned)
	require.NoError(t, err, "the instance admin bans the account")
	require.Equal(t, "applied", banned.Outcome)
	require.Equal(t, "v1", banned.State.Version)

	const accountBannedMessage = "Your account is banned on this instance"
	err = submitPost(accountView, community.DID, "a post from a banned account "+testkit.UniqueID(t))
	refusal := requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "a banned account's post.create")
	require.Equal(t, accountBannedMessage, refusal.XRPCMessage, "post.create names the instance account ban")

	err = accountView.Procedure(t.Context(), "social.coves.community.comment.create", map[string]any{
		"reply": map[string]any{
			"root":   map[string]any{"uri": target.URI, "cid": target.CID},
			"parent": map[string]any{"uri": target.URI, "cid": target.CID},
		},
		"content": "a reply from a banned account",
	}, nil)
	refusal = requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "a banned account's comment.create")
	require.Equal(t, accountBannedMessage, refusal.XRPCMessage, "comment.create names the instance account ban")

	err = accountView.Procedure(t.Context(), "social.coves.feed.vote.create", map[string]any{
		"subject":   map[string]any{"uri": target.URI, "cid": target.CID},
		"direction": "up",
	}, nil)
	refusal = requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "a banned account's vote.create")
	require.Equal(t, accountBannedMessage, refusal.XRPCMessage, "vote.create names the instance account ban")

	labelKey, endpoint := resolveInstanceLabeler(t)
	accountTakedowns := func(negation bool) []*comatproto.LabelDefs_Label {
		t.Helper()
		var matching []*comatproto.LabelDefs_Label
		for _, label := range queryLabels(t, endpoint, account.DID) {
			isNegation := label.Neg != nil && *label.Neg
			if label.Val == "!takedown" && isNegation == negation {
				matching = append(matching, label)
			}
		}
		return matching
	}

	takedowns := accountTakedowns(false)
	require.Len(t, takedowns, 1, "exactly one !takedown label for the banned account")
	require.Equal(t, communityInstanceDID, takedowns[0].Src)
	require.Equal(t, account.DID, takedowns[0].Uri, "an account label's uri is the bare DID")
	require.Nil(t, takedowns[0].Cid, "an account label is not record-scoped")
	require.Nil(t, takedowns[0].Exp, "a permanent ban's label has no expiry")
	takedown := labeling.FromLexicon(takedowns[0])
	require.NoError(t, takedown.VerifySignature(labelKey),
		"the account label must verify with the advertised #atproto_label key")

	var unbanned accountBanMutation
	err = adminView.Procedure(t.Context(), unbanAccountMethod, map[string]any{
		"subject":         account.DID,
		"expectedVersion": banned.State.Version,
		"idempotencyKey":  testkit.UniqueID(t) + "-unban",
		"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
	}, &unbanned)
	require.NoError(t, err, "the instance admin lifts the ban")
	require.Equal(t, "applied", unbanned.Outcome)
	require.Equal(t, "v2", unbanned.State.Version)

	require.NoError(t, submitPost(accountView, community.DID, "a post after the unban "+testkit.UniqueID(t)),
		"post.create must be accepted once the ban is lifted")

	negations := accountTakedowns(true)
	require.Len(t, negations, 1, "exactly one !takedown negation for the unbanned account")
	require.Equal(t, communityInstanceDID, negations[0].Src)
	require.Equal(t, account.DID, negations[0].Uri)
	negation := labeling.FromLexicon(negations[0])
	require.NoError(t, negation.VerifySignature(labelKey),
		"the negation must verify with the advertised #atproto_label key")
}
