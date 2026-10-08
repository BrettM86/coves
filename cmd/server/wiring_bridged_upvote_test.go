package main

import (
	"testing"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"

	"github.com/stretchr/testify/require"
)

func TestApplication_BridgedUpvoteTotalsFollowTrustedBridgeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts []string
		want  bool
	}{
		{"no trusted hosts", nil, false},
		{"trusted host", []string{"https://bridge.test"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &application{
				cfg:              &config.Config{Instance: config.InstanceConfig{TrustedBridgePDSHosts: tc.hosts}},
				credentialCipher: credentialciphertest.Fixed(),
			}
			app.buildRepositories()
			repo, ok := app.notificationRepo.(interface{ CountsBridgedUpvoteTotals() bool })
			require.True(t, ok, "notification repository must expose its bridge-total gate")
			require.Equal(t, tc.want, repo.CountsBridgedUpvoteTotals())
			// The retention job sweeps through its own field; it must be the same
			// configured repository, not one built without the bridge-total option.
			sweeper, ok := app.notificationRetentionSweeper.(interface{ CountsBridgedUpvoteTotals() bool })
			require.True(t, ok, "notification retention sweeper must expose its bridge-total gate")
			require.Equal(t, tc.want, sweeper.CountsBridgedUpvoteTotals(),
				"the retention sweeper must follow the trusted bridge configuration")
		})
	}
}
