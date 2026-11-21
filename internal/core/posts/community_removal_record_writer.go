package posts

import (
	"context"
	"errors"
	"fmt"

	"Coves/internal/atproto/pds"
)

// CommunityRemovalRecordWriter writes a community's removal record on its own,
// with nothing on an acceptance: the shape of a community's comment removal
// and restore, since a comment has no acceptance. An acceptance standing at the
// same record key is never read or touched.
type CommunityRemovalRecordWriter interface {
	// WriteRemovalRecord makes the removal record stand: one create, or one
	// update keeping the standing record's createdAt (unless cmd replaces
	// it), and nothing (a skip) when the standing record already carries
	// cmd's code and reason (and, when cmd replaces its createdAt, cmd's).
	WriteRemovalRecord(ctx context.Context, cmd CommunityRemovalCommand) (CommunityWriteResult, error)
	// DeleteRemovalRecord deletes the standing removal record in one commit,
	// and writes nothing (a skip, with the head rev and no CID) when none
	// stands.
	DeleteRemovalRecord(ctx context.Context, cmd CommunityRemovalDeleteCommand) (CommunityWriteResult, error)
}

// CommunityRemovalDeleteCommand deletes the community's removal record about
// a subject.
type CommunityRemovalDeleteCommand struct {
	CommunityDID string
	// SubjectURI is the removed record; the removal's rkey derives from it.
	SubjectURI string
	// BeforeCommit, when set, runs before every batch is sent; an error aborts
	// the write with nothing sent. Its attempt has no stand record: ClearCID
	// and ClearCode are the removal the batch deletes.
	BeforeCommit func(ctx context.Context, attempt CommunityCommitAttempt) error
}

// NewCommunityRemovalRecordWriter returns the removal-only writer over the
// repos the factory opens.
func NewCommunityRemovalRecordWriter(repos CommunityRepoFactory, now Clock, opts ...WriterOption) CommunityRemovalRecordWriter {
	w := &communityRecordWriter{repos: repos, now: now, sleep: sleepWithContext}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

func (w *communityRecordWriter) WriteRemovalRecord(ctx context.Context, cmd CommunityRemovalCommand) (CommunityWriteResult, error) {
	if err := validateRemovalCommand(cmd); err != nil {
		return CommunityWriteResult{}, err
	}
	return w.commitPair(ctx, pairCommit{
		communityDID:    cmd.CommunityDID,
		postURI:         cmd.PostURI,
		standCollection: RemovalCollection,
		record: func(createdAt string) map[string]any {
			return removalRecord(cmd, createdAt)
		},
		createdAt:          cmd.CreatedAt,
		replaceCreatedAtOf: cmd.ReplaceCreatedAtOf,
		beforeCommit:       cmd.BeforeCommit,
		unchanged: func(standing *standingRecord) bool {
			return removalUnchanged(standing, cmd)
		},
	})
}

func (w *communityRecordWriter) DeleteRemovalRecord(ctx context.Context, cmd CommunityRemovalDeleteCommand) (CommunityWriteResult, error) {
	switch {
	case cmd.CommunityDID == "":
		return CommunityWriteResult{}, fmt.Errorf("removal delete: %w", NewValidationError("communityDID", "is required"))
	case cmd.SubjectURI == "":
		return CommunityWriteResult{}, fmt.Errorf("removal delete: %w", NewValidationError("subjectURI",
			"is required — the record key is derived from it"))
	}
	if err := validateSubjectURI("removal delete", cmd.SubjectURI); err != nil {
		return CommunityWriteResult{}, err
	}
	repo, err := w.openRepo(ctx, cmd.CommunityDID)
	if err != nil {
		return CommunityWriteResult{}, err
	}
	rkey := SubjectRkey(cmd.SubjectURI)
	uri := recordURI(repo.DID(), RemovalCollection, rkey)

	for attempt := 0; ; attempt++ {
		// The head is read before the record, as commitPair does, so a write
		// interleaved between the two makes the guard stale.
		head, err := repo.GetLatestCommit(ctx)
		if err != nil {
			return CommunityWriteResult{}, fmt.Errorf("reading the head of %s: %w", cmd.CommunityDID, err)
		}
		standing, err := readStandingRecord(ctx, repo, RemovalCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}
		if standing == nil {
			return CommunityWriteResult{URI: uri, RKey: rkey, Rev: head.Rev, Skipped: true}, nil
		}
		if cmd.BeforeCommit != nil {
			if err := cmd.BeforeCommit(ctx, CommunityCommitAttempt{
				GuardCommitCID: head.CID, GuardRev: head.Rev, ClearCID: standing.CID, ClearCode: standing.Code,
			}); err != nil {
				return CommunityWriteResult{}, fmt.Errorf("recording the attempt to delete the removal of %s in %s: %w",
					cmd.SubjectURI, cmd.CommunityDID, err)
			}
		}
		result, err := repo.ApplyWrites(ctx, []pds.Write{{
			Op: pds.WriteOpDelete, Collection: RemovalCollection, RKey: rkey,
		}}, head.CID)
		if err == nil {
			return CommunityWriteResult{URI: uri, RKey: rkey, Rev: result.CommitRev}, nil
		}
		staleShape := errors.Is(err, pds.ErrSwapConflict) || errors.Is(err, pds.ErrServerError)
		if !staleShape || attempt >= swapRetryLimit {
			return CommunityWriteResult{}, fmt.Errorf("deleting the removal of %s in %s: %w", cmd.SubjectURI, cmd.CommunityDID, err)
		}
		if err := w.backoff(ctx, attempt); err != nil {
			return CommunityWriteResult{}, fmt.Errorf("deleting the removal of %s in %s: %w", cmd.SubjectURI, cmd.CommunityDID, err)
		}
	}
}
