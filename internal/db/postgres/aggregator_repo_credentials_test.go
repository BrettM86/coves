//go:build integration

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/aggregators"
	"Coves/tests/testkit"
)

// The aggregator credential store: API keys, the OAuth session behind them, and
// the background job that keeps that session alive.
//
// This is the only place in the AppView where a bearer secret is exchanged for
// an identity. An aggregator presents an API key on every write; the middleware
// hashes it and asks this repository who owns that hash. Everything downstream —
// which community the bot may post into, whose quota the post spends, which PDS
// the write is forwarded to — is decided from the DID that comes back. So the
// failure modes worth testing are not "does the column round-trip" but:
//
//   - Does a hash nobody holds return an ERROR, rather than a zero-valued
//     struct with a nil error? A repository that answered (&Aggregator{}, nil)
//     would authenticate every unknown key as the aggregator with the empty
//     DID. Both hash lookups are asserted for this explicitly.
//   - Does revocation actually stop authentication? Reading api_key_revoked_at
//     back proves the column was written and nothing else; the claim that
//     matters is that the lookup path refuses the key afterwards, so every
//     revocation assertion here goes through GetByAPIKeyHash or
//     GetCredentialsByAPIKeyHash rather than through the column.
//   - Does one aggregator's write reach another's row? Every mutator is a bare
//     UPDATE … WHERE did = $1, and the cost of a missing predicate is one bot
//     holding another bot's PDS session.
//
// The OAuth tokens and the DPoP private key are encrypted at rest by Postgres
// (pgp_sym_encrypt against the key seeded by migration 006, moved to these
// columns by 025). That makes the encryption part of the SQL rather than part
// of any Go code, which in turn makes a repository test the only place it is
// exercised at all — a mocked repository stores plaintext strings and proves
// nothing. Where it matters, the ciphertext column is read directly to confirm
// the plaintext is not sitting in it.

