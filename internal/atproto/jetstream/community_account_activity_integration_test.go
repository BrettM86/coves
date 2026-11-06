//go:build integration

package jetstream

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/moderation"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	selfIndexAdminDID    = "did:plc:selfindexadmin"
	selfIndexSpamReason  = "social.coves.moderation.defs#reasonSpam"
	selfIndexRestoreNote = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

// selfIndexScene is a community C on this Postgres clone, the real post
// consumer that indexes author posts into it, and the instance moderation
// service over the Postgres store.
type selfIndexScene struct {
	db         *sql.DB
	community  string
	posts      *PostEventConsumer
	users      *mockUserService
	moderation moderation.Service
}

func newSelfIndexScene(t *testing.T) selfIndexScene {
	t.Helper()
	db := testkit.DB(t)
	communityName := testkit.UniqueIDWithPrefix(t, "selfidxcomm")
	community := fixtures.DID(communityName)
	insertBridgedCommunity(t, db, community, communityName+".test", community)

	userService := newMockUserService()
	communityRepository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	return selfIndexScene{
		db:        db,
		community: community,
		posts: NewPostEventConsumer(
			postgres.NewPostRepository(db), communityRepository, userService, db,
			WithAdmissions(postgres.NewAdmissionRepository(db)),
			WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		),
		users: userService,
		moderation: moderation.NewService(
			moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
			postgres.NewModerationRepository(db),
			moderation.Config{
				InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
				Communities: communityRepository,
				// The production bridge trust: a community on the trusted
				// bridge host is never banned as an account.
				BridgeTrust: trustingBridge(),
			},
		),
	}
}

// indexPost indexes a postv2 by author into the scene's community through the
// post consumer and returns its URI.
func (s selfIndexScene) indexPost(t *testing.T, author, rkey, cid string, timeUS int64) string {
	t.Helper()
	require.NoError(t, s.posts.HandleEvent(t.Context(), pv2Event(
		author, "create", rkey, testkit.TID(), cid, timeUS,
		pv2Record(s.community, "a post by "+rkey, "body"),
	)), "fixture: indexing the author's post")
	return pv2URI(author, rkey)
}

// admit records the community's acceptance of uri pinned at cid, the event
// that makes the post visible to other viewers.
func (s selfIndexScene) admit(t *testing.T, uri, cid string, timeUS int64) {
	t.Helper()
	require.NoError(t, s.posts.HandleEvent(t.Context(), acceptanceEvent(s.community, uri, cid, testkit.TID(), timeUS)),
		"fixture: accepting the post")
}

func (s selfIndexScene) publishProfile(t *testing.T, consumer *CommunityEventConsumer, did, name, origin string) error {
	t.Helper()
	return consumer.HandleEvent(t.Context(), &JetstreamEvent{
		Kind: "commit", Did: did, TimeUS: time.Now().UnixMicro(),
		Commit: profileCommit("create", name, origin, nil),
	})
}

func (s selfIndexScene) communityRows(t *testing.T, did string) int {
	t.Helper()
	return countRows(t, s.db, `SELECT COUNT(*) FROM communities WHERE did = $1`, did)
}

func (s selfIndexScene) banAccount(t *testing.T, account, key string) (*moderation.MutationResult, error) {
	t.Helper()
	return s.moderation.BanAccount(t.Context(), selfIndexAdminDID, moderation.BanAccountRequest{
		Subject: account, ExpectedVersion: "v0", IdempotencyKey: key, Reason: selfIndexSpamReason,
	})
}

