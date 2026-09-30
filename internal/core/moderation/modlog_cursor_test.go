package moderation_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newModlogCursorService(store *inMemoryModerationStore, config moderation.Config) moderation.Service {
	return moderation.NewService(&fakeSubjectReader{}, store, config)
}

func modlogCursorConfig() moderation.Config {
	return moderation.Config{InstanceDID: "did:web:instance.example", CursorSecret: "secret-one"}
}

func modlogCursorRows() []moderation.Action {
	createdAt := time.Date(2026, 9, 28, 12, 0, 0, 123456000, time.UTC)
	rows := make([]moderation.Action, 3)
	for index, id := range []string{"3mwmynewest", "3mwmymiddle", "3mwmyoldest"} {
		rows[index] = modlogTestAction()
		rows[index].ID = id
		rows[index].CreatedAt = createdAt.Add(-time.Duration(index) * time.Microsecond)
	}
	return rows
}

func TestModlogCursorPagesAndDeterminism(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	rows := modlogCursorRows()
	store.listRows = rows
	service := newModlogCursorService(store, modlogCursorConfig())
	params := moderation.ListActionsParams{Limit: modlogLimit(2), Action: moderation.ActionRemove}

	first, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Len(t, first.Actions, 2)
	assert.Equal(t, []string{rows[0].ID, rows[1].ID}, []string{first.Actions[0].Ref.ActionID, first.Actions[1].Ref.ActionID})
	require.NotEmpty(t, first.Cursor, "a limit+1 result must issue a cursor")
	assert.LessOrEqual(t, len(first.Cursor), 2048)
	assert.NotContains(t, first.Cursor, "=")
	assert.NotContains(t, first.Cursor, "+")
	assert.NotContains(t, first.Cursor, "/")
	_, err = base64.RawURLEncoding.DecodeString(first.Cursor)
	require.NoError(t, err, "cursor must be unpadded base64url")
	require.Len(t, store.listQueries, 1)
	assert.Equal(t, 3, store.listQueries[0].Limit)
	assert.Nil(t, store.listQueries[0].Before)

	repeated, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	assert.Equal(t, first.Cursor, repeated.Cursor, "identical requests and rows must yield identical cursor bytes")

	store.listRows = rows[2:]
	params.Cursor = first.Cursor
	params.Limit = modlogLimit(1) // Page size is not part of the cursor's filter binding.
	second, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, second.Actions, 1)
	assert.Equal(t, rows[2].ID, second.Actions[0].Ref.ActionID)
	assert.Empty(t, second.Cursor, "at most limit rows means no next page")
	require.Len(t, store.listQueries, 3)
	assert.Equal(t, 2, store.listQueries[2].Limit)
	assert.Equal(t, &moderation.ActionKey{CreatedAt: rows[1].CreatedAt, ID: rows[1].ID}, store.listQueries[2].Before)

	store.listRows = rows[:2]
	params.Cursor = ""
	params.Limit = modlogLimit(2)
	last, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	assert.Empty(t, last.Cursor, "an exact-size first page has no cursor")
}