// aggregatorAPIKeyHash returns a value shaped like what the middleware actually
// stores: the hex SHA-256 of the presented key, which is exactly the 64
// characters api_key_hash is declared to hold.
func aggregatorAPIKeyHash(t *testing.T, key string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// aggregatorOAuthSession builds a complete OAuth session. Every field is
// distinct and carries the label, so an assertion can never pass because two
// columns happen to hold the same string.
func aggregatorOAuthSession(label string, expiresAt time.Time) *aggregators.OAuthCredentials {
	return &aggregators.OAuthCredentials{
		AccessToken:             "access-token-" + label,
		RefreshToken:            "refresh-token-" + label,
		TokenExpiresAt:          expiresAt,
		PDSURL:                  "https://pds-" + label + ".example.com",
		AuthServerIss:           "https://auth-" + label + ".example.com",
		AuthServerTokenEndpoint: "https://auth-" + label + ".example.com/token",
		DPoPPrivateKeyMultibase: "zdpop-private-key-" + label,
		DPoPAuthServerNonce:     "authserver-nonce-" + label,
		DPoPPDSNonce:            "pds-nonce-" + label,
	}
}

// aggregatorKeyPrefix renders the non-secret log-correlation prefix within the
// 12 characters api_key_prefix is declared to hold, so a long test label cannot
// turn into a value-too-long failure that has nothing to do with the assertion.
func aggregatorKeyPrefix(label string) string {
	prefix := "cvs_" + label
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return prefix
}

// aggregatorWithAPIKey indexes an aggregator and completes the OAuth handshake
// for it, returning the DID and the hash a caller would authenticate with.
func aggregatorWithAPIKey(t *testing.T, repo aggregators.Repository, label string, expiresAt time.Time) (did, keyHash string) {
	t.Helper()

	did = indexAggregator(t, repo, "Keyed "+label)
	keyHash = aggregatorAPIKeyHash(t, "secret-"+label+"-"+did)
	require.NoError(t, repo.SetAPIKey(context.Background(), did, aggregatorKeyPrefix(label), keyHash,
		aggregatorOAuthSession(label, expiresAt)))
	return did, keyHash
}

// aggregatorCiphertext reads the three at-rest-encrypted columns straight out
// of the row, which is the only way to tell "encrypted" from "the round trip
// happens to work".
func aggregatorCiphertext(t *testing.T, db *sql.DB, did string) (access, refresh, dpop []byte) {
	t.Helper()
	require.NoError(t, db.QueryRow(`
		SELECT oauth_access_token_encrypted, oauth_refresh_token_encrypted, oauth_dpop_private_key_encrypted
		FROM aggregators WHERE did = $1`, did).Scan(&access, &refresh, &dpop))
	return access, refresh, dpop
}

func TestAggregatorRepo_SetAPIKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("stores the whole OAuth session behind the key", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Fresh Aggregator")
		expires := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Microsecond)
		session := aggregatorOAuthSession("full", expires)
		keyHash := aggregatorAPIKeyHash(t, "secret-full")

		require.NoError(t, repo.SetAPIKey(ctx, did, "cvs_full", keyHash, session))

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)

		assert.Equal(t, "cvs_full", creds.APIKeyPrefix)
		assert.Equal(t, keyHash, creds.APIKeyHash)
		assert.Equal(t, session.AccessToken, creds.OAuthAccessToken,
			"the access token is what signs the write to the aggregator's PDS; a token that does not "+
				"survive the round trip through pgp_sym_encrypt leaves the bot authenticated to Coves "+
				"and unable to write anywhere")
		assert.Equal(t, session.RefreshToken, creds.OAuthRefreshToken)
		assert.Equal(t, session.PDSURL, creds.OAuthPDSURL)
		assert.Equal(t, session.AuthServerIss, creds.OAuthAuthServerIss)
		assert.Equal(t, session.AuthServerTokenEndpoint, creds.OAuthAuthServerTokenEndpoint)
		assert.Equal(t, session.DPoPPrivateKeyMultibase, creds.OAuthDPoPPrivateKeyMultibase,
			"without the DPoP private key the refresh job cannot prove possession, and every stored "+
				"session becomes unrenewable the moment its access token lapses")
		assert.Equal(t, session.DPoPAuthServerNonce, creds.OAuthDPoPAuthServerNonce)
		assert.Equal(t, session.DPoPPDSNonce, creds.OAuthDPoPPDSNonce)

		require.NotNil(t, creds.OAuthTokenExpiresAt)
		assert.True(t, creds.OAuthTokenExpiresAt.Equal(expires),
			"expiry = %v, want %v", *creds.OAuthTokenExpiresAt, expires)
		require.NotNil(t, creds.APIKeyCreatedAt, "api_key_created_at is the audit trail's only record "+
			"of when this credential came into existence")
		assert.WithinDuration(t, time.Now(), *creds.APIKeyCreatedAt, time.Minute)
		assert.Nil(t, creds.APIKeyRevokedAt)
		assert.Nil(t, creds.APIKeyLastUsed, "a key that has never authenticated has never been used")
		assert.True(t, creds.HasActiveAPIKey())
	})

	// The three sensitive columns are BYTEA holding pgp_sym_encrypt output. If a
	// future migration or a refactor made them plain text, every assertion above
	// would still pass — the round trip would simply be an identity function —
	// and a database backup would carry every aggregator's PDS credentials in
	// the clear.
	t.Run("keeps the secrets out of the clear in the row itself", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Encrypted Aggregator")
		session := aggregatorOAuthSession("cipher", time.Now().Add(time.Hour))
		require.NoError(t, repo.SetAPIKey(ctx, did, "cvs_cipher", aggregatorAPIKeyHash(t, "secret-cipher"), session))

		access, refresh, dpop := aggregatorCiphertext(t, db, did)
		require.NotEmpty(t, access)
		assert.False(t, bytes.Contains(access, []byte(session.AccessToken)),
			"the access token is readable in oauth_access_token_encrypted")
		assert.False(t, bytes.Contains(refresh, []byte(session.RefreshToken)),
			"the refresh token is readable in oauth_refresh_token_encrypted")
		assert.False(t, bytes.Contains(dpop, []byte(session.DPoPPrivateKeyMultibase)),
			"the DPoP private key is readable in oauth_dpop_private_key_encrypted")
	})

	// The three CASE WHEN $n != '' guards. An OAuth flow that produced no
	// refresh token must leave NULL rather than the ciphertext of "": NULL is
	// what ListAggregatorsNeedingTokenRefresh and the credential reader use to
	// tell "no token" from "a token that happens to be empty".
	t.Run("an absent token is NULL, not encrypted emptiness", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Tokenless Aggregator")
		session := aggregatorOAuthSession("sparse", time.Now().Add(time.Hour))
		session.AccessToken = ""
		session.RefreshToken = ""
		session.DPoPPrivateKeyMultibase = ""
		require.NoError(t, repo.SetAPIKey(ctx, did, "cvs_sparse", aggregatorAPIKeyHash(t, "secret-sparse"), session))

		access, refresh, dpop := aggregatorCiphertext(t, db, did)
		assert.Nil(t, access)
		assert.Nil(t, refresh)
		assert.Nil(t, dpop)

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Empty(t, creds.OAuthAccessToken, "a NULL ciphertext must read back as the empty string, "+
			"not as a decryption error that would take the whole credential lookup down")
		assert.Empty(t, creds.OAuthRefreshToken)
		assert.Empty(t, creds.OAuthDPoPPrivateKeyMultibase)
	})

	// Re-keying is the recovery path after a leak: the aggregator runs the OAuth
	// flow again and receives a new key. The old one must stop working in the
	// same instant, and the only proof of that is the authenticating lookup.
	t.Run("a new key supersedes the old one", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Rekeyed Aggregator")
		leaked := aggregatorAPIKeyHash(t, "leaked-key")
		require.NoError(t, repo.SetAPIKey(ctx, did, "cvs_old", leaked,
			aggregatorOAuthSession("old", time.Now().Add(time.Hour))))

		replacement := aggregatorAPIKeyHash(t, "replacement-key")
		require.NoError(t, repo.SetAPIKey(ctx, did, "cvs_new", replacement,
			aggregatorOAuthSession("new", time.Now().Add(2*time.Hour))))

		_, err := repo.GetByAPIKeyHash(ctx, leaked)
		assert.ErrorIs(t, err, aggregators.ErrAggregatorNotFound,
			"the superseded key still authenticates; re-keying after a leak would not revoke anything")

		authenticated, err := repo.GetByAPIKeyHash(ctx, replacement)
		require.NoError(t, err)
		assert.Equal(t, did, authenticated.DID)

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "access-token-new", creds.OAuthAccessToken,
			"the replacement session must overwrite the old one, not sit beside it")
	})

	// api_key_revoked_at = NULL is in the UPDATE list on purpose: an aggregator
	// whose key was revoked completes OAuth again, and the row it lands on still
	// carries the revocation. Without the reset the new key would be born dead.
	t.Run("re-keying clears an earlier revocation", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "revived", time.Now().Add(time.Hour))
		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		reissued := aggregatorAPIKeyHash(t, "reissued-key")
		require.NoError(t, repo.SetAPIKey(ctx, did, "cvs_again", reissued,
			aggregatorOAuthSession("again", time.Now().Add(time.Hour))))

		authenticated, err := repo.GetByAPIKeyHash(ctx, reissued)
		require.NoError(t, err, "the reissued key is refused; the aggregator can never recover from a revocation")
		assert.Equal(t, did, authenticated.DID)

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Nil(t, creds.APIKeyRevokedAt)
	})

	t.Run("writes to one aggregator only", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		subject := indexAggregator(t, repo, "Subject")
		bystander, bystanderHash := aggregatorWithAPIKey(t, repo, "bystander", time.Now().Add(time.Hour))

		require.NoError(t, repo.SetAPIKey(ctx, subject, "cvs_subj", aggregatorAPIKeyHash(t, "subject-key"),
			aggregatorOAuthSession("subject", time.Now().Add(time.Hour))))

		untouched, err := repo.GetAggregatorCredentials(ctx, bystander)
		require.NoError(t, err)
		assert.Equal(t, bystanderHash, untouched.APIKeyHash,
			"one aggregator's OAuth completion overwrote another's credentials")
		assert.Equal(t, "access-token-bystander", untouched.OAuthAccessToken)
	})

	t.Run("reports an aggregator the AppView never indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		err := repo.SetAPIKey(ctx, "did:plc:"+testkit.UniqueID(t), "cvs_ghost",
			aggregatorAPIKeyHash(t, "ghost-key"), aggregatorOAuthSession("ghost", time.Now().Add(time.Hour)))
		assert.ErrorIs(t, err, aggregators.ErrAggregatorNotFound,
			"an UPDATE matching no rows must be reported: silently returning nil would tell the OAuth "+
				"callback that a key was issued, and hand the caller a secret that authenticates nothing")
	})
}

