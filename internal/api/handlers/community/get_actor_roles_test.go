package community

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
)

type actorRoleJSON struct {
	Community string `json:"community"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

type getActorRolesJSON struct {
	Actor string          `json:"actor"`
	Roles []actorRoleJSON `json:"roles"`
}

func TestGetActorRoles_ListsEveryCommunityRole(t *testing.T) {
	t.Parallel()

	const actor = "did:plc:rolesactor"
	reader := fakeRoleReader{bySubject: map[string][]communities.CommunityRole{actor: {
		{
			CommunityDID: "did:plc:gamingcommunity", SubjectDID: actor, Role: "owner",
			RecordKey: "actorkey", RecordCID: "bafyreiowner", RecordCreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			CommunityDID: "did:plc:chesscommunity", SubjectDID: actor, Role: "moderator",
			RecordKey: "actorkey", RecordCID: "bafyreimoderator", RecordCreatedAt: time.Date(2026, 11, 2, 11, 30, 0, 0, time.UTC),
		},
	}}}
	handler := NewGetActorRolesHandler(reader)

	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.community.getActorRoles?actor="+url.QueryEscape(actor), nil)
	rec := httptest.NewRecorder()

	handler.HandleGetActorRoles(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got getActorRolesJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "response body %q is not a getActorRoles response", rec.Body.String())
	assert.Equal(t, actor, got.Actor)
	assert.ElementsMatch(t, []actorRoleJSON{
		{Community: "did:plc:gamingcommunity", Role: "owner", CreatedAt: "2026-11-01T10:00:00Z"},
		{Community: "did:plc:chesscommunity", Role: "moderator", CreatedAt: "2026-11-02T11:30:00Z"},
	}, got.Roles)
}
