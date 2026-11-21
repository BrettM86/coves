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
	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures/commentfeedfixture"

	"github.com/stretchr/testify/require"
)

// Shared fixture for the community comment action tests against real
// Postgres: the community post fixture (rosteralpha, owner O, moderators M and
// M2, the instance admin, the fake community repo, accepted posts) plus the
// production comment resolver and comments indexed through the production
// comment consumer. Outcomes are read back the way a client or the modlog
// reads them: the comment service's getComments thread, the moderation_actions
// and moderation_decisions rows, the comment subject's community version and
// the removal record's rev gate.

const (
	commentDBAlphaRkey = "3kcommentalpha1" // commentAlphaURI's rkey in the author's repo
	commentDBReplyRkey = "3kcommentreply1"
	commentDBReplyCID  = "bafyreiacommentreplyaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	commentDBContent      = "the comment under moderation"
	commentDBReplyContent = "a reply to the comment"

	// commentDBExternalCreatedAt is the createdAt of a removal written outside
	// Coves.
	commentDBExternalCreatedAt = "2026-11-12T09:00:00Z"

	// commentDBHeadThreeCID is the head after a third commit (rosterHeadThreeRev).
	commentDBHeadThreeCID = "bafyreiaheadthreeaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// seededCommunityComment is a comment indexed in a rosteralpha thread.
type seededCommunityComment struct {
	URI, CID    string
	SubjectRkey string // the removal record's key in the community repo
	RemovalURI  string // the removal record's URI in rosteralpha's repo
}

type communityCommentDBFixture struct {
	*communityPostDBFixture
	comments *jetstream.CommentEventConsumer
	reads    comments.Service
}

// newCommunityCommentDBFixture extends newCommunityPostDBFixture for comment
// actions: the service resolves comments with
// postgres.NewCommunityCommentResolver, the author is an indexed user, and
// comments are indexed through the production comment consumer wired to the
// community comment removal index. No post or comment is seeded.
func newCommunityCommentDBFixture(t *testing.T) *communityCommentDBFixture {
	t.Helper()
	base := newCommunityPostDBFixture(t)
	base.config.Comments = postgres.NewCommunityCommentResolver(base.db)
	base.service = moderation.NewCommunityActionService(base.config)

	_, err := base.db.Exec(`
		INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, 'rosterauthor.test', 'https://native.pds.test', NOW())
	`, postDBAuthor)
	require.NoError(t, err)

	return &communityCommentDBFixture{
		communityPostDBFixture: base,
		comments: jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(base.db), base.db,
			jetstream.WithCommentCommunityRemovals(postgres.NewCommunityCommentRemovalRepository(base.db))),
		reads: comments.NewCommentServiceWithPDSFactory(
			postgres.NewCommentRepository(base.db), postgres.NewUserRepository(base.db),
			postgres.NewPostRepository(base.db, postgres.WithAccountRestrictionAuthority(rosterInstanceDID)),
			postgres.NewCommunityRepository(base.db, credentialciphertest.Fixed()), nil, nil,
			comments.WithFeedRegistry(commentfeedfixture.Registry(base.db))),
	}
}

// commentDBThread is an accepted rosteralpha post, the comment under
// moderation replying to it (commentAlphaURI) and a reply to that comment.
type commentDBThread struct {
	Post     seededCommunityPost
	Comment  seededCommunityComment
	ReplyURI string
}

// seedThread indexes postDBAlpha accepted in rosteralpha, commentAlphaURI
// replying to it and a reply to the comment. A removal the service writes for
// the comment gets commentRemovalWrittenCID.
func (f *communityCommentDBFixture) seedThread(t *testing.T) commentDBThread {
	t.Helper()
	post := f.seedAcceptedPost(t, postDBAlpha)
	comment := f.indexComment(t, commentDBAlphaRkey, commentAlphaCID, commentDBContent,
		post.URI, post.CID, post.URI, post.CID, commentRemovalWrittenCID)
	require.Equal(t, commentAlphaURI, comment.URI, "fixture: the comment under moderation")
	reply := f.indexComment(t, commentDBReplyRkey, commentDBReplyCID, commentDBReplyContent,
		post.URI, post.CID, comment.URI, comment.CID, "")
	return commentDBThread{Post: post, Comment: comment, ReplyURI: reply.URI}
}

