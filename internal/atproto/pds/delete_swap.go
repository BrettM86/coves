package pds

import (
	"context"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// The GUARDED delete: com.atproto.repo.deleteRecord with a swapRecord CID.
//
// # WHY THIS EXISTS SEPARATELY FROM DeleteRecord
//
// Client.DeleteRecord sends no swap guard, which is right for the ordinary case:
// a user deleting their own post means "whatever is there, remove it", and
// making every caller carry a CID would turn a delete into a read-then-write
// race with itself.
//
// It is exactly wrong for a MIGRATION. The re-materialization tool reads a
// legacy record, converts it, writes the replacement, and deletes the original
// minutes or hours later. In that gap an edit can land — a cron the maintenance
// window did not stop, a mobile session on a cached token — and an unguarded
// delete destroys the newer content with no trace, having verified a replacement
// for the OLDER content. The tool checks the CID itself before deleting, but a
// check is a moment and the write that follows it is another; only a guard the
// PDS evaluates atomically with the delete closes that window.
//
// So this is a separate, opt-in surface: callers that want "remove whatever
// stands" keep the plain method, and callers that must not destroy an unseen
// version ask for the guard by type.

// GuardedDeleter is the swap-guarded delete surface.
//
// It is an interface (satisfied by the concrete client) rather than a method on
// Client so that a caller REQUIRING the guard states that requirement in its own
// types and fails to compile against a transport that cannot provide it, instead
// of silently falling back to an unguarded delete at 3am.
type GuardedDeleter interface {
	// DeleteRecordWithSwap deletes a record only if it currently carries
	// swapRecord as its CID. A mismatch comes back as ErrSwapConflict; a record
	// that is already gone comes back as ErrNotFound, which an idempotent caller
	// may treat as success.
	//
	// swapRecord is REQUIRED. Passing an empty string is an error rather than a
	// silent unguarded delete, because "I have no CID to guard on" is precisely
	// the state in which a delete must not proceed.
	DeleteRecordWithSwap(ctx context.Context, collection, rkey, swapRecord string) error
}

// Ensure the concrete client provides the guarded delete.
var _ GuardedDeleter = (*client)(nil)

// DeleteRecordWithSwap deletes a record under an optimistic-concurrency guard.
func (c *client) DeleteRecordWithSwap(ctx context.Context, collection, rkey, swapRecord string) error {
	if swapRecord == "" {
		return fmt.Errorf("deleteRecord: refusing to delete %s/%s in %s without a swapRecord guard: "+
			"an unguarded delete removes whatever stands, including a version this caller has never seen",
			collection, rkey, c.did)
	}

	payload := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"rkey":       rkey,
		"swapRecord": swapRecord,
	}

	if err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.deleteRecord"), payload, nil); err != nil {
		return wrapAPIError(err, "deleteRecord")
	}
	return nil
}
