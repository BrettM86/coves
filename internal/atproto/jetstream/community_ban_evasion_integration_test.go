//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// banEvasionCommunityLookup is the decider's community lookup over the
// indexed communities table. The admission row already names the resolved
// community DID, so resolution returns the identifier unchanged.
type banEvasionCommunityLookup struct {
	repository communities.Repository
}

func (lookup banEvasionCommunityLookup) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (lookup banEvasionCommunityLookup) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return lookup.repository.GetByDID(ctx, did)
}

// newBanEvasionDecider is the production admission decider over the scene's
// Postgres clone, with the instance account bans read as production reads them.
func newBanEvasionDecider(s selfIndexScene) *posts.AdmissionEngineDecider {
	return posts.NewAdmissionEngineDecider(posts.DeciderDeps{
		Posts:       postgres.NewPostRepository(s.db),
		Communities: banEvasionCommunityLookup{repository: postgres.NewCommunityRepository(s.db, credentialciphertest.Fixed())},
		Policy: posts.AdmissionPolicy{
			Ledger:      postgres.NewSubmissionLedger(s.db),
			Bans:        postgres.NewCommunityBanRepository(s.db),
			AccountBans: postgres.NewAccountBanReader(s.db, fixtures.InstanceDID()),
			Now:         time.Now,
		},
	})
}

// Acceptance: a DID with no Coves footprint indexes itself as a community
// first, then posts and votes in another community C. It is banned as an
// account: its post in C is hidden from another viewer, its new post in C is
// deferred at admission while a clean author's is admitted, and an unban
// serves the post again.
func TestBanAccount_CommunityFirstDIDWithActivityIsBannedAsAccount(t *testing.T) {
	t.Parallel()
	s := newSelfIndexScene(t)
	ctx := t.Context()
	instanceDID := fixtures.InstanceDID()

	evaderName := testkit.UniqueIDWithPrefix(t, "evader")
	evader := fixtures.DID(evaderName)
	viewerName := testkit.UniqueIDWithPrefix(t, "evadeviewer")
	viewer := fixtures.DID(viewerName)
	fixtures.User(t, s.db, viewerName+".test", viewer)
	cleanName := testkit.UniqueIDWithPrefix(t, "evadeclean")
	clean := fixtures.DID(cleanName)
	fixtures.User(t, s.db, cleanName+".test", clean)
	s.users.users[clean] = &users.User{DID: clean, Handle: cleanName + ".test"}

	consumer := NewCommunityEventConsumer(
		postgres.NewCommunityRepository(s.db, credentialciphertest.Fixed()), originTestInstance, true, nil)
	require.NoError(t, s.publishProfile(t, consumer, evader, evaderName, ""),
		"fixture: a DID with no Coves footprint indexes as a community")
	require.Equal(t, 1, s.communityRows(t, evader), "fixture: the DID is an indexed community")

	const evaderCID = "bafyreievaderpost"
	base := time.Now().UnixMicro()
	evaderPost := s.indexPost(t, evader, "evaderpost", evaderCID, base)
	s.admit(t, evaderPost, evaderCID, base+1_000_000)

	votes := NewVoteEventConsumer(postgres.NewVoteRepository(s.db), s.users, s.db)
	require.NoError(t, votes.HandleEvent(ctx, bhUpvoteEvent(evader, "evadervote", testkit.TID(), base+2_000_000, evaderPost, evaderCID)),
		"fixture: indexing the community DID's vote")
	require.Equal(t, 1, countRows(t, s.db, `SELECT COUNT(*) FROM votes WHERE voter_did = $1`, evader),
		"fixture: the community DID's vote is indexed")

	served := postgres.NewPostRepository(s.db, postgres.WithAccountRestrictionAuthority(instanceDID))
	before, err := served.VisibleHeaderView(ctx, evaderPost, viewer)
	require.NoError(t, err)
	require.NotNil(t, before, "control: the admitted post is served to another viewer before any ban")

	result, err := s.banAccount(t, evader, "evader-ban")
	require.NoError(t, err, "an indexed community DID with account activity is bannable as an account")
	require.NotNil(t, result)
	assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	assert.Equal(t, moderation.SubjectKind("account"), result.Action.SubjectKind)
	var storedKind string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT subject_kind FROM moderation_actions WHERE id::text = $1`, result.Action.ID).Scan(&storedKind))
	assert.Equal(t, "account", storedKind, "the ban is stored as an account-kind action")

	during, err := served.VisibleHeaderView(ctx, evaderPost, viewer)
	require.NoError(t, err)
	assert.Nil(t, during, "the banned community DID's post in C is hidden from another viewer")

	decider := newBanEvasionDecider(s)
	newPost := s.indexPost(t, evader, "evadernewpost", "bafyreievadernewpost", base+3_000_000)
	controlPost := s.indexPost(t, clean, "cleanpost", "bafyreicleanpost", base+4_000_000)
	controlDecision, err := decider.DecideAdmission(ctx, s.community, controlPost)
	require.NoError(t, err, "control: a clean author's post in C is decided")
	assert.True(t, controlDecision.Admitted(), "control: a clean author's post in C is admitted")
	_, err = decider.DecideAdmission(ctx, s.community, newPost)
	assert.ErrorIs(t, err, posts.ErrAuthorAccountBanned, "the banned community DID's new post is deferred at admission")

	_, err = s.moderation.UnbanAccount(ctx, selfIndexAdminDID, moderation.UnbanAccountRequest{
		Subject: evader, ExpectedVersion: result.State.Version, IdempotencyKey: "evader-unban",
		Reason: selfIndexRestoreNote,
	})
	require.NoError(t, err, "the account ban on the community DID is lifted")

	after, err := served.VisibleHeaderView(ctx, evaderPost, viewer)
	require.NoError(t, err)
	assert.NotNil(t, after, "the unbanned community DID's post is served again")
	afterDecision, err := decider.DecideAdmission(ctx, s.community, newPost)
	require.NoError(t, err, "after the unban the new post is decided")
	assert.True(t, afterDecision.Admitted(), "after the unban the new post is admitted")
}
