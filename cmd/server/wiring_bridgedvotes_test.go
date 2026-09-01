package main

import (
	"testing"
	"time"

	"Coves/internal/config"

	"github.com/stretchr/testify/require"
)

// No CI tier sets TRUSTED_BRIDGE_PDS_HOSTS, so without this test the poller's
// wiring branch is never executed anywhere: a nil poller, a typo in the host
// list, or a constructor that panics on a nil DB would all ship unnoticed.
// NewBridgedVotesRepository(nil) wraps a nil DB without touching it and
// NewPoller never dials, so no infrastructure is needed.
func TestBuildBridgedVotePoller(t *testing.T) {
	t.Run("unset trust list leaves the poller nil", func(t *testing.T) {
		a := &application{cfg: &config.Config{}}
		require.NoError(t, a.buildBridgedVotePoller())
		require.Nil(t, a.bridgedVotePoller, "an unconfigured deployment must never turn community metadata into outbound traffic")
	})

	t.Run("configured trust list builds the poller", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Instance.TrustedBridgePDSHosts = []string{"https://bridge.example"}
		cfg.Instance.BridgedVotePollInterval = 5 * time.Minute
		a := &application{cfg: cfg}
		require.NoError(t, a.buildBridgedVotePoller())
		require.NotNil(t, a.bridgedVotePoller)
	})

	t.Run("invalid trust entry is a wiring error naming the entry", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Instance.TrustedBridgePDSHosts = []string{"https://bridge.example/pds"}
		a := &application{cfg: cfg}
		err := a.buildBridgedVotePoller()
		require.Error(t, err)
		require.Contains(t, err.Error(), "https://bridge.example/pds")
		require.Equal(t, 1, countOccurrences(err.Error(), "creating bridged vote poller"),
			"the constructor's stage prefix must not be repeated by the caller")
		require.Nil(t, a.bridgedVotePoller)
	})
}

func countOccurrences(s, substr string) int {
	count := 0
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			count++
		}
	}
	return count
}
