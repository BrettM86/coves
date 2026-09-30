package moderation_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"Coves/internal/core/moderation"
)

type inMemoryModerationDecisionKey struct {
	authorityDID string
	subjectURI   string
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
	idempotency     map[inMemoryModerationIdempotencyKey]moderation.IdempotencyRecord
	mediaBlocks     map[moderation.MediaBlock]bool
	nextActionID    int
}

func (state inMemoryModerationState) copy() inMemoryModerationState {
	working := inMemoryModerationState{
		indexedComments: make(map[string]moderation.IndexedComment, len(state.indexedComments)),
		indexedPosts:    make(map[string]moderation.IndexedPost, len(state.indexedPosts)),
		versions:        make(map[string]int64, len(state.versions)),
		actions:         make(map[string]moderation.Action, len(state.actions)),
		activeRemovals:  make(map[inMemoryModerationDecisionKey]string, len(state.activeRemovals)),
		idempotency:     make(map[inMemoryModerationIdempotencyKey]moderation.IdempotencyRecord, len(state.idempotency)),
		mediaBlocks:     make(map[moderation.MediaBlock]bool, len(state.mediaBlocks)),
		nextActionID:    state.nextActionID,
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
			idempotency:     make(map[inMemoryModerationIdempotencyKey]moderation.IdempotencyRecord),
			mediaBlocks:     make(map[moderation.MediaBlock]bool),
		},
	}
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
	return state, nil
}

func (store *inMemoryModerationStore) ListActions(_ context.Context, query moderation.ActionListQuery) ([]moderation.Action, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.listQueries = append(store.listQueries, query)
	if store.failListActions != nil {
		return nil, store.failListActions
	}
	return append([]moderation.Action(nil), store.listRows...), nil
}

type inMemoryModerationTransaction struct {
	store *inMemoryModerationStore
	state *inMemoryModerationState
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
	key := inMemoryModerationIdempotencyKey{record.ActorDID, record.AuthorityDID, record.Key}
	transaction.state.idempotency[key] = record
	return nil
}

func (transaction *inMemoryModerationTransaction) LockSubject(_ context.Context, subjectURI string) (int64, error) {
	version := transaction.state.versions[subjectURI]
	transaction.state.versions[subjectURI] = version
	return version, nil
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

func (transaction *inMemoryModerationTransaction) DeactivateMediaBlocks(_ context.Context, actionID string) error {
	transaction.store.writeCalls = append(transaction.store.writeCalls, "DeactivateMediaBlocks")
	for block := range transaction.state.mediaBlocks {
		if block.ActionID == actionID {
			transaction.state.mediaBlocks[block] = false
		}
	}
	return nil
}

var _ moderation.Store = (*inMemoryModerationStore)(nil)
var _ moderation.Transaction = (*inMemoryModerationTransaction)(nil)
