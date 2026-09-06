package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/posts"
)

// nilAdmissions makes the decision collections observable without letting the
// acceptance path reach a store after the missing-community guard.
type nilAdmissions struct{ posts.AdmissionRepository }

func boundedTransientEventJSON(t *testing.T, event *JetstreamEvent) []byte {
	t.Helper()
	data, err := json.Marshal(event)
	require.NoError(t, err, "the connector fixture must carry a real Jetstream event")
	return data
}

func TestBoundedTransientSites(t *testing.T) {
	run := func(
		t *testing.T,
		consumerName string,
		handler EventHandler,
		failedEvent *JetstreamEvent,
		healthyEvent *JetstreamEvent,
		consumerCalls func() int,
	) {
		t.Helper()

		server := newJetstreamTestServer(t, [][]byte{
			boundedTransientEventJSON(t, failedEvent),
			boundedTransientEventJSON(t, healthyEvent),
		})
		cursorStore := newFakeCursorStore()
		deadLetters := newFakeDeadLetterQueue()
		connector := NewConnector(consumerName, server.wsURL(), handler,
			fastConnectorOptions(WithCursorStore(cursorStore), WithDeadLetterWriter(deadLetters))...)
		startConnector(t, connector)

		waitFor(t, 2*time.Second, "attacker-reachable unresolved event dead-lettered", func() bool {
			return deadLetters.rowCount() == 1
		})
		waitFor(t, 2*time.Second, "healthy follow-up advanced the serial consumer lane", func() bool {
			return cursorStore.get(consumerName) == healthyEvent.TimeUS
		})

		require.Equal(t, 1, deadLetters.rowCount(),
			"only the unresolved event may enter the dead-letter queue; the healthy follow-up proves the lane moved on")
		assert.Equal(t, 1, consumerCalls(),
			"an attacker-reachable unresolved reference must consume one handler call; in-line retries let each fabricated event deny the serial lane for 4-44 seconds before 10 redrives")

		row := deadLetters.row(0)
		assert.Equal(t, failedEvent.TimeUS, row.EventTimeUS,
			"the dead letter must identify the unresolved event rather than the healthy follow-up")
		wantAttempts := MaxRedriveAttempts - UnresolvedRedriveAttempts
		assert.Equal(t, wantAttempts, row.Attempts,
			"an unresolved reference must retain only the bounded redrive budget; granting all 10 redrives compounds attacker-controlled lane-time denial")

		latest, err := deadLetters.LatestDeadLetterID(context.Background(), consumerName)
		require.NoError(t, err, "the bounded dead letter must remain inspectable by the redriver")
		retryable, err := deadLetters.ListRetryable(context.Background(), DeadLetterPageQuery{
			ConsumerName: consumerName,
			MaxAttempts:  MaxRedriveAttempts,
			ThroughID:    latest,
			Limit:        100,
		})
		require.NoError(t, err, "the redriver must be able to query genuine cross-repo ordering failures")
		assert.Len(t, retryable, 1,
			"the bounded row must stay retryable so ordering can converge without granting fabricated references 10 redrives")
		assert.Equal(t, healthyEvent.TimeUS, cursorStore.get(consumerName),
			"advancing through the healthy follow-up proves one unresolved reference cannot pin the serial lane")
	}

	t.Run("user profile identity resolution", func(t *testing.T) {
		resolver := &mockIdentityResolverForUser{resolveErr: errors.New("identity directory unavailable")}
		consumer := NewUserEventConsumer(newMockUserService(), resolver)
		run(t, ConsumerUsers, consumer,
			&JetstreamEvent{
				Did:    "did:plc:boundeduser",
				Kind:   "commit",
				TimeUS: 101_000,
				Commit: &CommitEvent{
					Rev:        "3lboundeduser",
					Operation:  "create",
					Collection: CovesProfileCollection,
					RKey:       "self",
					CID:        "bafyboundeduser",
					Record:     map[string]interface{}{"$type": CovesProfileCollection},
				},
			},
			&JetstreamEvent{
				Did:    "did:plc:boundeduser",
				Kind:   "commit",
				TimeUS: 102_000,
				Commit: &CommitEvent{Operation: "create", Collection: "social.coves.test", RKey: "healthy"},
			},
			func() int { return resolver.calls },
		)
	})

	t.Run("community profile handle resolution", func(t *testing.T) {
		repo := newOriginRepo()
		resolver := &countingResolver{handle: identity.InvalidHandle, pdsURL: untrustedPDS}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)
		run(t, ConsumerCommunities, consumer,
			&JetstreamEvent{
				Did:    "did:plc:boundedcommunity",
				Kind:   "commit",
				TimeUS: 201_000,
				Commit: profileCommit("create", "bounded", "", nil),
			},
			&JetstreamEvent{
				Did:    "did:plc:boundedcommunity",
				Kind:   "commit",
				TimeUS: 202_000,
				Commit: &CommitEvent{Operation: "create", Collection: "social.coves.test", RKey: "healthy"},
			},
			func() int { return resolver.calls },
		)
	})

	t.Run("acceptance from an unindexed community", func(t *testing.T) {
		repo := newOriginRepo()
		consumer := NewPostEventConsumer(nil, repo, nil, nil, WithAdmissions(&nilAdmissions{}))
		run(t, ConsumerPosts, consumer,
			&JetstreamEvent{
				Did:    "did:plc:boundedcommunity",
				Kind:   "commit",
				TimeUS: 301_000,
				Commit: &CommitEvent{
					Rev:        "3lboundedacceptance",
					Operation:  "create",
					Collection: posts.AcceptanceCollection,
					RKey:       "boundedacceptance",
					CID:        "bafyboundedacceptance",
					Record: map[string]interface{}{
						"$type": posts.AcceptanceCollection,
						"subject": map[string]interface{}{
							"uri": "at://did:plc:someone/social.coves.post.v2/x",
							"cid": "bafyboundedsubject",
						},
						"createdAt": "2026-09-02T00:00:00Z",
					},
				},
			},
			&JetstreamEvent{
				Did:    "did:plc:boundedcommunity",
				Kind:   "commit",
				TimeUS: 302_000,
				Commit: &CommitEvent{Operation: "create", Collection: "social.coves.test", RKey: "healthy"},
			},
			func() int { return repo.getByDIDCalls },
		)
	})
}
