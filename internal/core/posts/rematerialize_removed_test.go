package posts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
)

// The source must actually shrink on delete: the final re-scan is what keeps a
// skipped legacy record in RemainingLegacy while excluding the migrated one.
type removedContractSource struct {
	*memSource
	deletedURIs []string
}

func (s *removedContractSource) ListLegacyPosts(ctx context.Context) ([]LegacyPost, error) {
	posts, err := s.memSource.ListLegacyPosts(ctx)
	return append([]LegacyPost(nil), posts...), err
}

func (s *removedContractSource) DeleteLegacyPost(ctx context.Context, legacy LegacyPost, swapCID string) error {
	if err := s.memSource.DeleteLegacyPost(ctx, legacy, swapCID); err != nil {
		return err
	}
	s.deletedURIs = append(s.deletedURIs, legacy.URI)
	for index, post := range s.posts {
		if post.URI == legacy.URI {
			s.posts = append(s.posts[:index], s.posts[index+1:]...)
			break
		}
	}
	return nil
}

type removedContractLedger struct {
	*memLedger
	discoveredURIs []string
}

func (l *removedContractLedger) Discover(ctx context.Context, oldURI, communityDID, authorDID string) (RematerializeLedgerRow, error) {
	l.discoveredURIs = append(l.discoveredURIs, oldURI)
	return l.memLedger.Discover(ctx, oldURI, communityDID, authorDID)
}

type removedContractAcceptanceWriter struct {
	*memAcceptanceWriter
	postURIs []string
}

func (w *removedContractAcceptanceWriter) WriteAcceptance(ctx context.Context, command CommunityWriteCommand) (CommunityWriteResult, error) {
	w.postURIs = append(w.postURIs, command.PostURI)
	return w.memAcceptanceWriter.WriteAcceptance(ctx, command)
}

type removedContractIndex struct{ tombstonedURIs []string }

func (i *removedContractIndex) SoftDelete(_ context.Context, uri string) error {
	i.tombstonedURIs = append(i.tombstonedURIs, uri)
	return nil
}

type removedContractLookup struct{ byURI map[string][]RemovalSource }

func (l *removedContractLookup) ActiveRemovalsByURIs(_ context.Context, uris []string) (map[string][]RemovalSource, error) {
	result := make(map[string][]RemovalSource)
	for _, uri := range uris {
		if sources, found := l.byURI[uri]; found {
			result[uri] = sources
		}
	}
	return result, nil
}

func TestRematerialize_OuterContract_SkipsInstanceRemovedLegacyPost(t *testing.T) {
	ctx := context.Background()
	instanceDID := "did:web:coves-instance.invalid"
	communityDID := "did:plc:community2222222222222222"
	removedAuthorDID := "did:plc:removedauthor111111111111"
	remainingAuthorDID := "did:plc:remainingauthor2222222222"
	legacyPost := func(authorDID, rkey string) LegacyPost {
		return LegacyPost{
			URI:          "at://" + communityDID + "/" + LegacyPostCollection + "/" + rkey,
			CID:          "bafylegacy" + rkey,
			CommunityDID: communityDID,
			AuthorDID:    authorDID,
			RawRecord: map[string]any{
				"$type":     LegacyPostCollection,
				"community": communityDID,
				"author":    authorDID,
				"title":     "a legacy post",
				"createdAt": "2026-01-02T03:04:05Z",
			},
		}
	}
	removed := legacyPost(removedAuthorDID, "3kremoved")
	remaining := legacyPost(remainingAuthorDID, "3kremaining")
	source := &removedContractSource{memSource: &memSource{posts: []LegacyPost{removed, remaining}}}
	ledger := &removedContractLedger{memLedger: newMemLedger()}
	communityRepo := &memCommunityRepo{did: communityDID, records: map[string]*pds.RecordResponse{}}
	writer := &removedContractAcceptanceWriter{memAcceptanceWriter: &memAcceptanceWriter{repo: communityRepo}}
	index := &removedContractIndex{}
	authorRepos := map[string]*memAuthorRepo{
		removedAuthorDID:   newMemAuthorRepo(removedAuthorDID),
		remainingAuthorDID: newMemAuthorRepo(remainingAuthorDID),
	}
	var resolvedAuthorDIDs []string
	var progress []RematerializeProgress
	tool := &Rematerializer{
		Source: source,
		Ledger: ledger,
		AuthorRepos: func(_ context.Context, authorDID string, _ *oauth.ClientSessionData) (AuthorRepo, error) {
			resolvedAuthorDIDs = append(resolvedAuthorDIDs, authorDID)
			return authorRepos[authorDID], nil
		},
		Acceptances: writer,
		CommunityRepos: func(context.Context, string) (CommunityRepo, error) {
			return communityRepo, nil
		},
		Index:       index,
		Removals:    &removedContractLookup{byURI: map[string][]RemovalSource{removed.URI: {{AuthorityDID: instanceDID, ScopeKind: "instance"}}}},
		InstanceDID: instanceDID,
		Progress: func(event RematerializeProgress) {
			progress = append(progress, event)
		},
	}

	report, err := tool.Run(ctx)
	require.NoError(t, err)

	_, foundRemoved, err := ledger.Get(ctx, removed.URI)
	require.NoError(t, err)
	require.False(t, foundRemoved, "instance-removed legacy post must never get a ledger row")
	assert.NotContains(t, ledger.discoveredURIs, removed.URI, "Discover must never be called for an instance-removed post")
	assert.NotContains(t, resolvedAuthorDIDs, removedAuthorDID, "removed author's repo must never be resolved")
	assert.Zero(t, authorRepos[removedAuthorDID].puts, "removed author's repo must never receive a postv2")
	for _, postURI := range writer.postURIs {
		assert.False(t, strings.HasPrefix(postURI, "at://"+removedAuthorDID+"/"), "acceptance must not be written for the removed author's post: %s", postURI)
	}
	assert.NotContains(t, source.deletedURIs, removed.URI)
	assert.NotContains(t, index.tombstonedURIs, removed.URI)
	_, stillPresent, err := source.ReadLegacyPost(ctx, removed.URI)
	require.NoError(t, err)
	assert.True(t, stillPresent, "removed post must remain in the legacy source")

	row, foundRemaining, err := ledger.Get(ctx, remaining.URI)
	require.NoError(t, err)
	require.True(t, foundRemaining, "unremoved post must be discovered")
	assert.Equal(t, RematerializeDone, row.State)
	assert.Contains(t, source.deletedURIs, remaining.URI)
	assert.Contains(t, index.tombstonedURIs, remaining.URI)
	assert.Equal(t, []string{"at://" + remainingAuthorDID + "/" + PostV2Collection + "/" + RematerializeRkey(remaining.URI)}, writer.postURIs)
	_, stillPresent, err = source.ReadLegacyPost(ctx, remaining.URI)
	require.NoError(t, err)
	assert.False(t, stillPresent, "migrated post must no longer be in the legacy source")

	assert.Equal(t, 1, report.SkippedRemoved, "census and source passes must count the removed URI only once")
	assert.Equal(t, 1, report.RemainingLegacy)
	assert.False(t, report.ScopeComplete)
	assert.False(t, report.Complete)
	skips := skipEvents(progress, removed.URI)
	require.Len(t, skips, 1, "the census reports the skip; the source pass must not report it again")
	assert.NotEmpty(t, skips[0].Note, "the skipped post must be reported with a note")
}

// skipEvents returns the progress events that report uri as skipped for a
// removal.
func skipEvents(progress []RematerializeProgress, uri string) []RematerializeProgress {
	var events []RematerializeProgress
	for _, event := range progress {
		if event.OldURI == uri && event.To == RematerializeSkippedRemoved {
			events = append(events, event)
		}
	}
	return events
}

type noRematerializeRemovals struct{}

func (noRematerializeRemovals) ActiveRemovalsByURIs(context.Context, []string) (map[string][]RemovalSource, error) {
	return nil, nil
}

type refusalSource struct {
	*memSource
	calls []string
}

func (s *refusalSource) ListLegacyPosts(ctx context.Context) ([]LegacyPost, error) {
	s.calls = append(s.calls, "ListLegacyPosts")
	return s.memSource.ListLegacyPosts(ctx)
}

