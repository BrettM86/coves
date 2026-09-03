//go:build integration

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialCipherRepoCommunityUpdateCredentials(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	cipher := credentialCipherRepoTestCipher(t)
	repo := NewCommunityRepository(db, cipher)
	community := credentialCipherRepoCommunity(t, "update")

	_, err := repo.Create(ctx, community)
	require.NoError(t, err)
	passwordBefore, _, _ := communityCredentialCiphertext(t, db, community.DID)

	const newAccessToken = "community-access-token-after-refresh"
	const newRefreshToken = "community-refresh-token-after-refresh"
	require.NoError(t, repo.UpdateCredentials(ctx, community.DID, newAccessToken, newRefreshToken))

	passwordAfter, accessAfter, refreshAfter := communityCredentialCiphertext(t, db, community.DID)
	t.Run("rewrites tokens with the application cipher", func(t *testing.T) {
		assertCredentialCiphertext(t, cipher, accessAfter,
			"communities.pds_access_token_encrypted:"+community.DID, newAccessToken)
		assertCredentialCiphertext(t, cipher, refreshAfter,
			"communities.pds_refresh_token_encrypted:"+community.DID, newRefreshToken)
	})

	t.Run("leaves the password ciphertext untouched", func(t *testing.T) {
		assert.Equal(t, passwordBefore, passwordAfter)
	})

	t.Run("returns the refreshed tokens", func(t *testing.T) {
		retrieved, err := repo.GetByDID(ctx, community.DID)
		require.NoError(t, err)
		assert.Equal(t, newAccessToken, retrieved.PDSAccessToken)
		assert.Equal(t, newRefreshToken, retrieved.PDSRefreshToken)
		assert.Equal(t, community.PDSPassword, retrieved.PDSPassword)
	})
}

func TestCredentialCipherRepoCommunityEmptyCredentialsStayNull(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	repo := NewCommunityRepository(db, credentialCipherRepoTestCipher(t))
	community := credentialCipherRepoCommunity(t, "empty")
	community.PDSPassword = ""
	community.PDSAccessToken = ""
	community.PDSRefreshToken = ""

	_, err := repo.Create(ctx, community)
	require.NoError(t, err)

	password, access, refresh := communityCredentialCiphertext(t, db, community.DID)
	assert.Nil(t, password, "empty password must be SQL NULL")
	assert.Nil(t, access, "empty access token must be SQL NULL")
	assert.Nil(t, refresh, "empty refresh token must be SQL NULL")

	retrieved, err := repo.GetByDID(ctx, community.DID)
	require.NoError(t, err)
	assert.Empty(t, retrieved.PDSPassword)
	assert.Empty(t, retrieved.PDSAccessToken)
	assert.Empty(t, retrieved.PDSRefreshToken)
}

func TestCredentialCipherRepoCommunityRejectsTampering(t *testing.T) {
	t.Run("flipped authentication tag", func(t *testing.T) {
		db := testkit.DB(t)
		ctx := context.Background()
		repo := NewCommunityRepository(db, credentialCipherRepoTestCipher(t))
		community := credentialCipherRepoCommunity(t, "flipped")
		_, err := repo.Create(ctx, community)
		require.NoError(t, err)

		password, _, _ := communityCredentialCiphertext(t, db, community.DID)
		require.NotEmpty(t, password)
		tampered := append([]byte(nil), password...)
		tampered[len(tampered)-1] ^= 0x01
		_, err = db.ExecContext(ctx,
			`UPDATE communities SET pds_password_encrypted = $2 WHERE did = $1`,
			community.DID, tampered)
		require.NoError(t, err)

		retrieved, err := repo.GetByDID(ctx, community.DID)
		assert.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext)
		assert.Nil(t, retrieved, "a decryption failure must not become a community with an empty password")
	})

	t.Run("ciphertext copied from another community", func(t *testing.T) {
		db := testkit.DB(t)
		ctx := context.Background()
		repo := NewCommunityRepository(db, credentialCipherRepoTestCipher(t))
		target := credentialCipherRepoCommunity(t, "copy-target")
		source := credentialCipherRepoCommunity(t, "copy-source")
		source.PDSPassword = "password-bound-to-the-source-community"
		_, err := repo.Create(ctx, target)
		require.NoError(t, err)
		_, err = repo.Create(ctx, source)
		require.NoError(t, err)

		sourcePassword, _, _ := communityCredentialCiphertext(t, db, source.DID)
		require.NotEmpty(t, sourcePassword)
		_, err = db.ExecContext(ctx,
			`UPDATE communities SET pds_password_encrypted = $2 WHERE did = $1`,
			target.DID, sourcePassword)
		require.NoError(t, err)

		retrieved, err := repo.GetByDID(ctx, target.DID)
		assert.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext)
		assert.Nil(t, retrieved, "row-bound ciphertext copied across communities must not yield a password")
	})
}

