//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

// Shared fixture for the community post action tests against real Postgres:
// the community action fixture (rosteralpha, owner O, moderator M, the
// instance admin, the fake community repo), plus a second moderator M2, the
// production post resolver, and posts indexed and accepted through the real
// admission producer. Outcomes are read back through the production read paths:
// the admission row, the author's own post view, the community feed, the
// moderation_actions rows and the post subject's community version.

const (
	postDBAuthor = "did:plc:rosterauthor"

	// postDBSeededVersion is a post's community version once its acceptance
	// is indexed (the acceptance is its first community record change).
	postDBSeededVersion = 1

	postDBRemovalReason = "Selling followers"
	postDBReasonSpam    = "social.coves.moderation.defs#reasonSpam"
)

// postDBSeed is one post to index and accept: its rkey in the author's repo,
// its CID, the CID of its standing acceptance record, and the CIDs the fake
// repo gives a removal or acceptance written for it.
type postDBSeed struct {
	RKey, CID, AcceptanceCID, RemovalWrittenCID, AcceptanceWrittenCID string
}

// postDBAlpha is postAlphaURI with the T0 contract's CIDs.
var postDBAlpha = postDBSeed{
	RKey: "3kpostalpha1", CID: postAlphaCID, AcceptanceCID: postAcceptanceCID,
	RemovalWrittenCID: postRemovalWrittenCID, AcceptanceWrittenCID: postAcceptanceWrittenCID,
}

// postDBSeedFor is a further post at rkey with CIDs named after it.
func postDBSeedFor(rkey string) postDBSeed {
	return postDBSeed{
		RKey: rkey, CID: "bafyreipost" + rkey, AcceptanceCID: "bafyreiacceptance" + rkey,
		RemovalWrittenCID: "bafyreiremovalwritten" + rkey, AcceptanceWrittenCID: "bafyreiacceptancewritten" + rkey,
	}
}

// seededCommunityPost is an indexed, accepted post in rosteralpha.
type seededCommunityPost struct {
	URI, CID      string
	SubjectRkey   string // the stand records' key in the community repo
	AcceptanceURI string
	RemovalURI    string
}

type communityPostDBFixture struct {
	*communityActionDBFixture
	postRepo *postgres.PostRepository
	feeds    feedgen.Service
}

