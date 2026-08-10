package posts

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
)

// DRY RUN: the same code path, with only the MUTATIONS replaced.
//
// # WHY THIS IS SEAM WRAPPERS AND NOT AN `if dryRun` BRANCH
//
// A dry run whose value is a flag consulted inside the state machine tests a
// DIFFERENT program than the real one: every branch is a place the two can
// diverge, and the divergences accumulate in exactly the code the operator is
// trying to rehearse. So the state machine has no idea it is rehearsing. Every
// seam is wrapped instead, and a wrapped seam either
//
//   - performs the real operation, when it is a READ (the source listing, the
//     legacy re-read, credential resolution, blob fetches, record reads), or
//   - records the intended mutation in memory and answers subsequent reads from
//     that memory, when it is a WRITE.
//
// The consequence is that a dry run really does: resolve every author's
// credentials (and so really does discover a missing grant), re-read every
// legacy record from the PDS (and so really does discover an edit that landed),
// build every postv2 body losslessly, derive every rkey, enumerate and FETCH
// every embed blob (and so really does discover an unreachable or oversized
// one), and run the whole pre-delete verification against the records it would
// have written. What it never does is put a byte in a repo or take one out.
//
// # WHAT IT CANNOT PROVE
//
// The CIDs are synthesised from the record bodies rather than minted by a PDS,
// so a dry run cannot prove that the PDS accepts a record it will validate, and
// it cannot prove a blob upload succeeds. It proves the tool's decisions, its
// scope, its conversions and its reachability — which is what an operator is
// asking about at 2am.

// DryRunOf returns a Rematerializer that walks the same code path as tool but
// mutates nothing.
//
// The returned tool shares no state with the original: cancel it, discard it,
// run it twice. Its ledger writes, repo writes and acceptance writes live in an
// in-memory overlay that is thrown away with it.
func DryRunOf(tool *Rematerializer) *Rematerializer {
	overlay := &dryRunOverlay{
		ledgerRows: map[string]RematerializeLedgerRow{},
		records:    map[string]*pds.RecordResponse{},
		blobs:      map[string]bool{},
	}

	dry := &Rematerializer{
		Source:           &dryRunSource{inner: tool.Source, overlay: overlay},
		Ledger:           &dryRunLedger{inner: tool.Ledger, overlay: overlay},
		AuthorRepos:      dryRunAuthorRepos(tool.AuthorRepos, overlay),
		Acceptances:      &dryRunAcceptanceWriter{overlay: overlay},
		CommunityRepos:   dryRunCommunityRepos(tool.CommunityRepos, overlay),
		Blobs:            &dryRunBlobClient{inner: tool.blobClient(), overlay: overlay},
		CommunityScope:   tool.CommunityScope,
		Progress:         tool.Progress,
		PerRecordTimeout: tool.PerRecordTimeout,
		AbortOnFallback:  tool.AbortOnFallback,
	}
	return dry
}

// DryRunDeletes reports how many legacy records the tool WOULD have deleted.
// It returns 0 and false for a Rematerializer that is not a dry run.
func DryRunDeletes(tool *Rematerializer) (int, bool) {
	source, ok := tool.Source.(*dryRunSource)
	if !ok {
		return 0, false
	}
	source.overlay.mu.Lock()
	defer source.overlay.mu.Unlock()
	return source.overlay.deletes, true
}

// dryRunOverlay is the in-memory store every wrapped write lands in and every
// wrapped read falls back to. One overlay per dry run.
type dryRunOverlay struct {
	mu             sync.Mutex
	ledgerRows     map[string]RematerializeLedgerRow
	ledgerOriginal map[string]RematerializeState // the state each row stood at on disk
	deletedURIs    map[string]bool
	records        map[string]*pds.RecordResponse // "did/collection/rkey" -> record
	blobs          map[string]bool                // content-keyed marks for the rehearsal
	deletes        int
}

func (o *dryRunOverlay) recordKey(did, collection, rkey string) string {
	return did + "/" + collection + "/" + rkey
}

func (o *dryRunOverlay) putRecord(did, collection, rkey string, value map[string]any) *pds.RecordResponse {
	o.mu.Lock()
	defer o.mu.Unlock()
	rec := &pds.RecordResponse{
		URI:   "at://" + did + "/" + collection + "/" + rkey,
		CID:   dryRunCID(value),
		Value: value,
	}
	o.records[o.recordKey(did, collection, rkey)] = rec
	return rec
}

