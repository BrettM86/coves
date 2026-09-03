package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"Coves/internal/crypto/credentialcipher"
)

// ReencryptReport counts what a ReencryptLegacyCredentials pass rewrote.
type ReencryptReport struct {
	// CommunitiesRewritten is the number of community rows whose pgcrypto
	// credentials were resealed with the application cipher.
	CommunitiesRewritten int
	// AggregatorsRewritten is the same count for aggregator rows.
	AggregatorsRewritten int
}

// ReencryptLegacyCredentials moves credentials off the database-held pgcrypto
// key before migration 046 removes that key. One transaction makes the pass
// atomic with migration 046's legacy-data guard and prevents a failure from
// leaving a half-converted database. Each table is handled only after all three
// of its credential columns exist. A database older than migration 025 may
// therefore need a second boot: migration 046 refuses the first boot, and after
// the operator restarts, this pass converts credentials sealed by migration 025.
// A generated dev key is refused because credentials resealed with it would be
// stranded at restart.
func ReencryptLegacyCredentials(
	ctx context.Context,
	db *sql.DB,
	cipher *credentialcipher.Cipher,
	keyIsEphemeral bool,
) (report ReencryptReport, returnErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ReencryptReport{}, fmt.Errorf("begin credential re-encryption transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("roll back credential re-encryption transaction: %w", rollbackErr))
		}
	}()

	communitiesAvailable, err := credentialColumnsAvailable(ctx, tx, "communities",
		"pds_password_encrypted", "pds_access_token_encrypted", "pds_refresh_token_encrypted")
	if err != nil {
		return ReencryptReport{}, err
	}
	aggregatorsAvailable, err := credentialColumnsAvailable(ctx, tx, "aggregators",
		"oauth_access_token_encrypted", "oauth_refresh_token_encrypted", "oauth_dpop_private_key_encrypted")
	if err != nil {
		return ReencryptReport{}, err
	}

	var encryptionKeysTable sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('encryption_keys')`).Scan(&encryptionKeysTable); err != nil {
		return ReencryptReport{}, fmt.Errorf("check for legacy encryption key table: %w", err)
	}
	if !encryptionKeysTable.Valid {
		if err := rejectUnrecoverableLegacyCredentials(ctx, tx, communitiesAvailable, aggregatorsAvailable); err != nil {
			return ReencryptReport{}, err
		}
		if err := tx.Commit(); err != nil {
			return ReencryptReport{}, fmt.Errorf("commit credential re-encryption transaction: %w", err)
		}
		return ReencryptReport{}, nil
	}

	var communities []legacyCommunityCredentials
	if communitiesAvailable {
		communities, err = loadLegacyCommunityCredentials(ctx, tx)
		if err != nil {
			return ReencryptReport{}, err
		}
	}
	var aggregators []legacyAggregatorCredentials
	if aggregatorsAvailable {
		aggregators, err = loadLegacyAggregatorCredentials(ctx, tx)
		if err != nil {
			return ReencryptReport{}, err
		}
	}

	legacyRowCount := len(communities) + len(aggregators)
	if keyIsEphemeral && legacyRowCount > 0 {
		return ReencryptReport{}, fmt.Errorf(
			"cannot re-encrypt %d legacy credential rows with a generated ENCRYPTION_KEY; configure a persistent ENCRYPTION_KEY first",
			legacyRowCount)
	}
	if legacyRowCount == 0 {
		if err := tx.Commit(); err != nil {
			return ReencryptReport{}, fmt.Errorf("commit credential re-encryption transaction: %w", err)
		}
		return ReencryptReport{}, nil
	}

	for _, community := range communities {
		password, passwordLegacy, err := reencryptLegacyCredential(
			ctx, tx, cipher, legacyCredential{
				table: "communities", column: "pds_password_encrypted", did: community.did,
				context: communityPDSPasswordCredentialContext(community.did), ciphertext: community.password,
			})
		if err != nil {
			return ReencryptReport{}, err
		}
		accessToken, accessTokenLegacy, err := reencryptLegacyCredential(
			ctx, tx, cipher, legacyCredential{
				table: "communities", column: "pds_access_token_encrypted", did: community.did,
				context: communityPDSAccessTokenCredentialContext(community.did), ciphertext: community.accessToken,
			})
		if err != nil {
			return ReencryptReport{}, err
		}
		refreshToken, refreshTokenLegacy, err := reencryptLegacyCredential(
			ctx, tx, cipher, legacyCredential{
				table: "communities", column: "pds_refresh_token_encrypted", did: community.did,
				context: communityPDSRefreshTokenCredentialContext(community.did), ciphertext: community.refreshToken,
			})
		if err != nil {
			return ReencryptReport{}, err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE communities SET
				pds_password_encrypted = CASE WHEN $2 THEN $3 ELSE pds_password_encrypted END,
				pds_access_token_encrypted = CASE WHEN $4 THEN $5 ELSE pds_access_token_encrypted END,
				pds_refresh_token_encrypted = CASE WHEN $6 THEN $7 ELSE pds_refresh_token_encrypted END
			WHERE did = $1`,
			community.did,
			passwordLegacy, password,
			accessTokenLegacy, accessToken,
			refreshTokenLegacy, refreshToken,
		); err != nil {
			return ReencryptReport{}, fmt.Errorf("update communities credentials for DID %s: %w", community.did, err)
		}
		report.CommunitiesRewritten++
	}

	for _, aggregator := range aggregators {
		accessToken, accessTokenLegacy, err := reencryptLegacyCredential(
			ctx, tx, cipher, legacyCredential{
				table: "aggregators", column: "oauth_access_token_encrypted", did: aggregator.did,
				context: aggregatorOAuthAccessTokenCredentialContext(aggregator.did), ciphertext: aggregator.accessToken,
			})
		if err != nil {
			return ReencryptReport{}, err
		}
		refreshToken, refreshTokenLegacy, err := reencryptLegacyCredential(
			ctx, tx, cipher, legacyCredential{
				table: "aggregators", column: "oauth_refresh_token_encrypted", did: aggregator.did,
				context: aggregatorOAuthRefreshTokenCredentialContext(aggregator.did), ciphertext: aggregator.refreshToken,
			})
		if err != nil {
			return ReencryptReport{}, err
		}
		dpopPrivateKey, dpopPrivateKeyLegacy, err := reencryptLegacyCredential(
			ctx, tx, cipher, legacyCredential{
				table: "aggregators", column: "oauth_dpop_private_key_encrypted", did: aggregator.did,
				context: aggregatorOAuthDPoPPrivateKeyCredentialContext(aggregator.did), ciphertext: aggregator.dpopPrivateKey,
			})
		if err != nil {
			return ReencryptReport{}, err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE aggregators SET
				oauth_access_token_encrypted = CASE WHEN $2 THEN $3 ELSE oauth_access_token_encrypted END,
				oauth_refresh_token_encrypted = CASE WHEN $4 THEN $5 ELSE oauth_refresh_token_encrypted END,
				oauth_dpop_private_key_encrypted = CASE WHEN $6 THEN $7 ELSE oauth_dpop_private_key_encrypted END
			WHERE did = $1`,
			aggregator.did,
			accessTokenLegacy, accessToken,
			refreshTokenLegacy, refreshToken,
			dpopPrivateKeyLegacy, dpopPrivateKey,
		); err != nil {
			return ReencryptReport{}, fmt.Errorf("update aggregators credentials for DID %s: %w", aggregator.did, err)
		}
		report.AggregatorsRewritten++
	}

	if err := tx.Commit(); err != nil {
		return ReencryptReport{}, fmt.Errorf("commit credential re-encryption transaction: %w", err)
	}
	return report, nil
}

func credentialColumnsAvailable(
	ctx context.Context,
	tx *sql.Tx,
	table, firstColumn, secondColumn, thirdColumn string,
) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
			AND table_name = $1
			AND column_name IN ($2, $3, $4)`,
		table, firstColumn, secondColumn, thirdColumn).Scan(&count); err != nil {
		return false, fmt.Errorf("check %s credential columns: %w", table, err)
	}
	return count == 3, nil
}