// newCommunityPostDBFixture extends newCommunityActionDBFixture for post
// actions: M2 (rosterSecondModerator) is a second moderator of rosteralpha,
// the service resolves posts with postgres.NewCommunityPostResolver and limits
// removals to 60 per moderator and 300 per community per hour. No post is
// seeded.
func newCommunityPostDBFixture(t *testing.T) *communityPostDBFixture {
	t.Helper()
	base := newCommunityActionDBFixture(t)
	role := rosterRole(rosterCommunity, rosterSecondModerator, communities.RoleModerator,
		rosterSecondModeratorKey, rosterSecondModeratorCID, time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC))
	_, err := base.db.Exec(`
		INSERT INTO community_roles (community_did, subject_did, role, record_key, record_cid, record_created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, role.CommunityDID, role.SubjectDID, role.Role, role.RecordKey, role.RecordCID, role.RecordCreatedAt)
	require.NoError(t, err)

	base.config.Posts = postgres.NewCommunityPostResolver(base.db)
	base.config.RemovalLimits = moderation.CommunityRemovalLimits{PerModerator: 60, PerCommunity: 300, Window: time.Hour}
	base.service = moderation.NewCommunityActionService(base.config)

	_, err = base.authorizer.Authorize(context.Background(), rosterCommunity, rosterSecondModerator, communities.RoleModerator)
	require.NoError(t, err, "fixture: M2 must be authorized as moderator")
	return &communityPostDBFixture{
		communityActionDBFixture: base,
		postRepo:                 postgres.NewPostRepository(base.db, postgres.WithAccountRestrictionAuthority(rosterInstanceDID)),
		feeds:                    communityNewFeed(base.db),
	}
}

// serviceWithLimits is a service over the fixture's store and repo with the
// given removal limits.
func (f *communityPostDBFixture) serviceWithLimits(limits moderation.CommunityRemovalLimits) *moderation.CommunityActionService {
	config := f.config
	config.RemovalLimits = limits
	return moderation.NewCommunityActionService(config)
}

// seedAcceptedPost indexes the author's post in rosteralpha and accepts it
// through the admission producer at head zero's rev; the acceptance record
// stands in the community repo, and the fake repo has CIDs for a removal or
// acceptance written for the post.
func (f *communityPostDBFixture) seedAcceptedPost(t *testing.T, seed postDBSeed) seededCommunityPost {
	t.Helper()
	post := f.indexAcceptedPost(t, rosterCommunity, seed)

	f.repo.setPostRecord(posts.AcceptanceCollection, post.SubjectRkey, seed.AcceptanceCID, map[string]any{
		"$type":     posts.AcceptanceCollection,
		"subject":   map[string]any{"uri": post.URI, "cid": post.CID},
		"createdAt": postStandingCreatedAt,
	})
	f.repo.mu.Lock()
	f.repo.writeCIDs[posts.RemovalCollection+"/"+post.SubjectRkey] = seed.RemovalWrittenCID
	f.repo.writeCIDs[posts.AcceptanceCollection+"/"+post.SubjectRkey] = seed.AcceptanceWrittenCID
	f.repo.mu.Unlock()

	require.Equal(t, posts.AdmissionStatusAccepted, f.admission(t, post.URI).Status, "fixture: the post is accepted")
	require.EqualValues(t, postDBSeededVersion, f.postVersion(t, post.URI), "fixture: the post's community version")
	return post
}

// indexAcceptedPost indexes the author's post in communityDID and accepts it
// through the admission producer at head zero's rev. It touches no PDS repo.
func (f *communityPostDBFixture) indexAcceptedPost(t *testing.T, communityDID string, seed postDBSeed) seededCommunityPost {
	t.Helper()
	ctx := context.Background()
	post := seededCommunityPost{
		URI: "at://" + postDBAuthor + "/" + moderation.PostV2Collection + "/" + seed.RKey,
		CID: seed.CID,
	}
	post.SubjectRkey = posts.SubjectRkey(post.URI)
	post.AcceptanceURI = "at://" + communityDID + "/" + posts.AcceptanceCollection + "/" + post.SubjectRkey
	post.RemovalURI = "at://" + communityDID + "/" + posts.RemovalCollection + "/" + post.SubjectRkey

	_, err := f.db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, post.URI, post.CID, seed.RKey, postDBAuthor, communityDID, "post "+seed.RKey)
	require.NoError(t, err)
	result, err := postgres.NewAdmissionRepository(f.db).ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID: communityDID, PostURI: post.URI,
		AcceptanceURI: post.AcceptanceURI, AcceptanceRkey: post.SubjectRkey, PinnedCID: post.CID,
		Watermark: posts.CommunityWatermark{Rev: rosterHeadZeroRev, OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: the acceptance applies")
	return post
}

// setCommitHead is the head the fake repo's next successful batch commits at.
func (f *communityPostDBFixture) setCommitHead(head pds.LatestCommit) {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.commitHead = head
}

func removePostWith(service *moderation.CommunityActionService, actorDID, postURI, code, reason string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return service.RemovePost(context.Background(), moderation.RemovePostRequest{
		ActorDID: actorDID, PostURI: postURI, Code: code, Reason: reason,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityPostDBFixture) removePost(actorDID, postURI, code, reason string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return removePostWith(f.service, actorDID, postURI, code, reason, expectedVersion, key)
}

func (f *communityPostDBFixture) restorePost(actorDID, postURI string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RestorePost(context.Background(), moderation.RestorePostRequest{
		ActorDID: actorDID, PostURI: postURI, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// admission is the post's rosteralpha admission row.
func (f *communityPostDBFixture) admission(t *testing.T, postURI string) *posts.Admission {
	t.Helper()
	return f.admissionIn(t, rosterCommunity, postURI)
}

// admissionIn is the post's admission row in communityDID.
func (f *communityPostDBFixture) admissionIn(t *testing.T, communityDID, postURI string) *posts.Admission {
	t.Helper()
	admission, err := postgres.NewAdmissionRepository(f.db).Get(context.Background(), communityDID, postURI)
	require.NoError(t, err)
	require.NotNil(t, admission, "the post has an admission row")
	return admission
}

// postVersion is the post subject's community version as getCommunityState
// reads it.
func (f *communityPostDBFixture) postVersion(t *testing.T, postURI string) int64 {
	t.Helper()
	return f.postVersionIn(t, rosterCommunity, postURI)
}

// postVersionIn is the post subject's version in communityDID.
func (f *communityPostDBFixture) postVersionIn(t *testing.T, communityDID, postURI string) int64 {
	t.Helper()
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(
		context.Background(), communityDID, postURI, moderation.CommunitySubjectPost)
	require.NoError(t, err)
	return state.Version
}

// authorView is the author's own post.get view of the post (GetViewsByURIs,
// the authorPostViewSelectColumns read path), or nil when none is served.
func (f *communityPostDBFixture) authorView(t *testing.T, postURI string) *posts.PostView {
	t.Helper()
	views, err := f.postRepo.GetViewsByURIs(context.Background(), []string{postURI}, postDBAuthor)
	require.NoError(t, err)
	return views[postURI]
}

// communityNewFeed is getFeed over the production new generator and post
// hydrator, with the instance's account restriction, as the public community
// feed serves sort new.
func communityNewFeed(db *sql.DB) feedgen.Service {
	restriction := postgres.WithAccountRestrictionAuthority(rosterInstanceDID)
	registry := feedgen.NewRegistry(rosterInstanceDID, feedgen.NewBuiltin(postgres.NewNewGenerator(db, restriction)))
	return feedgen.NewService(registry, postgres.NewPostHydrator(db, restriction), postgres.NewCursorSigner("test-secret"))
}

// feedURIs is rosteralpha's public community feed, newest first.
func (f *communityPostDBFixture) feedURIs(t *testing.T) []string {
	t.Helper()
	feed, err := f.feeds.GetFeed(context.Background(), feedgen.GetFeedRequest{
		Scope: feedgen.Scope{Name: feedgen.ScopeCommunity, CommunityDID: rosterCommunity},
		Feed:  feedgen.NewRegistry(rosterInstanceDID).URI("new"), Limit: 50,
	})
	require.NoError(t, err)
	out := []string{}
	for _, item := range feed.Feed {
		out = append(out, item.Post.URI)
	}
	return out
}

// storedPostAction is one moderation_actions row about a post in rosteralpha.
type storedPostAction struct {
	Action, ActorDID              string
	AdminActing                   bool
	ScopeKind, ScopeCommunityDID  string
	SubjectURI, SubjectCollection string
	Reason, Origin                string
}

// postActions is rosteralpha's moderation_actions rows whose subject is a
// post, oldest first.
func (f *communityPostDBFixture) postActions(t *testing.T) []storedPostAction {
	t.Helper()
	return f.postActionsIn(t, rosterCommunity)
}

// postActionsIn is communityDID's moderation_actions rows whose subject is a
// post, oldest first.
func (f *communityPostDBFixture) postActionsIn(t *testing.T, communityDID string) []storedPostAction {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT action, COALESCE(actor_did, ''), admin_acting, scope_kind, COALESCE(scope_community_did, ''),
		       subject_uri, COALESCE(subject_collection, ''), COALESCE(reason, ''), origin
		FROM moderation_actions
		WHERE scope_community_did = $1 AND subject_uri LIKE 'at://%/' || $2 || '/%'
		ORDER BY created_at, id
	`, communityDID, moderation.PostV2Collection)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []storedPostAction{}
	for rows.Next() {
		var action storedPostAction
		require.NoError(t, rows.Scan(&action.Action, &action.ActorDID, &action.AdminActing, &action.ScopeKind,
			&action.ScopeCommunityDID, &action.SubjectURI, &action.SubjectCollection, &action.Reason, &action.Origin))
		out = append(out, action)
	}
	require.NoError(t, rows.Err())
	return out
}

