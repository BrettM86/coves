package main

import (
	"testing"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"

	"github.com/stretchr/testify/require"
)

func TestApplication_WiresCommentReplyNotifications(t *testing.T) {
	app := &application{cfg: &config.Config{}, credentialCipher: credentialciphertest.Fixed()}
	app.buildRepositories()

	require.NotNil(t, app.notificationRepo, "buildRepositories must construct the notification repository")
	require.True(t, app.buildCommentConsumer().NotificationsWired(),
		"the comment consumer registered for the feed must carry the notification repository")
}

// Without the bridge trust, the comment consumer would notify recipients hosted
// on a trusted bridge PDS.
func TestApplication_WiresCommentBridgeTrust(t *testing.T) {
	const trustedBridgeHost = "https://bridge.test"
	app := &application{
		cfg:              &config.Config{Instance: config.InstanceConfig{TrustedBridgePDSHosts: []string{trustedBridgeHost}}},
		credentialCipher: credentialciphertest.Fixed(),
	}
	app.buildRepositories()
	app.buildJetstreamInfrastructure()

	require.True(t, app.bridgeTrust.TrustsPDS(trustedBridgeHost),
		"buildJetstreamInfrastructure must build the bridge trust from the configured hosts")
	require.True(t, app.buildCommentConsumer().BridgeTrustWired(),
		"the comment consumer registered for the feed must carry the bridge trust")
}

func TestApplication_WiresVoteNotificationsAndErasureGate(t *testing.T) {
	app := &application{cfg: &config.Config{}, credentialCipher: credentialciphertest.Fixed()}
	app.buildRepositories()

	require.NotNil(t, app.notificationRepo, "buildRepositories must construct the notification repository")
	consumer := app.buildVoteConsumer()
	require.True(t, consumer.NotificationsWired(),
		"the vote consumer registered for the feed must carry the notification repository")
	require.True(t, consumer.ErasureGated(), "the existing erased-subject gate must remain wired")
}

func TestApplication_WiresVoteBridgeTrust(t *testing.T) {
	const trustedBridgeHost = "https://bridge.test"
	app := &application{
		cfg:              &config.Config{Instance: config.InstanceConfig{TrustedBridgePDSHosts: []string{trustedBridgeHost}}},
		credentialCipher: credentialciphertest.Fixed(),
	}
	app.buildRepositories()
	app.buildJetstreamInfrastructure()

	require.True(t, app.bridgeTrust.TrustsPDS(trustedBridgeHost),
		"buildJetstreamInfrastructure must build bridge trust from the configured hosts")
	consumer := app.buildVoteConsumer()
	require.True(t, consumer.BridgeTrustWired(),
		"the vote consumer registered for the feed must carry bridge trust")
	require.True(t, consumer.ErasureGated(), "the existing erased-subject gate must remain wired")
}

func TestApplication_WiresPostNotifications(t *testing.T) {
	app := &application{cfg: &config.Config{}, credentialCipher: credentialciphertest.Fixed()}
	app.buildRepositories()

	require.NotNil(t, app.notificationRepo, "buildRepositories must construct the notification repository")
	require.True(t, app.buildPostConsumer().NotificationsWired(),
		"the post consumer registered for the feed must carry the notification repository")
}
