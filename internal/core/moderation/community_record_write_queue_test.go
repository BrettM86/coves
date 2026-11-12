package moderation_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B6: the community record write queue reconciles one outbox row at a time
// against a hosted community's repo, through a fake writer over an in-memory
// repo and a fake admission stamper, on an injected clock.

const (
	queueHostedDID    = "did:plc:hostedcommunity"
	queueNotHostedDID = "did:plc:notmine"
	queuePostURI      = "at://did:plc:postauthor/social.coves.community.postv2/3kpost"
	queuePostCID      = "bafyreipostversion"
	queueRestoredCID  = "bafyreirestoredversion"
	queueCommentURI   = "at://did:plc:commentauthor/social.coves.community.comment/3kcomment"
	queueCommentCID   = "bafyreicommentversion"
	queueCreatedAt    = "2026-11-11T12:00:00Z"
	// queueAcceptanceURI is the acceptance record an undo writes back.
	queueAcceptanceURI  = "at://did:plc:hostedcommunity/social.coves.community.acceptance/3kacceptance"
	queueAcceptanceRkey = "3kacceptance"
)

var queueStart = time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type queueClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *queueClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *queueClock) Set(now time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = now
}

type fakeRecordKey struct {
	communityDID string
	subjectURI   string
}

// fakeInstanceRemovalRepo is an in-memory posts.InstanceRemovalWriter: one
// removal and one acceptance slot per subject, and a commit counter that names
// every record CID ("bafyreiwritten<n>") and rev ("3krev<nnnn>") it mints.
type fakeInstanceRemovalRepo struct {
	mu          sync.Mutex
	hosted      map[string]bool
	removals    map[fakeRecordKey]posts.StandingRemoval
	acceptances map[fakeRecordKey]posts.StandingRecordRef
	commits     int
	headRev     string

	writeCalls int
	undoCalls  int
	// failWrites are consumed one per WriteInstanceRemoval call; the error is
	// returned before anything is written.
	failWrites []error
	// lostWriteResponses counts WriteInstanceRemoval calls that commit and
	// then lose their response.
	lostWriteResponses int
	// beforeWrite and beforeUndo run once, inside the next call, before it
	// touches the repo.
	beforeWrite func()
	beforeUndo  func()
	// beforeAcceptanceRead runs once, inside the next StandingAcceptance call.
	beforeAcceptanceRead func()
	// failAcceptanceReads are consumed one per StandingAcceptance call, after
	// beforeAcceptanceRead.
	failAcceptanceReads []error
	// blockWrite runs once, inside the next WriteInstanceRemoval call after
	// beforeWrite, with the call's context; an error it returns is the call's
	// error, with nothing written.
	blockWrite func(ctx context.Context) error
}

func newFakeInstanceRemovalRepo() *fakeInstanceRemovalRepo {
	return &fakeInstanceRemovalRepo{
		hosted:      map[string]bool{queueHostedDID: true},
		removals:    make(map[fakeRecordKey]posts.StandingRemoval),
		acceptances: make(map[fakeRecordKey]posts.StandingRecordRef),
		headRev:     "3krev0000",
	}
}

func (repo *fakeInstanceRemovalRepo) commit() (cid, rev string) {
	repo.commits++
	repo.headRev = fmt.Sprintf("3krev%04d", repo.commits)
	return fmt.Sprintf("bafyreiwritten%d", repo.commits), repo.headRev
}

func (repo *fakeInstanceRemovalRepo) opened(communityDID string) error {
	if !repo.hosted[communityDID] {
		return fmt.Errorf("opening the repo of community %s: %w", communityDID, posts.ErrCommunityNotHosted)
	}
	return nil
}

func takeHook(hook *func()) func() {
	run := *hook
	*hook = nil
	return run
}

func (repo *fakeInstanceRemovalRepo) setAcceptance(communityDID, subjectURI, subjectCID string) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.acceptances[fakeRecordKey{communityDID, subjectURI}] = posts.StandingRecordRef{CID: "bafyreiacceptance", SubjectCID: subjectCID}
}

func (repo *fakeInstanceRemovalRepo) setRemoval(communityDID string, removal posts.StandingRemoval) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.removals[fakeRecordKey{communityDID, removal.SubjectURI}] = removal
}

func (repo *fakeInstanceRemovalRepo) removal(communityDID, subjectURI string) (posts.StandingRemoval, bool) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	removal, ok := repo.removals[fakeRecordKey{communityDID, subjectURI}]
	return removal, ok
}

func (repo *fakeInstanceRemovalRepo) acceptance(communityDID, subjectURI string) (posts.StandingRecordRef, bool) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	acceptance, ok := repo.acceptances[fakeRecordKey{communityDID, subjectURI}]
	return acceptance, ok
}

func (repo *fakeInstanceRemovalRepo) writeCallCount() int {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	return repo.writeCalls
}

func (repo *fakeInstanceRemovalRepo) commitCount() int {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	return repo.commits
}

