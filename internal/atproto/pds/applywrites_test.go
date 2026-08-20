package pds

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atclient"
)

// The commit-aware transport: applyWrites, the commit rev, and the two error
// classes a state-shaped writer cannot work without.
//
// These are transport tests. They prove the JSON that goes on the wire and the
// Go values that come back, because everything above them — the removal commit
// that must not be observable in halves, the acceptance that must not be
// re-minted on a retry — is built out of exactly those two things and cannot
// be debugged through them.

const (
	applyWritesDID = "did:plc:test"
	testCommitRev  = "3kjzl5kcb2s2v"
	testCommitCID  = "bafyreicommitaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// newCommitClient returns a client pointed at a test server, as the commit-aware
// interface. The assertion that the concrete client satisfies CommitClient at
// all is TestClientImplementsCommitClient below.
func newCommitClient(t *testing.T, handler http.HandlerFunc) (CommitClient, func()) {
	t.Helper()

	server := httptest.NewServer(handler)

	// The hatch is open because httptest listens on loopback, which is exactly the
	// address class the guard refuses. These tests are about applyWrites' WIRE
	// FORMAT, not about which addresses may be dialled — factory_guard_test.go
	// owns that — so opening it here keeps each test measuring one thing.
	generic, err := NewFromAccessToken(server.URL, applyWritesDID, "test-token",
		PrivateHostOptions(true)...)
	if err != nil {
		server.Close()
		t.Fatalf("NewFromAccessToken: %v", err)
	}

	commit, ok := generic.(CommitClient)
	if !ok {
		server.Close()
		t.Fatal("the concrete PDS client does not implement CommitClient; the community-repo " +
			"writers cannot be built on it")
	}
	return commit, server.Close
}

func TestClientImplementsCommitClient(t *testing.T) {
	var _ CommitClient = (*client)(nil)
}

func TestClient_ApplyWrites_WireFormat(t *testing.T) {
	// One commit carrying the whole moderation action: the acceptance goes, the
	// removal arrives. §3.3 requires them together so the firehose never
	// carries a half-completed action, and this is the request that makes that
	// true.
	var payload map[string]any

	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/xrpc/com.atproto.repo.applyWrites" {
			t.Errorf("path = %q, want /xrpc/com.atproto.repo.applyWrites", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"commit": map[string]any{"cid": testCommitCID, "rev": testCommitRev},
			"results": []any{
				map[string]any{"$type": "com.atproto.repo.applyWrites#deleteResult"},
				map[string]any{
					"$type": "com.atproto.repo.applyWrites#createResult",
					"uri":   "at://did:plc:test/social.coves.community.removal/rk",
					"cid":   "bafyreiremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			},
		})
	}

	c, closeServer := newCommitClient(t, handler)
	defer closeServer()

	result, err := c.ApplyWrites(context.Background(), []Write{
		{Op: WriteOpDelete, Collection: "social.coves.community.acceptance", RKey: "rk"},
		{
			Op:         WriteOpCreate,
			Collection: "social.coves.community.removal",
			RKey:       "rk",
			Record:     map[string]any{"$type": "social.coves.community.removal", "code": "spam"},
		},
	}, testCommitCID)
	if err != nil {
		t.Fatalf("ApplyWrites: %v", err)
	}
	if result == nil {
		t.Fatal("ApplyWrites returned no result")
	}

	if got := payload["repo"]; got != applyWritesDID {
		t.Errorf("repo = %v, want %s", got, applyWritesDID)
	}

	// swapCommit is the batch's optimistic guard. Without it a concurrent
	// moderator action is silently clobbered instead of detected.
	if got := payload["swapCommit"]; got != testCommitCID {
		t.Errorf("swapCommit = %v, want %s", got, testCommitCID)
	}

	// validate:false, and it must be the BOOLEAN false rather than absent.
	// These are Coves lexicons; a PDS that has never been taught them refuses
	// to validate them, and the lexicon's default is not false.
	validate, present := payload["validate"]
	if !present {
		t.Error("validate is absent from the request; the PDS cannot validate Coves lexicons and will refuse the batch")
	} else if validate != false {
		t.Errorf("validate = %v, want false", validate)
	}

	writes, ok := payload["writes"].([]any)
	if !ok {
		t.Fatalf("writes = %#v, want an array", payload["writes"])
	}
	if len(writes) != 2 {
		t.Fatalf("len(writes) = %d, want 2", len(writes))
	}

	// The union discriminant. A batch whose entries carry no $type is not an
	// applyWrites batch at all — the PDS cannot tell a create from a delete.
	del, _ := writes[0].(map[string]any)
	if del["$type"] != "com.atproto.repo.applyWrites#delete" {
		t.Errorf("writes[0].$type = %v, want com.atproto.repo.applyWrites#delete", del["$type"])
	}
	if del["collection"] != "social.coves.community.acceptance" || del["rkey"] != "rk" {
		t.Errorf("writes[0] = %#v, want the acceptance at rkey rk", del)
	}
	if _, hasRecord := del["value"]; hasRecord {
		t.Error("a delete must not carry a record body")
	}

	create, _ := writes[1].(map[string]any)
	if create["$type"] != "com.atproto.repo.applyWrites#create" {
		t.Errorf("writes[1].$type = %v, want com.atproto.repo.applyWrites#create", create["$type"])
	}
	if create["value"] == nil {
		t.Error("a create must carry its record body")
	}

	// The commit rev is the §5.2 watermark. Dropping it is the whole reason
	// this method exists alongside the older ones.
	if result.CommitRev != testCommitRev {
		t.Errorf("CommitRev = %q, want %q", result.CommitRev, testCommitRev)
	}
	if result.CommitCID != testCommitCID {
		t.Errorf("CommitCID = %q, want %q", result.CommitCID, testCommitCID)
	}
	if len(result.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(result.Results))
	}
	if result.Results[1].URI != "at://did:plc:test/social.coves.community.removal/rk" {
		t.Errorf("Results[1].URI = %q, want the removal's URI", result.Results[1].URI)
	}
}