func (s *refusalSource) ReadLegacyPost(ctx context.Context, uri string) (LegacyPost, bool, error) {
	s.calls = append(s.calls, "ReadLegacyPost")
	return s.memSource.ReadLegacyPost(ctx, uri)
}

func (s *refusalSource) DeleteLegacyPost(ctx context.Context, legacy LegacyPost, swapCID string) error {
	s.calls = append(s.calls, "DeleteLegacyPost")
	return s.memSource.DeleteLegacyPost(ctx, legacy, swapCID)
}

// callRecordingLedger records every ledger call, so a test can assert which
// ones a skipped or refused post reached.
type callRecordingLedger struct {
	RematerializeLedger
	calls []string
}

func (l *callRecordingLedger) Discover(ctx context.Context, oldURI, communityDID, authorDID string) (RematerializeLedgerRow, error) {
	l.calls = append(l.calls, "Discover")
	return l.RematerializeLedger.Discover(ctx, oldURI, communityDID, authorDID)
}

func (l *callRecordingLedger) Get(ctx context.Context, oldURI string) (RematerializeLedgerRow, bool, error) {
	l.calls = append(l.calls, "Get")
	return l.RematerializeLedger.Get(ctx, oldURI)
}

func (l *callRecordingLedger) ListResumable(ctx context.Context, communityDID string) ([]RematerializeLedgerRow, error) {
	l.calls = append(l.calls, "ListResumable")
	return l.RematerializeLedger.ListResumable(ctx, communityDID)
}

func (l *callRecordingLedger) RecordPostV2Written(ctx context.Context, oldURI, sourceCID, newURI, newCID, newRkey string) error {
	l.calls = append(l.calls, "RecordPostV2Written")
	return l.RematerializeLedger.RecordPostV2Written(ctx, oldURI, sourceCID, newURI, newCID, newRkey)
}

func (l *callRecordingLedger) MarkVerified(ctx context.Context, oldURI string) error {
	l.calls = append(l.calls, "MarkVerified")
	return l.RematerializeLedger.MarkVerified(ctx, oldURI)
}

func (l *callRecordingLedger) MarkMigrated(ctx context.Context, oldURI string) error {
	l.calls = append(l.calls, "MarkMigrated")
	return l.RematerializeLedger.MarkMigrated(ctx, oldURI)
}

func (l *callRecordingLedger) MarkDone(ctx context.Context, oldURI string) error {
	l.calls = append(l.calls, "MarkDone")
	return l.RematerializeLedger.MarkDone(ctx, oldURI)
}

func (l *callRecordingLedger) MarkFallback(ctx context.Context, oldURI string, state RematerializeState, reason string) error {
	l.calls = append(l.calls, "MarkFallback")
	return l.RematerializeLedger.MarkFallback(ctx, oldURI, state, reason)
}

func (l *callRecordingLedger) ReopenFallback(ctx context.Context, communityDID string) (int, error) {
	l.calls = append(l.calls, "ReopenFallback")
	return l.RematerializeLedger.ReopenFallback(ctx, communityDID)
}

func (l *callRecordingLedger) CountByState(ctx context.Context, communityDID string) (map[RematerializeState]int, error) {
	l.calls = append(l.calls, "CountByState")
	return l.RematerializeLedger.CountByState(ctx, communityDID)
}

func TestRematerialize_RequiresRemovalSeamBeforeAnyWork(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		run           bool
		missingLookup bool
	}{
		{name: "Run/nil lookup", run: true, missingLookup: true},
		{name: "Run/empty instance DID", run: true},
		{name: "RematerializeOne/nil lookup", missingLookup: true},
		{name: "RematerializeOne/empty instance DID"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			communityDID := "did:plc:community2222222222222222"
			authorDID := "did:plc:author11111111111111111"
			legacy := LegacyPost{
				URI:          "at://" + communityDID + "/" + LegacyPostCollection + "/3krefusal",
				CID:          "bafylegacyrefusal",
				CommunityDID: communityDID,
				AuthorDID:    authorDID,
				RawRecord: map[string]any{
					"$type": LegacyPostCollection, "community": communityDID,
					"author": authorDID, "title": "a legacy post", "createdAt": "2026-01-02T03:04:05Z",
				},
			}
			source := &refusalSource{memSource: &memSource{posts: []LegacyPost{legacy}}}
			ledger := &callRecordingLedger{RematerializeLedger: newMemLedger()}
			authorRepo := newMemAuthorRepo(authorDID)
			communityRepo := &memCommunityRepo{did: communityDID, records: map[string]*pds.RecordResponse{}}
			writer := &removedContractAcceptanceWriter{memAcceptanceWriter: &memAcceptanceWriter{repo: communityRepo}}
			index := &removedContractIndex{}
			var authorResolutions int
			tool := &Rematerializer{
				Source: source, Ledger: ledger,
				AuthorRepos: func(context.Context, string, *oauth.ClientSessionData) (AuthorRepo, error) {
					authorResolutions++
					return authorRepo, nil
				},
				Acceptances: writer,
				CommunityRepos: func(context.Context, string) (CommunityRepo, error) {
					return communityRepo, nil
				},
				Index:       index,
				Removals:    noRematerializeRemovals{},
				InstanceDID: "did:web:coves-instance.invalid",
			}
			missingField := "InstanceDID"
			if testCase.missingLookup {
				tool.Removals = nil
				missingField = "Removals"
			} else {
				tool.InstanceDID = ""
			}

			var err error
			if testCase.run {
				_, err = tool.Run(ctx)
			} else {
				_, err = tool.RematerializeOne(ctx, legacy)
			}
			require.ErrorContains(t, err, missingField, "a missing removal lookup or instance DID must fail, naming it, before touching any seam")
			assert.Empty(t, source.calls, "no legacy source operation may run")
			assert.Empty(t, ledger.calls, "no ledger operation may run")
			assert.Zero(t, authorResolutions, "no author repo may be resolved")
			assert.Zero(t, authorRepo.puts, "no postv2 may be written")
			assert.Empty(t, writer.postURIs, "no acceptance may be written")
			assert.Empty(t, index.tombstonedURIs, "no index row may be tombstoned")
		})
	}
}

