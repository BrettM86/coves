//go:build e2e

package e2e

import (
	"context"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestPipelineSmoke is the T2 harness's SELF-TEST. It is deliberately NOT a
// contract and carries no ingestion-contract marker, so it does not satisfy
// cmd/contract-manifest for any collection — task 14 writes the real
// social.coves.actor.profile contract, with the update, delete and blob cases
// this omits.
//
// What it proves is narrower and lands first on purpose: that the pipeline
// contracts of tasks 11-15 have somewhere to run. Namely that in the hermetic
// stack a record written straight to the PDS reaches an AppView serving
// endpoint, through the AppView container's own consumers, within
// contractBudget — and that the harness above (endpoints, budgets, identity,
// waiting, consumer-health diagnostics) is wired correctly end to end.
//
// The shape is the minimum that can prove that, and it takes TWO writes rather
// than one:
//
//   - the account is created through signup, because the user consumer ignores
//     identities the AppView has never seen (see IndexedAccount);
//   - a first profile record is written directly to the repo and waited for.
//     This step ARMS the test rather than asserting anything — see below;
//   - a second record, with a different unique display name, is written to the
//     same rkey and awaited. THIS is the assertion.
//
// # WHY TWO WRITES
//
// The obvious one-write version has a false-pass, and it is the reconciliation
// hazard named in this package's doc. users.maybeBackfillProfile spawns a
// detached goroutine at signup that fetches social.coves.actor.profile/self
// straight from the PDS. If that fetch lands after the record is written, the
// display name appears in Postgres having never touched the firehose, and the
// test goes green with every consumer dead — the exact failure it exists to
// catch.
//
// Backfill is conditional, so the fix is to falsify its condition first. It
// touches a profile only when EVERY field is empty, checked twice: at the spawn
// site (`if user.DisplayName != "" || user.Bio != "" || ...  return`) and again
// immediately before the write, after the fetch, so that a firehose event
// arriving mid-fetch is never clobbered ("firehose delivered a profile while we
// were fetching — keep it"). Once the first display name is in the row, both
// checks are false forever, and no code path other than the consumer can put
// the SECOND display name there.
//
// So: the first write may be delivered by the firehose or by backfill — the
// test does not care which, it only needs the row non-empty. The second write
// has exactly one possible source. A dead Jetstream, an unwired consumer, a
// wrong wantedCollections filter, a consumer stuck on a dead letter — every one
// of them fails this test at the second wait.
func TestPipelineSmoke(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "smoke")

	// The collection is spelled out rather than imported from
	// internal/core/users. It is a wire identifier the PDS, Jetstream and the
	// consumer must all agree on, and a contract that reads the constant would
	// keep passing if the constant changed on both sides at once — which is
	// exactly the federation break it is supposed to catch.
	const profileCollection = "social.coves.actor.profile"

	// Both names are unique per run, so a stale row from an earlier run on a
	// kept PDS volume cannot pass either wait for us.
	armingName := "arming " + testkit.UniqueID(t)
	provingName := "proving " + testkit.UniqueID(t)

	writeProfile := func(displayName, note string) {
		t.Helper()
		account.PutRecord(t, profileCollection, "self", map[string]any{
			"$type":       profileCollection,
			"displayName": displayName,
			"description": note,
		})
	}

	awaitDisplayName := func(displayName, description string) ProfileView {
		t.Helper()
		var observed ProfileView
		p.Await(t, description, func() (bool, error) {
			view, err := p.Profile(context.Background(), account.DID)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			observed = view
			return view.DisplayName == displayName, nil
		})
		return observed
	}

	// Step 1 — arm. Proves nothing on its own (profile backfill could have
	// delivered this); it exists to leave the profile row non-empty, which
	// disarms backfill for the rest of the test.
	writeProfile(armingName, "arming write: may arrive via the firehose OR via profile backfill")
	awaitDisplayName(armingName, "the profile row to become non-empty (disarming profile backfill)")

	// Step 2 — prove. Backfill cannot write over a non-empty profile, so the
	// only remaining path from the PDS to this endpoint is
	// firehose → Jetstream → the AppView's consumers → Postgres.
	writeProfile(provingName, "proving write: only the firehose can deliver this one")
	observed := awaitDisplayName(provingName,
		"the second directly-written profile to reach social.coves.actor.getProfile via the consumers")

	require.Equal(t, provingName, observed.DisplayName)
	require.Equal(t, account.DID, observed.DID,
		"the AppView served a different actor than the one that wrote the record")
}
