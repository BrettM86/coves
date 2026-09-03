//go:build integration

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialCipherAcceptance(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()

	cipherA, err := credentialcipher.New(bytes.Repeat([]byte{0xa5}, credentialcipher.KeySize))
	require.NoError(t, err)
	cipherB, err := credentialcipher.New(bytes.Repeat([]byte{0x5a}, credentialcipher.KeySize))
	require.NoError(t, err)

	communityRepo := NewCommunityRepository(db, cipherA)
	aggregatorRepo := NewAggregatorRepository(db, cipherA)

	id := testkit.UniqueID(t)
	communityDID := "did:plc:" + id
	community := &communities.Community{
		DID:             communityDID,
		Handle:          fmt.Sprintf("!credential-cipher-%s@coves.local", id),
		Name:            "credential-cipher-acceptance",
		OwnerDID:        communityDID,
		CreatedByDID:    "did:plc:credential-cipher-creator",
		HostedByDID:     "did:web:coves.local",
		Visibility:      "public",
		PDSEmail:        "credential-cipher@communities.coves.local",
		PDSPassword:     "community-password-acceptance",
		PDSAccessToken:  "community-access-token-acceptance",
		PDSRefreshToken: "community-refresh-token-acceptance",
		PDSURL:          testkit.Endpoints().PDS.BaseURL,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	_, err = communityRepo.Create(ctx, community)
	require.NoError(t, err)

	aggregatorDID := indexAggregator(t, aggregatorRepo, "Credential Cipher Acceptance")
	oauthSession := aggregatorOAuthSession("acceptance", time.Now().Add(time.Hour))
	require.NoError(t, aggregatorRepo.SetAPIKey(
		ctx,
		aggregatorDID,
		aggregatorKeyPrefix("acceptance"),
		aggregatorAPIKeyHash(t, "credential-cipher-acceptance"),
		oauthSession,
	))

	var communityPassword, communityAccess, communityRefresh []byte
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT pds_password_encrypted, pds_access_token_encrypted, pds_refresh_token_encrypted
		FROM communities
		WHERE did = $1`, communityDID).Scan(&communityPassword, &communityAccess, &communityRefresh))
	aggregatorAccess, aggregatorRefresh, aggregatorDPoP := aggregatorCiphertext(t, db, aggregatorDID)

	credentials := []struct {
		name       string
		ciphertext []byte
		plaintext  string
		context    string
	}{
		{
			name:       "community password",
			ciphertext: communityPassword,
			plaintext:  community.PDSPassword,
			context:    "communities.pds_password_encrypted:" + communityDID,
		},
		{
			name:       "community access token",
			ciphertext: communityAccess,
			plaintext:  community.PDSAccessToken,
			context:    "communities.pds_access_token_encrypted:" + communityDID,
		},
		{
			name:       "community refresh token",
			ciphertext: communityRefresh,
			plaintext:  community.PDSRefreshToken,
			context:    "communities.pds_refresh_token_encrypted:" + communityDID,
		},
		{
			name:       "aggregator access token",
			ciphertext: aggregatorAccess,
			plaintext:  oauthSession.AccessToken,
			context:    "aggregators.oauth_access_token_encrypted:" + aggregatorDID,
		},
		{
			name:       "aggregator refresh token",
			ciphertext: aggregatorRefresh,
			plaintext:  oauthSession.RefreshToken,
			context:    "aggregators.oauth_refresh_token_encrypted:" + aggregatorDID,
		},
		{
			name:       "aggregator DPoP private key",
			ciphertext: aggregatorDPoP,
			plaintext:  oauthSession.DPoPPrivateKeyMultibase,
			context:    "aggregators.oauth_dpop_private_key_encrypted:" + aggregatorDID,
		},
	}

	t.Run("stores opaque non-NULL ciphertext", func(t *testing.T) {
		for _, credential := range credentials {
			assert.NotNil(t, credential.ciphertext, "%s ciphertext is NULL", credential.name)
			assert.False(t, bytes.Contains(credential.ciphertext, []byte(credential.plaintext)),
				"%s ciphertext contains its plaintext", credential.name)
		}
	})

	t.Run("cipher A decrypts every credential with its row and column context", func(t *testing.T) {
		for _, credential := range credentials {
			plaintext, decryptErr := cipherA.Decrypt(credential.ciphertext, credential.context)
			if assert.NoError(t, decryptErr, "%s did not decrypt with cipher A", credential.name) {
				assert.Equal(t, credential.plaintext, plaintext, "%s plaintext mismatch", credential.name)
			}
		}
	})

	t.Run("cipher B cannot decrypt cipher A credentials", func(t *testing.T) {
		for _, credential := range credentials {
			_, decryptErr := cipherB.Decrypt(credential.ciphertext, credential.context)
			assert.ErrorIs(t, decryptErr, credentialcipher.ErrInvalidCiphertext,
				"%s decrypted with the wrong key", credential.name)
		}
	})

	t.Run("repositories return plaintext credentials", func(t *testing.T) {
		retrievedCommunity, getErr := communityRepo.GetByDID(ctx, communityDID)
		require.NoError(t, getErr)
		assert.Equal(t, community.PDSPassword, retrievedCommunity.PDSPassword)
		assert.Equal(t, community.PDSAccessToken, retrievedCommunity.PDSAccessToken)
		assert.Equal(t, community.PDSRefreshToken, retrievedCommunity.PDSRefreshToken)

		retrievedAggregator, getErr := aggregatorRepo.GetAggregatorCredentials(ctx, aggregatorDID)
		require.NoError(t, getErr)
		assert.Equal(t, oauthSession.AccessToken, retrievedAggregator.OAuthAccessToken)
		assert.Equal(t, oauthSession.RefreshToken, retrievedAggregator.OAuthRefreshToken)
		assert.Equal(t, oauthSession.DPoPPrivateKeyMultibase, retrievedAggregator.OAuthDPoPPrivateKeyMultibase)
	})

	t.Run("database stores no encryption key", func(t *testing.T) {
		var encryptionKeysTable sql.NullString
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('encryption_keys')`).Scan(&encryptionKeysTable))
		assert.False(t, encryptionKeysTable.Valid, "encryption_keys still exists as %q", encryptionKeysTable.String)
	})
}