func TestClient_ApplyWrites_UpdateUsesTheUpdateDiscriminant(t *testing.T) {
	// create-vs-update is not cosmetic: the PDS answers a create of an existing
	// record with a 500, and an update of a missing one likewise. The writer
	// chooses between them from a pre-read, and this proves the choice survives
	// onto the wire.
	var payload map[string]any

	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"commit": map[string]any{"cid": testCommitCID, "rev": testCommitRev},
			"results": []any{map[string]any{
				"$type": "com.atproto.repo.applyWrites#updateResult",
				"uri":   "at://did:plc:test/social.coves.community.removal/rk",
				"cid":   "bafyreiremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
		})
	})
	defer closeServer()

	if _, err := c.ApplyWrites(context.Background(), []Write{{
		Op:         WriteOpUpdate,
		Collection: "social.coves.community.removal",
		RKey:       "rk",
		Record:     map[string]any{"$type": "social.coves.community.removal"},
	}}, ""); err != nil {
		t.Fatalf("ApplyWrites: %v", err)
	}

	writes, _ := payload["writes"].([]any)
	if len(writes) != 1 {
		t.Fatalf("len(writes) = %d, want 1", len(writes))
	}
	if entry, _ := writes[0].(map[string]any); entry["$type"] != "com.atproto.repo.applyWrites#update" {
		t.Errorf("writes[0].$type = %v, want com.atproto.repo.applyWrites#update", entry["$type"])
	}

	// An empty swapCommit means "no guard", not "guard against the empty
	// string". Sending it would have every unguarded batch rejected.
	if _, present := payload["swapCommit"]; present {
		t.Error("an empty swapCommit must be omitted, not sent")
	}
}

func TestClient_ApplyWrites_RefusesAShortResultsArray(t *testing.T) {
	// The results are POSITIONAL — one per submitted write, in order — and the
	// caller indexes into them to find the record its commit made stand
	// (standCIDOf in the community writer). A server returning fewer results
	// than writes would silently hand the caller the WRONG entry, or an empty
	// CID for a record that committed. That is a malformed success and must be
	// an error, not a zero value the caller persists.
	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"commit": map[string]any{"cid": testCommitCID, "rev": testCommitRev},
			// Two writes went up; one result comes back.
			"results": []any{
				map[string]any{"$type": "com.atproto.repo.applyWrites#deleteResult"},
			},
		})
	})
	defer closeServer()

	_, err := c.ApplyWrites(context.Background(), []Write{
		{Op: WriteOpDelete, Collection: "social.coves.community.acceptance", RKey: "rk"},
		{
			Op:         WriteOpCreate,
			Collection: "social.coves.community.removal",
			RKey:       "rk",
			Record:     map[string]any{"$type": "social.coves.community.removal", "code": "spam"},
		},
	}, testCommitCID)
	if err == nil {
		t.Fatal("a results array shorter than the batch is a malformed response and must be an error")
	}
}