// localPostAction is the action a local remove or restore of postURI by
// actorDID writes.
func localPostAction(action, actorDID string, adminActing bool, postURI, reason string) storedPostAction {
	return storedPostAction{
		Action: action, ActorDID: actorDID, AdminActing: adminActing,
		ScopeKind: "community", ScopeCommunityDID: rosterCommunity,
		SubjectURI: postURI, SubjectCollection: "social.coves.community.postv2",
		Reason: reason, Origin: "local",
	}
}

// postConsumer is the production post consumer over the fixture's database,
// wired for community-repo acceptance and removal events, including removals
// whose subject is a comment.
func (f *communityPostDBFixture) postConsumer() *jetstream.PostEventConsumer {
	return postConsumerOn(f.db)
}

func postConsumerOn(db *sql.DB) *jetstream.PostEventConsumer {
	return jetstream.NewPostEventConsumer(postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), nil, db,
		jetstream.WithAdmissions(postgres.NewAdmissionRepository(db)),
		jetstream.WithPostCommunityCommentRemovals(postgres.NewCommunityCommentRemovalRepository(db)),
		jetstream.WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)))
}

// deliverPost hands one community-repo commit to the post consumer.
func (f *communityPostDBFixture) deliverPost(event *jetstream.JetstreamEvent) error {
	return f.postConsumer().HandleEvent(context.Background(), event)
}