func (repo *fakeInstanceRemovalRepo) StandingAcceptance(_ context.Context, communityDID, subjectURI string) (*posts.StandingRecordRef, error) {
	repo.mu.Lock()
	hook := takeHook(&repo.beforeAcceptanceRead)
	repo.mu.Unlock()
	if hook != nil {
		hook()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.opened(communityDID); err != nil {
		return nil, err
	}
	if len(repo.failAcceptanceReads) > 0 {
		err := repo.failAcceptanceReads[0]
		repo.failAcceptanceReads = repo.failAcceptanceReads[1:]
		return nil, err
	}
	acceptance, ok := repo.acceptances[fakeRecordKey{communityDID, subjectURI}]
	if !ok {
		return nil, nil
	}
	return &acceptance, nil
}

func (repo *fakeInstanceRemovalRepo) StandingRemoval(_ context.Context, communityDID, subjectURI string) (*posts.StandingRemoval, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.opened(communityDID); err != nil {
		return nil, err
	}
	removal, ok := repo.removals[fakeRecordKey{communityDID, subjectURI}]
	if !ok {
		return nil, nil
	}
	return &removal, nil
}

func (repo *fakeInstanceRemovalRepo) WriteInstanceRemoval(ctx context.Context, cmd posts.InstanceRemovalCommand) (posts.InstanceRemovalResult, error) {
	repo.mu.Lock()
	repo.writeCalls++
	hook := takeHook(&repo.beforeWrite)
	block := repo.blockWrite
	repo.blockWrite = nil
	repo.mu.Unlock()
	if hook != nil {
		hook()
	}
	if block != nil {
		if err := block(ctx); err != nil {
			return posts.InstanceRemovalResult{}, err
		}
	}
	repo.mu.Lock()
	if err := repo.opened(cmd.CommunityDID); err != nil {
		repo.mu.Unlock()
		return posts.InstanceRemovalResult{}, err
	}
	if len(repo.failWrites) > 0 {
		err := repo.failWrites[0]
		repo.failWrites = repo.failWrites[1:]
		repo.mu.Unlock()
		return posts.InstanceRemovalResult{}, err
	}
	key := fakeRecordKey{cmd.CommunityDID, cmd.SubjectURI}
	if standing, ok := repo.removals[key]; ok {
		repo.mu.Unlock()
		return posts.InstanceRemovalResult{
			CommunityWriteResult: posts.CommunityWriteResult{CID: standing.CID, Rev: repo.headRev, Skipped: true},
			Standing:             &standing,
		}, nil
	}
	_, withdrew := repo.acceptances[key]
	repo.mu.Unlock()
	// The batch is about to be sent: the writer reports whether it deletes an
	// acceptance, and the caller may abort it.
	if cmd.BeforeCommit != nil {
		if err := cmd.BeforeCommit(ctx, withdrew); err != nil {
			return posts.InstanceRemovalResult{}, err
		}
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	delete(repo.acceptances, key)
	cid, rev := repo.commit()
	repo.removals[key] = posts.StandingRemoval{
		CID: cid, SubjectURI: cmd.SubjectURI, SubjectCID: cmd.SubjectCID,
		Code: string(cmd.Code), CreatedAt: cmd.CreatedAt,
	}
	if repo.lostWriteResponses > 0 {
		repo.lostWriteResponses--
		return posts.InstanceRemovalResult{}, errors.New("reading the applyWrites response: connection reset")
	}
	return posts.InstanceRemovalResult{
		CommunityWriteResult: posts.CommunityWriteResult{CID: cid, Rev: rev},
		WithdrewAcceptance:   withdrew,
	}, nil
}

func (repo *fakeInstanceRemovalRepo) UndoInstanceRemoval(_ context.Context, cmd posts.InstanceRemovalUndoCommand) (posts.CommunityWriteResult, error) {
	repo.mu.Lock()
	repo.undoCalls++
	hook := takeHook(&repo.beforeUndo)
	repo.mu.Unlock()
	if hook != nil {
		hook()
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.opened(cmd.CommunityDID); err != nil {
		return posts.CommunityWriteResult{}, err
	}
	key := fakeRecordKey{cmd.CommunityDID, cmd.SubjectURI}
	standing, ok := repo.removals[key]
	if !ok || standing.CID != cmd.RemovalCID {
		return posts.CommunityWriteResult{Rev: repo.headRev, Skipped: true}, nil
	}
	delete(repo.removals, key)
	cid, rev := repo.commit()
	if cmd.AcceptanceCID != "" {
		repo.acceptances[key] = posts.StandingRecordRef{CID: cid, SubjectCID: cmd.AcceptanceCID}
		return posts.CommunityWriteResult{URI: queueAcceptanceURI, RKey: queueAcceptanceRkey, CID: cid, Rev: rev}, nil
	}
	return posts.CommunityWriteResult{CID: cid, Rev: rev}, nil
}

type recordedStamp struct {
	kind           string
	communityDID   string
	postURI        string
	rev            string
	decisionCode   string
	pinnedCID      string
	acceptanceURI  string
	acceptanceRkey string
}

// fakeAdmissionStamper records the admission stamps the queue applies.
type fakeAdmissionStamper struct {
	mu     sync.Mutex
	stamps []recordedStamp
	// failures fails that many of the next stamps of a kind ("removal",
	// "acceptance", "removal-delete"); a failed stamp is not recorded.
	failures map[string]int
}

func (stamper *fakeAdmissionStamper) failNext(kind string) {
	stamper.mu.Lock()
	defer stamper.mu.Unlock()
	if stamper.failures == nil {
		stamper.failures = make(map[string]int)
	}
	stamper.failures[kind]++
}

func (stamper *fakeAdmissionStamper) record(stamp recordedStamp) (posts.AdmissionResult, error) {
	stamper.mu.Lock()
	defer stamper.mu.Unlock()
	if stamper.failures[stamp.kind] > 0 {
		stamper.failures[stamp.kind]--
		return posts.AdmissionResult{}, errors.New("updating community_post_admissions: connection reset")
	}
	stamper.stamps = append(stamper.stamps, stamp)
	return posts.AdmissionResult{Outcome: posts.AdmissionApplied}, nil
}

func (stamper *fakeAdmissionStamper) recorded() []recordedStamp {
	stamper.mu.Lock()
	defer stamper.mu.Unlock()
	return append([]recordedStamp(nil), stamper.stamps...)
}

func (stamper *fakeAdmissionStamper) ApplyRemoval(_ context.Context, cmd posts.ApplyRemovalCommand) (posts.AdmissionResult, error) {
	return stamper.record(recordedStamp{kind: "removal", communityDID: cmd.CommunityDID, postURI: cmd.PostURI, rev: cmd.Watermark.Rev, decisionCode: cmd.DecisionCode})
}

func (stamper *fakeAdmissionStamper) ApplyAcceptance(_ context.Context, cmd posts.ApplyAcceptanceCommand) (posts.AdmissionResult, error) {
	return stamper.record(recordedStamp{
		kind: "acceptance", communityDID: cmd.CommunityDID, postURI: cmd.PostURI, rev: cmd.Watermark.Rev, pinnedCID: cmd.PinnedCID,
		acceptanceURI: cmd.AcceptanceURI, acceptanceRkey: cmd.AcceptanceRkey,
	})
}

func (stamper *fakeAdmissionStamper) ApplyRemovalDelete(_ context.Context, cmd posts.CommunityDeleteCommand) (posts.AdmissionResult, error) {
	return stamper.record(recordedStamp{kind: "removal-delete", communityDID: cmd.CommunityDID, postURI: cmd.PostURI, rev: cmd.Watermark.Rev})
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type queueHarness struct {
	store   *inMemoryModerationStore
	repo    *fakeInstanceRemovalRepo
	stamper *fakeAdmissionStamper
	clock   *queueClock
	queue   *moderation.CommunityRecordWriteQueue
}

func newQueueHarness() queueHarness {
	return newQueueHarnessWithConfig(moderation.CommunityRecordWriteQueueConfig{})
}

// newQueueHarnessWithConfig builds the harness with config's other fields; Now
// is always the harness clock.
func newQueueHarnessWithConfig(config moderation.CommunityRecordWriteQueueConfig) queueHarness {
	clock := &queueClock{now: queueStart}
	store := newInMemoryModerationStore(queueStart)
	store.outboxNow = clock.Now
	repo := newFakeInstanceRemovalRepo()
	stamper := &fakeAdmissionStamper{}
	config.Now = clock.Now
	queue := moderation.NewCommunityRecordWriteQueue(store, repo, stamper, config)
	return queueHarness{store: store, repo: repo, stamper: stamper, clock: clock, queue: queue}
}

// attempt runs one post-commit attempt for the subject and waits for it.
func (harness queueHarness) attempt(t *testing.T, communityDID, subjectURI string) {
	t.Helper()
	harness.queue.WriteAfterCommit(t.Context(), moderation.CommunityRecordSubject{CommunityDID: communityDID, SubjectURI: subjectURI})
	require.NoError(t, harness.queue.Wait(t.Context()))
}

func (harness queueHarness) row(t *testing.T, communityDID, subjectURI string) moderation.CommunityRecordWrite {
	t.Helper()
	write, exists := harness.store.communityRecordWrite(communityDID, subjectURI)
	require.True(t, exists)
	return write
}

type queueSubject struct {
	name       string
	uri        string
	cid        string
	collection string
	isPost     bool
}

var (
	queuePost    = queueSubject{name: "post", uri: queuePostURI, cid: queuePostCID, collection: "social.coves.community.postv2", isPost: true}
	queueComment = queueSubject{name: "comment", uri: queueCommentURI, cid: queueCommentCID, collection: "social.coves.community.comment"}
)

// removalRow is the row an instance removal of subject by actionID at
// createdAt records.
func removalRow(communityDID string, subject queueSubject, actionID string, code posts.DecisionCode, createdAt time.Time) moderation.CommunityRecordWrite {
	return moderation.CommunityRecordWrite{
		CommunityDID: communityDID, SubjectURI: subject.uri, SubjectCollection: subject.collection,
		SubjectCID: subject.cid, RemovalActionID: actionID, Code: code, RemovalCreatedAt: createdAt,
	}
}

// ownFingerprint is the removal record the queue writes for removalRow(...,
// "spam", queueStart).
func ownFingerprint(cid string, subject queueSubject) posts.StandingRemoval {
	return posts.StandingRemoval{CID: cid, SubjectURI: subject.uri, SubjectCID: subject.cid, Code: "spam", CreatedAt: queueCreatedAt}
}

func restoreCIDFor(subject queueSubject) string {
	if subject.isPost {
		return queueRestoredCID
	}
	return ""
}

// ---------------------------------------------------------------------------
// desired = removed
// ---------------------------------------------------------------------------

func TestCommunityRecordWriteQueue_WritesTheRemovalWhenNoneStands(t *testing.T) {
	for _, row := range []struct {
		name             string
		subject          queueSubject
		acceptanceStands bool
		wantWithdrew     bool
		wantStamps       []recordedStamp
	}{
		{
			name: "post with a standing acceptance", subject: queuePost, acceptanceStands: true, wantWithdrew: true,
			wantStamps: []recordedStamp{{kind: "removal", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", decisionCode: "spam"}},
		},
		{
			name: "post never accepted", subject: queuePost,
			wantStamps: []recordedStamp{{kind: "removal", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", decisionCode: "spam"}},
		},
		{name: "comment", subject: queueComment},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newQueueHarness()
			if row.acceptanceStands {
				harness.repo.setAcceptance(queueHostedDID, row.subject.uri, row.subject.cid)
			}
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, row.subject, "action-a", "spam", queueStart))

			harness.attempt(t, queueHostedDID, row.subject.uri)

			removal, ok := harness.repo.removal(queueHostedDID, row.subject.uri)
			require.True(t, ok, "the removal record must be written")
			assert.Equal(t, ownFingerprint("bafyreiwritten1", row.subject), removal)
			_, acceptanceStands := harness.repo.acceptance(queueHostedDID, row.subject.uri)
			assert.False(t, acceptanceStands)

			write := harness.row(t, queueHostedDID, row.subject.uri)
			assert.True(t, write.Completed)
			assert.Equal(t, "bafyreiwritten1", write.OwnedRemovalCID)
			assert.Equal(t, row.wantWithdrew, write.WithdrewAcceptance)
			assert.Equal(t, row.wantStamps, harness.stamper.recorded(),
				"a post's admission row is stamped with the commit rev; a comment has none")
		})
	}
}

func TestCommunityRecordWriteQueue_AdoptsARemovalWithItsOwnContent(t *testing.T) {
	// A write whose response was lost left exactly the record the queue would
	// write: it is the queue's own.
	harness := newQueueHarness()
	harness.repo.setRemoval(queueHostedDID, ownFingerprint("bafyreilostresponse", queuePost))
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.store.updateCommunityRecordWrite(queueHostedDID, queuePostURI, func(write *moderation.CommunityRecordWrite) {
		startedAt := queueStart
		write.LastWriteStartedAt = &startedAt
	})

	harness.attempt(t, queueHostedDID, queuePostURI)

	assert.Equal(t, 0, harness.repo.commitCount(), "an adopted removal is not written again")
	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.True(t, write.Completed)
	assert.Equal(t, "bafyreilostresponse", write.OwnedRemovalCID)
}

func TestCommunityRecordWriteQueue_LeavesAForeignRemovalAlone(t *testing.T) {
	for name, foreign := range map[string]posts.StandingRemoval{
		"another code":       {CID: "bafyreiforeign", SubjectURI: queuePostURI, SubjectCID: queuePostCID, Code: "off-topic", CreatedAt: queueCreatedAt},
		"a reason":           {CID: "bafyreiforeign", SubjectURI: queuePostURI, SubjectCID: queuePostCID, Code: "spam", Reason: "seen in modqueue", CreatedAt: queueCreatedAt},
		"another createdAt":  {CID: "bafyreiforeign", SubjectURI: queuePostURI, SubjectCID: queuePostCID, Code: "spam", CreatedAt: "2026-11-10T08:00:00Z"},
		"another subjectCID": {CID: "bafyreiforeign", SubjectURI: queuePostURI, SubjectCID: "bafyreiolderversion", Code: "spam", CreatedAt: queueCreatedAt},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newQueueHarness()
			harness.repo.setRemoval(queueHostedDID, foreign)
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))

			harness.attempt(t, queueHostedDID, queuePostURI)

			write := harness.row(t, queueHostedDID, queuePostURI)
			assert.True(t, write.Completed, "a removal someone else wrote already removes the post")
			assert.Equal(t, "", write.OwnedRemovalCID)

			// Restoring later must not undo a removal the instance never wrote.
			harness.clock.Set(queueStart.Add(3 * time.Minute))
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
			harness.attempt(t, queueHostedDID, queuePostURI)

			standing, ok := harness.repo.removal(queueHostedDID, queuePostURI)
			require.True(t, ok)
			assert.Equal(t, foreign, standing)
			assert.Equal(t, 0, harness.repo.commitCount())
			assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
		})
	}
}