func TestModlogCursorRejectsChangedContextBeforeStore(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	store.listRows = modlogCursorRows()
	service := newModlogCursorService(store, modlogCursorConfig())
	params := moderation.ListActionsParams{Limit: modlogLimit(2), Action: moderation.ActionRemove,
		Subject: modlogTestAction().SubjectURI, Since: "2026-09-28T12:00:00.123456Z"}
	public, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.NotEmpty(t, public.Cursor)
	admin, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{ListActionsParams: params, ActionID: "a"})
	require.NoError(t, err)
	require.NotEmpty(t, admin.Cursor)

	tampered := public.Cursor
	if tampered[0] == 'A' {
		tampered = "B" + tampered[1:]
	} else {
		tampered = "A" + tampered[1:]
	}
	for _, test := range []struct {
		name   string
		change func(*moderation.ListActionsParams)
	}{
		{"action", func(p *moderation.ListActionsParams) { p.Action = moderation.ActionRestore }},
		{"subject removed", func(p *moderation.ListActionsParams) { p.Subject = "" }},
		{"subject changed", func(p *moderation.ListActionsParams) { p.Subject += "2" }},
		{"since", func(p *moderation.ListActionsParams) { p.Since = "2026-09-28T12:00:00.123457Z" }},
		{"until added", func(p *moderation.ListActionsParams) { p.Until = "2026-09-29T12:00:00Z" }},
		{"collection added", func(p *moderation.ListActionsParams) { p.Collection = moderation.CommentCollection }},
		{"origin added", func(p *moderation.ListActionsParams) { p.Origin = moderation.OriginLocal }},
		{"authority added", func(p *moderation.ListActionsParams) { p.Authority = "did:plc:operator" }},
		{"actor added", func(p *moderation.ListActionsParams) { p.Actor = "did:plc:operator" }},
		{"community added", func(p *moderation.ListActionsParams) { p.Community = "did:plc:community" }},
		{"one character changed", func(p *moderation.ListActionsParams) { p.Cursor = tampered }},
		{"non-base64url cursor", func(p *moderation.ListActionsParams) { p.Cursor = "not?base64url" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := params
			replay.Cursor = public.Cursor
			test.change(&replay)
			before := len(store.listQueries)
			_, err := service.ListActions(t.Context(), replay)
			require.ErrorIs(t, err, moderation.ErrInvalidCursor)
			assert.Len(t, store.listQueries, before, "invalid cursor must not query the store")
		})
	}
	for _, test := range []struct {
		name   string
		replay func() error
	}{
		{"public cursor at admin endpoint", func() error {
			_, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{ListActionsParams: moderation.ListActionsParams{Action: params.Action, Subject: params.Subject, Since: params.Since, Cursor: public.Cursor}})
			return err
		}},
		{"admin cursor at public endpoint", func() error {
			_, err := service.ListActions(t.Context(), moderation.ListActionsParams{Action: params.Action, Subject: params.Subject, Since: params.Since, Cursor: admin.Cursor})
			return err
		}},
		{"admin action ID changed", func() error {
			_, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{ListActionsParams: moderation.ListActionsParams{Action: params.Action, Subject: params.Subject, Since: params.Since, Cursor: admin.Cursor}, ActionID: "b"})
			return err
		}},
		{"different signing secret", func() error {
			config := modlogCursorConfig()
			config.CursorSecret = "secret-two"
			_, err := newModlogCursorService(store, config).ListActions(t.Context(), moderation.ListActionsParams{Action: params.Action, Subject: params.Subject, Since: params.Since, Cursor: public.Cursor})
			return err
		}},
		{"different instance DID", func() error {
			config := modlogCursorConfig()
			config.InstanceDID = "did:web:other.example"
			_, err := newModlogCursorService(store, config).ListActions(t.Context(), moderation.ListActionsParams{Action: params.Action, Subject: params.Subject, Since: params.Since, Cursor: public.Cursor})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(store.listQueries)
			require.ErrorIs(t, test.replay(), moderation.ErrInvalidCursor)
			assert.Len(t, store.listQueries, before, "invalid cursor must not query the store")
		})
	}
}

func TestModlogCursorStaysShortWithMaximumAllowedDIDs(t *testing.T) {
	// Authority and actor accept 2048 bytes; community's distinct parameter
	// bound is 320 bytes. All three lengths are independently maximal.
	longDID := "did:plc:" + strings.Repeat("a", 2040)
	communityDID := "did:plc:" + strings.Repeat("b", 312)
	require.Len(t, longDID, 2048)
	require.Len(t, communityDID, 320)
	_, err := syntax.ParseDID(longDID)
	require.NoError(t, err, "long DID must be syntactically valid")
	_, err = syntax.ParseDID(communityDID)
	require.NoError(t, err, "community DID must be syntactically valid")
	store := newInMemoryModerationStore(time.Now())
	store.listRows = modlogCursorRows()[:2]
	service := newModlogCursorService(store, modlogCursorConfig())
	params := moderation.ListActionsParams{Limit: modlogLimit(1), Authority: longDID, Actor: longDID, Community: communityDID}
	page, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.NotEmpty(t, page.Cursor)
	assert.LessOrEqual(t, len(page.Cursor), 2048, "cursor must contain a fixed-size digest rather than full resolved DIDs")
	params.Cursor = page.Cursor
	before := len(store.listQueries)
	_, err = service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, store.listQueries, before+1)
	assert.Equal(t, longDID, store.listQueries[before].AuthorityDID)
	assert.Equal(t, longDID, store.listQueries[before].ActorDID)
	assert.Equal(t, communityDID, store.listQueries[before].CommunityDID)
}

