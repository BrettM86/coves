package pds

import (
	"context"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Batch commits and commit revisions.
//
// Everything in this file exists because the community-repo writers of
// docs/PRD_AUTHOR_OWNED_POSTS.md §5.6 need two things the pre-existing Client
// surface cannot give them:
//
//  1. THE COMMIT REV. §5.2's ordering gate is keyed on the repo revision the
//     write landed in, and the AppView stamps that rev onto the admission row
//     optimistically so its own write is not overtaken by the firehose copy of
//     the same event. createRecord/putRecord/applyWrites all return a `commit`
//     object; the existing methods throw it away.
//
//  2. ONE COMMIT, SEVERAL RECORDS. §3.3 requires that the acceptance deletion
//     and the removal write reach the firehose as a single commit, so a
//     consumer can never observe a half-completed moderation action. That is
//     com.atproto.repo.applyWrites and nothing else.
//
// These are added ALONGSIDE the existing methods rather than replacing them:
// Client is implemented by test doubles across five domains, and widening it
// would break every one of them for the benefit of one caller.

// WriteOp names one operation inside an applyWrites batch. The values are the
// discriminants of the lexicon's union — `com.atproto.repo.applyWrites#create`
// and friends — with the `#` prefix supplied by the transport.
type WriteOp string

const (
	// WriteOpCreate creates a record that must NOT already exist. The PDS
	// answers a create of an existing rkey with a 500, so the caller has to
	// pre-read presence and choose between create and update.
	WriteOpCreate WriteOp = "create"

	// WriteOpUpdate writes a record that MUST already exist.
	WriteOpUpdate WriteOp = "update"

	// WriteOpDelete removes a record that must already exist. The PDS answers a
	// delete of a missing rkey with a 500, so a batch is shaped by a pre-read
	// rather than by optimism.
	WriteOpDelete WriteOp = "delete"
)

// Write is one operation in an applyWrites batch.
type Write struct {
	// Record is the record body for a create or an update, and nil for a
	// delete.
	Record any

	Op         WriteOp
	Collection string
	RKey       string
}

// WriteResult is what one operation in a batch produced. A delete produces no
// URI and no CID; the lexicon's `#deleteResult` is an empty object.
type WriteResult struct {
	Op  WriteOp
	URI string
	CID string
}

// ApplyWritesResult is the commit a batch landed in, plus the per-operation
// results in the order the operations were submitted.
type ApplyWritesResult struct {
	CommitRev string
	CommitCID string
	Results   []WriteResult
}

// RecordCommit is what a single-record write committed: the record's own
// identity and the repo commit that carries it.
type RecordCommit struct {
	URI string
	CID string

	// CommitRev is the repo revision this write landed in — the §5.2 watermark
	// the AppView stamps on the admission row.
	CommitRev string
	CommitCID string
}

// LatestCommit is a repo's current head, as com.atproto.sync.getLatestCommit
// reports it. It is what a batch passes as swapCommit so a concurrent writer's
// commit is a detected conflict rather than a silent clobber.
type LatestCommit struct {
	CID string
	Rev string
}

// CommitClient is Client plus the commit-aware writes.
//
// It is a separate interface rather than an extension of Client so that the
// existing test doubles for Client keep compiling. The concrete client
// satisfies both, and a caller that needs a commit rev asks for this one.
type CommitClient interface {
	Client

	// ApplyWrites applies every write as ONE repo commit, so the firehose
	// carries them together or not at all.
	//
	// swapCommit, when non-empty, is the commit CID the batch expects the repo
	// to be at; a mismatch is ErrSwapConflict rather than an overwrite. Record
	// validation is disabled on the wire (`validate: false`) because the
	// records are Coves lexicons the PDS has never been taught.
	ApplyWrites(ctx context.Context, writes []Write, swapCommit string) (*ApplyWritesResult, error)

	// PutRecordWithCommit is PutRecord with the commit rev retained.
	PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*RecordCommit, error)

	// CreateRecordWithCommit is CreateRecord with the commit rev retained.
	CreateRecordWithCommit(ctx context.Context, collection, rkey string, record any) (*RecordCommit, error)

	// GetLatestCommit returns the repo's current head.
	GetLatestCommit(ctx context.Context) (*LatestCommit, error)
}