func TestRematerializeOne_OnlyInstanceRemovalSkipsBeforeLedgerMutation(t *testing.T) {
	instanceDID := "did:web:coves-instance.invalid"
	otherAuthorityDID := "did:web:other-instance.invalid"
	for _, testCase := range []struct {
		name        string
		removals    []RemovalSource
		seededRow   bool
		wantSkipped bool
	}{
		{
			name:        "instance removal with no ledger row",
			removals:    []RemovalSource{{AuthorityDID: instanceDID, ScopeKind: "instance"}},
			wantSkipped: true,
		},
		{
			name: "instance removal among other authorities",
			removals: []RemovalSource{
				{AuthorityDID: otherAuthorityDID, ScopeKind: "instance"},
				{AuthorityDID: instanceDID, ScopeKind: "instance"},
			},
			wantSkipped: true,
		},
		{
			name:        "instance removal after postv2 was written",
			removals:    []RemovalSource{{AuthorityDID: instanceDID, ScopeKind: "instance"}},
			seededRow:   true,
			wantSkipped: true,
		},
		{
			name:     "another authority instance removal migrates",
			removals: []RemovalSource{{AuthorityDID: otherAuthorityDID, ScopeKind: "instance"}},
		},
		{
			name:     "own authority community removal migrates",
			removals: []RemovalSource{{AuthorityDID: instanceDID, ScopeKind: "community"}},
		},
		{name: "no removal migrates"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			communityDID := "did:plc:community2222222222222222"
			authorDID := "did:plc:author11111111111111111"
			legacy := LegacyPost{
				URI:          "at://" + communityDID + "/" + LegacyPostCollection + "/3kdirect",
				CID:          "bafylegacydirect",
				CommunityDID: communityDID,
				AuthorDID:    authorDID,
				RawRecord: map[string]any{
					"$type":     LegacyPostCollection,
					"community": communityDID,
					"author":    authorDID,
					"title":     "a legacy post",
					"createdAt": "2026-01-02T03:04:05Z",
				},
			}
			source := &removedContractSource{memSource: &memSource{posts: []LegacyPost{legacy}}}
			baseLedger := newMemLedger()
			authorRepo := newMemAuthorRepo(authorDID)
			var seededRow RematerializeLedgerRow
			if testCase.seededRow {
				_, err := baseLedger.Discover(ctx, legacy.URI, communityDID, authorDID)
				require.NoError(t, err)
				rkey := RematerializeRkey(legacy.URI)
				newURI := "at://" + authorDID + "/" + PostV2Collection + "/" + rkey
				newCID := "bafypostv2direct"
				require.NoError(t, baseLedger.RecordPostV2Written(ctx, legacy.URI, legacy.CID, newURI, newCID, rkey))
				var seededFound bool
				seededRow, seededFound, err = baseLedger.Get(ctx, legacy.URI)
				require.NoError(t, err)
				require.True(t, seededFound)
				body, err := postV2Body(legacy)
				require.NoError(t, err)
				authorRepo.records[PostV2Collection+"/"+rkey] = &pds.RecordResponse{URI: newURI, CID: newCID, Value: body}
			}
			ledger := &callRecordingLedger{RematerializeLedger: baseLedger}
			communityRepo := &memCommunityRepo{did: communityDID, records: map[string]*pds.RecordResponse{}}
			writer := &removedContractAcceptanceWriter{memAcceptanceWriter: &memAcceptanceWriter{repo: communityRepo}}
			index := &removedContractIndex{}
			var resolvedAuthorDIDs []string
			var progress []RematerializeProgress
			tool := &Rematerializer{
				Source: source, Ledger: ledger,
				AuthorRepos: func(_ context.Context, did string, _ *oauth.ClientSessionData) (AuthorRepo, error) {
					resolvedAuthorDIDs = append(resolvedAuthorDIDs, did)
					return authorRepo, nil
				},
				Acceptances: writer,
				CommunityRepos: func(context.Context, string) (CommunityRepo, error) {
					return communityRepo, nil
				},
				Index:       index,
				Removals:    &removedContractLookup{byURI: map[string][]RemovalSource{legacy.URI: testCase.removals}},
				InstanceDID: instanceDID,
				Progress: func(event RematerializeProgress) {
					progress = append(progress, event)
				},
			}

			state, err := tool.RematerializeOne(ctx, legacy)
			require.NoError(t, err)
			if testCase.wantSkipped {
				require.Equal(t, RematerializeSkippedRemoved, state, "an active instance removal must leave the legacy post untouched")
				assert.NotContains(t, ledger.calls, "Discover", "skip must precede Discover, even for a resumable row")
				for _, mutation := range []string{"RecordPostV2Written", "MarkVerified", "MarkMigrated", "MarkDone", "MarkFallback", "ReopenFallback"} {
					assert.NotContains(t, ledger.calls, mutation, "skip must not advance the ledger")
				}
				row, found, err := baseLedger.Get(ctx, legacy.URI)
				require.NoError(t, err)
				if testCase.seededRow {
					assert.True(t, found)
					assert.Equal(t, seededRow, row, "the pre-existing ledger row must be byte-for-byte unchanged")
				} else {
					assert.False(t, found, "a skipped post must not acquire a ledger row")
				}
				assert.Empty(t, resolvedAuthorDIDs)
				assert.Zero(t, authorRepo.puts)
				assert.Empty(t, writer.postURIs)
				assert.Empty(t, source.deletedURIs)
				assert.Empty(t, index.tombstonedURIs)
				skips := skipEvents(progress, legacy.URI)
				require.Len(t, skips, 1, "the skipped post must be reported once")
				if testCase.seededRow {
					assert.Contains(t, skips[0].Note, "a postv2 already stands at "+seededRow.NewURI+" and is NOT removed",
						"the skip must point the operator at the postv2 that escaped the removal")
				} else {
					assert.Equal(t, "active instance removal; leaving legacy post untouched", skips[0].Note)
				}
				return
			}

			assert.Equal(t, RematerializeDone, state, "a non-matching removal must not prevent migration")
			row, found, err := baseLedger.Get(ctx, legacy.URI)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, RematerializeDone, row.State)
			assert.Contains(t, ledger.calls, "Discover")
			assert.Equal(t, []string{authorDID}, resolvedAuthorDIDs)
			assert.Equal(t, 1, authorRepo.puts)
			assert.Equal(t, []string{row.NewURI}, writer.postURIs)
			assert.Equal(t, []string{legacy.URI}, source.deletedURIs)
			assert.Equal(t, []string{legacy.URI}, index.tombstonedURIs)
		})
	}
}

type removedRunFixture struct {
	removed              LegacyPost
	clean                LegacyPost
	source               *removedContractSource
	ledger               *removedContractLedger
	lookup               *removedContractLookup
	writer               *removedContractAcceptanceWriter
	index                *removedContractIndex
	authorRepos          map[string]*memAuthorRepo
	resolvedAuthorDIDs   []string
	unavailableAuthorDID string
	tool                 *Rematerializer
}

func newRemovedRunFixture() *removedRunFixture {
	instanceDID := "did:web:coves-instance.invalid"
	communityDID := "did:plc:community2222222222222222"
	removedAuthorDID := "did:plc:removedauthor111111111111"
	cleanAuthorDID := "did:plc:cleanauthor22222222222222"
	post := func(authorDID, rkey string) LegacyPost {
		return LegacyPost{
			URI:          "at://" + communityDID + "/" + LegacyPostCollection + "/" + rkey,
			CID:          "bafylegacy" + rkey,
			CommunityDID: communityDID,
			AuthorDID:    authorDID,
			RawRecord: map[string]any{
				"$type":     LegacyPostCollection,
				"community": communityDID,
				"author":    authorDID,
				"title":     "a legacy post",
				"createdAt": "2026-01-02T03:04:05Z",
			},
		}
	}
	fixture := &removedRunFixture{
		removed: post(removedAuthorDID, "3kremovedrun"),
		clean:   post(cleanAuthorDID, "3kcleanrun"),
		source:  &removedContractSource{memSource: &memSource{}},
		ledger:  &removedContractLedger{memLedger: newMemLedger()},
		index:   &removedContractIndex{},
		authorRepos: map[string]*memAuthorRepo{
			removedAuthorDID: newMemAuthorRepo(removedAuthorDID),
			cleanAuthorDID:   newMemAuthorRepo(cleanAuthorDID),
		},
	}
	fixture.source.posts = []LegacyPost{fixture.removed, fixture.clean}
	fixture.lookup = &removedContractLookup{byURI: map[string][]RemovalSource{
		fixture.removed.URI: {{AuthorityDID: instanceDID, ScopeKind: "instance"}},
	}}
	communityRepo := &memCommunityRepo{did: communityDID, records: map[string]*pds.RecordResponse{}}
	fixture.writer = &removedContractAcceptanceWriter{memAcceptanceWriter: &memAcceptanceWriter{repo: communityRepo}}
	fixture.tool = &Rematerializer{
		Source: fixture.source,
		Ledger: fixture.ledger,
		AuthorRepos: func(_ context.Context, authorDID string, _ *oauth.ClientSessionData) (AuthorRepo, error) {
			fixture.resolvedAuthorDIDs = append(fixture.resolvedAuthorDIDs, authorDID)
			if authorDID == fixture.unavailableAuthorDID {
				return nil, fmt.Errorf("no restorable credentials: %w", ErrNoAuthorCredentials)
			}
			return fixture.authorRepos[authorDID], nil
		},
		Acceptances: fixture.writer,
		CommunityRepos: func(context.Context, string) (CommunityRepo, error) {
			return communityRepo, nil
		},
		Index:       fixture.index,
		Removals:    fixture.lookup,
		InstanceDID: instanceDID,
	}
	return fixture
}

// addPost lists one more legacy post, by its own author, in the fixture's
// community.
func (f *removedRunFixture) addPost(authorDID, rkey string) LegacyPost {
	communityDID := f.removed.CommunityDID
	post := LegacyPost{
		URI:          "at://" + communityDID + "/" + LegacyPostCollection + "/" + rkey,
		CID:          "bafylegacy" + rkey,
		CommunityDID: communityDID,
		AuthorDID:    authorDID,
		RawRecord: map[string]any{
			"$type":     LegacyPostCollection,
			"community": communityDID,
			"author":    authorDID,
			"title":     "a legacy post",
			"createdAt": "2026-01-02T03:04:05Z",
		},
	}
	f.authorRepos[authorDID] = newMemAuthorRepo(authorDID)
	f.source.posts = append(f.source.posts, post)
	return post
}

