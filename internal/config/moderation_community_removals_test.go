package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCommunityRemovalLimits(t *testing.T) {
	for _, test := range []struct {
		name          string
		perModerator  string
		perCommunity  string
		wantModerator int
		wantCommunity int
	}{
		{name: "defaults", wantModerator: 60, wantCommunity: 300},
		{name: "configured limits", perModerator: "5", perCommunity: "7", wantModerator: 5, wantCommunity: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv("COMMUNITY_REMOVALS_PER_MODERATOR_PER_HOUR", test.perModerator)
			t.Setenv("COMMUNITY_REMOVALS_PER_COMMUNITY_PER_HOUR", test.perCommunity)
			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, test.wantModerator, cfg.Moderation.CommunityRemovalsPerModerator)
			assert.Equal(t, test.wantCommunity, cfg.Moderation.CommunityRemovalsPerCommunity)
		})
	}
}

func TestLoadCommunityRemovalLimitsRejectsInvalidSettings(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{"COMMUNITY_REMOVALS_PER_MODERATOR_PER_HOUR", "0"},
		{"COMMUNITY_REMOVALS_PER_MODERATOR_PER_HOUR", "abc"},
		{"COMMUNITY_REMOVALS_PER_COMMUNITY_PER_HOUR", "0"},
		{"COMMUNITY_REMOVALS_PER_COMMUNITY_PER_HOUR", "abc"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			clearEnv(t)
			prodEnv(t)
			t.Setenv(test.name, test.value)
			cfg, err := Load()
			if err == nil {
				err = cfg.Validate()
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, test.name)
		})
	}
}

func TestClearEnvForTestClearsCommunityRemovalLimits(t *testing.T) {
	names := []string{"COMMUNITY_REMOVALS_PER_MODERATOR_PER_HOUR", "COMMUNITY_REMOVALS_PER_COMMUNITY_PER_HOUR"}
	for _, name := range names {
		t.Setenv(name, "9")
	}
	ClearEnvForTest(t)
	for _, name := range names {
		assert.Empty(t, os.Getenv(name), "%s must be in loadedEnvVars", name)
	}
	t.Setenv("IS_DEV_ENV", "true")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 60, cfg.Moderation.CommunityRemovalsPerModerator)
	assert.Equal(t, 300, cfg.Moderation.CommunityRemovalsPerCommunity)
}