// postStandEvent is a rosteralpha commit of an acceptance or removal record;
// a delete carries no CID or record.
func postStandEvent(op, collection, rkey, rev, cid string, record map[string]any) *jetstream.JetstreamEvent {
	return postStandEventIn(rosterCommunity, op, collection, rkey, rev, cid, record)
}

// postStandEventIn is postStandEvent in communityDID's repo.
func postStandEventIn(communityDID, op, collection, rkey, rev, cid string, record map[string]any) *jetstream.JetstreamEvent {
	return &jetstream.JetstreamEvent{
		Kind: "commit", Did: communityDID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: rev, Operation: op, Collection: collection, RKey: rkey, CID: cid, Record: record,
		},
	}
}

// removalRecordBody is a removal record of post at its CID.
func removalRecordBody(post seededCommunityPost, code, reason, createdAt string) map[string]any {
	record := map[string]any{
		"$type":     posts.RemovalCollection,
		"subject":   map[string]any{"uri": post.URI, "cid": post.CID},
		"code":      code,
		"createdAt": createdAt,
	}
	if reason != "" {
		record["reason"] = reason
	}
	return record
}

// banAccount bans accountDID at rosteralpha's instance through the real
// instance moderation service.
func (f *communityPostDBFixture) banAccount(t *testing.T, accountDID string) {
	t.Helper()
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db)),
		postgres.NewModerationRepository(f.db),
		moderation.Config{
			InstanceDID: rosterInstanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
			Communities: postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed()),
		},
	)
	result, err := service.BanAccount(context.Background(), rosterAdmin, moderation.BanAccountRequest{
		Subject: accountDID, ExpectedVersion: "v0", IdempotencyKey: "post-db-ban-" + accountDID,
		Reason: postDBReasonSpam,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome, "fixture: the ban applies")
	_, err = f.authorizer.Authorize(context.Background(), rosterCommunity, accountDID, communities.RoleModerator)
	require.Error(t, err, "fixture: the authorizer refuses the banned account")
}

