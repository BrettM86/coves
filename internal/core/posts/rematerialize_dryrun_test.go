package posts

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
)

// A REHEARSAL THAT DOES NOT REHEARSE IS WORSE THAN NO REHEARSAL, because the
// operator then confirms `-yes` on the strength of it.
//
// These tests pin the two halves of that: the dry run really performs every read
// and decision the real run performs, and it performs NONE of the writes.

// ---- in-memory seams -------------------------------------------------------

type memAuthorRepo struct {
	did     string
	records map[string]*pds.RecordResponse
	puts    int
	uploads int
	deletes int
}

func newMemAuthorRepo(did string) *memAuthorRepo {
	return &memAuthorRepo{did: did, records: map[string]*pds.RecordResponse{}}
}

func (r *memAuthorRepo) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	rec, ok := r.records[collection+"/"+rkey]
	if !ok {
		return nil, pds.ErrNotFound
	}
	return rec, nil
}

func (r *memAuthorRepo) PutRecordWithCommit(_ context.Context, collection, rkey string, _ any, _ string) (*pds.RecordCommit, error) {
	r.puts++
	uri := "at://" + r.did + "/" + collection + "/" + rkey
	r.records[collection+"/"+rkey] = &pds.RecordResponse{URI: uri, CID: "bafyreal" + rkey}
	return &pds.RecordCommit{URI: uri, CID: "bafyreal" + rkey}, nil
}

func (r *memAuthorRepo) DeleteRecord(context.Context, string, string) error { r.deletes++; return nil }
func (r *memAuthorRepo) UploadBlob(context.Context, []byte, string) (*blobs.BlobRef, error) {
	r.uploads++
	return &blobs.BlobRef{}, nil
}
func (r *memAuthorRepo) DID() string     { return r.did }
func (r *memAuthorRepo) HostURL() string { return "http://author-pds.invalid" }

type memCommunityRepo struct {
	did     string
	records map[string]*pds.RecordResponse
	puts    int
}

func (r *memCommunityRepo) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	rec, ok := r.records[collection+"/"+rkey]
	if !ok {
		return nil, pds.ErrNotFound
	}
	return rec, nil
}
func (r *memCommunityRepo) PutRecordWithCommit(context.Context, string, string, any, string) (*pds.RecordCommit, error) {
	r.puts++
	return &pds.RecordCommit{}, nil
}
func (r *memCommunityRepo) ApplyWrites(context.Context, []pds.Write, string) (*pds.ApplyWritesResult, error) {
	return nil, errors.New("unused")
}
func (r *memCommunityRepo) GetLatestCommit(context.Context) (*pds.LatestCommit, error) {
	return &pds.LatestCommit{}, nil
}
func (r *memCommunityRepo) DID() string     { return r.did }
func (r *memCommunityRepo) HostURL() string { return "http://community-pds.invalid" }

type memAcceptanceWriter struct {
	repo  *memCommunityRepo
	calls int
}

func (w *memAcceptanceWriter) WriteAcceptance(_ context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	w.calls++
	rkey := SubjectRkey(cmd.PostURI)
	w.repo.records[AcceptanceCollection+"/"+rkey] = &pds.RecordResponse{
		CID:   "bafyacceptreal",
		Value: map[string]any{"subject": map[string]any{"uri": cmd.PostURI, "cid": cmd.PostCID}},
	}
	return CommunityWriteResult{RKey: rkey, CID: "bafyacceptreal"}, nil
}
func (w *memAcceptanceWriter) WriteRemoval(context.Context, CommunityRemovalCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, errors.New("unused")
}
func (w *memAcceptanceWriter) RestoreAcceptance(context.Context, CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, errors.New("unused")
}
func (w *memAcceptanceWriter) RepinAcceptance(context.Context, CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, errors.New("unused")
}
func (w *memAcceptanceWriter) DeleteAcceptance(context.Context, CommunityAcceptanceDeleteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, errors.New("unused")
}

type memSource struct {
	posts   []LegacyPost
	reads   int
	deletes int
}

func (s *memSource) ListLegacyPosts(context.Context) ([]LegacyPost, error) { return s.posts, nil }
func (s *memSource) ReadLegacyPost(_ context.Context, uri string) (LegacyPost, bool, error) {
	s.reads++
	for _, p := range s.posts {
		if p.URI == uri {
			return p, true, nil
		}
	}
	return LegacyPost{}, false, nil
}
func (s *memSource) DeleteLegacyPost(_ context.Context, _ LegacyPost, swapCID string) error {
	if swapCID == "" {
		return errors.New("unguarded delete")
	}
	s.deletes++
	return nil
}

type memLedger struct {
	rows   map[string]RematerializeLedgerRow
	writes int
}

func newMemLedger() *memLedger { return &memLedger{rows: map[string]RematerializeLedgerRow{}} }

