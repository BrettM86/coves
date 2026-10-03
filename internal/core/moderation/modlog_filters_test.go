package moderation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type modlogFilterCommunityResolver struct {
	did string
	err error
	// failures overrides did and err for one identifier.
	failures map[string]error
	calls    []string
}

func (resolver *modlogFilterCommunityResolver) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	resolver.calls = append(resolver.calls, identifier)
	if err, failed := resolver.failures[identifier]; failed {
		return "", err
	}
	return resolver.did, resolver.err
}

type modlogFilterHandleResolver struct {
	dids map[string]string
	err  error
	// failures overrides dids and err for one handle.
	failures map[string]error
	calls    []string
}

func (resolver *modlogFilterHandleResolver) ResolveHandle(_ context.Context, handle string) (string, string, error) {
	resolver.calls = append(resolver.calls, handle)
	if err, failed := resolver.failures[handle]; failed {
		return "", "", err
	}
	return resolver.dids[handle], "", resolver.err
}

func modlogFilterService(store *inMemoryModerationStore, communities *modlogFilterCommunityResolver, handles *modlogFilterHandleResolver) moderation.Service {
	config := moderation.Config{InstanceDID: "did:web:instance.example", CursorSecret: "secret-one"}
	if communities != nil {
		config.CommunityResolver = communities
	}
	if handles != nil {
		config.HandleResolver = handles
	}
	return moderation.NewService(&fakeSubjectReader{}, store, config)
}

func modlogFilterList(t *testing.T, service moderation.Service, admin bool, params moderation.ListActionsParams, actionID string) (any, error) {
	t.Helper()
	return modlogFilterListContext(t.Context(), service, admin, params, actionID)
}

func modlogFilterListContext(ctx context.Context, service moderation.Service, admin bool, params moderation.ListActionsParams, actionID string) (any, error) {
	if admin {
		return service.ListAdminActions(ctx, moderation.ListAdminActionsParams{ListActionsParams: params, ActionID: actionID})
	}
	return service.ListActions(ctx, params)
}

func TestModlogFilterDIDsBypassResolvers(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			community := &modlogFilterCommunityResolver{did: "did:plc:wrongcommunity"}
			handles := &modlogFilterHandleResolver{dids: map[string]string{}}
			params := moderation.ListActionsParams{
				Authority: "did:plc:authority", Actor: "did:plc:actor", Community: "did:plc:community",
			}
			_, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, params, "")
			require.NoError(t, err)
			require.Len(t, store.listQueries, 1)
			assert.Equal(t, params.Authority, store.listQueries[0].AuthorityDID)
			assert.Equal(t, params.Actor, store.listQueries[0].ActorDID)
			assert.Equal(t, params.Community, store.listQueries[0].CommunityDID)
			assert.Empty(t, community.calls)
			assert.Empty(t, handles.calls)
		})
	}
}

func TestModlogFilterResolvesHandlesAndCommunityForms(t *testing.T) {
	for _, identifier := range []string{"name@coves.social", "!name@coves.social", "name.coves.social"} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", identifier, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				community := &modlogFilterCommunityResolver{did: "did:plc:resolvedcommunity"}
				handles := &modlogFilterHandleResolver{dids: map[string]string{
					"authority.example": "did:plc:resolvedauthority",
					"actor.example":     "did:plc:resolvedactor",
				}}
				params := moderation.ListActionsParams{Authority: "authority.example", Actor: "actor.example", Community: identifier}
				_, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, params, "")
				require.NoError(t, err)
				require.Len(t, store.listQueries, 1)
				assert.Equal(t, "did:plc:resolvedauthority", store.listQueries[0].AuthorityDID)
				assert.Equal(t, "did:plc:resolvedactor", store.listQueries[0].ActorDID)
				assert.Equal(t, "did:plc:resolvedcommunity", store.listQueries[0].CommunityDID)
				assert.Equal(t, []string{"authority.example", "actor.example"}, handles.calls)
				assert.Equal(t, []string{identifier}, community.calls)
			})
		}
	}
}

