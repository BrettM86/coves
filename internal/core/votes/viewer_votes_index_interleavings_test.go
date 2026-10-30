//go:build integration

package votes_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Acceptance for PRD §5.1-§5.3 against the real votes table: a viewer's votes
// on one subject through like, unlike, replace and re-like, with Jetstream's
// indexing of each write arriving before, between or after the writes that
// follow it.
//
// The test plays Jetstream. After each AppView write it indexes and
// soft-deletes rows itself, through the Postgres repository, in a controlled
// order and only into states the vote consumer can produce: at most one
// active row per voter and subject, and a new create soft-deletes the
// previous active row first, as the consumer's stale-vote cleanup does. No
// consumer runs. Writes go to the fake PDS, whose committed records are the
// truth the index converges on.
//
// Records the service mints are named new#1, new#2, ... in create order, as
// committedState names them.

// indexEvent is one Jetstream event reaching the vote consumer.
type indexEvent struct {
	create    bool
	label     string
	direction string
}

func indexCreate(label, direction string) indexEvent {
	return indexEvent{create: true, label: label, direction: direction}
}

func indexDelete(label string) indexEvent {
	return indexEvent{label: label}
}

// interleavingStep is a tap on an arrow or a batch of index events, and what
// the viewer sees afterwards.
type interleavingStep struct {
	// tap is the arrow tapped; empty for an indexing step.
	tap string
	// wantCast is the record the tap returns; empty when it toggles off.
	wantCast string
	// wantWrites are the writes the tap commits to the viewer's repo; they
	// show which existing vote the tap acted on.
	wantWrites []string
	index      []indexEvent
	// wantVote is "<direction> <record>", or empty for no vote.
	wantVote string
}

// interleavingRun is one sequence against a fresh votes table.
type interleavingRun struct {
	service    votes.Service
	fake       *fakePDS
	repository votes.Repository
	writes     []string
}

// voteURI is the URI of the record new#N.
func (run *interleavingRun) voteURI(t *testing.T, label string) string {
	t.Helper()
	ordinal, err := strconv.Atoi(strings.TrimPrefix(label, "new#"))
	require.NoErrorf(t, err, "test table: %q is not a minted record", label)
	creates := run.fake.creates()
	require.LessOrEqualf(t, ordinal, len(creates), "test table: %s has not been created yet", label)
	return "at://" + testVoterDID + "/social.coves.feed.vote/" + creates[ordinal-1]
}

// viewerVote renders "<direction> <record>" as the resolver's answer.
func (run *interleavingRun) viewerVote(t *testing.T, vote string) map[string]votes.ViewerVote {
	t.Helper()
	if vote == "" {
		return nil
	}
	direction, label, found := strings.Cut(vote, " ")
	require.Truef(t, found, "test table: %q is not \"<direction> <record>\"", vote)
	return map[string]votes.ViewerVote{testSubject: {Direction: direction, URI: run.voteURI(t, label)}}
}

// index applies one Jetstream event as the vote consumer would.
func (run *interleavingRun) index(t *testing.T, event indexEvent) {
	t.Helper()
	ctx := context.Background()
	uri := run.voteURI(t, event.label)
	if !event.create {
		require.NoError(t, run.repository.Delete(ctx, uri), "test setup: indexing the delete of %s", event.label)
		return
	}
	stale, err := run.repository.GetByVoterAndSubject(ctx, testVoterDID, testSubject)
	switch {
	case errors.Is(err, votes.ErrVoteNotFound):
	case err != nil:
		require.NoError(t, err, "test setup: reading the active row before indexing %s", event.label)
	case stale.URI != uri:
		require.NoError(t, run.repository.Delete(ctx, stale.URI),
			"test setup: soft-deleting the stale row before indexing %s", event.label)
	}
	rkey := uri[strings.LastIndex(uri, "/")+1:]
	require.NoError(t, run.repository.Create(ctx, &votes.Vote{
		URI:        uri,
		CID:        "bafycid" + rkey,
		RKey:       rkey,
		VoterDID:   testVoterDID,
		SubjectURI: testSubject,
		SubjectCID: testSubjectCI,
		Direction:  event.direction,
		CreatedAt:  time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
	}), "test setup: indexing the create of %s", event.label)
}

