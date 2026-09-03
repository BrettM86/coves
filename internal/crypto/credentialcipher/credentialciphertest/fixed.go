// Package credentialciphertest provides a deterministic Cipher for tests that
// need to construct repositories but do not care about key management.
package credentialciphertest

import "Coves/internal/crypto/credentialcipher"

// Fixed returns a Cipher built from a constant, publicly known key. Never use
// it outside tests.
func Fixed() *credentialcipher.Cipher {
	cipher, err := credentialcipher.New([]byte("coves-test-credential-key-000000"))
	if err != nil {
		panic("credentialciphertest: fixed key rejected: " + err.Error())
	}
	return cipher
}
