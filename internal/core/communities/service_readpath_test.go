package communities_test

import (
	"context"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
)

// The community service's read surface: nine methods that resolve an
// identifier, clamp a page size and forward to the repository.
//
// # WHY THESE ARE WORTH TESTING AT ALL
//
// They look like pass-throughs, and that is exactly the risk. Each one performs
// two silent transformations before the repository sees anything, and both are
// unbounded-query defences:
//
//   - The identifier a client supplies may be a DID, a handle, an @-handle or a
//     scoped !name@instance string, and the repository understands only DIDs.
//     A method that forwards the raw string looks up a handle that cannot exist
//     and 404s a community that plainly does.
//   - The limit a client supplies is clamped to 50 unless it is between 1 and
//     100. A method that forgets is an endpoint a client can ask for every row
//     in the table through, which is the "unbounded queries" red flag in
//     CLAUDE.md rather than a cosmetic default.
//
// Neither is visible in the return value — both are visible only in what
// reached the repository — which is why this file asserts against a recording
// fake instead of a database. The complementary T1 file,
// service_identifier_resolution_test.go, proves the resolution itself against
// real SQL; the concern here is which of these methods remembered to do it.
//
// The clamp deserves one note. Nothing rejects an out-of-range limit: 0, -1 and
// 5,000 all silently become 50. That is a deliberate choice (a client asking
// for too much gets a page rather than a 400) and it is pinned below, because
// the alternative reading — that large limits are honoured — is what an
// unbounded-query review would assume.

const (
	// fakeInstanceDomain is what these services believe they are. It only has
	// to be a valid domain and to differ from the domain used in the
	// not-this-instance cases.
	fakeInstanceDomain = "coves.example"
	fakeInstanceDID    = "did:web:coves.example"

	seededCommunityDID    = "did:plc:seededcommunity000000"
	seededCommunityHandle = "c-gardening." + fakeInstanceDomain
	readerDID             = "did:plc:readerreaderreaderrea"
)

// newFakeBackedService wires the real service to a recording repository and no
// PDS at all. Every method under test in this file is a read, so the PDS
// factory is never reached; the tests that DO reach it pass their own.
func newFakeBackedService(t *testing.T) (communities.Service, *fakeCommunityRepo) {
	t.Helper()
	repo := newFakeCommunityRepo()
	service := communities.NewCommunityServiceWithPDSFactory(
		repo, "http://pds.invalid", fakeInstanceDID, fakeInstanceDomain, nil, nil, nil,
		communities.PrivateHostOptions(true)...)
	return service, repo
}

// seededService additionally puts one community in the repository, addressable
// by DID, handle and scoped identifier.
func seededService(t *testing.T) (communities.Service, *fakeCommunityRepo, *communities.Community) {
	t.Helper()
	service, repo := newFakeBackedService(t)
	community := repo.seed(&communities.Community{
		DID:        seededCommunityDID,
		Handle:     seededCommunityHandle,
		Name:       "gardening",
		Visibility: "public",
	})
	return service, repo, community
}

// everyAddressableForm is the set of strings a client may legitimately use for
// the seeded community. Every identifier-taking method must accept all of them.
func everyAddressableForm() map[string]string {
	return map[string]string{
		"DID":                       seededCommunityDID,
		"canonical handle":          seededCommunityHandle,
		"at-identifier":             "@" + seededCommunityHandle,
		"scoped identifier":         "!gardening@" + fakeInstanceDomain,
		"handle with outer spaces":  "  " + seededCommunityHandle + "  ",
		"handle with an upper-case": "c-gardening." + "COVES.EXAMPLE",
	}
}