func TestRematerializeRun_SkippedRemovedResetsOnTheSameRematerializer(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()

	firstReport, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, firstReport.SkippedRemoved, "the first run must count P1 once across its census and source passes")
	_, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	assert.False(t, found, "the skipped post must have no ledger row")
	cleanRow, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, cleanRow.State)
	assert.Equal(t, 1, firstReport.RemainingLegacy)

	delete(fixture.lookup.byURI, fixture.removed.URI)
	secondReport, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, secondReport.SkippedRemoved, "the count must be reset for each Run on the same Rematerializer")
	removedRow, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	require.True(t, found, "a restored legacy post must enter the ledger on the second run")
	assert.Equal(t, RematerializeDone, removedRow.State)
	assert.Contains(t, fixture.source.deletedURIs, fixture.removed.URI)
}

func TestRematerializeRun_RemovedAuthorWithoutCredentialsDoesNotTriggerAbort(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	fixture.unavailableAuthorDID = fixture.removed.AuthorDID
	fixture.tool.AbortOnFallback = true

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err, "a removed author's missing credentials must not abort the run")
	assert.Equal(t, 1, report.SkippedRemoved)
	_, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	assert.False(t, found, "a removed post must not acquire even a fallback row")
	assert.NotContains(t, fixture.ledger.discoveredURIs, fixture.removed.URI)
	assert.NotContains(t, fixture.resolvedAuthorDIDs, fixture.removed.AuthorDID)
	cleanRow, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, cleanRow.State)
	assert.Contains(t, fixture.source.deletedURIs, fixture.clean.URI)
	assert.NotContains(t, fixture.source.deletedURIs, fixture.removed.URI)
}

func TestRematerializeRun_RemovedPostWithFallbackRowStillCountsAsRemaining(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	fixture.unavailableAuthorDID = fixture.removed.AuthorDID
	seededRow := RematerializeLedgerRow{
		OldURI: fixture.removed.URI, CommunityDID: fixture.removed.CommunityDID,
		AuthorDID: fixture.removed.AuthorDID, State: RematerializeFallbackLeftLegacy,
		Reason: "credentials were unavailable on an earlier run",
	}
	fixture.ledger.rows[fixture.removed.URI] = seededRow

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.SkippedRemoved, "a removed post with an existing fallback row still counts as skipped")
	assert.NotContains(t, fixture.ledger.discoveredURIs, fixture.removed.URI, "census must skip before Discover even if a fallback row exists")
	assert.NotContains(t, fixture.resolvedAuthorDIDs, fixture.removed.AuthorDID)
	row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, seededRow, row, "the terminal fallback row and reason must remain unchanged")
	cleanRow, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, cleanRow.State)
	assert.Equal(t, 1, report.RemainingLegacy, "a skipped post stays legacy even when an older fallback row accounts for its URI")
	assert.False(t, report.Complete)
	assert.NotContains(t, fixture.source.deletedURIs, fixture.removed.URI)
}

type removedAfterDiscoveryLookup struct {
	*removedContractLookup
	ledger           *removedContractLedger
	targetURI        string
	checkedBeforeRow bool
	checkedAfterRow  bool
}

func (l *removedAfterDiscoveryLookup) ActiveRemovalsByURIs(ctx context.Context, uris []string) (map[string][]RemovalSource, error) {
	result, err := l.removedContractLookup.ActiveRemovalsByURIs(ctx, uris)
	if err != nil {
		return nil, err
	}
	for _, uri := range uris {
		if uri != l.targetURI {
			continue
		}
		_, discovered, err := l.ledger.Get(ctx, uri)
		if err != nil {
			return nil, err
		}
		if discovered {
			l.checkedAfterRow = true
			result[uri] = []RemovalSource{{AuthorityDID: "did:web:coves-instance.invalid", ScopeKind: "instance"}}
		} else {
			l.checkedBeforeRow = true
		}
	}
	return result, nil
}

func TestRematerializeRun_RemovalAfterCensusDiscoverySkipsSourcePass(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	const blobCID = "bafkreiembeddedblobcid"
	fixture.clean.RawRecord["embed"] = map[string]any{
		"$type": "social.coves.embed.images",
		"images": []any{map[string]any{
			"alt":   "an image",
			"image": map[string]any{"$type": "blob", "ref": map[string]any{"$link": blobCID}, "mimeType": "image/png"},
		}},
	}
	fixture.source.posts = []LegacyPost{fixture.clean}
	blobs := &countingBlobClient{bytesFor: map[string][]byte{blobCID: []byte("PNGDATA")}}
	fixture.tool.Blobs = blobs
	lookup := &removedAfterDiscoveryLookup{
		removedContractLookup: &removedContractLookup{byURI: map[string][]RemovalSource{}},
		ledger:                fixture.ledger, targetURI: fixture.clean.URI,
	}
	fixture.tool.Removals = lookup

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	assert.True(t, lookup.checkedBeforeRow, "the census must see the post before discovery")
	assert.True(t, lookup.checkedAfterRow, "the source pass must recheck the removal after discovery")
	row, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found, "the clean census post must have been discovered")
	assert.Equal(t, RematerializeDiscovered, row.State)
	assert.Empty(t, row.SourceCID)
	assert.Empty(t, row.NewURI)
	assert.Equal(t, []string{fixture.clean.URI}, fixture.ledger.discoveredURIs, "the source pass must not call Discover again")
	assert.Equal(t, 1, fixture.ledger.writes, "only the census discovery may mutate the ledger")
	assert.Zero(t, fixture.authorRepos[fixture.clean.AuthorDID].puts)
	assert.Zero(t, fixture.authorRepos[fixture.clean.AuthorDID].uploads)
	assert.Zero(t, blobs.fetches)
	assert.Empty(t, fixture.writer.postURIs)
	assert.Empty(t, fixture.source.deletedURIs)
	assert.Empty(t, fixture.index.tombstonedURIs)
	assert.Equal(t, 1, report.SkippedRemoved)
	assert.Equal(t, 1, report.RemainingLegacy)
	assert.False(t, report.ScopeComplete)
	assert.False(t, report.Complete)
}

// seedResumableRemovedRow leaves the fixture's removed post as an earlier run
// would have: a ledger row at state, the postv2 standing in the author's repo,
// and, past postv2_written, the community's acceptance.
func seedResumableRemovedRow(t *testing.T, fixture *removedRunFixture, state RematerializeState) RematerializeLedgerRow {
	t.Helper()
	rkey := RematerializeRkey(fixture.removed.URI)
	newURI := "at://" + fixture.removed.AuthorDID + "/" + PostV2Collection + "/" + rkey
	newCID := "bafypostv2resumed"
	seededRow := RematerializeLedgerRow{
		OldURI: fixture.removed.URI, CommunityDID: fixture.removed.CommunityDID,
		AuthorDID: fixture.removed.AuthorDID, State: state,
		SourceCID: fixture.removed.CID, NewURI: newURI, NewCID: newCID, NewRkey: rkey,
		CreatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.January, 3, 4, 5, 6, 0, time.UTC),
	}
	fixture.ledger.rows[fixture.removed.URI] = seededRow
	body, err := postV2Body(fixture.removed)
	require.NoError(t, err)
	fixture.authorRepos[fixture.removed.AuthorDID].records[PostV2Collection+"/"+rkey] = &pds.RecordResponse{
		URI: newURI, CID: newCID, Value: body,
	}
	if state != RematerializePostV2Written {
		fixture.writer.repo.records[AcceptanceCollection+"/"+SubjectRkey(newURI)] = &pds.RecordResponse{
			CID:   "bafyacceptresumed",
			Value: map[string]any{"subject": map[string]any{"uri": newURI, "cid": newCID}},
		}
	}
	return seededRow
}

