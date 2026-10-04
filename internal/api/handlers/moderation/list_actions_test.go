package moderation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/moderation"
	"Coves/internal/validation"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type actionListServiceFake struct {
	moderation.Service
	publicCalls []moderation.ListActionsParams
	adminCalls  []moderation.ListAdminActionsParams
	publicPage  *moderation.ActionPage
	adminPage   *moderation.AdminActionPage
	err         error
}

func (fake *actionListServiceFake) ListActions(_ context.Context, params moderation.ListActionsParams) (*moderation.ActionPage, error) {
	fake.publicCalls = append(fake.publicCalls, params)
	return fake.publicPage, fake.err
}

func (fake *actionListServiceFake) ListAdminActions(_ context.Context, params moderation.ListAdminActionsParams) (*moderation.AdminActionPage, error) {
	fake.adminCalls = append(fake.adminCalls, params)
	return fake.adminPage, fake.err
}

func requestActionList(fake *actionListServiceFake, admin bool, query, authorization string, authenticatedContext bool) *httptest.ResponseRecorder {
	path := "/xrpc/social.coves.moderation.listActions"
	if admin {
		path = "/xrpc/social.coves.moderation.listAdminActions"
	}
	request := httptest.NewRequest(http.MethodGet, path+query, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if authenticatedContext {
		request = request.WithContext(context.WithValue(request.Context(), middleware.UserDIDKey, "did:plc:admin"))
	}
	response := httptest.NewRecorder()
	if admin {
		NewListAdminActionsHandler(fake).HandleListAdminActions(response, request)
	} else {
		NewListActionsHandler(fake).HandleListActions(response, request)
	}
	return response
}

func assertActionListError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, response.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, code, body["error"])
	assert.NotEmpty(t, body["message"])
	assert.Len(t, body, 2)
}

func TestActionListHandlersDecodeAllQueryParameters(t *testing.T) {
	limit := 17
	query := "?limit=17&cursor=opaque-cursor&subject=at%3A%2F%2Fdid%3Aplc%3Aauthor%2Fsocial.coves.community.comment%2F3kabc" +
		"&collection=social.coves.community.comment&action=remove&origin=local&authority=did%3Aweb%3Ainstance.test" +
		"&actor=did%3Aplc%3Aadmin&community=sample%40instance.test&since=2026-09-01T00%3A00%3A00Z" +
		"&until=2026-10-01T00%3A00%3A00Z&actionId=3kexact"
	want := moderation.ListActionsParams{
		Limit: &limit, Cursor: "opaque-cursor", Subject: "at://did:plc:author/social.coves.community.comment/3kabc",
		Collection: "social.coves.community.comment", Action: "remove", Origin: "local",
		Authority: "did:web:instance.test", Actor: "did:plc:admin", Community: "sample@instance.test",
		Since: "2026-09-01T00:00:00Z", Until: "2026-10-01T00:00:00Z",
	}
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			fake := &actionListServiceFake{publicPage: &moderation.ActionPage{Actions: []moderation.ActionView{}}, adminPage: &moderation.AdminActionPage{Actions: []moderation.AdminActionView{}}}
			requestActionList(fake, admin, query, "", false)
			if admin {
				assert.Equal(t, []moderation.ListAdminActionsParams{{ListActionsParams: want, ActionID: "3kexact"}}, fake.adminCalls)
				assert.Empty(t, fake.publicCalls)
			} else {
				assert.Equal(t, []moderation.ListActionsParams{want}, fake.publicCalls, "public requests must ignore actionId")
				assert.Empty(t, fake.adminCalls)
			}

			fake = &actionListServiceFake{publicPage: &moderation.ActionPage{Actions: []moderation.ActionView{}}, adminPage: &moderation.AdminActionPage{Actions: []moderation.AdminActionView{}}}
			requestActionList(fake, admin, "", "", false)
			if admin {
				assert.Equal(t, []moderation.ListAdminActionsParams{{}}, fake.adminCalls, "absent limit is nil")
			} else {
				assert.Equal(t, []moderation.ListActionsParams{{}}, fake.publicCalls, "absent limit is nil")
			}
		})
	}
}