func TestService_LimitClamping(t *testing.T) {
	t.Parallel()

	// Each case names a method, the limit a client asked for, and the limit the
	// repository must be handed. The clamp is identical in all of them, which is
	// itself the point: one of these forgetting is invisible from outside.
	for _, tc := range []struct {
		name  string
		asked int
		want  int
	}{
		{"zero becomes the default", 0, 50},
		{"negative becomes the default", -1, 50},
		{"over a hundred becomes the default", 5000, 50},
		{"exactly a hundred is honoured", 100, 100},
		{"one hundred and one is clamped", 101, 50},
		{"a small page is honoured", 7, 7},
		{"one is honoured", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			t.Run("GetUserSubscriptions", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.GetUserSubscriptions(ctx, readerDID, tc.asked, 3)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("ListSubscriptions")
				require.NoError(t, err)
				assert.Equal(t, []any{readerDID, tc.want, 3}, call.args)
			})

			t.Run("GetCommunitySubscribers", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.GetCommunitySubscribers(ctx, seededCommunityDID, tc.asked, 3)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("ListSubscribers")
				require.NoError(t, err)
				assert.Equal(t, []any{seededCommunityDID, tc.want, 3}, call.args)
			})

			t.Run("GetBlockedCommunities", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.GetBlockedCommunities(ctx, readerDID, tc.asked, 3)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("ListBlockedCommunities")
				require.NoError(t, err)
				assert.Equal(t, []any{readerDID, tc.want, 3}, call.args)
			})

			t.Run("ListCommunityMembers", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.ListCommunityMembers(ctx, seededCommunityDID, tc.asked, 3)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("ListMembers")
				require.NoError(t, err)
				assert.Equal(t, []any{seededCommunityDID, tc.want, 3}, call.args)
			})

			t.Run("ListCommunities", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.ListCommunities(ctx, communities.ListCommunitiesRequest{
					Sort: "popular", Limit: tc.asked, Offset: 3,
				})
				require.NoError(t, err)
				call, err := repo.onlyCallTo("List")
				require.NoError(t, err)
				req := call.args[0].(communities.ListCommunitiesRequest)
				assert.Equal(t, tc.want, req.Limit)
				assert.Equal(t, 3, req.Offset, "the offset is never clamped, only the page size")
				assert.Equal(t, "popular", req.Sort, "clamping must not discard the rest of the request")
			})

			t.Run("SearchCommunities", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, _, err := service.SearchCommunities(ctx, communities.SearchCommunitiesRequest{
					Query: "garden", Visibility: "unlisted", Limit: tc.asked, Offset: 3,
				})
				require.NoError(t, err)
				call, err := repo.onlyCallTo("Search")
				require.NoError(t, err)
				assert.Equal(t, communities.SearchCommunitiesRequest{
					Query: "garden", Visibility: "unlisted", Limit: tc.want, Offset: 3,
				}, call.args[0],
					"the clamp must rewrite the limit and nothing else. Asserted as the whole request "+
						"rather than field by field: a dropped offset silently re-serves page one "+
						"forever, and a dropped visibility filter makes unlisted communities "+
						"searchable")
			})
		})
	}
}

func TestService_IdentifierTakingReadsResolveBeforeQuerying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Every method here takes a client-supplied identifier and must hand the
	// repository a DID. The scoped and @-prefixed forms are the ones that catch
	// a forgotten resolve: a raw "!gardening@coves.example" reaching a
	// community_did column matches nothing, and the caller sees an empty list
	// rather than an error.
	for name, identifier := range everyAddressableForm() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			t.Run("GetCommunitySubscribers", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.GetCommunitySubscribers(ctx, identifier, 10, 0)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("ListSubscribers")
				require.NoError(t, err)
				assert.Equal(t, seededCommunityDID, call.args[0])
			})

			t.Run("ListCommunityMembers", func(t *testing.T) {
				service, repo, _ := seededService(t)
				_, err := service.ListCommunityMembers(ctx, identifier, 10, 0)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("ListMembers")
				require.NoError(t, err)
				assert.Equal(t, seededCommunityDID, call.args[0])
			})

			t.Run("GetMembership", func(t *testing.T) {
				service, repo, _ := seededService(t)
				repo.membership = &communities.Membership{UserDID: readerDID, CommunityDID: seededCommunityDID}
				_, err := service.GetMembership(ctx, readerDID, identifier)
				require.NoError(t, err)
				call, err := repo.onlyCallTo("GetMembership")
				require.NoError(t, err)
				assert.Equal(t, []any{readerDID, seededCommunityDID}, call.args)
			})

			t.Run("IsBlocked", func(t *testing.T) {
				service, repo, _ := seededService(t)
				repo.isBlocked = true
				blocked, err := service.IsBlocked(ctx, readerDID, identifier)
				require.NoError(t, err)
				assert.True(t, blocked)
				call, err := repo.onlyCallTo("IsBlocked")
				require.NoError(t, err)
				assert.Equal(t, []any{readerDID, seededCommunityDID}, call.args)
			})
		})
	}
}

