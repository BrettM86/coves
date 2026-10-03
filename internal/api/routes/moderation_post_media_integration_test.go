//go:build integration

package routes_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	postMediaSpamReason    = "social.coves.moderation.defs#reasonSpam"
	postMediaIllegalReason = "social.coves.moderation.defs#reasonIllegalContent"
	postMediaPreset        = "content_preview"
)

func (h *moderationMediaHarness) indexedImagePost(t *testing.T, collection, imageCID string) (moderation.StrongRef, string) {
	t.Helper()
	var communityDID string
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT community_did FROM posts WHERE uri = $1`, h.postURI).Scan(&communityDID))
	rkey := testkit.TID()
	authorDID := h.ownerA
	blobOwnerDID := authorDID
	uriAuthority := authorDID
	if collection == moderation.LegacyPostCollection {
		blobOwnerDID = communityDID
		uriAuthority = communityDID
	}
	subject := moderation.StrongRef{
		URI: "at://" + uriAuthority + "/" + collection + "/" + rkey,
		CID: mediaImageCID("post record " + rkey),
	}
	embed, err := json.Marshal(map[string]any{
		"$type": "social.coves.embed.images",
		"images": []any{map[string]any{
			"alt": "shared post image",
			"image": map[string]any{
				"$type": "blob", "ref": map[string]any{"$link": imageCID},
				"mimeType": "image/png", "size": 10,
			},
		}},
	})
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, embed, created_at)
		VALUES ($1, $2, $3, $4, $5, 'image post', 'body', $6::jsonb, NOW())
	`, subject.URI, subject.CID, rkey, authorDID, communityDID, string(embed))
	require.NoError(t, err)
	if collection == moderation.PostV2Collection {
		_, err = h.db.ExecContext(t.Context(), `
			INSERT INTO community_post_admissions
				(community_did, post_uri, status, accepted_cid, evaluated_cid, created_at, updated_at)
			VALUES ($1, $2, 'accepted', $3, $3, NOW(), NOW())
		`, communityDID, subject.URI, subject.CID)
		require.NoError(t, err)
	}
	h.pds.mu.Lock()
	h.pds.known[mediaBlobKey{blobOwnerDID, imageCID}] = true
	h.pds.mu.Unlock()
	indexed, err := postgres.NewPostRepository(h.db).GetRawIndexedRow(t.Context(), subject.URI)
	require.NoError(t, err)
	require.Equal(t, subject.CID, indexed.CID)
	require.NotNil(t, indexed.Embed)
	assert.Contains(t, *indexed.Embed, imageCID)
	return subject, blobOwnerDID
}

type postMediaLogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*postMediaLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (capture *postMediaLogCapture) Handle(_ context.Context, record slog.Record) error {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.records = append(capture.records, record.Clone())
	return nil
}
func (capture *postMediaLogCapture) WithAttrs([]slog.Attr) slog.Handler { return capture }
func (capture *postMediaLogCapture) WithGroup(string) slog.Handler      { return capture }
func (capture *postMediaLogCapture) snapshot() []slog.Record {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return append([]slog.Record(nil), capture.records...)
}