func TestRematerializeRun_RemovedResumableRowsStayUnchanged(t *testing.T) {
	for _, state := range []RematerializeState{RematerializePostV2Written, RematerializeVerified, RematerializeMigrated} {
		for _, listed := range []bool{true, false} {
			name := string(state) + "/ledger only"
			if listed {
				name = string(state) + "/source and ledger"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				fixture := newRemovedRunFixture()
				if listed {
					fixture.source.posts = []LegacyPost{fixture.removed}
				} else {
					fixture.source.posts = nil
				}
				seededRow := seedResumableRemovedRow(t, fixture, state)
				newURI := seededRow.NewURI
				ledgerSpy := &callRecordingLedger{RematerializeLedger: fixture.ledger}
				fixture.tool.Ledger = ledgerSpy
				var progress []RematerializeProgress
				fixture.tool.Progress = func(event RematerializeProgress) { progress = append(progress, event) }

				report, err := fixture.tool.Run(ctx)
				require.NoError(t, err)
				row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
				require.NoError(t, err)
				require.True(t, found)
				assert.Equal(t, seededRow, row, "a removed resumable row must retain all fields")
				assert.Contains(t, ledgerSpy.calls, "ListResumable", "the seeded row must reach the reconcile pass")
				for _, mutation := range []string{"Discover", "RecordPostV2Written", "MarkVerified", "MarkMigrated", "MarkDone", "MarkFallback"} {
					assert.NotContains(t, ledgerSpy.calls, mutation)
				}
				assert.Zero(t, fixture.ledger.writes)
				assert.Empty(t, fixture.resolvedAuthorDIDs)
				assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].puts)
				assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].uploads)
				assert.Empty(t, fixture.writer.postURIs)
				assert.Empty(t, fixture.source.deletedURIs)
				assert.Empty(t, fixture.index.tombstonedURIs)
				skips := skipEvents(progress, fixture.removed.URI)
				namingPostV2 := 0
				for _, event := range skips {
					assert.NotEqual(t, "reconciled from the ledger", event.Note,
						"Run must not re-report RematerializeOne's skip as a %s → %s reconcile", event.From, event.To)
					if strings.Contains(event.Note, "a postv2 already stands at "+newURI+" and is NOT removed") {
						namingPostV2++
					}
				}
				assert.Equal(t, 1, namingPostV2,
					"the ledger reconcile must reach the removed row once, and its skip must name the postv2 that escaped the removal")
				assert.Equal(t, 1, report.SkippedRemoved, "census, source and reconcile sightings count once")
				assert.Equal(t, map[RematerializeState]int{state: 1}, report.ByState)
				remaining := 0
				if listed {
					remaining = 1
				}
				assert.Equal(t, remaining, report.RemainingLegacy)
				assert.False(t, report.ScopeComplete)
				assert.False(t, report.Complete)
			})
		}
	}
}

func TestRematerializeRun_ReopenedFallbackRemovedBeforeRetry(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	fixture.source.posts = []LegacyPost{fixture.removed}
	// ReopenFallback leaves the row at discovered and clears the fallback reason.
	seededRow := RematerializeLedgerRow{
		OldURI: fixture.removed.URI, CommunityDID: fixture.removed.CommunityDID,
		AuthorDID: fixture.removed.AuthorDID, State: RematerializeDiscovered,
		CreatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.January, 3, 4, 5, 6, 0, time.UTC),
	}
	fixture.ledger.rows[fixture.removed.URI] = seededRow
	ledgerSpy := &callRecordingLedger{RematerializeLedger: fixture.ledger}
	fixture.tool.Ledger = ledgerSpy

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, seededRow, row, "the reopened row must stay discovered")
	assert.NotContains(t, ledgerSpy.calls, "Discover")
	for _, mutation := range []string{"RecordPostV2Written", "MarkVerified", "MarkMigrated", "MarkDone", "MarkFallback"} {
		assert.NotContains(t, ledgerSpy.calls, mutation)
	}
	assert.Zero(t, fixture.ledger.writes)
	assert.Empty(t, fixture.resolvedAuthorDIDs)
	assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].puts)
	assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].uploads)
	assert.Empty(t, fixture.writer.postURIs)
	assert.Empty(t, fixture.source.deletedURIs)
	assert.Empty(t, fixture.index.tombstonedURIs)
	assert.Equal(t, 1, report.SkippedRemoved)
	assert.Equal(t, 1, report.RemainingLegacy)
	assert.False(t, report.ScopeComplete)
	assert.False(t, report.Complete)
}

type conditionalRemovalFailureLookup struct {
	failure       error
	failURI       string
	afterDoneURI  string
	ledger        *removedContractLedger
	askedURIs     []string
	checkedBefore bool
	failedAfter   bool
}

func (l *conditionalRemovalFailureLookup) ActiveRemovalsByURIs(ctx context.Context, uris []string) (map[string][]RemovalSource, error) {
	for _, uri := range uris {
		l.askedURIs = append(l.askedURIs, uri)
		if l.failURI != "" && uri != l.failURI {
			continue
		}
		if l.afterDoneURI != "" {
			row, found, err := l.ledger.Get(ctx, l.afterDoneURI)
			if err != nil {
				return nil, err
			}
			if !found || row.State != RematerializeDone {
				l.checkedBefore = true
				continue
			}
			l.failedAfter = true
		}
		return nil, l.failure
	}
	return nil, nil
}

type censusSnapshotLedger struct {
	*callRecordingLedger
	targetURI string
	row       RematerializeLedgerRow
	seen      bool
}

func (l *censusSnapshotLedger) Discover(ctx context.Context, oldURI, communityDID, authorDID string) (RematerializeLedgerRow, error) {
	row, err := l.callRecordingLedger.Discover(ctx, oldURI, communityDID, authorDID)
	if err == nil && oldURI == l.targetURI {
		l.row = row
		l.seen = true
	}
	return row, err
}

func TestRematerializeRun_RemovalLookupErrorOnFirstRecordStopsBeforeDiscovery(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	errLookupUnavailable := errors.New("removal lookup unavailable")
	lookup := &conditionalRemovalFailureLookup{failure: errLookupUnavailable}
	fixture.tool.Removals = lookup
	ledgerSpy := &callRecordingLedger{RematerializeLedger: fixture.ledger}
	fixture.tool.Ledger = ledgerSpy

	_, err := fixture.tool.Run(ctx)
	require.ErrorIs(t, err, errLookupUnavailable)
	assert.Equal(t, []string{fixture.removed.URI}, lookup.askedURIs, "the first lookup error must halt enumeration")
	assert.NotContains(t, ledgerSpy.calls, "Discover")
	assert.Zero(t, fixture.ledger.writes)
	for _, legacy := range []LegacyPost{fixture.removed, fixture.clean} {
		_, found, getErr := fixture.ledger.Get(ctx, legacy.URI)
		require.NoError(t, getErr)
		assert.False(t, found, "the failed run must not create a ledger row for %s", legacy.URI)
		assert.Zero(t, fixture.authorRepos[legacy.AuthorDID].puts)
		assert.Zero(t, fixture.authorRepos[legacy.AuthorDID].uploads)
	}
	assert.Empty(t, fixture.resolvedAuthorDIDs)
	assert.Empty(t, fixture.writer.postURIs)
	assert.Empty(t, fixture.source.deletedURIs)
	assert.Empty(t, fixture.index.tombstonedURIs)
}

