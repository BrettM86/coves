package moderation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cdnRecordOwnerDID     = "did:plc:cdnrecordowner"
	cdnRecordCanonicalCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	cdnRecordSecondCID    = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
)

// cdnRecordStore reports the configured subset of recorded targets as newly
// pended and claims whatever an attempt asks for.
type cdnRecordStore struct {
	mu      sync.Mutex
	pended  []imageproxy.BlockedBlob
	records [][]imageproxy.BlockedBlob
	claims  [][]imageproxy.BlockedBlob
}

func (store *cdnRecordStore) RecordCDNPurgeTargets(_ context.Context, blobs []imageproxy.BlockedBlob) ([]imageproxy.BlockedBlob, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.records = append(store.records, append([]imageproxy.BlockedBlob(nil), blobs...))
	return append([]imageproxy.BlockedBlob(nil), store.pended...), nil
}

func (store *cdnRecordStore) ClaimDueCDNPurgeTargets(_ context.Context, claim moderation.CDNPurgeClaim) ([]moderation.CDNPurgeTarget, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.claims = append(store.claims, append([]imageproxy.BlockedBlob(nil), claim.Blobs...))
	targets := make([]moderation.CDNPurgeTarget, len(claim.Blobs))
	for i, blob := range claim.Blobs {
		targets[i] = moderation.CDNPurgeTarget{Blob: blob}
	}
	return targets, nil
}

func (*cdnRecordStore) CompleteCDNPurgeTarget(context.Context, moderation.CDNPurgeTarget) error {
	return nil
}

func (*cdnRecordStore) RescheduleCDNPurgeTarget(context.Context, moderation.CDNPurgeTarget, time.Time, string) error {
	return nil
}

func (store *cdnRecordStore) recorded() [][]imageproxy.BlockedBlob {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([][]imageproxy.BlockedBlob(nil), store.records...)
}

func (store *cdnRecordStore) claimed() [][]imageproxy.BlockedBlob {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([][]imageproxy.BlockedBlob(nil), store.claims...)
}

type cdnRecordPurger struct {
	mu     sync.Mutex
	purged []imageproxy.BlockedBlob
}

func (purger *cdnRecordPurger) PurgeBlobs(_ context.Context, blobs []imageproxy.BlockedBlob) imageproxy.CDNPurgeResult {
	purger.mu.Lock()
	defer purger.mu.Unlock()
	purger.purged = append(purger.purged, blobs...)
	return imageproxy.CDNPurgeResult{Acknowledged: append([]imageproxy.BlockedBlob(nil), blobs...)}
}

func (purger *cdnRecordPurger) purgedBlobs() []imageproxy.BlockedBlob {
	purger.mu.Lock()
	defer purger.mu.Unlock()
	return append([]imageproxy.BlockedBlob(nil), purger.purged...)
}

// recordAndSettle records through a fresh queue and waits for any immediate
// attempt the recording scheduled.
func recordAndSettle(t *testing.T, store *cdnRecordStore, purger *cdnRecordPurger, blobs []imageproxy.BlockedBlob) error {
	t.Helper()
	queue := moderation.NewCDNPurgeQueue(store, purger, moderation.CDNPurgeQueueConfig{WriteTimeout: time.Second})
	err := queue.RecordCDNPurgeTargets(t.Context(), blobs)
	waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, queue.Wait(waitCtx), "immediate attempt did not settle")
	return err
}

func TestCDNPurgeQueueRecordRejectsInvalidTargets(t *testing.T) {
	parsed, err := cid.Decode(cdnRecordCanonicalCID)
	require.NoError(t, err)
	base58CID, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	valid := imageproxy.BlockedBlob{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordCanonicalCID}

	for _, test := range []struct {
		name  string
		blobs []imageproxy.BlockedBlob
	}{
		{name: "empty owner DID", blobs: []imageproxy.BlockedBlob{{CID: cdnRecordCanonicalCID}}},
		{name: "empty CID", blobs: []imageproxy.BlockedBlob{{OwnerDID: cdnRecordOwnerDID}}},
		{name: "uppercase owner DID", blobs: []imageproxy.BlockedBlob{{OwnerDID: "did:plc:CDNRecordOwner", CID: cdnRecordCanonicalCID}}},
		{name: "CID that does not decode", blobs: []imageproxy.BlockedBlob{{OwnerDID: cdnRecordOwnerDID, CID: "bafybeimockimagetest123"}}},
		{name: "non-canonical CID encoding", blobs: []imageproxy.BlockedBlob{{OwnerDID: cdnRecordOwnerDID, CID: base58CID}}},
		{name: "invalid target after a valid one", blobs: []imageproxy.BlockedBlob{valid, {OwnerDID: cdnRecordOwnerDID, CID: base58CID}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &cdnRecordStore{pended: test.blobs}
			purger := &cdnRecordPurger{}
			require.Error(t, recordAndSettle(t, store, purger, test.blobs))
			assert.Empty(t, store.recorded(), "an invalid target must never reach the store")
			assert.Empty(t, store.claimed(), "an invalid target must not schedule an immediate attempt")
			assert.Empty(t, purger.purgedBlobs(), "an invalid target must never reach the CDN")
		})
	}
}

func TestCDNPurgeQueueRecordAttemptsOnlyNewlyPendedTargets(t *testing.T) {
	first := imageproxy.BlockedBlob{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordCanonicalCID}
	second := imageproxy.BlockedBlob{OwnerDID: cdnRecordOwnerDID, CID: cdnRecordSecondCID}
	both := []imageproxy.BlockedBlob{first, second}

	for _, test := range []struct {
		name   string
		pended []imageproxy.BlockedBlob
	}{
		{name: "all newly pended", pended: both},
		{name: "one already pending", pended: []imageproxy.BlockedBlob{second}},
		{name: "all already pending", pended: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &cdnRecordStore{pended: test.pended}
			purger := &cdnRecordPurger{}
			require.NoError(t, recordAndSettle(t, store, purger, both))
			assert.Equal(t, [][]imageproxy.BlockedBlob{both}, store.recorded(), "every valid target is recorded")
			assert.ElementsMatch(t, test.pended, purger.purgedBlobs(),
				"only targets the store inserted or re-pended get an immediate attempt")
			if len(test.pended) == 0 {
				assert.Empty(t, store.claimed(), "nothing newly pended schedules no attempt")
			}
		})
	}
}