// Not parallel: this test replaces slog.Default while the removal runs.
func TestModerationPostCDNFailureDoesNotUndoLocalRemoval(t *testing.T) {
	const token = "route-cdn-token-SENTINEL"
	const bodySentinel = "cf-response-body-SENTINEL"
	clock := &moderationCDNClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	endpoint.SetResponse(http.StatusInternalServerError, bodySentinel)
	h := newModerationCDNPurgeHarness(t, endpoint, clock, 30*time.Second)
	imageCID := mediaImageCID("post cdn failure")
	subject, authorDID := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
	require.Equal(t, h.ownerA, authorDID)
	require.Equal(t, http.StatusOK, h.request(t, postMediaPreset, authorDID, imageCID))
	_, err := os.Stat(h.cachePath(postMediaPreset, authorDID, imageCID))
	require.NoError(t, err, "the post image must be cached on disk before removal")

	logs := &postMediaLogCapture{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	removed := h.remove(t, subject, postMediaSpamReason)
	waitModerationCDN(t, h.queue)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	require.Len(t, endpoint.Requests(), 1, "the failing purge must have been attempted")
	assert.ElementsMatch(t, moderationCDNFiles(authorDID, imageCID), endpoint.Requests()[0].Files)
	h.assertNoCachedBlob(t, imageCID, authorDID)
	require.Equal(t, http.StatusNotFound, h.request(t, postMediaPreset, authorDID, imageCID))
	blocked, err := postgres.NewModerationRepository(h.db).IsBlocked(t.Context(), authorDID, imageCID)
	require.NoError(t, err)
	assert.True(t, blocked)
	first := moderationCDNTarget(t, h.db, authorDID, imageCID)
	assert.Equal(t, "pending", first.state)
	assert.Equal(t, 1, first.attempts)
	assert.Equal(t, "http_500", first.failureCode)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), first.nextAttemptAt)

	var matchingErrors int
	for _, record := range logs.snapshot() {
		var text strings.Builder
		text.WriteString(record.Message)
		attrs := make(map[string]string)
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value.String()
			text.WriteString(" " + attr.Key + "=" + attr.Value.String())
			return true
		})
		assert.NotContains(t, text.String(), token, "no log record may expose the Cloudflare token")
		assert.NotContains(t, text.String(), bodySentinel, "no log record may expose the Cloudflare response")
		if record.Level == slog.LevelError && attrs["code"] == "http_500" && attrs["count"] == "1" {
			matchingErrors++
		}
	}
	assert.Equal(t, 1, matchingErrors, "exactly one aggregated Error record must carry http_500 and count=1 for the first failure")
	slog.SetDefault(previousLogger)

	newQueue := func() *moderation.CDNPurgeQueue {
		return moderation.NewCDNPurgeQueue(postgres.NewModerationRepository(h.db),
			newMediaCloudflarePurger(t, endpoint, token), moderation.CDNPurgeQueueConfig{WriteTimeout: 30 * time.Second, Now: clock.Now})
	}
	clock.Set(time.Date(2026, 9, 28, 12, 0, 59, 999999000, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	assert.Len(t, endpoint.Requests(), 1)
	clock.Set(time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	require.Len(t, endpoint.Requests(), 2)
	assert.ElementsMatch(t, moderationCDNFiles(authorDID, imageCID), endpoint.Requests()[1].Files)
	second := moderationCDNTarget(t, h.db, authorDID, imageCID)
	assert.Equal(t, "pending", second.state)
	assert.Equal(t, 2, second.attempts)
	assert.Equal(t, "http_500", second.failureCode)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 3, 0, 0, time.UTC), second.nextAttemptAt)
	clock.Set(time.Date(2026, 9, 28, 12, 2, 59, 999999000, time.UTC))
	require.NoError(t, h.queue.Sweep(t.Context()))
	assert.Len(t, endpoint.Requests(), 2)

	clock.Set(time.Date(2026, 9, 28, 12, 3, 0, 0, time.UTC))
	entered, release := endpoint.HoldNextRequest(t)
	firstSweep := make(chan error, 1)
	go func() { firstSweep <- newQueue().Sweep(t.Context()) }()
	awaitModerationCDN(t, "first concurrent sweep request", entered)
	require.NoError(t, newQueue().Sweep(t.Context()))
	assert.Len(t, endpoint.Requests(), 3, "the second queue cannot claim an in-flight target")
	release()
	require.NoError(t, awaitModerationCDN(t, "first concurrent sweep to finish", firstSweep))
	assert.ElementsMatch(t, moderationCDNFiles(authorDID, imageCID), endpoint.Requests()[2].Files)
	third := moderationCDNTarget(t, h.db, authorDID, imageCID)
	assert.Equal(t, "pending", third.state)
	assert.Equal(t, 3, third.attempts)
	assert.Equal(t, time.Date(2026, 9, 28, 12, 7, 0, 0, time.UTC), third.nextAttemptAt)
	endpoint.SetResponse(http.StatusOK, `{"success":true}`)
	clock.Set(time.Date(2026, 9, 28, 12, 7, 0, 0, time.UTC))
	require.NoError(t, newQueue().Sweep(t.Context()), "a restarted queue must recover the due target")
	require.Len(t, endpoint.Requests(), 4)
	assert.ElementsMatch(t, moderationCDNFiles(authorDID, imageCID), endpoint.Requests()[3].Files)
	assert.Equal(t, "completed", moderationCDNTarget(t, h.db, authorDID, imageCID).state)
	clock.Set(time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC))
	require.NoError(t, newQueue().Sweep(t.Context()))
	assert.Len(t, endpoint.Requests(), 4)
}

