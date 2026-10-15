package jetstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"Coves/internal/atproto/identity"
)

// identityRefreshFailureDebounceMaxEntries bounds memory: an attacker can mint
// many failing identities, so old entries are evicted rather than letting the
// map grow with attacker-controlled cardinality.
const identityRefreshFailureDebounceMaxEntries = 4096

// identityRefreshFailureDebounce remembers DIDs whose fresh identity
// resolution recently hit identityResolveTimeout, so repeated identity events
// for an identity whose handle domain hangs do not each cost the serial lane
// that timeout. It records timeouts only: a success or a fast failure (NXDOMAIN,
// handle.invalid, subject mismatch) leaves the next event to resolve fresh.
// Its TTL must not exceed IDENTITY_NEGATIVE_CACHE_TTL, which config keeps below
// REDRIVE_INTERVAL, so each redrive reaches the network. Safe for concurrent use.
type identityRefreshFailureDebounce struct {
	mu        sync.Mutex
	ttl       time.Duration
	now       func() time.Time
	expiresAt map[string]time.Time
	order     []string
}

// newIdentityRefreshFailureDebounce builds a debounce with the given TTL. A
// non-positive TTL means identity.DefaultNegativeCacheTTL.
func newIdentityRefreshFailureDebounce(ttl time.Duration) *identityRefreshFailureDebounce {
	return &identityRefreshFailureDebounce{
		ttl:       ttl,
		now:       time.Now,
		expiresAt: make(map[string]time.Time),
	}
}

func (d *identityRefreshFailureDebounce) failureTTL() time.Duration {
	if d.ttl <= 0 {
		return identity.DefaultNegativeCacheTTL
	}
	return d.ttl
}

// recentlyFailed reports whether a recorded timeout for did is still within the TTL.
func (d *identityRefreshFailureDebounce) recentlyFailed(did string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	expiresAt, ok := d.expiresAt[did]
	if !ok {
		return false
	}
	if !d.now().Before(expiresAt) {
		d.deleteLocked(did)
		return false
	}
	return true
}

func (d *identityRefreshFailureDebounce) recordFailure(did string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	expiresAt := d.now().Add(d.failureTTL())
	if _, exists := d.expiresAt[did]; exists {
		d.expiresAt[did] = expiresAt
		return
	}
	if len(d.expiresAt) >= identityRefreshFailureDebounceMaxEntries {
		d.sweepExpiredLocked()
	}
	if len(d.expiresAt) >= identityRefreshFailureDebounceMaxEntries {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.expiresAt, oldest)
	}
	d.expiresAt[did] = expiresAt
	d.order = append(d.order, did)
}

func (d *identityRefreshFailureDebounce) clear(did string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleteLocked(did)
}

func (d *identityRefreshFailureDebounce) deleteLocked(did string) {
	if _, ok := d.expiresAt[did]; !ok {
		return
	}
	delete(d.expiresAt, did)
	for i, key := range d.order {
		if key == did {
			d.order = append(d.order[:i], d.order[i+1:]...)
			return
		}
	}
}

func (d *identityRefreshFailureDebounce) sweepExpiredLocked() {
	now := d.now()
	order := d.order[:0]
	for _, did := range d.order {
		expiresAt, ok := d.expiresAt[did]
		if !ok {
			continue
		}
		if !now.Before(expiresAt) {
			delete(d.expiresAt, did)
			continue
		}
		order = append(order, did)
	}
	d.order = order
}

// ErrIdentityResolveFailed marks a refresh whose fresh resolution of the DID
// failed. It is added to the error chain without changing the message or the
// connector class the error already carries.
var ErrIdentityResolveFailed = errors.New("identity resolution failed")

// ErrVerifiedHandleInvalid marks a refresh whose verified handle breaks the
// handle rules of the row it would be written to.
var ErrVerifiedHandleInvalid = errors.New("verified handle breaks the stored handle rules")

// ErrHostedByMismatch marks a community refresh whose verified handle fails
// the community's hostedBy claim.
var ErrHostedByMismatch = errors.New("verified handle does not match the hostedBy claim")

// ErrHostedByVerificationPending marks a hostedBy check answered from the
// verification cache without a result: another verification of the same
// (hostedBy DID, handle domain) is in flight, or its last fetch failed and the
// failure is still cached. A later attempt can succeed.
var ErrHostedByVerificationPending = errors.New("hostedBy DID document verification pending")

// markedRefreshError adds a refresh-outcome sentinel to an error's chain. The
// message and the rest of the chain, connector class included, are unchanged.
type markedRefreshError struct {
	err  error
	mark error
}

func (e *markedRefreshError) Error() string        { return e.err.Error() }
func (e *markedRefreshError) Unwrap() error        { return e.err }
func (e *markedRefreshError) Is(target error) bool { return target == e.mark }

func markRefreshError(err, mark error) error {
	return &markedRefreshError{err: err, mark: mark}
}

// IdentityHandleRefresh is what one locked handle refresh established.
type IdentityHandleRefresh struct {
	// StoredHandle is the handle read inside the lock. Empty when the refresh
	// did not reach the read.
	StoredHandle string
	// VerifiedHandle is the handle verification established for the DID, also
	// when a later step failed. Empty when verification did not complete.
	VerifiedHandle string
	// Changed is true only after the verified handle was written.
	Changed bool
}