func TestAggregatorRepo_GetByAPIKeyHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("answers with the aggregator that owns the hash and no other", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		firstDID, firstHash := aggregatorWithAPIKey(t, repo, "first", time.Now().Add(time.Hour))
		secondDID, secondHash := aggregatorWithAPIKey(t, repo, "second", time.Now().Add(time.Hour))

		first, err := repo.GetByAPIKeyHash(ctx, firstHash)
		require.NoError(t, err)
		assert.Equal(t, firstDID, first.DID,
			"the DID this returns is the identity every later authorization decision is made against")
		assert.Equal(t, "Keyed first", first.DisplayName)

		second, err := repo.GetByAPIKeyHash(ctx, secondHash)
		require.NoError(t, err)
		assert.Equal(t, secondDID, second.DID)
	})

	// The zero-value-plus-nil-error shape, asserted head on. A caller that only
	// checked `err != nil` and then read `agg.DID` would, under that bug,
	// authenticate an attacker's arbitrary key as the aggregator with the empty
	// DID — and the empty DID is what an unset column scans into everywhere else
	// in this package.
	t.Run("a hash nobody holds is an error, not an empty aggregator", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)
		aggregatorWithAPIKey(t, repo, "legit", time.Now().Add(time.Hour))

		guessed, err := repo.GetByAPIKeyHash(ctx, aggregatorAPIKeyHash(t, "a-key-nobody-issued"))
		require.ErrorIs(t, err, aggregators.ErrAggregatorNotFound)
		assert.Nil(t, guessed, "a non-nil aggregator alongside an error invites a caller that checks "+
			"only one of the two")
	})

	// An empty presented key hashes to something, but a caller that skipped the
	// hashing step entirely would arrive here with "". No aggregator may match
	// it — least of all one whose api_key_hash is NULL because it never
	// completed OAuth.
	t.Run("an empty hash matches nobody, including aggregators with no key", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)
		indexAggregator(t, repo, "Never Completed OAuth")

		_, err := repo.GetByAPIKeyHash(ctx, "")
		assert.ErrorIs(t, err, aggregators.ErrAggregatorNotFound)
	})

	t.Run("a revoked key stops authenticating", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "doomed", time.Now().Add(time.Hour))
		before, err := repo.GetByAPIKeyHash(ctx, keyHash)
		require.NoError(t, err)
		require.Equal(t, did, before.DID)

		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		after, err := repo.GetByAPIKeyHash(ctx, keyHash)
		assert.ErrorIs(t, err, aggregators.ErrAPIKeyRevoked,
			"revocation is only real if the authenticating lookup refuses the key")
		assert.Nil(t, after, "a revoked lookup must hand back no aggregator at all; returning the row "+
			"beside the error is how a caller ends up acting on it")
	})

	// ErrAPIKeyRevoked and ErrAggregatorNotFound are different answers on
	// purpose: the first means "this key was yours and is not any more", which
	// the operator needs to see in the logs, and the second means "this key was
	// never anyone's".
	t.Run("distinguishes a revoked key from an unknown one", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "classify", time.Now().Add(time.Hour))
		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		_, revokedErr := repo.GetByAPIKeyHash(ctx, keyHash)
		_, unknownErr := repo.GetByAPIKeyHash(ctx, aggregatorAPIKeyHash(t, "never-issued"))
		assert.ErrorIs(t, revokedErr, aggregators.ErrAPIKeyRevoked)
		assert.NotErrorIs(t, revokedErr, aggregators.ErrAggregatorNotFound)
		assert.ErrorIs(t, unknownErr, aggregators.ErrAggregatorNotFound)
		assert.NotErrorIs(t, unknownErr, aggregators.ErrAPIKeyRevoked)
	})
}

