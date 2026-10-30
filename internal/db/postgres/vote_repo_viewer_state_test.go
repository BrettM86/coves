//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/votes"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The two queries behind a viewer's vote state (PRD §5.2): the index query
// for a viewer's active votes on a set of subjects, and the confirmation
// query that reports whether each recently written vote URI is indexed and
// whether it has been soft-deleted.

const (
	viewerStateVoterDID      = "did:plc:viewerstatevoteraaaaaaaa"
	viewerStateOtherVoterDID = "did:plc:viewerstateotherbbbbbbbbb"

	viewerStateFirstSubject       = "at://did:plc:viewerstatecommunity/social.coves.community.post/3kfirst"
	viewerStateSecondSubject      = "at://did:plc:viewerstatecommunity/social.coves.community.post/3ksecond"
	viewerStateThirdSubject       = "at://did:plc:viewerstatecommunity/social.coves.community.post/3kthird"
	viewerStateUnrequestedSubject = "at://did:plc:viewerstatecommunity/social.coves.community.post/3kunrequested"

	viewerStateFirstUpVote         = "at://did:plc:viewerstatevoteraaaaaaaa/social.coves.feed.vote/3kfirstup"
	viewerStateSecondOldUpVote     = "at://did:plc:viewerstatevoteraaaaaaaa/social.coves.feed.vote/3ksecondoldup"
	viewerStateSecondDownVote      = "at://did:plc:viewerstatevoteraaaaaaaa/social.coves.feed.vote/3kseconddown"
	viewerStateThirdDeletedVote    = "at://did:plc:viewerstatevoteraaaaaaaa/social.coves.feed.vote/3kthirdgone"
	viewerStateUnrequestedVote     = "at://did:plc:viewerstatevoteraaaaaaaa/social.coves.feed.vote/3kunrequested"
	viewerStateOtherVoterVote      = "at://did:plc:viewerstateotherbbbbbbbbb/social.coves.feed.vote/3kothervoter"
	viewerStateNeverIndexedVote    = "at://did:plc:viewerstatevoteraaaaaaaa/social.coves.feed.vote/3knotindexed"
	viewerStateOtherVoterThirdVote = "at://did:plc:viewerstateotherbbbbbbbbb/social.coves.feed.vote/3kotherthird"
)

// indexViewerStateVote inserts an active vote row the way the vote consumer
// does when Jetstream delivers a create.
func indexViewerStateVote(t *testing.T, repository votes.Repository, uri, voterDID, subjectURI, direction string) {
	t.Helper()
	require.NoError(t, repository.Create(context.Background(), &votes.Vote{
		URI:        uri,
		CID:        "bafyreiviewerstate",
		RKey:       uri[strings.LastIndex(uri, "/")+1:],
		VoterDID:   voterDID,
		SubjectURI: subjectURI,
		SubjectCID: "bafyreiviewerstatesubject",
		Direction:  direction,
		CreatedAt:  time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
	}), "test setup: indexing %s", uri)
}

// softDeleteViewerStateVote soft-deletes a row the way the vote consumer does
// when Jetstream delivers the delete.
func softDeleteViewerStateVote(t *testing.T, repository votes.Repository, uri string) {
	t.Helper()
	require.NoError(t, repository.Delete(context.Background(), uri), "test setup: soft-deleting %s", uri)
}

// viewerStateRows is the fixture both queries run against.
func viewerStateRows(t *testing.T, db *sql.DB) votes.Repository {
	t.Helper()
	repository := NewVoteRepository(db)
	indexViewerStateVote(t, repository, viewerStateFirstUpVote, viewerStateVoterDID, viewerStateFirstSubject, "up")
	// The second subject had an up vote that was replaced by a down vote.
	indexViewerStateVote(t, repository, viewerStateSecondOldUpVote, viewerStateVoterDID, viewerStateSecondSubject, "up")
	softDeleteViewerStateVote(t, repository, viewerStateSecondOldUpVote)
	indexViewerStateVote(t, repository, viewerStateSecondDownVote, viewerStateVoterDID, viewerStateSecondSubject, "down")
	// The third subject's only vote by the viewer was withdrawn.
	indexViewerStateVote(t, repository, viewerStateThirdDeletedVote, viewerStateVoterDID, viewerStateThirdSubject, "up")
	softDeleteViewerStateVote(t, repository, viewerStateThirdDeletedVote)
	// Another voter's active votes on requested subjects.
	indexViewerStateVote(t, repository, viewerStateOtherVoterVote, viewerStateOtherVoterDID, viewerStateFirstSubject, "down")
	indexViewerStateVote(t, repository, viewerStateOtherVoterThirdVote, viewerStateOtherVoterDID, viewerStateThirdSubject, "up")
	// The viewer's active vote on a subject the request does not name.
	indexViewerStateVote(t, repository, viewerStateUnrequestedVote, viewerStateVoterDID, viewerStateUnrequestedSubject, "up")
	return repository
}

