package moderation_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
)

type inMemoryModerationDecisionKey struct {
	authorityDID string
	subjectURI   string
}

type inMemoryModerationLabelKey struct {
	authorityDID string
	subjectURI   string
	value        string
}

type inMemoryModerationLabelDecision struct {
	actionID string
	active   bool
}

type inMemoryModerationIdempotencyKey struct {
	actorDID     string
	authorityDID string
	key          string
}

type inMemoryModerationState struct {
	indexedComments map[string]moderation.IndexedComment
	indexedPosts    map[string]moderation.IndexedPost
	versions        map[string]int64
	actions         map[string]moderation.Action
	activeRemovals  map[inMemoryModerationDecisionKey]string
	labelDecisions  map[inMemoryModerationLabelKey]inMemoryModerationLabelDecision
	idempotency     map[inMemoryModerationIdempotencyKey]moderation.IdempotencyRecord
	mediaBlocks     map[moderation.MediaBlock]bool
	cdnPurgeTargets []imageproxy.BlockedBlob
	nextActionID    int
	// communityRecordWrites are the outbox rows, one per subject in a hosted
	// community. They roll back with the transaction that wrote them.
	communityRecordWrites map[moderation.CommunityRecordSubject]moderation.CommunityRecordWrite
}

func (state inMemoryModerationState) copy() inMemoryModerationState {
	working := inMemoryModerationState{
		indexedComments: make(map[string]moderation.IndexedComment, len(state.indexedComments)),
		indexedPosts:    make(map[string]moderation.IndexedPost, len(state.indexedPosts)),
		versions:        make(map[string]int64, len(state.versions)),
		actions:         make(map[string]moderation.Action, len(state.actions)),
		activeRemovals:  make(map[inMemoryModerationDecisionKey]string, len(state.activeRemovals)),
		labelDecisions:  make(map[inMemoryModerationLabelKey]inMemoryModerationLabelDecision, len(state.labelDecisions)),
		idempotency:     make(map[inMemoryModerationIdempotencyKey]moderation.IdempotencyRecord, len(state.idempotency)),
		mediaBlocks:     make(map[moderation.MediaBlock]bool, len(state.mediaBlocks)),
		cdnPurgeTargets: append([]imageproxy.BlockedBlob(nil), state.cdnPurgeTargets...),
		nextActionID:    state.nextActionID,
		communityRecordWrites: make(map[moderation.CommunityRecordSubject]moderation.CommunityRecordWrite,
			len(state.communityRecordWrites)),
	}
	for key, write := range state.communityRecordWrites {
		working.communityRecordWrites[key] = copyCommunityRecordWrite(write)
	}
	for key, comment := range state.indexedComments {
		comment.ImageCIDs = append([]string(nil), comment.ImageCIDs...)
		working.indexedComments[key] = comment
	}
	for key, post := range state.indexedPosts {
		post.BlobCIDs = append([]string(nil), post.BlobCIDs...)
		working.indexedPosts[key] = post
	}
	for key, version := range state.versions {
		working.versions[key] = version
	}
	for key, action := range state.actions {
		working.actions[key] = action
	}
	for key, actionID := range state.activeRemovals {
		working.activeRemovals[key] = actionID
	}
	for key, decision := range state.labelDecisions {
		working.labelDecisions[key] = decision
	}
	for key, record := range state.idempotency {
		working.idempotency[key] = record
	}
	for block, active := range state.mediaBlocks {
		working.mediaBlocks[block] = active
	}
	return working
}

// Each transaction holds the store lock and changes a private snapshot. The
// write log includes attempts even if an error rolls the snapshot back.
type inMemoryModerationStore struct {
	mu                     sync.Mutex
	state                  inMemoryModerationState
	now                    time.Time
	writeCalls             []string
	listQueries            []moderation.ActionListQuery
	listRows               []moderation.Action
	failInsertAction       error
	failSetRemovalDecision error
	failListActions        error
	failSetLabelDecision   error
	// failActiveLabelsAfterLabelDecision fails ActiveLabels reads made after
	// the transaction has changed a label decision.
	failActiveLabelsAfterLabelDecision error
	// accountSubjectFacts are the facts AccountSubjectFacts returns per DID;
	// a DID with no entry has no account activity and no community row.
	accountSubjectFacts     map[string]moderation.AccountSubjectFacts
	failAccountSubjectFacts error
	// failSaveIdempotencyRecord fails the last write of a mutation, rolling
	// back everything the transaction wrote before it.
	failSaveIdempotencyRecord error
	// hostedCommunities are the community DIDs whose PDS credentials this
	// AppView holds.
	hostedCommunities map[string]bool
	// outboxNow is the clock the community record write rows are scheduled
	// by; nil uses now.
	outboxNow func() time.Time
	// communityRecordLeases holds each claimed row's lease expiry.
	communityRecordLeases    map[moderation.CommunityRecordSubject]time.Time
	nextCommunityRecordClaim int64
	// failCommunityRecordOutcome fails the next Complete or Reschedule of a
	// subject's row once, before anything is persisted.
	failCommunityRecordOutcome map[moderation.CommunityRecordSubject]error
}