func TestService_IdentifierTakingReadsFailBeforeQueryingWhenResolutionFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// An identifier that names nothing must stop at the resolve. Falling through
	// to the repository with an unresolved string is the failure mode that
	// produces "no members" instead of "no such community", and the two are not
	// the same answer to a client.
	unknown := "!nosuchcommunity@" + fakeInstanceDomain

	t.Run("GetCommunitySubscribers", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		_, err := service.GetCommunitySubscribers(ctx, unknown, 10, 0)
		require.Error(t, err)
		assert.Empty(t, repo.callsTo("ListSubscribers"))
	})

	t.Run("ListCommunityMembers", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		_, err := service.ListCommunityMembers(ctx, unknown, 10, 0)
		require.Error(t, err)
		assert.Empty(t, repo.callsTo("ListMembers"))
	})

	t.Run("GetMembership", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		_, err := service.GetMembership(ctx, readerDID, unknown)
		require.Error(t, err)
		assert.Empty(t, repo.callsTo("GetMembership"))
	})

	t.Run("IsBlocked", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		blocked, err := service.IsBlocked(ctx, readerDID, unknown)
		require.Error(t, err)
		assert.False(t, blocked, "an unresolvable community must not read as not-blocked: the caller "+
			"uses this to decide whether to serve content")
		assert.Empty(t, repo.callsTo("IsBlocked"))
	})
}

// TestService_UserScopedReadsDoNotResolveAnything pins the other half of the
// asymmetry: GetUserSubscriptions and GetBlockedCommunities take a user DID,
// which is already a DID, and must not try to resolve it as a community.
func TestService_UserScopedReadsDoNotResolveAnything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	service, repo, _ := seededService(t)
	_, err := service.GetUserSubscriptions(ctx, readerDID, 10, 0)
	require.NoError(t, err)
	_, err = service.GetBlockedCommunities(ctx, readerDID, 10, 0)
	require.NoError(t, err)

	assert.Empty(t, repo.callsTo("GetByDID"),
		"a user DID is not a community identifier; verifying it against the communities table would "+
			"make a user with no communities look like a user who does not exist")
	assert.Empty(t, repo.callsTo("GetByHandle"))
}

