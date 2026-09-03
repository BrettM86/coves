//go:build integration

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"Coves/internal/crypto/credentialcipher"
	"Coves/internal/db/migrations"
	"Coves/tests/testkit"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	legacyCredentialVersion = byte(0xc3)
	appCredentialVersion    = byte(0x01)
)

func TestMigration046DownRestoresUsableEncryptionKey(t *testing.T) {
	db := testkit.DB(t)
	assert.False(t, credentialReencryptKeyTable(t, db).Valid,
		"migration 046 Up must drop encryption_keys before its Down behavior can be tested")
	require.EqualValues(t, 46, testkit.MigrateDownOne(t, db, 46))

	table := credentialReencryptKeyTable(t, db)
	require.True(t, table.Valid, "migration 046 Down must recreate encryption_keys")

	var keyCount, keyLength int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT COUNT(*), COALESCE(MAX(octet_length(key_data)), 0)
		FROM encryption_keys
	`).Scan(&keyCount, &keyLength))
	assert.Equal(t, 1, keyCount)
	assert.Equal(t, credentialcipher.KeySize, keyLength)
}

func TestMigration046RejectsLegacyCiphertext(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	did := credentialReencryptInsertCommunity(t, db, "migration-legacy")
	credentialReencryptSeedLegacyCommunity(t, db, did,
		"migration legacy password", nil, nil)

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	if assert.Error(t, err, "migration 046 must refuse to drop the only key for legacy ciphertext") {
		assert.Contains(t, strings.ToLower(err.Error()), "re-encrypt")
	}
	assert.True(t, credentialReencryptKeyTable(t, db).Valid,
		"a refused migration must leave encryption_keys available for recovery")
}

func TestMigration046DropsEncryptionKeysAfterLegacyDataRemoved(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	did := credentialReencryptInsertCommunity(t, db, "migration-app-cipher")
	cipher := credentialReencryptCipher(t)
	ciphertext, err := cipher.Encrypt(
		"already encrypted by the application",
		"communities.pds_password_encrypted:"+did,
	)
	require.NoError(t, err)
	require.Equal(t, appCredentialVersion, ciphertext[0])
	_, err = db.ExecContext(ctx,
		`UPDATE communities SET pds_password_encrypted = $2 WHERE did = $1`, did, ciphertext)
	require.NoError(t, err)

	testkit.MigrateUp(t, db)
	assert.False(t, credentialReencryptKeyTable(t, db).Valid)
}

func TestCredentialReencryptRewritesLegacyRowsAndIsIdempotent(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	cipher := credentialReencryptCipher(t)

	communityAllDID := credentialReencryptInsertCommunity(t, db, "community-all")
	communityPasswordDID := credentialReencryptInsertCommunity(t, db, "community-password")
	aggregatorAllDID := credentialReencryptInsertAggregator(t, db, "aggregator-all")
	aggregatorEmptyDID := credentialReencryptInsertAggregator(t, db, "aggregator-empty")

	communityPassword := "community all password"
	communityAccess := "community all access"
	communityRefresh := "community all refresh"
	passwordOnly := "community password only"
	aggregatorAccess := "aggregator all access"
	aggregatorRefresh := "aggregator all refresh"
	aggregatorDPoP := "aggregator all dpop"
	credentialReencryptSeedLegacyCommunity(t, db, communityAllDID,
		communityPassword, &communityAccess, &communityRefresh)
	credentialReencryptSeedLegacyCommunity(t, db, communityPasswordDID,
		passwordOnly, nil, nil)
	credentialReencryptSeedLegacyAggregator(t, db, aggregatorAllDID,
		&aggregatorAccess, &aggregatorRefresh, &aggregatorDPoP)

	// Counts are rows with at least one value rewritten, not credential columns:
	// three values in one row still contribute one to its table's count.
	report, err := ReencryptLegacyCredentials(ctx, db, cipher, false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{CommunitiesRewritten: 2, AggregatorsRewritten: 1}, report)

	communityAll := credentialReencryptCommunityBytes(t, db, communityAllDID)
	credentialReencryptAssertAppCiphertext(t, cipher, communityAll.password,
		"communities.pds_password_encrypted:"+communityAllDID, communityPassword)
	credentialReencryptAssertAppCiphertext(t, cipher, communityAll.access,
		"communities.pds_access_token_encrypted:"+communityAllDID, communityAccess)
	credentialReencryptAssertAppCiphertext(t, cipher, communityAll.refresh,
		"communities.pds_refresh_token_encrypted:"+communityAllDID, communityRefresh)

	communityPasswordOnly := credentialReencryptCommunityBytes(t, db, communityPasswordDID)
	credentialReencryptAssertAppCiphertext(t, cipher, communityPasswordOnly.password,
		"communities.pds_password_encrypted:"+communityPasswordDID, passwordOnly)
	assert.Nil(t, communityPasswordOnly.access)
	assert.Nil(t, communityPasswordOnly.refresh)

	aggregatorAll := credentialReencryptAggregatorBytes(t, db, aggregatorAllDID)
	credentialReencryptAssertAppCiphertext(t, cipher, aggregatorAll.access,
		"aggregators.oauth_access_token_encrypted:"+aggregatorAllDID, aggregatorAccess)
	credentialReencryptAssertAppCiphertext(t, cipher, aggregatorAll.refresh,
		"aggregators.oauth_refresh_token_encrypted:"+aggregatorAllDID, aggregatorRefresh)
	credentialReencryptAssertAppCiphertext(t, cipher, aggregatorAll.dpop,
		"aggregators.oauth_dpop_private_key_encrypted:"+aggregatorAllDID, aggregatorDPoP)

	aggregatorEmpty := credentialReencryptAggregatorBytes(t, db, aggregatorEmptyDID)
	assert.Nil(t, aggregatorEmpty.access)
	assert.Nil(t, aggregatorEmpty.refresh)
	assert.Nil(t, aggregatorEmpty.dpop)

	secondReport, err := ReencryptLegacyCredentials(ctx, db, cipher, false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{}, secondReport)
	assert.Equal(t, communityAll, credentialReencryptCommunityBytes(t, db, communityAllDID))
	assert.Equal(t, communityPasswordOnly, credentialReencryptCommunityBytes(t, db, communityPasswordDID))
	assert.Equal(t, aggregatorAll, credentialReencryptAggregatorBytes(t, db, aggregatorAllDID))
	assert.Equal(t, aggregatorEmpty, credentialReencryptAggregatorBytes(t, db, aggregatorEmptyDID))
}

func TestCredentialReencryptLeavesAppCipherRowsAndRewritesLegacySiblings(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	cipher := credentialReencryptCipher(t)
	appDID := credentialReencryptInsertCommunity(t, db, "mixed-app")
	legacyDID := credentialReencryptInsertCommunity(t, db, "mixed-legacy")

	const appPassword = "password already using application encryption"
	appBytes, err := cipher.Encrypt(appPassword,
		"communities.pds_password_encrypted:"+appDID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`UPDATE communities SET pds_password_encrypted = $2 WHERE did = $1`, appDID, appBytes)
	require.NoError(t, err)
	const legacyPassword = "password still using pgcrypto"
	credentialReencryptSeedLegacyCommunity(t, db, legacyDID, legacyPassword, nil, nil)

	report, err := ReencryptLegacyCredentials(ctx, db, cipher, false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{CommunitiesRewritten: 1}, report)
	assert.Equal(t, appBytes, credentialReencryptCommunityBytes(t, db, appDID).password,
		"already migrated ciphertext must not be resealed with a new nonce")
	credentialReencryptAssertAppCiphertext(t, cipher,
		credentialReencryptCommunityBytes(t, db, legacyDID).password,
		"communities.pds_password_encrypted:"+legacyDID, legacyPassword)
}

func TestCredentialReencryptRejectsEphemeralKeyWhenLegacyRowsExist(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	cipher := credentialReencryptCipher(t)
	did := credentialReencryptInsertCommunity(t, db, "ephemeral-legacy")
	credentialReencryptSeedLegacyCommunity(t, db, did, "legacy password", nil, nil)
	before := credentialReencryptCommunityBytes(t, db, did)

	report, err := ReencryptLegacyCredentials(ctx, db, cipher, true)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "ENCRYPTION_KEY")
	}
	assert.Equal(t, ReencryptReport{}, report)
	assert.Equal(t, before, credentialReencryptCommunityBytes(t, db, did))
	assert.Equal(t, legacyCredentialVersion,
		credentialReencryptCommunityBytes(t, db, did).password[0])
	assert.True(t, credentialReencryptKeyTable(t, db).Valid)
}

func TestCredentialReencryptAllowsEphemeralKeyWithoutLegacyRows(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	cipher := credentialReencryptCipher(t)
	did := credentialReencryptInsertCommunity(t, db, "ephemeral-app")
	ciphertext, err := cipher.Encrypt("already migrated",
		"communities.pds_password_encrypted:"+did)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`UPDATE communities SET pds_password_encrypted = $2 WHERE did = $1`, did, ciphertext)
	require.NoError(t, err)

	report, err := ReencryptLegacyCredentials(context.Background(), db, cipher, true)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{}, report)
	assert.Equal(t, ciphertext, credentialReencryptCommunityBytes(t, db, did).password)
}

func TestCredentialReencryptRollsBackEveryRowOnCorruptLegacyCiphertext(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	cipher := credentialReencryptCipher(t)
	validDID := credentialReencryptInsertCommunity(t, db, "transaction-valid")
	corruptDID := credentialReencryptInsertAggregator(t, db, "transaction-corrupt")
	credentialReencryptSeedLegacyCommunity(t, db, validDID, "valid sibling password", nil, nil)

	const corruptPlaintext = "plaintext-that-must-not-leak"
	_, err := db.ExecContext(ctx, `
		UPDATE aggregators
		SET oauth_dpop_private_key_encrypted = pgp_sym_encrypt($2, 'some-other-key')
		WHERE did = $1
	`, corruptDID, corruptPlaintext)
	require.NoError(t, err)
	corruptBefore := credentialReencryptAggregatorBytes(t, db, corruptDID)
	credentialReencryptRequireLegacy(t, corruptBefore.dpop)
	validBefore := credentialReencryptCommunityBytes(t, db, validDID)

	report, err := ReencryptLegacyCredentials(ctx, db, cipher, false)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "aggregators")
		assert.Contains(t, err.Error(), corruptDID)
		assert.NotContains(t, err.Error(), corruptPlaintext)
	}
	assert.Equal(t, ReencryptReport{}, report)
	assert.Equal(t, validBefore, credentialReencryptCommunityBytes(t, db, validDID),
		"a later corrupt row must roll back an earlier valid conversion")
	assert.Equal(t, corruptBefore, credentialReencryptAggregatorBytes(t, db, corruptDID))
	assert.Equal(t, legacyCredentialVersion,
		credentialReencryptCommunityBytes(t, db, validDID).password[0])
}

func TestCredentialReencryptIsNoOpWithoutEncryptionKeysTable(t *testing.T) {
	db := testkit.DB(t)
	assert.False(t, credentialReencryptKeyTable(t, db).Valid,
		"the version-46 template must not contain encryption_keys")

	report, err := ReencryptLegacyCredentials(
		context.Background(), db, credentialReencryptCipher(t), false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{}, report)
}

func TestCredentialReencryptHandsOffToMigration046(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	cipher := credentialReencryptCipher(t)
	did := credentialReencryptInsertCommunity(t, db, "handoff")
	const password = "handoff legacy password"
	credentialReencryptSeedLegacyCommunity(t, db, did, password, nil, nil)

	report, err := ReencryptLegacyCredentials(ctx, db, cipher, false)
	require.NoError(t, err)
	require.Equal(t, ReencryptReport{CommunitiesRewritten: 1}, report)
	credentialReencryptAssertAppCiphertext(t, cipher,
		credentialReencryptCommunityBytes(t, db, did).password,
		"communities.pds_password_encrypted:"+did, password)

	testkit.MigrateUp(t, db)
	assert.False(t, credentialReencryptKeyTable(t, db).Valid)
}

func TestCredentialReencryptConvertsCommunityBeforeAggregatorCredentialColumnsExist(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	require.NoError(t, err)
	_, err = provider.DownTo(ctx, 24)
	require.NoError(t, err)
	require.True(t, credentialReencryptKeyTable(t, db).Valid)

	var aggregatorCredentialColumnCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
			AND table_name = 'aggregators'
			AND column_name IN (
				'oauth_access_token_encrypted',
				'oauth_refresh_token_encrypted',
				'oauth_dpop_private_key_encrypted'
			)
	`).Scan(&aggregatorCredentialColumnCount))
	require.Zero(t, aggregatorCredentialColumnCount)

	did := credentialReencryptInsertCommunity(t, db, "pre-025")
	const password = "legacy password before aggregator encryption"
	credentialReencryptSeedLegacyCommunity(t, db, did, password, nil, nil)
	cipher := credentialReencryptCipher(t)

	report, err := ReencryptLegacyCredentials(ctx, db, cipher, false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{CommunitiesRewritten: 1}, report)
	credentialReencryptAssertAppCiphertext(t, cipher,
		credentialReencryptCommunityBytes(t, db, did).password,
		"communities.pds_password_encrypted:"+did, password)

	testkit.MigrateUp(t, db)
	assert.False(t, credentialReencryptKeyTable(t, db).Valid)
}

