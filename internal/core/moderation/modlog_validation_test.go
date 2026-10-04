package moderation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type modlogValidationCommunityResolver struct{}

func (modlogValidationCommunityResolver) ResolveCommunityIdentifier(context.Context, string) (string, error) {
	return "did:plc:resolvedcommunity", nil
}

func newModlogValidationService() (moderation.Service, *inMemoryModerationStore) {
	store := newInMemoryModerationStore(time.Now())
	service := moderation.NewService(&fakeSubjectReader{}, store, moderation.Config{
		InstanceDID:       "did:web:test.coves.social",
		CursorSecret:      "test-secret",
		CommunityResolver: modlogValidationCommunityResolver{},
	})
	return service, store
}

func modlogListForValidation(t *testing.T, service moderation.Service, admin bool, params moderation.ListActionsParams, actionID string) error {
	t.Helper()
	if admin {
		_, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{
			ListActionsParams: params,
			ActionID:          actionID,
		})
		return err
	}
	_, err := service.ListActions(t.Context(), params)
	return err
}

func TestModlogListRejectsInvalidParametersBeforeStore(t *testing.T) {
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
	}{
		{"limit zero", moderation.ListActionsParams{Limit: modlogLimit(0)}},
		{"limit above maximum", moderation.ListActionsParams{Limit: modlogLimit(101)}},
		{"negative limit", moderation.ListActionsParams{Limit: modlogLimit(-1)}},
		{"cursor above 2048 bytes", moderation.ListActionsParams{Cursor: strings.Repeat("a", 2049)}},
		{"community above 320 bytes", moderation.ListActionsParams{Community: strings.Repeat("a", 321)}},
		{"invalid subject", moderation.ListActionsParams{Subject: "not-a-uri"}},
		{"invalid collection", moderation.ListActionsParams{Collection: "not an nsid"}},
		{"action above 64 bytes", moderation.ListActionsParams{Action: strings.Repeat("a", 65)}},
		{"origin above 64 bytes", moderation.ListActionsParams{Origin: strings.Repeat("a", 65)}},
		{"invalid authority", moderation.ListActionsParams{Authority: "not a handle!"}},
		{"invalid actor", moderation.ListActionsParams{Actor: "not a handle!"}},
		{"invalid since", moderation.ListActionsParams{Since: "yesterday"}},
		{"invalid until", moderation.ListActionsParams{Until: "yesterday"}},
		{"invalid UTF-8 cursor", moderation.ListActionsParams{Cursor: "\xff"}},
		{"invalid UTF-8 action", moderation.ListActionsParams{Action: "\xff"}},
		{"invalid UTF-8 origin", moderation.ListActionsParams{Origin: "\xff"}},
		{"invalid UTF-8 community", moderation.ListActionsParams{Community: "\xff"}},
	} {
		for _, endpoint := range []struct {
			name  string
			admin bool
		}{{"public", false}, {"admin", true}} {
			t.Run(endpoint.name+"/"+test.name, func(t *testing.T) {
				service, store := newModlogValidationService()
				err := modlogListForValidation(t, service, endpoint.admin, test.params, "")
				require.ErrorIs(t, err, moderation.ErrInvalidRequest)
				require.Empty(t, store.listQueries, "invalid parameters must never reach the store")
			})
		}
	}
	for _, test := range []struct {
		name     string
		actionID string
	}{
		{"actionId above 128 bytes", strings.Repeat("a", 129)},
		{"invalid UTF-8 actionId", "\xff"},
	} {
		t.Run("admin/"+test.name, func(t *testing.T) {
			service, store := newModlogValidationService()
			err := modlogListForValidation(t, service, true, moderation.ListActionsParams{}, test.actionID)
			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			require.Empty(t, store.listQueries, "invalid actionId must never reach the store")
		})
	}
}

func modlogLimit(value int) *int { return &value }

func TestModlogListAcceptsBoundariesAndRequestsOneExtraRow(t *testing.T) {
	for _, test := range []struct {
		name       string
		params     moderation.ListActionsParams
		actionID   string
		storeLimit int
	}{
		{"default limit and absent cursor", moderation.ListActionsParams{}, "", 51},
		{"limit one", moderation.ListActionsParams{Limit: modlogLimit(1)}, "", 2},
		{"limit ten", moderation.ListActionsParams{Limit: modlogLimit(10)}, "", 11},
		{"limit one hundred", moderation.ListActionsParams{Limit: modlogLimit(100)}, "", 101},
		{"community exactly 320 bytes", moderation.ListActionsParams{Community: strings.Repeat("a", 320)}, "", 51},
		{"action exactly 64 bytes", moderation.ListActionsParams{Action: strings.Repeat("a", 64)}, "", 51},
		{"origin exactly 64 bytes", moderation.ListActionsParams{Origin: strings.Repeat("a", 64)}, "", 51},
		{"actionId exactly 128 bytes", moderation.ListActionsParams{}, strings.Repeat("a", 128), 51},
	} {
		for _, endpoint := range []struct {
			name  string
			admin bool
		}{{"public", false}, {"admin", true}} {
			if test.actionID != "" && !endpoint.admin {
				continue
			}
			t.Run(endpoint.name+"/"+test.name, func(t *testing.T) {
				service, store := newModlogValidationService()
				err := modlogListForValidation(t, service, endpoint.admin, test.params, test.actionID)
				require.NoError(t, err)
				require.Len(t, store.listQueries, 1, "accepted filters must reach the store exactly once")
				query := store.listQueries[0]
				require.Equal(t, test.storeLimit, query.Limit, "the store fetches one extra row for pagination")
				if test.params.Community != "" {
					require.Equal(t, "did:plc:resolvedcommunity", query.CommunityDID)
				}
				if test.params.Action != "" {
					require.Equal(t, test.params.Action, query.Action)
				}
				if test.params.Origin != "" {
					require.Equal(t, test.params.Origin, query.Origin)
				}
				if test.actionID != "" {
					require.Equal(t, test.actionID, query.ActionID)
				}
			})
		}
	}
}