func TestAggregatorRepo_GetCredentialsByAPIKeyHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("hands the owner's session to the authenticated caller", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		mineDID, mineHash := aggregatorWithAPIKey(t, repo, "mine", time.Now().Add(time.Hour))
		aggregatorWithAPIKey(t, repo, "theirs", time.Now().Add(time.Hour))

		creds, err := repo.GetCredentialsByAPIKeyHash(ctx, mineHash)
		require.NoError(t, err)
		assert.Equal(t, mineDID, creds.DID)
		assert.Equal(t, "access-token-mine", creds.OAuthAccessToken,
			"the credentials returned for a key must be the key owner's; handing back another "+
				"aggregator's session would let one bot write to another bot's repository")
		assert.Equal(t, "refresh-token-mine", creds.OAuthRefreshToken)
		assert.Equal(t, "zdpop-private-key-mine", creds.OAuthDPoPPrivateKeyMultibase)
		assert.Equal(t, mineHash, creds.APIKeyHash)
		assert.True(t, creds.HasActiveAPIKey())
	})

	// A different domain error from GetByAPIKeyHash for the same condition, and
	// deliberately so: this is the middleware's path, where "invalid API key" is
	// the answer the client gets, while GetByAPIKeyHash serves lookups that have
	// already established the aggregator exists.
	t.Run("an unknown hash is an invalid key rather than a missing aggregator", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)
		aggregatorWithAPIKey(t, repo, "present", time.Now().Add(time.Hour))

		creds, err := repo.GetCredentialsByAPIKeyHash(ctx, aggregatorAPIKeyHash(t, "forged"))
		require.ErrorIs(t, err, aggregators.ErrAPIKeyInvalid)
		assert.Nil(t, creds, "credentials alongside an error is a token handed to an unauthenticated caller")
	})

	t.Run("withholds the session when the key is revoked", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "cancelled", time.Now().Add(time.Hour))
		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		creds, err := repo.GetCredentialsByAPIKeyHash(ctx, keyHash)
		require.ErrorIs(t, err, aggregators.ErrAPIKeyRevoked)
		assert.Nil(t, creds, "a revoked key must yield no OAuth tokens; the row is still there and "+
			"still decryptable, so only this guard stands between a withdrawn credential and a live "+
			"PDS session")
	})
}

func TestAggregatorRepo_RevokeAPIKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("revokes only the aggregator named", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		targetDID, _ := aggregatorWithAPIKey(t, repo, "target", time.Now().Add(time.Hour))
		_, bystanderHash := aggregatorWithAPIKey(t, repo, "spared", time.Now().Add(time.Hour))

		require.NoError(t, repo.RevokeAPIKey(ctx, targetDID))

		spared, err := repo.GetByAPIKeyHash(ctx, bystanderHash)
		require.NoError(t, err, "revoking one aggregator's key locked out another's")
		assert.NotEqual(t, targetDID, spared.DID)
	})

	// The WHERE clause carries `AND api_key_hash IS NOT NULL`, so an aggregator
	// that never completed OAuth cannot be "revoked". Reporting that as
	// not-found rather than success is what stops an operator believing they
	// have disabled a bot that was never holding a credential in the first
	// place — the bot they meant is still out there under a DID they mistyped.
	t.Run("reports an aggregator that has no key to revoke", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		keyless := indexAggregator(t, repo, "Keyless")
		assert.ErrorIs(t, repo.RevokeAPIKey(ctx, keyless), aggregators.ErrAggregatorNotFound)
		assert.ErrorIs(t, repo.RevokeAPIKey(ctx, "did:plc:"+testkit.UniqueID(t)), aggregators.ErrAggregatorNotFound)
	})

	// Revoking twice is not an error, because the hash is still there — but the
	// second NOW() overwrites the first. The revocation timestamp is the audit
	// record of when the credential was withdrawn, and a repeated call moves it
	// forward.
	t.Run("a repeated revocation rewrites when the key was withdrawn", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "twice", time.Now().Add(time.Hour))
		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		firstCreds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		require.NotNil(t, firstCreds.APIKeyRevokedAt)
		firstRevokedAt := *firstCreds.APIKeyRevokedAt

		// Backdate the recorded revocation so a second call has something
		// unambiguous to overwrite; NOW() twice in the same second would not.
		_, err = db.ExecContext(ctx,
			`UPDATE aggregators SET api_key_revoked_at = $2 WHERE did = $1`,
			did, firstRevokedAt.Add(-72*time.Hour))
		require.NoError(t, err)

		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		secondCreds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		require.NotNil(t, secondCreds.APIKeyRevokedAt)
		assert.WithinDuration(t, time.Now(), *secondCreds.APIKeyRevokedAt, time.Minute,
			"IF THIS FAILED (issue 2026-07-31-revoke-api-key-rewrites-revocation-timestamp.md) the defect is FIXED — delete this pin. The correct behaviour is to leave "+
				"api_key_revoked_at alone once it is set (WHERE … AND api_key_revoked_at IS NULL), "+
				"because it is the audit record of when a credential was withdrawn and a second call "+
				"currently moves it forward by days")
	})
}

