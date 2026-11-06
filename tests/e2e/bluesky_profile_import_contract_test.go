//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const blueskyProfileCollection = "app.bsky.actor.profile"

// TestImportBlueskyProfileContract proves the design's core assumption on a
// real PDS: a Coves profile putRecord may reuse a blob ref the account's own
// Bluesky profile already holds, with no uploadBlob of its own.
//
//	import  → importBlueskyProfile(["displayName","avatar"]) answers 200 naming
//	          exactly those fields, the PDS record carries the Bluesky avatar ref,
//	          and a single getProfile serves the Bluesky name and an avatar URL
//	          with its CID, which pins the write-through wiring in the route
//	consume → a DIRECT putRecord (no AppView call, so no write-through) of the
//	          imported record with a run-unique name and the Bluesky BANNER ref as
//	          avatar reaches getProfile, which only the Jetstream consumer can do
//	dismiss → a second account's dismissBlueskyProfileImport answers 200 twice
func TestImportBlueskyProfileContract(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "bskyimp")
	client := p.AppView.As(p.AppView.SignIn(t, account))

	avatar := account.UploadBlob(t, testkit.TestPNG(64, 64), "image/png")
	banner := account.UploadBlob(t, testkit.TestJPEG(96, 32), "image/jpeg")
	require.NotEqual(t, avatar.CID(), banner.CID(),
		"the avatar and banner must differ, or the consumer phase cannot tell them apart")

	blueskyName := "Bluesky " + testkit.UniqueID(t)
	account.PutRecord(t, blueskyProfileCollection, "self", map[string]any{
		"$type":       blueskyProfileCollection,
		"displayName": blueskyName,
		"description": "bio written on Bluesky",
		"avatar":      blobRefValue(avatar),
		"banner":      blobRefValue(banner),
	})

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// ---- import ---------------------------------------------------------------
	var imported struct {
		URI      string   `json:"uri"`
		CID      string   `json:"cid"`
		Imported []string `json:"imported"`
	}
	require.NoError(t, client.Procedure(ctx, "social.coves.actor.importBlueskyProfile",
		map[string]any{"fields": []string{"displayName", "avatar"}}, &imported),
		"importBlueskyProfile must accept a Coves profile reusing the Bluesky avatar blob ref")
	require.ElementsMatch(t, []string{"displayName", "avatar"}, imported.Imported)
	require.Equal(t, "at://"+account.DID+"/"+profileCollection+"/self", imported.URI)
	require.NotEmpty(t, imported.CID)

	// Read once, not polled: the handler's own write-through must have reached
	// the users table before it answered, ahead of Jetstream.
	view, images, err := p.profileWithImages(ctx, account.DID)
	require.NoError(t, err, "getProfile straight after the import")
	require.Equal(t, blueskyName, view.DisplayName, "the import must write the name through to the index")
	require.Contains(t, images.Avatar, avatar.CID(), "the import must write the avatar through to the index")

	written := account.GetRecord(t, profileCollection, "self")
	require.Equal(t, imported.CID, written.CID, "the response must name the record the PDS holds")
	require.Equal(t, blueskyName, written.Value["displayName"])
	writtenAvatar, ok := written.Value["avatar"].(map[string]any)
	require.True(t, ok, "the written Coves record must carry an avatar blob ref, got %#v", written.Value["avatar"])
	require.Equal(t, map[string]any{"$link": avatar.CID()}, writtenAvatar["ref"],
		"the Coves avatar must be the Bluesky avatar blob ref itself, not a re-upload")
	_, hasBanner := written.Value["banner"]
	require.False(t, hasBanner, "banner was not requested and must not be imported")

	// ---- consume --------------------------------------------------------------
	// Written straight into the repo: the AppView never sees this call, so the
	// only way getProfile can show it is firehose → Jetstream → consumer.
	direct := map[string]any{}
	for key, value := range written.Value {
		direct[key] = value
	}
	direct["$type"] = profileCollection
	direct["avatar"] = blobRefValue(banner)

	// Re-put with a fresh name every ~2s: the users consumer's recency guard
	// skips an event older than users.updated_at, which the import's own
	// write-through and event can push past the first direct put under load.
	var consumerName string
	var lastPut time.Time
	p.Await(t, "the consumer to index the directly written profile with the banner blob as avatar",
		func() (bool, error) {
			if time.Since(lastPut) >= 2*time.Second {
				consumerName = "consumer " + testkit.UniqueID(t)
				direct["displayName"] = consumerName
				account.PutRecord(t, profileCollection, "self", direct)
				lastPut = time.Now()
			}
			view, images, err := p.profileWithImages(context.Background(), account.DID)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			return view.DisplayName == consumerName && strings.Contains(images.Avatar, banner.CID()), nil
		})

	// ---- dismiss --------------------------------------------------------------
	other := p.IndexedAccount(t, "bskydis")
	otherClient := p.AppView.As(p.AppView.SignIn(t, other))
	require.NoError(t, otherClient.Procedure(ctx, "social.coves.actor.dismissBlueskyProfileImport", nil, nil),
		"the first dismissal must answer 200")
	require.NoError(t, otherClient.Procedure(ctx, "social.coves.actor.dismissBlueskyProfileImport", nil, nil),
		"a repeated dismissal is idempotent and must answer 200")
}