// Ensure the concrete client implements the commit-aware surface too.
var _ CommitClient = (*client)(nil)

// applyWritesUnion is the lexicon namespace the batch's union discriminants are
// drawn from. A batch entry without a `$type` is not an applyWrites entry: the
// PDS has no other way to tell a create from a delete.
const applyWritesUnion = "com.atproto.repo.applyWrites#"

// commitResponse is the `commit` object every commit-aware write returns.
type commitResponse struct {
	CID string `json:"cid"`
	Rev string `json:"rev"`
}

// ApplyWrites applies a batch of writes as one repo commit.
func (c *client) ApplyWrites(ctx context.Context, writes []Write, swapCommit string) (*ApplyWritesResult, error) {
	if len(writes) == 0 {
		// An empty batch would be a commit that says nothing, and the PDS would
		// happily make one. Refusing locally keeps a caller whose state-shaping
		// concluded "there is nothing to do" from advancing the repo's revision
		// — and from stamping a watermark off a commit that changed no record.
		return nil, fmt.Errorf("applyWrites: %w: a batch must carry at least one write", ErrBadRequest)
	}

	entries := make([]map[string]any, 0, len(writes))
	for i, write := range writes {
		entry := map[string]any{
			"$type":      applyWritesUnion + string(write.Op),
			"collection": write.Collection,
			"rkey":       write.RKey,
		}

		switch write.Op {
		case WriteOpCreate, WriteOpUpdate:
			if write.Record == nil {
				return nil, fmt.Errorf("applyWrites: %w: writes[%d] is a %s with no record body",
					ErrBadRequest, i, write.Op)
			}
			entry["value"] = write.Record
		case WriteOpDelete:
			// No `value`. A delete carrying a record body is a malformed union
			// member, and the PDS is entitled to refuse the whole batch for it.
		default:
			return nil, fmt.Errorf("applyWrites: %w: writes[%d] has unknown operation %q",
				ErrBadRequest, i, write.Op)
		}

		entries = append(entries, entry)
	}

	payload := map[string]any{
		"repo":   c.did,
		"writes": entries,
		// The records are Coves lexicons the PDS has never been taught, so
		// validate:true is a refusal. It must be the BOOLEAN false rather than
		// absent — the lexicon's default is not false.
		"validate": false,
	}

	// An empty swapCommit means "no guard", not "guard against the empty
	// string": sending it would have every unguarded batch refused.
	if swapCommit != "" {
		payload["swapCommit"] = swapCommit
	}

	var response struct {
		Commit  *commitResponse `json:"commit"`
		Results []struct {
			Type string `json:"$type"`
			URI  string `json:"uri"`
			CID  string `json:"cid"`
		} `json:"results"`
	}

	if err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.applyWrites"), payload, &response); err != nil {
		return nil, wrapAPIError(err, "applyWrites")
	}

	if response.Commit == nil || response.Commit.Rev == "" {
		// The commit rev IS the watermark this method exists to obtain. A
		// success without one leaves the caller with a committed batch it cannot
		// order, and silently reporting an empty rev would have it stamp a clock
		// value no commit ever had.
		return nil, fmt.Errorf("applyWrites: PDS returned success without a commit rev (%d writes)", len(writes))
	}

	result := &ApplyWritesResult{
		CommitRev: response.Commit.Rev,
		CommitCID: response.Commit.CID,
		Results:   make([]WriteResult, len(response.Results)),
	}
	for i, entry := range response.Results {
		result.Results[i] = WriteResult{
			Op:  writeOpOfResult(entry.Type),
			URI: entry.URI,
			CID: entry.CID,
		}
	}

	return result, nil
}

// writeOpOfResult maps a result's union discriminant back onto the operation
// that produced it. An unrecognised discriminant yields the empty op rather
// than a guess: the results are positional, so the caller can still match them
// to what it submitted.
func writeOpOfResult(unionType string) WriteOp {
	switch unionType {
	case applyWritesUnion + "createResult":
		return WriteOpCreate
	case applyWritesUnion + "updateResult":
		return WriteOpUpdate
	case applyWritesUnion + "deleteResult":
		return WriteOpDelete
	default:
		return ""
	}
}

