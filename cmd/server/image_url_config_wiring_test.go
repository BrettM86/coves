package main

import (
	"fmt"
	"testing"

	"Coves/internal/config"
	"Coves/internal/core/blobs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildImageProxy publishes AllowPrivateBlobHosts from allowPrivateHosts. Both
// directions are pinned: a hardcoded true unguards production blob URLs, and a
// hardcoded false breaks the local PDS stack. The proxy is left disabled so the
// test reaches the publish without a cache directory or fetcher.
func TestBuildImageProxy_PublishesAllowPrivateBlobHostsFromIsDevEnv(t *testing.T) {
	for _, isDevEnv := range []bool{false, true} {
		t.Run(fmt.Sprintf("IsDevEnv=%t", isDevEnv), func(t *testing.T) {
			blobs.ResetImageURLConfigForTesting()
			t.Cleanup(blobs.ResetImageURLConfigForTesting)

			a := &application{cfg: &config.Config{IsDevEnv: isDevEnv}}
			purger, err := a.buildImageProxy()
			require.NoError(t, err)
			require.Nil(t, purger)

			assert.Equal(t, isDevEnv, blobs.GetImageURLConfig().AllowPrivateBlobHosts)
		})
	}
}