// indexComment hands the author's comment create to the production comment
// consumer: rkey, cid and content, with the given root and parent refs. A
// non-empty removalWriteCID is the CID the fake repo gives a removal of the
// comment written to rosteralpha's repo.
func (f *communityCommentDBFixture) indexComment(t *testing.T, rkey, cid, content, rootURI, rootCID, parentURI, parentCID, removalWriteCID string) seededCommunityComment {
	t.Helper()
	record := map[string]any{
		"$type":   moderation.CommentCollection,
		"content": content,
		"reply": map[string]any{
			"root":   map[string]any{"uri": rootURI, "cid": rootCID},
			"parent": map[string]any{"uri": parentURI, "cid": parentCID},
		},
		"createdAt": "2026-11-12T08:00:00Z",
	}
	require.NoError(t, f.comments.HandleEvent(context.Background(), &jetstream.JetstreamEvent{
		Kind: "commit", Did: postDBAuthor, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: "3kcommentrev01", Operation: "create", Collection: moderation.CommentCollection,
			RKey: rkey, CID: cid, Record: record,
		},
	}), "fixture: indexing comment %s", rkey)

	comment := seededCommunityComment{
		URI: "at://" + postDBAuthor + "/" + moderation.CommentCollection + "/" + rkey,
		CID: cid,
	}
	comment.SubjectRkey = posts.SubjectRkey(comment.URI)
	comment.RemovalURI = "at://" + rosterCommunity + "/" + posts.RemovalCollection + "/" + comment.SubjectRkey
	var indexed int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM comments WHERE uri = $1 AND deleted_at IS NULL`, comment.URI).Scan(&indexed))
	require.Equal(t, 1, indexed, "fixture: comment %s is indexed", rkey)
	if removalWriteCID != "" {
		f.repo.mu.Lock()
		f.repo.writeCIDs[posts.RemovalCollection+"/"+comment.SubjectRkey] = removalWriteCID
		f.repo.mu.Unlock()
	}
	return comment
}

func (f *communityCommentDBFixture) removeComment(actorDID, commentURI, code, reason string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return removeCommentWith(f.service, actorDID, commentURI, code, reason, expectedVersion, key)
}

func removeCommentWith(service *moderation.CommunityActionService, actorDID, commentURI, code, reason string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return service.RemoveComment(context.Background(), moderation.RemoveCommentRequest{
		ActorDID: actorDID, CommentURI: commentURI, Code: code, Reason: reason,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityCommentDBFixture) restoreComment(actorDID, commentURI string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.RestoreComment(context.Background(), moderation.RestoreCommentRequest{
		ActorDID: actorDID, CommentURI: commentURI, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// commentRemovalBody is a removal record of comment at its CID.
func commentRemovalBody(comment seededCommunityComment, code, reason, createdAt string) map[string]any {
	record := map[string]any{
		"$type":     posts.RemovalCollection,
		"subject":   map[string]any{"uri": comment.URI, "cid": comment.CID},
		"code":      code,
		"createdAt": createdAt,
	}
	if reason != "" {
		record["reason"] = reason
	}
	return record
}

// standExternalCommentRemoval puts a removal of comment written outside Coves
// into rosteralpha's repo (the head at head) and hands its create to the post
// consumer at head's rev, so the index and the PDS agree; the next batch
// commits at head two. The removal applies and is the comment's first
// community record change.
func (f *communityCommentDBFixture) standExternalCommentRemoval(t *testing.T, comment seededCommunityComment, head pds.LatestCommit, cid string, body map[string]any) {
	t.Helper()
	f.repo.setPostRecord(posts.RemovalCollection, comment.SubjectRkey, cid, body)
	f.repo.landCommit(head)
	require.NoError(t, f.deliverPost(postStandEvent("create", posts.RemovalCollection, comment.SubjectRkey, head.Rev, cid, body)))
	f.setCommitHead(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	active, exists := f.commentDecision(t, comment.URI)
	require.True(t, exists && active, "fixture: the external removal applies")
	require.EqualValues(t, 1, f.commentVersion(t, comment.URI), "fixture: the comment's community version after the external removal")
}

// commentVersion is the comment subject's rosteralpha version in
// community_subject_versions, 0 when none is stored.
func (f *communityCommentDBFixture) commentVersion(t *testing.T, commentURI string) int64 {
	t.Helper()
	var version int64
	err := f.db.QueryRow(`
		SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $2
	`, rosterCommunity, commentURI).Scan(&version)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err)
	return version
}

// commentDecision reports rosteralpha's community-scope removal decision on
// the comment: whether it is active, and whether one exists.
func (f *communityCommentDBFixture) commentDecision(t *testing.T, commentURI string) (active, exists bool) {
	t.Helper()
	err := f.db.QueryRow(`
		SELECT active FROM moderation_decisions
		WHERE subject_uri = $1 AND authority_did = $2 AND scope_kind = 'community'
		  AND scope_community_did = $2 AND kind = 'removal'
	`, commentURI, rosterCommunity).Scan(&active)
	if err == sql.ErrNoRows {
		return false, false
	}
	require.NoError(t, err)
	return active, true
}

// commentDecisionActionID is the action rosteralpha's removal decision on the
// comment points to, "" when there is no decision.
func (f *communityCommentDBFixture) commentDecisionActionID(t *testing.T, commentURI string) string {
	t.Helper()
	var actionID sql.NullString
	err := f.db.QueryRow(`
		SELECT active_action_id FROM moderation_decisions
		WHERE subject_uri = $1 AND authority_did = $2 AND scope_kind = 'community'
		  AND scope_community_did = $2 AND kind = 'removal'
	`, commentURI, rosterCommunity).Scan(&actionID)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return actionID.String
}

// reversedActionID is the action the action actionID reverses, "".
func (f *communityCommentDBFixture) reversedActionID(t *testing.T, actionID string) string {
	t.Helper()
	var reverses sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT reverses_action_id FROM moderation_actions WHERE id = $1`, actionID).Scan(&reverses))
	return reverses.String
}

