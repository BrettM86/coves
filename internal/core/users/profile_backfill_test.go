package users

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// backfillSettle is how long the skip-path tests wait before asserting that no
// backfill fetch happened — long enough to catch a regressed async fetch.
const backfillSettle = 100 * time.Millisecond

// waitForBackfill waits for the async backfill goroutine to hit the fake PDS
// and (via done) finish its UpdateProfile write.
func waitForBackfill(t *testing.T, hits *int64, done <-chan struct{}) {
	t.Helper()
	require.Eventually(t, func() bool {
		return atomic.LoadInt64(hits) == 1
	}, 5*time.Second, 10*time.Millisecond, "backfill goroutine never fetched from the PDS")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill goroutine never called UpdateProfile")
	}
}

// newProfilePDS spins up a fake PDS that answers com.atproto.repo.getRecord for
// profile records. handler receives the request after basic endpoint assertions.
func newProfilePDS(t *testing.T, expectedDID string, hits *int64, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		assert.Equal(t, "/xrpc/com.atproto.repo.getRecord", r.URL.Path)
		assert.Equal(t, expectedDID, r.URL.Query().Get("repo"))
		assert.Equal(t, ProfileCollection, r.URL.Query().Get("collection"))
		assert.Equal(t, "self", r.URL.Query().Get("rkey"))
		handler(w, r)
	}))
}

func writeProfileRecord(t *testing.T, w http.ResponseWriter, value map[string]interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]interface{}{
		"uri":   "at://did:plc:test/social.coves.actor.profile/self",
		"cid":   "bafyreicid",
		"value": value,
	})
	require.NoError(t, err)
}

func blobRef(cid string) map[string]interface{} {
	return map[string]interface{}{
		"$type":    "blob",
		"ref":      map[string]interface{}{"$link": cid},
		"mimeType": "image/png",
		"size":     12345,
	}
}

func TestFetchProfileRecord_AllFields(t *testing.T) {
	var hits int64
	srv := newProfilePDS(t, "did:plc:alice", &hits, func(w http.ResponseWriter, r *http.Request) {
		writeProfileRecord(t, w, map[string]interface{}{
			"$type":       ProfileCollection,
			"displayName": "Alice",
			"description": "hello from alice",
			"avatar":      blobRef("bafkreiavatar"),
			"banner":      blobRef("bafkreibanner"),
		})
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:alice")
	require.NoError(t, err)
	require.NotNil(t, input)

	require.NotNil(t, input.DisplayName)
	assert.Equal(t, "Alice", *input.DisplayName)
	require.NotNil(t, input.Bio)
	assert.Equal(t, "hello from alice", *input.Bio)
	require.NotNil(t, input.AvatarCID)
	assert.Equal(t, "bafkreiavatar", *input.AvatarCID)
	require.NotNil(t, input.BannerCID)
	assert.Equal(t, "bafkreibanner", *input.BannerCID)
	assert.Equal(t, int64(1), atomic.LoadInt64(&hits))
}

func TestFetchProfileRecord_Bare404IsError(t *testing.T) {
	// A 404 whose body is NOT an XRPC error object means we likely never reached
	// a PDS (stale pds_url behind a generic web server / reverse proxy). That
	// must surface as an error — silently classifying it as "user has no profile
	// record" would give up permanently and invisibly.
	var hits int64
	srv := newProfilePDS(t, "did:plc:stale", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>404</html>"))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:stale")
	require.Error(t, err, "a bare 404 (no XRPC error body) must be an error, not not-found")
	assert.Nil(t, input)
	assert.Contains(t, err.Error(), "404")
}

func TestFetchProfileRecord_XRPC404IsNotFound(t *testing.T) {
	// A 404 carrying a real XRPC error body came from a PDS — that IS a missing
	// record (some PDS implementations 404 instead of 400 RecordNotFound).
	var hits int64
	srv := newProfilePDS(t, "did:plc:notfound404", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"NotFound","message":"record not found"}`))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:notfound404")
	require.NoError(t, err)
	assert.Nil(t, input, "XRPC 404 means the user has no profile record")
}

func TestFetchProfileRecord_RecordNotFound(t *testing.T) {
	var hits int64
	srv := newProfilePDS(t, "did:plc:bob", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"RecordNotFound","message":"Record not found"}`))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:bob")
	require.NoError(t, err)
	assert.Nil(t, input, "missing profile record must be a nil result, not an error")
}

