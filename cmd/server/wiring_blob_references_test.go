package main

import (
	"testing"

	"Coves/internal/config"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"

	"github.com/stretchr/testify/require"
)

func TestApplication_WiresPostBlobReferences(t *testing.T) {
	app := &application{cfg: &config.Config{}, credentialCipher: credentialciphertest.Fixed()}
	app.buildRepositories()
	require.True(t, app.buildPostConsumer().BlobReferencesWired(),
		"the post consumer registered for the feed must carry the blob reference repository")
}

func TestApplication_WiresCommentBlobReferences(t *testing.T) {
	app := &application{cfg: &config.Config{}, credentialCipher: credentialciphertest.Fixed()}
	app.buildRepositories()
	require.True(t, app.buildCommentConsumer().BlobReferencesWired(),
		"the comment consumer registered for the feed must carry the blob reference repository")
}