func TestRematerializeRun_RemovalLookupErrorAfterCompletedPostKeepsProgress(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	first := fixture.clean
	second := fixture.removed
	errLookupUnavailable := errors.New("removal lookup unavailable")
	lookup := &conditionalRemovalFailureLookup{
		failure: errLookupUnavailable, failURI: second.URI,
		afterDoneURI: first.URI, ledger: fixture.ledger,
	}
	fixture.tool.Removals = lookup
	ledgerSpy := &censusSnapshotLedger{
		callRecordingLedger: &callRecordingLedger{RematerializeLedger: fixture.ledger},
		targetURI:           second.URI,
	}
	fixture.tool.Ledger = ledgerSpy
	const blobCID = "bafkreierrorpathblob"
	second.RawRecord["embed"] = map[string]any{
		"$type": "social.coves.embed.images",
		"images": []any{map[string]any{
			"alt":   "an image",
			"image": map[string]any{"$type": "blob", "ref": map[string]any{"$link": blobCID}, "mimeType": "image/png"},
		}},
	}
	fixture.source.posts = []LegacyPost{first, second}
	blobs := &countingBlobClient{bytesFor: map[string][]byte{blobCID: []byte("PNGDATA")}}
	fixture.tool.Blobs = blobs

	_, err := fixture.tool.Run(ctx)
	require.ErrorIs(t, err, errLookupUnavailable)
	assert.True(t, lookup.checkedBefore, "P2 must have passed the census lookup while P1 was not done")
	assert.True(t, lookup.failedAfter, "P2 must fail its source-pass lookup only after P1 reached done")
	require.True(t, ledgerSpy.seen, "the census must discover P2 before its later lookup fails")
	firstRow, found, err := fixture.ledger.Get(ctx, first.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, firstRow.State)
	assert.Equal(t, []string{firstRow.NewURI}, fixture.writer.postURIs, "P1 acceptance must persist")
	assert.Equal(t, []string{first.URI}, fixture.source.deletedURIs, "P1 delete must persist")
	assert.Equal(t, []string{first.URI}, fixture.index.tombstonedURIs, "P1 tombstone must persist")
	assert.Equal(t, 1, fixture.authorRepos[first.AuthorDID].puts)
	secondRow, found, err := fixture.ledger.Get(ctx, second.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ledgerSpy.row, secondRow, "P2's census row must be unchanged in every field")
	assert.Equal(t, RematerializeDiscovered, secondRow.State)
	secondDiscoveries := 0
	for _, uri := range fixture.ledger.discoveredURIs {
		if uri == second.URI {
			secondDiscoveries++
		}
	}
	assert.Equal(t, 1, secondDiscoveries, "only the census may Discover P2; the source pass must not Discover it again")
	assert.Zero(t, fixture.authorRepos[second.AuthorDID].puts)
	assert.Zero(t, fixture.authorRepos[second.AuthorDID].uploads)
	assert.Zero(t, blobs.fetches)
	assert.NotContains(t, fixture.source.deletedURIs, second.URI)
	assert.NotContains(t, fixture.index.tombstonedURIs, second.URI)
}

func TestRematerializeRun_RemovalLookupErrorOnLedgerOnlyRowLeavesItUntouched(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	fixture.source.posts = nil
	errLookupUnavailable := errors.New("removal lookup unavailable")
	lookup := &conditionalRemovalFailureLookup{failure: errLookupUnavailable, failURI: fixture.removed.URI}
	fixture.tool.Removals = lookup
	rkey := RematerializeRkey(fixture.removed.URI)
	newURI := "at://" + fixture.removed.AuthorDID + "/" + PostV2Collection + "/" + rkey
	newCID := "bafypostv2ledgeronly"
	seededRow := RematerializeLedgerRow{
		OldURI: fixture.removed.URI, CommunityDID: fixture.removed.CommunityDID,
		AuthorDID: fixture.removed.AuthorDID, State: RematerializeMigrated,
		SourceCID: fixture.removed.CID, NewURI: newURI, NewCID: newCID, NewRkey: rkey,
		CreatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.January, 3, 4, 5, 6, 0, time.UTC),
	}
	fixture.ledger.rows[fixture.removed.URI] = seededRow
	body, err := postV2Body(fixture.removed)
	require.NoError(t, err)
	fixture.authorRepos[fixture.removed.AuthorDID].records[PostV2Collection+"/"+rkey] = &pds.RecordResponse{
		URI: newURI, CID: newCID, Value: body,
	}
	fixture.writer.repo.records[AcceptanceCollection+"/"+SubjectRkey(newURI)] = &pds.RecordResponse{
		CID:   "bafyacceptledgeronly",
		Value: map[string]any{"subject": map[string]any{"uri": newURI, "cid": newCID}},
	}
	ledgerSpy := &callRecordingLedger{RematerializeLedger: fixture.ledger}
	fixture.tool.Ledger = ledgerSpy

	_, err = fixture.tool.Run(ctx)
	require.ErrorIs(t, err, errLookupUnavailable)
	assert.Contains(t, ledgerSpy.calls, "ListResumable")
	assert.Equal(t, []string{fixture.removed.URI}, lookup.askedURIs, "only the reconcile pass may reach the missing legacy record")
	row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, seededRow, row, "the failed lookup must not change any ledger field")
	for _, mutation := range []string{"Discover", "RecordPostV2Written", "MarkVerified", "MarkMigrated", "MarkDone", "MarkFallback"} {
		assert.NotContains(t, ledgerSpy.calls, mutation)
	}
	assert.Zero(t, fixture.ledger.writes)
	assert.Empty(t, fixture.resolvedAuthorDIDs)
	assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].puts)
	assert.Empty(t, fixture.writer.postURIs)
	assert.Empty(t, fixture.source.deletedURIs)
	assert.Empty(t, fixture.index.tombstonedURIs)
}

func TestDryRun_SkipsInstanceRemovedLegacyPostLikeTheRealRun(t *testing.T) {
	ctx := context.Background()
	realFixture := newRemovedRunFixture()
	dryFixture := newRemovedRunFixture()
	require.Equal(t, realFixture.removed, dryFixture.removed)
	require.Equal(t, realFixture.clean, dryFixture.clean)
	var dryProgress []RematerializeProgress
	dryFixture.tool.Progress = func(event RematerializeProgress) {
		dryProgress = append(dryProgress, event)
	}

	realReport, err := realFixture.tool.Run(ctx)
	require.NoError(t, err)
	dry := DryRunOf(dryFixture.tool)
	dryReport, err := dry.Run(ctx)
	require.NoError(t, err)

	require.Equal(t, 1, realReport.SkippedRemoved)
	assert.Equal(t, realReport.SkippedRemoved, dryReport.SkippedRemoved)
	assert.Equal(t, realReport.RemainingLegacy, dryReport.RemainingLegacy)
	assert.Equal(t, realReport.Done, dryReport.Done)
	assert.Equal(t, realReport.Discovered, dryReport.Discovered)
	assert.Equal(t, realReport.ScopeComplete, dryReport.ScopeComplete)
	assert.Equal(t, realReport.Complete, dryReport.Complete)
	assert.Equal(t, 1, dryReport.RemainingLegacy, "P1 must remain in the source after the simulated P2 delete")
	assert.Equal(t, 1, dryReport.Done)
	assert.Equal(t, 1, dryReport.Discovered)
	assert.False(t, dryReport.ScopeComplete)
	assert.False(t, dryReport.Complete)
	deletes, isDry := DryRunDeletes(dry)
	require.True(t, isDry)
	assert.Equal(t, 1, deletes, "only clean P2 should be scheduled for deletion")
	tombstones, isDry := DryRunTombstones(dry)
	require.True(t, isDry)
	assert.Equal(t, 1, tombstones, "only clean P2 should be scheduled for a tombstone")

	assert.Empty(t, dryFixture.ledger.rows, "the dry run must not persist even P2's ledger row")
	assert.Zero(t, dryFixture.ledger.writes)
	assert.Empty(t, dryFixture.ledger.discoveredURIs)
	assert.Empty(t, dryFixture.writer.postURIs)
	assert.Zero(t, dryFixture.writer.calls)
	assert.Empty(t, dryFixture.writer.repo.records)
	assert.Zero(t, dryFixture.writer.repo.puts)
	assert.Empty(t, dryFixture.source.deletedURIs)
	assert.Zero(t, dryFixture.source.deletes)
	assert.Empty(t, dryFixture.index.tombstonedURIs)
	for _, authorDID := range []string{dryFixture.removed.AuthorDID, dryFixture.clean.AuthorDID} {
		repo := dryFixture.authorRepos[authorDID]
		assert.Zero(t, repo.puts)
		assert.Zero(t, repo.uploads)
		assert.Zero(t, repo.deletes)
		assert.Empty(t, repo.records)
	}
	assert.Equal(t, []string{dryFixture.clean.AuthorDID}, dryFixture.resolvedAuthorDIDs,
		"P1 must be skipped before resolving A1, while clean P2 must resolve A2")
	drySkips := skipEvents(dryProgress, dryFixture.removed.URI)
	require.Len(t, drySkips, 1, "the dry run must report P1's skip once")
	assert.NotEmpty(t, drySkips[0].Note, "the dry run must report P1's skip with a note")
}