func TestActionListHandlersRejectPresentEmptyAndNonIntegerParameters(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, query := range []string{
			"?limit=", "?limit=abc", "?cursor=", "?subject=", "?collection=", "?action=", "?origin=",
			"?authority=", "?actor=", "?community=", "?since=", "?until=",
		} {
			t.Run(fmt.Sprintf("admin=%t/%s", admin, query), func(t *testing.T) {
				fake := &actionListServiceFake{}
				assertActionListError(t, requestActionList(fake, admin, query, "", false), http.StatusBadRequest, "InvalidRequest")
				assert.Empty(t, fake.publicCalls)
				assert.Empty(t, fake.adminCalls)
			})
		}
	}
	t.Run("admin actionId empty", func(t *testing.T) {
		fake := &actionListServiceFake{}
		assertActionListError(t, requestActionList(fake, true, "?actionId=", "", false), http.StatusBadRequest, "InvalidRequest")
		assert.Empty(t, fake.adminCalls)
	})
	// actionId is not a public filter, even if present and empty.
	public := &actionListServiceFake{publicPage: &moderation.ActionPage{Actions: []moderation.ActionView{}}}
	response := requestActionList(public, false, "?actionId=", "", false)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, []moderation.ListActionsParams{{}}, public.publicCalls)
}

func TestActionListHandlersMapWrappedServiceErrors(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, test := range []struct {
			name   string
			err    error
			status int
			code   string
		}{
			{"invalid request", moderation.ErrInvalidRequest, http.StatusBadRequest, "InvalidRequest"},
			{"invalid cursor", moderation.ErrInvalidCursor, http.StatusBadRequest, "InvalidCursor"},
			{"unavailable", moderation.ErrModerationUnavailable, http.StatusServiceUnavailable, "ModerationUnavailable"},
			{"unknown", errors.New("private database failure marker"), http.StatusInternalServerError, "InternalServerError"},
		} {
			t.Run(fmt.Sprintf("admin=%t/%s", admin, test.name), func(t *testing.T) {
				fake := &actionListServiceFake{err: fmt.Errorf("listing actions: %w", test.err)}
				response := requestActionList(fake, admin, "?action=remove", "", false)
				assertActionListError(t, response, test.status, test.code)
				assert.NotContains(t, response.Body.String(), test.err.Error(), "service error details must not be disclosed")
				if admin {
					assert.Equal(t, []moderation.ListAdminActionsParams{{ListActionsParams: moderation.ListActionsParams{Action: "remove"}}}, fake.adminCalls)
				} else {
					assert.Equal(t, []moderation.ListActionsParams{{Action: "remove"}}, fake.publicCalls)
				}
			})
		}
	}
}

