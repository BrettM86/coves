//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommunityCreatorOwnsTheCommunity is the production wiring of owner
// authority: community.create writes the creator's owner record to the
// community's repo and indexes it, so the creator can update the community and
// another account cannot, and the creator's viewer state names the role.
//
// It never sets the community's password through the PDS admin: the AppView
// must keep its own credentials to perform the creator's update.
func TestCommunityCreatorOwnsTheCommunity(t *testing.T) {
	p := newPipeline(t)
	creator := p.IndexedAccount(t, "owc")
	other := p.IndexedAccount(t, "owo")
	creatorClient := p.AppView.As(p.AppView.SignIn(t, creator))
	otherClient := p.AppView.As(p.AppView.SignIn(t, other))
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, creatorClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "ow"), "description": "a community its creator owns",
	}, &created))
	require.NotEmpty(t, created.DID)

	t.Run("the community repo holds the creator's owner record", func(t *testing.T) {
		var record struct {
			Value map[string]any `json:"value"`
		}
		require.NoError(t, p.PDS.Anon.Query(ctx, "com.atproto.repo.getRecord", url.Values{
			"repo": {created.DID}, "collection": {moderatorCollection}, "rkey": {moderatorRecordKey(creator.DID)},
		}, &record))
		assert.Equal(t, moderatorCollection, record.Value["$type"])
		assert.Equal(t, creator.DID, record.Value["subject"])
		assert.Equal(t, "owner", record.Value["role"])
		assert.NotEmpty(t, record.Value["createdAt"])
	})

	t.Run("the creator's community.get names the owner role", func(t *testing.T) {
		var response struct {
			Viewer map[string]any `json:"viewer"`
		}
		require.NoError(t, creatorClient.Query(ctx, "social.coves.community.get",
			url.Values{"community": {created.DID}}, &response))
		assert.Equal(t, "owner", response.Viewer["role"])
	})

	t.Run("the creator's viewer authority reports a community role", func(t *testing.T) {
		var authority map[string]any
		require.NoError(t, creatorClient.Query(ctx, "social.coves.actor.getViewerAuthority", url.Values{}, &authority))
		assert.Equal(t, true, authority["hasCommunityRole"])
	})

	t.Run("another account's update is refused", func(t *testing.T) {
		err := otherClient.Procedure(ctx, "social.coves.community.update", map[string]any{
			"communityDid": created.DID, "description": "taken over",
		}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusForbidden), "want 403, got %v", err)
		var statusErr *testkit.StatusError
		require.ErrorAs(t, err, &statusErr)
		assert.Equal(t, "Forbidden", statusErr.XRPCError)
	})

	t.Run("the creator's update succeeds", func(t *testing.T) {
		require.NoError(t, creatorClient.Procedure(ctx, "social.coves.community.update", map[string]any{
			"communityDid": created.DID, "description": "updated by its owner",
		}, nil))
	})
}