func TestModlogFilterUnknownTargetReturnsIndistinguishableEmptyPage(t *testing.T) {
	for _, test := range []struct {
		name      string
		params    moderation.ListActionsParams
		community error
		handle    error
	}{
		{"community", moderation.ListActionsParams{Community: "missing@coves.social"}, fmt.Errorf("resolve: %w", communities.ErrCommunityNotFound), nil},
		{"authority handle", moderation.ListActionsParams{Authority: "missing.example"}, nil, &identity.ErrNotFound{Identifier: "missing.example"}},
		{"actor handle", moderation.ListActionsParams{Actor: "missing.example"}, nil, &identity.ErrNotFound{Identifier: "missing.example"}},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				store.listRows = modlogCursorRows()[:2] // A mistaken store call would disclose actions and a cursor.
				community := &modlogFilterCommunityResolver{err: test.community}
				handles := &modlogFilterHandleResolver{err: test.handle}
				service := modlogFilterService(store, community, handles)
				page, err := modlogFilterList(t, service, admin, test.params, "")
				require.NoError(t, err)
				require.NotNil(t, page)
				raw, err := json.Marshal(page)
				require.NoError(t, err)
				assert.Equal(t, `{"actions":[]}`, string(raw), "empty page has non-nil actions and no cursor")
				assert.Empty(t, store.listQueries, "unknown target must never query the store")
			})
		}
	}
}

func TestModlogFilterInvalidTargetsFailBeforeStore(t *testing.T) {
	for _, test := range []struct {
		name      string
		params    moderation.ListActionsParams
		community error
		handle    error
	}{
		{"community validation", moderation.ListActionsParams{Community: "bad@coves.social"}, communities.NewValidationError("community", "invalid"), nil},
		{"community invalid input", moderation.ListActionsParams{Community: "bad@coves.social"}, communities.ErrInvalidInput, nil},
		{"ambiguous community", moderation.ListActionsParams{Community: "bad@coves.social"}, communities.ErrAmbiguousCommunity, nil},
		{"authority invalid identifier", moderation.ListActionsParams{Authority: "authority.example"}, nil, &identity.ErrInvalidIdentifier{Identifier: "authority.example", Reason: "invalid"}},
		{"actor invalid identifier", moderation.ListActionsParams{Actor: "actor.example"}, nil, &identity.ErrInvalidIdentifier{Identifier: "actor.example", Reason: "invalid"}},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				community := &modlogFilterCommunityResolver{err: test.community}
				handles := &modlogFilterHandleResolver{err: test.handle}
				_, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, test.params, "")
				require.ErrorIs(t, err, moderation.ErrInvalidRequest)
				assert.Empty(t, store.listQueries)
			})
		}
	}
}

func TestModlogFilterResolutionOutageFailsBeforeStore(t *testing.T) {
	for _, test := range []struct {
		name      string
		params    moderation.ListActionsParams
		community error
		handle    error
		nilTarget string
	}{
		{"community outage", moderation.ListActionsParams{Community: "name@coves.social"}, errors.New("resolver unavailable"), nil, ""},
		{"handle outage", moderation.ListActionsParams{Actor: "actor.example"}, nil, &identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "resolver unavailable"}, ""},
		{"missing community resolver", moderation.ListActionsParams{Community: "name@coves.social"}, nil, nil, "community"},
		{"missing actor resolver", moderation.ListActionsParams{Actor: "actor.example"}, nil, nil, "handle"},
		{"missing authority resolver", moderation.ListActionsParams{Authority: "authority.example"}, nil, nil, "handle"},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				community := &modlogFilterCommunityResolver{err: test.community}
				handles := &modlogFilterHandleResolver{err: test.handle}
				if test.nilTarget == "community" {
					community = nil
				}
				if test.nilTarget == "handle" {
					handles = nil
				}
				_, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, test.params, "")
				require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
				assert.Empty(t, store.listQueries)
			})
		}
	}
}

func TestModlogFilterValidationAndCursorPrecedeResolution(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			community := &modlogFilterCommunityResolver{err: errors.New("should not resolve")}
			handles := &modlogFilterHandleResolver{err: errors.New("should not resolve")}
			service := modlogFilterService(store, community, handles)
			_, err := modlogFilterList(t, service, admin, moderation.ListActionsParams{
				Limit: modlogLimit(0), Cursor: "not?base64url", Actor: "actor.example",
			}, "")
			require.ErrorIs(t, err, moderation.ErrInvalidRequest, "parameter validation precedes cursor verification")
			_, err = modlogFilterList(t, service, admin, moderation.ListActionsParams{
				Cursor: "not?base64url", Actor: "actor.example", Community: "name@coves.social",
			}, "")
			require.ErrorIs(t, err, moderation.ErrInvalidCursor, "cursor verification precedes target resolution")
			assert.Empty(t, handles.calls)
			assert.Empty(t, community.calls)
			assert.Empty(t, store.listQueries)
		})
	}
}

