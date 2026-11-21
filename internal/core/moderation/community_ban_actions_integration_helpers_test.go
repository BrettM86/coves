//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

// Shared fixture for the community ban action tests against real Postgres:
// the community comment fixture (rosteralpha, owner O, moderators M and M2,
// the instance admin, the fake community repo, accepted posts, the production
// comment consumer and comment reads) with the ban limits at 60 per moderator
// and 300 per community per hour and the production ban lister. The banned
// account's posts, comments and votes go through the production post
// admission decider, comment and vote services, and comment and vote
// consumers. Outcomes are read back from the ban index, the hide reasons and
// counts, the moderation_actions rows and the (community, account) version.

const (
	// banDBRewrittenCID is the CID the fake repo gives a rewrite of
	// banSubject's ban record.
	banDBRewrittenCID = "bafyreiabanrewrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	banDBReasonDiscretion = "social.coves.moderation.defs#reasonModeratorDiscretion"
	banDBRewriteReason    = "Evading an earlier ban"

	banDBVoteCollection = "social.coves.feed.vote"
)

var (
	// banDBFarExpiry is safely after both the service clock and the wall
	// clock the comment and vote paths read.
	banDBFarExpiry = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	// banDBLapsedExpiry is before both clocks.
	banDBLapsedExpiry = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

type communityBanDBFixture struct {
	*communityCommentDBFixture
	bans *postgres.CommunityBanRepository
}

// newCommunityBanDBFixture extends newCommunityCommentDBFixture for ban
// actions: ban limits 60 per moderator and 300 per community per hour, the
// production ban lister, banSubject an indexed user with no role and no ban,
// and a create or update of banSubject's ban record gets banWrittenCID.
func newCommunityBanDBFixture(t *testing.T) *communityBanDBFixture {
	t.Helper()
	base := newCommunityCommentDBFixture(t)
	base.config.BanLimits = moderation.CommunityRemovalLimits{PerModerator: 60, PerCommunity: 300, Window: time.Hour}
	base.config.Bans = postgres.NewCommunityBanLister(base.db)
	base.service = moderation.NewCommunityActionService(base.config)
	base.indexUser(t, banSubject, "rosteroutsider.test")
	// The post admission decider reads rosteralpha through the community
	// repository, which decrypts its stored PDS tokens: store real
	// ciphertexts (rosteralpha stays hosted).
	require.NoError(t, postgres.NewCommunityRepository(base.db, credentialciphertest.Fixed()).
		UpdateCredentials(context.Background(), rosterCommunity, "access", "hosted"),
		"fixture: rosteralpha's PDS tokens as the repository stores them")
	base.repo.mu.Lock()
	base.repo.writeCIDs[communities.BanCollection+"/"+banSubjectKey] = banWrittenCID
	base.repo.mu.Unlock()

	f := &communityBanDBFixture{communityCommentDBFixture: base, bans: postgres.NewCommunityBanRepository(base.db)}
	require.EqualValues(t, 0, f.accountVersion(t, banSubject), "fixture: banSubject's version starts at zero")
	require.Nil(t, f.inForceBan(t, banSubject, rosterClockStart), "fixture: banSubject is not banned")
	return f
}

func banWith(service *moderation.CommunityActionService, actorDID, subjectDID, reason string, expiresAt *time.Time, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return service.BanCommunityAccount(context.Background(), moderation.BanCommunityAccountRequest{
		ActorDID: actorDID, CommunityDID: rosterCommunity, SubjectDID: subjectDID, Reason: reason,
		ExpiresAt: expiresAt, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityBanDBFixture) ban(actorDID, subjectDID, reason string, expiresAt *time.Time, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return banWith(f.service, actorDID, subjectDID, reason, expiresAt, expectedVersion, key)
}

func (f *communityBanDBFixture) unban(actorDID, subjectDID string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.UnbanCommunityAccount(context.Background(), moderation.UnbanCommunityAccountRequest{
		ActorDID: actorDID, CommunityDID: rosterCommunity, SubjectDID: subjectDID,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// accountVersion is the (rosteralpha, account) version getCommunityState
// reads for the account kind.
func (f *communityBanDBFixture) accountVersion(t *testing.T, accountDID string) int64 {
	t.Helper()
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(
		context.Background(), rosterCommunity, accountDID, moderation.CommunitySubjectAccount)
	require.NoError(t, err)
	return state.Version
}

// inForceBan is the account's rosteralpha ban in force at now, read through
// the ban index the enforcement paths read, or nil.
func (f *communityBanDBFixture) inForceBan(t *testing.T, accountDID string, now time.Time) *communities.CommunityBan {
	t.Helper()
	ban, err := communities.ActiveCommunityBan(context.Background(), f.bans, rosterCommunity, accountDID, now)
	require.NoError(t, err)
	if ban != nil {
		ban.IndexedAt = time.Time{}
		if ban.ExpiresAt != nil {
			utc := ban.ExpiresAt.UTC()
			ban.ExpiresAt = &utc
		}
	}
	return ban
}

// banRowExists reports whether the account has a rosteralpha ban row, in
// force or lapsed.
func (f *communityBanDBFixture) banRowExists(t *testing.T, accountDID string) bool {
	t.Helper()
	_, err := f.bans.GetCommunityBan(context.Background(), rosterCommunity, accountDID)
	if errors.Is(err, communities.ErrCommunityBanNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

// standBan indexes a ban on banSubject at banStandingCID with the given
// reason and expiry and puts the same record into the community repo.
func (f *communityBanDBFixture) standBan(t *testing.T, reason string, expiresAt *time.Time) {
	t.Helper()
	require.NoError(t, f.bans.UpsertCommunityBan(context.Background(), communities.CommunityBan{
		CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
		RecordCID: banStandingCID, Reason: reason, ExpiresAt: expiresAt,
	}), "fixture: index the standing ban")
	body := map[string]any{
		"$type": communities.BanCollection, "subject": banSubject, "createdAt": banStandingCreated,
	}
	if reason != "" {
		body["reason"] = reason
	}
	if expiresAt != nil {
		body["expiresAt"] = expiresAt.UTC().Format(time.RFC3339)
	}
	f.repo.setPostRecord(communities.BanCollection, banSubjectKey, banStandingCID, body)
}

// storedBanAction is one moderation_actions row about an account in
// rosteralpha.
type storedBanAction struct {
	ID                                         string
	Action, ActorDID                           string
	AdminActing                                bool
	AuthorityDID, ScopeKind, ScopeCommunityDID string
	SubjectURI, SubjectKind, SubjectCollection string
	Reason, Origin                             string
	ExpiresAt                                  *time.Time
	ReversesActionID                           string
	CreatedAt                                  time.Time
}

// banActions is rosteralpha's moderation_actions rows whose subject is
// accountDID, oldest first.
func (f *communityBanDBFixture) banActions(t *testing.T, accountDID string) []storedBanAction {
	t.Helper()
	return f.banActionsIn(t, rosterCommunity, accountDID)
}

// banActionsIn is communityDID's moderation_actions rows whose subject is
// accountDID, oldest first.
func (f *communityBanDBFixture) banActionsIn(t *testing.T, communityDID, accountDID string) []storedBanAction {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT id, action, COALESCE(actor_did, ''), admin_acting, COALESCE(authority_did, ''), scope_kind,
		       COALESCE(scope_community_did, ''), subject_uri, COALESCE(subject_kind, ''), COALESCE(subject_collection, ''),
		       COALESCE(reason, ''), origin, expires_at, COALESCE(reverses_action_id, ''), created_at
		FROM moderation_actions
		WHERE scope_community_did = $1 AND subject_uri = 'at://' || $2
		ORDER BY created_at, id
	`, communityDID, accountDID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []storedBanAction{}
	for rows.Next() {
		var action storedBanAction
		var expiresAt sql.NullTime
		require.NoError(t, rows.Scan(&action.ID, &action.Action, &action.ActorDID, &action.AdminActing,
			&action.AuthorityDID, &action.ScopeKind, &action.ScopeCommunityDID, &action.SubjectURI,
			&action.SubjectKind, &action.SubjectCollection, &action.Reason, &action.Origin, &expiresAt,
			&action.ReversesActionID, &action.CreatedAt))
		if expiresAt.Valid {
			utc := expiresAt.Time.UTC()
			action.ExpiresAt = &utc
		}
		action.CreatedAt = action.CreatedAt.UTC()
		out = append(out, action)
	}
	require.NoError(t, rows.Err())
	return out
}

// banActionRowsText is every rosteralpha moderation_actions row about
// accountDID rendered as JSON, to check what text an action carries.
func (f *communityBanDBFixture) banActionRowsText(t *testing.T, accountDID string) string {
	t.Helper()
	var text sql.NullString
	require.NoError(t, f.db.QueryRow(`
		SELECT string_agg(row_to_json(a)::text, ' ') FROM moderation_actions a
		WHERE scope_community_did = $1 AND subject_uri = 'at://' || $2
	`, rosterCommunity, accountDID).Scan(&text))
	return text.String
}

// localBanAction is the action a local ban (remove) or unban (restore) of
// banSubject by actorDID at createdAt writes, without its ID or reversal.
func localBanAction(action, actorDID string, adminActing bool, expiresAt *time.Time, createdAt time.Time) storedBanAction {
	return storedBanAction{
		Action: action, ActorDID: actorDID, AdminActing: adminActing,
		AuthorityDID: rosterCommunity, ScopeKind: "community", ScopeCommunityDID: rosterCommunity,
		SubjectURI: "at://" + banSubject, SubjectKind: "account",
		Reason: banDBReasonDiscretion, Origin: "local", ExpiresAt: expiresAt, CreatedAt: createdAt,
	}
}

// withoutIDs is actions with ID and ReversesActionID cleared, for comparing
// the rest of each row.
func withoutIDs(actions []storedBanAction) []storedBanAction {
	out := make([]storedBanAction, len(actions))
	for i, action := range actions {
		action.ID, action.ReversesActionID = "", ""
		out[i] = action
	}
	return out
}

// banDBCommunityLookup is the admission decider's community lookup over the
// indexed communities table; the decider is handed community DIDs.
type banDBCommunityLookup struct {
	repository communities.Repository
}

func (lookup banDBCommunityLookup) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	return identifier, nil
}

func (lookup banDBCommunityLookup) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	return lookup.repository.GetByDID(ctx, did)
}

// admissionDecider is the production post admission decider over the
// fixture's database, reading the ban index on the wall clock; trusted names
// the trusted aggregators.
func (f *communityBanDBFixture) admissionDecider(trusted map[string]bool) *posts.AdmissionEngineDecider {
	return posts.NewAdmissionEngineDecider(posts.DeciderDeps{
		Posts:       postgres.NewPostRepository(f.db),
		Communities: banDBCommunityLookup{repository: postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed())},
		Policy: posts.AdmissionPolicy{
			Ledger:      postgres.NewSubmissionLedger(f.db),
			Bans:        postgres.NewCommunityBanRepository(f.db),
			AccountBans: postgres.NewAccountBanReader(f.db, rosterInstanceDID),
			Now:         time.Now,
		},
		TrustedAggregatorDIDs: trusted,
	})
}

// indexPostBy indexes authorDID's post at rkey in rosteralpha, unaccepted.
func (f *communityBanDBFixture) indexPostBy(t *testing.T, authorDID, rkey string) string {
	t.Helper()
	uri := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + rkey
	_, err := f.db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, uri, "bafyreipost"+rkey, rkey, authorDID, rosterCommunity, "post "+rkey)
	require.NoError(t, err, "fixture: index post %s", rkey)
	return uri
}

// decidePost is the admission decider's decision on authorDID's new post at
// rkey in rosteralpha.
func (f *communityBanDBFixture) decidePost(t *testing.T, trusted map[string]bool, authorDID, rkey string) posts.AdmissionDecision {
	t.Helper()
	uri := f.indexPostBy(t, authorDID, rkey)
	decision, err := f.admissionDecider(trusted).DecideAdmission(context.Background(), rosterCommunity, uri)
	require.NoError(t, err, "the post %s is decided", rkey)
	return decision
}

// refusingPDS stands in for an account's own PDS on the comment and vote
// write paths: it records every call and fails it, so a refusal before any
// write leaves it empty.
type refusingPDS struct {
	mu    sync.Mutex
	did   string
	calls []string
}

var errRefusingPDS = errors.New("refusingPDS: the write reached the account's PDS")

func (r *refusingPDS) record(call string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	return errRefusingPDS
}

func (r *refusingPDS) callsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *refusingPDS) CreateRecord(_ context.Context, collection, _ string, _ any) (string, string, error) {
	return "", "", r.record("createRecord " + collection)
}

func (r *refusingPDS) DeleteRecord(_ context.Context, collection, _ string) error {
	return r.record("deleteRecord " + collection)
}

func (r *refusingPDS) ListRecords(_ context.Context, collection string, _ int, _ string) (*pds.ListRecordsResponse, error) {
	return nil, r.record("listRecords " + collection)
}

func (r *refusingPDS) GetRecord(_ context.Context, collection, _ string) (*pds.RecordResponse, error) {
	return nil, r.record("getRecord " + collection)
}

func (r *refusingPDS) PutRecord(_ context.Context, collection, _ string, _ any, _ string) (string, string, error) {
	return "", "", r.record("putRecord " + collection)
}

func (r *refusingPDS) UploadBlob(context.Context, []byte, string) (*blobs.BlobRef, error) {
	return nil, r.record("uploadBlob")
}

func (r *refusingPDS) ApplyWrites(context.Context, []pds.Write, string) (*pds.ApplyWritesResult, error) {
	return nil, r.record("applyWrites")
}

func (r *refusingPDS) DID() string     { return r.did }
func (r *refusingPDS) HostURL() string { return "https://pds.invalid" }

func banDBSession(t *testing.T, did string) *oauthlib.ClientSessionData {
	t.Helper()
	parsed, err := syntax.ParseDID(did)
	require.NoError(t, err)
	return &oauthlib.ClientSessionData{AccountDID: parsed, AccessToken: "test-access-token"}
}

// createComment asks the production comment service, wired to the community
// ban index, for accountDID's top-level comment on the post; the account's
// PDS refuses every write.
func (f *communityBanDBFixture) createComment(t *testing.T, accountDID string, post seededCommunityPost) (*refusingPDS, error) {
	t.Helper()
	account := &refusingPDS{did: accountDID}
	service := comments.NewCommentServiceWithPDSFactory(
		postgres.NewCommentRepository(f.db), nil, postgres.NewPostRepository(f.db), nil, nil,
		func(context.Context, *oauthlib.ClientSessionData) (pds.Client, error) { return account, nil },
		comments.WithCommunityBans(postgres.NewCommunityBanRepository(f.db)),
	)
	_, err := service.CreateComment(context.Background(), banDBSession(t, accountDID), comments.CreateCommentRequest{
		Reply: comments.ReplyRef{
			Root:   comments.StrongRef{URI: post.URI, CID: post.CID},
			Parent: comments.StrongRef{URI: post.URI, CID: post.CID},
		},
		Content: "a comment in rosteralpha",
	})
	return account, err
}

// createVote asks the production vote service, wired to the community ban
// index, for accountDID's upvote on the post; the account's PDS refuses every
// write.
func (f *communityBanDBFixture) createVote(t *testing.T, accountDID string, post seededCommunityPost) (*refusingPDS, error) {
	t.Helper()
	account := &refusingPDS{did: accountDID}
	service := votes.NewServiceWithPDSFactory(postgres.NewVoteRepository(f.db), nil,
		func(context.Context, *oauthlib.ClientSessionData) (votes.PDSClient, error) { return account, nil },
		votes.WithCommunityBans(postgres.NewCommunityBanRepository(f.db)))
	_, err := service.CreateVote(context.Background(), banDBSession(t, accountDID), votes.CreateVoteRequest{
		Subject:   votes.StrongRef{URI: post.URI, CID: post.CID},
		Direction: "up",
	})
	return account, err
}

// indexCommentBy hands accountDID's top-level comment on the post, written
// straight to its PDS, to the production comment consumer, and returns its
// URI.
func (f *communityBanDBFixture) indexCommentBy(t *testing.T, accountDID, rkey string, post seededCommunityPost) string {
	t.Helper()
	require.NoError(t, f.comments.HandleEvent(context.Background(), &jetstream.JetstreamEvent{
		Kind: "commit", Did: accountDID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: "3kbancommentrev1", Operation: "create", Collection: moderation.CommentCollection,
			RKey: rkey, CID: "bafyreicomment" + rkey,
			Record: map[string]any{
				"$type":   moderation.CommentCollection,
				"content": "a comment by " + accountDID,
				"reply": map[string]any{
					"root":   map[string]any{"uri": post.URI, "cid": post.CID},
					"parent": map[string]any{"uri": post.URI, "cid": post.CID},
				},
				"createdAt": "2026-11-13T12:00:00Z",
			},
		},
	}), "indexing comment %s", rkey)
	return "at://" + accountDID + "/" + moderation.CommentCollection + "/" + rkey
}

// indexUpvoteBy hands accountDID's upvote on the post, written straight to
// its PDS, to the production vote consumer, and returns its URI.
func (f *communityBanDBFixture) indexUpvoteBy(t *testing.T, accountDID, rkey string, post seededCommunityPost) string {
	t.Helper()
	consumer := jetstream.NewVoteEventConsumer(postgres.NewVoteRepository(f.db), nil, f.db)
	require.NoError(t, consumer.HandleEvent(context.Background(), &jetstream.JetstreamEvent{
		Kind: "commit", Did: accountDID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: "3kbanvoterev1", Operation: "create", Collection: banDBVoteCollection,
			RKey: rkey, CID: "bafyreivote" + rkey,
			Record: map[string]any{
				"$type":     banDBVoteCollection,
				"subject":   map[string]any{"uri": post.URI, "cid": post.CID},
				"direction": "up",
				"createdAt": "2026-11-13T12:00:00Z",
			},
		},
	}), "indexing vote %s", rkey)
	return "at://" + accountDID + "/" + banDBVoteCollection + "/" + rkey
}

// hideReason is the hide_reason of the indexed comment or vote at uri in
// table ("" when served).
func (f *communityBanDBFixture) hideReason(t *testing.T, table, uri string) string {
	t.Helper()
	var reason sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT hide_reason FROM `+table+` WHERE uri = $1`, uri).Scan(&reason),
		"read the hide reason of %s", uri)
	return reason.String
}

// postCounts is the post's indexed comment and upvote counts.
func (f *communityBanDBFixture) postCounts(t *testing.T, postURI string) (commentCount, upvoteCount int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT comment_count, upvote_count FROM posts WHERE uri = $1`, postURI).
		Scan(&commentCount, &upvoteCount))
	return commentCount, upvoteCount
}

// threadServes reports whether getComments serves commentURI in the post's
// thread to an anonymous viewer.
func (f *communityBanDBFixture) threadServes(t *testing.T, postURI, commentURI string) bool {
	t.Helper()
	response, err := f.reads.GetComments(context.Background(), &comments.GetCommentsRequest{
		PostURI: postURI, Sort: "new", Depth: 5, Limit: 50,
	})
	require.NoError(t, err)
	return findThreadComment(response.Comments, commentURI) != nil
}

const (
	// banLocalBodyCID is the CID a PDS assigns the ban record M writes for
	// banSubject at rosterClockStart with banReason and banDBFarExpiry:
	// {"$type":"social.coves.moderation.ban","createdAt":"2026-11-13T12:00:00Z",
	// "expiresAt":"2099-01-01T00:00:00Z","reason":"Spamming invite links",
	// "subject":"did:plc:rosteroutsider"} (DAG-CBOR, CIDv1, sha256), computed
	// once with indigo's atdata.MarshalCBOR and go-cid outside the code under
	// test, as postDBAlphaRemovalBodyCID was.
	banLocalBodyCID = "bafyreih563xxr5sch2bvqhap6wg25brp2x7tgoynqvcwfxbdeextynuxzi"

	// banLocalCreatedAt is the createdAt of a ban whose intent was created at
	// rosterClockStart.
	banLocalCreatedAt = "2026-11-13T12:00:00Z"

	// banExternalCID is the CID of a ban on banSubject written outside Coves.
	banExternalCID    = "bafyreiabanexternalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	banExternalReason = "Raiding threads"
)

// setBanWriteCID is the CID the fake repo gives the next create or update of
// banSubject's ban record.
func (f *communityBanDBFixture) setBanWriteCID(cid string) {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.writeCIDs[communities.BanCollection+"/"+banSubjectKey] = cid
}

// loseNextResponse makes the fake repo commit the next batch and lose its
// response.
func (f *communityBanDBFixture) loseNextResponse() {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
}

// accountVersionIn is the (communityDID, account) version getCommunityState
// reads for the account kind.
func (f *communityBanDBFixture) accountVersionIn(t *testing.T, communityDID, accountDID string) int64 {
	t.Helper()
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(
		context.Background(), communityDID, accountDID, moderation.CommunitySubjectAccount)
	require.NoError(t, err)
	return state.Version
}

// banRowIn is the account's ban row in communityDID, in force or lapsed, or
// nil, with IndexedAt cleared and the expiry in UTC.
func (f *communityBanDBFixture) banRowIn(t *testing.T, communityDID, accountDID string) *communities.CommunityBan {
	t.Helper()
	ban, err := f.bans.GetCommunityBan(context.Background(), communityDID, accountDID)
	if errors.Is(err, communities.ErrCommunityBanNotFound) {
		return nil
	}
	require.NoError(t, err)
	require.NotNil(t, ban)
	ban.IndexedAt = time.Time{}
	if ban.ExpiresAt != nil {
		utc := ban.ExpiresAt.UTC()
		ban.ExpiresAt = &utc
	}
	return ban
}

// banFirehose is the production consumer of community ban records (the post
// consumer with the ban index, as cmd/server wires it) over the fixture's
// database, with the real rev gate.
func (f *communityBanDBFixture) banFirehose() *jetstream.PostEventConsumer {
	return jetstream.NewPostEventConsumer(postgres.NewPostRepository(f.db),
		postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed()), nil, f.db,
		jetstream.WithAdmissions(postgres.NewAdmissionRepository(f.db)),
		jetstream.WithPostCommunityCommentRemovals(postgres.NewCommunityCommentRemovalRepository(f.db)),
		jetstream.WithPostCommunityBans(postgres.NewCommunityBanRepository(f.db)),
		jetstream.WithDeletedAccounts(postgres.NewDeletedAccountRepository(f.db)))
}

// deliverBan hands one commit of banSubject's ban record in communityDID's
// repo to the ban consumer; a delete carries no CID or record.
func (f *communityBanDBFixture) deliverBan(t *testing.T, communityDID, op, rev, cid string, record map[string]any) {
	t.Helper()
	require.NoError(t, f.banFirehose().HandleEvent(context.Background(), &jetstream.JetstreamEvent{
		Kind: "commit", Did: communityDID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: rev, Operation: op, Collection: communities.BanCollection, RKey: banSubjectKey, CID: cid, Record: record,
		},
	}), "the ban consumer handles the %s at %s", op, rev)
}

// banRecordBody is a ban record on banSubject.
func banRecordBody(reason string, expiresAt *time.Time, createdAt string) map[string]any {
	body := map[string]any{"$type": communities.BanCollection, "subject": banSubject, "createdAt": createdAt}
	if reason != "" {
		body["reason"] = reason
	}
	if expiresAt != nil {
		body["expiresAt"] = expiresAt.UTC().Format(time.RFC3339)
	}
	return body
}

// externalBanAction is the action a ban (remove) or its delete (restore)
// written outside Coves records in communityDID: no actor, origin inherited,
// the moderator-discretion reason.
func externalBanAction(communityDID, action string, expiresAt *time.Time) storedBanAction {
	return storedBanAction{
		Action: action, AuthorityDID: communityDID, ScopeKind: "community", ScopeCommunityDID: communityDID,
		SubjectURI: "at://" + banSubject, SubjectKind: "account",
		Reason: banDBReasonDiscretion, Origin: "inherited", ExpiresAt: expiresAt,
	}
}

// withoutIDsOrTimes is withoutIDs with CreatedAt cleared as well.
func withoutIDsOrTimes(actions []storedBanAction) []storedBanAction {
	out := withoutIDs(actions)
	for i := range out {
		out[i].CreatedAt = time.Time{}
	}
	return out
}

// banActionIDs is the IDs of actions, in order.
func banActionIDs(actions []storedBanAction) []string {
	ids := make([]string, len(actions))
	for i, action := range actions {
		ids[i] = action.ID
	}
	return ids
}

// banActionRowsTextIn is banActionRowsText in communityDID.
func (f *communityBanDBFixture) banActionRowsTextIn(t *testing.T, communityDID, accountDID string) string {
	t.Helper()
	var text sql.NullString
	require.NoError(t, f.db.QueryRow(`
		SELECT string_agg(row_to_json(a)::text, ' ') FROM moderation_actions a
		WHERE scope_community_did = $1 AND subject_uri = 'at://' || $2
	`, communityDID, accountDID).Scan(&text))
	return text.String
}
