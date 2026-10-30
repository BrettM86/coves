package votes

import (
	"context"
	"sync"
	"time"
)

const (
	// recentWriteRetention bounds how long this AppView's own write overrides
	// the index while Jetstream catches up (PRD §5.1). It has to cover
	// Jetstream lag; after it, the index is authoritative.
	recentWriteRetention = 24 * time.Hour

	// recentWritePruneLimit is how many expired entries each write removes.
	// Writes are the only way entries are added, so pruning on write keeps the
	// overlay proportional to recent writes without a background goroutine.
	recentWritePruneLimit = 64
)

// viewerSubjectKey identifies one viewer's vote on one subject.
type viewerSubjectKey struct {
	viewerDID  string
	subjectURI string
}

// recentWriteState is what this AppView last did to a viewer's vote on a subject.
type recentWriteState int

const (
	// recentWriteVoted: a vote was cast; the index may not show it yet.
	recentWriteVoted recentWriteState = iota + 1
	// recentWriteRemoved: a vote was deleted; the index may still show it.
	recentWriteRemoved
	// recentWriteUnverified: the last write's outcome is unknown, so the next
	// write for the key reads the PDS (PRD §5.4).
	recentWriteUnverified
)

// recentWrite is one overlay entry.
type recentWrite struct {
	recordedAt time.Time
	// uri is the vote cast (Voted) or deleted (Removed).
	uri string
	// direction describes the vote cast (Voted only).
	direction string
	// rkey is the vote's record key (Voted only).
	rkey string
	// request is the write whose outcome is unknown (Unverified only).
	request ambiguousRequest
	state   recentWriteState
	// version identifies this entry, so a read retires only the entry it
	// evaluated and never a newer write for the same key.
	version uint64
}

// ambiguousOperation is what an ambiguous write asked for. The zero value
// means no retry is recognized as the same request.
type ambiguousOperation int

const (
	// ambiguousOperationCast: CreateVote cast a vote in direction, by a plain
	// create or a direction switch.
	ambiguousOperationCast ambiguousOperation = iota + 1
	// ambiguousOperationToggleOff: CreateVote deleted the removedURI vote,
	// which had direction.
	ambiguousOperationToggleOff
	// ambiguousOperationDelete: DeleteVote deleted the removedURI vote.
	ambiguousOperationDelete
)

// ambiguousRequest describes a write whose outcome is unknown, so a retry of
// the same request can return its result once the PDS shows it holds (D6),
// and a create can reuse the record key a plain create attempted (PRD §5.4).
type ambiguousRequest struct {
	operation ambiguousOperation
	// direction is the CreateVote direction (cast and toggle-off).
	direction string
	// removedURI is the vote a toggle-off or DeleteVote was deleting.
	removedURI string
	// createRKey is the record key a plain create attempted; empty for a
	// direction switch, toggle-off or delete.
	createRKey string
}

// recentWriteOverlay holds this AppView's own recent vote writes until the
// index shows positive evidence that it has caught up with each one.
//
// Its lock is never held across a repository or PDS call.
type recentWriteOverlay struct {
	entries map[viewerSubjectKey]recentWrite
	// pruneQueue lists every recorded write in record order, for pruning.
	// Each record appends one item and pops up to recentWritePruneLimit
	// expired ones, so the queue holds at most the last retention period's
	// writes plus a backlog of retired or replaced items that drains by that
	// limit per write.
	pruneQueue  []recentWritePruneItem
	mutex       sync.Mutex
	lastVersion uint64
}

// recentWritePruneItem is one recorded write awaiting expiry.
type recentWritePruneItem struct {
	recordedAt time.Time
	key        viewerSubjectKey
	version    uint64
}

func newRecentWriteOverlay() *recentWriteOverlay {
	return &recentWriteOverlay{entries: make(map[viewerSubjectKey]recentWrite)}
}

func recentWriteExpired(write recentWrite, now time.Time) bool {
	return now.Sub(write.recordedAt) >= recentWriteRetention
}

// snapshot returns the unexpired entries for the viewer's subjects, keyed by
// subject URI.
func (o *recentWriteOverlay) snapshot(viewerDID string, subjectURIs []string, now time.Time) map[string]recentWrite {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	entries := make(map[string]recentWrite)
	for _, subjectURI := range subjectURIs {
		write, exists := o.entries[viewerSubjectKey{viewerDID: viewerDID, subjectURI: subjectURI}]
		if exists && !recentWriteExpired(write, now) {
			entries[subjectURI] = write
		}
	}
	return entries
}

// record replaces the key's entry with a new version of write and removes up
// to recentWritePruneLimit expired entries.
func (o *recentWriteOverlay) record(key viewerSubjectKey, write recentWrite, now time.Time) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	o.lastVersion++
	write.version = o.lastVersion
	write.recordedAt = now
	o.entries[key] = write
	o.pruneQueue = append(o.pruneQueue, recentWritePruneItem{recordedAt: now, key: key, version: write.version})

	// Pop expired items from the front. If the clock went backwards, an
	// unexpired head stops pruning early; a queued item only deletes the
	// map entry it recorded, never a newer version for the same key.
	pruned := 0
	for pruned < recentWritePruneLimit && len(o.pruneQueue) > 0 {
		head := o.pruneQueue[0]
		if now.Sub(head.recordedAt) < recentWriteRetention {
			break
		}
		o.pruneQueue[0] = recentWritePruneItem{}
		o.pruneQueue = o.pruneQueue[1:]
		if existing, exists := o.entries[head.key]; exists && existing.version == head.version {
			delete(o.entries, head.key)
		}
		pruned++
	}
}

// retireIfVersion removes the key's entry only if it is still the version
// that was evaluated.
func (o *recentWriteOverlay) retireIfVersion(key viewerSubjectKey, version uint64) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if write, exists := o.entries[key]; exists && write.version == version {
		delete(o.entries, key)
	}
}

// size reports how many entries the overlay holds, expired or not.
func (o *recentWriteOverlay) size() int {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	return len(o.entries)
}

// viewerSubjectLocks serializes writes for one viewer and subject (D4).
// Waiting is a select on the request context, so a canceled wait returns
// without touching the PDS.
type viewerSubjectLocks struct {
	locks map[viewerSubjectKey]*viewerSubjectLock
	mutex sync.Mutex
}

// viewerSubjectLock is held while its one-slot channel holds a token.
type viewerSubjectLock struct {
	token chan struct{}
	// users counts holders and waiters; the lock is dropped from the map
	// when it reaches zero.
	users int
}

func newViewerSubjectLocks() *viewerSubjectLocks {
	return &viewerSubjectLocks{locks: make(map[viewerSubjectKey]*viewerSubjectLock)}
}

// acquire waits for the key's lock and returns its release function, or the
// context's error if the context ends first.
func (l *viewerSubjectLocks) acquire(ctx context.Context, key viewerSubjectKey) (func(), error) {
	l.mutex.Lock()
	lock, exists := l.locks[key]
	if !exists {
		lock = &viewerSubjectLock{token: make(chan struct{}, 1)}
		l.locks[key] = lock
	}
	lock.users++
	l.mutex.Unlock()

	select {
	case lock.token <- struct{}{}:
		return func() {
			<-lock.token
			l.leave(key, lock)
		}, nil
	case <-ctx.Done():
		l.leave(key, lock)
		return nil, ctx.Err()
	}
}

// leave drops one holder or waiter and removes an idle lock.
func (l *viewerSubjectLocks) leave(key viewerSubjectKey, lock *viewerSubjectLock) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	lock.users--
	if lock.users == 0 {
		delete(l.locks, key)
	}
}