func TestCredentialCipherRepoAggregatorUpdateOAuthTokens(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	cipher := credentialCipherRepoTestCipher(t)
	repo := NewAggregatorRepository(db, cipher)
	aggregatorDID := indexAggregator(t, repo, "Credential Update Aggregator")
	keyHash := aggregatorAPIKeyHash(t, "credential-update-api-key")
	session := aggregatorOAuthSession("before-update", time.Now().Add(-time.Hour))
	require.NoError(t, repo.SetAPIKey(ctx, aggregatorDID, aggregatorKeyPrefix("update"), keyHash, session))
	_, _, dpopBefore := aggregatorCiphertext(t, db, aggregatorDID)

	const newAccessToken = "aggregator-access-token-after-refresh"
	const newRefreshToken = "aggregator-refresh-token-after-refresh"
	require.NoError(t, repo.UpdateOAuthTokens(
		ctx, aggregatorDID, newAccessToken, newRefreshToken, time.Now().Add(-time.Minute)))

	accessAfter, refreshAfter, dpopAfter := aggregatorCiphertext(t, db, aggregatorDID)
	t.Run("rewrites tokens with the application cipher", func(t *testing.T) {
		assertCredentialCiphertext(t, cipher, accessAfter,
			"aggregators.oauth_access_token_encrypted:"+aggregatorDID, newAccessToken)
		assertCredentialCiphertext(t, cipher, refreshAfter,
			"aggregators.oauth_refresh_token_encrypted:"+aggregatorDID, newRefreshToken)
	})

	t.Run("leaves the DPoP ciphertext untouched", func(t *testing.T) {
		assert.Equal(t, dpopBefore, dpopAfter)
	})

	t.Run("all credential readers return the refreshed tokens", func(t *testing.T) {
		byDID, err := repo.GetAggregatorCredentials(ctx, aggregatorDID)
		require.NoError(t, err)
		assert.Equal(t, newAccessToken, byDID.OAuthAccessToken)
		assert.Equal(t, newRefreshToken, byDID.OAuthRefreshToken)

		byHash, err := repo.GetCredentialsByAPIKeyHash(ctx, keyHash)
		require.NoError(t, err)
		assert.Equal(t, newAccessToken, byHash.OAuthAccessToken)
		assert.Equal(t, newRefreshToken, byHash.OAuthRefreshToken)

		due, err := repo.ListAggregatorsNeedingTokenRefresh(ctx, 0)
		require.NoError(t, err)
		listed := credentialCipherRepoFindAggregator(due, aggregatorDID)
		require.NotNil(t, listed)
		assert.Equal(t, newAccessToken, listed.OAuthAccessToken)
		assert.Equal(t, newRefreshToken, listed.OAuthRefreshToken)
	})
}

func TestCredentialCipherRepoAggregatorEmptyCredentialsStayNull(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	repo := NewAggregatorRepository(db, credentialCipherRepoTestCipher(t))
	aggregatorDID := indexAggregator(t, repo, "Empty Credential Aggregator")
	session := aggregatorOAuthSession("empty", time.Now().Add(time.Hour))
	session.AccessToken = ""
	session.RefreshToken = ""
	session.DPoPPrivateKeyMultibase = ""

	require.NoError(t, repo.SetAPIKey(
		ctx,
		aggregatorDID,
		aggregatorKeyPrefix("empty"),
		aggregatorAPIKeyHash(t, "empty-credential-api-key"),
		session,
	))

	access, refresh, dpop := aggregatorCiphertext(t, db, aggregatorDID)
	assert.Nil(t, access, "empty access token must be SQL NULL")
	assert.Nil(t, refresh, "empty refresh token must be SQL NULL")
	assert.Nil(t, dpop, "empty DPoP private key must be SQL NULL")

	retrieved, err := repo.GetAggregatorCredentials(ctx, aggregatorDID)
	require.NoError(t, err)
	assert.Empty(t, retrieved.OAuthAccessToken)
	assert.Empty(t, retrieved.OAuthRefreshToken)
	assert.Empty(t, retrieved.OAuthDPoPPrivateKeyMultibase)
}

func TestCredentialCipherRepoAggregatorRejectsTamperedDPoPKey(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	repo := NewAggregatorRepository(db, credentialCipherRepoTestCipher(t))
	aggregatorDID := indexAggregator(t, repo, "Tampered Credential Aggregator")
	keyHash := aggregatorAPIKeyHash(t, "tampered-credential-api-key")
	session := aggregatorOAuthSession("tampered", time.Now().Add(-time.Minute))
	require.NoError(t, repo.SetAPIKey(
		ctx, aggregatorDID, aggregatorKeyPrefix("tampered"), keyHash, session))

	_, _, dpop := aggregatorCiphertext(t, db, aggregatorDID)
	require.NotEmpty(t, dpop)
	tampered := append([]byte(nil), dpop...)
	tampered[len(tampered)-1] ^= 0x01
	_, err := db.ExecContext(ctx,
		`UPDATE aggregators SET oauth_dpop_private_key_encrypted = $2 WHERE did = $1`,
		aggregatorDID, tampered)
	require.NoError(t, err)

	byDID, err := repo.GetAggregatorCredentials(ctx, aggregatorDID)
	assert.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext)
	assert.Nil(t, byDID)

	byHash, err := repo.GetCredentialsByAPIKeyHash(ctx, keyHash)
	assert.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext)
	assert.Nil(t, byHash)

	due, err := repo.ListAggregatorsNeedingTokenRefresh(ctx, 0)
	assert.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext,
		"the refresh list must fail rather than silently skip a row with corrupted credentials")
	assert.Nil(t, due)
}