func rejectUnrecoverableLegacyCredentials(
	ctx context.Context,
	tx *sql.Tx,
	communitiesAvailable, aggregatorsAvailable bool,
) error {
	var affected []string
	if communitiesAvailable {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM communities
			WHERE (octet_length(pds_password_encrypted) > 0 AND get_byte(pds_password_encrypted, 0) <> $1)
			   OR (octet_length(pds_access_token_encrypted) > 0 AND get_byte(pds_access_token_encrypted, 0) <> $1)
			   OR (octet_length(pds_refresh_token_encrypted) > 0 AND get_byte(pds_refresh_token_encrypted, 0) <> $1)`,
			credentialcipher.Version).Scan(&count); err != nil {
			return fmt.Errorf("count unrecoverable communities credentials: %w", err)
		}
		if count > 0 {
			affected = append(affected, fmt.Sprintf("communities: %d", count))
		}
	}
	if aggregatorsAvailable {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM aggregators
			WHERE (octet_length(oauth_access_token_encrypted) > 0 AND get_byte(oauth_access_token_encrypted, 0) <> $1)
			   OR (octet_length(oauth_refresh_token_encrypted) > 0 AND get_byte(oauth_refresh_token_encrypted, 0) <> $1)
			   OR (octet_length(oauth_dpop_private_key_encrypted) > 0 AND get_byte(oauth_dpop_private_key_encrypted, 0) <> $1)`,
			credentialcipher.Version).Scan(&count); err != nil {
			return fmt.Errorf("count unrecoverable aggregators credentials: %w", err)
		}
		if count > 0 {
			affected = append(affected, fmt.Sprintf("aggregators: %d", count))
		}
	}
	if len(affected) > 0 {
		return fmt.Errorf("legacy credential rows (%s) cannot be recovered because encryption_keys was already dropped; restore a pre-046 backup or NULL the columns and re-provision credentials", strings.Join(affected, ", "))
	}
	return nil
}