// scopedRemovedSource lists only its scope's posts, as the production source
// does for a -community run.
type scopedRemovedSource struct {
	*removedContractSource
	scope string
}

func (s *scopedRemovedSource) ListLegacyPosts(ctx context.Context) ([]LegacyPost, error) {
	all, err := s.removedContractSource.ListLegacyPosts(ctx)
	if err != nil {
		return nil, err
	}
	var inScope []LegacyPost
	for _, post := range all {
		if s.scope == "" || post.CommunityDID == s.scope {
			inScope = append(inScope, post)
		}
	}
	return inScope, nil
}

// scopedRemovedLedger resumes and counts by community, as the migration-037
// ledger does.
type scopedRemovedLedger struct {
	*removedContractLedger
}

func (l scopedRemovedLedger) ListResumable(_ context.Context, communityDID string) ([]RematerializeLedgerRow, error) {
	var rows []RematerializeLedgerRow
	for _, row := range l.rows {
		if (communityDID == "" || row.CommunityDID == communityDID) && row.State != RematerializeDone && !IsFallback(row.State) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (l scopedRemovedLedger) CountByState(_ context.Context, communityDID string) (map[RematerializeState]int, error) {
	counts := map[RematerializeState]int{}
	for _, row := range l.rows {
		if communityDID == "" || row.CommunityDID == communityDID {
			counts[row.State]++
		}
	}
	return counts, nil
}

// A scoped run re-scans only its own community, so it cannot see a legacy post
// standing in another one: here, one a scoped run of that community skipped for
// an instance removal. Only an unscoped run may report the migration Complete.
func TestRematerializeRun_ScopedRunNeverReportsComplete(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	communityB := fixture.removed.CommunityDID
	communityA := "did:plc:communityaaaaaaaaaaaaaaaaa"
	inA := fixture.clean
	inA.URI = "at://" + communityA + "/" + LegacyPostCollection + "/3kcleanina"
	inA.CommunityDID = communityA
	inA.RawRecord = map[string]any{
		"$type":     LegacyPostCollection,
		"community": communityA,
		"author":    inA.AuthorDID,
		"title":     "a legacy post",
		"createdAt": "2026-01-02T03:04:05Z",
	}
	fixture.source.posts = []LegacyPost{fixture.removed, inA}
	source := &scopedRemovedSource{removedContractSource: fixture.source}
	fixture.tool.Source = source
	fixture.tool.Ledger = scopedRemovedLedger{removedContractLedger: fixture.ledger}

	source.scope, fixture.tool.CommunityScope = communityB, communityB
	reportB, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reportB.SkippedRemoved, "the B run must skip the removed post")
	assert.Equal(t, 1, reportB.RemainingLegacy)
	assert.False(t, reportB.Complete)

	source.scope, fixture.tool.CommunityScope = communityA, communityA
	reportA, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	rowA, found, err := fixture.ledger.Get(ctx, inA.URI)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, RematerializeDone, rowA.State)
	assert.True(t, reportA.ScopeComplete, "the A run drained its own scope")
	assert.Zero(t, reportA.RemainingLegacy, "the A run's re-scan cannot see community B")
	assert.Equal(t, reportA.GlobalDiscovered, reportA.GlobalDone, "every row the ledger knows is done")
	assert.False(t, reportA.Complete,
		"a scoped run reported the whole migration complete while the removed legacy post still stands in another community")

	source.scope, fixture.tool.CommunityScope = "", ""
	reportAll, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, reportAll.RemainingLegacy, "an unscoped run sees the removed post")
	assert.False(t, reportAll.Complete)

	delete(fixture.lookup.byURI, fixture.removed.URI)
	reportAll, err = fixture.tool.Run(ctx)
	require.NoError(t, err)
	assert.Zero(t, reportAll.RemainingLegacy)
	assert.True(t, reportAll.Complete, "an unscoped run over a drained migration is the §11 step 6 gate")
}

// removalByCheckLookup answers the n-th removal check of targetURI with
// removedOnCheck[n-1], and repeats the last answer after that. Every other URI
// has no removal.
type removalByCheckLookup struct {
	targetURI      string
	instanceDID    string
	removedOnCheck []bool
	checks         int
}

func (l *removalByCheckLookup) ActiveRemovalsByURIs(_ context.Context, uris []string) (map[string][]RemovalSource, error) {
	result := make(map[string][]RemovalSource)
	for _, uri := range uris {
		if uri != l.targetURI {
			continue
		}
		l.checks++
		if l.removedOnCheck[min(l.checks, len(l.removedOnCheck))-1] {
			result[uri] = []RemovalSource{{AuthorityDID: l.instanceDID, ScopeKind: "instance"}}
		}
	}
	return result, nil
}

// The census verdict holds for the whole run. A post the census skipped is not
// retried by the source pass even if the removal is lifted in between: its
// author never went through the credential census.
func TestRematerializeRun_CensusSkipHoldsWhenTheRemovalIsLiftedBeforeTheSourcePass(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	lookup := &removalByCheckLookup{
		targetURI: fixture.removed.URI, instanceDID: fixture.tool.InstanceDID,
		removedOnCheck: []bool{true, false},
	}
	fixture.tool.Removals = lookup
	var progress []RematerializeProgress
	fixture.tool.Progress = func(event RematerializeProgress) { progress = append(progress, event) }

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, lookup.checks, "only the census may check a post the census skipped")
	_, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	assert.False(t, found, "a post the census skipped must not acquire a ledger row later in the run")
	assert.NotContains(t, fixture.ledger.discoveredURIs, fixture.removed.URI)
	assert.NotContains(t, fixture.resolvedAuthorDIDs, fixture.removed.AuthorDID,
		"the skipped post's author never went through the credential census")
	assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].puts)
	assert.Zero(t, fixture.authorRepos[fixture.removed.AuthorDID].uploads)
	assert.NotContains(t, fixture.source.deletedURIs, fixture.removed.URI)
	assert.NotContains(t, fixture.index.tombstonedURIs, fixture.removed.URI)
	cleanRow, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, cleanRow.State)
	assert.Equal(t, []string{cleanRow.NewURI}, fixture.writer.postURIs, "only the clean post may be accepted")
	assert.Equal(t, 1, report.SkippedRemoved)
	assert.Equal(t, 1, report.RemainingLegacy, "the skipped post still stands as legacy")
	assert.False(t, report.ScopeComplete)
	assert.False(t, report.Complete)
	assert.Len(t, skipEvents(progress, fixture.removed.URI), 1,
		"the census reports the skip; the source pass must not report it again")
}

// The census verdict also holds for the ledger reconcile. A listed post whose
// row an earlier run left past discovered, skipped by the census, is not
// finished by the reconcile pass when the removal is lifted in between, and the
// census's own skip names the postv2 that already stands.
func TestRematerializeRun_CensusSkipHoldsForAResumableRowWhenTheRemovalIsLifted(t *testing.T) {
	for _, state := range []RematerializeState{RematerializePostV2Written, RematerializeVerified, RematerializeMigrated} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			fixture := newRemovedRunFixture()
			fixture.source.posts = []LegacyPost{fixture.removed}
			seededRow := seedResumableRemovedRow(t, fixture, state)
			lookup := &removalByCheckLookup{
				targetURI: fixture.removed.URI, instanceDID: fixture.tool.InstanceDID,
				removedOnCheck: []bool{true, false},
			}
			fixture.tool.Removals = lookup
			var progress []RematerializeProgress
			fixture.tool.Progress = func(event RematerializeProgress) { progress = append(progress, event) }

			report, err := fixture.tool.Run(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, lookup.checks, "only the census may check a post the census skipped")
			row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, seededRow, row, "the reconcile pass must not advance a row the census skipped")
			assert.Empty(t, fixture.writer.postURIs)
			assert.Empty(t, fixture.source.deletedURIs)
			assert.Empty(t, fixture.index.tombstonedURIs)
			assert.Equal(t, 1, report.SkippedRemoved)
			assert.Equal(t, 1, report.RemainingLegacy)
			skips := skipEvents(progress, fixture.removed.URI)
			require.Len(t, skips, 1, "the census reports the skip once")
			assert.Contains(t, skips[0].Note, "a postv2 already stands at "+seededRow.NewURI+" and is NOT removed")
		})
	}
}