// recordGateRev is the jetstream_record_revs rev held for recordURI, "" when
// none.
func (f *communityCommentDBFixture) recordGateRev(t *testing.T, recordURI string) string {
	t.Helper()
	var rev string
	err := f.db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, recordURI).Scan(&rev)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return rev
}

// commentActions is the moderation_actions rows about the comment, oldest
// first by insertion (action ids are monotonic; local and firehose
// actions stamp created_at from different clocks).
func (f *communityCommentDBFixture) commentActions(t *testing.T, commentURI string) []storedPostAction {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT action, COALESCE(actor_did, ''), admin_acting, scope_kind, COALESCE(scope_community_did, ''),
		       subject_uri, COALESCE(subject_collection, ''), COALESCE(reason, ''), origin
		FROM moderation_actions
		WHERE subject_uri = $1
		ORDER BY id
	`, commentURI)
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

// localCommentAction is the action a local remove or restore of commentURI by
// actorDID writes in rosteralpha.
func localCommentAction(action, actorDID string, adminActing bool, commentURI, reason string) storedPostAction {
	return storedPostAction{
		Action: action, ActorDID: actorDID, AdminActing: adminActing,
		ScopeKind: "community", ScopeCommunityDID: rosterCommunity,
		SubjectURI: commentURI, SubjectCollection: "social.coves.community.comment",
		Reason: reason, Origin: "local",
	}
}

// commentDBServed is one comment as getComments serves it.
type commentDBServed struct {
	Content        string
	DeletionReason string
	Moderation     *comments.ModerationView
	ReplyURIs      []string
}

// servedComment is the comment as the comment service's getComments serves
// the post's thread to an anonymous viewer; it fails when the comment is not
// in the thread.
func (f *communityCommentDBFixture) servedComment(t *testing.T, postURI, commentURI string) commentDBServed {
	t.Helper()
	response, err := f.reads.GetComments(context.Background(), &comments.GetCommentsRequest{
		PostURI: postURI, Sort: "new", Depth: 5, Limit: 50,
	})
	require.NoError(t, err)
	node := findThreadComment(response.Comments, commentURI)
	require.NotNil(t, node, "the thread serves %s", commentURI)
	served := commentDBServed{Moderation: node.Comment.Moderation}
	if node.Comment.DeletionReason != nil {
		served.DeletionReason = *node.Comment.DeletionReason
	}
	if node.Comment.Record != nil {
		encoded, err := json.Marshal(node.Comment.Record)
		require.NoError(t, err)
		var record struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(encoded, &record) == nil {
			served.Content = record.Content
		}
	}
	for _, reply := range node.Replies {
		served.ReplyURIs = append(served.ReplyURIs, reply.Comment.URI)
	}
	return served
}

func findThreadComment(nodes []*comments.ThreadViewComment, uri string) *comments.ThreadViewComment {
	for _, node := range nodes {
		if node.Comment != nil && node.Comment.URI == uri {
			return node
		}
		if found := findThreadComment(node.Replies, uri); found != nil {
			return found
		}
	}
	return nil
}

// communityRemovedModeration is the moderation view of a comment rosteralpha
// removed.
func communityRemovedModeration() *comments.ModerationView {
	return &comments.ModerationView{State: "removed", Sources: []comments.ModerationSourceView{{
		AuthorityDID: rosterCommunity,
		Scope:        comments.ModerationScopeView{Kind: "community", CommunityDID: rosterCommunity},
	}}}
}

const (
	// commentDBRemovalBodyCID is the CID the PDS assigns the removal a
	// moderator writes for commentAlphaURI at rosterClockStart:
	// commentRemovalBody(comment, "spam", commentRemovalReason,
	// postDBIntentCreatedAt) (DAG-CBOR, CIDv1, sha256), computed once with
	// indigo's atdata.MarshalCBOR and go-cid outside the code under test.
	commentDBRemovalBodyCID = "bafyreigvhq56sba2yag25ykqnkodkngyz3fw65qfctmkuxaqhftnmysyyu"
)

// setCommentRemovalWriteCID is the CID the fake repo gives the next create or
// update of comment's removal record.
func (f *communityCommentDBFixture) setCommentRemovalWriteCID(comment seededCommunityComment, cid string) {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	f.repo.writeCIDs[posts.RemovalCollection+"/"+comment.SubjectRkey] = cid
}

// externalCommentAction is the action a comment removal or its deletion
// indexed from communityDID's repo records when no local intent wrote it: no
// actor, origin inherited, the record's mapped code as the reason.
func externalCommentAction(communityDID, action, commentURI, reason string) storedPostAction {
	return storedPostAction{
		Action: action, ScopeKind: "community", ScopeCommunityDID: communityDID,
		SubjectURI: commentURI, SubjectCollection: "social.coves.community.comment",
		Reason: reason, Origin: "inherited",
	}
}

// newCommentActions is after without the actions already in before, as a
// multiset: the actions written in between, whatever their created_at order.
func newCommentActions(before, after []storedPostAction) []storedPostAction {
	remaining := append([]storedPostAction(nil), before...)
	out := []storedPostAction{}
	for _, action := range after {
		matched := false
		for i, old := range remaining {
			if old == action {
				remaining = append(remaining[:i], remaining[i+1:]...)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, action)
		}
	}
	return out
}

// newActionIDs is the ids in after that are not in before.
func newActionIDs(before, after []string) []string {
	seen := map[string]bool{}
	for _, id := range before {
		seen[id] = true
	}
	out := []string{}
	for _, id := range after {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}