// apply runs one step and checks the writes so far and what the viewer sees.
func (run *interleavingRun) apply(t *testing.T, number int, step interleavingStep) {
	t.Helper()
	ctx := context.Background()
	description := "step " + strconv.Itoa(number+1)
	if step.tap != "" {
		description += " (tap " + step.tap + ")"
		response, err := run.service.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: step.tap})
		require.NoError(t, err, description)
		run.writes = append(run.writes, step.wantWrites...)
		operations, _ := committedState(run.fake)
		require.Equal(t, run.writes, operations, description+": the writes committed to the viewer's repo")
		if step.wantCast == "" {
			require.Empty(t, response.URI, description+": the tap toggles the vote off")
		} else {
			require.Equal(t, run.voteURI(t, step.wantCast), response.URI, description+": the record the tap cast")
		}
	} else {
		description += " (indexing)"
		for _, event := range step.index {
			run.index(t, event)
		}
	}

	got, err := run.service.ViewerVotes(ctx, testVoterDID, []string{testSubject})
	require.NoError(t, err, description)
	requireViewerVotes(t, run.viewerVote(t, step.wantVote), got, description+": the viewer's vote")
	operations, _ := committedState(run.fake)
	require.Equal(t, run.writes, operations, description+": showing the viewer's vote writes nothing")
}