const (
	// postDBAlphaRemovalBodyCID is the CID the PDS assigns the removal a
	// moderator writes for postDBAlpha at rosterClockStart: removalRecordBody(
	// post, "spam", postDBRemovalReason, "2026-11-13T12:00:00Z") (DAG-CBOR,
	// CIDv1, sha256), computed once with indigo's atdata.MarshalCBOR and go-cid
	// outside the code under test.
	postDBAlphaRemovalBodyCID = "bafyreiersg6fzu5qyiuj56mkrkvrkm6gtu7fbfsfrjnk53kimhjmsh7hke"

	// postDBIntentCreatedAt is the createdAt of a removal whose intent was
	// created at rosterClockStart.
	postDBIntentCreatedAt = "2026-11-13T12:00:00Z"

	postDBReasonRuleViolation = "social.coves.moderation.defs#reasonRuleViolation"
)

// setRemovalWriteCID is the CID the fake repo gives the next create or update
// of post's removal record.
func (f *communityPostDBFixture) setRemovalWriteCID(post seededCommunityPost, cid string) {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.writeCIDs[posts.RemovalCollection+"/"+post.SubjectRkey] = cid
}

// standExternalRemoval puts a removal of post written outside Coves into the
// community repo (the acceptance deleted, the head at head) and hands the
// commit's two events to the post consumer at head's rev, so the index and
// the PDS agree.
func (f *communityPostDBFixture) standExternalRemoval(t *testing.T, post seededCommunityPost, head pds.LatestCommit, cid string, body map[string]any) {
	t.Helper()
	f.repo.setPostRecord(posts.RemovalCollection, post.SubjectRkey, cid, body)
	f.repo.mu.Lock()
	delete(f.repo.records, posts.AcceptanceCollection+"/"+post.SubjectRkey)
	f.repo.mu.Unlock()
	f.repo.landCommit(head)
	f.deliverRemovalCommit(t, post, head.Rev, cid, body)
}

// deliverRemovalCommit hands a removal commit's firehose events to the post
// consumer: the removal create at rev with cid and body, then the acceptance
// delete at rev.
func (f *communityPostDBFixture) deliverRemovalCommit(t *testing.T, post seededCommunityPost, rev, cid string, body map[string]any) {
	t.Helper()
	require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, post.SubjectRkey, rev, cid, body)))
	require.NoError(t, f.deliverPost(postStandEvent("delete", posts.AcceptanceCollection, post.SubjectRkey, rev, "", nil)))
}

// openIntents is how many of rosteralpha's intents are still open.
func (f *communityPostDBFixture) openIntents(t *testing.T) int {
	t.Helper()
	open := 0
	for _, intent := range f.intents(t) {
		if intent.State == "open" {
			open++
		}
	}
	return open
}

// externalPostAction is the action a removal or restore of postURI written
// outside Coves records: no actor, origin inherited.
func externalPostAction(action, postURI, reason string) storedPostAction {
	return externalPostActionIn(rosterCommunity, action, postURI, reason)
}

// externalPostActionIn is externalPostAction in communityDID.
func externalPostActionIn(communityDID, action, postURI, reason string) storedPostAction {
	return storedPostAction{
		Action: action, ScopeKind: "community", ScopeCommunityDID: communityDID,
		SubjectURI: postURI, SubjectCollection: "social.coves.community.postv2",
		Reason: reason, Origin: "inherited",
	}
}

// postDBUnhostedCommunity is a community this instance indexes but does not
// host (no stored PDS refresh token).
const postDBUnhostedCommunity = "did:plc:rosterbridged"

// seedUnhostedCommunityPost indexes postDBUnhostedCommunity and the author's
// post in it, accepted through the admission producer at head zero's rev.
func (f *communityPostDBFixture) seedUnhostedCommunityPost(t *testing.T, seed postDBSeed) seededCommunityPost {
	t.Helper()
	insertRosterCommunity(t, f.db, postDBUnhostedCommunity, "rosterbridged.test", false)
	post := f.indexAcceptedPost(t, postDBUnhostedCommunity, seed)
	require.Equal(t, posts.AdmissionStatusAccepted, f.admissionIn(t, postDBUnhostedCommunity, post.URI).Status,
		"fixture: the post is accepted")
	require.EqualValues(t, postDBSeededVersion, f.postVersionIn(t, postDBUnhostedCommunity, post.URI),
		"fixture: the post's community version")
	return post
}