// ---------------------------------------------------------------------------
// desired = restored
// ---------------------------------------------------------------------------

func TestCommunityRecordWriteQueue_UndoesItsOwnRemovalOnRestore(t *testing.T) {
	for _, row := range []struct {
		name           string
		subject        queueSubject
		withdrew       bool
		wantAcceptance bool
		wantStamps     []recordedStamp
	}{
		{
			name: "post whose acceptance the removal withdrew", subject: queuePost, withdrew: true, wantAcceptance: true,
			wantStamps: []recordedStamp{{
				kind: "acceptance", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", pinnedCID: queueRestoredCID,
				acceptanceURI: queueAcceptanceURI, acceptanceRkey: queueAcceptanceRkey,
			}},
		},
		{
			name: "post never accepted", subject: queuePost,
			wantStamps: []recordedStamp{{kind: "removal-delete", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001"}},
		},
		{name: "comment", subject: queueComment},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newQueueHarness()
			harness.repo.setRemoval(queueHostedDID, ownFingerprint("bafyreiours", row.subject))
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, row.subject, "action-a", "spam", queueStart))
			harness.store.updateCommunityRecordWrite(queueHostedDID, row.subject.uri, func(write *moderation.CommunityRecordWrite) {
				write.OwnedRemovalCID = "bafyreiours"
				write.AcceptanceSeen = row.withdrew
				write.WithdrewAcceptance = row.withdrew
				write.Completed = true
			})
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, row.subject.uri, "action-a", restoreCIDFor(row.subject)))

			harness.attempt(t, queueHostedDID, row.subject.uri)

			_, removalStands := harness.repo.removal(queueHostedDID, row.subject.uri)
			assert.False(t, removalStands, "the queue's own removal is deleted")
			acceptance, acceptanceStands := harness.repo.acceptance(queueHostedDID, row.subject.uri)
			assert.Equal(t, row.wantAcceptance, acceptanceStands, "an acceptance comes back only if the removal withdrew one")
			if row.wantAcceptance {
				assert.Equal(t, queueRestoredCID, acceptance.SubjectCID, "the acceptance pins the CID indexed at restore")
			}
			assert.Equal(t, 1, harness.repo.commitCount(), "the undo is one commit")

			write := harness.row(t, queueHostedDID, row.subject.uri)
			assert.True(t, write.Completed)
			assert.Equal(t, "", write.OwnedRemovalCID)
			assert.Equal(t, row.wantStamps, harness.stamper.recorded())
		})
	}
}