func TestAggregatorRepo_UpdateAPIKeyLastUsed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// last_used_at is what an operator reads to decide whether a key is dead
	// wood. It is written on every successful authentication, so a write that
	// silently did nothing would make every live aggregator look abandoned.
	t.Run("moves the timestamp forward", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "active", time.Now().Add(time.Hour))

		before, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		require.Nil(t, before.APIKeyLastUsed)

		require.NoError(t, repo.UpdateAPIKeyLastUsed(ctx, did))

		first, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		require.NotNil(t, first.APIKeyLastUsed)
		assert.WithinDuration(t, time.Now(), *first.APIKeyLastUsed, time.Minute)

		// Backdate, then authenticate again: comparing two NOW() calls made
		// microseconds apart would prove nothing, whereas a stamp that has to
		// climb out of last week can only have been rewritten.
		_, err = db.ExecContext(ctx,
			`UPDATE aggregators SET api_key_last_used_at = NOW() - INTERVAL '7 days' WHERE did = $1`, did)
		require.NoError(t, err)

		require.NoError(t, repo.UpdateAPIKeyLastUsed(ctx, did))
		second, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		require.NotNil(t, second.APIKeyLastUsed)
		assert.WithinDuration(t, time.Now(), *second.APIKeyLastUsed, time.Minute,
			"a later authentication left a week-old last-used stamp in place")
	})

	t.Run("leaves the credential itself alone", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "audited", time.Now().Add(time.Hour))
		require.NoError(t, repo.UpdateAPIKeyLastUsed(ctx, did))

		authenticated, err := repo.GetByAPIKeyHash(ctx, keyHash)
		require.NoError(t, err, "recording a successful authentication invalidated the key that "+
			"performed it")
		assert.Equal(t, did, authenticated.DID)

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "access-token-audited", creds.OAuthAccessToken)
		assert.Nil(t, creds.APIKeyRevokedAt)
	})

	t.Run("touches one aggregator", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "user", time.Now().Add(time.Hour))
		idle, _ := aggregatorWithAPIKey(t, repo, "idle", time.Now().Add(time.Hour))

		require.NoError(t, repo.UpdateAPIKeyLastUsed(ctx, did))

		untouched, err := repo.GetAggregatorCredentials(ctx, idle)
		require.NoError(t, err)
		assert.Nil(t, untouched.APIKeyLastUsed,
			"one aggregator authenticating marked another as recently active")
	})

	t.Run("reports a DID nothing indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		assert.ErrorIs(t, repo.UpdateAPIKeyLastUsed(ctx, "did:plc:"+testkit.UniqueID(t)),
			aggregators.ErrAggregatorNotFound)
	})
}

