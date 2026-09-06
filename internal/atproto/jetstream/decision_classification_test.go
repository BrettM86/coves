package jetstream

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/posts"
)

func TestDecisionClassification(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name               string
		collection         string
		getByDIDErr        error
		wantUnresolved     bool
		wantInfrastructure bool
	}{
		{name: "acceptance missing community", collection: posts.AcceptanceCollection, wantUnresolved: true},
		{name: "removal missing community", collection: posts.RemovalCollection, wantUnresolved: true},
		{name: "acceptance repository failure", collection: posts.AcceptanceCollection, getByDIDErr: errors.New("database unavailable"), wantInfrastructure: true},
		{name: "removal repository failure", collection: posts.RemovalCollection, getByDIDErr: errors.New("database unavailable"), wantInfrastructure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newOriginRepo()
			repo.getByDIDErr = tc.getByDIDErr
			consumer := NewPostEventConsumer(nil, repo, nil, nil, WithAdmissions(&nilAdmissions{}))
			record := map[string]interface{}{
				"$type": tc.collection,
				"subject": map[string]interface{}{
					"uri": "at://did:plc:decisionauthor/social.coves.post.v2/decision",
					"cid": "bafydecision",
				},
				"createdAt": "2026-09-02T00:00:00Z",
			}
			if tc.collection == posts.RemovalCollection {
				record["code"] = "moderator"
			}

			err := consumer.HandleEvent(context.Background(), taxonomyEvent(
				"did:plc:decisioncommunity", tc.collection, "create", "decision", record))
			require.Error(t, err, "a decision from an unverifiable community must not be applied")
			assert.NotErrorIs(t, err, ErrPermanentEvent,
				"community existence can change after cross-repo delivery, so the decision must remain redrivable")

			if tc.wantUnresolved {
				assert.ErrorIs(t, err, ErrUnresolvedReference,
					"a missing community is attacker-reachable cross-repo ordering, not infrastructure; in-line retries would block the serial lane without making the profile arrive")
				assert.NotErrorIs(t, err, errValidationInfra,
					"a repository NotFound is an ordering result, not evidence that the database failed")
			}
			if tc.wantInfrastructure {
				assert.ErrorIs(t, err, errValidationInfra,
					"a generic repository failure must keep infrastructure retries because the database may recover in-line")
				assert.NotErrorIs(t, err, ErrUnresolvedReference,
					"classifying a database outage as an unresolved reference would skip the useful in-line retry ladder")
			}
		})
	}
}