func TestModlogFilterHiddenExclusionDependsOnlyOnSubjectDetail(t *testing.T) {
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
		hidden bool
	}{
		{"no filters", moderation.ListActionsParams{}, false},
		{"subject", moderation.ListActionsParams{Subject: modlogTestAction().SubjectURI}, true},
		{"collection", moderation.ListActionsParams{Collection: moderation.CommentCollection}, true},
		{"community", moderation.ListActionsParams{Community: "did:plc:community"}, true},
		{"subject and community", moderation.ListActionsParams{Subject: modlogTestAction().SubjectURI, Community: "did:plc:community"}, true},
		{"actor", moderation.ListActionsParams{Actor: "did:plc:actor"}, false},
		{"authority", moderation.ListActionsParams{Authority: "did:plc:authority"}, false},
		{"action", moderation.ListActionsParams{Action: moderation.ActionRemove}, false},
		{"origin", moderation.ListActionsParams{Origin: moderation.OriginLocal}, false},
		{"since", moderation.ListActionsParams{Since: "2026-09-28T12:00:00Z"}, false},
		{"until", moderation.ListActionsParams{Until: "2026-09-29T12:00:00Z"}, false},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				_, err := modlogFilterList(t, modlogFilterService(store, nil, nil), admin, test.params, "")
				require.NoError(t, err)
				require.Len(t, store.listQueries, 1)
				assert.Equal(t, test.hidden && !admin, store.listQueries[0].ExcludeHidden)
				assert.Equal(t, test.hidden && !admin, store.listQueries[0].ExcludeRestricted)
			})
		}
	}
}

func TestModlogFilterActionIDIsAdminOnly(t *testing.T) {
	store := newInMemoryModerationStore(time.Now())
	service := modlogFilterService(store, nil, nil)
	_, err := modlogFilterList(t, service, false, moderation.ListActionsParams{}, "")
	require.NoError(t, err)
	_, err = modlogFilterList(t, service, true, moderation.ListActionsParams{}, "x")
	require.NoError(t, err)
	require.Len(t, store.listQueries, 2)
	assert.Empty(t, store.listQueries[0].ActionID)
	assert.Equal(t, "x", store.listQueries[1].ActionID)
}

// Permanent handle failures are the caller's identifier being wrong, not an
// outage, so they are the not-found class: the indistinguishable empty page,
// never a retryable 503. The list follows the OAuth login precedent.
func TestModlogFilterPermanentHandleFailuresReturnEmptyPage(t *testing.T) {
	for _, sentinel := range []error{
		indigoIdentity.ErrHandleMismatch, indigoIdentity.ErrHandleNotDeclared,
		indigoIdentity.ErrHandleReservedTLD, indigoIdentity.ErrInvalidHandle,
		indigoIdentity.ErrHandleNotFound, indigoIdentity.ErrDIDNotFound,
	} {
		for _, field := range []string{"actor", "authority"} {
			for _, admin := range []bool{false, true} {
				t.Run(fmt.Sprintf("%v/%s/admin=%t", sentinel, field, admin), func(t *testing.T) {
					store := newInMemoryModerationStore(time.Now())
					store.listRows = modlogCursorRows()[:2]
					handles := &modlogFilterHandleResolver{err: &identity.ErrResolutionFailed{
						Identifier: "broken.example", Reason: "permanent", Err: fmt.Errorf("%w: broken.example", sentinel),
					}}
					params := moderation.ListActionsParams{Actor: "broken.example"}
					if field == "authority" {
						params = moderation.ListActionsParams{Authority: "broken.example"}
					}
					page, err := modlogFilterList(t, modlogFilterService(store, nil, handles), admin, params, "")
					require.NoError(t, err)
					raw, err := json.Marshal(page)
					require.NoError(t, err)
					assert.Equal(t, `{"actions":[]}`, string(raw))
					assert.Empty(t, store.listQueries, "a permanently unresolvable target must never query the store")
				})
			}
		}
	}
}

// A resolver that answers an empty DID without an error must not silently
// drop the filter and return the whole log.
func TestModlogFilterEmptyResolvedDIDIsNotFound(t *testing.T) {
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
	}{
		{"actor", moderation.ListActionsParams{Actor: "empty.example"}},
		{"authority", moderation.ListActionsParams{Authority: "empty.example"}},
		{"community", moderation.ListActionsParams{Community: "empty@coves.social"}},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				store.listRows = modlogCursorRows()[:2]
				community := &modlogFilterCommunityResolver{}
				handles := &modlogFilterHandleResolver{dids: map[string]string{}}
				page, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, test.params, "")
				require.NoError(t, err)
				raw, err := json.Marshal(page)
				require.NoError(t, err)
				assert.Equal(t, `{"actions":[]}`, string(raw))
				assert.Empty(t, store.listQueries, "an empty resolved DID must never widen the filter")
			})
		}
	}
}