func (l *memLedger) Discover(_ context.Context, oldURI, communityDID, authorDID string) (RematerializeLedgerRow, error) {
	if row, ok := l.rows[oldURI]; ok {
		return row, nil
	}
	l.writes++
	row := RematerializeLedgerRow{
		OldURI: oldURI, State: RematerializeDiscovered,
		CommunityDID: communityDID, AuthorDID: authorDID,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	l.rows[oldURI] = row
	return row, nil
}
func (l *memLedger) Get(_ context.Context, oldURI string) (RematerializeLedgerRow, bool, error) {
	row, ok := l.rows[oldURI]
	return row, ok, nil
}
func (l *memLedger) ListResumable(context.Context, string) ([]RematerializeLedgerRow, error) {
	var out []RematerializeLedgerRow
	for _, row := range l.rows {
		if row.State != RematerializeDone && !IsFallback(row.State) {
			out = append(out, row)
		}
	}
	return out, nil
}
func (l *memLedger) RecordPostV2Written(_ context.Context, oldURI, sourceCID, newURI, newCID, newRkey string) error {
	l.writes++
	row := l.rows[oldURI]
	row.State = RematerializePostV2Written
	row.SourceCID, row.NewURI, row.NewCID, row.NewRkey = sourceCID, newURI, newCID, newRkey
	l.rows[oldURI] = row
	return nil
}
func (l *memLedger) advance(oldURI string, to RematerializeState) error {
	l.writes++
	row := l.rows[oldURI]
	row.State = to
	l.rows[oldURI] = row
	return nil
}
func (l *memLedger) MarkVerified(_ context.Context, uri string) error {
	return l.advance(uri, RematerializeVerified)
}
func (l *memLedger) MarkMigrated(_ context.Context, uri string) error {
	return l.advance(uri, RematerializeMigrated)
}
func (l *memLedger) MarkDone(_ context.Context, uri string) error {
	return l.advance(uri, RematerializeDone)
}
func (l *memLedger) MarkFallback(_ context.Context, uri string, state RematerializeState, _ string) error {
	return l.advance(uri, state)
}
func (l *memLedger) ReopenFallback(context.Context, string) (int, error) { return 0, nil }
func (l *memLedger) CountByState(context.Context, string) (map[RematerializeState]int, error) {
	counts := map[RematerializeState]int{}
	for _, row := range l.rows {
		counts[row.State]++
	}
	return counts, nil
}

type countingBlobClient struct {
	fetches  int
	probes   int
	bytesFor map[string][]byte
}

func (c *countingBlobClient) Fetch(_ context.Context, _, _, cid string) ([]byte, error) {
	c.fetches++
	data, ok := c.bytesFor[cid]
	if !ok {
		return nil, fmt.Errorf("no such blob %s", cid)
	}
	return data, nil
}
func (c *countingBlobClient) Present(context.Context, string, string, string) (bool, error) {
	c.probes++
	return true, nil
}

// dryRunFixture wires a complete, working tool over in-memory seams.
func dryRunFixture() (*Rematerializer, *memSource, *memLedger, *memAuthorRepo, *memAcceptanceWriter, *countingBlobClient) {
	communityDID := "did:plc:community2222222222222222"
	authorDID := "did:plc:author11111111111111111"
	blobCID := "bafkreiembeddedblobcid"

	legacy := LegacyPost{
		URI:          "at://" + communityDID + "/" + LegacyPostCollection + "/3kdryrun",
		CID:          "bafylegacycid",
		CommunityDID: communityDID,
		AuthorDID:    authorDID,
		RawRecord: map[string]any{
			"$type":     LegacyPostCollection,
			"community": communityDID,
			"author":    authorDID,
			"title":     "a post with media",
			"createdAt": "2026-01-02T03:04:05Z",
			"embed": map[string]any{
				"$type": "social.coves.embed.images",
				"images": []any{map[string]any{
					"alt":   "a picture",
					"image": map[string]any{"$type": "blob", "ref": map[string]any{"$link": blobCID}, "mimeType": "image/png"},
				}},
			},
		},
	}

	source := &memSource{posts: []LegacyPost{legacy}}
	ledger := newMemLedger()
	authorRepo := newMemAuthorRepo(authorDID)
	communityRepo := &memCommunityRepo{did: communityDID, records: map[string]*pds.RecordResponse{}}
	writer := &memAcceptanceWriter{repo: communityRepo}
	blobClient := &countingBlobClient{bytesFor: map[string][]byte{blobCID: []byte("PNGDATA")}}

	tool := &Rematerializer{
		Source:      source,
		Ledger:      ledger,
		AuthorRepos: func(context.Context, string, *oauth.ClientSessionData) (AuthorRepo, error) { return authorRepo, nil },
		Acceptances: writer,
		CommunityRepos: func(context.Context, string) (CommunityRepo, error) {
			return communityRepo, nil
		},
		Blobs: blobClient,
	}
	return tool, source, ledger, authorRepo, writer, blobClient
}

// The rehearsal must reach the SAME verdict the real run does, having really
// resolved credentials, really re-read the record, and really fetched the blob.
func TestDryRun_WalksTheWholeCodePathAndMutatesNothing(t *testing.T) {
	tool, source, ledger, authorRepo, writer, blobClient := dryRunFixture()

	dry := DryRunOf(tool)
	report, err := dry.Run(context.Background())
	require.NoErrorf(t, err, "the dry run failed; a rehearsal that cannot complete tells the operator nothing about the real run")

	// It got all the way to the end.
	assert.Equalf(t, 1, report.Done,
		"the rehearsal did not carry the record to done. A dry run that stops early cannot tell the operator whether the real run would succeed")
	assert.Truef(t, report.ScopeComplete, "the rehearsal must reach the same completion verdict the real run would")

	would, isDry := DryRunDeletes(dry)
	require.True(t, isDry)
	assert.Equalf(t, 1, would, "the rehearsal must report the delete it would have made; that count is the number the operator is being asked to authorise")

	// It really did the reads and the work.
	assert.Positivef(t, source.reads,
		"the rehearsal never RE-READ the legacy record. That read is where an edit landing after the listing is discovered, and rehearsing without it "+
			"means the dry run cannot find the exact problem a real run would hit")
	assert.Positivef(t, blobClient.fetches,
		"the rehearsal never FETCHED the embed blob. An unreachable or oversized blob is precisely the kind of failure a rehearsal exists to surface "+
			"before the destructive run")
	assert.Positivef(t, blobClient.probes, "the rehearsal must still run the blob presence check that gates the delete")

	// And it mutated nothing.
	assert.Zerof(t, authorRepo.puts,
		"the rehearsal WROTE %d record(s) into the author's repo", authorRepo.puts)
	assert.Zerof(t, authorRepo.uploads,
		"the rehearsal UPLOADED %d blob(s) into the author's repo", authorRepo.uploads)
	assert.Zerof(t, writer.calls,
		"the rehearsal wrote %d acceptance(s) into the community's repo", writer.calls)
	assert.Zerof(t, source.deletes,
		"THE REHEARSAL DELETED %d LEGACY RECORD(S). This is the failure this whole flag exists to make impossible", source.deletes)
	assert.Zerof(t, ledger.writes,
		"the rehearsal wrote %d row(s) to the real ledger; a dry run that leaves ledger state behind makes the next REAL run skip work it never did",
		ledger.writes)
}

// A rehearsal must surface a stranded author, because that is the single most
// consequential thing an operator learns before committing.
func TestDryRun_StillFindsAStrandedAuthor(t *testing.T) {
	tool, source, _, _, writer, _ := dryRunFixture()
	tool.AuthorRepos = func(_ context.Context, did string, _ *oauth.ClientSessionData) (AuthorRepo, error) {
		return nil, fmt.Errorf("no stored session for %s: %w", did, ErrNoAuthorCredentials)
	}

	report, err := DryRunOf(tool).Run(context.Background())
	require.NoError(t, err)

	assert.Equalf(t, 1, report.Fallbacks,
		"the rehearsal did not report the stranded author. Discovering at rehearsal time that an author's grant is gone is the difference between "+
			"re-authorizing them and finding out afterwards that their posts were left behind")
	assert.Falsef(t, report.Complete, "a rehearsal with a stranded post must not report the migration complete")
	assert.Zerof(t, source.deletes, "nothing may be deleted in a rehearsal")
	assert.Zerof(t, writer.calls, "nothing may be written in a rehearsal")
}

// A rehearsal must surface a RETRYABLE credential failure as a failed run, for
// the same reason the real one does.
func TestDryRun_StillFailsOnARetryableCredentialError(t *testing.T) {
	tool, _, _, _, _, _ := dryRunFixture()
	tool.AuthorRepos = func(_ context.Context, did string, _ *oauth.ClientSessionData) (AuthorRepo, error) {
		return nil, fmt.Errorf("resuming %s: %w: connection refused", did, ErrAuthorCredentialsUnavailable)
	}

	_, err := DryRunOf(tool).Run(context.Background())
	require.Errorf(t, err,
		"the rehearsal swallowed a retryable credential failure. The operator would then confirm -yes on a rehearsal that had quietly skipped the "+
			"very authors the real run is about to fail on")
}

// A rehearsal is not a real run, and DryRunDeletes must say so for one.
func TestDryRunDeletes_ReportsNothingForARealRun(t *testing.T) {
	tool, _, _, _, _, _ := dryRunFixture()
	_, isDry := DryRunDeletes(tool)
	assert.Falsef(t, isDry,
		"a REAL run reported itself as a dry run; the operator's console would then say 'nothing was written' after the records were deleted")
}