type modlogChangingHandleResolver struct {
	did   string
	calls []string
}

func (resolver *modlogChangingHandleResolver) ResolveHandle(_ context.Context, handle string) (string, string, error) {
	resolver.calls = append(resolver.calls, handle)
	return resolver.did, "", nil
}

func TestModlogCursorBindsReResolvedHandleDID(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	store.listRows = modlogCursorRows()[:2]
	resolver := &modlogChangingHandleResolver{did: "did:plc:first"}
	config := modlogCursorConfig()
	config.HandleResolver = resolver
	service := newModlogCursorService(store, config)
	params := moderation.ListActionsParams{Limit: modlogLimit(1), Actor: "operator.example"}
	page, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.NotEmpty(t, page.Cursor)
	require.Equal(t, []string{params.Actor}, resolver.calls)
	require.Len(t, store.listQueries, 1)
	assert.Equal(t, "did:plc:first", store.listQueries[0].ActorDID)

	params.Cursor = page.Cursor
	_, err = service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, store.listQueries, 2)
	assert.Equal(t, "did:plc:first", store.listQueries[1].ActorDID)
	assert.Equal(t, []string{params.Actor, params.Actor}, resolver.calls, "replay re-resolves and verifies the same target")

	resolver.did = "did:plc:second"
	before := len(store.listQueries)
	_, err = service.ListActions(t.Context(), params)
	require.ErrorIs(t, err, moderation.ErrInvalidCursor)
	assert.Len(t, store.listQueries, before, "a remapped handle must never query the store as its new DID")
}

func TestModlogCursorBindsReResolvedAuthorityDID(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	store.listRows = modlogCursorRows()[:2]
	resolver := &modlogChangingHandleResolver{did: "did:plc:first"}
	config := modlogCursorConfig()
	config.HandleResolver = resolver
	service := newModlogCursorService(store, config)
	params := moderation.ListActionsParams{Limit: modlogLimit(1), Authority: "authority.example"}
	page, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.NotEmpty(t, page.Cursor)
	require.Len(t, store.listQueries, 1)
	assert.Equal(t, "did:plc:first", store.listQueries[0].AuthorityDID)

	params.Cursor = page.Cursor
	_, err = service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, store.listQueries, 2)

	resolver.did = "did:plc:second"
	before := len(store.listQueries)
	_, err = service.ListActions(t.Context(), params)
	require.ErrorIs(t, err, moderation.ErrInvalidCursor)
	assert.Len(t, store.listQueries, before, "a remapped authority handle must never query the store as its new DID")
}

func TestModlogCursorBindsReResolvedCommunityDID(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	store.listRows = modlogCursorRows()[:2]
	resolver := &modlogFilterCommunityResolver{did: "did:plc:firstcommunity"}
	config := modlogCursorConfig()
	config.CommunityResolver = resolver
	service := newModlogCursorService(store, config)
	params := moderation.ListActionsParams{Limit: modlogLimit(1), Community: "name@coves.social"}
	page, err := service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.NotEmpty(t, page.Cursor)
	require.Len(t, store.listQueries, 1)
	assert.Equal(t, "did:plc:firstcommunity", store.listQueries[0].CommunityDID)

	params.Cursor = page.Cursor
	_, err = service.ListActions(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, store.listQueries, 2)
	assert.Equal(t, []string{params.Community, params.Community}, resolver.calls, "replay re-resolves and verifies the same community")

	resolver.did = "did:plc:secondcommunity"
	before := len(store.listQueries)
	_, err = service.ListActions(t.Context(), params)
	require.ErrorIs(t, err, moderation.ErrInvalidCursor)
	assert.Len(t, store.listQueries, before, "a re-pointed community name must never query the store as its new DID")
}

