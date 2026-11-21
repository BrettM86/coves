package community

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
)

const rolesTestCommunityDID = "did:plc:gamingcommunity"

// rolesTestService resolves the identifiers the test names to one community
// DID, through either of the Service methods the handler may use.
type rolesTestService struct {
	*mockCommunityService
	identifiers map[string]string
}

func (s *rolesTestService) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	did, ok := s.identifiers[identifier]
	if !ok {
		return "", communities.ErrCommunityNotFound
	}
	return did, nil
}

func (s *rolesTestService) GetCommunity(ctx context.Context, identifier string) (*communities.Community, error) {
	did, err := s.ResolveCommunityIdentifier(ctx, identifier)
	if err != nil {
		return nil, err
	}
	return &communities.Community{DID: did, Handle: "gaming.coves.social", Name: "gaming"}, nil
}

// fakeRoleReader is an in-memory role index keyed by community DID and by
// subject DID.
type fakeRoleReader struct {
	byCommunity map[string][]communities.CommunityRole
	bySubject   map[string][]communities.CommunityRole
}

func (f fakeRoleReader) ListCommunityRoles(_ context.Context, communityDID string) ([]communities.CommunityRole, error) {
	return f.byCommunity[communityDID], nil
}

func (f fakeRoleReader) ListRolesBySubject(_ context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	return f.bySubject[subjectDID], nil
}

type roleViewJSON struct {
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

type getModeratorsJSON struct {
	Community  string         `json:"community"`
	Owner      *roleViewJSON  `json:"owner"`
	Moderators []roleViewJSON `json:"moderators"`
}

func TestGetModerators_ListsOwnerAndEveryModerator(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	rows := []communities.CommunityRole{{
		CommunityDID: rolesTestCommunityDID, SubjectDID: "did:plc:gamingowner", Role: "owner",
		RecordKey: "ownerkey", RecordCID: "bafyreiowner", RecordCreatedAt: createdAt,
	}}
	wantModerators := make([]roleViewJSON, 0, 150)
	for i := 1; i <= 150; i++ {
		subject := fmt.Sprintf("did:plc:gamingmoderator%03d", i)
		rows = append(rows, communities.CommunityRole{
			CommunityDID: rolesTestCommunityDID, SubjectDID: subject, Role: "moderator",
			RecordKey: fmt.Sprintf("moderatorkey%03d", i), RecordCID: "bafyreimoderator", RecordCreatedAt: createdAt,
		})
		wantModerators = append(wantModerators, roleViewJSON{Subject: subject, Role: "moderator", CreatedAt: "2026-11-01T10:00:00Z"})
	}
	service := &rolesTestService{
		mockCommunityService: &mockCommunityService{},
		identifiers: map[string]string{
			"gaming.coves.social":  rolesTestCommunityDID,
			"!gaming@coves.social": rolesTestCommunityDID,
		},
	}
	reader := fakeRoleReader{byCommunity: map[string][]communities.CommunityRole{rolesTestCommunityDID: rows}}
	handler := NewGetModeratorsHandler(service, reader)

	for _, identifier := range []string{"gaming.coves.social", "!gaming@coves.social"} {
		t.Run(identifier, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet,
				"/xrpc/social.coves.community.getModerators?community="+url.QueryEscape(identifier), nil)
			rec := httptest.NewRecorder()

			handler.HandleGetModerators(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var got getModeratorsJSON
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "response body %q is not a getModerators response", rec.Body.String())
			assert.Equal(t, rolesTestCommunityDID, got.Community)
			assert.Equal(t, &roleViewJSON{Subject: "did:plc:gamingowner", Role: "owner", CreatedAt: "2026-11-01T10:00:00Z"}, got.Owner)
			assert.ElementsMatch(t, wantModerators, got.Moderators)
		})
	}
}