// Control characters never name a real filter target, and a NUL reaching
// Postgres is an encoding error the caller could use to force a 503.
func TestModlogListRejectsControlCharactersBeforeCursorResolutionAndStore(t *testing.T) {
	for _, control := range []string{"\x00", "\x1f", "\x7f", "\u0085"} {
		for _, test := range []struct {
			name     string
			params   moderation.ListActionsParams
			actionID string
		}{
			{"action", moderation.ListActionsParams{Action: "remove" + control}, ""},
			{"origin", moderation.ListActionsParams{Origin: "lo" + control + "cal"}, ""},
			{"community", moderation.ListActionsParams{Community: "name" + control + "@coves.social"}, ""},
			{"actor", moderation.ListActionsParams{Actor: "actor.example" + control}, ""},
			{"authority", moderation.ListActionsParams{Authority: control + "authority.example"}, ""},
			{"actionId", moderation.ListActionsParams{}, "3mwmy" + control},
		} {
			for _, admin := range []bool{false, true} {
				if test.actionID != "" && !admin {
					continue
				}
				t.Run(fmt.Sprintf("%s/%q/admin=%t", test.name, control, admin), func(t *testing.T) {
					store := newInMemoryModerationStore(time.Now())
					community := &modlogFilterCommunityResolver{did: "did:plc:resolvedcommunity"}
					handles := &modlogFilterHandleResolver{dids: map[string]string{}}
					params := test.params
					params.Cursor = "not?base64url" // Parameter validation precedes the cursor check.
					_, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, params, test.actionID)
					require.ErrorIs(t, err, moderation.ErrInvalidRequest)
					assert.Empty(t, community.calls, "a control character must never reach the community resolver")
					assert.Empty(t, handles.calls, "a control character must never reach the handle resolver")
					assert.Empty(t, store.listQueries, "a control character must never reach the store")
				})
			}
		}
	}
}

// since is inclusive and until exclusive, so since >= until (after both
// round up to whole microseconds) selects nothing and is a caller mistake.
func TestModlogListRejectsEmptyTimeWindow(t *testing.T) {
	for _, test := range []struct {
		name   string
		since  string
		until  string
		reject bool
	}{
		{"equal", "2026-09-28T12:00:00.123456Z", "2026-09-28T12:00:00.123456Z", true},
		{"since after until", "2026-09-29T12:00:00Z", "2026-09-28T12:00:00Z", true},
		{"equal across time zones", "2026-09-28T14:00:00+02:00", "2026-09-28T12:00:00Z", true},
		{"since rounds up onto until", "2026-09-28T12:00:00.1234561Z", "2026-09-28T12:00:00.123457Z", true},
		{"until rounds up past since", "2026-09-28T12:00:00.123456Z", "2026-09-28T12:00:00.1234561Z", false},
		{"one microsecond window", "2026-09-28T12:00:00.123456Z", "2026-09-28T12:00:00.123457Z", false},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				service, store := newModlogValidationService()
				err := modlogListForValidation(t, service, admin, moderation.ListActionsParams{Since: test.since, Until: test.until}, "")
				if test.reject {
					require.ErrorIs(t, err, moderation.ErrInvalidRequest)
					assert.Empty(t, store.listQueries)
					return
				}
				require.NoError(t, err)
				assert.Len(t, store.listQueries, 1)
			})
		}
	}
}

// Stored subjects always carry a DID authority, so a handle authority can
// never match; GetSubjectState rejects the same input.
func TestModlogListRejectsHandleAuthoritySubject(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			service, store := newModlogValidationService()
			err := modlogListForValidation(t, service, admin, moderation.ListActionsParams{
				Subject: "at://alice.test/social.coves.community.comment/3mwmycomment",
			}, "")
			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			assert.Empty(t, store.listQueries)

			err = modlogListForValidation(t, service, admin, moderation.ListActionsParams{
				Subject: "at://did:plc:alice/social.coves.community.comment/3mwmycomment",
			}, "")
			require.NoError(t, err)
			assert.Len(t, store.listQueries, 1)
		})
	}
}