func TestCommunityRecordWriteQueue_RestoreWritesNothingWhenItsRemovalIsGone(t *testing.T) {
	for name, standing := range map[string]*posts.StandingRemoval{
		"a community moderator replaced it": {CID: "bafyreimoderator", SubjectURI: queuePostURI, SubjectCID: queuePostCID, Code: "off-topic", CreatedAt: "2026-11-11T13:00:00Z"},
		"it was deleted":                    nil,
	} {
		t.Run(name, func(t *testing.T) {
			harness := newQueueHarness()
			if standing != nil {
				harness.repo.setRemoval(queueHostedDID, *standing)
			}
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
			harness.store.updateCommunityRecordWrite(queueHostedDID, queuePostURI, func(write *moderation.CommunityRecordWrite) {
				write.OwnedRemovalCID = "bafyreiours"
				write.WithdrewAcceptance = true
				write.Completed = true
			})
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))

			harness.attempt(t, queueHostedDID, queuePostURI)

			assert.Equal(t, 0, harness.repo.commitCount(), "restore reverses only what the instance's own removal wrote")
			_, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
			assert.False(t, acceptanceStands)
			if standing != nil {
				removal, ok := harness.repo.removal(queueHostedDID, queuePostURI)
				require.True(t, ok)
				assert.Equal(t, *standing, removal)
			}
			assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
			assert.Empty(t, harness.stamper.recorded())
		})
	}
}

// ---------------------------------------------------------------------------
// Failures and communities hosted elsewhere
// ---------------------------------------------------------------------------

func TestCommunityRecordWriteQueue_ReschedulesAFailedWriteWithBackoff(t *testing.T) {
	harness := newQueueHarness()
	harness.repo.failWrites = []error{errors.New("dial tcp: connection refused"), errors.New("dial tcp: connection refused")}
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))

	harness.attempt(t, queueHostedDID, queuePostURI)
	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.False(t, write.Completed)
	assert.Equal(t, 1, write.Attempts)
	assert.Equal(t, queueStart.Add(time.Minute), write.NextAttemptAt)
	assert.NotEmpty(t, write.LastFailureCode)

	// Not due yet: a sweep leaves it alone.
	harness.clock.Set(queueStart.Add(30 * time.Second))
	require.NoError(t, harness.queue.Sweep(t.Context()))
	assert.Equal(t, 1, harness.repo.writeCallCount())

	harness.clock.Set(queueStart.Add(time.Minute))
	require.NoError(t, harness.queue.Sweep(t.Context()))
	write = harness.row(t, queueHostedDID, queuePostURI)
	assert.Equal(t, 2, write.Attempts)
	assert.Equal(t, queueStart.Add(3*time.Minute), write.NextAttemptAt, "the second failure backs off two minutes")

	harness.clock.Set(queueStart.Add(3 * time.Minute))
	require.NoError(t, harness.queue.Sweep(t.Context()))
	write = harness.row(t, queueHostedDID, queuePostURI)
	assert.True(t, write.Completed)
	assert.Equal(t, "bafyreiwritten1", write.OwnedRemovalCID)
}

func TestCommunityRecordWriteQueue_CompletesARowWhoseCommunityIsNotHosted(t *testing.T) {
	harness := newQueueHarness()
	harness.store.seedCommunityRecordRemoval(removalRow(queueNotHostedDID, queuePost, "action-a", "spam", queueStart))

	harness.attempt(t, queueNotHostedDID, queuePostURI)

	write := harness.row(t, queueNotHostedDID, queuePostURI)
	assert.True(t, write.Completed, "a community this AppView no longer hosts is never retried")
	assert.Equal(t, 0, write.Attempts)
	assert.Equal(t, 0, harness.repo.commitCount())
}