// A client that went away is not an outage: cancellation passes through
// unwrapped, while a blown deadline stays unavailable.
func TestModlogListCancellationIsNotUnavailable(t *testing.T) {
	for _, test := range []struct {
		name        string
		params      moderation.ListActionsParams
		community   error
		handle      error
		store       error
		cause       error
		unavailable bool
	}{
		{"handle resolver canceled", moderation.ListActionsParams{Actor: "actor.example"}, nil,
			&identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "canceled", Err: context.Canceled}, nil, context.Canceled, false},
		{"community resolver canceled", moderation.ListActionsParams{Community: "name@coves.social"},
			fmt.Errorf("resolve community: %w", context.Canceled), nil, nil, context.Canceled, false},
		{"store canceled", moderation.ListActionsParams{}, nil, nil, fmt.Errorf("query: %w", context.Canceled), context.Canceled, false},
		{"handle resolver deadline", moderation.ListActionsParams{Actor: "actor.example"}, nil,
			&identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "deadline", Err: context.DeadlineExceeded}, nil, context.DeadlineExceeded, true},
		{"community resolver deadline", moderation.ListActionsParams{Community: "name@coves.social"},
			fmt.Errorf("resolve community: %w", context.DeadlineExceeded), nil, nil, context.DeadlineExceeded, true},
		{"store deadline", moderation.ListActionsParams{}, nil, nil, fmt.Errorf("query: %w", context.DeadlineExceeded), context.DeadlineExceeded, true},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				store.failListActions = test.store
				community := &modlogFilterCommunityResolver{err: test.community}
				handles := &modlogFilterHandleResolver{err: test.handle}
				_, err := modlogFilterList(t, modlogFilterService(store, community, handles), admin, test.params, "")
				require.ErrorIs(t, err, test.cause)
				assert.Equal(t, test.unavailable, errors.Is(err, moderation.ErrModerationUnavailable))
			})
		}
	}
}

// lib/pq reports a statement cancelled mid-flight as SQLSTATE 57014, which
// wraps no context error, so the caller's context decides: a client that went
// away is cancellation, and a blown caller deadline stays unavailable.
func TestModlogListCancellationDuringStatementFollowsCallerContext(t *testing.T) {
	statementCanceled := &pq.Error{Code: "57014", Message: "canceling statement due to user request"}
	for _, test := range []struct {
		name      string
		params    moderation.ListActionsParams
		community error
		handle    error
		store     error
		resolver  bool
	}{
		{"store", moderation.ListActionsParams{}, nil, nil, fmt.Errorf("list moderation actions: %w", statementCanceled), false},
		{
			"community resolver",
			moderation.ListActionsParams{Community: "name@coves.social"},
			fmt.Errorf("failed to look up community handle: %w", statementCanceled), nil, nil, true,
		},
		{
			"actor handle resolver",
			moderation.ListActionsParams{Actor: "actor.example"},
			nil,
			&identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "cache lookup failed", Err: statementCanceled}, nil, true,
		},
		{
			"authority handle resolver",
			moderation.ListActionsParams{Authority: "authority.example"},
			nil,
			&identity.ErrResolutionFailed{Identifier: "authority.example", Reason: "cache lookup failed", Err: statementCanceled}, nil, true,
		},
	} {
		for _, admin := range []bool{false, true} {
			run := func(ctx context.Context) (*inMemoryModerationStore, error) {
				store := newInMemoryModerationStore(time.Now())
				store.failListActions = test.store
				community := &modlogFilterCommunityResolver{err: test.community}
				handles := &modlogFilterHandleResolver{err: test.handle}
				_, err := modlogFilterListContext(ctx, modlogFilterService(store, community, handles), admin, test.params, "")
				return store, err
			}
			assertFailedWhere := func(t *testing.T, store *inMemoryModerationStore) {
				t.Helper()
				if test.resolver {
					assert.Empty(t, store.listQueries, "a failed resolution must never query the store")
				} else {
					assert.Len(t, store.listQueries, 1)
				}
			}
			t.Run(fmt.Sprintf("%s/canceled/admin=%t", test.name, admin), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				store, err := run(ctx)
				require.ErrorIs(t, err, context.Canceled)
				assert.NotErrorIs(t, err, moderation.ErrModerationUnavailable)
				assert.NotErrorIs(t, err, moderation.ErrResolverUnavailable)
				assertFailedWhere(t, store)
			})
			t.Run(fmt.Sprintf("%s/deadline/admin=%t", test.name, admin), func(t *testing.T) {
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				store, err := run(ctx)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
				assert.Equal(t, test.resolver, errors.Is(err, moderation.ErrResolverUnavailable))
				assert.ErrorIs(t, err, statementCanceled, "the driver error stays in the logged cause")
				assertFailedWhere(t, store)
			})
		}
	}
}

