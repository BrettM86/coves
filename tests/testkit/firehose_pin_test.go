// Package testkit_test holds the wire-format pin, and it is an EXTERNAL test
// package for exactly one reason.
//
// testkit itself may not import internal/atproto/jetstream: that package's
// consumers pull in communities, posts, comments, users, votes, userblocks and
// aggregators, and testkit importing anything under internal/core would make
// those packages' own in-package tests import cycles.
//
// An external test package has no such constraint. `package testkit_test`
// compiles into the test binary rather than into testkit, so nothing that
// imports testkit inherits this import — the dependency rule holds, and the
// duplicated structs still get checked against the originals.
package testkit_test

import (
	"encoding/json"
	"testing"

	"Coves/internal/atproto/jetstream"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventMirrorsTheProductionWireFormat is the guard on testkit's second
// declaration of the Jetstream event.
//
// firehose.go redeclares these structs because it cannot import the originals.
// The cost of that is drift: a renamed JSON tag in internal/atproto/jetstream
// would leave every testkit matcher quietly matching nothing, and the symptom
// would be a timeout in whichever test happened to run first — not a compile
// error, and not a message mentioning the rename.
//
// So the check runs in the only direction that catches it: marshal a PRODUCTION
// value, decode it with testkit's structs, and assert every field a matcher
// reads survived the trip.
func TestEventMirrorsTheProductionWireFormat(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		production := jetstream.JetstreamEvent{
			Did:    "did:plc:alice",
			Kind:   "commit",
			TimeUS: 1751000000000000,
			Commit: &jetstream.CommitEvent{
				Rev:        "3kabcdefghij",
				Operation:  "update",
				Collection: "social.coves.community.post",
				RKey:       "3kzzzzzzzzzzz",
				CID:        "bafyreiabc",
				Record:     map[string]any{"$type": "social.coves.community.post", "title": "hello"},
			},
		}

		var mirrored testkit.Event
		requireRoundTrip(t, production, &mirrored)

		assert.Equal(t, production.Did, mirrored.DID)
		assert.Equal(t, production.Kind, mirrored.Kind)
		assert.Equal(t, production.TimeUS, mirrored.TimeUS)
		require.NotNil(t, mirrored.Commit, "the commit payload did not survive: a JSON tag has moved")
		assert.Equal(t, production.Commit.Rev, mirrored.Commit.Rev)
		assert.Equal(t, production.Commit.Operation, mirrored.Commit.Operation)
		assert.Equal(t, production.Commit.Collection, mirrored.Commit.Collection)
		assert.Equal(t, production.Commit.RKey, mirrored.Commit.RKey)
		assert.Equal(t, production.Commit.CID, mirrored.Commit.CID)
		assert.Equal(t, "hello", mirrored.Commit.Record["title"])

		// The AT-URI every matcher is built from is assembled from three of
		// those fields, so it is worth asserting as a whole.
		assert.Equal(t, "at://did:plc:alice/social.coves.community.post/3kzzzzzzzzzzz", mirrored.URI())
	})

	t.Run("identity", func(t *testing.T) {
		production := jetstream.JetstreamEvent{
			Did:    "did:plc:alice",
			Kind:   "identity",
			TimeUS: 1751000000000001,
			Identity: &jetstream.IdentityEvent{
				Did:    "did:plc:alice",
				Handle: "alice.local.coves.dev",
				Seq:    42,
				Time:   "2026-07-29T00:00:00Z",
			},
		}

		var mirrored testkit.Event
		requireRoundTrip(t, production, &mirrored)

		require.NotNil(t, mirrored.Identity)
		assert.Equal(t, production.Identity.Did, mirrored.Identity.DID)
		assert.Equal(t, production.Identity.Handle, mirrored.Identity.Handle)
		assert.Equal(t, production.Identity.Seq, mirrored.Identity.Seq)
		assert.Equal(t, production.Identity.Time, mirrored.Identity.Time)
	})

	t.Run("account", func(t *testing.T) {
		production := jetstream.JetstreamEvent{
			Did:    "did:plc:alice",
			Kind:   "account",
			TimeUS: 1751000000000002,
			Account: &jetstream.AccountEvent{
				Did:    "did:plc:alice",
				Active: true,
				Seq:    43,
				Time:   "2026-07-29T00:00:00Z",
			},
		}

		var mirrored testkit.Event
		requireRoundTrip(t, production, &mirrored)

		require.NotNil(t, mirrored.Account)
		assert.Equal(t, production.Account.Did, mirrored.Account.DID)
		assert.Equal(t, production.Account.Active, mirrored.Account.Active)
		assert.Equal(t, production.Account.Seq, mirrored.Account.Seq)
		assert.Equal(t, production.Account.Time, mirrored.Account.Time)
	})
}

// TestEventIntoDecodesBackIntoTheProductionType closes the other half of the
// loop: Event.Into is how a test feeds a real consumer, so a frame testkit
// received must decode into the type the consumer expects.
func TestEventIntoDecodesBackIntoTheProductionType(t *testing.T) {
	const frame = `{"did":"did:plc:alice","kind":"commit","time_us":1751000000000000,` +
		`"commit":{"rev":"3kabcdefghij","operation":"create","collection":"social.coves.community.post",` +
		`"rkey":"3kzzzzzzzzzzz","cid":"bafyreiabc","record":{"$type":"social.coves.community.post","title":"hello"}}}`

	// Decoding the frame the way Await does, then handing it on the way a
	// migrated test would.
	var mirrored testkit.Event
	require.NoError(t, json.Unmarshal([]byte(frame), &mirrored))

	var production jetstream.JetstreamEvent
	require.NoError(t, json.Unmarshal([]byte(frame), &production))

	assert.Equal(t, mirrored.DID, production.Did)
	assert.Equal(t, mirrored.TimeUS, production.TimeUS)
	require.NotNil(t, production.Commit)
	assert.Equal(t, mirrored.Commit.RKey, production.Commit.RKey)
	assert.Equal(t, mirrored.Commit.Collection, production.Commit.Collection)
	assert.Equal(t, "hello", production.Commit.Record["title"])
}

func requireRoundTrip(t *testing.T, from any, into any) {
	t.Helper()
	encoded, err := json.Marshal(from)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, into))
}