// Acceptance (i): an account with a Coves footprint cannot index itself as a
// community, so it stays bannable and its ban hides its post from other
// viewers.
func TestCommunityProfile_AccountWithPostIsRefusedAndStaysBannable(t *testing.T) {
	t.Parallel()
	s := newSelfIndexScene(t)
	ctx := t.Context()
	instanceDID := fixtures.InstanceDID()

	accountName := testkit.UniqueIDWithPrefix(t, "selfidxuser")
	account := fixtures.DID(accountName)
	fixtures.User(t, s.db, accountName+".test", account)
	s.users.users[account] = &users.User{DID: account, Handle: accountName + ".test"}
	viewerName := testkit.UniqueIDWithPrefix(t, "selfidxviewer")
	viewer := fixtures.DID(viewerName)
	fixtures.User(t, s.db, viewerName+".test", viewer)

	const cid = "bafyreiselfindexpost"
	base := time.Now().UnixMicro()
	uri := s.indexPost(t, account, "selfindexpost", cid, base)
	s.admit(t, uri, cid, base+1_000_000)

	served := postgres.NewPostRepository(s.db, postgres.WithAccountRestrictionAuthority(instanceDID))
	before, err := served.VisibleHeaderView(ctx, uri, viewer)
	require.NoError(t, err)
	require.NotNil(t, before, "control: the admitted post is served to another viewer before any ban")

	consumer := NewCommunityEventConsumer(
		postgres.NewCommunityRepository(s.db, credentialciphertest.Fixed()), originTestInstance, true, nil)
	err = s.publishProfile(t, consumer, account, accountName, "")
	require.ErrorIs(t, err, ErrPermanentEvent,
		"a community profile from a DID with a users row and an indexed post is refused permanently")
	assert.Equal(t, 0, s.communityRows(t, account), "the account must not be indexed as a community")

	result, err := s.banAccount(t, account, "selfindex-ban")
	require.NoError(t, err, "the account is still bannable after its refused community profile")
	require.NotNil(t, result)
	assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)

	after, err := served.VisibleHeaderView(ctx, uri, viewer)
	require.NoError(t, err)
	assert.Nil(t, after, "the banned account's post is hidden from another viewer")

	ban, err := postgres.NewAccountBanReader(s.db, instanceDID).ActiveBan(ctx, account)
	require.NoError(t, err)
	require.NotNil(t, ban)
	assert.Equal(t, result.Action.ID, ban.ActionID)
	assert.True(t, ban.InForce(time.Now()), "a permanent ban is in force")
}

// Acceptance (ii): a footprint-less DID banned permanently may publish a
// community profile and is indexed, and the ban keeps reading as an account
// ban: it is not a community removal and RestoreCommunity cannot lift it.
// Replaces TestCommunityProfile_BannedAccountIsNotIndexedInPostgres, which
// pinned the refusal of that profile.
func TestCommunityProfile_BannedFootprintlessAccountIsIndexedAndBanKeepsItsKind(t *testing.T) {
	t.Parallel()
	s := newSelfIndexScene(t)
	ctx := t.Context()
	instanceDID := fixtures.InstanceDID()

	accountName := testkit.UniqueIDWithPrefix(t, "bancomm")
	account := fixtures.DID(accountName)

	result, err := s.banAccount(t, account, "footprintless-ban")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	require.NotNil(t, result.State)

	consumer := NewCommunityEventConsumer(
		postgres.NewCommunityRepository(s.db, credentialciphertest.Fixed()), originTestInstance, true, nil)
	require.NoError(t, s.publishProfile(t, consumer, account, accountName, ""),
		"a banned DID with no Coves footprint is indexed as a community")
	assert.Equal(t, 1, s.communityRows(t, account))

	removed, err := postgres.NewCommunityRemovalReader(s.db, instanceDID).CommunityRemoved(ctx, account)
	require.NoError(t, err)
	assert.False(t, removed, "an account ban does not read as a community removal after the DID is indexed")

	banReader := postgres.NewAccountBanReader(s.db, instanceDID)
	ban, err := banReader.ActiveBan(ctx, account)
	require.NoError(t, err)
	require.NotNil(t, ban, "the account ban still reads after the DID is indexed as a community")
	assert.Equal(t, result.Action.ID, ban.ActionID)
	assert.True(t, ban.InForce(time.Now()))

	_, err = s.moderation.RestoreCommunity(ctx, selfIndexAdminDID, moderation.RestoreCommunityRequest{
		Subject: account, ExpectedVersion: result.State.Version, IdempotencyKey: "footprintless-restore",
		Reason: selfIndexRestoreNote,
	})
	require.ErrorIs(t, err, moderation.ErrStateConflict, "RestoreCommunity never reverses an account ban")

	ban, err = banReader.ActiveBan(ctx, account)
	require.NoError(t, err)
	require.NotNil(t, ban, "the refused restore leaves the ban in place")
	assert.Equal(t, result.Action.ID, ban.ActionID)
}

