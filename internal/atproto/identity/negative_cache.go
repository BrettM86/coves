package identity

import (
	"context"
	"errors"
	"strings"
	"time"
)

// DefaultNegativeCacheTTL bounds how long failed or unverified DID resolution is
// remembered in-process. Without it, every retry re-resolves an attacker-chosen
// tarpit DID at up to 10 seconds per attempt (docs/SECURITY_AUDIT_2026-09-01.md §1.3).
// It must remain below the dead-letter redrive interval so a redrive reaches the
// network; internal/config refuses to load when REDRIVE_INTERVAL is not
// greater than IDENTITY_NEGATIVE_CACHE_TTL.
const DefaultNegativeCacheTTL = 90 * time.Second

// negativeCacheMaxEntries bounds memory over hit rate: an attacker can mint
// unlimited did:web names, so old entries are evicted rather than letting the
// process-local cache grow with attacker-controlled cardinality.
const negativeCacheMaxEntries = 4096

// newCachingResolverWithClock builds a caching resolver whose negative cache
// uses the given TTL and clock. A non-positive TTL means DefaultNegativeCacheTTL.
func newCachingResolverWithClock(base Resolver, cache IdentityCache, negativeTTL time.Duration, now func() time.Time) Resolver {
	if negativeTTL <= 0 {
		negativeTTL = DefaultNegativeCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	return &cachingResolver{
		base:            base,
		cache:           cache,
		negativeTTL:     negativeTTL,
		now:             now,
		negativeEntries: make(map[string]negativeCacheEntry),
	}
}

type negativeCacheEntry struct {
	identity  *Identity
	err       error
	expiresAt time.Time
}

func (r *cachingResolver) getNegative(identifier string) (cached *Identity, found bool, err error) {
	if !strings.HasPrefix(identifier, "did:") {
		return nil, false, nil
	}

	r.negativeMu.Lock()
	defer r.negativeMu.Unlock()

	entry, ok := r.negativeEntries[identifier]
	if !ok {
		return nil, false, nil
	}
	if !r.now().Before(entry.expiresAt) {
		r.deleteNegativeLocked(identifier)
		return nil, false, nil
	}
	if entry.identity == nil {
		return nil, true, entry.err
	}

	clone := *entry.identity
	clone.Method = MethodCache
	return &clone, true, nil
}

func (r *cachingResolver) storeNegative(identifier string, resolved *Identity, err error) {
	if !strings.HasPrefix(identifier, "did:") {
		return
	}
	if err != nil && !negativeCacheableError(err) {
		return
	}
	if err == nil && (resolved == nil || (resolved.Handle != "" && resolved.Handle != InvalidHandle)) {
		return
	}

	entry := negativeCacheEntry{err: err, expiresAt: r.now().Add(r.negativeTTL)}
	if resolved != nil {
		clone := *resolved
		entry.identity = &clone
	}

	r.negativeMu.Lock()
	defer r.negativeMu.Unlock()

	if _, exists := r.negativeEntries[identifier]; exists {
		r.negativeEntries[identifier] = entry
		return
	}
	if len(r.negativeEntries) >= negativeCacheMaxEntries {
		r.sweepExpiredNegativeLocked()
	}
	if len(r.negativeEntries) >= negativeCacheMaxEntries {
		oldest := r.negativeOrder[0]
		r.negativeOrder = r.negativeOrder[1:]
		delete(r.negativeEntries, oldest)
	}
	r.negativeEntries[identifier] = entry
	r.negativeOrder = append(r.negativeOrder, identifier)
}

// negativeCacheableError excludes cancellation when a base preserves it in the
// error chain. Resolve also checks its caller context because the production
// base resolver stringifies cancellation inside ErrResolutionFailed.
func negativeCacheableError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var invalidIdentifier *ErrInvalidIdentifier
	if errors.As(err, &invalidIdentifier) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var notFound *ErrNotFound
	if errors.As(err, &notFound) {
		return true
	}
	var resolutionFailed *ErrResolutionFailed
	return errors.As(err, &resolutionFailed)
}

func (r *cachingResolver) deleteNegative(identifier string) {
	r.negativeMu.Lock()
	defer r.negativeMu.Unlock()
	r.deleteNegativeLocked(identifier)
}

func (r *cachingResolver) deleteNegativeLocked(identifier string) {
	if _, ok := r.negativeEntries[identifier]; !ok {
		return
	}
	delete(r.negativeEntries, identifier)
	for i, key := range r.negativeOrder {
		if key == identifier {
			r.negativeOrder = append(r.negativeOrder[:i], r.negativeOrder[i+1:]...)
			return
		}
	}
}

func (r *cachingResolver) sweepExpiredNegativeLocked() {
	now := r.now()
	order := r.negativeOrder[:0]
	for _, identifier := range r.negativeOrder {
		entry, ok := r.negativeEntries[identifier]
		if !ok {
			continue
		}
		if !now.Before(entry.expiresAt) {
			delete(r.negativeEntries, identifier)
			continue
		}
		order = append(order, identifier)
	}
	r.negativeOrder = order
}