// ---------------------------------------------------------------------------
// Desired changes during an attempt
// ---------------------------------------------------------------------------

func TestCommunityRecordWriteQueue_ARestoreDuringTheRemovalWriteIsReconciledNext(t *testing.T) {
	harness := newQueueHarness()
	harness.repo.setAcceptance(queueHostedDID, queuePostURI, queuePostCID)
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.repo.beforeWrite = func() {
		assert.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
	}

	harness.attempt(t, queueHostedDID, queuePostURI)

	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.Equal(t, "bafyreiwritten1", write.OwnedRemovalCID, "what the attempt wrote is persisted")
	assert.True(t, write.WithdrewAcceptance)
	assert.False(t, write.Completed, "the desired state changed under the attempt, so the row stays pending")

	harness.attempt(t, queueHostedDID, queuePostURI)

	_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.False(t, removalStands)
	acceptance, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	require.True(t, acceptanceStands)
	assert.Equal(t, queueRestoredCID, acceptance.SubjectCID)
	write = harness.row(t, queueHostedDID, queuePostURI)
	assert.True(t, write.Completed)
	assert.Equal(t, "", write.OwnedRemovalCID)
}

func TestCommunityRecordWriteQueue_ARemovalAfterARestoreEndsWithItsOwnRecord(t *testing.T) {
	// Admin A removes, A's restore is being written when admin B removes
	// again: the repo must end with B's removal standing.
	for _, subject := range []queueSubject{queuePost, queueComment} {
		t.Run(subject.name, func(t *testing.T) {
			harness := newQueueHarness()
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, subject, "action-a", "spam", queueStart))
			harness.attempt(t, queueHostedDID, subject.uri)
			require.Equal(t, "bafyreiwritten1", harness.row(t, queueHostedDID, subject.uri).OwnedRemovalCID)

			removedAgainAt := queueStart.Add(time.Minute)
			harness.clock.Set(removedAgainAt)
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, subject.uri, "action-a", restoreCIDFor(subject)))
			harness.repo.beforeUndo = func() {
				harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, subject, "action-b", "rule-violation", removedAgainAt))
			}
			harness.attempt(t, queueHostedDID, subject.uri)

			write := harness.row(t, queueHostedDID, subject.uri)
			assert.False(t, write.Completed)
			assert.Equal(t, moderation.CommunityRecordRemoved, write.Desired)

			harness.attempt(t, queueHostedDID, subject.uri)

			removal, ok := harness.repo.removal(queueHostedDID, subject.uri)
			require.True(t, ok, "B's removal must stand")
			assert.Equal(t, "rule-violation", removal.Code)
			assert.Equal(t, "2026-11-11T12:01:00Z", removal.CreatedAt)
			write = harness.row(t, queueHostedDID, subject.uri)
			assert.True(t, write.Completed)
			assert.Equal(t, removal.CID, write.OwnedRemovalCID)
		})
	}
}

// ---------------------------------------------------------------------------
// Lost responses and writes still in flight (v3)
// ---------------------------------------------------------------------------

func TestCommunityRecordWriteQueue_AdoptionAfterALostResponseStillRestoresTheAcceptance(t *testing.T) {
	harness := newQueueHarness()
	harness.repo.setAcceptance(queueHostedDID, queuePostURI, queuePostCID)
	harness.repo.lostWriteResponses = 1
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))

	harness.attempt(t, queueHostedDID, queuePostURI)
	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.True(t, write.AcceptanceSeen, "the acceptance seen before the write is persisted under the claim")
	assert.False(t, write.Completed)

	harness.clock.Set(queueStart.Add(time.Minute))
	harness.attempt(t, queueHostedDID, queuePostURI)
	write = harness.row(t, queueHostedDID, queuePostURI)
	assert.True(t, write.Completed)
	assert.Equal(t, "bafyreiwritten1", write.OwnedRemovalCID)
	assert.True(t, write.WithdrewAcceptance, "the acceptance the lost write deleted is still owed back")

	harness.clock.Set(queueStart.Add(5 * time.Minute))
	require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
	harness.attempt(t, queueHostedDID, queuePostURI)

	_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.False(t, removalStands)
	acceptance, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	require.True(t, acceptanceStands)
	assert.Equal(t, queueRestoredCID, acceptance.SubjectCID)
}

func TestCommunityRecordWriteQueue_RestoreUndoesARemovalItNeverAdopted(t *testing.T) {
	// The removal write lost its response and the restore arrives before any
	// attempt adopted it: the record's content proves it is the instance's.
	harness := newQueueHarness()
	harness.repo.lostWriteResponses = 1
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.attempt(t, queueHostedDID, queuePostURI)
	require.Equal(t, "", harness.row(t, queueHostedDID, queuePostURI).OwnedRemovalCID)

	harness.clock.Set(queueStart.Add(5 * time.Minute))
	require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
	harness.attempt(t, queueHostedDID, queuePostURI)

	_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.False(t, removalStands, "a removal with the instance's own content is undone")
	_, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	assert.False(t, acceptanceStands, "no acceptance stood, so none is written")
	assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
}

func TestCommunityRecordWriteQueue_RestoreWaitsOutAWriteThatMayStillLand(t *testing.T) {
	harness := newQueueHarness()
	harness.repo.failWrites = []error{context.DeadlineExceeded}
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.attempt(t, queueHostedDID, queuePostURI)

	harness.clock.Set(queueStart.Add(10 * time.Second))
	require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
	harness.attempt(t, queueHostedDID, queuePostURI)

	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.False(t, write.Completed, "nothing stands yet, but the timed-out write may still commit within its lease")
	assert.Equal(t, queueStart.Add(2*time.Minute), write.NextAttemptAt, "the restore is due again when the write's lease ends")

	// The timed-out write commits after all.
	harness.repo.setRemoval(queueHostedDID, ownFingerprint("bafyreilatewrite", queuePost))
	harness.clock.Set(queueStart.Add(2 * time.Minute))
	harness.attempt(t, queueHostedDID, queuePostURI)

	_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.False(t, removalStands, "the late removal is undone")
	assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
}

// ---------------------------------------------------------------------------
// Review round: attempt deadlines, stamp retries, ownership across takeovers,
// late acceptances, lost claims, batch errors (second-opinion C2–C5, I1, I3,
// I5, I6)
// ---------------------------------------------------------------------------