// A Group DID whose verified PDS is a trusted bridge host may index as a
// community after it authored posts or comments (Tidepool's Group-as-author);
// the same footprint from a repo elsewhere is refused even when its record
// asserts bridge metadata. The indexed post-first bridge group is then refused
// by BanAccount: a trusted bridge's community is removed with removeCommunity.
func TestCommunityProfile_AccountActivityExemptOnlyForTrustedBridge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		pdsURL       string
		handleDomain string
		footprint    string
		wantIndexed  bool
		checkBan     bool
	}{
		{"post-first on the trusted bridge is indexed and not bannable as an account", trustedBridgePDS, ".lemmy-world.tdpl.io", "post", true, true},
		{"comment-first on the trusted bridge is indexed", trustedBridgePDS, ".lemmy-world.tdpl.io", "comment", true, false},
		{"post-first on an untrusted PDS asserting bridge metadata is refused", untrustedPDS, ".example.net", "post", false, false},
		// The DID controller writes the PDS endpoint: naming the bridge PDS
		// with a verified handle outside the bridge's domain never exempts.
		{"post-first naming the trusted bridge PDS with a handle outside the bridge domain is refused", trustedBridgePDS, ".lemmy-world.example.org", "post", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSelfIndexScene(t)
			groupName := testkit.UniqueIDWithPrefix(t, "bridgegroup")
			group := fixtures.DID(groupName)
			handle := groupName + tc.handleDomain
			insertBridgedUserOnPDS(t, s.db, group, handle, tc.pdsURL)
			s.users.users[group] = &users.User{DID: group, Handle: handle}

			base := time.Now().UnixMicro()
			switch tc.footprint {
			case "post":
				s.indexPost(t, group, "groupauthored", "bafyreigrouppost", base)
				require.Equal(t, 1, countRows(t, s.db, `SELECT COUNT(*) FROM posts WHERE author_did = $1`, group),
					"fixture: the group's post is indexed")
			case "comment":
				otherName := testkit.UniqueIDWithPrefix(t, "bridgeother")
				other := fixtures.DID(otherName)
				fixtures.User(t, s.db, otherName+".test", other)
				root := s.indexPost(t, other, "commentroot", "bafyreicommentroot", base)
				_, err := s.db.Exec(`INSERT INTO comments
					(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
					VALUES ($1, 'bafyreigroupcomment', 'groupcomment', $2, $3, 'bafyreicommentroot', $3, 'bafyreicommentroot', 'comment', NOW())`,
					"at://"+group+"/social.coves.community.comment/groupcomment", group, root)
				require.NoError(t, err, "fixture: the group's comment is indexed")
			}

			consumer := NewCommunityEventConsumer(
				postgres.NewCommunityRepository(s.db, credentialciphertest.Fixed()), originTestInstance, true,
				fixedResolver{id: identity.Identity{Handle: handle, PDSURL: tc.pdsURL}},
				WithCommunityBridgeTrust(trustingBridge()))
			err := s.publishProfile(t, consumer, group, groupName, bridgedOrigin)

			if tc.wantIndexed {
				require.NoError(t, err)
				assert.Equal(t, 1, s.communityRows(t, group))
				if tc.checkBan {
					result, err := s.banAccount(t, group, "bridge-group-ban")
					require.ErrorIs(t, err, moderation.ErrInvalidSubject,
						"a trusted bridge's community with account activity is never banned as an account")
					assert.Nil(t, result)
					assert.Equal(t, 0, countRows(t, s.db, `SELECT COUNT(*) FROM moderation_actions WHERE subject_uri = $1`, "at://"+group),
						"the refused ban writes no moderation action")
				}
				return
			}
			require.ErrorIs(t, err, ErrPermanentEvent,
				"only a verified bridge PDS with a handle under the bridge's domain exempts account activity")
			assert.Equal(t, 0, s.communityRows(t, group))
		})
	}
}