func TestService_ReadsReturnWhatTheRepositoryGave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Paging is the repository's job — it is the only layer that can do it
	// correctly, because the ORDER BY and the OFFSET have to agree. The claim
	// here is therefore not "the rows come back" (which any echo satisfies) but
	// that the service adds NOTHING on top: no second slice, no re-sort, no
	// re-derivation of fields the query already answered.
	t.Run("subscriptions are forwarded whole, in the repository's order", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		repo.subscriptions = []*communities.Subscription{
			{UserDID: readerDID, CommunityDID: "did:plc:c000000000000000000c", ContentVisibility: 4},
			{UserDID: readerDID, CommunityDID: "did:plc:a000000000000000000a", ContentVisibility: 1},
			{UserDID: readerDID, CommunityDID: "did:plc:b000000000000000000b", ContentVisibility: 5},
		}

		// Ask for a page SMALLER than what the repository answers with. The
		// clamp rewrites the limit the query is given; it must not then be
		// applied a second time in Go. A service that also sliced would drop
		// rows the repository had already paged, and the dropped rows would
		// never be reachable at any offset.
		got, err := service.GetUserSubscriptions(ctx, readerDID, 2, 0)
		require.NoError(t, err)
		require.Len(t, got, 3,
			"the service truncated the repository's page. Paging belongs to the query, and slicing "+
				"again here silently loses rows: the next offset skips past them")

		assert.Equal(t,
			[]string{"did:plc:c000000000000000000c", "did:plc:a000000000000000000a", "did:plc:b000000000000000000b"},
			[]string{got[0].CommunityDID, got[1].CommunityDID, got[2].CommunityDID},
			"the order came back changed. It is subscribed_at DESC from the query, and it is what the "+
				"offset paginates through — re-sorting in Go makes every page after the first "+
				"arbitrary")
		assert.Equal(t, []int{4, 1, 5},
			[]int{got[0].ContentVisibility, got[1].ContentVisibility, got[2].ContentVisibility},
			"the service must not re-derive or default fields the repository already answered")
	})

	t.Run("search results carry the total separately from the page", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		repo.searchResult = []*communities.Community{{DID: seededCommunityDID}}
		repo.searchTotal = 42

		got, total, err := service.SearchCommunities(ctx, communities.SearchCommunitiesRequest{Query: "garden"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, 42, total,
			"the total is what a client paginates against; dropping it or replacing it with len(page) "+
				"makes every result set look like one page")
	})

	t.Run("GetByDID is the post service's direct door and does not resolve", func(t *testing.T) {
		t.Parallel()
		service, repo, community := seededService(t)

		got, err := service.GetByDID(ctx, seededCommunityDID)
		require.NoError(t, err)
		assert.Equal(t, community.Handle, got.Handle)
		require.Len(t, repo.callsTo("GetByDID"), 1,
			"this method exists so the post service can skip identifier resolution; adding a resolve "+
				"step would put a second query on the path of every post write")
	})
}

func TestService_ReadsPropagateRepositoryFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A datastore failure must not be flattened into an empty page. Every one of
	// these methods returns a slice, and a caller that only checks len() would
	// render "this community has no members" during an outage.
	newFailing := func(t *testing.T) communities.Service {
		t.Helper()
		service, repo, _ := seededService(t)
		repo.err = errRepositoryUnavailable
		return service
	}

	t.Run("GetUserSubscriptions", func(t *testing.T) {
		t.Parallel()
		_, err := newFailing(t).GetUserSubscriptions(ctx, readerDID, 10, 0)
		require.ErrorIs(t, err, errRepositoryUnavailable)
	})

	t.Run("GetBlockedCommunities", func(t *testing.T) {
		t.Parallel()
		_, err := newFailing(t).GetBlockedCommunities(ctx, readerDID, 10, 0)
		require.ErrorIs(t, err, errRepositoryUnavailable)
	})

	t.Run("ListCommunities", func(t *testing.T) {
		t.Parallel()
		_, err := newFailing(t).ListCommunities(ctx, communities.ListCommunitiesRequest{})
		require.ErrorIs(t, err, errRepositoryUnavailable)
	})

	t.Run("SearchCommunities", func(t *testing.T) {
		t.Parallel()
		_, total, err := newFailing(t).SearchCommunities(ctx, communities.SearchCommunitiesRequest{Query: "x"})
		require.ErrorIs(t, err, errRepositoryUnavailable)
		assert.Zero(t, total)
	})

	t.Run("GetByDID", func(t *testing.T) {
		t.Parallel()
		_, err := newFailing(t).GetByDID(ctx, seededCommunityDID)
		require.ErrorIs(t, err, errRepositoryUnavailable)
	})

	t.Run("IsBlocked", func(t *testing.T) {
		t.Parallel()
		blocked, err := newFailing(t).IsBlocked(ctx, readerDID, seededCommunityDID)
		require.ErrorIs(t, err, errRepositoryUnavailable)
		assert.False(t, blocked)
	})
}

