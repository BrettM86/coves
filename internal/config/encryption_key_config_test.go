package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadReadsEncryptionKeyFromEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("ENCRYPTION_KEY", " \t"+validEncryptionKey+"\n ")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, validEncryptionKey, cfg.EncryptionKey)
	assert.False(t, cfg.EncryptionKeyGenerated)
}

func TestLoadGeneratesUniqueEncryptionKeysInDev(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")

	first, err := Load()
	require.NoError(t, err)
	second, err := Load()
	require.NoError(t, err)

	for name, cfg := range map[string]*Config{"first": first, "second": second} {
		t.Run(name, func(t *testing.T) {
			decoded, decodeErr := base64.StdEncoding.DecodeString(cfg.EncryptionKey)
			require.NoError(t, decodeErr, "generated ENCRYPTION_KEY must use standard base64")
			assert.Len(t, decoded, 32)
			assert.True(t, cfg.EncryptionKeyGenerated)
		})
	}
	assert.NotEqual(t, first.EncryptionKey, second.EncryptionKey,
		"two process starts must not receive the same randomly generated dev key")
}

func TestLoadRejectsMalformedEncryptionKeyInDev(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "non-base64", value: "not valid base64!"},
		{name: "sixteen bytes", value: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x16}, 16))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("ENCRYPTION_KEY", test.value)

			_, err := Load()
			require.Error(t, err, "a set but invalid key must not be replaced by a generated dev key")
			assert.Contains(t, err.Error(), "ENCRYPTION_KEY")
		})
	}
}

func TestLoadValidatesEncryptionKeyInProduction(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantText string
	}{
		{
			name:     "placeholder",
			value:    "CHANGE_ME_BASE64_ENCODED_KEY",
			wantText: "openssl rand -base64 32",
		},
		{
			name:     "non-base64",
			value:    "not valid base64!",
			wantText: "base64",
		},
		{
			name:     "wrong decoded length",
			value:    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x16}, 16)),
			wantText: "32 bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv("ENCRYPTION_KEY", test.value)

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ENCRYPTION_KEY")
			assert.Contains(t, err.Error(), test.wantText)
		})
	}
}

func TestLoadAcceptsValidEncryptionKeyInProduction(t *testing.T) {
	clearEnv(t)
	prodEnv(t)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, validEncryptionKey, cfg.EncryptionKey)
	assert.False(t, cfg.EncryptionKeyGenerated)
}

func TestClearEnvForTestClearsEncryptionKey(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", validEncryptionKey)

	ClearEnvForTest(t)

	assert.Empty(t, os.Getenv("ENCRYPTION_KEY"))
}