func TestAggregatorRepo_UpdateOAuthTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// This runs after every successful refresh against the aggregator's auth
	// server. A write that did not land means the next cycle refreshes with a
	// refresh token the server has already rotated away, and the session is
	// gone for good.
	t.Run("replaces both tokens and the expiry", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "refreshed", time.Now().Add(time.Minute))
		renewedUntil := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Microsecond)

		require.NoError(t, repo.UpdateOAuthTokens(ctx, did, "rotated-access", "rotated-refresh", renewedUntil))

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "rotated-access", creds.OAuthAccessToken)
		assert.Equal(t, "rotated-refresh", creds.OAuthRefreshToken,
			"the rotated refresh token is single-use at most auth servers; failing to store it strands "+
				"the session at the next renewal")
		require.NotNil(t, creds.OAuthTokenExpiresAt)
		assert.True(t, creds.OAuthTokenExpiresAt.Equal(renewedUntil),
			"expiry = %v, want %v", *creds.OAuthTokenExpiresAt, renewedUntil)
		assert.False(t, creds.IsOAuthTokenExpired(), "a freshly refreshed session must not look expired")
	})

	t.Run("re-encrypts rather than storing the new token in the clear", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "reciphered", time.Now().Add(time.Minute))
		require.NoError(t, repo.UpdateOAuthTokens(ctx, did, "post-refresh-access", "post-refresh-refresh",
			time.Now().Add(time.Hour)))

		access, refresh, _ := aggregatorCiphertext(t, db, did)
		assert.False(t, bytes.Contains(access, []byte("post-refresh-access")))
		assert.False(t, bytes.Contains(refresh, []byte("post-refresh-refresh")))
	})

	// The UPDATE lists three columns. Everything the refresh flow needs on the
	// NEXT cycle — the DPoP private key, the nonces, the token endpoint — is
	// outside that list, and would be unrecoverable if a refresh cleared it.
	t.Run("leaves the rest of the session intact", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "partial", time.Now().Add(time.Minute))
		require.NoError(t, repo.UpdateOAuthTokens(ctx, did, "a", "r", time.Now().Add(time.Hour)))

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "zdpop-private-key-partial", creds.OAuthDPoPPrivateKeyMultibase)
		assert.Equal(t, "authserver-nonce-partial", creds.OAuthDPoPAuthServerNonce)
		assert.Equal(t, "pds-nonce-partial", creds.OAuthDPoPPDSNonce)
		assert.Equal(t, "https://pds-partial.example.com", creds.OAuthPDSURL)
		assert.Equal(t, "https://auth-partial.example.com/token", creds.OAuthAuthServerTokenEndpoint)

		authenticated, err := repo.GetByAPIKeyHash(ctx, keyHash)
		require.NoError(t, err, "refreshing the OAuth session invalidated the API key that fronts it")
		assert.Equal(t, did, authenticated.DID)
	})

	t.Run("refreshes one aggregator's session", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "renewer", time.Now().Add(time.Minute))
		other, _ := aggregatorWithAPIKey(t, repo, "sleeper", time.Now().Add(time.Minute))

		require.NoError(t, repo.UpdateOAuthTokens(ctx, did, "only-mine", "only-mine-refresh",
			time.Now().Add(time.Hour)))

		untouched, err := repo.GetAggregatorCredentials(ctx, other)
		require.NoError(t, err)
		assert.Equal(t, "access-token-sleeper", untouched.OAuthAccessToken,
			"one aggregator's token refresh overwrote another's access token")
	})

	t.Run("reports a DID nothing indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		err := repo.UpdateOAuthTokens(ctx, "did:plc:"+testkit.UniqueID(t), "a", "r", time.Now().Add(time.Hour))
		assert.ErrorIs(t, err, aggregators.ErrAggregatorNotFound,
			"a refresh that matched no row must say so: reporting success would make the job count a "+
				"renewal that never happened and never retry it")
	})
}

func TestAggregatorRepo_UpdateOAuthNonces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("records the nonce each server last issued", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "nonced", time.Now().Add(time.Hour))
		require.NoError(t, repo.UpdateOAuthNonces(ctx, did, "fresh-authserver", "fresh-pds"))

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "fresh-authserver", creds.OAuthDPoPAuthServerNonce)
		assert.Equal(t, "fresh-pds", creds.OAuthDPoPPDSNonce)
	})

	// COALESCE(NULLIF($n, ''), <column>) is why this can be called with only the
	// half of the pair that changed. A DPoP request talks to one server at a
	// time, and blanking the other server's nonce would cost a wasted round trip
	// (and a 401) on the next request to it.
	t.Run("an empty nonce keeps the stored one", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "halfnonce", time.Now().Add(time.Hour))

		require.NoError(t, repo.UpdateOAuthNonces(ctx, did, "", "pds-only"))
		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "authserver-nonce-halfnonce", creds.OAuthDPoPAuthServerNonce,
			"an empty argument overwrote the auth server nonce instead of leaving it alone")
		assert.Equal(t, "pds-only", creds.OAuthDPoPPDSNonce)

		require.NoError(t, repo.UpdateOAuthNonces(ctx, did, "authserver-only", ""))
		creds, err = repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "authserver-only", creds.OAuthDPoPAuthServerNonce)
		assert.Equal(t, "pds-only", creds.OAuthDPoPPDSNonce,
			"an empty argument overwrote the PDS nonce instead of leaving it alone")
	})

	t.Run("does not disturb the tokens or the key", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "intact", time.Now().Add(time.Hour))
		require.NoError(t, repo.UpdateOAuthNonces(ctx, did, "n1", "n2"))

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)
		assert.Equal(t, "access-token-intact", creds.OAuthAccessToken)
		assert.Equal(t, "refresh-token-intact", creds.OAuthRefreshToken)

		authenticated, err := repo.GetByAPIKeyHash(ctx, keyHash)
		require.NoError(t, err)
		assert.Equal(t, did, authenticated.DID)
	})

	t.Run("touches one aggregator", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, _ := aggregatorWithAPIKey(t, repo, "talker", time.Now().Add(time.Hour))
		other, _ := aggregatorWithAPIKey(t, repo, "quiet", time.Now().Add(time.Hour))

		require.NoError(t, repo.UpdateOAuthNonces(ctx, did, "mine-authserver", "mine-pds"))

		untouched, err := repo.GetAggregatorCredentials(ctx, other)
		require.NoError(t, err)
		assert.Equal(t, "authserver-nonce-quiet", untouched.OAuthDPoPAuthServerNonce,
			"one aggregator's DPoP nonce landed on another's row, which would make that aggregator "+
				"replay a nonce it was never issued")
		assert.Equal(t, "pds-nonce-quiet", untouched.OAuthDPoPPDSNonce)
	})

	t.Run("reports a DID nothing indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		assert.ErrorIs(t, repo.UpdateOAuthNonces(ctx, "did:plc:"+testkit.UniqueID(t), "a", "p"),
			aggregators.ErrAggregatorNotFound)
	})
}

