package posts

import (
	"context"
	"errors"
	"fmt"

	"Coves/internal/atproto/pds"
)

// InstanceRemovalCommand is an instance admin's removal mirrored into a hosted
// community's repo as the community's own removal record.
type InstanceRemovalCommand struct {
	CommunityDID string
	SubjectURI   string

	// SubjectCID is the version the admin reviewed: the removal's strongRef.
	SubjectCID string

	// Code is the public removal code the record carries, mapped from the
	// instance action's reason. The record has no reason field set, and the
	// action's private note never leaves the AppView.
	Code DecisionCode

	// CreatedAt is the instance removal action's time, so the record's content
	// is a pure function of the action and a lost response can be recognised.
	CreatedAt string

	// BeforeCommit, when set, is called before each batch is sent, reporting
	// whether that batch deletes a standing acceptance. An error aborts the
	// write with nothing sent.
	BeforeCommit func(ctx context.Context, deletesAcceptance bool) error
}

// StandingRemoval is a removal record found standing at a subject's rkey.
type StandingRemoval struct {
	// CID is the record's own CID.
	CID        string
	SubjectURI string
	SubjectCID string
	Code       string
	Reason     string
	CreatedAt  string
}

// StandingRecordRef is an acceptance record found standing at a subject's rkey.
type StandingRecordRef struct {
	// CID is the record's own CID.
	CID string
	// SubjectCID is the content CID the record's strongRef pins.
	SubjectCID string
}

// InstanceRemovalResult is what WriteInstanceRemoval did.
type InstanceRemovalResult struct {
	CommunityWriteResult

	// Standing is the removal that already stood, in which case nothing was
	// written.
	Standing *StandingRemoval

	// WithdrewAcceptance reports that the committed batch deleted a standing
	// acceptance.
	WithdrewAcceptance bool
}

// InstanceRemovalUndoCommand reverses one instance removal record.
type InstanceRemovalUndoCommand struct {
	CommunityDID string
	SubjectURI   string

	// RemovalCID is the record CID of the removal being undone. Nothing is
	// written unless the standing removal is exactly this record.
	RemovalCID string

	// AcceptanceCID, when set, is the content CID a fresh acceptance written in
	// the same commit pins.
	AcceptanceCID string
}

// InstanceRemovalWriter writes and reverses the community records an instance
// removal produces in a community this AppView hosts.
type InstanceRemovalWriter interface {
	// StandingAcceptance reads the acceptance standing for the subject, or nil.
	StandingAcceptance(ctx context.Context, communityDID, subjectURI string) (*StandingRecordRef, error)

	// StandingRemoval reads the removal standing for the subject, or nil.
	StandingRemoval(ctx context.Context, communityDID, subjectURI string) (*StandingRemoval, error)

	// WriteInstanceRemoval writes the removal record, deleting a standing
	// acceptance in the same commit. A standing removal is returned in
	// Standing and nothing is written.
	WriteInstanceRemoval(ctx context.Context, cmd InstanceRemovalCommand) (InstanceRemovalResult, error)

	// UndoInstanceRemoval deletes the removal record named by cmd.RemovalCID
	// and, when cmd.AcceptanceCID is set, writes an acceptance pinning it in
	// the same commit. Any other standing state is a skip.
	UndoInstanceRemoval(ctx context.Context, cmd InstanceRemovalUndoCommand) (CommunityWriteResult, error)
}