// BanAccount refuses a community with account activity as a trusted bridge's
// only when the stored community row is one: its pds_url is a trusted bridge
// host AND its stored handle is under that host's registrable domain. The DID's
// users row carries the opposite handle and PDS, so the outcome shows the
// stored community row decided it.
func TestBanAccount_TrustedBridgeRefusalReadsStoredCommunityHandleAndPDS(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                  string
		communityPDSURL       string
		communityHandleDomain string
		userPDSURL            string
		userHandleDomain      string
		wantRefused           bool
	}{
		{
			name:            "bridge PDS with a stored handle outside the bridge domain is banned",
			communityPDSURL: trustedBridgePDS, communityHandleDomain: ".lemmy-world.example.org",
			userPDSURL: trustedBridgePDS, userHandleDomain: "-user.lemmy-world.tdpl.io",
		},
		{
			name:            "bridge PDS with a stored bridge-domain handle is refused",
			communityPDSURL: trustedBridgePDS, communityHandleDomain: ".lemmy-world.tdpl.io",
			userPDSURL: untrustedPDS, userHandleDomain: "-user.lemmy-world.example.org",
			wantRefused: true,
		},
		{
			name:            "untrusted PDS with a stored bridge-domain handle is banned",
			communityPDSURL: untrustedPDS, communityHandleDomain: ".lemmy-world.tdpl.io",
			userPDSURL: trustedBridgePDS, userHandleDomain: "-user.lemmy-world.tdpl.io",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := newSelfIndexScene(t)
			groupName := testkit.UniqueIDWithPrefix(t, "storedbridge")
			group := fixtures.DID(groupName)
			// The users row is the account activity, so neither outcome can
			// come from a community with no activity.
			insertBridgedUserOnPDS(t, s.db, group, groupName+test.userHandleDomain, test.userPDSURL)
			insertBridgedCommunityOnPDS(t, s.db, group, groupName+test.communityHandleDomain, group, test.communityPDSURL)

			result, err := s.banAccount(t, group, "stored-bridge-ban")
			if test.wantRefused {
				require.ErrorIs(t, err, moderation.ErrInvalidSubject,
					"a trusted bridge's stored community is removed with removeCommunity, never banned")
				assert.Nil(t, result)
				assert.Equal(t, 0, countRows(t, s.db, `SELECT COUNT(*) FROM moderation_actions WHERE subject_uri = $1`, "at://"+group),
					"the refused ban writes no moderation action")
				return
			}
			require.NoError(t, err, "a community row that is not a trusted bridge's stays bannable as an account")
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			assert.Equal(t, 1, countRows(t, s.db, `SELECT COUNT(*) FROM moderation_actions WHERE subject_uri = $1 AND subject_kind = 'account'`, "at://"+group),
				"the ban is recorded as an account action")
		})
	}
}

// profileEvent is a community profile commit at rev in did's repo.
func profileEvent(did, operation, rev, name string) *JetstreamEvent {
	commit := &CommitEvent{
		Rev: rev, Operation: operation, Collection: "social.coves.community.profile", RKey: "self",
	}
	if operation != "delete" {
		commit = profileCommit(operation, name, "", nil)
		commit.Rev = rev
	}
	return &JetstreamEvent{Kind: "commit", Did: did, TimeUS: time.Now().UnixMicro(), Commit: commit}
}

// A native community that authored a post in itself gets a users row from
// post author hydration. That row is not account activity: after the
// community's profile is deleted (its posts cascade), republishing the profile
// indexes the community again.
func TestCommunityProfile_HydratedAuthorRowDoesNotBlockReindexAfterDelete(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := t.Context()
	name := testkit.UniqueIDWithPrefix(t, "hydcomm")
	community := fixtures.DID(name)
	resolver := fixedResolver{id: identity.Identity{Handle: name + ".communities.example.net", PDSURL: untrustedPDS}}

	communityRepository := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	communityConsumer := NewCommunityEventConsumer(communityRepository, originTestInstance, true, resolver,
		WithCommunityBridgeTrust(trustingBridge()))
	userRepository := postgres.NewUserRepository(db)
	postConsumer := NewPostEventConsumer(
		postgres.NewPostRepository(db), communityRepository,
		users.NewUserService(userRepository, nil, untrustedPDS, nil, ""), db,
		WithPostIdentityResolver(resolver),
		WithAdmissions(postgres.NewAdmissionRepository(db)),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
	)
	communityRows := func() int {
		return countRows(t, db, `SELECT COUNT(*) FROM communities WHERE did = $1`, community)
	}

	require.NoError(t, communityConsumer.HandleEvent(ctx, profileEvent(community, "create", testkit.TID(), name)),
		"fixture: the community is indexed")
	require.Equal(t, 1, communityRows())

	require.NoError(t, postConsumer.HandleEvent(ctx, pv2Event(community, "create", "hydratedpost", testkit.TID(),
		"bafyreihydratedpost", time.Now().UnixMicro(), pv2Record(community, "a post by the community", "body"))),
		"fixture: the community's post in itself is indexed")
	require.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM posts WHERE author_did = $1`, community))
	hydrated, err := userRepository.GetByDID(ctx, community)
	require.NoError(t, err, "post author hydration indexes the community's users row")
	assert.Equal(t, users.IndexSourceAuthorHydration, hydrated.IndexSource, "the hydrated row records its source")

	require.NoError(t, communityConsumer.HandleEvent(ctx, profileEvent(community, "delete", testkit.TID(), name)))
	require.Equal(t, 0, communityRows(), "fixture: the profile delete removes the community")
	require.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM posts WHERE author_did = $1`, community),
		"fixture: the community's posts cascade with it")

	require.NoError(t, communityConsumer.HandleEvent(ctx, profileEvent(community, "create", testkit.TID(), name)),
		"a hydrated users row is not account activity, so the republished profile is indexed")
	assert.Equal(t, 1, communityRows())
}