func TestCredentialReencryptSkipsUnavailableColumnsAtLowestReachableSchema(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	require.NoError(t, err)
	_, err = provider.DownTo(ctx, 15)
	require.NoError(t, err)
	require.True(t, credentialReencryptKeyTable(t, db).Valid)

	var aggregatorCredentialColumnCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
			AND table_name = 'aggregators'
			AND column_name IN (
				'oauth_access_token_encrypted',
				'oauth_refresh_token_encrypted',
				'oauth_dpop_private_key_encrypted'
			)
	`).Scan(&aggregatorCredentialColumnCount))
	require.Zero(t, aggregatorCredentialColumnCount)

	report, err := ReencryptLegacyCredentials(ctx, db, credentialReencryptCipher(t), false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{}, report)
}

func TestCredentialReencryptNormalizesZeroLengthCredentialsToNull(t *testing.T) {
	db := credentialReencryptVersion45Database(t)
	ctx := context.Background()
	communityDID := credentialReencryptInsertCommunity(t, db, "zero-length")
	aggregatorDID := credentialReencryptInsertAggregator(t, db, "zero-length")
	_, err := db.ExecContext(ctx,
		`UPDATE communities SET pds_password_encrypted = ''::bytea WHERE did = $1`, communityDID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`UPDATE aggregators SET oauth_refresh_token_encrypted = ''::bytea WHERE did = $1`, aggregatorDID)
	require.NoError(t, err)
	communityBefore := credentialReencryptCommunityBytes(t, db, communityDID)
	aggregatorBefore := credentialReencryptAggregatorBytes(t, db, aggregatorDID)
	require.NotNil(t, communityBefore.password)
	require.Empty(t, communityBefore.password)
	require.NotNil(t, aggregatorBefore.refresh)
	require.Empty(t, aggregatorBefore.refresh)

	report, err := ReencryptLegacyCredentials(ctx, db, credentialReencryptCipher(t), false)
	require.NoError(t, err)
	assert.Equal(t, ReencryptReport{CommunitiesRewritten: 1, AggregatorsRewritten: 1}, report)
	assert.Nil(t, credentialReencryptCommunityBytes(t, db, communityDID).password)
	assert.Nil(t, credentialReencryptAggregatorBytes(t, db, aggregatorDID).refresh)

	testkit.MigrateUp(t, db)
	assert.False(t, credentialReencryptKeyTable(t, db).Valid)
}

func TestCredentialReencryptRejectsLegacyRowsAfterEncryptionKeysDropped(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	require.False(t, credentialReencryptKeyTable(t, db).Valid)
	did := credentialReencryptInsertCommunity(t, db, "orphaned-legacy")
	_, err := db.ExecContext(ctx, `
		UPDATE communities
		SET pds_access_token_encrypted = pgp_sym_encrypt('stale', 'unrelated-key')
		WHERE did = $1
	`, did)
	require.NoError(t, err)
	before := credentialReencryptCommunityBytes(t, db, did)
	credentialReencryptRequireLegacy(t, before.access)

	report, err := ReencryptLegacyCredentials(ctx, db, credentialReencryptCipher(t), false)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "encryption_keys")
	assert.Contains(t, strings.ToLower(err.Error()), "communities")
	assert.Equal(t, ReencryptReport{}, report)
	assert.Equal(t, before, credentialReencryptCommunityBytes(t, db, did))
}

func credentialReencryptVersion45Database(t *testing.T) *sql.DB {
	t.Helper()
	db := testkit.DB(t)
	require.EqualValues(t, 46, testkit.MigrateDownOne(t, db, 46))
	return db
}

func credentialReencryptCipher(t *testing.T) *credentialcipher.Cipher {
	t.Helper()
	cipher, err := credentialcipher.New(bytes.Repeat([]byte{0x71}, credentialcipher.KeySize))
	require.NoError(t, err)
	return cipher
}

func credentialReencryptKeyTable(t *testing.T, db *sql.DB) sql.NullString {
	t.Helper()
	var table sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT to_regclass('encryption_keys')`).Scan(&table))
	return table
}

