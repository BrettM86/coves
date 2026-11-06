// coves:allow-host-literal-file: this file tests the DID_WEB_HOSTS parser, and every host:port here is a parser INPUT or an expected parse result, matched as a string and never dialled.
package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed K-256 keys. The private multibase is a throwaway generated for these
// tests; the public multibase beside it is that key's public half.
const (
	labelSigningKeyPrivateMultibase = "z3vLhUAjHvPNd6Z3rb5QQ2Dnd51QfVsvSxnTwYfpx7RdhCf8"
	labelSigningKeyPublicMultibase  = "zQ3shgJ7Pd7JWkPhsfnk4nNADhHZPr9QjXsWZNssxPApcfsxf"
	instanceAtprotoPublicMultibase  = "zQ3shZXbRaEdLTpPrF8kXoRaeU3uaZyUQWdZFBxeSFe656pj2"
	retiringLabelKeyPublicMultibase = "zQ3shpUuQCT3TezuiV8JGT2Gek2yV5SQ7DuqjDZ1tUBULR9vM"
)

func TestLoadLabelPublicationConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name        string
		devEnv      bool
		env         map[string]string
		wantMention string
	}{
		{
			name:        "malformed signing key",
			env:         map[string]string{"LABEL_SIGNING_KEY": "not-a-key"},
			wantMention: "LABEL_SIGNING_KEY",
		},
		{
			name:        "public key given as the signing key",
			env:         map[string]string{"LABEL_SIGNING_KEY": labelSigningKeyPublicMultibase},
			wantMention: "LABEL_SIGNING_KEY",
		},
		{
			name:        "retiring key without a signing key",
			env:         map[string]string{"LABEL_SIGNING_KEY_RETIRING": retiringLabelKeyPublicMultibase},
			wantMention: "LABEL_SIGNING_KEY_RETIRING",
		},
		{
			name: "malformed retiring key",
			env: map[string]string{
				"LABEL_SIGNING_KEY":          labelSigningKeyPrivateMultibase,
				"LABEL_SIGNING_KEY_RETIRING": "not-a-key",
			},
			wantMention: "LABEL_SIGNING_KEY_RETIRING",
		},
		{
			name:        "did:web host redirection outside dev",
			env:         map[string]string{"DID_WEB_HOSTS": "coves.social=127.0.0.1:8081"},
			wantMention: "DID_WEB_HOSTS",
		},
		{
			name:        "did:web host entry without a target",
			devEnv:      true,
			env:         map[string]string{"DID_WEB_HOSTS": "coves.social"},
			wantMention: "DID_WEB_HOSTS",
		},
		{
			name:        "did:web host entry with an empty target",
			devEnv:      true,
			env:         map[string]string{"DID_WEB_HOSTS": "coves.social="},
			wantMention: "DID_WEB_HOSTS",
		},
		{
			name:        "did:web host entry with a leading dot",
			devEnv:      true,
			env:         map[string]string{"DID_WEB_HOSTS": ".coves.social=127.0.0.1:8081"},
			wantMention: "DID_WEB_HOSTS",
		},
		{
			name:        "signing key without an instance atproto key",
			env:         map[string]string{"LABEL_SIGNING_KEY": labelSigningKeyPrivateMultibase},
			wantMention: "INSTANCE_ATPROTO_PUBLIC_KEY",
		},
		{
			name: "instance atproto key equal to the label key",
			env: map[string]string{
				"LABEL_SIGNING_KEY":           labelSigningKeyPrivateMultibase,
				"INSTANCE_ATPROTO_PUBLIC_KEY": labelSigningKeyPublicMultibase,
			},
			wantMention: "INSTANCE_ATPROTO_PUBLIC_KEY",
		},
		{
			name:        "malformed instance atproto key",
			env:         map[string]string{"INSTANCE_ATPROTO_PUBLIC_KEY": "not-a-key"},
			wantMention: "INSTANCE_ATPROTO_PUBLIC_KEY",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			if test.devEnv {
				t.Setenv("IS_DEV_ENV", "true")
			}
			for name, value := range test.env {
				t.Setenv(name, value)
			}

			_, err := Load()
			require.Error(t, err, "Load() accepted the invalid value")
			assert.ErrorContains(t, err, test.wantMention)
		})
	}
}

func TestLoadLabelPublicationConfigParsesValidValues(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("LABEL_SIGNING_KEY", labelSigningKeyPrivateMultibase)
	t.Setenv("LABEL_SIGNING_KEY_RETIRING", retiringLabelKeyPublicMultibase)
	t.Setenv("INSTANCE_ATPROTO_PUBLIC_KEY", instanceAtprotoPublicMultibase)
	t.Setenv("DID_WEB_HOSTS", "coves.social=127.0.0.1:8081")

	cfg, err := Load()
	require.NoError(t, err)

	require.NotNil(t, cfg.Labels.SigningKey, "LABEL_SIGNING_KEY did not reach Labels.SigningKey")
	signingPublicKey, err := cfg.Labels.SigningKey.PublicKey()
	require.NoError(t, err)
	assert.Equal(t, labelSigningKeyPublicMultibase, signingPublicKey.Multibase())
	assert.Equal(t, retiringLabelKeyPublicMultibase, cfg.Labels.RetiringKey)
	assert.Equal(t, instanceAtprotoPublicMultibase, cfg.Instance.AtprotoPublicKey)
	assert.Equal(t, map[string]string{"coves.social": "127.0.0.1:8081"}, cfg.Identity.DIDWebHosts)
}

func TestClearEnvForTestClearsLabelPublicationSettings(t *testing.T) {
	names := []string{"LABEL_SIGNING_KEY", "LABEL_SIGNING_KEY_RETIRING", "INSTANCE_ATPROTO_PUBLIC_KEY", "DID_WEB_HOSTS"}
	for _, name := range names {
		t.Setenv(name, "set")
	}
	ClearEnvForTest(t)
	for _, name := range names {
		assert.Empty(t, os.Getenv(name), "%s must be in loadedEnvVars", name)
	}
}