func modlogPageCursor(t *testing.T, page any) string {
	t.Helper()
	switch typed := page.(type) {
	case *moderation.ActionPage:
		return typed.Cursor
	case *moderation.AdminActionPage:
		return typed.Cursor
	default:
		t.Fatalf("unexpected page type %T", page)
		return ""
	}
}

// A continuation page whose target is in the not-found class cannot be an
// empty last page: that would end pagination silently.
func TestModlogCursorContinuationWithUnknownTargetIsInvalidCursor(t *testing.T) {
	notFoundHandle := &identity.ErrNotFound{Identifier: "missing.example"}
	notFoundCommunity := fmt.Errorf("resolve: %w", communities.ErrCommunityNotFound)
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
		change func(*moderation.ListActionsParams, *modlogFilterHandleResolver, *modlogFilterCommunityResolver)
	}{
		{"actor changed to unknown", moderation.ListActionsParams{Actor: "actor.example"},
			func(params *moderation.ListActionsParams, _ *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				params.Actor = "missing.example"
			}},
		{"authority changed to unknown", moderation.ListActionsParams{Authority: "authority.example"},
			func(params *moderation.ListActionsParams, _ *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				params.Authority = "missing.example"
			}},
		{"community changed to unknown", moderation.ListActionsParams{Community: "name@coves.social"},
			func(params *moderation.ListActionsParams, _ *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				params.Community = "missing@coves.social"
			}},
		{"unknown actor added", moderation.ListActionsParams{},
			func(params *moderation.ListActionsParams, _ *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				params.Actor = "missing.example"
			}},
		{"actor handle stops resolving", moderation.ListActionsParams{Actor: "actor.example"},
			func(_ *moderation.ListActionsParams, handles *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				handles.failures["actor.example"] = notFoundHandle
			}},
		{"authority handle stops resolving", moderation.ListActionsParams{Authority: "authority.example"},
			func(_ *moderation.ListActionsParams, handles *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				handles.failures["authority.example"] = notFoundHandle
			}},
		{"community stops resolving", moderation.ListActionsParams{Community: "name@coves.social"},
			func(_ *moderation.ListActionsParams, _ *modlogFilterHandleResolver, community *modlogFilterCommunityResolver) {
				community.failures["name@coves.social"] = notFoundCommunity
			}},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				store.listRows = modlogCursorRows()[:2]
				handles := &modlogFilterHandleResolver{
					dids:     map[string]string{"actor.example": "did:plc:actor", "authority.example": "did:plc:authority"},
					failures: map[string]error{"missing.example": notFoundHandle},
				}
				community := &modlogFilterCommunityResolver{did: "did:plc:community",
					failures: map[string]error{"missing@coves.social": notFoundCommunity}}
				service := modlogFilterService(store, community, handles)
				params := test.params
				params.Limit = modlogLimit(1)
				page, err := modlogFilterList(t, service, admin, params, "")
				require.NoError(t, err)
				cursor := modlogPageCursor(t, page)
				require.NotEmpty(t, cursor)

				params.Cursor = cursor
				test.change(&params, handles, community)
				before := len(store.listQueries)
				_, err = modlogFilterList(t, service, admin, params, "")
				require.ErrorIs(t, err, moderation.ErrInvalidCursor)
				assert.Len(t, store.listQueries, before, "an unknown continuation target must never query the store")

				params.Cursor = ""
				page, err = modlogFilterList(t, service, admin, params, "")
				require.NoError(t, err, "without a cursor the unknown target stays an indistinguishable empty page")
				assert.Len(t, store.listQueries, before)
			})
		}
	}
}