func TestModerationPostV2MediaRemovalColdAndWarm(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := newModerationMediaHarness(t, false)
			imageCID := mediaImageCID("postv2 removal " + name)
			subject, ownerDID := h.indexedImagePost(t, moderation.PostV2Collection, imageCID)
			require.Equal(t, h.ownerA, ownerDID, "postv2 image blobs belong to the author")
			if warm {
				for _, preset := range []string{postMediaPreset, "content_full"} {
					require.Equal(t, http.StatusOK, h.request(t, preset, ownerDID, imageCID))
					_, err := os.Stat(h.cachePath(preset, ownerDID, imageCID))
					require.NoError(t, err, "the image must be on disk before removal")
				}
			}
			before := h.pds.count(ownerDID, imageCID)
			if warm {
				require.Equal(t, 2, before)
			} else {
				require.Zero(t, before, "a cold image has not been fetched")
			}
			removed := h.remove(t, subject, postMediaSpamReason)
			h.assertNoCachedBlob(t, imageCID, ownerDID)
			for _, preset := range []string{postMediaPreset, "content_full"} {
				require.Equal(t, http.StatusNotFound, h.request(t, preset, ownerDID, imageCID))
			}
			assert.Equal(t, before, h.pds.count(ownerDID, imageCID), "blocked post image must not be fetched")
			h.restore(t, subject, removed)
			require.Equal(t, http.StatusOK, h.request(t, postMediaPreset, ownerDID, imageCID))
			assert.Equal(t, before+1, h.pds.count(ownerDID, imageCID), "restored postv2 image must come from a new PDS fetch")
		})
	}
}

func TestModerationLegacyPostSharedImageRemovalAndRestore(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := newModerationMediaHarness(t, false)
			imageCID := mediaImageCID("legacy shared post " + name)
			first, ownerDID := h.indexedImagePost(t, moderation.LegacyPostCollection, imageCID)
			second, secondOwner := h.indexedImagePost(t, moderation.LegacyPostCollection, imageCID)
			require.Equal(t, ownerDID, secondOwner)
			require.NotEqual(t, h.ownerA, ownerDID, "legacy post blobs belong to the community")
			secondView, err := postgres.NewPostRepository(h.db).GetViewsByURIs(t.Context(), []string{second.URI}, "")
			require.NoError(t, err)
			require.NotNil(t, secondView[second.URI], "the unremoved legacy post must remain served")
			if warm {
				for _, preset := range []string{postMediaPreset, "content_full"} {
					require.Equal(t, http.StatusOK, h.request(t, preset, ownerDID, imageCID))
					_, err := os.Stat(h.cachePath(preset, ownerDID, imageCID))
					require.NoError(t, err)
				}
			}
			before := h.pds.count(ownerDID, imageCID)
			removed := h.remove(t, first, postMediaSpamReason)
			h.assertNoCachedBlob(t, imageCID, ownerDID)
			// PRD §9 Media: one community-owned blob shared by two legacy posts is blocked for both when either is removed.
			secondView, err = postgres.NewPostRepository(h.db).GetViewsByURIs(t.Context(), []string{second.URI}, "")
			require.NoError(t, err)
			require.NotNil(t, secondView[second.URI], "the second post remains visible even though its shared image is blocked")
			for _, preset := range []string{postMediaPreset, "content_full"} {
				require.Equal(t, http.StatusNotFound, h.request(t, preset, secondOwner, imageCID), "the shared image on L2 must be refused")
			}
			assert.Equal(t, before, h.pds.count(ownerDID, imageCID))
			h.pds.mu.Lock()
			h.pds.known[mediaBlobKey{h.ownerB, imageCID}] = true
			h.pds.mu.Unlock()
			require.Equal(t, http.StatusOK, h.request(t, postMediaPreset, h.ownerB, imageCID), "spam is scoped to the community blob owner")
			assert.Equal(t, 1, h.pds.count(h.ownerB, imageCID))
			h.restore(t, first, removed)
			require.Equal(t, http.StatusOK, h.request(t, postMediaPreset, ownerDID, imageCID))
			assert.Equal(t, before+1, h.pds.count(ownerDID, imageCID), "restore must re-fetch the purged community-owned image")
		})
	}
}