func TestViewerVotes_IndexingInterleavingsOnRealIndex(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		steps []interleavingStep
		// wantRecords are the viewer's PDS records once indexing has caught
		// up, in committedState's rendering; the viewer's vote must match them.
		wantRecords map[string]string
		// next is a tap after indexing has caught up, and what it acts on.
		next interleavingStep
	}{
		{
			name: "like then unlike, both indexed after the unlike",
			steps: []interleavingStep{
				{tap: "up", wantCast: "new#1", wantWrites: []string{"create(new#1)"}, wantVote: "up new#1"},
				{tap: "up", wantWrites: []string{"delete(new#1)"}},
				{index: []indexEvent{indexCreate("new#1", "up")}},
				{index: []indexEvent{indexDelete("new#1")}},
			},
			wantRecords: map[string]string{},
			next:        interleavingStep{tap: "up", wantCast: "new#2", wantWrites: []string{"create(new#2)"}, wantVote: "up new#2"},
		},
		{
			name: "like then unlike, the create indexed between them",
			steps: []interleavingStep{
				{tap: "up", wantCast: "new#1", wantWrites: []string{"create(new#1)"}, wantVote: "up new#1"},
				{index: []indexEvent{indexCreate("new#1", "up")}, wantVote: "up new#1"},
				{tap: "up", wantWrites: []string{"delete(new#1)"}},
				{index: []indexEvent{indexDelete("new#1")}},
			},
			wantRecords: map[string]string{},
			next:        interleavingStep{tap: "up", wantCast: "new#2", wantWrites: []string{"create(new#2)"}, wantVote: "up new#2"},
		},
		{
			name: "replace then toggle off, all indexing after the writes",
			steps: []interleavingStep{
				{tap: "up", wantCast: "new#1", wantWrites: []string{"create(new#1)"}, wantVote: "up new#1"},
				{tap: "down", wantCast: "new#2", wantWrites: []string{"delete(new#1)", "create(new#2)"}, wantVote: "down new#2"},
				{tap: "down", wantWrites: []string{"delete(new#2)"}},
				{index: []indexEvent{indexCreate("new#1", "up")}},
				{index: []indexEvent{indexDelete("new#1"), indexCreate("new#2", "down")}},
				{index: []indexEvent{indexDelete("new#2")}},
			},
			wantRecords: map[string]string{},
			next:        interleavingStep{tap: "down", wantCast: "new#3", wantWrites: []string{"create(new#3)"}, wantVote: "down new#3"},
		},
		{
			name: "replace then toggle off, indexing one step behind",
			steps: []interleavingStep{
				{tap: "up", wantCast: "new#1", wantWrites: []string{"create(new#1)"}, wantVote: "up new#1"},
				{tap: "down", wantCast: "new#2", wantWrites: []string{"delete(new#1)", "create(new#2)"}, wantVote: "down new#2"},
				{index: []indexEvent{indexCreate("new#1", "up")}, wantVote: "down new#2"},
				{tap: "down", wantWrites: []string{"delete(new#2)"}},
				{index: []indexEvent{indexDelete("new#1"), indexCreate("new#2", "down")}},
				{index: []indexEvent{indexDelete("new#2")}},
			},
			wantRecords: map[string]string{},
			next:        interleavingStep{tap: "down", wantCast: "new#3", wantWrites: []string{"create(new#3)"}, wantVote: "down new#3"},
		},
		{
			name: "replace then toggle off, each write indexed before the next",
			steps: []interleavingStep{
				{tap: "up", wantCast: "new#1", wantWrites: []string{"create(new#1)"}, wantVote: "up new#1"},
				{index: []indexEvent{indexCreate("new#1", "up")}, wantVote: "up new#1"},
				{tap: "down", wantCast: "new#2", wantWrites: []string{"delete(new#1)", "create(new#2)"}, wantVote: "down new#2"},
				{index: []indexEvent{indexDelete("new#1"), indexCreate("new#2", "down")}, wantVote: "down new#2"},
				{tap: "down", wantWrites: []string{"delete(new#2)"}},
				{index: []indexEvent{indexDelete("new#2")}},
			},
			wantRecords: map[string]string{},
			next:        interleavingStep{tap: "down", wantCast: "new#3", wantWrites: []string{"create(new#3)"}, wantVote: "down new#3"},
		},
		{
			name: "like, unlike and like again before anything is indexed",
			steps: []interleavingStep{
				{tap: "up", wantCast: "new#1", wantWrites: []string{"create(new#1)"}, wantVote: "up new#1"},
				{tap: "up", wantWrites: []string{"delete(new#1)"}},
				{tap: "up", wantCast: "new#2", wantWrites: []string{"create(new#2)"}, wantVote: "up new#2"},
				{index: []indexEvent{indexCreate("new#1", "up")}, wantVote: "up new#2"},
				{index: []indexEvent{indexDelete("new#1")}, wantVote: "up new#2"},
				{index: []indexEvent{indexCreate("new#2", "up")}, wantVote: "up new#2"},
			},
			wantRecords: map[string]string{"new#2": "up " + testSubject},
			next:        interleavingStep{tap: "up", wantWrites: []string{"delete(new#2)"}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			repository := postgres.NewVoteRepository(testkit.DB(t))
			run := &interleavingRun{
				service:    newServiceForVoters(t, repository, fake),
				fake:       fake,
				repository: repository,
			}
			for number, step := range testCase.steps {
				run.apply(t, number, step)
			}

			// Indexing has caught up: the viewer's vote is the PDS record set,
			// read from the index alone.
			_, records := committedState(fake)
			require.Equal(t, testCase.wantRecords, records, "the viewer's vote records once indexing has caught up")
			wantFinalVote := ""
			for label, record := range testCase.wantRecords {
				direction, _, _ := strings.Cut(record, " ")
				wantFinalVote = direction + " " + label
			}
			got, err := run.service.ViewerVotes(context.Background(), testVoterDID, []string{testSubject})
			require.NoError(t, err)
			requireViewerVotes(t, run.viewerVote(t, wantFinalVote), got,
				"once indexing has caught up, the viewer's vote matches the PDS records")
			require.Zero(t, votes.OverlaySizeForTest(run.service),
				"every recent write is confirmed once indexing has caught up")

			run.apply(t, len(testCase.steps), testCase.next)
			require.Zero(t, fake.listCalls, "no step scans the PDS: every write found its target in the overlay or the index")
		})
	}
}
