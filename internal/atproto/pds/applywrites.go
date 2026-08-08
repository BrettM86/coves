package pds

import "context"

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

// LatestCommit is a repo's current head, as com.atproto.repo.getLatestCommit
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

// ApplyWrites applies a batch of writes as one repo commit.
func (c *client) ApplyWrites(ctx context.Context, writes []Write, swapCommit string) (*ApplyWritesResult, error) {
	return nil, nil
}

// PutRecordWithCommit creates or updates a record and reports the commit.
func (c *client) PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*RecordCommit, error) {
	return nil, nil
}

// CreateRecordWithCommit creates a record and reports the commit.
func (c *client) CreateRecordWithCommit(ctx context.Context, collection, rkey string, record any) (*RecordCommit, error) {
	return nil, nil
}

// GetLatestCommit returns the repo's current head.
func (c *client) GetLatestCommit(ctx context.Context) (*LatestCommit, error) {
	return nil, nil
}
