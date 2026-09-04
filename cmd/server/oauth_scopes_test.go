package main

import (
	"Coves/internal/core/communities"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The OAuth scopes must grant postv2 writes, or the CORE WRITE PATH is refused
// (whole-branch review, P1).
//
// Under author-owned posts, CreatePost / UpdatePost / delete AND the cutover tool
// all write social.coves.community.postv2 into the AUTHOR's repo through the
// author's own OAuth session. A scope-enforcing PDS rejects a write to a
// collection the granted scopes do not name — so a session minted from
// oauthScopes() that lists only community.post cannot write a single postv2, and
// the whole feature fails at the PDS boundary with an authorization error rather
// than anywhere the AppView can see.
//
// The community.post grant must ALSO survive: the tool DELETES the deprecated
// community.post records through these same sessions during the drain, so
// dropping that scope would strand every legacy record undeleteable.

// scopeFor returns the granted scope entry for a collection, and whether one
// exists. A scope is "repo:<collection>?action=...&action=..." (or bare
// "repo:<collection>").
func scopeFor(scopes []string, collection string) (string, bool) {
	prefix := "repo:" + collection
	for _, s := range scopes {
		if s == prefix || strings.HasPrefix(s, prefix+"?") {
			return s, true
		}
	}
	return "", false
}

func TestOAuthScopes_GrantCommunityBlockCreateDelete(t *testing.T) {
	scopes := oauthScopes()

	communityBlock, ok := scopeFor(scopes, "social.coves.community.block")
	require.Truef(t, ok,
		"oauthScopes() grants no repo:social.coves.community.block scope. BlockCommunity and UnblockCommunity write this collection through the viewer's OAuth session, so a scope-enforcing PDS refuses both operations. Scopes: %v", scopes)
	assert.Equal(t, communities.CommunityBlockOAuthScope, communityBlock,
		"the server must request the exact scope the write service later enforces")

	for _, action := range []string{"action=create", "action=delete"} {
		assert.Containsf(t, communityBlock, action,
			"the community-block scope must grant %s: blocking creates the record and unblocking deletes it", action)
	}
}

func TestOAuthScopes_GrantPostV2CreateUpdateDelete(t *testing.T) {
	scopes := oauthScopes()

	postv2, ok := scopeFor(scopes, "social.coves.community.postv2")
	require.Truef(t, ok,
		"oauthScopes() grants no repo:social.coves.community.postv2 scope. CreatePost, UpdatePost, post.delete and the cutover tool all write postv2 through the author's OAuth "+
			"session, so a scope-enforcing PDS refuses the entire write path. Grant postv2 with create+update+delete. Scopes: %v", scopes)

	for _, action := range []string{"action=create", "action=update", "action=delete"} {
		assert.Containsf(t, postv2, action,
			"the postv2 scope must grant %s: create is the write, update is post.update (§3.4), delete is post.delete — the tool and the write path perform all three", action)
	}
}

func TestOAuthScopes_RetainCommunityPostThroughTheDrain(t *testing.T) {
	scopes := oauthScopes()

	post, ok := scopeFor(scopes, "social.coves.community.post")
	require.Truef(t, ok,
		"oauthScopes() dropped the deprecated community.post grant. The cutover tool DELETES legacy community.post records through these sessions during the drain (§11); "+
			"without the grant every legacy record is stranded undeleteable. Keep it until the post-drain follow-up retires the collection. Scopes: %v", scopes)

	assert.Containsf(t, post, "action=delete",
		"the retained community.post grant must include action=delete — the drain's whole job is deleting those records")
}
