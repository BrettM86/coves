//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The session contract: a sealed AppView session, minted by the real OAuth web
// login against the stack's PDS, is accepted by RequireAuth and survives a read
// of the personalised timeline.
//
// # WHY THIS CONTRACT EXISTS
//
// §3.4b used to record, as a standing limitation, that nothing outside a browser
// could mint the one credential RequireAuth accepts — a sealed token naming a row in the OAuth
// session store, issued only by /oauth/callback. testkit.AppView.SignIn closes
// that gap without any AppView code: it drives /oauth/login, the PDS'
// authorization server, and /oauth/callback exactly as a browser does, and
// hands back the coves_session value the callback set. Nothing here writes to
// the AppView's database, so the tier's rules (contracts_test.go) still hold.
//
// Every viewer-scoped contract that follows is built on SignIn, so this one
// pins the helper itself: if the token it returns is not a live session, the
// failure belongs here, named as such, rather than as a 401 in a contract about
// something else.
//
// # WHY /api/me IS READ TWICE
//
// getTimeline is not just the first viewer-scoped read this tier can make. Its
// handler hydrates viewer vote state (common.PopulateViewerVoteState), which
// resumes the OAuth session to talk to the viewer's PDS, and the session
// operation path deletes the stored session when a refresh is rejected
// (internal/atproto/oauth/session_operation.go). A token that authenticates
// once and is then silently revoked by that path would pass a single /api/me
// check. The second read, after the timeline, proves the session is still in
// the store.
func TestSessionContract(t *testing.T) {
	p := newPipeline(t)

	// Given an account the AppView has indexed through signup.
	account := p.IndexedAccount(t, "sess")

	// When the harness signs it in through the real OAuth web flow.
	token := p.AppView.SignIn(t, account)
	viewer := p.AppView.As(token)

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// Then /api/me recognises the session as that account.
	var me struct {
		DID string `json:"did"`
	}
	require.NoError(t, viewer.Get(ctx, "/api/me", &me),
		"GET /api/me with the token SignIn returned for %s must be accepted by RequireAuth", account.Handle)
	require.Equal(t, account.DID, me.DID, "/api/me must identify the signed-in account")

	// And the personalised timeline, which is RequireAuth, serves it.
	var timeline struct {
		Feed []json.RawMessage `json:"feed"`
	}
	require.NoError(t, viewer.Query(ctx, "social.coves.feed.getTimeline", nil, &timeline),
		"social.coves.feed.getTimeline must accept the sealed session for %s", account.Handle)

	// And the timeline's viewer-state path left the session in place.
	var meAfter struct {
		DID string `json:"did"`
	}
	require.NoError(t, viewer.Get(ctx, "/api/me", &meAfter),
		"GET /api/me after getTimeline must still be accepted: a 401 here means reading the timeline revoked %s's session", account.Handle)
	require.Equal(t, account.DID, meAfter.DID, "/api/me after getTimeline must still identify the signed-in account")
}

// TestSessionContract_ForwardsAnAuthenticatedWriteToThePDS proves the session
// SignIn mints is not only accepted by RequireAuth but carries working DPoP
// credentials: a write made through the AppView lands in the signed-in
// account's OWN repo on the PDS.
//
// social.coves.actor.blockUser is the write because it is the smallest one
// that forwards to the PDS as the viewer — the AppView creates the block record
// with the session's tokens and answers with its URI and CID. The record is then
// read back from the PDS WITHOUT any credential, so what is asserted is the
// repo's own state rather than the AppView's account of it: the right repo, the
// right collection, the right subject, and the very CID the AppView reported.
func TestSessionContract_ForwardsAnAuthenticatedWriteToThePDS(t *testing.T) {
	p := newPipeline(t)

	// Given two indexed accounts, the second signed in through OAuth.
	blocked := p.IndexedAccount(t, "sblk")
	blocker := p.IndexedAccount(t, "sbkr")
	viewer := p.AppView.As(p.AppView.SignIn(t, blocker))

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// When the signed-in account blocks the other through the AppView.
	var response struct {
		Block struct {
			RecordURI string `json:"recordUri"`
			RecordCID string `json:"recordCid"`
		} `json:"block"`
	}
	require.NoError(t, viewer.Procedure(ctx, "social.coves.actor.blockUser",
		map[string]string{"subject": blocked.DID}, &response),
		"social.coves.actor.blockUser must accept the sealed session for %s", blocker.Handle)

	// Then the AppView names a record in the blocker's own repo.
	repoPrefix := "at://" + blocker.DID + "/" + actorBlockCollection + "/"
	require.True(t, strings.HasPrefix(response.Block.RecordURI, repoPrefix),
		"blockUser's recordUri %q must name %s's repo and the %s collection", response.Block.RecordURI, blocker.Handle, actorBlockCollection)
	rkey := strings.TrimPrefix(response.Block.RecordURI, repoPrefix)
	require.NotEmpty(t, rkey, "blockUser's recordUri %q has no record key", response.Block.RecordURI)
	require.NotContains(t, rkey, "/", "blockUser's recordUri %q is not collection/rkey shaped", response.Block.RecordURI)
	require.NotEmpty(t, response.Block.RecordCID, "blockUser answered without a recordCid")

	// And the PDS, read without any credential, holds exactly that record.
	var stored struct {
		URI   string `json:"uri"`
		CID   string `json:"cid"`
		Value struct {
			Subject string `json:"subject"`
		} `json:"value"`
	}
	require.NoError(t, p.PDS.Anon.Query(ctx, "com.atproto.repo.getRecord", url.Values{
		"repo":       {blocker.DID},
		"collection": {actorBlockCollection},
		"rkey":       {rkey},
	}, &stored), "the block record blockUser reported must exist in %s's repo on the PDS", blocker.Handle)
	require.Equal(t, blocked.DID, stored.Value.Subject, "the block record in the PDS must name the blocked account")
	require.Equal(t, response.Block.RecordCID, stored.CID, "the PDS must hold the very record version blockUser reported")
}

// TestSessionContract_RejectsAWrongPassword pins SignInE's failure against the
// real provider: a wrong password is refused at the sign-in step, and the error
// that says so repeats neither the password that was tried nor the real one —
// @atproto/oauth-provider's own messages are free text, and this is the tier
// where they are the real ones.
func TestSessionContract_RejectsAWrongPassword(t *testing.T) {
	p := newPipeline(t)

	// Given an indexed account.
	account := p.IndexedAccount(t, "swpw")
	wrongPassword := "wrong-" + account.Password

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// When the harness signs in with the wrong password.
	token, err := p.AppView.SignInE(ctx, account.Handle, wrongPassword)

	// Then sign-in fails at the sign-in step without disclosing either password.
	// The leak checks come first and never print the message or the token, so a
	// failure cannot copy a secret into the test output; once they pass, the
	// message is safe to show.
	require.Error(t, err, "signing %s in with a wrong password must fail", account.Handle)
	require.True(t, token == "", "a refused sign-in must not return a token")
	message := err.Error()
	require.False(t, strings.Contains(message, wrongPassword), "the error leaks the password that was tried")
	require.False(t, strings.Contains(message, account.Password), "the error leaks the account's real password")
	// @atproto/oauth-provider answers a wrong password with HTTP 400 and the
	// OAuth error code invalid_request.
	require.True(t, strings.HasPrefix(message, "pds sign-in:"), "the error must name the sign-in step, got %q", message)
	require.Contains(t, message, "HTTP 400", "the provider must refuse the wrong password with HTTP 400")
	require.Contains(t, message, "invalid_request", "the provider must refuse the wrong password with error invalid_request")
}