func TestModlogCursorContinuationResolverOutageStaysUnavailable(t *testing.T) {
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
		fail   func(*modlogFilterHandleResolver, *modlogFilterCommunityResolver)
	}{
		{"handle outage", moderation.ListActionsParams{Actor: "actor.example"},
			func(handles *modlogFilterHandleResolver, _ *modlogFilterCommunityResolver) {
				handles.err = &identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "down", Err: errors.New("dial tcp: refused")}
				handles.dids = nil
			}},
		{"community outage", moderation.ListActionsParams{Community: "name@coves.social"},
			func(_ *modlogFilterHandleResolver, community *modlogFilterCommunityResolver) {
				community.did, community.err = "", errors.New("community resolver down")
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			store.listRows = modlogCursorRows()[:2]
			handles := &modlogFilterHandleResolver{dids: map[string]string{"actor.example": "did:plc:actor"}}
			community := &modlogFilterCommunityResolver{did: "did:plc:community"}
			service := modlogFilterService(store, community, handles)
			params := test.params
			params.Limit = modlogLimit(1)
			page, err := service.ListActions(t.Context(), params)
			require.NoError(t, err)
			require.NotEmpty(t, page.Cursor)

			test.fail(handles, community)
			params.Cursor = page.Cursor
			before := len(store.listQueries)
			_, err = service.ListActions(t.Context(), params)
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.NotErrorIs(t, err, moderation.ErrInvalidCursor)
			assert.Len(t, store.listQueries, before)
		})
	}
}

// The filter digest is only a MAC input, recomputed on verification: the
// cursor carries version, context, createdAt, the action ID and the MAC.
func TestModlogCursorCarriesNoFilterDigest(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	rows := modlogCursorRows()
	store.listRows = rows
	service := newModlogCursorService(store, modlogCursorConfig())
	page, err := service.ListActions(t.Context(), moderation.ListActionsParams{Limit: modlogLimit(2), Action: moderation.ActionRemove})
	require.NoError(t, err)
	decoded, err := base64.RawURLEncoding.DecodeString(page.Cursor)
	require.NoError(t, err)
	const versionContextAndCreatedAt, mac = 1 + 1 + 8, 32
	assert.Len(t, decoded, versionContextAndCreatedAt+len(rows[1].ID)+mac)
	assert.Equal(t, rows[1].ID, string(decoded[versionContextAndCreatedAt:len(decoded)-mac]))
}

func TestModlogListRefusesEmptyCursorSecret(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			store.listRows = modlogCursorRows()
			config := modlogCursorConfig()
			config.CursorSecret = ""
			_, err := modlogFilterList(t, newModlogCursorService(store, config), admin, moderation.ListActionsParams{Limit: modlogLimit(1)}, "")
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.NotErrorIs(t, err, moderation.ErrResolverUnavailable)
			assert.Empty(t, store.listQueries, "an unsigned cursor must never be issued")
		})
	}
}

func TestModlogListRoundsTimeBoundsUpToMicroseconds(t *testing.T) {
	base := time.Date(2026, 9, 28, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
		want   time.Time
		since  bool
	}{
		{"since exact", moderation.ListActionsParams{Since: "2026-09-28T12:00:00.123456Z"}, base, true},
		{"since sub-microsecond", moderation.ListActionsParams{Since: "2026-09-28T12:00:00.1234561Z"}, base.Add(time.Microsecond), true},
		{"until exact", moderation.ListActionsParams{Until: "2026-09-28T12:00:00.123456Z"}, base, false},
		{"until sub-microsecond", moderation.ListActionsParams{Until: "2026-09-28T12:00:00.1234561Z"}, base.Add(time.Microsecond), false},
		{"since timezone offset", moderation.ListActionsParams{Since: "2026-09-28T14:00:00.123456+02:00"}, base, true},
		{"until timezone offset and sub-microsecond", moderation.ListActionsParams{Until: "2026-09-28T14:00:00.1234561+02:00"}, base.Add(time.Microsecond), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			service := newModlogCursorService(store, modlogCursorConfig())
			_, err := service.ListActions(t.Context(), test.params)
			require.NoError(t, err)
			require.Len(t, store.listQueries, 1)
			actual := store.listQueries[0].Until
			if test.since {
				actual = store.listQueries[0].Since
			}
			require.NotNil(t, actual)
			assert.Equal(t, test.want, actual.UTC())
		})
	}
}
