package posts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
)

// The community-record writers at T0: everything about their behaviour that a
// fake repo can prove — local validation, call ordering, swap-guard plumbing,
// and the repin's refusal to invent an acceptance. The outer contract against a
// real PDS is engine_contract_test.go.

const (
	writerCommunityDID = "did:plc:cccccccccccccccccccccccc"
	writerPostURI      = "at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/social.coves.community.postv2/3kjzl5kcb2s2v"
	writerPostCID      = "bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// writerClock is the fixed time every T0 writer test stamps records with.
func writerClock() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }

// refusingFactory fails the test if the writer reaches for a repo at all — the
// assertion behind "violations are refused before any network call".
func refusingFactory(t *testing.T) CommunityRepoFactory {
	t.Helper()
	return func(_ context.Context, communityDID string) (CommunityRepo, error) {
		t.Errorf("the writer opened the repo of %s; a locally invalid command must be refused "+
			"before any network call", communityDID)
		return nil, errors.New("must not be reached")
	}
}

// ---------------------------------------------------------------------------
// Local validation (§3.2/§3.3): records are sent with validate:false, so the
// PDS checks nothing — what this process sends is what the firehose carries.
// ---------------------------------------------------------------------------

func TestWriter_RefusesAMalformedSubjectURIBeforeAnyNetworkCall(t *testing.T) {
	t.Parallel()

	// Every record these writers produce embeds the subject as a strongRef,
	// and the put goes out with validate:false — the PDS has never been taught
	// Coves lexicons, so nothing downstream re-checks the shape. A subject
	// that is not an at:// URI is therefore a programming error at THIS
	// boundary: let it through and the malformed strongRef is published to the
	// firehose under the community's signature.
	for name, uri := range map[string]string{
		"https scheme":    "https://example.com/not-a-post",
		"bare identifier": "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa",
		"unparseable":     "at://",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := NewCommunityRecordWriter(refusingFactory(t), writerClock)
			ctx := context.Background()

			writeCmd := CommunityWriteCommand{
				CommunityDID: writerCommunityDID,
				PostURI:      uri,
				PostCID:      writerPostCID,
			}
			removalCmd := CommunityRemovalCommand{
				CommunityDID: writerCommunityDID,
				PostURI:      uri,
				PostCID:      writerPostCID,
				Code:         DecisionSpam,
			}

			_, err := writer.WriteAcceptance(ctx, writeCmd)
			require.Errorf(t, err, "WriteAcceptance accepted subject URI %q", uri)
			_, err = writer.RepinAcceptance(ctx, writeCmd)
			require.Errorf(t, err, "RepinAcceptance accepted subject URI %q", uri)
			_, err = writer.RestoreAcceptance(ctx, writeCmd)
			require.Errorf(t, err, "RestoreAcceptance accepted subject URI %q", uri)
			_, err = writer.WriteRemoval(ctx, removalCmd)
			require.Errorf(t, err, "WriteRemoval accepted subject URI %q", uri)
		})
	}
}

func TestWriter_RefusesARemovalCodeOverTheLexiconMaxLength(t *testing.T) {
	t.Parallel()

	// The removal lexicon caps `code` at 64 bytes (maxLength). validate:false
	// means the PDS will happily commit a longer one — and every conformant
	// consumer on the network is then entitled to refuse the record this
	// community signed. A code the engine minted that long is a programming
	// error, caught here rather than published.
	writer := NewCommunityRecordWriter(refusingFactory(t), writerClock)

	_, err := writer.WriteRemoval(context.Background(), CommunityRemovalCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
		Code:         DecisionCode(strings.Repeat("x", 65)),
	})
	require.Error(t, err, "a 65-byte code exceeds the lexicon's maxLength of 64 and must be refused")

	// The boundary itself is legal: 64 bytes is the maxLength, not one past it.
	repo := newFakeCommunityRepo(writerCommunityDID)
	boundedWriter := NewCommunityRecordWriter(fixedFactory(repo), writerClock)
	_, err = boundedWriter.WriteRemoval(context.Background(), CommunityRemovalCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
		Code:         DecisionCode(strings.Repeat("x", 64)),
	})
	assert.NoError(t, err, "a 64-byte code is exactly the lexicon's maxLength and must pass")
}

// ---------------------------------------------------------------------------
// Swap-retry backoff
// ---------------------------------------------------------------------------