// A removal that lands after the check at the top of RematerializeOne but
// before the acceptance is honoured: the postv2 already written stays
// unaccepted, the row stays at postv2_written, and nothing is deleted.
func TestRematerializeOne_RemovalBeforeTheAcceptanceLeavesThePostV2Unaccepted(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	legacy := fixture.clean
	lookup := &removalByCheckLookup{
		targetURI: legacy.URI, instanceDID: fixture.tool.InstanceDID,
		removedOnCheck: []bool{false, true},
	}
	fixture.tool.Removals = lookup
	var progress []RematerializeProgress
	fixture.tool.Progress = func(event RematerializeProgress) { progress = append(progress, event) }

	state, err := fixture.tool.RematerializeOne(ctx, legacy)
	require.NoError(t, err)
	assert.Equal(t, RematerializeSkippedRemoved, state)
	assert.Equal(t, 2, lookup.checks, "the removal must be checked again right before the acceptance")
	newURI := "at://" + legacy.AuthorDID + "/" + PostV2Collection + "/" + RematerializeRkey(legacy.URI)
	row, found, err := fixture.ledger.Get(ctx, legacy.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializePostV2Written, row.State, "the row must stay at postv2_written")
	assert.Equal(t, newURI, row.NewURI)
	assert.Equal(t, 1, fixture.authorRepos[legacy.AuthorDID].puts, "the postv2 written before the removal stays in the author's repo")
	assert.Zero(t, fixture.writer.calls, "the acceptance must not be written")
	assert.Empty(t, fixture.writer.postURIs)
	assert.Empty(t, fixture.source.deletedURIs)
	assert.Empty(t, fixture.index.tombstonedURIs)
	skips := skipEvents(progress, legacy.URI)
	require.Len(t, skips, 1)
	assert.Contains(t, skips[0].Note, newURI, "the skip must name the unaccepted postv2")
}

// The same late removal, seen by a whole run: the post is counted as skipped,
// stays legacy, and the reconcile pass leaves its postv2_written row alone.
func TestRematerializeRun_RemovalBeforeTheAcceptanceCountsAsSkippedAndRemaining(t *testing.T) {
	ctx := context.Background()
	fixture := newRemovedRunFixture()
	lookup := &removalByCheckLookup{
		targetURI: fixture.removed.URI, instanceDID: fixture.tool.InstanceDID,
		// The census, then the top of RematerializeOne, then the recheck before
		// the acceptance.
		removedOnCheck: []bool{false, false, true},
	}
	fixture.tool.Removals = lookup
	var progress []RematerializeProgress
	fixture.tool.Progress = func(event RematerializeProgress) { progress = append(progress, event) }

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializePostV2Written, row.State)
	cleanRow, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{cleanRow.NewURI}, fixture.writer.postURIs, "the removed post's postv2 must not be accepted")
	assert.NotContains(t, fixture.source.deletedURIs, fixture.removed.URI)
	assert.NotContains(t, fixture.index.tombstonedURIs, fixture.removed.URI)
	assert.Equal(t, 1, report.SkippedRemoved)
	assert.Equal(t, 1, report.RemainingLegacy)
	assert.Equal(t, map[RematerializeState]int{RematerializePostV2Written: 1, RematerializeDone: 1}, report.ByState)
	assert.False(t, report.ScopeComplete)
	assert.False(t, report.Complete)
	for _, event := range skipEvents(progress, fixture.removed.URI) {
		assert.NotEqual(t, "reconciled from the ledger", event.Note)
	}
}

// Mutant M24: a census that skipped on ANY active removal, while
// RematerializeOne stayed strict, would bypass the credential preflight and
// count the post as skipped. A removal by another authority, or by the instance
// at community scope, must go through the census and migrate.
func TestRematerializeRun_NonMatchingRemovalGoesThroughTheCensusAndMigrates(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		removal RemovalSource
	}{
		{name: "instance authority at community scope", removal: RemovalSource{AuthorityDID: "did:web:coves-instance.invalid", ScopeKind: "community"}},
		{name: "another authority at instance scope", removal: RemovalSource{AuthorityDID: "did:web:other-instance.invalid", ScopeKind: "instance"}},
	} {
		t.Run(testCase.name+"/migrates", func(t *testing.T) {
			ctx := context.Background()
			fixture := newRemovedRunFixture()
			fixture.lookup.byURI[fixture.removed.URI] = []RemovalSource{testCase.removal}
			fixture.tool.AbortOnFallback = true

			report, err := fixture.tool.Run(ctx)
			require.NoError(t, err)
			assert.Zero(t, report.SkippedRemoved)
			row, found, err := fixture.ledger.Get(ctx, fixture.removed.URI)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, RematerializeDone, row.State)
			assert.Contains(t, fixture.source.deletedURIs, fixture.removed.URI)
			assert.Zero(t, report.RemainingLegacy)
			assert.True(t, report.Complete)
		})
		t.Run(testCase.name+"/credential census still aborts", func(t *testing.T) {
			ctx := context.Background()
			fixture := newRemovedRunFixture()
			fixture.lookup.byURI[fixture.removed.URI] = []RemovalSource{testCase.removal}
			fixture.unavailableAuthorDID = fixture.removed.AuthorDID
			fixture.tool.AbortOnFallback = true

			_, err := fixture.tool.Run(ctx)
			require.ErrorContains(t, err, fixture.removed.AuthorDID,
				"the credential census must reach a post whose removal does not match, and abort on its stranded author")
			assert.Empty(t, fixture.writer.postURIs, "the abort must precede every repo mutation")
			assert.Empty(t, fixture.source.deletedURIs)
		})
	}
}

// Moderation writes only removals by the instance DID at instance scope, so an
// active removal that matches neither means this tool's INSTANCE_DID differs
// from the server's. Such a post still migrates, but the run counts it.
func TestRematerializeRun_CountsActiveRemovalsThatDoNotMatchTheInstance(t *testing.T) {
	ctx := context.Background()
	otherAuthorityDID := "did:web:other-instance.invalid"
	build := func() (*removedRunFixture, LegacyPost, LegacyPost) {
		fixture := newRemovedRunFixture()
		unmatched := fixture.addPost("did:plc:unmatchedauthor3333333333", "3kunmatched")
		both := fixture.addPost("did:plc:bothauthor444444444444444", "3kboth")
		fixture.lookup.byURI[unmatched.URI] = []RemovalSource{{AuthorityDID: otherAuthorityDID, ScopeKind: "instance"}}
		fixture.lookup.byURI[both.URI] = []RemovalSource{
			{AuthorityDID: otherAuthorityDID, ScopeKind: "instance"},
			{AuthorityDID: fixture.tool.InstanceDID, ScopeKind: "instance"},
		}
		return fixture, unmatched, both
	}
	fixture, unmatched, both := build()
	var progress []RematerializeProgress
	fixture.tool.Progress = func(event RematerializeProgress) { progress = append(progress, event) }

	report, err := fixture.tool.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, report.UnmatchedRemovals,
		"only the post whose every active removal misses the instance DID at instance scope is unmatched")
	assert.Equal(t, 2, report.SkippedRemoved, "the matched post and the post with both kinds of removal are skipped")
	unmatchedRow, found, err := fixture.ledger.Get(ctx, unmatched.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, unmatchedRow.State, "an unmatched removal is counted, not skipped")
	_, found, err = fixture.ledger.Get(ctx, both.URI)
	require.NoError(t, err)
	assert.False(t, found, "a matching removal skips the post whatever else stands")
	cleanRow, found, err := fixture.ledger.Get(ctx, fixture.clean.URI)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, RematerializeDone, cleanRow.State)
	assert.Condition(t, func() bool {
		for _, event := range progress {
			if event.OldURI == unmatched.URI && strings.Contains(event.Note, "not by the instance DID at instance scope") {
				return true
			}
		}
		return false
	}, "the census must report the unmatched removal")

	dryFixture, _, _ := build()
	dryReport, err := DryRunOf(dryFixture.tool).Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, dryReport.UnmatchedRemovals, "the dry run must count the same unmatched removal")
	assert.Equal(t, 2, dryReport.SkippedRemoved)
}