func TestActionListHandlersSerializeExactLexiconOutput(t *testing.T) {
	action := moderation.Action{
		ID: "3kabc", ActorDID: "did:plc:admin", AuthorityDID: "did:web:instance.test",
		ScopeKind: moderation.ScopeInstance, SubjectURI: "at://did:plc:author/social.coves.community.comment/3kabc",
		ObservedCID: "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm",
		Action:      moderation.ActionRemove, Origin: moderation.OriginLocal,
		Reason: "social.coves.moderation.defs#reasonSpam", PrivateNote: "admin-only note",
		CreatedAt: time.Date(2026, time.September, 28, 12, 30, 0, 0, time.UTC),
	}
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
	for _, admin := range []bool{false, true} {
		for _, withItem := range []bool{false, true} {
			t.Run(fmt.Sprintf("admin=%t/item=%t", admin, withItem), func(t *testing.T) {
				fake := &actionListServiceFake{
					publicPage: &moderation.ActionPage{Actions: []moderation.ActionView{}},
					adminPage:  &moderation.AdminActionPage{Actions: []moderation.AdminActionView{}},
				}
				lexiconType := "social.coves.moderation.listActions#output"
				var page any = fake.publicPage
				if admin {
					lexiconType = "social.coves.moderation.listAdminActions#output"
					page = fake.adminPage
				}
				if withItem {
					fake.publicPage.Actions = []moderation.ActionView{moderation.NewActionView(action)}
					fake.publicPage.Cursor = "next-page"
					fake.adminPage.Actions = []moderation.AdminActionView{moderation.NewAdminActionView(action)}
					fake.adminPage.Cursor = "next-page"
				}
				response := requestActionList(fake, admin, "", "", false)
				require.Equal(t, http.StatusOK, response.Code)
				require.NotEmpty(t, response.Body.Bytes(), "handler must write the action page")
				assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
				want, err := json.Marshal(page)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), response.Body.String(), "response must be exactly the service page")
				if !withItem {
					assert.JSONEq(t, `{"actions":[]}`, response.Body.String())
				}
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				for key := range body {
					assert.Contains(t, []string{"actions", "cursor"}, key)
				}
				decoded, err := atdata.UnmarshalJSON(response.Body.Bytes())
				require.NoError(t, err)
				assert.NoError(t, validation.ValidateData(catalog, decoded, lexiconType, 0))
			})
		}
	}
}

func TestPublicActionListIgnoresAuthentication(t *testing.T) {
	action := moderation.Action{
		ID: "3kabc", ActorDID: "did:plc:admin", AuthorityDID: "did:web:instance.test",
		ScopeKind: moderation.ScopeInstance, Action: moderation.ActionRemove, Origin: moderation.OriginLocal,
		CreatedAt: time.Date(2026, time.September, 28, 12, 30, 0, 0, time.UTC),
	}
	page := &moderation.ActionPage{Actions: []moderation.ActionView{moderation.NewActionView(action)}}
	fake := &actionListServiceFake{publicPage: page}
	public := requestActionList(fake, false, "?limit=1", "", false)
	authenticated := requestActionList(fake, false, "?limit=1", "Bearer arbitrary-token", true)
	assert.Equal(t, http.StatusOK, public.Code)
	assert.Equal(t, public.Code, authenticated.Code)
	assert.Equal(t, public.Body.String(), authenticated.Body.String())
	assert.Equal(t, public.Header().Get("Content-Type"), authenticated.Header().Get("Content-Type"))
	require.Len(t, fake.publicCalls, 2)
	assert.Equal(t, fake.publicCalls[0], fake.publicCalls[1])
}

func captureActionListLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logged
}

func TestActionListHandlersRejectRepeatedParameters(t *testing.T) {
	values := map[string]string{
		"limit": "5", "cursor": "opaque-cursor", "subject": "at%3A%2F%2Fdid%3Aplc%3Aauthor%2Fsocial.coves.community.comment%2F3kabc",
		"collection": "social.coves.community.comment", "action": "remove", "origin": "local",
		"authority": "did%3Aweb%3Ainstance.test", "actor": "did%3Aplc%3Aadmin", "community": "sample%40instance.test",
		"since": "2026-09-01T00%3A00%3A00Z", "until": "2026-10-01T00%3A00%3A00Z", "actionId": "3kexact",
	}
	for name, value := range values {
		for _, admin := range []bool{false, true} {
			if name == "actionId" && !admin {
				continue
			}
			for _, query := range []string{
				fmt.Sprintf("?%s=%s&%s=%s", name, value, name, value),
				fmt.Sprintf("?%s=%s&%s=other", name, value, name),
			} {
				t.Run(fmt.Sprintf("admin=%t/%s", admin, query), func(t *testing.T) {
					fake := &actionListServiceFake{}
					assertActionListError(t, requestActionList(fake, admin, query, "", false), http.StatusBadRequest, "InvalidRequest")
					assert.Empty(t, fake.publicCalls)
					assert.Empty(t, fake.adminCalls)
				})
			}
		}
	}
	// actionId is not a public parameter, so the public endpoint ignores it
	// however often it appears.
	public := &actionListServiceFake{publicPage: &moderation.ActionPage{Actions: []moderation.ActionView{}}}
	response := requestActionList(public, false, "?actionId=a&actionId=b", "", false)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, []moderation.ListActionsParams{{}}, public.publicCalls)
}