func newInMemoryModerationStore(now time.Time) *inMemoryModerationStore {
	return &inMemoryModerationStore{
		now: now,
		state: inMemoryModerationState{
			indexedComments: make(map[string]moderation.IndexedComment),
			indexedPosts:    make(map[string]moderation.IndexedPost),
			versions:        make(map[string]int64),
			actions:         make(map[string]moderation.Action),
			activeRemovals:  make(map[inMemoryModerationDecisionKey]string),
			labelDecisions:  make(map[inMemoryModerationLabelKey]inMemoryModerationLabelDecision),
			idempotency:     make(map[inMemoryModerationIdempotencyKey]moderation.IdempotencyRecord),
			mediaBlocks:     make(map[moderation.MediaBlock]bool),

			communityRecordWrites: make(map[moderation.CommunityRecordSubject]moderation.CommunityRecordWrite),
		},
		hostedCommunities:     make(map[string]bool),
		communityRecordLeases: make(map[moderation.CommunityRecordSubject]time.Time),
	}
}

func (store *inMemoryModerationStore) seedActiveLabel(action moderation.Action) {
	store.state.actions[action.ID] = action
	store.state.labelDecisions[inMemoryModerationLabelKey{action.AuthorityDID, action.SubjectURI, action.LabelValue}] = inMemoryModerationLabelDecision{actionID: action.ID, active: true}
}

func (store *inMemoryModerationStore) InTransaction(ctx context.Context, fn func(context.Context, moderation.Transaction) error) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	working := store.state.copy()
	transaction := &inMemoryModerationTransaction{store: store, state: &working}
	if err := fn(ctx, transaction); err != nil {
		return err
	}
	store.state = working
	return nil
}

func (store *inMemoryModerationStore) SubjectModeration(_ context.Context, authorityDID, subjectURI string) (*moderation.SubjectModeration, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	state := &moderation.SubjectModeration{Version: store.state.versions[subjectURI]}
	key := inMemoryModerationDecisionKey{authorityDID: authorityDID, subjectURI: subjectURI}
	if actionID := store.state.activeRemovals[key]; actionID != "" {
		action := store.state.actions[actionID]
		state.ActiveRemoval = &action
	}
	var values []string
	for key, decision := range store.state.labelDecisions {
		if key.authorityDID == authorityDID && key.subjectURI == subjectURI && decision.active {
			values = append(values, key.value)
		}
	}
	sort.Strings(values)
	for _, value := range values {
		key := inMemoryModerationLabelKey{authorityDID, subjectURI, value}
		state.ActiveLabels = append(state.ActiveLabels, store.state.actions[store.state.labelDecisions[key].actionID])
	}
	return state, nil
}