func TestFetchProfileRecord_CouldNotLocateRecordMessage(t *testing.T) {
	// Some PDS implementations use InvalidRequest with a descriptive message
	var hits int64
	srv := newProfilePDS(t, "did:plc:bob", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"InvalidRequest","message":"Could not locate record: at://did:plc:bob/social.coves.actor.profile/self"}`))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:bob")
	require.NoError(t, err)
	assert.Nil(t, input)
}

func TestFetchProfileRecord_ServerErrorPropagates(t *testing.T) {
	var hits int64
	srv := newProfilePDS(t, "did:plc:carol", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:carol")
	require.Error(t, err)
	assert.Nil(t, input)
	assert.Contains(t, err.Error(), "500")
}

func TestFetchProfileRecord_OtherBadRequestIsError(t *testing.T) {
	// A 400 that is NOT record-not-found (e.g. malformed repo) must surface as an
	// error so callers don't mistake a broken request for "user has no profile"
	var hits int64
	srv := newProfilePDS(t, "did:plc:dave", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"InvalidRequest","message":"Invalid repo identifier"}`))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:dave")
	require.Error(t, err)
	assert.Nil(t, input)
}

func TestFetchProfileRecord_LocateMessageWithWrongErrorCodeIsError(t *testing.T) {
	// The "could not locate record" message substring is only trusted on the
	// error codes the reference PDS uses for missing records (RecordNotFound,
	// InvalidRequest). An arbitrary error that merely mentions the phrase must
	// not be classified as not-found.
	var hits int64
	srv := newProfilePDS(t, "did:plc:grace", &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"SomethingElse","message":"could not locate record"}`))
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:grace")
	require.Error(t, err, "message substring alone must not classify a 400 as not-found")
	assert.Nil(t, input)
}

func TestParseProfileRecord_TruncatesOverlongFields(t *testing.T) {
	// The users table CHECK constraints cap display_name at 64 and bio at 256
	// characters (Postgres length() counts characters, so truncation must be by
	// runes). Multi-byte runes prove the cut is not by bytes and never splits a rune.
	longName := strings.Repeat("é", 70)         // 70 runes, 140 bytes
	longDescription := strings.Repeat("日", 300) // 300 runes, 900 bytes

	input := parseProfileRecord(map[string]interface{}{
		"displayName": longName,
		"description": longDescription,
	})

	require.NotNil(t, input.DisplayName)
	assert.Equal(t, 64, utf8.RuneCountInString(*input.DisplayName))
	assert.Equal(t, strings.Repeat("é", 64), *input.DisplayName)
	assert.True(t, utf8.ValidString(*input.DisplayName), "truncation must not split a rune")

	require.NotNil(t, input.Bio)
	assert.Equal(t, 256, utf8.RuneCountInString(*input.Bio))
	assert.Equal(t, strings.Repeat("日", 256), *input.Bio)
	assert.True(t, utf8.ValidString(*input.Bio), "truncation must not split a rune")

	// At-limit values pass through untouched
	atLimit := parseProfileRecord(map[string]interface{}{
		"displayName": strings.Repeat("a", 64),
		"description": strings.Repeat("b", 256),
	})
	require.NotNil(t, atLimit.DisplayName)
	assert.Equal(t, strings.Repeat("a", 64), *atLimit.DisplayName)
	require.NotNil(t, atLimit.Bio)
	assert.Equal(t, strings.Repeat("b", 256), *atLimit.Bio)
}

func TestParseProfileRecord_RejectsImplausibleBlobCID(t *testing.T) {
	input := parseProfileRecord(map[string]interface{}{
		"avatar": blobRef("../../../etc/passwd"),    // non-alphanumeric chars
		"banner": blobRef(strings.Repeat("a", 300)), // over the length cap
	})
	assert.Nil(t, input.AvatarCID, "$link with non-CID charset must be rejected")
	assert.Nil(t, input.BannerCID, "$link over the length cap must be rejected")

	valid := parseProfileRecord(map[string]interface{}{
		"avatar": blobRef("bafkreib2qya3v6fyfvdkr5gkuwrmhxjkkvsyyx2xczpkqnvkq3rc5jm2gq"),
	})
	require.NotNil(t, valid.AvatarCID)
	assert.Equal(t, "bafkreib2qya3v6fyfvdkr5gkuwrmhxjkkvsyyx2xczpkqnvkq3rc5jm2gq", *valid.AvatarCID)
}

func TestFetchProfileRecord_MalformedBlobIgnored(t *testing.T) {
	var hits int64
	srv := newProfilePDS(t, "did:plc:eve", &hits, func(w http.ResponseWriter, r *http.Request) {
		writeProfileRecord(t, w, map[string]interface{}{
			"displayName": "Eve",
			// Not a blob ref: missing $type/ref structure
			"avatar": map[string]interface{}{"cid": "bafkreibad"},
		})
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:eve")
	require.NoError(t, err)
	require.NotNil(t, input)
	require.NotNil(t, input.DisplayName)
	assert.Equal(t, "Eve", *input.DisplayName)
	assert.Nil(t, input.AvatarCID, "malformed blob ref must not produce an avatar CID")
}

func TestFetchProfileRecord_EmptyRecordIsNil(t *testing.T) {
	var hits int64
	srv := newProfilePDS(t, "did:plc:frank", &hits, func(w http.ResponseWriter, r *http.Request) {
		writeProfileRecord(t, w, map[string]interface{}{"$type": ProfileCollection})
	})
	defer srv.Close()

	input, err := FetchProfileRecord(context.Background(), srv.Client(), srv.URL, "did:plc:frank")
	require.NoError(t, err)
	assert.Nil(t, input, "record with no indexable fields means nothing to apply")
}

// TestIndexUser_BackfillsEmptyProfile verifies the full IndexUser → fetch → UpdateProfile
// path for a newly indexed user with no profile data.
func TestIndexUser_BackfillsEmptyProfile(t *testing.T) {
	testDID := "did:plc:backfillme"
	testHandle := "backfillme.test"

	var hits int64
	srv := newProfilePDS(t, testDID, &hits, func(w http.ResponseWriter, r *http.Request) {
		writeProfileRecord(t, w, map[string]interface{}{
			"displayName": "Backfilled",
			"avatar":      blobRef("bafkreiavatar"),
		})
	})
	defer srv.Close()

	mockRepo := new(MockUserRepository)
	newUser := &User{
		DID:       testDID,
		Handle:    testHandle,
		PDSURL:    srv.URL, // profile is fetched from the user's own PDS
		CreatedAt: time.Now(),
	}
	mockRepo.On("Create", mock.Anything, mock.Anything).Return(newUser, nil)
	// The detached goroutine re-checks emptiness before writing (a concurrent
	// firehose event may have won the race) — still-empty user lets the write proceed.
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(newUser, nil)
	done := make(chan struct{})
	mockRepo.On("UpdateProfile", mock.Anything, testDID, mock.MatchedBy(func(input UpdateProfileInput) bool {
		return input.DisplayName != nil && *input.DisplayName == "Backfilled" &&
			input.AvatarCID != nil && *input.AvatarCID == "bafkreiavatar" &&
			input.Bio == nil && input.BannerCID == nil
	})).Return(newUser, nil).Run(func(args mock.Arguments) { close(done) })

	service := NewUserService(mockRepo, nil, "https://default.pds", nil, "",
		WithProfileBackfill(srv.Client()))

	err := service.IndexUser(context.Background(), testDID, testHandle, srv.URL)
	require.NoError(t, err)

	waitForBackfill(t, &hits, done)
	mockRepo.AssertExpectations(t)
}

// TestIndexUser_SkipsBackfillWhenProfilePopulated verifies that users who already have
// profile data are never re-fetched (the firehose is the source of truth for them).
func TestIndexUser_SkipsBackfillWhenProfilePopulated(t *testing.T) {
	testDID := "did:plc:hasprofile"

	var hits int64
	srv := newProfilePDS(t, testDID, &hits, func(w http.ResponseWriter, r *http.Request) {
		t.Error("PDS must not be called for a user with existing profile data")
	})
	defer srv.Close()

	mockRepo := new(MockUserRepository)
	existingUser := &User{
		DID:       testDID,
		Handle:    "hasprofile.test",
		PDSURL:    srv.URL,
		AvatarCID: "bafkreialready",
		CreatedAt: time.Now(),
	}
	mockRepo.On("Create", mock.Anything, mock.Anything).Return(existingUser, nil)

	service := NewUserService(mockRepo, nil, "https://default.pds", nil, "",
		WithProfileBackfill(srv.Client()))

	err := service.IndexUser(context.Background(), testDID, "hasprofile.test", srv.URL)
	require.NoError(t, err)

	// The emptiness check is synchronous (no goroutine spawns for populated
	// profiles), but settle briefly so a regressed async fetch would be caught.
	time.Sleep(backfillSettle)
	assert.Equal(t, int64(0), atomic.LoadInt64(&hits))
	mockRepo.AssertNotCalled(t, "UpdateProfile", mock.Anything, mock.Anything, mock.Anything)
}

// TestIndexUser_BackfillDisabledByDefault verifies that without WithProfileBackfill,
// IndexUser never reaches out to a PDS (preserves prior behavior for all callers that
// don't opt in).
func TestIndexUser_BackfillDisabledByDefault(t *testing.T) {
	testDID := "did:plc:nobackfill"

	var hits int64
	srv := newProfilePDS(t, testDID, &hits, func(w http.ResponseWriter, r *http.Request) {
		t.Error("PDS must not be called when backfill is not enabled")
	})
	defer srv.Close()

	mockRepo := new(MockUserRepository)
	newUser := &User{DID: testDID, Handle: "nobackfill.test", PDSURL: srv.URL, CreatedAt: time.Now()}
	mockRepo.On("Create", mock.Anything, mock.Anything).Return(newUser, nil)

	service := NewUserService(mockRepo, nil, "https://default.pds", nil, "")

	err := service.IndexUser(context.Background(), testDID, "nobackfill.test", srv.URL)
	require.NoError(t, err)

	// Backfill disabled → no goroutine spawns; settle briefly to catch a
	// regressed async fetch before asserting.
	time.Sleep(backfillSettle)
	assert.Equal(t, int64(0), atomic.LoadInt64(&hits))
}

// TestIndexUser_BackfillFailureDoesNotFailIndexing verifies backfill is best-effort:
// a dead or erroring PDS must not fail the IndexUser call that triggered it (which
// would dead-letter the post/comment event being consumed).
func TestIndexUser_BackfillFailureDoesNotFailIndexing(t *testing.T) {
	testDID := "did:plc:deadpds"

	var hits int64
	srv := newProfilePDS(t, testDID, &hits, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	mockRepo := new(MockUserRepository)
	newUser := &User{DID: testDID, Handle: "deadpds.test", PDSURL: srv.URL, CreatedAt: time.Now()}
	mockRepo.On("Create", mock.Anything, mock.Anything).Return(newUser, nil)

	service := NewUserService(mockRepo, nil, "https://default.pds", nil, "",
		WithProfileBackfill(srv.Client()))

	err := service.IndexUser(context.Background(), testDID, "deadpds.test", srv.URL)
	assert.NoError(t, err, "backfill failure must never fail indexing")

	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&hits) == 1
	}, 5*time.Second, 10*time.Millisecond, "backfill goroutine never fetched from the PDS")
	// Settle so the goroutine's post-fetch path (which must bail on the error)
	// has finished before asserting no write happened.
	time.Sleep(backfillSettle)
	mockRepo.AssertNotCalled(t, "UpdateProfile", mock.Anything, mock.Anything, mock.Anything)
}

// TestIndexUser_BackfillsOnHandleChange verifies the handle-conflict path (existing
// user, changed handle) also heals an empty profile.
func TestIndexUser_BackfillsOnHandleChange(t *testing.T) {
	testDID := "did:plc:renamed"
	newHandle := "newname.test"

	var hits int64
	srv := newProfilePDS(t, testDID, &hits, func(w http.ResponseWriter, r *http.Request) {
		writeProfileRecord(t, w, map[string]interface{}{"displayName": "Renamed"})
	})
	defer srv.Close()

	mockRepo := new(MockUserRepository)
	renamedUser := &User{DID: testDID, Handle: newHandle, PDSURL: srv.URL, CreatedAt: time.Now()}
	mockRepo.On("Create", mock.Anything, mock.Anything).Return(nil, ErrHandleAlreadyTaken)
	mockRepo.On("UpdateHandle", mock.Anything, testDID, newHandle).Return(renamedUser, nil)
	mockRepo.On("GetByDID", mock.Anything, testDID).Return(renamedUser, nil)
	done := make(chan struct{})
	mockRepo.On("UpdateProfile", mock.Anything, testDID, mock.MatchedBy(func(input UpdateProfileInput) bool {
		return input.DisplayName != nil && *input.DisplayName == "Renamed"
	})).Return(renamedUser, nil).Run(func(args mock.Arguments) { close(done) })

	service := NewUserService(mockRepo, nil, "https://default.pds", nil, "",
		WithProfileBackfill(srv.Client()))

	err := service.IndexUser(context.Background(), testDID, newHandle, srv.URL)
	require.NoError(t, err)

	waitForBackfill(t, &hits, done)
	mockRepo.AssertExpectations(t)
}