// blockUntilDone is a writer stuck on a PDS that never answers: it returns
// only when its context ends, reporting the context's error, or after a
// safety bound that the attempt's own timeout must beat.
func blockUntilDone(observed *error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			*observed = ctx.Err()
			return fmt.Errorf("sending applyWrites: %w", ctx.Err())
		case <-time.After(3 * time.Second):
			return errors.New("the attempt's own timeout never ended the write")
		}
	}
}

func TestCommunityRecordWriteQueue_AnAttemptThatRunsOutItsOwnTimeoutCountsAFailure(t *testing.T) {
	// The attempt's own deadline is the PDS failing to answer, not the sweep
	// being cut off: it counts, and the row backs off.
	harness := newQueueHarnessWithConfig(moderation.CommunityRecordWriteQueueConfig{AttemptTimeout: 50 * time.Millisecond})
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	var observed error
	harness.repo.blockWrite = blockUntilDone(&observed)

	require.NoError(t, harness.queue.Sweep(t.Context()))

	assert.ErrorIs(t, observed, context.DeadlineExceeded, "the write is ended by the attempt's own timeout")
	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.False(t, write.Completed)
	assert.Equal(t, 1, write.Attempts, "a timed-out attempt is a failed attempt")
	assert.Equal(t, "pds_timed_out", write.LastFailureCode)
	assert.Equal(t, queueStart.Add(time.Minute), write.NextAttemptAt, "a timed-out attempt backs off like any failure")
}

func TestCommunityRecordWriteQueue_CancellingTheSweepDoesNotCutAnAttemptInFlight(t *testing.T) {
	harness := newQueueHarness()
	secondPost := queueSubject{name: "second post", uri: queuePostURI + "second", cid: queuePostCID, collection: queuePost.collection, isPost: true}
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, secondPost, "action-b", "spam", queueStart))

	started := make(chan struct{})
	release := make(chan struct{})
	var contextErrAfterCancel error
	harness.repo.blockWrite = func(ctx context.Context) error {
		close(started)
		<-release
		contextErrAfterCancel = ctx.Err()
		return nil
	}
	sweepCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- harness.queue.Sweep(sweepCtx) }()
	<-started
	cancel()
	close(release)
	assert.ErrorIs(t, <-done, context.Canceled, "the cancelled sweep reports its cancellation")

	assert.NoError(t, contextErrAfterCancel, "the attempt's context outlives the sweep's: the write in flight runs to completion")
	first := harness.row(t, queueHostedDID, queuePostURI)
	assert.True(t, first.Completed, "the attempt in flight finishes and records its outcome")
	assert.Equal(t, "bafyreiwritten1", first.OwnedRemovalCID)
	assert.Equal(t, 0, first.Attempts, "nothing failed, so nothing is counted")
	assert.Empty(t, first.LastFailureCode)

	assert.Equal(t, 1, harness.repo.writeCallCount(), "a cancelled sweep claims no further rows")
	second := harness.row(t, queueHostedDID, secondPost.uri)
	assert.False(t, second.Completed)
	assert.Equal(t, 0, second.Attempts)
}

func TestCommunityRecordWriteQueue_AFailedAdmissionStampIsReappliedOnRetry(t *testing.T) {
	for _, row := range []struct {
		name string
		// restore seeds an owned removal and a restore; false seeds a removal.
		restore    bool
		withdrew   bool
		failKind   string
		wantStamps []recordedStamp
	}{
		{
			name: "removal", failKind: "removal",
			wantStamps: []recordedStamp{{kind: "removal", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", decisionCode: "spam"}},
		},
		{
			name: "restore writing the acceptance back", restore: true, withdrew: true, failKind: "acceptance",
			wantStamps: []recordedStamp{{
				kind: "acceptance", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", pinnedCID: queueRestoredCID,
				acceptanceURI: queueAcceptanceURI, acceptanceRkey: queueAcceptanceRkey,
			}},
		},
		{
			name: "restore deleting the removal only", restore: true, failKind: "removal-delete",
			wantStamps: []recordedStamp{{kind: "removal-delete", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001"}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newQueueHarness()
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
			if row.restore {
				harness.repo.setRemoval(queueHostedDID, ownFingerprint("bafyreiours", queuePost))
				harness.store.updateCommunityRecordWrite(queueHostedDID, queuePostURI, func(write *moderation.CommunityRecordWrite) {
					write.OwnedRemovalCID = "bafyreiours"
					write.AcceptanceSeen = row.withdrew
					write.WithdrewAcceptance = row.withdrew
					write.Completed = true
				})
				require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
			}
			harness.stamper.failNext(row.failKind)

			harness.attempt(t, queueHostedDID, queuePostURI)

			write := harness.row(t, queueHostedDID, queuePostURI)
			assert.False(t, write.Completed, "the admission row is not stamped yet")
			assert.Equal(t, 1, write.Attempts)
			assert.Equal(t, "admission_stamp_failed", write.LastFailureCode)
			assert.Equal(t, queueStart.Add(time.Minute), write.NextAttemptAt)
			assert.Empty(t, harness.stamper.recorded())

			harness.clock.Set(queueStart.Add(time.Minute))
			harness.attempt(t, queueHostedDID, queuePostURI)

			assert.Equal(t, row.wantStamps, harness.stamper.recorded(),
				"the retry stamps the admission row with the rev of the commit the first attempt made")
			assert.Equal(t, 1, harness.repo.commitCount(), "the retry writes nothing to the repo again")
			assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
		})
	}
}

func TestCommunityRecordWriteQueue_ATakeoverKeepsOwningAnEarlierUnacknowledgedRemoval(t *testing.T) {
	// Admin A's removal is written but its response is lost; A is restored and
	// admin B removes again before any attempt reconciles. The record A's
	// write left is still the instance's: once B is restored too, no removal
	// the instance wrote may stand.
	for _, subject := range []queueSubject{queuePost, queueComment} {
		t.Run(subject.name, func(t *testing.T) {
			harness := newQueueHarness()
			harness.repo.lostWriteResponses = 1
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, subject, "action-a", "spam", queueStart))
			harness.attempt(t, queueHostedDID, subject.uri)
			_, lostWriteLanded := harness.repo.removal(queueHostedDID, subject.uri)
			require.True(t, lostWriteLanded, "fixture: A's write committed")
			require.Equal(t, "", harness.row(t, queueHostedDID, subject.uri).OwnedRemovalCID, "fixture: its response was lost")

			removedAgainAt := queueStart.Add(time.Minute)
			harness.clock.Set(removedAgainAt)
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, subject.uri, "action-a", restoreCIDFor(subject)))
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, subject, "action-b", "rule-violation", removedAgainAt))
			harness.attempt(t, queueHostedDID, subject.uri)
			require.True(t, harness.row(t, queueHostedDID, subject.uri).Completed, "fixture: B's removal reconciled")

			harness.clock.Set(queueStart.Add(5 * time.Minute))
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, subject.uri, "action-b", restoreCIDFor(subject)))
			harness.attempt(t, queueHostedDID, subject.uri)

			standing, removalStands := harness.repo.removal(queueHostedDID, subject.uri)
			assert.Falsef(t, removalStands, "both removals were restored, yet the record A's lost write left still stands: %+v", standing)
			assert.True(t, harness.row(t, queueHostedDID, subject.uri).Completed)
		})
	}
}