func (store *inMemoryModerationStore) ListActions(_ context.Context, query moderation.ActionListQuery) ([]moderation.Action, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.listQueries = append(store.listQueries, query)
	if store.failListActions != nil {
		return nil, store.failListActions
	}
	rows := make([]moderation.Action, 0, len(store.listRows))
	for _, row := range store.listRows {
		excluded := false
		for _, kind := range moderation.PublicExcludedActions() {
			excluded = excluded || (query.ExcludeLabelActions && row.Action == kind)
		}
		if !excluded {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type inMemoryModerationTransaction struct {
	store                *inMemoryModerationStore
	state                *inMemoryModerationState
	labelDecisionWritten bool
	// lockedSubjects are the subject URIs LockSubject has locked.
	lockedSubjects map[string]bool
}

func (*inMemoryModerationTransaction) LockActor(context.Context, string) error {
	// InTransaction already serializes writers to the store.
	return nil
}

func (transaction *inMemoryModerationTransaction) LiveIdempotencyRecord(_ context.Context, actorDID, authorityDID, key string, now time.Time) (*moderation.IdempotencyRecord, error) {
	record, exists := transaction.state.idempotency[inMemoryModerationIdempotencyKey{actorDID, authorityDID, key}]
	if !exists || !now.Before(record.ExpiresAt) {
		return nil, nil
	}
	return &record, nil
}

func (transaction *inMemoryModerationTransaction) CountLiveIdempotencyKeys(_ context.Context, actorDID string, now time.Time) (int, error) {
	count := 0
	for key, record := range transaction.state.idempotency {
		if key.actorDID == actorDID && now.Before(record.ExpiresAt) {
			count++
		}
	}
	return count, nil
}

func (transaction *inMemoryModerationTransaction) SaveIdempotencyRecord(_ context.Context, record moderation.IdempotencyRecord) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "SaveIdempotencyRecord")
	if transaction.store.failSaveIdempotencyRecord != nil {
		return transaction.store.failSaveIdempotencyRecord
	}
	key := inMemoryModerationIdempotencyKey{record.ActorDID, record.AuthorityDID, record.Key}
	transaction.state.idempotency[key] = record
	return nil
}

func (transaction *inMemoryModerationTransaction) LockSubject(_ context.Context, subjectURI string) (int64, error) {
	if transaction.lockedSubjects == nil {
		transaction.lockedSubjects = make(map[string]bool)
	}
	transaction.lockedSubjects[subjectURI] = true
	version := transaction.state.versions[subjectURI]
	transaction.state.versions[subjectURI] = version
	return version, nil
}

// AccountSubjectFacts refuses a read made before LockSubject on at://<did>:
// facts read outside the subject lock are not serialized with first activity.
func (transaction *inMemoryModerationTransaction) AccountSubjectFacts(_ context.Context, did string) (moderation.AccountSubjectFacts, error) {
	if !transaction.lockedSubjects["at://"+did] {
		return moderation.AccountSubjectFacts{}, fmt.Errorf("account subject facts of %s read before LockSubject on at://%s", did, did)
	}
	if transaction.store.failAccountSubjectFacts != nil {
		return moderation.AccountSubjectFacts{}, transaction.store.failAccountSubjectFacts
	}
	return transaction.store.accountSubjectFacts[did], nil
}

func (transaction *inMemoryModerationTransaction) ReadIndexedComment(_ context.Context, subjectURI string) (*moderation.IndexedComment, error) {
	comment, exists := transaction.state.indexedComments[subjectURI]
	if !exists {
		return nil, moderation.ErrSubjectNotIndexed
	}
	comment.ImageCIDs = append([]string(nil), comment.ImageCIDs...)
	return &comment, nil
}

func (transaction *inMemoryModerationTransaction) ReadIndexedPost(_ context.Context, subjectURI string) (*moderation.IndexedPost, error) {
	post, exists := transaction.state.indexedPosts[subjectURI]
	if !exists {
		return nil, moderation.ErrSubjectNotIndexed
	}
	post.BlobCIDs = append([]string(nil), post.BlobCIDs...)
	return &post, nil
}

func (transaction *inMemoryModerationTransaction) GetAction(_ context.Context, actionID string) (*moderation.Action, error) {
	action, exists := transaction.state.actions[actionID]
	if !exists {
		return nil, moderation.ErrDecisionNotFound
	}
	return &action, nil
}

func (transaction *inMemoryModerationTransaction) ActiveRemoval(_ context.Context, authorityDID, subjectURI string) (*moderation.Action, error) {
	key := inMemoryModerationDecisionKey{authorityDID: authorityDID, subjectURI: subjectURI}
	actionID := transaction.state.activeRemovals[key]
	if actionID == "" {
		return nil, nil
	}
	action := transaction.state.actions[actionID]
	return &action, nil
}

func (transaction *inMemoryModerationTransaction) ActiveLabels(_ context.Context, authorityDID, subjectURI string) ([]moderation.Action, error) {
	if transaction.labelDecisionWritten && transaction.store.failActiveLabelsAfterLabelDecision != nil {
		return nil, transaction.store.failActiveLabelsAfterLabelDecision
	}
	var values []string
	for key, decision := range transaction.state.labelDecisions {
		if key.authorityDID == authorityDID && key.subjectURI == subjectURI && decision.active {
			values = append(values, key.value)
		}
	}
	sort.Strings(values)
	actions := make([]moderation.Action, 0, len(values))
	for _, value := range values {
		key := inMemoryModerationLabelKey{authorityDID, subjectURI, value}
		actions = append(actions, transaction.state.actions[transaction.state.labelDecisions[key].actionID])
	}
	return actions, nil
}

func (transaction *inMemoryModerationTransaction) SetLabelDecision(_ context.Context, authorityDID, subjectURI, value, actionID string, active bool) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "SetLabelDecision")
	if transaction.store.failSetLabelDecision != nil {
		return transaction.store.failSetLabelDecision
	}
	key := inMemoryModerationLabelKey{authorityDID, subjectURI, value}
	decision, exists := transaction.state.labelDecisions[key]
	if active {
		if _, actionExists := transaction.state.actions[actionID]; !actionExists {
			return moderation.ErrDecisionNotFound
		}
		if exists && decision.active {
			return fmt.Errorf("label decision %q on %s is already active", value, subjectURI)
		}
		transaction.state.labelDecisions[key] = inMemoryModerationLabelDecision{actionID: actionID, active: true}
	} else {
		if !exists || !decision.active || decision.actionID != actionID {
			return fmt.Errorf("no active label decision %q on %s for action %s", value, subjectURI, actionID)
		}
		decision.active = false
		transaction.state.labelDecisions[key] = decision
	}
	transaction.labelDecisionWritten = true
	return nil
}