func TestActionListHandlersNilPageIsLoggedWithACause(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			logged := captureActionListLog(t)
			response := requestActionList(&actionListServiceFake{}, admin, "", "", false)
			assertActionListError(t, response, http.StatusInternalServerError, "InternalServerError")
			assert.Contains(t, logged.String(), "level=ERROR")
			assert.Contains(t, logged.String(), "moderation service returned no page")
			assert.NotContains(t, logged.String(), "error=<nil>")
		})
	}
}

func TestActionListHandlersCancellationIsNotAServerError(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			logged := captureActionListLog(t)
			fake := &actionListServiceFake{err: fmt.Errorf("list public actions: %w", context.Canceled)}
			response := requestActionList(fake, admin, "", "", false)
			assertActionListError(t, response, http.StatusBadRequest, "RequestCanceled")
			assert.NotContains(t, logged.String(), "level=ERROR", "a client that went away is not a server failure")
		})
	}
}

type actionListStoreFake struct {
	moderation.Store
	err error
}

func (store actionListStoreFake) ListActions(context.Context, moderation.ActionListQuery) ([]moderation.Action, error) {
	return nil, store.err
}

type actionListHandleResolverFake struct {
	err error
}

func (resolver actionListHandleResolverFake) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", resolver.err
}

// Resolver outages are someone else's service and log at WARN; a store
// failure is ours and logs at ERROR. Both answer 503 ModerationUnavailable.
func TestActionListHandlersLogUnavailabilityByOrigin(t *testing.T) {
	for _, test := range []struct {
		name     string
		query    string
		resolver moderation.HandleResolver
		store    error
		status   int
		code     string
		level    string
	}{
		{"handle resolver outage", "?actor=actor.example",
			actionListHandleResolverFake{err: &identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "down", Err: errors.New("dial tcp: refused")}},
			nil, http.StatusServiceUnavailable, "ModerationUnavailable", "WARN"},
		{"handle resolver not configured", "?authority=authority.example", nil, nil,
			http.StatusServiceUnavailable, "ModerationUnavailable", "WARN"},
		{"store outage", "?action=remove", nil, errors.New("pq: connection refused"),
			http.StatusServiceUnavailable, "ModerationUnavailable", "ERROR"},
		{"store canceled", "?action=remove", nil, fmt.Errorf("pq: %w", context.Canceled),
			http.StatusBadRequest, "RequestCanceled", ""},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				logged := captureActionListLog(t)
				config := moderation.Config{InstanceDID: "did:web:instance.test", CursorSecret: "handler-cursor-secret", HandleResolver: test.resolver}
				service := moderation.NewService(nil, actionListStoreFake{err: test.store}, config)
				path := "/xrpc/social.coves.moderation.listActions"
				if admin {
					path = "/xrpc/social.coves.moderation.listAdminActions"
				}
				request := httptest.NewRequest(http.MethodGet, path+test.query, nil)
				response := httptest.NewRecorder()
				if admin {
					NewListAdminActionsHandler(service).HandleListAdminActions(response, request)
				} else {
					NewListActionsHandler(service).HandleListActions(response, request)
				}
				assertActionListError(t, response, test.status, test.code)
				for _, level := range []string{"WARN", "ERROR"} {
					if level == test.level {
						assert.Contains(t, logged.String(), "level="+level)
					} else {
						assert.NotContains(t, logged.String(), "level="+level)
					}
				}
				if test.store != nil && test.level != "" {
					assert.Contains(t, logged.String(), test.store.Error(), "the store cause must reach the server log")
				}
				assert.False(t, strings.Contains(response.Body.String(), "pq:"), "the cause must not reach the response body")
			})
		}
	}
}