func TestModerationLegacyPostIllegalContentBlocksEveryOwner(t *testing.T) {
	h, _ := newModerationMediaHarness(t, false)
	imageCID := mediaImageCID("legacy global removal")
	subject, communityDID := h.indexedImagePost(t, moderation.LegacyPostCollection, imageCID)
	owners := []string{communityDID, h.ownerA, h.ownerB}
	h.pds.mu.Lock()
	for _, owner := range owners {
		h.pds.known[mediaBlobKey{owner, imageCID}] = true
	}
	h.pds.mu.Unlock()
	for _, owner := range owners {
		for _, preset := range []string{postMediaPreset, "content_full"} {
			require.Equal(t, http.StatusOK, h.request(t, preset, owner, imageCID))
			_, err := os.Stat(h.cachePath(preset, owner, imageCID))
			require.NoError(t, err, "each owner's blob must be cached before global removal")
		}
	}
	removed := h.remove(t, subject, postMediaIllegalReason)
	h.assertNoCachedBlob(t, imageCID, "")
	for _, owner := range owners {
		before := h.pds.count(owner, imageCID)
		require.Equal(t, http.StatusNotFound, h.request(t, postMediaPreset, owner, imageCID))
		assert.Equal(t, before, h.pds.count(owner, imageCID), "ownerless block must refuse every owner's blob without fetching")
	}
	h.restore(t, subject, removed)
	for _, owner := range owners {
		before := h.pds.count(owner, imageCID)
		require.Equal(t, http.StatusOK, h.request(t, postMediaPreset, owner, imageCID))
		assert.Equal(t, before+1, h.pds.count(owner, imageCID), "restore must trigger a real PDS fetch for each purged owner")
	}
}

// postV2ImageEvent is a postv2 commit in ownerDID's repo whose record embeds imageCIDs.
func postV2ImageEvent(ownerDID, communityDID, operation, rkey, rev, recordCID string, timeUS int64, imageCIDs ...string) *jetstream.JetstreamEvent {
	var record map[string]interface{}
	if operation != "delete" {
		images := make([]interface{}, 0, len(imageCIDs))
		for _, imageCID := range imageCIDs {
			images = append(images, map[string]interface{}{"alt": "recreated image", "image": map[string]interface{}{
				"$type": "blob", "ref": map[string]interface{}{"$link": imageCID}, "mimeType": "image/png", "size": 10,
			}})
		}
		record = map[string]interface{}{
			"$type": jetstream.PostV2Collection, "community": communityDID, "title": "recreated post",
			"content": "body", "createdAt": "2026-03-01T00:00:00Z",
			"embed": map[string]interface{}{"$type": "social.coves.embed.images", "images": images},
		}
	}
	return &jetstream.JetstreamEvent{Kind: "commit", Did: ownerDID, TimeUS: timeUS, Commit: &jetstream.CommitEvent{
		Rev: rev, Operation: operation, Collection: jetstream.PostV2Collection, RKey: rkey, CID: recordCID, Record: record,
	}}
}