func TestClient_ApplyWrites_RefusesACreateOrUpdateResultWithoutURIOrCID(t *testing.T) {
	// A create or an update committed a record the caller is about to
	// reference: its result's uri and cid are exactly what gets persisted onto
	// the admission row. A 200 that omits them is the same class of malformed
	// body recordCommit refuses for single-record writes.
	for name, entry := range map[string]map[string]any{
		"create without cid": {
			"$type": "com.atproto.repo.applyWrites#createResult",
			"uri":   "at://did:plc:test/social.coves.community.removal/rk",
		},
		"update without uri": {
			"$type": "com.atproto.repo.applyWrites#updateResult",
			"cid":   "bafyreiremovalaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, closeServer := newCommitClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"commit":  map[string]any{"cid": testCommitCID, "rev": testCommitRev},
					"results": []any{entry},
				})
			})
			defer closeServer()

			op := WriteOpCreate
			if name == "update without uri" {
				op = WriteOpUpdate
			}
			_, err := c.ApplyWrites(context.Background(), []Write{{
				Op:         op,
				Collection: "social.coves.community.removal",
				RKey:       "rk",
				Record:     map[string]any{"$type": "social.coves.community.removal", "code": "spam"},
			}}, "")
			if err == nil {
				t.Fatal("a create/update result without uri+cid is a malformed response and must be an error")
			}
		})
	}
}

func TestClient_ApplyWrites_MapsInvalidSwapToSwapConflict(t *testing.T) {
	// VERIFIED AGAINST A LIVE PDS: a failed swap comes back as HTTP 400 with
	// "error": "InvalidSwap", NOT the 409 the lexicon documents. That is why
	// the status code alone is not enough — 400 is otherwise ErrBadRequest,
	// which a caller would report rather than retry.
	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "InvalidSwap",
			"message": "Commit was at bafyreiotheraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
	})
	defer closeServer()

	_, err := c.ApplyWrites(context.Background(), []Write{
		{Op: WriteOpDelete, Collection: "social.coves.community.acceptance", RKey: "rk"},
	}, testCommitCID)
	if err == nil {
		t.Fatal("a lost swap must be an error")
	}
	if !errors.Is(err, ErrSwapConflict) {
		t.Errorf("error %v does not match ErrSwapConflict; a lost race is the one 400 that must be "+
			"re-read and retried rather than reported", err)
	}
}

func TestClient_PutRecordWithCommit(t *testing.T) {
	var payload map[string]any

	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/com.atproto.repo.putRecord" {
			t.Errorf("path = %q, want /xrpc/com.atproto.repo.putRecord", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uri":    "at://did:plc:test/social.coves.community.acceptance/rk",
			"cid":    "bafyreiacceptanceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"commit": map[string]any{"cid": testCommitCID, "rev": testCommitRev},
		})
	})
	defer closeServer()

	result, err := c.PutRecordWithCommit(context.Background(),
		"social.coves.community.acceptance", "rk",
		map[string]any{"$type": "social.coves.community.acceptance"},
		"bafyreipreviousaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("PutRecordWithCommit: %v", err)
	}
	if result == nil {
		t.Fatal("PutRecordWithCommit returned no result")
	}

	if payload["swapRecord"] != "bafyreipreviousaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("swapRecord = %v, want the CID the caller expected to be replacing", payload["swapRecord"])
	}
	if result.URI != "at://did:plc:test/social.coves.community.acceptance/rk" {
		t.Errorf("URI = %q", result.URI)
	}
	if result.CID != "bafyreiacceptanceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("CID = %q", result.CID)
	}
	if result.CommitRev != testCommitRev {
		t.Errorf("CommitRev = %q, want %q — without it the admission row has no watermark to stamp",
			result.CommitRev, testCommitRev)
	}
}

func TestClient_PutRecordWithCommit_MapsInvalidSwapToSwapConflict(t *testing.T) {
	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "InvalidSwap",
			"message": "Record was at bafyreiotheraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
	})
	defer closeServer()

	_, err := c.PutRecordWithCommit(context.Background(),
		"social.coves.community.acceptance", "rk",
		map[string]any{"$type": "social.coves.community.acceptance"}, "bafyreistale")
	if !errors.Is(err, ErrSwapConflict) {
		t.Errorf("error %v does not match ErrSwapConflict", err)
	}
}

func TestClient_CreateRecordWithCommit(t *testing.T) {
	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/com.atproto.repo.createRecord" {
			t.Errorf("path = %q, want /xrpc/com.atproto.repo.createRecord", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uri":    "at://did:plc:test/social.coves.community.acceptance/rk",
			"cid":    "bafyreiacceptanceaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"commit": map[string]any{"cid": testCommitCID, "rev": testCommitRev},
		})
	})
	defer closeServer()

	result, err := c.CreateRecordWithCommit(context.Background(),
		"social.coves.community.acceptance", "rk",
		map[string]any{"$type": "social.coves.community.acceptance"})
	if err != nil {
		t.Fatalf("CreateRecordWithCommit: %v", err)
	}
	if result == nil {
		t.Fatal("CreateRecordWithCommit returned no result")
	}
	if result.CommitRev != testCommitRev {
		t.Errorf("CommitRev = %q, want %q", result.CommitRev, testCommitRev)
	}
}