func TestAggregatorRepo_GetAggregatorCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// "Indexed but never authenticated" and "not indexed at all" are different
	// states, and only the second is an error. The OAuth callback reads this
	// before issuing a key: if an aggregator with no key looked like a missing
	// aggregator, no aggregator could ever get its first key.
	t.Run("an aggregator with no key yields empty credentials, not an error", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did := indexAggregator(t, repo, "Unenrolled")
		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err)

		assert.Equal(t, did, creds.DID)
		assert.Empty(t, creds.APIKeyHash)
		assert.Empty(t, creds.APIKeyPrefix)
		assert.Nil(t, creds.APIKeyCreatedAt)
		assert.Nil(t, creds.APIKeyRevokedAt)
		assert.Nil(t, creds.OAuthTokenExpiresAt)
		assert.Empty(t, creds.OAuthAccessToken, "a NULL ciphertext must not become a decryption failure")
		assert.False(t, creds.HasActiveAPIKey())
		assert.True(t, creds.IsOAuthTokenExpired(),
			"no expiry recorded must read as expired, so the refresh path is the conservative default")
	})

	// Unlike the by-hash lookup, this one deliberately returns revoked
	// credentials: it serves administrative and refresh callers who need to see
	// that a key exists and is dead, not authentication.
	t.Run("returns revoked credentials rather than refusing them", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		did, keyHash := aggregatorWithAPIKey(t, repo, "shownrevoked", time.Now().Add(time.Hour))
		require.NoError(t, repo.RevokeAPIKey(ctx, did))

		creds, err := repo.GetAggregatorCredentials(ctx, did)
		require.NoError(t, err, "the by-DID lookup must still answer for a revoked aggregator; it is "+
			"how an operator sees that the key was withdrawn rather than never issued")
		assert.Equal(t, keyHash, creds.APIKeyHash)
		require.NotNil(t, creds.APIKeyRevokedAt)
		assert.False(t, creds.HasActiveAPIKey())
	})

	t.Run("reports a DID nothing indexed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)

		creds, err := repo.GetAggregatorCredentials(ctx, "did:plc:"+testkit.UniqueID(t))
		require.ErrorIs(t, err, aggregators.ErrAggregatorNotFound)
		assert.Nil(t, creds)
	})
}

