//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const subjectStateMethod = "social.coves.moderation.getSubjectState"

type moderationSubjectStateResponse struct {
	State struct {
		Subject     string `json:"subject"`
		RecordState string `json:"recordState"`
		Version     string `json:"version"`
		Moderation  struct {
			State string `json:"state"`
		} `json:"moderation"`
		CurrentSubject struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"currentSubject"`
	} `json:"state"`
}

func TestModerationSubjectStateAPIContract(t *testing.T) {
	p := newPipeline(t)
	author := p.IndexedAccount(t, "modsubject")
	community := indexedCommunity(t, p, "ms", author.DID)
	rkey := testkit.TID()
	post := author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, "moderation subject "+testkit.UniqueID(t), "a post awaiting admission"))
	awaitStatus(t, p, post.URI, community.DID, "pending",
		"the author's post to reach the AppView index through the consumers")

	params := url.Values{"subject": {post.URI}}
	admins := []struct {
		name   string
		number int
	}{
		{name: "first bootstrap admin", number: 1},
		{name: "second bootstrap admin", number: 2},
	}
	for _, admin := range admins {
		t.Run(admin.name, func(t *testing.T) {
			account := testkit.ModerationAdmin(t, admin.number)
			token := account.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
			var response moderationSubjectStateResponse
			err := p.AppView.As(token).Query(t.Context(), subjectStateMethod, params, &response)
			require.NoError(t, err, "the bootstrapped admin must be allowed to read the indexed subject")
			assert.Equal(t, post.URI, response.State.Subject)
			assert.Equal(t, "present", response.State.RecordState)
			assert.Equal(t, "v0", response.State.Version)
			assert.Equal(t, "clear", response.State.Moderation.State)
			assert.Equal(t, post.URI, response.State.CurrentSubject.URI)
			assert.Equal(t, post.CID, response.State.CurrentSubject.CID)
		})
	}

	nonAdmin := p.IndexedAccount(t, "modoutsider")
	t.Run("non-admin service JWT is forbidden", func(t *testing.T) {
		token := nonAdmin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)
		err := p.AppView.As(token).Query(t.Context(), subjectStateMethod, params, nil)
		requireXRPCRefusal(t, err, http.StatusForbidden, "Forbidden", "a non-admin's valid service JWT")
	})
	t.Run("missing credential requires authentication", func(t *testing.T) {
		err := p.AppView.Query(t.Context(), subjectStateMethod, params, nil)
		requireXRPCRefusal(t, err, http.StatusUnauthorized, "AuthRequired", "an unauthenticated request")
	})
	t.Run("service JWT for another method is rejected", func(t *testing.T) {
		account := testkit.ModerationAdmin(t, 1)
		token := account.ServiceAuth(t, communityInstanceDID, "social.coves.community.post.create")
		err := p.AppView.As(token).Query(t.Context(), subjectStateMethod, params, nil)
		requireXRPCRefusal(t, err, http.StatusUnauthorized, "AuthRequired", "a service JWT bound to another method")
	})
	t.Run("service JWT for another audience is rejected", func(t *testing.T) {
		account := testkit.ModerationAdmin(t, 1)
		token := account.ServiceAuth(t, "did:web:other.test", subjectStateMethod)
		err := p.AppView.As(token).Query(t.Context(), subjectStateMethod, params, nil)
		requireXRPCRefusal(t, err, http.StatusUnauthorized, "AuthRequired", "a service JWT addressed to another service")
	})
}
