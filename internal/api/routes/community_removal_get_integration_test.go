//go:build integration

package routes_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	communityhandler "Coves/internal/api/handlers/community"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	communityRemovalGetDisplayName = "Removal Fixture Community"
	communityRemovalGetDescription = "A community whose profile a removal withholds"
	communityRemovalGetAvatarCID   = "bafkreicommunityremovalgetavatar"
	communityRemovalGetBannerCID   = "bafkreicommunityremovalgetbanner"
)

// TestCommunityGetRemovedState reads community.get through the real handler,
// community service and repository bound to the instance authority, before,
// during and after an instance removal of the community.
func TestCommunityGetRemovedState(t *testing.T) {
	db := testkit.DB(t)
	name := testkit.UniqueIDWithPrefix(t, "removedget")
	communityDID, err := fixtures.Community(t.Context(), db, name, "owner"+name)
	require.NoError(t, err)
	handle := name + ".coves.social"
	_, err = db.ExecContext(t.Context(), `
		UPDATE communities SET display_name = $2, description = $3, avatar_cid = $4, banner_cid = $5 WHERE did = $1
	`, communityDID, communityRemovalGetDisplayName, communityRemovalGetDescription, communityRemovalGetAvatarCID, communityRemovalGetBannerCID)
	require.NoError(t, err)

	repository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed(), postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	service := communities.NewCommunityService(repository, testkit.Endpoints().PDS.BaseURL, fixtures.InstanceDID(), "coves.social", nil, nil, nil, nil)
	handler := communityhandler.NewGetHandler(service, repository)
	get := func(t *testing.T, identifier string) map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		handler.HandleGet(response, httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.get?community="+url.QueryEscape(identifier), nil))
		require.Equalf(t, http.StatusOK, response.Code, "community.get %s: %s", identifier, response.Body.String())
		var view map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
		return view
	}
	// requireFullView checks the profile fields; images is false for a
	// by-handle read, which hydrates no direct blob URL without the image
	// proxy because GetByHandle does not load pds_url (unrelated to removal).
	requireFullView := func(t *testing.T, view map[string]any, images bool) {
		t.Helper()
		assert.NotContains(t, view, "status")
		assert.Equal(t, communityRemovalGetDisplayName, view["displayName"])
		assert.Equal(t, communityRemovalGetDescription, view["description"])
		if images {
			assert.Contains(t, view["avatar"], communityRemovalGetAvatarCID)
			assert.Contains(t, view["banner"], communityRemovalGetBannerCID)
		}
	}
	moderator := communityremovalfixture.New(db)

	t.Run("before removal the full view is served", func(t *testing.T) {
		requireFullView(t, get(t, communityDID), true)
		requireFullView(t, get(t, handle), false)
	})

	moderator.Remove(t, communityDID)
	for _, test := range []struct {
		name, identifier string
	}{
		{name: "during removal by DID the view is withheld", identifier: communityDID},
		{name: "during removal by handle the view is withheld", identifier: handle},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := get(t, test.identifier)
			assert.Equal(t, "removed", view["status"])
			assert.Equal(t, communityDID, view["did"])
			assert.Equal(t, handle, view["handle"])
			assert.Equal(t, name, view["name"])
			for _, withheld := range []string{"displayName", "description", "avatar", "banner"} {
				assert.NotContains(t, view, withheld, "a removed community withholds %s", withheld)
			}
		})
	}

	moderator.Restore(t, communityDID)
	t.Run("after restore the full view is served again", func(t *testing.T) {
		requireFullView(t, get(t, communityDID), true)
		requireFullView(t, get(t, handle), false)
	})
}