func TestCommunityRecordWriteQueue_AnAcceptanceLandingDuringTheWriteIsWrittenBackOnRestore(t *testing.T) {
	// No acceptance stood at the queue's pre-read. One lands before the batch
	// is sent; the batch deletes it and its response is lost. The removal is
	// then adopted, and restoring the post must write the acceptance back.
	harness := newQueueHarness()
	harness.repo.lostWriteResponses = 1
	harness.repo.beforeWrite = func() {
		harness.repo.setAcceptance(queueHostedDID, queuePostURI, queuePostCID)
	}
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.attempt(t, queueHostedDID, queuePostURI)
	_, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	require.False(t, acceptanceStands, "fixture: the lost write deleted the acceptance")

	harness.clock.Set(queueStart.Add(time.Minute))
	harness.attempt(t, queueHostedDID, queuePostURI)
	require.Equal(t, "bafyreiwritten1", harness.row(t, queueHostedDID, queuePostURI).OwnedRemovalCID, "fixture: the removal is adopted")

	harness.clock.Set(queueStart.Add(5 * time.Minute))
	require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
	harness.attempt(t, queueHostedDID, queuePostURI)

	_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.False(t, removalStands)
	acceptance, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	require.True(t, acceptanceStands, "the acceptance the instance's write deleted is owed back")
	assert.Equal(t, queueRestoredCID, acceptance.SubjectCID)
}

func TestCommunityRecordWriteQueue_ALostClaimSendsNoWrite(t *testing.T) {
	// The lease runs out during the pre-read and another worker claims the
	// row: the stale attempt must not write to the community's repo, and must
	// not count a failure against the row it no longer holds.
	harness := newQueueHarness()
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.repo.beforeAcceptanceRead = func() {
		harness.store.stealCommunityRecordClaim(queueHostedDID, queuePostURI)
	}

	harness.attempt(t, queueHostedDID, queuePostURI)

	assert.Equal(t, 0, harness.repo.writeCallCount(), "an attempt that lost its claim sends nothing to the PDS")
	assert.Equal(t, 0, harness.repo.commitCount())
	write := harness.row(t, queueHostedDID, queuePostURI)
	assert.False(t, write.Completed)
	assert.Equal(t, 0, write.Attempts)
	assert.Empty(t, write.LastFailureCode)
	assert.Nil(t, write.LastWriteStartedAt)
}

func TestCommunityRecordWriteQueue_AFailedOutcomeWriteDoesNotStopTheSweep(t *testing.T) {
	harness := newQueueHarness()
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queueComment, "action-a", "spam", queueStart))
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-b", "spam", queueStart))
	// The comment's row sorts first in the batch.
	harness.store.failCommunityRecordOutcome = map[moderation.CommunityRecordSubject]error{
		{CommunityDID: queueHostedDID, SubjectURI: queueCommentURI}: errors.New("updating moderation_community_record_writes: connection reset"),
	}

	err := harness.queue.Sweep(t.Context())

	assert.Error(t, err, "the failed outcome write is reported")
	_, postRemoved := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.True(t, postRemoved, "the other claimed row is still attempted")
	assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
}

func TestCommunityRecordWriteQueue_ARemovalWithItsContentIsForeignBeforeAnyWrite(t *testing.T) {
	// A community moderator's removal of the same version, with the same code
	// and the same second, is identical to the queue's record. No write of the
	// queue's began, so it cannot be the queue's: it is neither adopted nor
	// undone.
	for _, subject := range []queueSubject{queuePost, queueComment} {
		t.Run(subject.name, func(t *testing.T) {
			harness := newQueueHarness()
			moderatorRemoval := ownFingerprint("bafyreimoderator", subject)
			harness.repo.setRemoval(queueHostedDID, moderatorRemoval)
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, subject, "action-a", "spam", queueStart))

			harness.attempt(t, queueHostedDID, subject.uri)

			write := harness.row(t, queueHostedDID, subject.uri)
			assert.True(t, write.Completed, "the moderator's removal already removes the subject")
			assert.Equal(t, "", write.OwnedRemovalCID, "a removal standing before any write is not adopted")

			harness.clock.Set(queueStart.Add(3 * time.Minute))
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, subject.uri, "action-a", restoreCIDFor(subject)))
			harness.attempt(t, queueHostedDID, subject.uri)

			standing, ok := harness.repo.removal(queueHostedDID, subject.uri)
			require.True(t, ok, "restoring must not undo the moderator's removal")
			assert.Equal(t, moderatorRemoval, standing)
			assert.Equal(t, 0, harness.repo.commitCount())
		})
	}
}

func TestCommunityRecordWriteQueue_RestoreBeforeAdoptionWritesBackTheAcceptanceSeen(t *testing.T) {
	// A removal write saw an acceptance and lost its response; the restore
	// reaches the row before any attempt adopted the removal.
	harness := newQueueHarness()
	harness.repo.setRemoval(queueHostedDID, ownFingerprint("bafyreilostresponse", queuePost))
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.store.updateCommunityRecordWrite(queueHostedDID, queuePostURI, func(write *moderation.CommunityRecordWrite) {
		startedAt := queueStart
		write.LastWriteStartedAt = &startedAt
		write.AcceptanceSeen = true
	})
	harness.clock.Set(queueStart.Add(5 * time.Minute))
	require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))

	harness.attempt(t, queueHostedDID, queuePostURI)

	_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
	assert.False(t, removalStands)
	acceptance, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	require.True(t, acceptanceStands, "the acceptance seen before the lost write is written back")
	assert.Equal(t, queueRestoredCID, acceptance.SubjectCID)
	assert.Equal(t, []recordedStamp{{
		kind: "acceptance", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", pinnedCID: queueRestoredCID,
		acceptanceURI: queueAcceptanceURI, acceptanceRkey: queueAcceptanceRkey,
	}}, harness.stamper.recorded())
}

