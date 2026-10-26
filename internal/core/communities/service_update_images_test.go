//go:build integration

package communities_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"
)

func communityImageBlobObject(t *testing.T, blob testkit.BlobRef) map[string]any {
	t.Helper()
	data, err := json.Marshal(blob)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(data, &object))
	return object
}

func communityWithCurrentImages(t *testing.T, prefix string) (communities.Service, communities.Repository, *testkit.Account, *communities.Community, string) {
	t.Helper()
	service, repo, pdsServer, community, creator := newUpdatableCommunity(t, prefix)
	account := pdsServer.Login(t, community.Handle, community.PDSPassword)
	avatar := account.UploadBlob(t, testkit.TestPNG(32, 24), "image/png")
	banner := account.UploadBlob(t, testkit.TestJPEG(96, 32), "image/jpeg")
	current := account.GetRecord(t, "social.coves.community.profile", "self")
	current.Value["avatar"] = communityImageBlobObject(t, avatar)
	current.Value["banner"] = communityImageBlobObject(t, banner)
	account.PutRecord(t, "social.coves.community.profile", "self", current.Value)
	community.AvatarCID, community.BannerCID = avatar.CID(), banner.CID()
	_, err := repo.Update(context.Background(), community)
	require.NoError(t, err)
	return service, repo, account, community, creator
}

func TestService_UpdateTextKeepsCurrentPDSImages(t *testing.T) {
	service, repo, account, community, creator := communityWithCurrentImages(t, "keep")
	current := account.GetRecord(t, "social.coves.community.profile", "self")
	current.Value["avatar"] = communityImageBlobObject(t, account.UploadBlob(t, testkit.TestPNG(48, 40), "image/png"))
	current.Value["banner"] = communityImageBlobObject(t, account.UploadBlob(t, testkit.TestJPEG(128, 48), "image/jpeg"))
	account.PutRecord(t, "social.coves.community.profile", "self", current.Value)
	before := account.GetRecord(t, "social.coves.community.profile", "self")
	indexed, err := repo.GetByDID(context.Background(), community.DID)
	require.NoError(t, err)
	require.NotEqual(t, indexed.AvatarCID, before.Value["avatar"].(map[string]any)["ref"].(map[string]any)["$link"], "the indexed avatar must lag the current PDS image")
	require.NotEqual(t, indexed.BannerCID, before.Value["banner"].(map[string]any)["ref"].(map[string]any)["$link"], "the indexed banner must lag the current PDS image")
	require.NotEqual(t, before.Value["avatar"], before.Value["banner"], "use distinct blob snapshots")
	displayName, description := "Text edit keeps images", "Both current PDS images must survive"
	_, err = service.UpdateCommunity(context.Background(), communities.UpdateCommunityRequest{
		CommunityDID: community.DID, UpdatedByDID: creator, DisplayName: &displayName, Description: &description,
	})
	require.NoError(t, err)
	after := account.GetRecord(t, "social.coves.community.profile", "self")
	assert.Equal(t, displayName, after.Value["displayName"])
	assert.Equal(t, description, after.Value["description"])
	assert.Equal(t, before.Value["avatar"], after.Value["avatar"], "text edit must preserve the complete current avatar blob")
	assert.Equal(t, before.Value["banner"], after.Value["banner"], "text edit must preserve the complete current banner blob")
}

func TestService_UpdateAvatarKeepsCurrentPDSBanner(t *testing.T) {
	service, _, account, community, creator := communityWithCurrentImages(t, "swap")
	before := account.GetRecord(t, "social.coves.community.profile", "self")
	requestedBytes := testkit.TestPNG(48, 40)
	requestedAvatar := communityImageBlobObject(t, account.UploadBlob(t, requestedBytes, "image/png"))
	require.NotEqual(t, before.Value["avatar"], requestedAvatar)
	_, err := service.UpdateCommunity(context.Background(), communities.UpdateCommunityRequest{
		CommunityDID: community.DID, UpdatedByDID: creator, AvatarBlob: requestedBytes, AvatarMimeType: "image/png",
	})
	require.NoError(t, err)
	after := account.GetRecord(t, "social.coves.community.profile", "self")
	assert.Equal(t, requestedAvatar, after.Value["avatar"], "write the requested avatar, including its independently uploaded CID and metadata")
	assert.Equal(t, before.Value["banner"], after.Value["banner"], "avatar replacement must preserve the complete current banner blob")
}

func TestService_UpdateDoesNotRebuildImagesFromStaleRow(t *testing.T) {
	service, repo, account, community, creator := communityWithCurrentImages(t, "none")
	current := account.GetRecord(t, "social.coves.community.profile", "self")
	// Keep the uploaded blobs referenced independently so the overlay's stale-row
	// reconstruction uses valid stored blobs and reaches the absence assertion.
	account.PutRecord(t, "social.coves.community.profile", "retained-images", current.Value)
	delete(current.Value, "avatar")
	delete(current.Value, "banner")
	account.PutRecord(t, "social.coves.community.profile", "self", current.Value)
	imageFree := account.GetRecord(t, "social.coves.community.profile", "self")
	require.NotContains(t, imageFree.Value, "avatar")
	require.NotContains(t, imageFree.Value, "banner")
	indexed, err := repo.GetByDID(context.Background(), community.DID)
	require.NoError(t, err)
	require.Equal(t, community.AvatarCID, indexed.AvatarCID)
	require.Equal(t, community.BannerCID, indexed.BannerCID)
	require.NotEmpty(t, indexed.AvatarCID)
	require.NotEmpty(t, indexed.BannerCID)
	description := "Current PDS profile is authoritative"
	_, err = service.UpdateCommunity(context.Background(), communities.UpdateCommunityRequest{
		CommunityDID: community.DID, UpdatedByDID: creator, Description: &description,
	})
	require.NoError(t, err)
	after := account.GetRecord(t, "social.coves.community.profile", "self")
	assert.Equal(t, description, after.Value["description"])
	assert.NotContains(t, after.Value, "avatar", "do not reconstruct an absent PDS avatar from the stale row")
	assert.NotContains(t, after.Value, "banner", "do not reconstruct an absent PDS banner from the stale row")
}