// acceptanceRecordBody is an acceptance record pinning post at its CID.
func acceptanceRecordBody(post seededCommunityPost, createdAt string) map[string]any {
	return map[string]any{
		"$type":     posts.AcceptanceCollection,
		"subject":   map[string]any{"uri": post.URI, "cid": post.CID},
		"createdAt": createdAt,
	}
}

// deliverRestoreCommitIn hands a restore commit's two firehose events in
// communityDID's repo to the post consumer at rev: the removal delete and the
// acceptance create (cid, pinned to the post's CID), acceptance first when
// acceptanceFirst.
func (f *communityPostDBFixture) deliverRestoreCommitIn(t *testing.T, communityDID string, post seededCommunityPost, rev, cid string, acceptanceFirst bool) {
	t.Helper()
	removalDelete := postStandEventIn(communityDID, "delete", posts.RemovalCollection, post.SubjectRkey, rev, "", nil)
	acceptance := postStandEventIn(communityDID, "create", posts.AcceptanceCollection, post.SubjectRkey, rev, cid,
		acceptanceRecordBody(post, postDBIntentCreatedAt))
	events := []*jetstream.JetstreamEvent{removalDelete, acceptance}
	if acceptanceFirst {
		events = []*jetstream.JetstreamEvent{acceptance, removalDelete}
	}
	for _, event := range events {
		require.NoError(t, f.deliverPost(event))
	}
}

// postDBInstanceModeration is the real instance moderation service over the
// fixture's database, with the migration-080 community record outbox writing
// through the production instance-removal writer into the fixture's one fake
// community repo. The outbox runs on its own clock, a minute ahead of the
// wall clock so rows the database stamped due now are due on it.
type postDBInstanceModeration struct {
	service moderation.Service
	queue   *moderation.CommunityRecordWriteQueue
	clock   *queueClock
	db      *sql.DB
}

// instanceModeration builds the instance moderation service and its outbox
// over the fixture.
func (f *communityPostDBFixture) instanceModeration(t *testing.T) *postDBInstanceModeration {
	t.Helper()
	store := postgres.NewModerationRepository(f.db)
	clock := &queueClock{now: time.Now().UTC().Add(time.Minute)}
	queue := moderation.NewCommunityRecordWriteQueue(store,
		posts.NewInstanceRemovalWriter(f.factory.open, clock.Now),
		postgres.NewAdmissionRepository(f.db),
		moderation.CommunityRecordWriteQueueConfig{Now: clock.Now})
	t.Cleanup(queue.Close)
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db)),
		store,
		moderation.Config{
			InstanceDID: rosterInstanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
			Communities:           postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed()),
			CommunityRecordWrites: queue,
		},
	)
	return &postDBInstanceModeration{service: service, queue: queue, clock: clock, db: f.db}
}

// removeContent removes post with instance authority as the instance admin
// and waits for the outbox's immediate attempt. It returns the removal action.
func (m *postDBInstanceModeration) removeContent(t *testing.T, post seededCommunityPost, key string) *moderation.Action {
	t.Helper()
	ctx := context.Background()
	state, err := m.service.GetSubjectState(ctx, post.URI)
	require.NoError(t, err)
	result, err := m.service.RemoveContent(ctx, rosterAdmin, moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: post.URI, CID: post.CID},
		ExpectedVersion: state.Version, IdempotencyKey: key, Reason: postDBReasonSpam,
	})
	require.NoError(t, err, "the instance removal applies")
	require.Equal(t, moderation.OutcomeApplied, result.Outcome, "the instance removal applies")
	require.NotNil(t, result.Action)
	require.NoError(t, m.queue.Wait(ctx))
	return result.Action
}