func TestService_SearchRequiresAQuery(t *testing.T) {
	t.Parallel()
	service, repo, _ := seededService(t)

	_, _, err := service.SearchCommunities(context.Background(),
		communities.SearchCommunitiesRequest{Query: "", Limit: 10})

	var validation *communities.ValidationError
	require.ErrorAs(t, err, &validation,
		"an empty query must be rejected rather than forwarded: the repository's ILIKE '%%' || '' || "+
			"'%%' matches every row in the table, so a client that omits the parameter gets an "+
			"unbounded scan back")
	assert.Equal(t, "query", validation.Field)
	assert.Empty(t, repo.callsTo("Search"))
}

func TestService_GetCommunity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("accepts every addressable form", func(t *testing.T) {
		t.Parallel()
		for name, identifier := range everyAddressableForm() {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				service, _, community := seededService(t)

				got, err := service.GetCommunity(ctx, identifier)
				require.NoError(t, err)
				assert.Equal(t, community.DID, got.DID)
			})
		}
	})

	// Community.DisplayHandle is documented as "computed, not stored" and is
	// tagged db:"-". Nothing computes it either: the only two writers in the
	// tree are ToCommunityView and ToCommunityViewDetailed, which set the field
	// on the VIEW they build. So a Community handed back by the service always
	// carries an empty DisplayHandle, and the ! form exists only once a view is
	// made. Pinned because the field's presence on the domain struct invites
	// exactly the opposite assumption, and it is serialised into JSON under
	// `displayHandle,omitempty` where a reader would expect to find it.
	t.Run("leaves the vestigial DisplayHandle field empty", func(t *testing.T) {
		t.Parallel()
		service, _, _ := seededService(t)

		got, err := service.GetCommunity(ctx, seededCommunityDID)
		require.NoError(t, err)
		assert.Empty(t, got.DisplayHandle,
			"IF THIS FAILED, something began populating Community.DisplayHandle. That is a fine change; "+
				"assert the value it now carries instead of reverting")
		assert.Equal(t, "!gardening@"+fakeInstanceDomain, got.GetDisplayHandle(),
			"the ! form a user sees is derived on demand from the stored atProto handle")
	})

	t.Run("rejects an empty identifier without querying", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)

		_, err := service.GetCommunity(ctx, "   ")
		require.ErrorIs(t, err, communities.ErrInvalidInput)
		assert.Empty(t, repo.calls)
	})
}

// TestService_WriteForwardMethodsRejectASessionlessCaller covers the guard that
// stands in front of every write-forward method.
//
// These four are the only Service methods that take an OAuth session, and each
// one starts by dereferencing session.AccountDID. A nil session must therefore
// be a validation error and not a panic: the middleware is supposed to have
// rejected the request already, so reaching here with nil means a route lost
// its RequireAuth, and a 400 says so while a panic takes the process down.
func TestService_WriteForwardMethodsRejectASessionlessCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var noSession *oauth.ClientSessionData

	t.Run("SubscribeToCommunity", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		_, err := service.SubscribeToCommunity(ctx, noSession, seededCommunityDID, 3)
		requireSessionRequired(t, err)
		assert.Empty(t, repo.calls)
	})

	t.Run("UnsubscribeFromCommunity", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		requireSessionRequired(t, service.UnsubscribeFromCommunity(ctx, noSession, seededCommunityDID))
		assert.Empty(t, repo.calls)
	})

	t.Run("BlockCommunity", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		_, err := service.BlockCommunity(ctx, noSession, seededCommunityDID)
		requireSessionRequired(t, err)
		assert.Empty(t, repo.calls)
	})

	t.Run("UnblockCommunity", func(t *testing.T) {
		t.Parallel()
		service, repo, _ := seededService(t)
		requireSessionRequired(t, service.UnblockCommunity(ctx, noSession, seededCommunityDID))
		assert.Empty(t, repo.calls)
	})
}

func requireSessionRequired(t *testing.T, err error) {
	t.Helper()
	var validation *communities.ValidationError
	require.ErrorAs(t, err, &validation)
	assert.Equal(t, "session", validation.Field)
}