// Resolver-origin unavailability carries its own sentinel as well as
// ErrModerationUnavailable, so the handler can log it below store failures.
func TestModlogListUnavailabilityNamesItsOrigin(t *testing.T) {
	for _, test := range []struct {
		name       string
		params     moderation.ListActionsParams
		community  *modlogFilterCommunityResolver
		handles    *modlogFilterHandleResolver
		store      error
		resolution bool
	}{
		{"handle outage", moderation.ListActionsParams{Actor: "actor.example"}, nil,
			&modlogFilterHandleResolver{err: &identity.ErrResolutionFailed{Identifier: "actor.example", Reason: "down", Err: errors.New("dial tcp: refused")}}, nil, true},
		{"community outage", moderation.ListActionsParams{Community: "name@coves.social"},
			&modlogFilterCommunityResolver{err: errors.New("community resolver down")}, nil, nil, true},
		{"missing handle resolver", moderation.ListActionsParams{Authority: "authority.example"}, nil, nil, nil, true},
		{"missing community resolver", moderation.ListActionsParams{Community: "name@coves.social"}, nil, nil, nil, true},
		{"store outage", moderation.ListActionsParams{}, nil, nil, errors.New("pq: connection refused"), false},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				store.failListActions = test.store
				_, err := modlogFilterList(t, modlogFilterService(store, test.community, test.handles), admin, test.params, "")
				require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
				assert.Equal(t, test.resolution, errors.Is(err, moderation.ErrResolverUnavailable))
			})
		}
	}
}

func TestModlogListStoreFailureIsUnavailable(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%t", admin), func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			store.failListActions = errors.New("pq: connection refused")
			page, err := modlogFilterList(t, modlogFilterService(store, nil, nil), admin, moderation.ListActionsParams{}, "")
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			require.ErrorIs(t, err, store.failListActions)
			assert.Nil(t, page)
			assert.Len(t, store.listQueries, 1)
		})
	}
}

// SQL and Go must agree on hiddenness. A hidden row returned under
// ExcludeHidden means they disagree, and the public page fails closed.
func TestModlogListFailsClosedWhenStoreReturnsHiddenRowUnderExclusion(t *testing.T) {
	hiddenReason := moderation.HiddenActionReasons()[0]
	for _, test := range []struct {
		name   string
		hidden func(*moderation.Action)
	}{
		{"hidden reason", func(action *moderation.Action) { action.Reason = hiddenReason }},
		{"reverses hidden reason", func(action *moderation.Action) {
			action.Action = moderation.ActionRestore
			action.ReversesActionID = "3mwmyreversed"
			action.ReversedActionReason = hiddenReason
		}},
	} {
		for _, lookAhead := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/look-ahead=%t", test.name, lookAhead), func(t *testing.T) {
				rows := modlogCursorRows()
				index := 0
				if lookAhead {
					index = 2 // The unprojected look-ahead row still proves the disagreement.
				}
				test.hidden(&rows[index])
				store := newInMemoryModerationStore(time.Now())
				store.listRows = rows
				service := modlogFilterService(store, nil, nil)
				params := moderation.ListActionsParams{Limit: modlogLimit(2), Subject: modlogTestAction().SubjectURI}
				page, err := modlogFilterList(t, service, false, params, "")
				require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
				assert.Nil(t, page)

				_, err = modlogFilterList(t, service, false, moderation.ListActionsParams{Limit: modlogLimit(2)}, "")
				require.NoError(t, err, "an unfiltered public page projects hidden rows without their subject")
				_, err = modlogFilterList(t, service, true, params, "")
				require.NoError(t, err, "the admin log never excludes hidden rows")
			})
		}
	}
}