// NewInstanceRemovalWriter returns the instance-removal writer over the repos
// the factory opens.
func NewInstanceRemovalWriter(repos CommunityRepoFactory, now Clock, opts ...WriterOption) InstanceRemovalWriter {
	w := &communityRecordWriter{repos: repos, now: now, sleep: sleepWithContext}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

func (w *communityRecordWriter) StandingAcceptance(ctx context.Context, communityDID, subjectURI string) (*StandingRecordRef, error) {
	standing, err := w.readSubjectRecord(ctx, "standing acceptance read", communityDID, subjectURI, AcceptanceCollection)
	if err != nil || standing == nil {
		return nil, err
	}
	return &StandingRecordRef{CID: standing.CID, SubjectCID: standing.SubjectCID}, nil
}

func (w *communityRecordWriter) StandingRemoval(ctx context.Context, communityDID, subjectURI string) (*StandingRemoval, error) {
	standing, err := w.readSubjectRecord(ctx, "standing removal read", communityDID, subjectURI, RemovalCollection)
	if err != nil || standing == nil {
		return nil, err
	}
	return standingRemovalOf(standing), nil
}

// readSubjectRecord reads what stands at the subject's rkey in one collection
// of the community's repo, or nil.
func (w *communityRecordWriter) readSubjectRecord(ctx context.Context, kind, communityDID, subjectURI, collection string) (*standingRecord, error) {
	if communityDID == "" {
		return nil, fmt.Errorf("%s: %w", kind, NewValidationError("communityDID", "is required"))
	}
	if err := validateSubjectURI(kind, subjectURI); err != nil {
		return nil, err
	}

	repo, err := w.openRepo(ctx, communityDID)
	if err != nil {
		return nil, err
	}
	return readStandingRecord(ctx, repo, collection, SubjectRkey(subjectURI))
}

// WriteInstanceRemoval is shaped like WriteRemoval, with three differences.
//
// ANY STANDING REMOVAL IS THE ANSWER. WriteRemoval updates a standing removal
// whose decision differs; this writer never does, because a removal already
// standing is either the community's own moderation or one this writer wrote
// earlier, and the caller tells the two apart from what is returned. Rewriting
// a community moderator's removal with the instance's code would replace their
// public decision with ours.
//
// createdAt is the instance action's time rather than the clock's, so a
// response lost after the commit landed leaves a record the caller can
// recognise as its own on the retry.
//
// cmd.BeforeCommit runs before each batch is sent, with whether that batch
// deletes an acceptance, so the caller can persist that evidence before a
// response that may be lost.
func (w *communityRecordWriter) WriteInstanceRemoval(ctx context.Context, cmd InstanceRemovalCommand) (InstanceRemovalResult, error) {
	if err := validateInstanceRemovalCommand(cmd); err != nil {
		return InstanceRemovalResult{}, err
	}

	repo, err := w.openRepo(ctx, cmd.CommunityDID)
	if err != nil {
		return InstanceRemovalResult{}, err
	}

	rkey := SubjectRkey(cmd.SubjectURI)
	uri := recordURI(repo.DID(), RemovalCollection, rkey)
	record := removalRecord(CommunityRemovalCommand{
		PostURI: cmd.SubjectURI,
		PostCID: cmd.SubjectCID,
		Code:    cmd.Code,
	}, cmd.CreatedAt)

	for attempt := 0; ; attempt++ {
		// The head is read before the records, as in commitPair: the batch is
		// guarded by it, and a skip reports it as the catch-up watermark.
		head, err := repo.GetLatestCommit(ctx)
		if err != nil {
			return InstanceRemovalResult{}, fmt.Errorf("reading the head of %s: %w", cmd.CommunityDID, err)
		}

		removal, err := readStandingRecord(ctx, repo, RemovalCollection, rkey)
		if err != nil {
			return InstanceRemovalResult{}, err
		}
		if removal != nil {
			return InstanceRemovalResult{
				CommunityWriteResult: CommunityWriteResult{URI: uri, RKey: rkey, CID: removal.CID, Rev: head.Rev, Skipped: true},
				Standing:             standingRemovalOf(removal),
			}, nil
		}

		acceptance, err := readStandingRecord(ctx, repo, AcceptanceCollection, rkey)
		if err != nil {
			return InstanceRemovalResult{}, err
		}

		writes := make([]pds.Write, 0, 2)
		if acceptance != nil {
			writes = append(writes, pds.Write{Op: pds.WriteOpDelete, Collection: AcceptanceCollection, RKey: rkey})
		}
		writes = append(writes, pds.Write{Op: pds.WriteOpCreate, Collection: RemovalCollection, RKey: rkey, Record: record})

		if cmd.BeforeCommit != nil {
			if err := cmd.BeforeCommit(ctx, acceptance != nil); err != nil {
				return InstanceRemovalResult{}, fmt.Errorf("before committing the instance removal of %s in %s: %w",
					cmd.SubjectURI, cmd.CommunityDID, err)
			}
		}

		result, err := repo.ApplyWrites(ctx, writes, head.CID)
		if err == nil {
			return InstanceRemovalResult{
				CommunityWriteResult: CommunityWriteResult{
					URI:  uri,
					RKey: rkey,
					CID:  standCIDOf(result, len(writes)-1),
					Rev:  result.CommitRev,
				},
				WithdrewAcceptance: acceptance != nil,
			}, nil
		}

		if err := w.retryStaleShape(ctx, attempt, err); err != nil {
			return InstanceRemovalResult{}, fmt.Errorf("committing the instance removal of %s in %s: %w",
				cmd.SubjectURI, cmd.CommunityDID, err)
		}
	}
}

// UndoInstanceRemoval deletes exactly the removal record the caller wrote.
//
// The match is on the RECORD CID, re-checked on every pass: a removal standing
// under any other CID is somebody else's decision, or a later one, and an undo
// that deleted it would reverse moderation nobody asked to reverse. The batch is
// guarded by the head read before the pre-reads, so a removal replaced between
// the pre-read and the commit is a conflict, and the re-read finds the stranger.
func (w *communityRecordWriter) UndoInstanceRemoval(ctx context.Context, cmd InstanceRemovalUndoCommand) (CommunityWriteResult, error) {
	if err := validateInstanceRemovalUndoCommand(cmd); err != nil {
		return CommunityWriteResult{}, err
	}

	repo, err := w.openRepo(ctx, cmd.CommunityDID)
	if err != nil {
		return CommunityWriteResult{}, err
	}

	rkey := SubjectRkey(cmd.SubjectURI)
	removalURI := recordURI(repo.DID(), RemovalCollection, rkey)

	for attempt := 0; ; attempt++ {
		head, err := repo.GetLatestCommit(ctx)
		if err != nil {
			return CommunityWriteResult{}, fmt.Errorf("reading the head of %s: %w", cmd.CommunityDID, err)
		}

		removal, err := readStandingRecord(ctx, repo, RemovalCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}
		if removal == nil || removal.CID != cmd.RemovalCID {
			return CommunityWriteResult{URI: removalURI, RKey: rkey, Rev: head.Rev, Skipped: true}, nil
		}

		writes := []pds.Write{{Op: pds.WriteOpDelete, Collection: RemovalCollection, RKey: rkey}}
		if cmd.AcceptanceCID != "" {
			acceptance, err := readStandingRecord(ctx, repo, AcceptanceCollection, rkey)
			if err != nil {
				return CommunityWriteResult{}, err
			}
			// An acceptance standing beside a removal should not exist, but the
			// batch has no upsert, so its presence still decides the op.
			op, createdAt := pds.WriteOpCreate, w.stamp()
			if acceptance != nil {
				op = pds.WriteOpUpdate
				if acceptance.CreatedAt != "" {
					createdAt = acceptance.CreatedAt
				}
			}
			writes = append(writes, pds.Write{
				Op:         op,
				Collection: AcceptanceCollection,
				RKey:       rkey,
				Record:     acceptanceRecord(cmd.SubjectURI, cmd.AcceptanceCID, createdAt),
			})
		}

		result, err := repo.ApplyWrites(ctx, writes, head.CID)
		if err == nil {
			if cmd.AcceptanceCID == "" {
				return CommunityWriteResult{URI: removalURI, RKey: rkey, Rev: result.CommitRev}, nil
			}
			return CommunityWriteResult{
				URI:  recordURI(repo.DID(), AcceptanceCollection, rkey),
				RKey: rkey,
				CID:  standCIDOf(result, len(writes)-1),
				Rev:  result.CommitRev,
			}, nil
		}

		if err := w.retryStaleShape(ctx, attempt, err); err != nil {
			return CommunityWriteResult{}, fmt.Errorf("undoing the instance removal of %s in %s: %w",
				cmd.SubjectURI, cmd.CommunityDID, err)
		}
	}
}

// retryStaleShape decides whether a failed batch is retried: a lost swapCommit
// or a 500 means the pre-read went stale, and is met by backing off and reading
// again, up to swapRetryLimit. Any other error, or the limit, is returned.
func (w *communityRecordWriter) retryStaleShape(ctx context.Context, attempt int, err error) error {
	staleShape := errors.Is(err, pds.ErrSwapConflict) || errors.Is(err, pds.ErrServerError)
	if !staleShape || attempt >= swapRetryLimit {
		return err
	}
	return w.backoff(ctx, attempt)
}

// standingRemovalOf is a pre-read removal as the caller sees it.
func standingRemovalOf(standing *standingRecord) *StandingRemoval {
	return &StandingRemoval{
		CID:        standing.CID,
		SubjectURI: standing.SubjectURI,
		SubjectCID: standing.SubjectCID,
		Code:       standing.Code,
		Reason:     standing.Reason,
		CreatedAt:  standing.CreatedAt,
	}
}

// validateInstanceRemovalCommand refuses an instance removal that would not be
// a well-formed removal record, or not a recognisable one: CreatedAt is what
// lets a lost response be told apart from a stranger's removal.
func validateInstanceRemovalCommand(cmd InstanceRemovalCommand) error {
	switch {
	case cmd.CommunityDID == "":
		return fmt.Errorf("instance removal write: %w", NewValidationError("communityDID", "is required"))
	case cmd.SubjectURI == "":
		return fmt.Errorf("instance removal write: %w", NewValidationError("subjectURI", "is required"))
	case cmd.SubjectCID == "":
		return fmt.Errorf("instance removal write: %w", NewValidationError("subjectCID",
			"is required — it is the version the admin reviewed"))
	case cmd.Code == "":
		return fmt.Errorf("instance removal write: %w", NewValidationError("code", "is required"))
	case len(cmd.Code) > removalCodeMaxLength:
		return fmt.Errorf("instance removal write: %w", NewValidationError("code",
			fmt.Sprintf("is %d bytes; the removal lexicon caps code at %d (maxLength)", len(cmd.Code), removalCodeMaxLength)))
	case cmd.CreatedAt == "":
		return fmt.Errorf("instance removal write: %w", NewValidationError("createdAt",
			"is required — it is the instance action's time, which makes a lost response recognisable"))
	}
	return validateSubjectURI("instance removal write", cmd.SubjectURI)
}

// validateInstanceRemovalUndoCommand refuses an undo that names no removal: an
// empty RemovalCID would match nothing, and the subject check matters for the
// same reason it does on DeleteAcceptance — the rkey is a digest of it.
func validateInstanceRemovalUndoCommand(cmd InstanceRemovalUndoCommand) error {
	switch {
	case cmd.CommunityDID == "":
		return fmt.Errorf("instance removal undo: %w", NewValidationError("communityDID", "is required"))
	case cmd.SubjectURI == "":
		return fmt.Errorf("instance removal undo: %w", NewValidationError("subjectURI", "is required"))
	case cmd.RemovalCID == "":
		return fmt.Errorf("instance removal undo: %w", NewValidationError("removalCID",
			"is required — the undo deletes only the removal record it names"))
	}
	return validateSubjectURI("instance removal undo", cmd.SubjectURI)
}