func (o *dryRunOverlay) getRecord(did, collection, rkey string) (*pds.RecordResponse, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	rec, ok := o.records[o.recordKey(did, collection, rkey)]
	return rec, ok
}

// dryRunCID synthesises a stable, obviously-fake CID from a record body.
//
// It is derived from the canonical bytes so that the tool's own CID comparisons
// — the converged-write body check, the postv2 re-read, the acceptance's pinned
// subject — mean the same thing in a rehearsal as they do in production. The
// "bafyDRYRUN" prefix is deliberate: a synthetic CID must never be mistaken for
// one a PDS minted if it escapes into a log.
func dryRunCID(value map[string]any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(fmt.Sprintf("%v", value))
	}
	digest := sha256.Sum256(raw)
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:16]))
	return "bafyDRYRUN" + encoded
}

// ---- source ---------------------------------------------------------------

// dryRunSource performs every READ for real — the listing and the pre-delete
// re-read both go to the PDS — and counts the deletes it did not make.
type dryRunSource struct {
	inner   LegacySource
	overlay *dryRunOverlay
}

// ListLegacyPosts lists for real, minus the records the rehearsal has already
// "deleted". Without that subtraction the final re-scan would always see every
// record still standing, and a rehearsal of a run that WOULD complete would
// report itself incomplete — which is the one number the operator reads.
func (s *dryRunSource) ListLegacyPosts(ctx context.Context) ([]LegacyPost, error) {
	all, err := s.inner.ListLegacyPosts(ctx)
	if err != nil {
		return nil, err
	}
	s.overlay.mu.Lock()
	defer s.overlay.mu.Unlock()
	if len(s.overlay.deletedURIs) == 0 {
		return all, nil
	}
	out := make([]LegacyPost, 0, len(all))
	for _, p := range all {
		if s.overlay.deletedURIs[p.URI] {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *dryRunSource) ReadLegacyPost(ctx context.Context, uri string) (LegacyPost, bool, error) {
	s.overlay.mu.Lock()
	deleted := s.overlay.deletedURIs[uri]
	s.overlay.mu.Unlock()
	if deleted {
		// A record this run has "deleted" must read as gone, or the reconcile pass
		// would rehearse a second delete of it.
		return LegacyPost{}, false, nil
	}
	return s.inner.ReadLegacyPost(ctx, uri)
}

func (s *dryRunSource) DeleteLegacyPost(_ context.Context, legacy LegacyPost, swapCID string) error {
	if swapCID == "" {
		// The rehearsal enforces the same precondition the real source does, so a
		// dry run catches a missing guard rather than masking it.
		return fmt.Errorf("dry run: refusing to rehearse an unguarded delete of %s", legacy.URI)
	}
	s.overlay.mu.Lock()
	defer s.overlay.mu.Unlock()
	s.overlay.deletes++
	if s.overlay.deletedURIs == nil {
		s.overlay.deletedURIs = map[string]bool{}
	}
	s.overlay.deletedURIs[legacy.URI] = true
	return nil
}

// ---- ledger ---------------------------------------------------------------

// dryRunLedger reads through to the real ledger — so a rehearsal resumes from
// the same place a real run would — and holds every write in the overlay.
type dryRunLedger struct {
	inner   RematerializeLedger
	overlay *dryRunOverlay
}

func (l *dryRunLedger) Discover(ctx context.Context, oldURI, communityDID, authorDID string) (RematerializeLedgerRow, error) {
	if row, ok := l.overlayRow(oldURI); ok {
		return row, nil
	}
	row, found, err := l.inner.Get(ctx, oldURI)
	if err != nil {
		return RematerializeLedgerRow{}, err
	}
	if !found {
		row = RematerializeLedgerRow{
			OldURI: oldURI, State: RematerializeDiscovered,
			CommunityDID: communityDID, AuthorDID: authorDID,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	l.setOverlayRow(row)
	return row, nil
}

func (l *dryRunLedger) Get(ctx context.Context, oldURI string) (RematerializeLedgerRow, bool, error) {
	if row, ok := l.overlayRow(oldURI); ok {
		return row, true, nil
	}
	return l.inner.Get(ctx, oldURI)
}

func (l *dryRunLedger) ListResumable(ctx context.Context, communityDID string) ([]RematerializeLedgerRow, error) {
	rows, err := l.inner.ListResumable(ctx, communityDID)
	if err != nil {
		return nil, err
	}
	out := make([]RematerializeLedgerRow, 0, len(rows))
	for _, row := range rows {
		if overlaid, ok := l.overlayRow(row.OldURI); ok {
			row = overlaid
		}
		if row.State == RematerializeDone || IsFallback(row.State) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (l *dryRunLedger) RecordPostV2Written(_ context.Context, oldURI, sourceCID, newURI, newCID, newRkey string) error {
	return l.advance(oldURI, RematerializeDiscovered, func(row *RematerializeLedgerRow) {
		row.State = RematerializePostV2Written
		row.SourceCID, row.NewURI, row.NewCID, row.NewRkey = sourceCID, newURI, newCID, newRkey
	})
}

func (l *dryRunLedger) MarkVerified(_ context.Context, oldURI string) error {
	return l.advance(oldURI, RematerializePostV2Written, func(row *RematerializeLedgerRow) {
		row.State = RematerializeVerified
	})
}

func (l *dryRunLedger) MarkMigrated(_ context.Context, oldURI string) error {
	return l.advance(oldURI, RematerializeVerified, func(row *RematerializeLedgerRow) {
		row.State = RematerializeMigrated
	})
}

func (l *dryRunLedger) MarkDone(_ context.Context, oldURI string) error {
	return l.advance(oldURI, RematerializeMigrated, func(row *RematerializeLedgerRow) {
		row.State = RematerializeDone
	})
}

func (l *dryRunLedger) MarkFallback(_ context.Context, oldURI string, state RematerializeState, reason string) error {
	if !IsFallback(state) {
		return fmt.Errorf("dry run: %q is not a fallback state", state)
	}
	return l.advance(oldURI, RematerializeDiscovered, func(row *RematerializeLedgerRow) {
		row.State = state
		row.Reason = reason
	})
}

// ReopenFallback is a no-op in a rehearsal: it changes no repo, so there is
// nothing to rehearse, and applying it would make the run report progress the
// operator has not authorised.
func (l *dryRunLedger) ReopenFallback(context.Context, string) (int, error) { return 0, nil }

func (l *dryRunLedger) CountByState(ctx context.Context, communityDID string) (map[RematerializeState]int, error) {
	counts, err := l.inner.CountByState(ctx, communityDID)
	if err != nil {
		return nil, err
	}
	if counts == nil {
		counts = map[RematerializeState]int{}
	}
	// Re-tally: an overlaid row's real state is the one on disk, and the rehearsal
	// moved it somewhere else. Move the count with it.
	l.overlay.mu.Lock()
	defer l.overlay.mu.Unlock()
	for _, row := range l.overlay.ledgerRows {
		if communityDID != "" && row.CommunityDID != communityDID {
			continue
		}
		if before, ok := l.overlay.ledgerOriginal[row.OldURI]; ok {
			if counts[before] > 0 {
				counts[before]--
				if counts[before] == 0 {
					delete(counts, before)
				}
			}
		}
		counts[row.State]++
	}
	return counts, nil
}

func (l *dryRunLedger) overlayRow(oldURI string) (RematerializeLedgerRow, bool) {
	l.overlay.mu.Lock()
	defer l.overlay.mu.Unlock()
	row, ok := l.overlay.ledgerRows[oldURI]
	return row, ok
}

func (l *dryRunLedger) setOverlayRow(row RematerializeLedgerRow) {
	l.overlay.mu.Lock()
	defer l.overlay.mu.Unlock()
	if _, seen := l.overlay.ledgerOriginal[row.OldURI]; !seen {
		if l.overlay.ledgerOriginal == nil {
			l.overlay.ledgerOriginal = map[string]RematerializeState{}
		}
		l.overlay.ledgerOriginal[row.OldURI] = row.State
	}
	l.overlay.ledgerRows[row.OldURI] = row
}

// advance applies a guarded transition to the overlay, enforcing the SAME
// from-state guard the real ledger does, so a rehearsal surfaces a divergence
// rather than hiding it.
func (l *dryRunLedger) advance(oldURI string, from RematerializeState, mutate func(*RematerializeLedgerRow)) error {
	l.overlay.mu.Lock()
	row, ok := l.overlay.ledgerRows[oldURI]
	l.overlay.mu.Unlock()
	if !ok {
		return fmt.Errorf("dry run: no ledger row for %s", oldURI)
	}
	if row.State != from {
		return fmt.Errorf("dry run: transitioning %s from %s: the row stands at %s (the ledger and the tool have diverged)", oldURI, from, row.State)
	}
	mutate(&row)
	row.UpdatedAt = time.Now()
	l.setOverlayRow(row)
	return nil
}

// ---- author repos ---------------------------------------------------------

// dryRunAuthorRepos resolves credentials FOR REAL — that is most of what a
// rehearsal is for — and wraps the resulting repo so its writes land in memory.
func dryRunAuthorRepos(inner AuthorRepoFactory, overlay *dryRunOverlay) AuthorRepoFactory {
	return func(ctx context.Context, authorDID string, session *oauth.ClientSessionData) (AuthorRepo, error) {
		repo, err := inner(ctx, authorDID, session)
		if err != nil {
			return nil, err
		}
		return &dryRunAuthorRepo{inner: repo, overlay: overlay}, nil
	}
}

type dryRunAuthorRepo struct {
	inner   AuthorRepo
	overlay *dryRunOverlay
}

func (r *dryRunAuthorRepo) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	if rec, ok := r.overlay.getRecord(r.inner.DID(), collection, rkey); ok {
		return rec, nil
	}
	return r.inner.GetRecord(ctx, collection, rkey)
}

func (r *dryRunAuthorRepo) PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	// The create-only guard is rehearsed against the REAL repo first: a record
	// that already stands must still produce the swap conflict that drives
	// createAuthorRecord's converge-by-read, or the rehearsal would not exercise
	// the branch a re-run actually takes.
	if swapRecord == "" {
		if _, err := r.GetRecord(ctx, collection, rkey); err == nil {
			return nil, pds.ErrSwapConflict
		}
	}
	var body map[string]any
	if raw, err := json.Marshal(record); err == nil {
		_ = json.Unmarshal(raw, &body)
	}
	rec := r.overlay.putRecord(r.inner.DID(), collection, rkey, body)
	return &pds.RecordCommit{URI: rec.URI, CID: rec.CID, CommitRev: "dryrun"}, nil
}

// DeleteRecord is a no-op: a rehearsal removes nothing.
func (r *dryRunAuthorRepo) DeleteRecord(context.Context, string, string) error { return nil }

// UploadBlob records that the bytes WOULD have been uploaded, and reports the
// community's own CID back — which is what a content-addressed store returns for
// identical bytes, and what the caller's equality check is written against.
func (r *dryRunAuthorRepo) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	r.overlay.mu.Lock()
	defer r.overlay.mu.Unlock()
	r.overlay.blobs[r.inner.DID()+"/"+dryRunBlobKey(data)] = true
	return &blobs.BlobRef{Type: "blob", MimeType: mimeType, Size: len(data)}, nil
}

func (r *dryRunAuthorRepo) DID() string { return r.inner.DID() }

// HostURL forwards the wrapped repo's host so blob probing addresses the same
// PDS a real run would.
func (r *dryRunAuthorRepo) HostURL() string {
	host, _ := hostURLOf(r.inner)
	return host
}

// dryRunBlobKey keys the overlay's uploaded-blob set by content, so the presence
// probe can answer for bytes this run fetched and would have uploaded.
func dryRunBlobKey(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:16])
}

// ---- acceptances ----------------------------------------------------------

// dryRunAcceptanceWriter records the acceptance it would have written and serves
// it back to the verification read, so the whole "read the acceptance back and
// check its subject" leg really runs.
type dryRunAcceptanceWriter struct {
	overlay *dryRunOverlay
}

func (w *dryRunAcceptanceWriter) WriteAcceptance(_ context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	rkey := SubjectRkey(cmd.PostURI)
	rec := w.overlay.putRecord(cmd.CommunityDID, AcceptanceCollection, rkey, map[string]any{
		"$type":     AcceptanceCollection,
		"subject":   map[string]any{"uri": cmd.PostURI, "cid": cmd.PostCID},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})
	return CommunityWriteResult{URI: rec.URI, RKey: rkey, CID: rec.CID, Rev: "dryrun"}, nil
}

// The moderation writes are unreachable from the tool by construction; a
// rehearsal that somehow reached one must say so rather than pretend.
func (w *dryRunAcceptanceWriter) WriteRemoval(context.Context, CommunityRemovalCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, fmt.Errorf("dry run: the re-materialization tool must never write a removal")
}
func (w *dryRunAcceptanceWriter) RestoreAcceptance(context.Context, CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, fmt.Errorf("dry run: the re-materialization tool must never restore an acceptance")
}
func (w *dryRunAcceptanceWriter) RepinAcceptance(context.Context, CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, fmt.Errorf("dry run: the re-materialization tool must never repin an acceptance")
}
func (w *dryRunAcceptanceWriter) DeleteAcceptance(context.Context, CommunityAcceptanceDeleteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, fmt.Errorf("dry run: the re-materialization tool must never delete an acceptance")
}

// ---- community repos ------------------------------------------------------

// dryRunCommunityRepos opens the REAL community repo — the tool needs its host
// URL to fetch blobs and its records to fall back on — and overlays the
// acceptance the rehearsal would have written.
func dryRunCommunityRepos(inner CommunityRepoFactory, overlay *dryRunOverlay) CommunityRepoFactory {
	if inner == nil {
		return nil
	}
	return func(ctx context.Context, communityDID string) (CommunityRepo, error) {
		repo, err := inner(ctx, communityDID)
		if err != nil {
			return nil, err
		}
		return &dryRunCommunityRepo{inner: repo, overlay: overlay}, nil
	}
}

type dryRunCommunityRepo struct {
	inner   CommunityRepo
	overlay *dryRunOverlay
}

func (r *dryRunCommunityRepo) GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	if rec, ok := r.overlay.getRecord(r.inner.DID(), collection, rkey); ok {
		return rec, nil
	}
	return r.inner.GetRecord(ctx, collection, rkey)
}

