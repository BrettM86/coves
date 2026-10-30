package votes

import (
	"context"
	"fmt"
)

// resolvedVote is one subject's answer from the resolver.
type resolvedVote struct {
	// vote is the viewer's vote, or nil for none.
	vote *existingVote
	// unverified means this AppView's last write for the subject had an
	// unknown outcome; vote is then the index value, and a write must read
	// the PDS instead (PRD §5.4).
	unverified bool
	// request is the ambiguous write (unverified only). A retry of the same
	// request returns its result if the PDS shows it holds (D6), and a
	// create that finds no vote reuses its createRKey, so a late commit of
	// the first create collides instead of leaving a second record.
	request ambiguousRequest
}

// ViewerVotes returns the viewer's votes on the subjects. Subjects without a
// vote are absent from the map.
func (s *voteService) ViewerVotes(ctx context.Context, viewerDID string, subjectURIs []string) (map[string]ViewerVote, error) {
	resolved, err := s.resolveViewerVotes(ctx, viewerDID, subjectURIs)
	if err != nil {
		return nil, err
	}
	viewerVotes := make(map[string]ViewerVote, len(resolved))
	for subjectURI, resolution := range resolved {
		if resolution.vote != nil {
			viewerVotes[subjectURI] = ViewerVote{Direction: resolution.vote.Direction, URI: resolution.vote.URI}
		}
	}
	return viewerVotes, nil
}

// resolveViewerVotes resolves the viewer's votes on the subjects from the
// votes index, overlaid by this AppView's own recent writes (PRD §5.2).
//
// The confirmation query runs before the index query, so the index result is
// at least as new as any confirmation paired with it. If either query fails,
// nothing is confirmed and the error is returned.
func (s *voteService) resolveViewerVotes(ctx context.Context, viewerDID string, subjectURIs []string) (map[string]resolvedVote, error) {
	resolved := make(map[string]resolvedVote, len(subjectURIs))
	if len(subjectURIs) == 0 {
		return resolved, nil
	}

	entries := s.overlay.snapshot(viewerDID, subjectURIs, s.now())

	var confirmableURIs []string
	for _, entry := range entries {
		if entry.state == recentWriteVoted || entry.state == recentWriteRemoved {
			confirmableURIs = append(confirmableURIs, entry.uri)
		}
	}
	var indexedStates map[string]IndexedVoteState
	if len(confirmableURIs) > 0 {
		var err error
		indexedStates, err = s.repo.GetIndexedStatesByURIs(ctx, confirmableURIs)
		if err != nil {
			return nil, fmt.Errorf("failed to confirm recent vote writes: %w", err)
		}
	}

	rows, err := s.repo.GetActiveVotesForSubjects(ctx, viewerDID, subjectURIs)
	if err != nil {
		return nil, fmt.Errorf("failed to read indexed votes: %w", err)
	}
	indexedVotes := make(map[string]*existingVote, len(rows))
	for _, row := range rows {
		indexedVotes[row.SubjectURI] = &existingVote{
			URI:       row.URI,
			CID:       row.CID,
			RKey:      row.RKey,
			Direction: row.Direction,
		}
	}

	for _, subjectURI := range subjectURIs {
		resolution := resolvedVote{vote: indexedVotes[subjectURI]}
		entry, hasEntry := entries[subjectURI]
		if hasEntry {
			key := viewerSubjectKey{viewerDID: viewerDID, subjectURI: subjectURI}
			indexedState, indexed := indexedStates[entry.uri]
			switch entry.state {
			case recentWriteVoted:
				// An active row means the create was indexed; a soft-deleted
				// one means a later delete was indexed too.
				if indexed {
					s.overlay.retireIfVersion(key, entry.version)
				} else {
					resolution.vote = &existingVote{URI: entry.uri, RKey: entry.rkey, Direction: entry.direction}
				}
			case recentWriteRemoved:
				// Absence confirms nothing: it cannot tell an indexed delete
				// from a create that has not been indexed yet.
				if indexed && indexedState == IndexedVoteDeleted {
					s.overlay.retireIfVersion(key, entry.version)
				} else {
					resolution.vote = nil
				}
			case recentWriteUnverified:
				resolution.unverified = true
				resolution.request = entry.request
			}
		}
		resolved[subjectURI] = resolution
	}
	return resolved, nil
}
