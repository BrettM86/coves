package main

import (
	"testing"
	"time"

	"Coves/internal/config"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production community action service must carry the configured ban
// limits and the ban lister. checkBanLimits allows every ban when both limits
// are zero, and ListCommunityBans answers unavailable without a lister, so a
// config built without either would turn the ban limits off or break the ban
// list in production with no other test noticing.
func TestCommunityActionConfig_WiresBanLimitsAndLister(t *testing.T) {
	a := &application{cfg: &config.Config{Moderation: config.ModerationConfig{
		IdempotencyRetention:      time.Hour,
		CommunityBansPerModerator: 60,
		CommunityBansPerCommunity: 300,
	}}}

	cfg := a.communityActionConfig(nil)

	assert.Equal(t, moderation.CommunityRemovalLimits{
		PerModerator: 60,
		PerCommunity: 300,
		Window:       communityBanLimitWindow,
	}, cfg.BanLimits, "the ban limits must come from the moderation settings")
	require.NotNil(t, cfg.Bans, "the ban lister must be wired for ListCommunityBans")
}
