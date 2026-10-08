package main

import (
	"testing"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

func TestBuildBridgedVotePoller_WiresNotificationsAndBridgeTrust(t *testing.T) {
	a := &application{
		cfg: &config.Config{Instance: config.InstanceConfig{
			TrustedBridgePDSHosts: []string{"https://bridge.test"},
		}},
		credentialCipher: credentialciphertest.Fixed(),
	}
	a.buildRepositories()
	a.buildJetstreamInfrastructure()
	require.NoError(t, a.buildBridgedVotePoller())
	require.NotNil(t, a.notificationRepo)
	require.NotNil(t, a.bridgeTrust)
	require.NotNil(t, a.bridgedVotePoller)
	store := a.bridgedVotePoller.Store()
	require.IsType(t, (*postgres.BridgedVotesRepository)(nil), store)
	repo := store.(*postgres.BridgedVotesRepository)
	notificationRepo, bridgeTrust := repo.NotificationWiring()
	require.NotNil(t, notificationRepo)
	require.NotNil(t, bridgeTrust)
	require.Same(t, a.notificationRepo, notificationRepo)
	require.Same(t, a.bridgeTrust, bridgeTrust)
}