func credentialReencryptInsertCommunity(t *testing.T, db *sql.DB, label string) string {
	t.Helper()
	id := testkit.UniqueID(t)
	did := "did:plc:" + id
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO communities (
			did, handle, name, owner_did, created_by_did, hosted_by_did, created_at
		) VALUES ($1, $2, $3, $1, $1, $1, NOW())
	`, did, "c-"+label+"-"+id+".coves.social", "credential-"+label)
	require.NoError(t, err)
	return did
}

func credentialReencryptInsertAggregator(t *testing.T, db *sql.DB, label string) string {
	t.Helper()
	id := testkit.UniqueID(t)
	did := "did:plc:" + id
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO aggregators (did, display_name, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, did, "Credential "+label, "at://"+did+"/social.coves.aggregator.service/self", "bafy"+id)
	require.NoError(t, err)
	return did
}

type credentialReencryptCommunityRaw struct {
	password []byte
	access   []byte
	refresh  []byte
}

func credentialReencryptCommunityBytes(t *testing.T, db *sql.DB, did string) credentialReencryptCommunityRaw {
	t.Helper()
	var raw credentialReencryptCommunityRaw
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT pds_password_encrypted, pds_access_token_encrypted, pds_refresh_token_encrypted
		FROM communities
		WHERE did = $1
	`, did).Scan(&raw.password, &raw.access, &raw.refresh))
	return raw
}