func TestClient_GetLatestCommit(t *testing.T) {
	// The swapCommit a batch is guarded by has to come from somewhere, and it
	// has to be read immediately before the batch is shaped — the pre-read and
	// the commit it is consistent with are the same observation.
	c, closeServer := newCommitClient(t, func(w http.ResponseWriter, r *http.Request) {
		// THE SYNC NAMESPACE, NOT THE REPO ONE. getLatestCommit is
		// com.atproto.sync.getLatestCommit — there is no repo-namespace
		// spelling, and a PDS answers that one with "No service configured"
		// rather than a 404, so the mistake reads as a deployment problem
		// instead of a wrong method name.
		if r.URL.Path != "/xrpc/com.atproto.sync.getLatestCommit" {
			t.Errorf("path = %q, want /xrpc/com.atproto.sync.getLatestCommit", r.URL.Path)
		}
		if got := r.URL.Query().Get("did"); got != applyWritesDID {
			t.Errorf("did = %q, want %q", got, applyWritesDID)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"cid": testCommitCID, "rev": testCommitRev})
	})
	defer closeServer()

	commit, err := c.GetLatestCommit(context.Background())
	if err != nil {
		t.Fatalf("GetLatestCommit: %v", err)
	}
	if commit == nil {
		t.Fatal("GetLatestCommit returned no commit")
	}
	if commit.CID != testCommitCID || commit.Rev != testCommitRev {
		t.Errorf("commit = %+v, want cid %q rev %q", commit, testCommitCID, testCommitRev)
	}
}

func TestWrapAPIError_ServerErrorsAreTheirOwnClass(t *testing.T) {
	// applyWrites answers a delete of a missing record — and a create of an
	// existing one — with a 500. A state-shaped writer meeting that has to know
	// its pre-read went stale and re-shape the batch, which it cannot do if a
	// 500 is indistinguishable from a socket that died mid-request.
	for _, status := range []int{500, 502, 503, 504} {
		err := wrapAPIError(&atclient.APIError{
			StatusCode: status,
			Name:       "InternalServerError",
			Message:    "Could not delete record: not found",
		}, "applyWrites")

		if !errors.Is(err, ErrServerError) {
			t.Errorf("HTTP %d: error %v does not match ErrServerError", status, err)
		}
	}
}

func TestWrapAPIError_NameChecksOutrankTheStatusEvenOn5xx(t *testing.T) {
	// The name says what happened; the status says how the server framed it. A
	// PDS (or a proxy in front of one) that wraps an InvalidSwap in a 500 is
	// still reporting a lost swap, and a caller that saw only ErrServerError
	// would resend the same shape instead of re-reading — the exact behaviour
	// the sentinel exists to prevent. So the name checks run BEFORE the 5xx
	// branch, for every status.
	err := wrapAPIError(&atclient.APIError{
		StatusCode: 500,
		Name:       "InvalidSwap",
		Message:    "Commit was at bafyreiotheraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, "applyWrites")
	if !errors.Is(err, ErrSwapConflict) {
		t.Errorf("a 500-carrying InvalidSwap must still map ErrSwapConflict, got %v", err)
	}

	err = wrapAPIError(&atclient.APIError{
		StatusCode: 500,
		Name:       "RecordNotFound",
		Message:    "Record not found",
	}, "getRecord")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("a 500-carrying RecordNotFound must still map ErrNotFound, got %v", err)
	}
}

func TestWrapAPIError_InvalidSwapIsNotAPlainBadRequest(t *testing.T) {
	err := wrapAPIError(&atclient.APIError{
		StatusCode: 400,
		Name:       "InvalidSwap",
		Message:    "Record was at bafyreiother",
	}, "putRecord")

	if !errors.Is(err, ErrSwapConflict) {
		t.Errorf("error %v does not match ErrSwapConflict", err)
	}

	// An ordinary 400 must keep meaning what it always meant. The InvalidSwap
	// branch is a name test on top of the status, not a replacement for it.
	plain := wrapAPIError(&atclient.APIError{
		StatusCode: 400,
		Name:       "InvalidRequest",
		Message:    "Bad input",
	}, "putRecord")

	if !errors.Is(plain, ErrBadRequest) {
		t.Errorf("an ordinary 400 must still be ErrBadRequest, got %v", plain)
	}
	if errors.Is(plain, ErrSwapConflict) {
		t.Error("a malformed request is not a lost race")
	}
}