// TestAggregatorRepo_ListAggregatorsNeedingTokenRefresh covers the query the
// hourly background job runs, and pins the reason that job currently does far
// more work than it was written to do.
//
// Three of the four WHERE predicates work: an aggregator with no key, with a
// revoked key, or with no recorded expiry is correctly left out. The fourth,
// `oauth_token_expires_at <= NOW() + $1`, does not — see the buffer subtest and
// the KNOWN DEFECT note on the function.
func TestAggregatorRepo_ListAggregatorsNeedingTokenRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// aggregatorRefreshFixture seeds one aggregator of every shape the query has
	// to sort out, and returns the DIDs by role.
	type aggregatorRefreshCohort struct {
		expired  string // token lapsed an hour ago
		soon     string // token lapses in 30 minutes
		distant  string // token lapses in three days
		noExpiry string // active key, no expiry recorded
		noKey    string // indexed, never completed OAuth
		revoked  string // key withdrawn
		repo     aggregators.Repository
	}
	seed := func(t *testing.T) aggregatorRefreshCohort {
		t.Helper()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)
		now := time.Now()

		cohort := aggregatorRefreshCohort{repo: repo}
		cohort.expired, _ = aggregatorWithAPIKey(t, repo, "expired", now.Add(-time.Hour))
		cohort.soon, _ = aggregatorWithAPIKey(t, repo, "soon", now.Add(30*time.Minute))
		cohort.distant, _ = aggregatorWithAPIKey(t, repo, "distant", now.Add(72*time.Hour))
		cohort.revoked, _ = aggregatorWithAPIKey(t, repo, "revoked", now.Add(-time.Hour))
		require.NoError(t, repo.RevokeAPIKey(ctx, cohort.revoked))

		cohort.noExpiry, _ = aggregatorWithAPIKey(t, repo, "noexpiry", now.Add(-time.Hour))
		_, err := db.ExecContext(ctx,
			`UPDATE aggregators SET oauth_token_expires_at = NULL WHERE did = $1`, cohort.noExpiry)
		require.NoError(t, err)

		cohort.noKey = indexAggregator(t, repo, "No Key")
		return cohort
	}

	didsOf := func(creds []*aggregators.AggregatorCredentials) []string {
		dids := make([]string, 0, len(creds))
		for _, c := range creds {
			dids = append(dids, c.DID)
		}
		return dids
	}

	t.Run("excludes aggregators that cannot or must not be refreshed", func(t *testing.T) {
		t.Parallel()
		cohort := seed(t)

		due, err := cohort.repo.ListAggregatorsNeedingTokenRefresh(ctx, time.Hour)
		require.NoError(t, err)
		listed := didsOf(due)

		assert.NotContains(t, listed, cohort.noKey,
			"an aggregator that never completed OAuth has no session to renew; refreshing it would "+
				"fail once per cycle forever")
		assert.NotContains(t, listed, cohort.revoked,
			"a withdrawn credential must not be kept alive by the background job — that is the whole "+
				"point of withdrawing it")
		assert.NotContains(t, listed, cohort.noExpiry,
			"no recorded expiry means no evidence the token is near lapsing")
	})

	// The window at its one working setting. With a zero buffer the query means
	// "already expired", and that is the boundary from the exclusive side: a
	// token good for another half hour must not appear.
	t.Run("a zero buffer selects only tokens that have already lapsed", func(t *testing.T) {
		t.Parallel()
		cohort := seed(t)

		due, err := cohort.repo.ListAggregatorsNeedingTokenRefresh(ctx, 0)
		require.NoError(t, err)
		listed := didsOf(due)

		assert.Contains(t, listed, cohort.expired)
		assert.NotContains(t, listed, cohort.soon,
			"a token with thirty minutes left is not yet expired")
		assert.NotContains(t, listed, cohort.distant)
	})

	// KNOWN DEFECT (issue 2026-07-31-token-refresh-window-nanoseconds-as-seconds.md),
	// pinned. expiryBuffer is a time.Duration, which database/sql
	// hands to the driver as its int64 NANOSECOND count, which Postgres then
	// coerces to an interval measured in SECONDS. The window is therefore a
	// billion times wider than the caller asked for: cmd/server passes
	// tokenRefreshExpiryBuffer = time.Hour, which arrives as 3.6e12 seconds —
	// roughly 114,000 years. Every aggregator with an active key and any
	// recorded expiry is selected, so the hourly job decrypts every stored
	// access token, refresh token and DPoP private key in the table on every
	// cycle before discarding almost all of them in RefreshTokensIfNeeded's own
	// Go-side expiry check.
	t.Run("the buffer is read as seconds, so a one-hour window spans millennia", func(t *testing.T) {
		t.Parallel()
		cohort := seed(t)

		// The value the production job actually passes.
		due, err := cohort.repo.ListAggregatorsNeedingTokenRefresh(ctx, time.Hour)
		require.NoError(t, err)
		listed := didsOf(due)

		assert.Contains(t, listed, cohort.expired)
		assert.Contains(t, listed, cohort.soon)
		assert.Contains(t, listed, cohort.distant,
			"IF THIS FAILED (issue 2026-07-31-token-refresh-window-nanoseconds-as-seconds.md) the defect is FIXED — delete this pin. A token that does not lapse for "+
				"three days is outside a one-hour refresh window and must not be listed. It is listed "+
				"because `NOW() + $1` binds the Duration as a nanosecond COUNT and Postgres reads that "+
				"count as seconds; binding make_interval(secs => $1.Seconds()) would fix it")

		// The scale error stated so it cannot be mistaken for an off-by-one:
		// the window the caller MEANT by time.Hour is the one produced by a
		// Duration of 3600 NANOseconds — one billionth of it.
		asIntended, err := cohort.repo.ListAggregatorsNeedingTokenRefresh(ctx, 3600*time.Nanosecond)
		require.NoError(t, err)
		intended := didsOf(asIntended)
		assert.Contains(t, intended, cohort.expired)
		assert.Contains(t, intended, cohort.soon,
			"IF THIS FAILED (issue 2026-07-31-token-refresh-window-nanoseconds-as-seconds.md) the defect is FIXED — delete this pin. 3600 nanoseconds is not an hour, "+
				"and only the seconds misreading makes it behave like one")
		assert.NotContains(t, intended, cohort.distant)
	})

	t.Run("carries the decrypted session the refresh needs", func(t *testing.T) {
		t.Parallel()
		cohort := seed(t)

		due, err := cohort.repo.ListAggregatorsNeedingTokenRefresh(ctx, 0)
		require.NoError(t, err)

		var expired *aggregators.AggregatorCredentials
		for _, creds := range due {
			if creds.DID == cohort.expired {
				expired = creds
			}
		}
		require.NotNil(t, expired, "the lapsed aggregator is the one this job exists for")

		assert.Equal(t, "refresh-token-expired", expired.OAuthRefreshToken,
			"the job refreshes with this token; a list that returned rows without decrypting them "+
				"would hand every renewal an empty credential")
		assert.Equal(t, "zdpop-private-key-expired", expired.OAuthDPoPPrivateKeyMultibase)
		assert.Equal(t, "https://auth-expired.example.com/token", expired.OAuthAuthServerTokenEndpoint)
		require.NotNil(t, expired.OAuthTokenExpiresAt)
		assert.True(t, expired.IsOAuthTokenExpired())
	})

	t.Run("an installation with nothing due lists nothing", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		repo := NewAggregatorRepository(db)
		indexAggregator(t, repo, "Idle")

		due, err := repo.ListAggregatorsNeedingTokenRefresh(ctx, 0)
		require.NoError(t, err)
		assert.Empty(t, due)
	})
}
