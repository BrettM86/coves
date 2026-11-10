package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"Coves/internal/core/communities"

	"github.com/lib/pq"
)

// pqUniqueViolation is the SQLSTATE Postgres reports for a unique-constraint
// violation.
const pqUniqueViolation = "23505"

// SaveProvisioningAccount stores a community PDS account's generated
// credentials before createAccount is called. A second account for one handle
// is refused with ErrProvisioningAccountExists.
func (r *postgresCommunityRepo) SaveProvisioningAccount(ctx context.Context, account *communities.ProvisioningAccount) error {
	passwordCiphertext, err := r.cipher.Encrypt(account.PDSPassword, communityProvisioningPasswordCredentialContext(account.Handle))
	if err != nil {
		return fmt.Errorf("failed to encrypt community provisioning password: %w", err)
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO community_provisioning_accounts (
			handle, name, created_by_did, hosted_by_did, pds_url, pds_email, pds_password_encrypted
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		account.Handle, account.Name, account.CreatedByDID, account.HostedByDID,
		account.PDSURL, account.PDSEmail, passwordCiphertext)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == pqUniqueViolation {
			return communities.ErrProvisioningAccountExists
		}
		return fmt.Errorf("failed to save community provisioning account: %w", err)
	}
	return nil
}

// GetProvisioningAccountByHandle returns the provisioning account holding the
// handle, or ErrProvisioningAccountNotFound.
func (r *postgresCommunityRepo) GetProvisioningAccountByHandle(ctx context.Context, handle string) (*communities.ProvisioningAccount, error) {
	account := &communities.ProvisioningAccount{}
	var passwordCiphertext []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT handle, name, created_by_did, hosted_by_did, pds_url, pds_email, pds_password_encrypted, created_at
		FROM community_provisioning_accounts
		WHERE handle = $1`, handle,
	).Scan(&account.Handle, &account.Name, &account.CreatedByDID, &account.HostedByDID,
		&account.PDSURL, &account.PDSEmail, &passwordCiphertext, &account.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, communities.ErrProvisioningAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get community provisioning account: %w", err)
	}

	account.PDSPassword, err = r.cipher.Decrypt(passwordCiphertext, communityProvisioningPasswordCredentialContext(account.Handle))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt community provisioning password: %w", err)
	}
	return account, nil
}

// DeleteProvisioningAccount removes the provisioning account holding the
// handle. Deleting one that does not exist is not an error.
func (r *postgresCommunityRepo) DeleteProvisioningAccount(ctx context.Context, handle string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM community_provisioning_accounts WHERE handle = $1`, handle); err != nil {
		return fmt.Errorf("failed to delete community provisioning account: %w", err)
	}
	return nil
}

// AttachHostedCredentials stores credentials on a community row that has none
// and is hosted by community.HostedByDID: the row the firehose consumer indexes
// when it sees the profile record before CreateCommunity stores the account.
// It never overwrites existing credentials or touches another host's row
// (ErrCommunityCredentialsConflict); with no row at all it returns
// ErrCommunityNotFound.
func (r *postgresCommunityRepo) AttachHostedCredentials(ctx context.Context, community *communities.Community) error {
	passwordCiphertext, err := encryptOptionalCredential(
		r.cipher, community.PDSPassword, communityPDSPasswordCredentialContext(community.DID))
	if err != nil {
		return fmt.Errorf("failed to encrypt community PDS password: %w", err)
	}
	accessTokenCiphertext, err := encryptOptionalCredential(
		r.cipher, community.PDSAccessToken, communityPDSAccessTokenCredentialContext(community.DID))
	if err != nil {
		return fmt.Errorf("failed to encrypt community PDS access token: %w", err)
	}
	refreshTokenCiphertext, err := encryptOptionalCredential(
		r.cipher, community.PDSRefreshToken, communityPDSRefreshTokenCredentialContext(community.DID))
	if err != nil {
		return fmt.Errorf("failed to encrypt community PDS refresh token: %w", err)
	}

	result, err := r.db.ExecContext(ctx, `
		UPDATE communities
		SET
			pds_email = $3,
			pds_password_encrypted = $4,
			pds_access_token_encrypted = $5,
			pds_refresh_token_encrypted = $6,
			pds_url = $7,
			updated_at = NOW()
		WHERE did = $1
		  AND hosted_by_did = $2
		  AND pds_refresh_token_encrypted IS NULL`,
		community.DID, community.HostedByDID, nullString(community.PDSEmail),
		passwordCiphertext, accessTokenCiphertext, refreshTokenCiphertext, nullString(community.PDSURL))
	if err != nil {
		return fmt.Errorf("failed to attach community credentials: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to read attached community credentials row count: %w", err)
	}
	if updated == 1 {
		return nil
	}

	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM communities WHERE did = $1)`, community.DID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("failed to check community row after credential attach: %w", err)
	}
	if !exists {
		return communities.ErrCommunityNotFound
	}
	return communities.ErrCommunityCredentialsConflict
}
