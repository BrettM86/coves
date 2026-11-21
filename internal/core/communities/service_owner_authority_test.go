package communities_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ownerAuthorityCreator = "did:plc:creator00000000000000" // aValidCreateRequest().CreatedByDID
	ownerAuthorityOwner   = "did:plc:ownerauthorityowner"
	ownerAuthorityAdmin   = "did:plc:ownerauthorityadmin"
	ownerAuthorityMod     = "did:plc:ownerauthoritymoderator"
	// RoleRecordKey("did:plc:creator00000000000000")
	creatorOwnerRecordKey = "lj5ksiybcif53ogtgmvnilhdjjwtlw2fr7yzuj42enno4r7qc3mq"
)

// UpdateCommunity authorizes by the indexed role, not by createdBy.
func TestUpdateCommunity_AuthorizesByIndexedOwner(t *testing.T) {
	for _, test := range []struct {
		name      string
		caller    string
		notHosted bool
		wantErr   error
	}{
		{name: "indexed owner who is not the creator", caller: ownerAuthorityOwner},
		{name: "creator with no role row", caller: ownerAuthorityCreator, wantErr: communities.ErrUnauthorized},
		{name: "indexed moderator", caller: ownerAuthorityMod, wantErr: communities.ErrUnauthorized},
		{name: "instance admin in a hosted community", caller: ownerAuthorityAdmin},
		{
			name: "instance admin in a community not hosted here", caller: ownerAuthorityAdmin, notHosted: true,
			wantErr: communities.ErrUnauthorized,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newCommunityMediaHarness(t)
			h.authz.roles.rows = []communities.CommunityRole{
				ownerRow(mediaCommunityDID, ownerAuthorityOwner, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)),
				moderatorRow(mediaCommunityDID, ownerAuthorityMod, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)),
			}
			h.authz.admins[ownerAuthorityAdmin] = true
			if test.notHosted {
				h.authz.hosted[mediaCommunityDID] = false
			}
			description := "updated by " + test.caller

			_, err := h.service.UpdateCommunity(context.Background(), communities.UpdateCommunityRequest{
				CommunityDID: mediaCommunityDID, UpdatedByDID: test.caller, Description: &description,
			})

			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				assert.Empty(t, h.recordWrites(), "a refused update must write nothing to the community's repo")
				return
			}
			require.NoError(t, err)
			profiles := h.writesTo("social.coves.community.profile")
			require.Len(t, profiles, 1)
			assert.Equal(t, "putRecord", profiles[0].method)
			assert.Equal(t, description, profiles[0].record["description"])
			assert.Equal(t, "did:plc:creator00000000000000", profiles[0].record["createdBy"],
				"the profile keeps the creator as createdBy whoever updates it")
		})
	}
}

// requireOwnerRecord asserts write is the creator's owner record in the
// created community's repo, written with the community account's token.
func requireOwnerRecord(t *testing.T, h *communityMediaHarness, write pdsRecordWrite, method string) {
	t.Helper()
	assert.Equal(t, method, write.method)
	assert.Equal(t, "did:plc:mediafixturecommunity", write.repo)
	assert.Equal(t, "social.coves.community.moderator", write.collection)
	assert.Equal(t, creatorOwnerRecordKey, write.rkey)
	assert.Equal(t, "Bearer "+h.accessToken, write.authorization, "the owner record is written as the community")
	require.ElementsMatch(t, []string{"$type", "subject", "role", "createdAt"}, keysOf(write.record))
	assert.Equal(t, "social.coves.community.moderator", write.record["$type"])
	assert.Equal(t, "did:plc:creator00000000000000", write.record["subject"])
	assert.Equal(t, "owner", write.record["role"])
	createdAt, ok := write.record["createdAt"].(string)
	require.True(t, ok, "createdAt must be a string, got %#v", write.record["createdAt"])
	_, err := time.Parse(time.RFC3339, createdAt)
	assert.NoError(t, err, "createdAt must be RFC 3339")
}

func keysOf(record map[string]any) []string {
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	return keys
}

// prepareCreate removes the seeded community so a create of its name is not
// refused as taken.
func (h *communityMediaHarness) prepareCreate() {
	h.repo.mu.Lock()
	defer h.repo.mu.Unlock()
	delete(h.repo.byHandle, h.stored.Handle)
	delete(h.repo.byDID, h.stored.DID)
}