func (transaction *inMemoryModerationTransaction) InsertContentSnapshot(context.Context, string, string, moderation.ContentSnapshot) error {
	return nil
}

func (transaction *inMemoryModerationTransaction) DeleteSubjectContentSnapshots(context.Context, string) error {
	return nil
}

func (transaction *inMemoryModerationTransaction) InsertAction(_ context.Context, action moderation.Action) (*moderation.Action, error) {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "InsertAction")
	if transaction.store.failInsertAction != nil {
		return nil, transaction.store.failInsertAction
	}
	for {
		transaction.state.nextActionID++
		action.ID = fmt.Sprintf("action-%d", transaction.state.nextActionID)
		if _, exists := transaction.state.actions[action.ID]; !exists {
			break
		}
	}
	action.CreatedAt = transaction.store.now
	transaction.state.actions[action.ID] = action
	return &action, nil
}

func (transaction *inMemoryModerationTransaction) SetRemovalDecision(_ context.Context, authorityDID, subjectURI, actionID string, active bool) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "SetRemovalDecision")
	if transaction.store.failSetRemovalDecision != nil {
		return transaction.store.failSetRemovalDecision
	}
	key := inMemoryModerationDecisionKey{authorityDID: authorityDID, subjectURI: subjectURI}
	if active {
		if _, exists := transaction.state.actions[actionID]; !exists {
			return moderation.ErrDecisionNotFound
		}
		transaction.state.activeRemovals[key] = actionID
	} else {
		delete(transaction.state.activeRemovals, key)
	}
	return nil
}

func (transaction *inMemoryModerationTransaction) SetSubjectVersion(_ context.Context, subjectURI string, version int64) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "SetSubjectVersion")
	transaction.state.versions[subjectURI] = version
	return nil
}

func (transaction *inMemoryModerationTransaction) InsertMediaBlocks(_ context.Context, blocks []moderation.MediaBlock) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "InsertMediaBlocks")
	for _, block := range blocks {
		transaction.state.mediaBlocks[block] = true
	}
	return nil
}

func (transaction *inMemoryModerationTransaction) RecordCDNPurgeTargets(_ context.Context, blobs []imageproxy.BlockedBlob) error {
	transaction.state.cdnPurgeTargets = append(transaction.state.cdnPurgeTargets, blobs...)
	return nil
}

// RependCDNPurgeTargets returns each distinct recorded target of the CIDs; the
// fake keeps no target state to move back to pending.
func (transaction *inMemoryModerationTransaction) RependCDNPurgeTargets(_ context.Context, cids []string) ([]imageproxy.BlockedBlob, error) {
	return distinctTargetsOfCIDs(transaction.state.cdnPurgeTargets, cids), nil
}