func TestWriter_BacksOffWithJitterBetweenSwapRetries(t *testing.T) {
	t.Parallel()

	// The writers that lose a swap to each other are the three acceptance
	// writers of §3.2 converging on the same rkey, so a retry fired
	// immediately — or after a FIXED interval — collides again on schedule.
	// Each retry therefore waits a jittered duration bounded by a doubling
	// ceiling. (Actually serializing the contenders is the per-community
	// queue, which is task 5's job; this only decorrelates the collisions.)
	//
	// The sleeper is injected and recorded: docs/TEST_ARCHITECTURE.md §3.3
	// forbids a test from actually sleeping, so the assertion is on the
	// durations, not the wall clock.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.putErrs = []error{pds.ErrSwapConflict, pds.ErrSwapConflict, nil}

	var pauses []time.Duration
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(_ context.Context, d time.Duration) error {
			pauses = append(pauses, d)
			return nil
		}))

	result, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.NoError(t, err, "two lost swaps are within the retry budget and must converge")
	assert.False(t, result.Skipped)

	require.Lenf(t, pauses, 2, "one pause per retry: two conflicts, two pauses")
	for i, pause := range pauses {
		ceiling := 25 * time.Millisecond << i
		assert.Positivef(t, pause, "pause %d must be positive — a zero pause is no backoff at all", i)
		assert.LessOrEqualf(t, pause, ceiling,
			"pause %d must stay under its doubling ceiling %v", i, ceiling)
	}
}

func TestWriter_ACancelledBackoffAbortsTheRetry(t *testing.T) {
	t.Parallel()

	// A worker shutting down mid-backoff must not fire another write: the
	// sleeper reports the cancellation and the writer surfaces it instead of
	// retrying into a dying process.
	repo := newFakeCommunityRepo(writerCommunityDID)
	repo.putErrs = []error{pds.ErrSwapConflict}

	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock,
		WithSwapRetrySleeper(func(_ context.Context, _ time.Duration) error {
			return context.Canceled
		}))

	_, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Lenf(t, repo.puts, 1, "the cancelled backoff must prevent the retry's put")
}

// ---------------------------------------------------------------------------
// Call ordering and the swapCommit guard
// ---------------------------------------------------------------------------

func TestWriter_RemovalCommitReadsTheHeadBeforeTheRecordsAndGuardsWithIt(t *testing.T) {
	t.Parallel()

	// THE ORDER IS THE GUARD. A swapCommit read AFTER the records could be
	// newer than the state the batch was shaped from — it would guard the
	// commit against a revision that already contains the change the shape
	// assumed absent. Read first, any interleaved write makes the guard stale:
	// a detected conflict rather than a silent clobber. No outcome value
	// reveals this order, so the fake records it.
	repo := newFakeCommunityRepo(writerCommunityDID)
	rkey := SubjectRkey(writerPostURI)
	repo.setRecord(AcceptanceCollection, rkey, "bafyreiacceptanceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", map[string]any{
		"$type":     AcceptanceCollection,
		"subject":   map[string]any{"uri": writerPostURI, "cid": writerPostCID},
		"createdAt": "2026-06-30T09:00:00Z",
	})

	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock)
	result, err := writer.WriteRemoval(context.Background(), CommunityRemovalCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
		Code:         DecisionSpam,
	})
	require.NoError(t, err)
	assert.False(t, result.Skipped)

	assert.Equal(t, []string{
		"GetLatestCommit",
		"GetRecord:" + RemovalCollection,
		"GetRecord:" + AcceptanceCollection,
		"ApplyWrites",
	}, repo.calls, "the head must be read BEFORE the record pre-reads the batch is shaped from")

	require.Len(t, repo.batches, 1)
	assert.Equalf(t, repo.head.CID, repo.batches[0].swapCommit,
		"the head CID that was read is the swapCommit the batch must be guarded by")
}

// ---------------------------------------------------------------------------
// RepinAcceptance
// ---------------------------------------------------------------------------