// RefreshIdentityHandle purges stale identity answers, resolves the DID fresh,
// and writes a changed verified handle. The callbacks read and write the owning
// row; changed is true only after a successful write. verifiedHandle is
// returned whenever verification succeeded, also when a later step failed. A
// failed resolution carries ErrIdentityResolveFailed in its chain. Everything
// from the DID purge to the handle write runs inside lock's per-DID section, so refreshes of
// one DID never overlap, and readStoredHandle must read the row inside it: a
// handle read before the lock may be stale. A nil lock refuses the refresh with
// a non-permanent error before any purge or write, as does a missing resolver
// or failure debounce. The cached DID answer is purged on every call. A failed resolution or handle verification that hit
// identityResolveTimeout while ctx is still live is recorded in failureDebounce; while that record is recent the
// refresh skips resolution and returns a deferred ErrUnresolvedReference, which
// does not spend a redrive attempt. Other failures are not recorded.
func RefreshIdentityHandle(
	ctx context.Context,
	did string,
	lock IdentityRefreshLock,
	cachingResolver, uncachedResolver identity.Resolver,
	failureDebounce *identityRefreshFailureDebounce,
	readStoredHandle func(context.Context) (string, error),
	writeHandle func(context.Context, string) error,
) (verifiedHandle string, changed bool, err error) {
	if lock == nil {
		return "", false, fmt.Errorf("identity refresh lock is not configured for %s", did)
	}
	if cachingResolver == nil {
		return "", false, fmt.Errorf("caching identity resolver is not configured for %s", did)
	}
	if uncachedResolver == nil {
		return "", false, fmt.Errorf("uncached identity resolver is not configured for %s", did)
	}
	if failureDebounce == nil {
		return "", false, fmt.Errorf("identity refresh failure debounce is not configured for %s", did)
	}
	err = lock.WithDIDLock(ctx, did, func(ctx context.Context) error {
		var refreshErr error
		verifiedHandle, changed, refreshErr = refreshIdentityHandleLocked(ctx, did, cachingResolver, uncachedResolver,
			failureDebounce, readStoredHandle, writeHandle)
		return refreshErr
	})
	if err != nil {
		return verifiedHandle, false, err
	}
	return verifiedHandle, changed, nil
}

// refreshIdentityHandleLocked is RefreshIdentityHandle's body; the caller holds
// the DID's refresh lock.
func refreshIdentityHandleLocked(
	ctx context.Context,
	did string,
	cachingResolver, uncachedResolver identity.Resolver,
	failureDebounce *identityRefreshFailureDebounce,
	readStoredHandle func(context.Context) (string, error),
	writeHandle func(context.Context, string) error,
) (verifiedHandle string, changed bool, err error) {
	if err := cachingResolver.Purge(ctx, did); err != nil {
		return "", false, fmt.Errorf("purge cached DID %s: %w", did, err)
	}
	if failureDebounce.recentlyFailed(did) {
		return "", false, deferRedrive(fmt.Errorf("%w: identity refresh for %s debounced after a recent resolution timeout",
			ErrUnresolvedReference, did))
	}

	resolveCtx, cancel := context.WithTimeout(ctx, identityResolveTimeout)
	defer cancel()
	resolved, resolveErr := uncachedResolver.Resolve(resolveCtx, did)
	if resolveErr == nil {
		verifiedHandle, err = identity.VerifiedHandle(resolved, did)
	}
	// Only identityResolveTimeout stalls the lane. A deadline or cancellation
	// from the caller's own context says nothing about the identity. The check
	// covers both failures because a hanging handle domain does not fail Resolve:
	// indigo spends the timeout on the well-known fetch and then reports
	// handle.invalid with a nil error, which VerifiedHandle rejects.
	if (resolveErr != nil || err != nil) &&
		ctx.Err() == nil && errors.Is(resolveCtx.Err(), context.DeadlineExceeded) {
		failureDebounce.recordFailure(did)
	}
	if resolveErr != nil {
		var invalidIdentifier *identity.ErrInvalidIdentifier
		if errors.As(resolveErr, &invalidIdentifier) {
			return "", false, markRefreshError(
				fmt.Errorf("%w: resolve identity %s: %w", ErrPermanentEvent, did, resolveErr), ErrIdentityResolveFailed)
		}
		return "", false, markRefreshError(
			fmt.Errorf("%w: resolve identity %s: %w", ErrUnresolvedReference, did, resolveErr), ErrIdentityResolveFailed)
	}
	if err != nil {
		if errors.Is(err, identity.ErrIdentitySubjectMismatch) {
			return "", false, fmt.Errorf("%w: verify identity %s: %w", ErrPermanentEvent, did, err)
		}
		return "", false, fmt.Errorf("%w: verify identity %s: %w", ErrUnresolvedReference, did, err)
	}
	failureDebounce.clear(did)

	storedHandle, err := readStoredHandle(ctx)
	if err != nil {
		return verifiedHandle, false, fmt.Errorf("read stored handle for %s: %w", did, err)
	}
	if storedHandle == verifiedHandle {
		return verifiedHandle, false, nil
	}
	if err := cachingResolver.Purge(ctx, storedHandle); err != nil {
		return verifiedHandle, false, fmt.Errorf("purge cached handle %s: %w", storedHandle, err)
	}
	if err := writeHandle(ctx, verifiedHandle); err != nil {
		return verifiedHandle, false, fmt.Errorf("update handle for %s: %w", did, err)
	}
	return verifiedHandle, true, nil
}