func TestCommunityRecordWriteQueue_ASkippedUndoStampsNothing(t *testing.T) {
	// A community moderator replaces the instance's removal just before the
	// undo commits: the undo writes nothing, and the admission row, which the
	// moderator's echo will move, is not stamped.
	harness := newQueueHarness()
	harness.repo.setRemoval(queueHostedDID, ownFingerprint("bafyreiours", queuePost))
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
	harness.store.updateCommunityRecordWrite(queueHostedDID, queuePostURI, func(write *moderation.CommunityRecordWrite) {
		write.OwnedRemovalCID = "bafyreiours"
		write.WithdrewAcceptance = true
		write.Completed = true
	})
	require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
	moderatorRemoval := posts.StandingRemoval{CID: "bafyreimoderator", SubjectURI: queuePostURI, SubjectCID: queuePostCID, Code: "off-topic", CreatedAt: "2026-11-11T12:30:00Z"}
	harness.repo.beforeUndo = func() { harness.repo.setRemoval(queueHostedDID, moderatorRemoval) }

	harness.attempt(t, queueHostedDID, queuePostURI)

	assert.Empty(t, harness.stamper.recorded(), "a skipped undo committed nothing to stamp")
	standing, ok := harness.repo.removal(queueHostedDID, queuePostURI)
	require.True(t, ok)
	assert.Equal(t, moderatorRemoval, standing)
	_, acceptanceStands := harness.repo.acceptance(queueHostedDID, queuePostURI)
	assert.False(t, acceptanceStands)
	assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
}

// ---------------------------------------------------------------------------
// Review round 2: sweep leases, restore stamps across takeovers
// ---------------------------------------------------------------------------

func TestCommunityRecordWriteQueue_ASweepAttemptsEachRowOnAFullLease(t *testing.T) {
	// The first row's write takes most of the lease. The second row's attempt
	// must still start on a whole lease of its own, so no other worker can
	// claim it while it is in flight.
	harness := newQueueHarness()
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queueComment, "action-a", "spam", queueStart))
	harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-b", "spam", queueStart))
	secondKey := moderation.CommunityRecordSubject{CommunityDID: queueHostedDID, SubjectURI: queuePostURI}
	secondAttemptStart := queueStart.Add(100 * time.Second)

	var secondWriteRan bool
	var leaseDuringSecondWrite time.Time
	var concurrentClaim []moderation.CommunityRecordWrite
	secondWrite := func(ctx context.Context) error {
		secondWriteRan = true
		harness.store.mu.Lock()
		leaseDuringSecondWrite = harness.store.communityRecordLeases[secondKey]
		harness.store.mu.Unlock()
		harness.clock.Set(secondAttemptStart.Add(90 * time.Second))
		claimed, err := harness.store.ClaimDueCommunityRecordWrites(ctx, moderation.CommunityRecordWriteClaim{
			DueAt: harness.clock.Now(), Lease: 2 * time.Minute, Limit: 1, Subject: &secondKey,
		})
		concurrentClaim = claimed
		return err
	}
	// The comment's row sorts first in the batch.
	harness.repo.blockWrite = func(context.Context) error {
		harness.clock.Set(secondAttemptStart)
		harness.repo.mu.Lock()
		harness.repo.blockWrite = secondWrite
		harness.repo.mu.Unlock()
		return nil
	}

	require.NoError(t, harness.queue.Sweep(t.Context()))

	require.True(t, secondWriteRan, "fixture: the second row's write ran")
	assert.False(t, leaseDuringSecondWrite.Before(secondAttemptStart.Add(2*time.Minute)),
		"the second row's lease must run a full two minutes past its attempt's start at %s; the store holds it until %s",
		secondAttemptStart, leaseDuringSecondWrite)
	assert.Empty(t, concurrentClaim, "another worker claimed the second row 90 seconds into its attempt")
}

func TestCommunityRecordWriteQueue_AFailedTakeoverKeepsTheRestoreStampOwed(t *testing.T) {
	// A's restore undid its removal but its admission stamp failed. B removes
	// again, and B's attempt fails before committing anything. Once B is
	// restored too, the admission row must still get A's undo stamp.
	for _, row := range []struct {
		name string
		fail func(repo *fakeInstanceRemovalRepo)
	}{
		{name: "the acceptance read fails", fail: func(repo *fakeInstanceRemovalRepo) {
			repo.failAcceptanceReads = []error{errors.New("dial tcp: connection refused")}
		}},
		{name: "the removal write fails", fail: func(repo *fakeInstanceRemovalRepo) {
			repo.failWrites = []error{errors.New("dial tcp: connection refused")}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			harness := newQueueHarness()
			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-a", "spam", queueStart))
			harness.attempt(t, queueHostedDID, queuePostURI)
			require.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed, "fixture: A's removal written")

			removedAgainAt := queueStart.Add(time.Minute)
			harness.clock.Set(removedAgainAt)
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-a", queueRestoredCID))
			harness.stamper.failNext("removal-delete")
			harness.attempt(t, queueHostedDID, queuePostURI)
			_, removalStands := harness.repo.removal(queueHostedDID, queuePostURI)
			require.False(t, removalStands, "fixture: A's undo committed")
			require.Equal(t, "3krev0002", harness.row(t, queueHostedDID, queuePostURI).RestoreStampRev, "fixture: A's restore stamp is owed")

			harness.store.seedCommunityRecordRemoval(removalRow(queueHostedDID, queuePost, "action-b", "rule-violation", removedAgainAt))
			row.fail(harness.repo)
			harness.attempt(t, queueHostedDID, queuePostURI)
			require.Equal(t, 2, harness.repo.commitCount(), "fixture: B's attempt committed nothing")
			require.False(t, harness.row(t, queueHostedDID, queuePostURI).Completed, "fixture: B's attempt failed")

			harness.clock.Set(queueStart.Add(5 * time.Minute))
			require.True(t, harness.store.seedCommunityRecordRestore(queueHostedDID, queuePostURI, "action-b", queueRestoredCID))
			harness.attempt(t, queueHostedDID, queuePostURI)

			assert.Equal(t, []recordedStamp{
				{kind: "removal", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0001", decisionCode: "spam"},
				{kind: "removal-delete", communityDID: queueHostedDID, postURI: queuePostURI, rev: "3krev0002"},
			}, harness.stamper.recorded(), "A's undo stamp is still owed after B's failed attempt")
			assert.True(t, harness.row(t, queueHostedDID, queuePostURI).Completed)
		})
	}
}