func (r *dryRunCommunityRepo) PutRecordWithCommit(_ context.Context, collection, rkey string, record any, _ string) (*pds.RecordCommit, error) {
	var body map[string]any
	if raw, err := json.Marshal(record); err == nil {
		_ = json.Unmarshal(raw, &body)
	}
	rec := r.overlay.putRecord(r.inner.DID(), collection, rkey, body)
	return &pds.RecordCommit{URI: rec.URI, CID: rec.CID, CommitRev: "dryrun"}, nil
}

func (r *dryRunCommunityRepo) ApplyWrites(context.Context, []pds.Write, string) (*pds.ApplyWritesResult, error) {
	return nil, fmt.Errorf("dry run: the re-materialization tool must never batch community writes")
}

func (r *dryRunCommunityRepo) GetLatestCommit(ctx context.Context) (*pds.LatestCommit, error) {
	return r.inner.GetLatestCommit(ctx)
}

func (r *dryRunCommunityRepo) DID() string { return r.inner.DID() }

func (r *dryRunCommunityRepo) HostURL() string {
	host, _ := hostURLOf(r.inner)
	return host
}

// ---- blobs ----------------------------------------------------------------

// dryRunBlobClient FETCHES FOR REAL — an unreachable or oversized blob is
// exactly the kind of thing a rehearsal exists to find — and answers the
// presence probe for bytes this run fetched, since it never uploaded them.
type dryRunBlobClient struct {
	inner   RematerializeBlobClient
	overlay *dryRunOverlay
}

func (c *dryRunBlobClient) Fetch(ctx context.Context, host, did, cid string) ([]byte, error) {
	data, err := c.inner.Fetch(ctx, host, did, cid)
	if err != nil {
		return nil, err
	}
	c.overlay.mu.Lock()
	c.overlay.blobs["fetched/"+dryRunBlobKey(data)] = true
	c.overlay.blobs["cid/"+cid] = true
	c.overlay.mu.Unlock()
	return data, nil
}

// Present PROBES FOR REAL — a host that cannot be reached at all is something the
// rehearsal should surface — and then answers for bytes this run fetched, since
// it never uploaded them.
//
// The real probe's ANSWER is deliberately not the rehearsal's answer when the
// bytes were fetched: the blob is legitimately absent from the author's repo
// because nothing was uploaded, and reporting that absence would stop every
// rehearsal at the first post carrying media. Its ERROR is not swallowed either
// — an unreachable host is a finding.
func (c *dryRunBlobClient) Present(ctx context.Context, host, did, cid string) (bool, error) {
	present, err := c.inner.Present(ctx, host, did, cid)

	c.overlay.mu.Lock()
	fetched := c.overlay.blobs["cid/"+cid]
	c.overlay.mu.Unlock()
	if fetched {
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return present, err
}