// restoreContent restores the instance removal with instance authority as the
// instance admin and waits for the outbox's immediate attempt.
func (m *postDBInstanceModeration) restoreContent(t *testing.T, post seededCommunityPost, removal *moderation.Action, key string) {
	t.Helper()
	ctx := context.Background()
	state, err := m.service.GetSubjectState(ctx, post.URI)
	require.NoError(t, err)
	result, err := m.service.RestoreContent(ctx, rosterAdmin, moderation.RestoreContentRequest{
		ActionID: removal.ID, ReviewedSubject: &moderation.StrongRef{URI: post.URI, CID: post.CID},
		ExpectedVersion: state.Version, IdempotencyKey: key,
		Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err, "the instance restore applies")
	require.Equal(t, moderation.OutcomeApplied, result.Outcome, "the instance restore applies")
	require.NoError(t, m.queue.Wait(ctx))
}

// drain runs the outbox to completion: its clock moves past any lease a
// removal write left (a restore waits that out before completing), a sweep
// reconciles every due row, and every row for rosteralpha must then be
// completed.
func (m *postDBInstanceModeration) drain(t *testing.T) {
	t.Helper()
	m.clock.Set(m.clock.Now().Add(10 * time.Minute))
	require.NoError(t, m.queue.Sweep(context.Background()))
	rows, err := m.db.Query(`SELECT subject_uri, state FROM moderation_community_record_writes WHERE community_did = $1`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var subjectURI, state string
		require.NoError(t, rows.Scan(&subjectURI, &state))
		require.Equalf(t, "completed", state, "the outbox row for %s has drained", subjectURI)
	}
	require.NoError(t, rows.Err())
}

// authorPostGet is the author's post.get result for the post, through the
// production post service.
func (f *communityPostDBFixture) authorPostGet(t *testing.T, postURI string) *posts.PostResult {
	t.Helper()
	service := posts.NewPostService(f.postRepo, nil, nil, nil, nil, nil, "",
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()))
	results, err := service.GetPosts(context.Background(), posts.GetPostsRequest{URIs: []string{postURI}, ViewerDID: postDBAuthor})
	require.NoError(t, err)
	require.Len(t, results, 1)
	return results[0]
}

// standRecord is the record standing at collection/rkey in the fake community
// repo, or nil.
func (f *communityPostDBFixture) standRecord(collection, rkey string) *pds.RecordResponse {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	record, ok := f.repo.records[collection+"/"+rkey]
	if !ok {
		return nil
	}
	copied := *record
	return &copied
}

// setAcceptanceWriteCID is the CID the fake repo gives the next create or
// update of post's acceptance record.
func (f *communityPostDBFixture) setAcceptanceWriteCID(post seededCommunityPost, cid string) {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.writeCIDs[posts.AcceptanceCollection+"/"+post.SubjectRkey] = cid
}

// recordCIDOf is the CID a PDS assigns body: DAG-CBOR, CIDv1, sha256, as
// postDBAlphaRemovalBodyCID was computed.
func recordCIDOf(t *testing.T, body map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	data, err := atdata.UnmarshalJSON(encoded)
	require.NoError(t, err)
	cbor, err := atdata.MarshalCBOR(data)
	require.NoError(t, err)
	sum, err := cid.NewPrefixV1(cid.DagCBOR, multihash.SHA2_256).Sum(cbor)
	require.NoError(t, err)
	return sum.String()
}

// indexedRemovalCID is the record CID community_post_removal_records holds
// for the post in rosteralpha, "" when it holds no row or no CID.
func (f *communityPostDBFixture) indexedRemovalCID(t *testing.T, postURI string) string {
	t.Helper()
	var recordCID sql.NullString
	err := f.db.QueryRow(`
		SELECT record_cid FROM community_post_removal_records WHERE community_did = $1 AND post_uri = $2
	`, rosterCommunity, postURI).Scan(&recordCID)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return recordCID.String
}
