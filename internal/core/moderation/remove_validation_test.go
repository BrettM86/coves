package moderation_test

import (
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveContentRejectsUnsupportedSubjectsReasonsAndOversizedInputs(t *testing.T) {
	const (
		actorDID    = "did:plc:moderationadmin"
		authorDID   = "did:plc:commentauthor"
		instanceDID = "did:web:moderation.test"
		commentCID  = "bafyreicommentvalidation"
		validReason = "social.coves.moderation.defs#reasonSpam"
	)
	clock := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	tooManyGraphemes := strings.Repeat("👍🏽", 1001)
	tooManyBytes := strings.Repeat("👨‍👩‍👧‍👦", 400) + "x"
	require.Equal(t, 1001, uniseg.GraphemeClusterCount(tooManyGraphemes))
	require.LessOrEqual(t, len(tooManyGraphemes), 10000)
	require.Equal(t, 10001, len(tooManyBytes))
	require.LessOrEqual(t, uniseg.GraphemeClusterCount(tooManyBytes), 1000)

	for _, test := range []struct {
		name       string
		collection string
		change     func(*moderation.RemoveContentRequest)
		want       error
	}{
		{name: "non-content collection is unsupported", collection: "app.bsky.feed.post", want: moderation.ErrInvalidSubject},
		{name: "unrecognized reason token", change: func(request *moderation.RemoveContentRequest) {
			request.Reason = "social.coves.moderation.defs#reasonCsam"
		}, want: moderation.ErrUnsupportedReason},
		{name: "unqualified reason", change: func(request *moderation.RemoveContentRequest) {
			request.Reason = "spam"
		}, want: moderation.ErrUnsupportedReason},
		{name: "private note exceeds grapheme limit", change: func(request *moderation.RemoveContentRequest) {
			request.PrivateNote = tooManyGraphemes
		}, want: moderation.ErrInvalidRequest},
		{name: "private note exceeds byte limit", change: func(request *moderation.RemoveContentRequest) {
			request.PrivateNote = tooManyBytes
		}, want: moderation.ErrInvalidRequest},
		{name: "idempotency key exceeds 128 bytes", change: func(request *moderation.RemoveContentRequest) {
			request.IdempotencyKey = strings.Repeat("k", 129)
		}, want: moderation.ErrInvalidRequest},
		{name: "expected version is required", change: func(request *moderation.RemoveContentRequest) {
			request.ExpectedVersion = ""
		}, want: moderation.ErrInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			collection := test.collection
			if collection == "" {
				collection = moderation.CommentCollection
			}
			uri := "at://" + authorDID + "/" + collection + "/3kabc"
			reader := &fakeSubjectReader{record: &moderation.IndexedRecord{URI: uri, CID: commentCID}}
			store := newInMemoryModerationStore(clock)
			store.state.indexedComments[uri] = moderation.IndexedComment{
				URI: uri, CID: commentCID, OwnerDID: authorDID, CommunityDID: "did:plc:community",
			}
			service := moderation.NewService(reader, store, moderation.Config{
				InstanceDID: instanceDID, IdempotencyRetention: 24 * time.Hour,
				MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return clock },
			})
			request := moderation.RemoveContentRequest{
				Subject:         moderation.StrongRef{URI: uri, CID: commentCID},
				ExpectedVersion: "v0", IdempotencyKey: "remove-validation", Reason: validReason,
			}
			if test.change != nil {
				test.change(&request)
			}

			result, err := service.RemoveContent(t.Context(), actorDID, request)
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			assert.Empty(t, store.writeCalls, "validation must not attempt a mutation")
			assert.Empty(t, store.state.actions)
			assert.Empty(t, store.state.activeRemovals)
			assert.Empty(t, store.state.versions)
			assert.Empty(t, store.state.idempotency)
		})
	}

	t.Run("the exact note and key limits allow a seeded comment removal", func(t *testing.T) {
		uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/3kabc"
		reader := &fakeSubjectReader{record: &moderation.IndexedRecord{URI: uri, CID: commentCID}}
		store := newInMemoryModerationStore(clock)
		store.state.indexedComments[uri] = moderation.IndexedComment{
			URI: uri, CID: commentCID, OwnerDID: authorDID, CommunityDID: "did:plc:community",
		}
		service := moderation.NewService(reader, store, moderation.Config{
			InstanceDID: instanceDID, IdempotencyRetention: 24 * time.Hour,
			MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return clock },
		})
		note := strings.Repeat("👍🏽", 1000)
		key := strings.Repeat("k", 128)
		require.Equal(t, 1000, uniseg.GraphemeClusterCount(note))
		require.Len(t, []byte(key), 128)

		result, err := service.RemoveContent(t.Context(), actorDID, moderation.RemoveContentRequest{
			Subject:         moderation.StrongRef{URI: uri, CID: commentCID},
			ExpectedVersion: "v0", IdempotencyKey: key, Reason: validReason, PrivateNote: note,
		})
		require.NotErrorIs(t, err, moderation.ErrInvalidRequest)
		require.NoError(t, err)
		require.NotNil(t, result, "a valid seeded request must reach the mutation path")
	})
}
