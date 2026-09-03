package postgres

import "Coves/internal/crypto/credentialcipher"

// Credential contexts bind each ciphertext to its table, column, and row DID.
// They are persisted as GCM additional authenticated data, so renaming a table
// or column or changing this format requires a re-encryption pass.
func communityPDSPasswordCredentialContext(did string) string {
	return "communities.pds_password_encrypted:" + did
}

func communityPDSAccessTokenCredentialContext(did string) string {
	return "communities.pds_access_token_encrypted:" + did
}

func communityPDSRefreshTokenCredentialContext(did string) string {
	return "communities.pds_refresh_token_encrypted:" + did
}

func aggregatorOAuthAccessTokenCredentialContext(did string) string {
	return "aggregators.oauth_access_token_encrypted:" + did
}

func aggregatorOAuthRefreshTokenCredentialContext(did string) string {
	return "aggregators.oauth_refresh_token_encrypted:" + did
}

func aggregatorOAuthDPoPPrivateKeyCredentialContext(did string) string {
	return "aggregators.oauth_dpop_private_key_encrypted:" + did
}

// encryptOptionalCredential returns an untyped nil for an empty credential so
// the driver binds SQL NULL. The return type is any on purpose: lib/pq encodes
// a nil []byte as an empty bytea, not NULL, and absent credentials must be NULL.
func encryptOptionalCredential(cipher *credentialcipher.Cipher, plaintext, context string) (any, error) {
	if plaintext == "" {
		return nil, nil
	}
	return cipher.Encrypt(plaintext, context)
}

// decryptOptionalCredential treats SQL NULL and zero-length bytea values as an
// absent optional credential.
func decryptOptionalCredential(cipher *credentialcipher.Cipher, ciphertext []byte, context string) (string, error) {
	if len(ciphertext) == 0 {
		return "", nil
	}
	return cipher.Decrypt(ciphertext, context)
}