func TestCredentialCipherRepoClassifiesLegacyPgcryptoValues(t *testing.T) {
	t.Run("community password", func(t *testing.T) {
		db := testkit.DB(t)
		ctx := context.Background()
		repo := NewCommunityRepository(db, credentialCipherRepoTestCipher(t))
		community := credentialCipherRepoCommunity(t, "legacy-pgcrypto")
		_, err := repo.Create(ctx, community)
		require.NoError(t, err)

		_, err = db.ExecContext(ctx, `
			UPDATE communities
			SET pds_password_encrypted = pgp_sym_encrypt('x', 'k')
			WHERE did = $1
		`, community.DID)
		require.NoError(t, err)

		retrieved, err := repo.GetByDID(ctx, community.DID)
		require.Error(t, err)
		assert.True(t, errors.Is(err, credentialcipher.ErrInvalidCiphertext))
		assert.True(t, errors.Is(err, credentialcipher.ErrUnsupportedVersion))
		assert.Contains(t, err.Error(), community.DID)
		assert.Contains(t, strings.ToLower(err.Error()), "pgcrypto")
		assert.Nil(t, retrieved)
	})

	t.Run("aggregator DPoP private key", func(t *testing.T) {
		db := testkit.DB(t)
		ctx := context.Background()
		repo := NewAggregatorRepository(db, credentialCipherRepoTestCipher(t))
		aggregatorDID := indexAggregator(t, repo, "Legacy Pgcrypto Aggregator")

		_, err := db.ExecContext(ctx, `
			UPDATE aggregators
			SET oauth_dpop_private_key_encrypted = pgp_sym_encrypt('x', 'k')
			WHERE did = $1
		`, aggregatorDID)
		require.NoError(t, err)

		retrieved, err := repo.GetAggregatorCredentials(ctx, aggregatorDID)
		require.Error(t, err)
		assert.True(t, errors.Is(err, credentialcipher.ErrInvalidCiphertext))
		assert.True(t, errors.Is(err, credentialcipher.ErrUnsupportedVersion))
		assert.Contains(t, err.Error(), aggregatorDID)
		assert.Contains(t, strings.ToLower(err.Error()), "pgcrypto")
		assert.Nil(t, retrieved)
	})
}

func credentialCipherRepoTestCipher(t *testing.T) *credentialcipher.Cipher {
	t.Helper()
	cipher, err := credentialcipher.New(bytes.Repeat([]byte{0x42}, credentialcipher.KeySize))
	require.NoError(t, err)
	return cipher
}

func credentialCipherRepoCommunity(t *testing.T, label string) *communities.Community {
	t.Helper()
	id := testkit.UniqueID(t)
	did := "did:plc:" + id
	return &communities.Community{
		DID:             did,
		Handle:          fmt.Sprintf("!cipher-%s-%s@coves.local", label, id),
		Name:            "credential-cipher-" + label,
		OwnerDID:        did,
		CreatedByDID:    "did:plc:credential-cipher-creator",
		HostedByDID:     "did:web:coves.local",
		Visibility:      "public",
		PDSEmail:        label + "@communities.coves.local",
		PDSPassword:     "community-password-" + label,
		PDSAccessToken:  "community-access-token-" + label,
		PDSRefreshToken: "community-refresh-token-" + label,
		PDSURL:          testkit.Endpoints().PDS.BaseURL,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
}

func communityCredentialCiphertext(t *testing.T, db *sql.DB, did string) (password, access, refresh []byte) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT pds_password_encrypted, pds_access_token_encrypted, pds_refresh_token_encrypted
		FROM communities
		WHERE did = $1`, did).Scan(&password, &access, &refresh))
	return password, access, refresh
}

func assertCredentialCiphertext(
	t *testing.T,
	cipher *credentialcipher.Cipher,
	ciphertext []byte,
	credentialContext string,
	wantPlaintext string,
) {
	t.Helper()
	if assert.NotEmpty(t, ciphertext) {
		assert.Equal(t, byte(0x01), ciphertext[0], "credential is not in the versioned AES-GCM format")
	}
	plaintext, err := cipher.Decrypt(ciphertext, credentialContext)
	if assert.NoError(t, err) {
		assert.Equal(t, wantPlaintext, plaintext)
	}
}

func credentialCipherRepoFindAggregator(
	credentials []*aggregators.AggregatorCredentials,
	did string,
) *aggregators.AggregatorCredentials {
	for _, credential := range credentials {
		if credential.DID == did {
			return credential
		}
	}
	return nil
}