// PutRecordWithCommit creates or updates a record and reports the commit.
//
// EVERY PUT THROUGH THIS METHOD IS GUARDED, and that is the difference from
// Client.PutRecord. A non-empty swapRecord is the record CID the caller expects
// to be replacing; an EMPTY one sends `swapRecord: null`, which the PDS reads as
// "there must be no record here yet". The state-shaped writers pre-read before
// they write, and an unguarded put would let a concurrent writer's record be
// clobbered in exactly the window the pre-read opened. Callers wanting the
// lenient, unguarded write still have PutRecord.
func (c *client) PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*RecordCommit, error) {
	payload := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
		// Coves lexicons; see ApplyWrites.
		"validate": false,
	}

	if swapRecord != "" {
		payload["swapRecord"] = swapRecord
	} else {
		// Explicit JSON null, NOT an absent key. Absent means "overwrite
		// whatever is there"; null means "expect nothing to be there", which is
		// what makes a concurrent create a detected ErrSwapConflict rather than
		// a silent overwrite.
		payload["swapRecord"] = nil
	}

	var response struct {
		URI    string          `json:"uri"`
		CID    string          `json:"cid"`
		Commit *commitResponse `json:"commit"`
	}

	if err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.putRecord"), payload, &response); err != nil {
		return nil, wrapAPIError(err, "putRecord")
	}

	return recordCommit("putRecord", collection, response.URI, response.CID, response.Commit)
}

// CreateRecordWithCommit creates a record and reports the commit.
func (c *client) CreateRecordWithCommit(ctx context.Context, collection, rkey string, record any) (*RecordCommit, error) {
	payload := map[string]any{
		"repo":       c.did,
		"collection": collection,
		"record":     record,
		"validate":   false,
	}

	// An empty rkey lets the PDS generate a TID, matching CreateRecord.
	if rkey != "" {
		payload["rkey"] = rkey
	}

	var response struct {
		URI    string          `json:"uri"`
		CID    string          `json:"cid"`
		Commit *commitResponse `json:"commit"`
	}

	if err := c.apiClient.Post(ctx, syntax.NSID("com.atproto.repo.createRecord"), payload, &response); err != nil {
		return nil, wrapAPIError(err, "createRecord")
	}

	return recordCommit("createRecord", collection, response.URI, response.CID, response.Commit)
}

// recordCommit validates a single-record write's response and shapes it.
//
// A 200 carrying no uri/cid, or no commit rev, is a malformed body from the PDS
// or something in front of it. It is reported rather than returned as a
// zero-valued success, because the two things missing here are precisely the two
// the caller is about to persist: the record it will reference, and the
// revision it will order by.
func recordCommit(operation, collection, uri, cid string, commit *commitResponse) (*RecordCommit, error) {
	if uri == "" || cid == "" {
		return nil, fmt.Errorf("%s: PDS returned success without uri/cid (collection %s)", operation, collection)
	}
	if commit == nil || commit.Rev == "" {
		return nil, fmt.Errorf("%s: PDS returned success without a commit rev (collection %s)", operation, collection)
	}

	return &RecordCommit{
		URI:       uri,
		CID:       cid,
		CommitRev: commit.Rev,
		CommitCID: commit.CID,
	}, nil
}

// GetLatestCommit returns the repo's current head.
//
// The method is com.atproto.SYNC.getLatestCommit. There is no
// com.atproto.repo.getLatestCommit — a PDS asked for one answers "No service
// configured for com.atproto.repo.getLatestCommit", because its XRPC router
// falls through to proxying a method it has never heard of. The head of a repo
// is sync-namespace data (it is the commit the firehose carries), and it is
// what a batch's swapCommit guard is read from.
func (c *client) GetLatestCommit(ctx context.Context) (*LatestCommit, error) {
	var response commitResponse

	if err := c.apiClient.Get(ctx, syntax.NSID("com.atproto.sync.getLatestCommit"),
		map[string]any{"did": c.did}, &response); err != nil {
		return nil, wrapAPIError(err, "getLatestCommit")
	}

	if response.CID == "" || response.Rev == "" {
		// The CID is what a batch is guarded by. An empty one would be sent as
		// "no guard" by ApplyWrites, silently turning an optimistic batch into
		// an unconditional one.
		return nil, fmt.Errorf("getLatestCommit: PDS returned success without a commit cid/rev (repo %s)", c.did)
	}

	return &LatestCommit{CID: response.CID, Rev: response.Rev}, nil
}