func TestCreateCommunity_WritesTheCreatorsOwnerRecordAfterTheProfile(t *testing.T) {
	h := newCommunityMediaHarness(t)
	h.authz.roles.rows = nil
	h.prepareCreate()

	created, err := h.service.CreateCommunity(context.Background(), aValidCreateRequest())
	require.NoError(t, err)
	require.NotNil(t, created)

	writes := h.recordWrites()
	require.Len(t, writes, 2, "a fresh create writes the profile, then the owner record")
	assert.Equal(t, "createRecord", writes[0].method)
	assert.Equal(t, "social.coves.community.profile", writes[0].collection)
	assert.Equal(t, "self", writes[0].rkey)
	requireOwnerRecord(t, h, writes[1], "createRecord")

	rows := h.authz.roles.snapshot()
	require.Len(t, rows, 1, "the owner role is indexed locally when the create returns")
	assert.Equal(t, "did:plc:mediafixturecommunity", rows[0].CommunityDID)
	assert.Equal(t, "did:plc:creator00000000000000", rows[0].SubjectDID)
	assert.Equal(t, "owner", rows[0].Role)
	assert.Equal(t, creatorOwnerRecordKey, rows[0].RecordKey)
	assert.Equal(t, "bafywrite2", rows[0].RecordCID, "the indexed row carries the CID the PDS returned for the owner record")
	recordCreatedAt, err := time.Parse(time.RFC3339, writes[1].record["createdAt"].(string))
	require.NoError(t, err)
	assert.True(t, recordCreatedAt.Equal(rows[0].RecordCreatedAt.Truncate(time.Second)),
		"the indexed row carries the record's createdAt: row %v, record %v", rows[0].RecordCreatedAt, recordCreatedAt)
}

// A failed owner record write fails the create before the community row is
// persisted, so the provisioning account survives for the creator's retry.
func TestCreateCommunity_OwnerRecordWriteFailureFailsTheCreate(t *testing.T) {
	for _, test := range []struct {
		name    string
		resumed bool
	}{
		{name: "fresh create"},
		{name: "resumed create", resumed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newCommunityMediaHarness(t)
			h.authz.roles.rows = nil
			h.prepareCreate()
			handle := "c-gardening." + fakeInstanceDomain
			if test.resumed {
				h.repo.seedProvisioningAccount(&communities.ProvisioningAccount{
					Handle: handle, Name: "gardening",
					CreatedByDID: ownerAuthorityCreator, HostedByDID: fakeInstanceDID,
					PDSURL: h.pdsURL, PDSEmail: "community-gardening@coves.example", PDSPassword: "saved-provisioning-password",
				})
				h.existingRecords["social.coves.community.profile/self"] = true
			}
			h.failingCollections["social.coves.community.moderator"] = true

			created, err := h.service.CreateCommunity(context.Background(), aValidCreateRequest())

			require.Error(t, err, "a create whose owner record was not written must not succeed")
			assert.Nil(t, created)
			_, err = h.repo.GetByDID(context.Background(), "did:plc:mediafixturecommunity")
			assert.ErrorIs(t, err, communities.ErrCommunityNotFound,
				"a create whose owner record was not written must not persist the community row")
			_, err = h.repo.GetProvisioningAccountByHandle(context.Background(), handle)
			assert.NoError(t, err, "the provisioning account must survive so the creator's retry can resume")
		})
	}
}

// An interrupted create left the account and its profile record but no owner
// record; the creator's retry resumes the account and writes the owner record
// with putRecord, so it succeeds even where createRecord would be refused
// because the record already exists.
func TestCreateCommunity_ResumedCreateWritesTheOwnerRecordWithPutRecord(t *testing.T) {
	h := newCommunityMediaHarness(t)
	h.authz.roles.rows = nil
	h.prepareCreate()
	h.repo.seedProvisioningAccount(&communities.ProvisioningAccount{
		Handle: "c-gardening." + fakeInstanceDomain, Name: "gardening",
		CreatedByDID: ownerAuthorityCreator, HostedByDID: fakeInstanceDID,
		PDSURL: h.pdsURL, PDSEmail: "community-gardening@coves.example", PDSPassword: "saved-provisioning-password",
	})
	h.existingRecords["social.coves.community.profile/self"] = true
	h.existingRecords["social.coves.community.moderator/"+creatorOwnerRecordKey] = true

	created, err := h.service.CreateCommunity(context.Background(), aValidCreateRequest())
	require.NoError(t, err)
	require.NotNil(t, created)

	assert.Empty(t, h.requestsAt("/xrpc/com.atproto.server.createAccount"), "the interrupted create's account is resumed")
	owners := h.writesTo("social.coves.community.moderator")
	require.Len(t, owners, 1)
	requireOwnerRecord(t, h, owners[0], "putRecord")
	for _, write := range h.recordWrites() {
		assert.Equal(t, "putRecord", write.method, "a resumed create replaces records rather than creating them")
	}
}