func distinctTargetsOfCIDs(recorded []imageproxy.BlockedBlob, cids []string) []imageproxy.BlockedBlob {
	var repended []imageproxy.BlockedBlob
	seen := make(map[imageproxy.BlockedBlob]bool)
	for _, blob := range recorded {
		if !slices.Contains(cids, blob.CID) || seen[blob] {
			continue
		}
		seen[blob] = true
		repended = append(repended, blob)
	}
	return repended
}

func (transaction *inMemoryModerationTransaction) DeactivateMediaBlocks(_ context.Context, actionID string) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "DeactivateMediaBlocks")
	for block := range transaction.state.mediaBlocks {
		if block.ActionID == actionID {
			transaction.state.mediaBlocks[block] = false
		}
	}
	return nil
}

func (*inMemoryModerationTransaction) InsertLabel(context.Context, labels.Issue) error { return nil }

// ---------------------------------------------------------------------------
// Community record writes: the outbox rows, in the transaction and for the
// queue. The semantics are the moderation.Transaction and
// moderation.CommunityRecordWriteStore contracts.
// ---------------------------------------------------------------------------

func copyCommunityRecordWrite(write moderation.CommunityRecordWrite) moderation.CommunityRecordWrite {
	if write.LastWriteStartedAt != nil {
		startedAt := *write.LastWriteStartedAt
		write.LastWriteStartedAt = &startedAt
	}
	return write
}

func (store *inMemoryModerationStore) outboxClock() time.Time {
	if store.outboxNow != nil {
		return store.outboxNow()
	}
	return store.now
}

// repend makes a row pending and due now under a new generation.
func repend(write *moderation.CommunityRecordWrite, now time.Time) {
	write.Generation++
	write.Completed = false
	write.NextAttemptAt = now
	write.Attempts = 0
	write.LastFailureCode = ""
}

func requireCommunityRecordRemoval(state *inMemoryModerationState, write moderation.CommunityRecordWrite, now time.Time) {
	key := moderation.CommunityRecordSubject{CommunityDID: write.CommunityDID, SubjectURI: write.SubjectURI}
	existing, exists := state.communityRecordWrites[key]
	var next moderation.CommunityRecordWrite
	if exists {
		// What the row knows about the repo (every attempt progress field)
		// survives a new owning action, and so do its generation and claim.
		next = copyCommunityRecordWrite(existing)
	}
	next.CommunityDID, next.SubjectURI = write.CommunityDID, write.SubjectURI
	next.SubjectCollection, next.SubjectCID = write.SubjectCollection, write.SubjectCID
	next.RemovalActionID, next.Code, next.RemovalCreatedAt = write.RemovalActionID, write.Code, write.RemovalCreatedAt
	next.Desired = moderation.CommunityRecordRemoved
	next.RestoreAcceptanceCID = ""
	repend(&next, now)
	state.communityRecordWrites[key] = next
}

func requestCommunityRecordRestore(state *inMemoryModerationState, communityDID, subjectURI, removalActionID, acceptanceCID string, now time.Time) bool {
	key := moderation.CommunityRecordSubject{CommunityDID: communityDID, SubjectURI: subjectURI}
	write, exists := state.communityRecordWrites[key]
	if !exists || write.RemovalActionID != removalActionID {
		return false
	}
	write.Desired = moderation.CommunityRecordRestored
	write.RestoreAcceptanceCID = acceptanceCID
	repend(&write, now)
	state.communityRecordWrites[key] = write
	return true
}

func (transaction *inMemoryModerationTransaction) IsHostedCommunity(_ context.Context, communityDID string) (bool, error) {
	return transaction.store.hostedCommunities[communityDID], nil
}

func (transaction *inMemoryModerationTransaction) RequireCommunityRecordRemoval(_ context.Context, write moderation.CommunityRecordWrite) error {
	requireCommunityRecordRemoval(transaction.state, write, transaction.store.outboxClock())
	return nil
}

func (transaction *inMemoryModerationTransaction) RequestCommunityRecordRestore(_ context.Context, communityDID, subjectURI, removalActionID, acceptanceCID string) (bool, error) {
	return requestCommunityRecordRestore(transaction.state, communityDID, subjectURI, removalActionID, acceptanceCID, transaction.store.outboxClock()), nil
}