func TestWriter_RepinUpdatesTheStandingAcceptanceInPlace(t *testing.T) {
	t.Parallel()

	// The bridgedStats exception of §5.5: the acceptance moves onto the new
	// content CID as an UPDATE of the same record — guarded by the standing
	// record's CID, carrying its createdAt forward — so the acceptance's
	// createdAt keeps meaning "when this community accepted this post" and the
	// record's identity survives every stats refresh.
	repo := newFakeCommunityRepo(writerCommunityDID)
	rkey := SubjectRkey(writerPostURI)
	standingCID := "bafyreiacceptanceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repo.setRecord(AcceptanceCollection, rkey, standingCID, map[string]any{
		"$type":     AcceptanceCollection,
		"subject":   map[string]any{"uri": writerPostURI, "cid": writerPostCID},
		"createdAt": "2026-06-30T09:00:00Z",
	})

	refreshedCID := "bafyreirefreshedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock)
	result, err := writer.RepinAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      refreshedCID,
	})
	require.NoError(t, err)
	assert.False(t, result.Skipped, "the standing acceptance pins the old CID, so there was work to do")
	assert.NotEmpty(t, result.Rev)

	require.Len(t, repo.puts, 1)
	put := repo.puts[0]
	assert.Equal(t, rkey, put.rkey, "the repin must reuse the subject's deterministic rkey")
	assert.Equalf(t, standingCID, put.swapRecord,
		"the update must be guarded by the record CID the pre-read found — an unguarded put would "+
			"clobber a concurrent writer in exactly the window the pre-read opened")
	subject, _ := put.record["subject"].(map[string]any)
	assert.Equal(t, refreshedCID, subject["cid"])
	assert.Equalf(t, "2026-06-30T09:00:00Z", put.record["createdAt"],
		"createdAt must be carried forward — restamping it would rewrite when the community "+
			"accepted the post, on every stats refresh")
}

func TestWriter_RepinRefusesWhenNoAcceptanceStands(t *testing.T) {
	t.Parallel()

	// A repin moves a STANDING acceptance and re-decides nothing, so it has no
	// authority to create one. An absent record means the AppView's row and
	// the community's repo disagree, and silently minting an acceptance nobody
	// decided is the one thing a path that skips admission must never do. The
	// documented outcome is pds.ErrNotFound, which the caller defers on.
	repo := newFakeCommunityRepo(writerCommunityDID)
	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock)

	_, err := writer.RepinAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, pds.ErrNotFound)
	assert.Emptyf(t, repo.puts, "nothing may be written: a repin never creates")
	assert.Emptyf(t, repo.batches, "nothing may be committed")
}

func TestWriter_AcceptanceRefusesAStandingRemoval(t *testing.T) {
	t.Parallel()

	// The T0 face of the §5.5 removal guard (the real-PDS races are
	// engine_contract_test.go): a removal standing at the subject's rkey
	// refuses both the write and the repin before anything is put.
	repo := newFakeCommunityRepo(writerCommunityDID)
	rkey := SubjectRkey(writerPostURI)
	repo.setRecord(RemovalCollection, rkey, "bafyreiremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", map[string]any{
		"$type":     RemovalCollection,
		"subject":   map[string]any{"uri": writerPostURI, "cid": writerPostCID},
		"code":      string(DecisionSpam),
		"createdAt": "2026-06-30T09:00:00Z",
	})

	writer := NewCommunityRecordWriter(fixedFactory(repo), writerClock)
	cmd := CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	}

	_, err := writer.WriteAcceptance(context.Background(), cmd)
	require.ErrorIs(t, err, ErrRemovalStands)
	_, err = writer.RepinAcceptance(context.Background(), cmd)
	require.ErrorIs(t, err, ErrRemovalStands)

	assert.Emptyf(t, repo.puts, "an acceptance over a standing removal must never reach the repo")
}

// ---------------------------------------------------------------------------
// The factory DID check
// ---------------------------------------------------------------------------

func TestWriter_RefusesAFactoryThatReturnsTheWrongRepo(t *testing.T) {
	t.Parallel()

	// The repo's DID is the AUTHORITY half of every record URI this writer
	// produces. A factory bug that handed back another community's session
	// would have one community vouching for a post with another community's
	// key — an acceptance that looks perfectly valid to every consumer on the
	// network. openRepo therefore proves the session is on the DID that was
	// asked for, and refuses before anything is read or written.
	wrongRepo := newFakeCommunityRepo("did:plc:dddddddddddddddddddddddd")
	writer := NewCommunityRecordWriter(fixedFactory(wrongRepo), writerClock)

	_, err := writer.WriteAcceptance(context.Background(), CommunityWriteCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did:plc:dddddddddddddddddddddddd",
		"the refusal must name the DID the factory actually returned")

	_, err = writer.WriteRemoval(context.Background(), CommunityRemovalCommand{
		CommunityDID: writerCommunityDID,
		PostURI:      writerPostURI,
		PostCID:      writerPostCID,
		Code:         DecisionSpam,
	})
	require.Error(t, err)

	// Refused up front: the wrong repo is never read, let alone written.
	assert.Emptyf(t, wrongRepo.calls,
		"openRepo must refuse before touching the repo at all; calls: %v", wrongRepo.calls)
}