// PRD Definition of done: deleting and recreating a removed postv2 keeps it
// removed and blocks the new blob, which the author's repo serves even though
// the AppView keeps the tombstone.
func TestModerationPostV2RecreatedImageBlockedColdAndWarm(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := newModerationMediaHarness(t, false)
			var communityDID string
			require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT community_did FROM posts WHERE uri = $1`, h.postURI).Scan(&communityDID))
			admissions := postgres.NewAdmissionRepository(h.db)
			consumer := jetstream.NewPostEventConsumer(
				postgres.NewPostRepository(h.db), postgres.NewCommunityRepository(h.db, credentialciphertest.Fixed()),
				users.NewUserService(postgres.NewUserRepository(h.db), nil, testkit.Endpoints().PDS.BaseURL, nil, ""), h.db,
				jetstream.WithAdmissions(admissions),
				jetstream.WithPostMediaReconciler(moderation.NewMediaReconciler(
					postgres.NewModerationRepository(h.db), fixtures.InstanceDID(), h.proxyService)),
			)
			originalCID, recreatedCID := mediaImageCID("recreate original "+name), mediaImageCID("recreate new "+name)
			rkey := testkit.TID()
			revs := []string{testkit.TID(), testkit.TID(), testkit.TID()}
			createdAt := time.Now().Add(-time.Minute).UnixMicro()
			subject := moderation.StrongRef{URI: "at://" + h.ownerA + "/" + jetstream.PostV2Collection + "/" + rkey, CID: mediaImageCID("recreate record " + name)}
			require.NoError(t, consumer.HandleEvent(t.Context(),
				postV2ImageEvent(h.ownerA, communityDID, "create", rkey, revs[0], subject.CID, createdAt, originalCID)))
			acceptanceRkey := testkit.TID()
			accepted, err := admissions.ApplyAcceptance(t.Context(), posts.ApplyAcceptanceCommand{
				CommunityDID: communityDID, PostURI: subject.URI,
				AcceptanceURI:  "at://" + communityDID + "/" + posts.AcceptanceCollection + "/" + acceptanceRkey,
				AcceptanceRkey: acceptanceRkey, PinnedCID: subject.CID,
				Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
			})
			require.NoError(t, err)
			require.Equal(t, posts.AdmissionApplied, accepted.Outcome)
			removed := h.remove(t, subject, postMediaSpamReason)
			require.NoError(t, consumer.HandleEvent(t.Context(),
				postV2ImageEvent(h.ownerA, communityDID, "delete", rkey, revs[1], "", createdAt+1_000_000)))

			h.pds.mu.Lock()
			h.pds.known[mediaBlobKey{h.ownerA, recreatedCID}] = true
			h.pds.mu.Unlock()
			if warm {
				// The new blob is fetchable from the author's repo before the
				// recreate reaches the AppView, so it can already be cached.
				for _, preset := range []string{postMediaPreset, "content_full"} {
					require.Equal(t, http.StatusOK, h.request(t, preset, h.ownerA, recreatedCID))
					_, err := os.Stat(h.cachePath(preset, h.ownerA, recreatedCID))
					require.NoError(t, err, "the new image must be on disk before the recreate")
				}
			}
			before := h.pds.count(h.ownerA, recreatedCID)
			if warm {
				require.Equal(t, 2, before)
			} else {
				require.Zero(t, before, "a cold image has not been fetched")
			}

			require.NoError(t, consumer.HandleEvent(t.Context(),
				postV2ImageEvent(h.ownerA, communityDID, "create", rkey, revs[2], subject.CID, createdAt+2_000_000, originalCID, recreatedCID)))
			indexed, err := postgres.NewPostRepository(h.db).GetRawIndexedRow(t.Context(), subject.URI)
			require.NoError(t, err)
			require.NotNil(t, indexed.DeletedAt, "the recreate must not resurrect the tombstone")
			state, err := h.moderation.GetSubjectState(t.Context(), subject.URI)
			require.NoError(t, err)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
			require.NotNil(t, state.LocalRemoval)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)

			h.assertNoCachedBlob(t, recreatedCID, h.ownerA)
			for _, preset := range []string{postMediaPreset, "content_full"} {
				for range 2 {
					require.Equal(t, http.StatusNotFound, h.request(t, preset, h.ownerA, recreatedCID))
				}
			}
			assert.Equal(t, before, h.pds.count(h.ownerA, recreatedCID), "the blocked recreated image must not be fetched")
		})
	}
}