// seedCommunityRecordRemoval commits a removal row outside a mutation.
func (store *inMemoryModerationStore) seedCommunityRecordRemoval(write moderation.CommunityRecordWrite) {
	store.mu.Lock()
	defer store.mu.Unlock()
	requireCommunityRecordRemoval(&store.state, write, store.outboxClock())
}

// seedCommunityRecordRestore commits a restore request outside a mutation.
func (store *inMemoryModerationStore) seedCommunityRecordRestore(communityDID, subjectURI, removalActionID, acceptanceCID string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return requestCommunityRecordRestore(&store.state, communityDID, subjectURI, removalActionID, acceptanceCID, store.outboxClock())
}

// communityRecordWrite reads a committed row.
func (store *inMemoryModerationStore) communityRecordWrite(communityDID, subjectURI string) (moderation.CommunityRecordWrite, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	write, exists := store.state.communityRecordWrites[moderation.CommunityRecordSubject{CommunityDID: communityDID, SubjectURI: subjectURI}]
	return copyCommunityRecordWrite(write), exists
}

// committedCommunityRecordWriteCount is how many rows are committed.
func (store *inMemoryModerationStore) committedCommunityRecordWriteCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.state.communityRecordWrites)
}

// markCommunityRecordWriteCompleted stands in for a worker finishing a row.
func (store *inMemoryModerationStore) markCommunityRecordWriteCompleted(communityDID, subjectURI string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := moderation.CommunityRecordSubject{CommunityDID: communityDID, SubjectURI: subjectURI}
	write := store.state.communityRecordWrites[key]
	write.Completed = true
	store.state.communityRecordWrites[key] = write
}

// updateCommunityRecordWrite changes a committed row in place, standing in for
// attempts that already ran.
func (store *inMemoryModerationStore) updateCommunityRecordWrite(communityDID, subjectURI string, change func(*moderation.CommunityRecordWrite)) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := moderation.CommunityRecordSubject{CommunityDID: communityDID, SubjectURI: subjectURI}
	write := store.state.communityRecordWrites[key]
	change(&write)
	store.state.communityRecordWrites[key] = write
}

func (store *inMemoryModerationStore) ClaimDueCommunityRecordWrites(_ context.Context, claim moderation.CommunityRecordWriteClaim) ([]moderation.CommunityRecordWrite, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.outboxClock()
	keys := make([]moderation.CommunityRecordSubject, 0, len(store.state.communityRecordWrites))
	for key := range store.state.communityRecordWrites {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].CommunityDID != keys[j].CommunityDID {
			return keys[i].CommunityDID < keys[j].CommunityDID
		}
		return keys[i].SubjectURI < keys[j].SubjectURI
	})
	var claimed []moderation.CommunityRecordWrite
	for _, key := range keys {
		if claim.Limit > 0 && len(claimed) >= claim.Limit {
			break
		}
		write := store.state.communityRecordWrites[key]
		switch {
		case write.Completed,
			write.NextAttemptAt.After(claim.DueAt),
			store.communityRecordLeases[key].After(now),
			claim.Subject != nil && *claim.Subject != key,
			slices.Contains(claim.ExcludedSubjects(), key):
			continue
		}
		store.nextCommunityRecordClaim++
		write.Claim = store.nextCommunityRecordClaim
		store.state.communityRecordWrites[key] = write
		store.communityRecordLeases[key] = now.Add(claim.Lease)
		claimed = append(claimed, copyCommunityRecordWrite(write))
	}
	return claimed, nil
}

// withTransactionFields keeps on progress the fields only a moderation
// transaction or the store's own scheduling may change. Every other field of a
// CommunityRecordWrite is attempt progress: persisted under the claim and kept
// across a takeover or a restore request. A field added for the transaction to
// own belongs in this list.
func withTransactionFields(progress *moderation.CommunityRecordWrite, row moderation.CommunityRecordWrite) {
	progress.CommunityDID, progress.SubjectURI = row.CommunityDID, row.SubjectURI
	progress.SubjectCollection, progress.SubjectCID = row.SubjectCollection, row.SubjectCID
	progress.RemovalActionID, progress.Code, progress.RemovalCreatedAt = row.RemovalActionID, row.Code, row.RemovalCreatedAt
	progress.Desired, progress.RestoreAcceptanceCID = row.Desired, row.RestoreAcceptanceCID
	progress.Completed, progress.Attempts = row.Completed, row.Attempts
	progress.NextAttemptAt, progress.LastFailureCode = row.NextAttemptAt, row.LastFailureCode
	progress.Generation, progress.Claim = row.Generation, row.Claim
}

