package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadModerationAdmins(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "trims and drops empty entries", value: " did:plc:a , did:plc:b ,, ", want: []string{"did:plc:a", "did:plc:b"}},
		{name: "empty list grants nobody", value: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv("MODERATION_ADMINS", test.value)

			cfg, err := Load()
			require.NoError(t, err)
			if test.want == nil {
				assert.Empty(t, cfg.Moderation.Admins)
			} else {
				assert.Equal(t, test.want, cfg.Moderation.Admins)
			}
		})
	}
}

func TestLoadModerationAdminsRejectsNonDID(t *testing.T) {
	clearEnv(t)
	prodEnv(t)
	t.Setenv("MODERATION_ADMINS", "did:plc:a,alice.test")

	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "MODERATION_ADMINS")
}

func TestClearEnvForTestClearsModerationAdmins(t *testing.T) {
	t.Setenv("MODERATION_ADMINS", "did:plc:a")

	ClearEnvForTest(t)

	assert.Empty(t, os.Getenv("MODERATION_ADMINS"))
}