// ---------------------------------------------------------------------------
// The fake repo
// ---------------------------------------------------------------------------

// fakeCommunityRepo is an in-memory CommunityRepo that records the order of
// every call, so a test can pin sequencing that no outcome value reveals.
type fakeCommunityRepo struct {
	did   string
	calls []string

	// records is keyed by collection+"/"+rkey.
	records map[string]*pds.RecordResponse

	head pds.LatestCommit

	// putErrs and applyErrs are consumed one per call, so a test can fail the
	// first attempt and let the retry through. A nil entry means success.
	putErrs   []error
	applyErrs []error

	// puts and batches record what was sent.
	puts    []fakePut
	batches []fakeBatch
}

type fakePut struct {
	collection string
	rkey       string
	record     map[string]any
	swapRecord string
}

type fakeBatch struct {
	writes     []pds.Write
	swapCommit string
}

func newFakeCommunityRepo(did string) *fakeCommunityRepo {
	return &fakeCommunityRepo{
		did:     did,
		records: map[string]*pds.RecordResponse{},
		head:    pds.LatestCommit{CID: "bafyreiheadaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rev: "3kjzl5headaaa"},
	}
}

func fixedFactory(repo CommunityRepo) CommunityRepoFactory {
	return func(_ context.Context, _ string) (CommunityRepo, error) { return repo, nil }
}

func (r *fakeCommunityRepo) key(collection, rkey string) string { return collection + "/" + rkey }

func (r *fakeCommunityRepo) setRecord(collection, rkey, cid string, value map[string]any) {
	r.records[r.key(collection, rkey)] = &pds.RecordResponse{
		URI:   "at://" + r.did + "/" + collection + "/" + rkey,
		CID:   cid,
		Value: value,
	}
}

func (r *fakeCommunityRepo) GetRecord(_ context.Context, collection, rkey string) (*pds.RecordResponse, error) {
	r.calls = append(r.calls, "GetRecord:"+collection)
	record, ok := r.records[r.key(collection, rkey)]
	if !ok {
		return nil, pds.ErrNotFound
	}
	return record, nil
}

func (r *fakeCommunityRepo) PutRecordWithCommit(_ context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error) {
	r.calls = append(r.calls, "PutRecordWithCommit:"+collection)
	body, _ := record.(map[string]any)
	r.puts = append(r.puts, fakePut{collection: collection, rkey: rkey, record: body, swapRecord: swapRecord})

	if len(r.putErrs) > 0 {
		err := r.putErrs[0]
		r.putErrs = r.putErrs[1:]
		if err != nil {
			return nil, err
		}
	}

	cid := "bafyreiputaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r.setRecord(collection, rkey, cid, body)
	return &pds.RecordCommit{
		URI:       "at://" + r.did + "/" + collection + "/" + rkey,
		CID:       cid,
		CommitRev: "3kjzl5putaaaa",
		CommitCID: "bafyreicommitputaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, nil
}

func (r *fakeCommunityRepo) ApplyWrites(_ context.Context, writes []pds.Write, swapCommit string) (*pds.ApplyWritesResult, error) {
	r.calls = append(r.calls, "ApplyWrites")
	r.batches = append(r.batches, fakeBatch{writes: writes, swapCommit: swapCommit})

	if len(r.applyErrs) > 0 {
		err := r.applyErrs[0]
		r.applyErrs = r.applyErrs[1:]
		if err != nil {
			return nil, err
		}
	}

	results := make([]pds.WriteResult, len(writes))
	for i, write := range writes {
		switch write.Op {
		case pds.WriteOpDelete:
			delete(r.records, r.key(write.Collection, write.RKey))
			results[i] = pds.WriteResult{Op: write.Op}
		default:
			body, _ := write.Record.(map[string]any)
			cid := "bafyreibatchaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			r.setRecord(write.Collection, write.RKey, cid, body)
			results[i] = pds.WriteResult{
				Op:  write.Op,
				URI: "at://" + r.did + "/" + write.Collection + "/" + write.RKey,
				CID: cid,
			}
		}
	}
	return &pds.ApplyWritesResult{CommitRev: "3kjzl5batchaa", CommitCID: "bafyreicommitbatchaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Results: results}, nil
}

func (r *fakeCommunityRepo) GetLatestCommit(_ context.Context) (*pds.LatestCommit, error) {
	r.calls = append(r.calls, "GetLatestCommit")
	head := r.head
	return &head, nil
}

func (r *fakeCommunityRepo) DID() string { return r.did }