type credentialReencryptAggregatorRaw struct {
	access  []byte
	refresh []byte
	dpop    []byte
}

func credentialReencryptAggregatorBytes(t *testing.T, db *sql.DB, did string) credentialReencryptAggregatorRaw {
	t.Helper()
	var raw credentialReencryptAggregatorRaw
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT oauth_access_token_encrypted, oauth_refresh_token_encrypted,
			oauth_dpop_private_key_encrypted
		FROM aggregators
		WHERE did = $1
	`, did).Scan(&raw.access, &raw.refresh, &raw.dpop))
	return raw
}

func credentialReencryptSeedLegacyCommunity(
	t *testing.T,
	db *sql.DB,
	did string,
	password string,
	accessToken *string,
	refreshToken *string,
) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		UPDATE communities SET
			pds_password_encrypted = pgp_sym_encrypt($2, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)),
			pds_access_token_encrypted = CASE WHEN $3::text IS NULL THEN NULL ELSE pgp_sym_encrypt($3::text, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) END,
			pds_refresh_token_encrypted = CASE WHEN $4::text IS NULL THEN NULL ELSE pgp_sym_encrypt($4::text, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) END
		WHERE did = $1
	`, did, password, credentialReencryptNullableString(accessToken), credentialReencryptNullableString(refreshToken))
	require.NoError(t, err)
	raw := credentialReencryptCommunityBytes(t, db, did)
	credentialReencryptRequireLegacy(t, raw.password)
	if accessToken == nil {
		require.Nil(t, raw.access)
	} else {
		credentialReencryptRequireLegacy(t, raw.access)
	}
	if refreshToken == nil {
		require.Nil(t, raw.refresh)
	} else {
		credentialReencryptRequireLegacy(t, raw.refresh)
	}
}