// persistCommunityRecordProgress copies what an attempt learned about the repo
// onto the row, if the attempt still holds its claim; otherwise it reports
// moderation.ErrCommunityRecordClaimLost and changes nothing.
func (store *inMemoryModerationStore) persistCommunityRecordProgress(write moderation.CommunityRecordWrite) (moderation.CommunityRecordWrite, error) {
	key := moderation.CommunityRecordSubject{CommunityDID: write.CommunityDID, SubjectURI: write.SubjectURI}
	row, exists := store.state.communityRecordWrites[key]
	if !exists || row.Claim != write.Claim {
		return row, fmt.Errorf("community %s subject %s claim %d: %w", write.CommunityDID, write.SubjectURI, write.Claim, moderation.ErrCommunityRecordClaimLost)
	}
	progress := copyCommunityRecordWrite(write)
	withTransactionFields(&progress, row)
	return progress, nil
}

// takeCommunityRecordOutcomeFailure consumes the injected outcome-write
// failure for the subject, if any.
func (store *inMemoryModerationStore) takeCommunityRecordOutcomeFailure(key moderation.CommunityRecordSubject) error {
	err := store.failCommunityRecordOutcome[key]
	delete(store.failCommunityRecordOutcome, key)
	return err
}

// stealCommunityRecordClaim stands for the row's lease running out and
// another worker claiming it: the attempt holding the old claim has lost it.
func (store *inMemoryModerationStore) stealCommunityRecordClaim(communityDID, subjectURI string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := moderation.CommunityRecordSubject{CommunityDID: communityDID, SubjectURI: subjectURI}
	write := store.state.communityRecordWrites[key]
	store.nextCommunityRecordClaim++
	write.Claim = store.nextCommunityRecordClaim
	store.state.communityRecordWrites[key] = write
	store.communityRecordLeases[key] = store.outboxClock().Add(2 * time.Minute)
}

func (store *inMemoryModerationStore) SaveCommunityRecordWriteProgress(_ context.Context, write moderation.CommunityRecordWrite) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	row, err := store.persistCommunityRecordProgress(write)
	if err != nil {
		return err
	}
	store.state.communityRecordWrites[moderation.CommunityRecordSubject{CommunityDID: write.CommunityDID, SubjectURI: write.SubjectURI}] = row
	return nil
}

func (store *inMemoryModerationStore) CompleteCommunityRecordWrite(_ context.Context, write moderation.CommunityRecordWrite) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := moderation.CommunityRecordSubject{CommunityDID: write.CommunityDID, SubjectURI: write.SubjectURI}
	if err := store.takeCommunityRecordOutcomeFailure(key); err != nil {
		return err
	}
	row, err := store.persistCommunityRecordProgress(write)
	if err != nil {
		return err
	}
	if row.Generation == write.Generation {
		row.Completed = true
	}
	store.state.communityRecordWrites[key] = row
	delete(store.communityRecordLeases, key)
	return nil
}

// RescheduleCommunityRecordWrite under a claim that still holds the row but an
// older generation persists the progress and leaves the new generation's
// schedule and failure count alone: the row stays due now.
func (store *inMemoryModerationStore) RescheduleCommunityRecordWrite(_ context.Context, write moderation.CommunityRecordWrite, nextAttemptAt time.Time, failureCode string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := moderation.CommunityRecordSubject{CommunityDID: write.CommunityDID, SubjectURI: write.SubjectURI}
	if err := store.takeCommunityRecordOutcomeFailure(key); err != nil {
		return err
	}
	row, err := store.persistCommunityRecordProgress(write)
	if err != nil {
		return err
	}
	if row.Generation == write.Generation {
		row.NextAttemptAt = nextAttemptAt
		if failureCode != "" {
			row.Attempts++
			row.LastFailureCode = failureCode
		}
	}
	store.state.communityRecordWrites[key] = row
	delete(store.communityRecordLeases, key)
	return nil
}

var (
	_ moderation.Store                     = (*inMemoryModerationStore)(nil)
	_ moderation.Transaction               = (*inMemoryModerationTransaction)(nil)
	_ moderation.CommunityRecordWriteStore = (*inMemoryModerationStore)(nil)
)