type legacyCommunityCredentials struct {
	did          string
	password     []byte
	accessToken  []byte
	refreshToken []byte
}

func loadLegacyCommunityCredentials(ctx context.Context, tx *sql.Tx) ([]legacyCommunityCredentials, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT did, pds_password_encrypted, pds_access_token_encrypted, pds_refresh_token_encrypted
		FROM communities
		WHERE (pds_password_encrypted IS NOT NULL AND
			(octet_length(pds_password_encrypted) = 0 OR get_byte(pds_password_encrypted, 0) <> $1))
		   OR (pds_access_token_encrypted IS NOT NULL AND
			(octet_length(pds_access_token_encrypted) = 0 OR get_byte(pds_access_token_encrypted, 0) <> $1))
		   OR (pds_refresh_token_encrypted IS NOT NULL AND
			(octet_length(pds_refresh_token_encrypted) = 0 OR get_byte(pds_refresh_token_encrypted, 0) <> $1))
		ORDER BY did
		FOR UPDATE`, credentialcipher.Version)
	if err != nil {
		return nil, fmt.Errorf("lock communities with legacy credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var credentials []legacyCommunityCredentials
	for rows.Next() {
		var credential legacyCommunityCredentials
		if err := rows.Scan(
			&credential.did, &credential.password, &credential.accessToken, &credential.refreshToken,
		); err != nil {
			return nil, fmt.Errorf("scan communities legacy credentials: %w", err)
		}
		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate communities legacy credentials: %w", err)
	}
	return credentials, nil
}

type legacyAggregatorCredentials struct {
	did            string
	accessToken    []byte
	refreshToken   []byte
	dpopPrivateKey []byte
}

func loadLegacyAggregatorCredentials(ctx context.Context, tx *sql.Tx) ([]legacyAggregatorCredentials, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT did, oauth_access_token_encrypted, oauth_refresh_token_encrypted,
			oauth_dpop_private_key_encrypted
		FROM aggregators
		WHERE (oauth_access_token_encrypted IS NOT NULL AND
			(octet_length(oauth_access_token_encrypted) = 0 OR get_byte(oauth_access_token_encrypted, 0) <> $1))
		   OR (oauth_refresh_token_encrypted IS NOT NULL AND
			(octet_length(oauth_refresh_token_encrypted) = 0 OR get_byte(oauth_refresh_token_encrypted, 0) <> $1))
		   OR (oauth_dpop_private_key_encrypted IS NOT NULL AND
			(octet_length(oauth_dpop_private_key_encrypted) = 0 OR get_byte(oauth_dpop_private_key_encrypted, 0) <> $1))
		ORDER BY did
		FOR UPDATE`, credentialcipher.Version)
	if err != nil {
		return nil, fmt.Errorf("lock aggregators with legacy credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var credentials []legacyAggregatorCredentials
	for rows.Next() {
		var credential legacyAggregatorCredentials
		if err := rows.Scan(
			&credential.did, &credential.accessToken, &credential.refreshToken, &credential.dpopPrivateKey,
		); err != nil {
			return nil, fmt.Errorf("scan aggregators legacy credentials: %w", err)
		}
		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aggregators legacy credentials: %w", err)
	}
	return credentials, nil
}

type legacyCredential struct {
	table      string
	column     string
	did        string
	context    string
	ciphertext []byte
}

// reencryptLegacyCredential returns the replacement value as any so that an
// untyped nil binds as SQL NULL; lib/pq would send a nil []byte as empty bytea.
func reencryptLegacyCredential(
	ctx context.Context,
	tx *sql.Tx,
	cipher *credentialcipher.Cipher,
	credential legacyCredential,
) (any, bool, error) {
	if credential.ciphertext == nil || (len(credential.ciphertext) > 0 && credential.ciphertext[0] == credentialcipher.Version) {
		return nil, false, nil
	}
	if len(credential.ciphertext) == 0 {
		return nil, true, nil
	}

	var plaintext string
	if err := tx.QueryRowContext(ctx,
		`SELECT pgp_sym_decrypt($1::bytea, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))`,
		credential.ciphertext).Scan(&plaintext); err != nil {
		return nil, false, fmt.Errorf("decrypt %s.%s for DID %s: %w", credential.table, credential.column, credential.did, err)
	}

	sealed, err := cipher.Encrypt(plaintext, credential.context)
	if err != nil {
		return nil, false, fmt.Errorf("encrypt %s.%s for DID %s: %w", credential.table, credential.column, credential.did, err)
	}
	return sealed, true, nil
}