func credentialReencryptSeedLegacyAggregator(
	t *testing.T,
	db *sql.DB,
	did string,
	accessToken *string,
	refreshToken *string,
	dpopPrivateKey *string,
) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		UPDATE aggregators SET
			oauth_access_token_encrypted = CASE WHEN $2::text IS NULL THEN NULL ELSE pgp_sym_encrypt($2::text, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) END,
			oauth_refresh_token_encrypted = CASE WHEN $3::text IS NULL THEN NULL ELSE pgp_sym_encrypt($3::text, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) END,
			oauth_dpop_private_key_encrypted = CASE WHEN $4::text IS NULL THEN NULL ELSE pgp_sym_encrypt($4::text, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) END
		WHERE did = $1
	`, did, credentialReencryptNullableString(accessToken), credentialReencryptNullableString(refreshToken),
		credentialReencryptNullableString(dpopPrivateKey))
	require.NoError(t, err)
	raw := credentialReencryptAggregatorBytes(t, db, did)
	for _, credential := range []struct {
		value      *string
		ciphertext []byte
	}{
		{value: accessToken, ciphertext: raw.access},
		{value: refreshToken, ciphertext: raw.refresh},
		{value: dpopPrivateKey, ciphertext: raw.dpop},
	} {
		if credential.value == nil {
			require.Nil(t, credential.ciphertext)
		} else {
			credentialReencryptRequireLegacy(t, credential.ciphertext)
		}
	}
}

func credentialReencryptNullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func credentialReencryptRequireLegacy(t *testing.T, ciphertext []byte) {
	t.Helper()
	require.NotEmpty(t, ciphertext, "legacy seed unexpectedly stored NULL")
	require.Equal(t, legacyCredentialVersion, ciphertext[0],
		"fixture must prove it seeded pgcrypto data rather than creating a false-green NULL")
}

func credentialReencryptAssertAppCiphertext(
	t *testing.T,
	cipher *credentialcipher.Cipher,
	ciphertext []byte,
	credentialContext string,
	wantPlaintext string,
) {
	t.Helper()
	if assert.NotEmpty(t, ciphertext) {
		assert.Equal(t, appCredentialVersion, ciphertext[0])
	}
	plaintext, err := cipher.Decrypt(ciphertext, credentialContext)
	if assert.NoError(t, err) {
		assert.Equal(t, wantPlaintext, plaintext)
	}
}
