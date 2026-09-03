package credentialcipher_test

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"Coves/internal/crypto/credentialcipher"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testKeyString  = "0123456789abcdef0123456789abcdef"
	wrongKeyString = "fedcba9876543210fedcba9876543210"
	wireVersion    = byte(0x01)
	nonceSize      = 12
	tagSize        = 16
)

func TestNewValidatesKeyLength(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)
	assert.NotNil(t, cipher)

	for _, length := range []int{0, 16, 31, 33, 64} {
		t.Run(stringLengthName(length), func(t *testing.T) {
			key := bytes.Repeat([]byte{'k'}, length)
			invalidCipher, err := credentialcipher.New(key)

			require.ErrorIs(t, err, credentialcipher.ErrInvalidKey)
			assert.Nil(t, invalidCipher)
			if len(key) > 0 {
				assert.NotContains(t, err.Error(), string(key), "error disclosed the rejected key")
			}
		})
	}
}

func TestNewFromBase64ValidatesEncodingAndKeyLength(t *testing.T) {
	encodedKey := base64.StdEncoding.EncodeToString([]byte(testKeyString))
	cipher, err := credentialcipher.NewFromBase64(encodedKey)
	require.NoError(t, err)
	assert.NotNil(t, cipher)

	tests := []struct {
		name    string
		encoded string
		key     string
	}{
		{
			name:    "non-base64 text",
			encoded: "this is not standard base64!",
		},
		{
			name:    "sixteen decoded bytes",
			encoded: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
			key:     "0123456789abcdef",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalidCipher, err := credentialcipher.NewFromBase64(test.encoded)

			require.ErrorIs(t, err, credentialcipher.ErrInvalidKey)
			assert.Nil(t, invalidCipher)
			if test.key != "" {
				assert.NotContains(t, err.Error(), test.key, "error disclosed the decoded key")
			}
		})
	}
}

func TestEncryptUsesVersionedAESGCMFramingAndRandomNonces(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)

	const plaintext = "same credential"
	const context = "communities.pds_password_encrypted:did:plc:framing"

	first, err := cipher.Encrypt(plaintext, context)
	require.NoError(t, err)
	second, err := cipher.Encrypt(plaintext, context)
	require.NoError(t, err)

	expectedLength := 1 + nonceSize + len(plaintext) + tagSize
	if assert.Len(t, first, expectedLength) {
		assert.Equal(t, wireVersion, first[0])
	}
	if assert.Len(t, second, expectedLength) {
		assert.Equal(t, wireVersion, second[0])
	}
	assert.NotEqual(t, first, second, "reusing a nonce makes repeated credential encryption deterministic")
}

func TestCipherRoundTripsCredentialPayloads(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)

	tests := []struct {
		name      string
		plaintext string
	}{
		{name: "empty", plaintext: ""},
		{name: "JWT punctuation", plaintext: "eyJhbGciOiJIUzI1NiJ9.payload+with/slash.signature=="},
		{name: "multibase key", plaintext: "zQ3shK7ExampleMultibasePrivateKey"},
		{name: "unicode", plaintext: "\u5bc6\u78bc-\u30c8\u30fc\u30af\u30f3-\U0001f510"},
		{name: "four KiB", plaintext: strings.Repeat("x", 4*1024)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context := "aggregators.oauth_access_token_encrypted:did:plc:" + strings.ReplaceAll(test.name, " ", "-")
			sealed, err := cipher.Encrypt(test.plaintext, context)
			require.NoError(t, err)
			assert.Len(t, sealed, 1+nonceSize+len(test.plaintext)+tagSize)

			opened, err := cipher.Decrypt(sealed, context)
			require.NoError(t, err)
			assert.Equal(t, test.plaintext, opened)
		})
	}
}

func TestDecryptRejectsWrongKeyAndContext(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)
	wrongCipher, err := credentialcipher.New([]byte(wrongKeyString))
	require.NoError(t, err)

	const plaintext = "credential-plaintext-do-not-leak"
	const context = "aggregators.oauth_refresh_token_encrypted:did:plc:binding"
	sealed, err := cipher.Encrypt(plaintext, context)
	require.NoError(t, err)

	t.Run("wrong key", func(t *testing.T) {
		_, err := wrongCipher.Decrypt(sealed, context)
		requireInvalidCiphertextWithoutSecrets(t, err, plaintext, testKeyString, wrongKeyString)
	})

	t.Run("wrong context", func(t *testing.T) {
		_, err := cipher.Decrypt(sealed, context+"-different")
		requireInvalidCiphertextWithoutSecrets(t, err, plaintext, testKeyString)
	})
}

func TestDecryptRejectsTamperingInEveryAuthenticatedRegion(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)

	const plaintext = "credential-plaintext-do-not-leak"
	const context = "communities.pds_access_token_encrypted:did:plc:tamper"
	sealed, err := cipher.Encrypt(plaintext, context)
	require.NoError(t, err)
	require.Greater(t, len(sealed), 1+nonceSize+tagSize)

	tests := []struct {
		name  string
		index int
	}{
		{name: "nonce", index: 1},
		{name: "body", index: 13},
		{name: "tag", index: len(sealed) - 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := append([]byte(nil), sealed...)
			tampered[test.index] ^= 0x01

			_, err := cipher.Decrypt(tampered, context)
			requireInvalidCiphertextWithoutSecrets(t, err, plaintext, testKeyString)
		})
	}
}

func TestDecryptRejectsMalformedCiphertext(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)

	tests := []struct {
		name       string
		ciphertext []byte
	}{
		{name: "nil", ciphertext: nil},
		{name: "empty", ciphertext: []byte{}},
		{name: "truncated", ciphertext: append([]byte{wireVersion}, make([]byte, nonceSize+tagSize-1)...)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := cipher.Decrypt(test.ciphertext, "communities.pds_refresh_token_encrypted:did:plc:malformed")
			requireInvalidCiphertextWithoutSecrets(t, err, testKeyString)
		})
	}
}

func TestDecryptRejectsUnsupportedVersion(t *testing.T) {
	cipher, err := credentialcipher.New([]byte(testKeyString))
	require.NoError(t, err)

	const credentialContext = "aggregators.oauth_dpop_private_key_encrypted:did:plc:version"
	ciphertext, err := cipher.Encrypt("versioned credential", credentialContext)
	require.NoError(t, err)

	tests := []struct {
		name        string
		version     byte
		messagePart string
	}{
		{name: "pgcrypto legacy ciphertext", version: 0xc3, messagePart: "pgcrypto"},
		{name: "unknown application version", version: 0x02, messagePart: "version 2"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsupported := append([]byte(nil), ciphertext...)
			unsupported[0] = test.version

			_, err := cipher.Decrypt(unsupported, credentialContext)
			assert.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext)
			assert.ErrorIs(t, err, credentialcipher.ErrUnsupportedVersion)
			assert.Contains(t, strings.ToLower(err.Error()), test.messagePart)
			assert.NotContains(t, err.Error(), testKeyString, "error disclosed the encryption key")
		})
	}
}

func requireInvalidCiphertextWithoutSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	require.ErrorIs(t, err, credentialcipher.ErrInvalidCiphertext)
	for _, secret := range secrets {
		assert.NotContains(t, err.Error(), secret, "error disclosed credential material")
	}
}

func stringLengthName(length int) string {
	return "length " + strconv.Itoa(length)
}
