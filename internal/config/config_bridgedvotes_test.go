package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBridgedVotePollConfigDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, cfg.Instance.BridgedVotePollInterval)
	require.Zero(t, cfg.Instance.BridgedVotePollLookback,
		"zero delegates the lookback default to the poller")
	require.Zero(t, cfg.Instance.BridgedVotePollSweepCap,
		"zero delegates the sweep-cap default to the poller")
}

func TestBridgedVotePollConfigParsesExplicitValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("BRIDGED_VOTE_POLL_INTERVAL", "17s")
	t.Setenv("BRIDGED_VOTE_POLL_LOOKBACK", "72h")
	t.Setenv("BRIDGED_VOTE_POLL_SWEEP_CAP", "321")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, 17*time.Second, cfg.Instance.BridgedVotePollInterval)
	require.Equal(t, 72*time.Hour, cfg.Instance.BridgedVotePollLookback)
	require.Equal(t, 321, cfg.Instance.BridgedVotePollSweepCap)
}

func TestBridgedVotePollConfigRejectsInvalidInterval(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("BRIDGED_VOTE_POLL_INTERVAL", "not-a-duration")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "BRIDGED_VOTE_POLL_INTERVAL")
}

func TestBridgedVotePollConfigRejectsNonPositiveInterval(t *testing.T) {
	for _, value := range []string{"0s", "-5m"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("TRUSTED_BRIDGE_PDS_HOSTS", "https://tdpl.io")
			t.Setenv("BRIDGED_VOTE_POLL_INTERVAL", value)

			_, err := Load()
			require.Error(t, err, "zero is not a disable switch for this job; a typo must fail the boot")
			require.Contains(t, err.Error(), "BRIDGED_VOTE_POLL_INTERVAL")
		})
	}
}

func TestBridgedVotePollConfigRejectsNegativeTuning(t *testing.T) {
	t.Run("lookback", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("BRIDGED_VOTE_POLL_LOOKBACK", "-1h")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "BRIDGED_VOTE_POLL_LOOKBACK")
	})
	t.Run("sweep cap", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("BRIDGED_VOTE_POLL_SWEEP_CAP", "-1")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "BRIDGED_VOTE_POLL_SWEEP_CAP")
	})
}

func TestTrustedBridgePDSHostsValidatedAtLoad(t *testing.T) {
	t.Run("scheme and host accepted", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("IS_DEV_ENV", "true")
		t.Setenv("TRUSTED_BRIDGE_PDS_HOSTS", "https://tdpl.io, https://bridge.example:8443/")

		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, []string{"https://tdpl.io", "https://bridge.example:8443/"}, cfg.Instance.TrustedBridgePDSHosts)
	})

	invalid := map[string]string{
		"schemeless (BridgeTrust used to tolerate it)": "tdpl.io",
		"path":                          "https://tdpl.io/pds",
		"credentials":                   "https://user:secret@tdpl.io",
		"one bad entry among good ones": "https://tdpl.io,ftp://bridge.example",
	}
	for name, value := range invalid {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("TRUSTED_BRIDGE_PDS_HOSTS", value)

			_, err := Load()
			require.Error(t, err, "the same list gates bridgedStats provenance, so a value one consumer would refuse must fail as config")
			require.Contains(t, err.Error(), "TRUSTED_BRIDGE_PDS_HOSTS")
		})
	}
}

func TestBridgedVotePollIntervalIsNotValidatedWithoutTrustedHosts(t *testing.T) {
	clearEnv(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("BRIDGED_VOTE_POLL_INTERVAL", "0s")

	_, err := Load()
	require.NoError(t, err, "with no trust list there is no poller to misconfigure")
}