// activeVote is the part of a returned row the resolver uses.
type activeVote struct {
	SubjectURI string
	Direction  string
	URI        string
}

func TestVoteRepo_GetActiveVotesForSubjects(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		voterDID    string
		subjectURIs []string
		want        []activeVote
	}{
		{
			name:        "the viewer's active votes on the requested subjects",
			voterDID:    viewerStateVoterDID,
			subjectURIs: []string{viewerStateFirstSubject, viewerStateSecondSubject, viewerStateThirdSubject},
			want: []activeVote{
				{SubjectURI: viewerStateFirstSubject, Direction: "up", URI: viewerStateFirstUpVote},
				{SubjectURI: viewerStateSecondSubject, Direction: "down", URI: viewerStateSecondDownVote},
			},
		},
		{
			name:        "another voter's view of the same subjects",
			voterDID:    viewerStateOtherVoterDID,
			subjectURIs: []string{viewerStateFirstSubject, viewerStateSecondSubject, viewerStateThirdSubject},
			want: []activeVote{
				{SubjectURI: viewerStateFirstSubject, Direction: "down", URI: viewerStateOtherVoterVote},
				{SubjectURI: viewerStateThirdSubject, Direction: "up", URI: viewerStateOtherVoterThirdVote},
			},
		},
		{
			name:        "no subjects",
			voterDID:    viewerStateVoterDID,
			subjectURIs: []string{},
			want:        nil,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			repository := viewerStateRows(t, testkit.DB(t))

			rows, err := repository.GetActiveVotesForSubjects(context.Background(), testCase.voterDID, testCase.subjectURIs)
			require.NoError(t, err)

			var got []activeVote
			for _, row := range rows {
				require.Equal(t, testCase.voterDID, row.VoterDID, "every row belongs to the requested voter")
				got = append(got, activeVote{SubjectURI: row.SubjectURI, Direction: row.Direction, URI: row.URI})
			}
			require.ElementsMatch(t, testCase.want, got,
				"exactly the voter's active rows on the requested subjects: no soft-deleted row, "+
					"no other voter's row, no row on a subject the request does not name")
		})
	}
}

func TestVoteRepo_GetIndexedStatesByURIs(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		uris []string
		want map[string]votes.IndexedVoteState
	}{
		{
			name: "active, soft-deleted and never-indexed URIs",
			uris: []string{viewerStateSecondDownVote, viewerStateThirdDeletedVote, viewerStateNeverIndexedVote},
			want: map[string]votes.IndexedVoteState{
				viewerStateSecondDownVote:   votes.IndexedVoteActive,
				viewerStateThirdDeletedVote: votes.IndexedVoteDeleted,
			},
		},
		{
			name: "a replaced vote and its replacement, which share a voter and subject",
			uris: []string{viewerStateSecondOldUpVote, viewerStateSecondDownVote},
			want: map[string]votes.IndexedVoteState{
				viewerStateSecondOldUpVote: votes.IndexedVoteDeleted,
				viewerStateSecondDownVote:  votes.IndexedVoteActive,
			},
		},
		{
			name: "no URIs",
			uris: []string{},
			want: map[string]votes.IndexedVoteState{},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			repository := viewerStateRows(t, testkit.DB(t))

			got, err := repository.GetIndexedStatesByURIs(context.Background(), testCase.uris)
			require.NoError(t, err)
			if len(testCase.want) == 0 {
				require.Empty(t, got)
				return
			}
			require.Equal(t, testCase.want, got,
				"each indexed URI reports active or soft-deleted; a URI with no row is absent")
		})
	}
}
