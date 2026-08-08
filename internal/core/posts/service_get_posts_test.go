package posts

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeBlockChecker is an in-memory BlockChecker for unit tests. It records the most
// recent call so tests can assert the viewer DID and deduped author DIDs passed in.
type fakeBlockChecker struct {
	blocked    map[string]bool // authorDID -> blocked
	err        error
	called     bool
	gotBlocker string
	gotDIDs    []string
}

func (f *fakeBlockChecker) AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error) {
	f.called = true
	f.gotBlocker = blockerDID
	f.gotDIDs = blockedDIDs
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]bool)
	for _, did := range blockedDIDs {
		if f.blocked[did] {
			out[did] = true
		}
	}
	return out, nil
}

// viewWithAuthor builds a minimal found PostView authored by authorDID.
func viewWithAuthor(uri, authorDID string) *PostView {
	return &PostView{URI: uri, CID: "cid-" + authorDID, Author: &AuthorView{DID: authorDID}}
}

const testCommunityDID = "did:plc:ewvi7nxzyoun6zhxrhs64oiz"

func didPostURI(rkey string) string {
	return "at://" + testCommunityDID + "/" + LegacyPostCollection + "/" + rkey
}

func TestParsePostURIParts(t *testing.T) {
	// parsePostURIParts is the pure splitter: it validates scheme/structure/collection/rkey
	// but does NOT check whether the authority is a DID or a handle.
	tests := []struct {
		name          string
		uri           string
		wantAuthority string
		wantRKey      string
		wantErr       bool
	}{
		{
			name:          "valid DID authority",
			uri:           didPostURI("abc123"),
			wantAuthority: testCommunityDID,
			wantRKey:      "abc123",
		},
		{
			name:          "handle authority parses (DID check happens in validatePostURI)",
			uri:           "at://c-test-community/" + LegacyPostCollection + "/abc123",
			wantAuthority: "c-test-community",
			wantRKey:      "abc123",
		},
		{
			name:    "missing at:// scheme",
			uri:     testCommunityDID + "/" + LegacyPostCollection + "/abc123",
			wantErr: true,
		},
		{
			name:    "too few segments",
			uri:     "at://" + testCommunityDID + "/" + LegacyPostCollection,
			wantErr: true,
		},
		{
			name:    "wrong collection",
			uri:     "at://" + testCommunityDID + "/app.bsky.feed.post/abc123",
			wantErr: true,
		},
		{
			name:    "missing rkey",
			uri:     "at://" + testCommunityDID + "/" + LegacyPostCollection + "/",
			wantErr: true,
		},
		{
			name:    "empty authority",
			uri:     "at:///" + LegacyPostCollection + "/abc123",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authority, rkey, err := parsePostURIParts(tt.uri, "uris")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePostURIParts(%q) = nil error, want error", tt.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePostURIParts(%q) unexpected error: %v", tt.uri, err)
			}
			if authority != tt.wantAuthority {
				t.Errorf("authority = %q, want %q", authority, tt.wantAuthority)
			}
			if rkey != tt.wantRKey {
				t.Errorf("rkey = %q, want %q", rkey, tt.wantRKey)
			}
		})
	}
}

func TestValidatePostURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr bool
	}{
		{name: "valid DID-based URI", uri: didPostURI("abc123"), wantErr: false},
		{name: "handle authority is rejected", uri: "at://c-test-community/" + LegacyPostCollection + "/abc123", wantErr: true},
		{name: "missing scheme", uri: testCommunityDID + "/" + LegacyPostCollection + "/abc", wantErr: true},
		{name: "wrong collection", uri: "at://" + testCommunityDID + "/app.bsky.feed.post/abc", wantErr: true},
		{name: "missing rkey", uri: "at://" + testCommunityDID + "/" + LegacyPostCollection + "/", wantErr: true},
		{name: "malformed DID authority", uri: "at://did:plc:UPPERCASE/" + LegacyPostCollection + "/abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePostURI(tt.uri)
			if tt.wantErr && err == nil {
				t.Fatalf("validatePostURI(%q) = nil, want error", tt.uri)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validatePostURI(%q) = %v, want nil", tt.uri, err)
			}
		})
	}
}

func TestGetPosts_Validation(t *testing.T) {
	s := &postService{}

	t.Run("empty uris is rejected", func(t *testing.T) {
		_, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: nil})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for empty uris, got %v", err)
		}
	})

	t.Run("too many uris is rejected", func(t *testing.T) {
		uris := make([]string, MaxGetPostsURIs+1)
		for i := range uris {
			uris[i] = didPostURI("rkey")
		}
		_, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: uris})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for too many uris, got %v", err)
		}
	})
}

// TestGetPosts_RejectsNonCanonicalURI verifies that malformed and handle-based URIs are
// rejected with a clear error (rather than silently degrading to notFound, which would
// hide client bugs and break on community handle changes).
func TestGetPosts_RejectsNonCanonicalURI(t *testing.T) {
	s := &postService{} // repo not reached: validation happens before the fetch

	t.Run("handle-based URI is rejected", func(t *testing.T) {
		_, err := s.GetPosts(context.Background(), GetPostsRequest{
			URIs: []string{"at://c-test-community/" + LegacyPostCollection + "/abc"},
		})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for handle-based URI, got %v", err)
		}
	})

	t.Run("malformed URI is rejected", func(t *testing.T) {
		_, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{"not-a-uri"}})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("expected validation error for malformed URI, got %v", err)
		}
	})
}

// TestGetPosts_OrderingAndNotFound verifies request order is preserved and that valid
// DID-based URIs whose post is absent come back as notFoundPost markers.
func TestGetPosts_OrderingAndNotFound(t *testing.T) {
	found := didPostURI("found1")
	missing := didPostURI("missing1")

	repo := &mockRepository{
		getViewsByURIsFunc: func(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error) {
			// Only the "found" URI exists in the AppView
			return map[string]*PostView{
				found: {URI: found, CID: "cid-found"},
			}, nil
		},
	}
	s := &postService{repo: repo}

	results, err := s.GetPosts(context.Background(), GetPostsRequest{
		URIs: []string{found, missing},
	})
	if err != nil {
		t.Fatalf("GetPosts returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (input order preserved), got %d", len(results))
	}

	// [0] found -> postView
	if results[0].Post == nil || results[0].NotFound != nil {
		t.Fatalf("results[0] expected a Post, got %+v", results[0])
	}
	if results[0].Post.URI != found {
		t.Errorf("results[0].Post.URI = %q, want %q", results[0].Post.URI, found)
	}

	// [1] missing (valid URI, absent in repo) -> notFoundPost echoing the requested URI
	if results[1].Post != nil || results[1].NotFound == nil {
		t.Fatalf("results[1] expected NotFound, got %+v", results[1])
	}
	if results[1].NotFound.URI != missing || !results[1].NotFound.NotFound {
		t.Errorf("results[1].NotFound = %+v, want {URI:%q, NotFound:true}", results[1].NotFound, missing)
	}
}

// TestGetPosts_ThreadsViewerIntoVisibilityGate pins that post.get asks the repository
// the question the CALLER asked, not an anonymous one.
//
// The service already had req.ViewerDID in hand (it runs the block filter on it) and
// passed "" to the visibility gate anyway, which made post.get the only surface that
// refused an author their own pending post — actor.getPosts, the feeds and the
// getComments thread header all show it. This is the service half of that contract;
// TestPostGetVisibility_AuthorSeesOwnPendingPost is the SQL half.
func TestGetPosts_ThreadsViewerIntoVisibilityGate(t *testing.T) {
	uri := didPostURI("v1")

	t.Run("an authenticated viewer reaches the gate", func(t *testing.T) {
		repo := &mockRepository{}
		s := &postService{repo: repo}

		if _, err := s.GetPosts(context.Background(), GetPostsRequest{
			URIs:      []string{uri},
			ViewerDID: "did:plc:theviewer",
		}); err != nil {
			t.Fatalf("GetPosts returned error: %v", err)
		}
		if repo.getViewsByURIsCalls != 1 {
			t.Fatalf("GetViewsByURIs called %d times, want 1", repo.getViewsByURIsCalls)
		}
		if repo.gotViewsViewerDID != "did:plc:theviewer" {
			t.Errorf("GetViewsByURIs got viewer %q, want %q — post.get must gate on the caller's own viewer, "+
				"or an author is told their own pending post does not exist",
				repo.gotViewsViewerDID, "did:plc:theviewer")
		}
	})

	t.Run("an anonymous caller is the explicit fail-closed empty viewer", func(t *testing.T) {
		repo := &mockRepository{}
		s := &postService{repo: repo}

		if _, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{uri}}); err != nil {
			t.Fatalf("GetPosts returned error: %v", err)
		}
		if repo.gotViewsViewerDID != "" {
			t.Errorf("GetViewsByURIs got viewer %q, want \"\" for an anonymous read", repo.gotViewsViewerDID)
		}
	})
}

// TestGetPosts_ViewerBlocksAuthor verifies that when an authenticated viewer has blocked
// a post's author, that post comes back as a blockedPost marker (blockedBy "author")
// while posts by unblocked authors and not-found URIs are unaffected, and request order
// is preserved. This keeps permalink/cold-load reads consistent with feed/timeline.
func TestGetPosts_ViewerBlocksAuthor(t *testing.T) {
	const blockedAuthor = "did:plc:blockedauthor"
	const okAuthor = "did:plc:okauthor"

	blockedURI := didPostURI("blocked1")
	okURI := didPostURI("ok1")
	missingURI := didPostURI("missing1")

	repo := &mockRepository{
		getViewsByURIsFunc: func(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error) {
			return map[string]*PostView{
				blockedURI: viewWithAuthor(blockedURI, blockedAuthor),
				okURI:      viewWithAuthor(okURI, okAuthor),
			}, nil
		},
	}
	checker := &fakeBlockChecker{blocked: map[string]bool{blockedAuthor: true}}
	s := &postService{repo: repo, blockChecker: checker}

	results, err := s.GetPosts(context.Background(), GetPostsRequest{
		URIs:      []string{blockedURI, okURI, missingURI},
		ViewerDID: "did:plc:viewer",
	})
	if err != nil {
		t.Fatalf("GetPosts returned error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// [0] blocked author -> blockedPost marker, no PostView leaked
	if results[0].Post != nil || results[0].NotFound != nil || results[0].Blocked == nil {
		t.Fatalf("results[0] expected Blocked, got %+v", results[0])
	}
	b := results[0].Blocked
	if b.URI != blockedURI || !b.Blocked || b.BlockedBy != "author" {
		t.Errorf("results[0].Blocked = %+v, want {URI:%q, Blocked:true, BlockedBy:author}", b, blockedURI)
	}
	if b.Author == nil || b.Author.DID != blockedAuthor {
		t.Errorf("results[0].Blocked.Author = %+v, want DID %q", b.Author, blockedAuthor)
	}

	// [1] unblocked author -> full postView
	if results[1].Post == nil || results[1].Blocked != nil {
		t.Fatalf("results[1] expected Post, got %+v", results[1])
	}
	if results[1].Post.URI != okURI {
		t.Errorf("results[1].Post.URI = %q, want %q", results[1].Post.URI, okURI)
	}

	// [2] absent URI -> notFoundPost (block filtering does not touch not-found slots)
	if results[2].NotFound == nil || results[2].Post != nil || results[2].Blocked != nil {
		t.Fatalf("results[2] expected NotFound, got %+v", results[2])
	}

	// The block lookup must have run with the viewer as blocker.
	if !checker.called || checker.gotBlocker != "did:plc:viewer" {
		t.Errorf("AreBlocked called=%v blocker=%q, want called with did:plc:viewer", checker.called, checker.gotBlocker)
	}
}

// TestGetPosts_DedupesAuthorDIDsForBlockCheck verifies the block lookup batches over
// unique author DIDs (duplicate authors are not queried twice).
func TestGetPosts_DedupesAuthorDIDsForBlockCheck(t *testing.T) {
	const author = "did:plc:sameauthor"
	uri1, uri2 := didPostURI("a"), didPostURI("b")

	repo := &mockRepository{
		getViewsByURIsFunc: func(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error) {
			return map[string]*PostView{
				uri1: viewWithAuthor(uri1, author),
				uri2: viewWithAuthor(uri2, author),
			}, nil
		},
	}
	checker := &fakeBlockChecker{}
	s := &postService{repo: repo, blockChecker: checker}

	if _, err := s.GetPosts(context.Background(), GetPostsRequest{
		URIs:      []string{uri1, uri2},
		ViewerDID: "did:plc:viewer",
	}); err != nil {
		t.Fatalf("GetPosts returned error: %v", err)
	}
	if len(checker.gotDIDs) != 1 || checker.gotDIDs[0] != author {
		t.Errorf("AreBlocked got DIDs %v, want exactly [%q]", checker.gotDIDs, author)
	}
}

// TestGetPosts_SkipsBlockFilter verifies block enforcement is skipped (the checker is
// never called, posts are returned as-is) when there is no authenticated viewer or no
// block checker wired.
func TestGetPosts_SkipsBlockFilter(t *testing.T) {
	const author = "did:plc:author"
	uri := didPostURI("p1")
	newRepo := func() *mockRepository {
		return &mockRepository{
			getViewsByURIsFunc: func(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error) {
				return map[string]*PostView{uri: viewWithAuthor(uri, author)}, nil
			},
		}
	}

	t.Run("no viewer DID -> checker not called", func(t *testing.T) {
		checker := &fakeBlockChecker{blocked: map[string]bool{author: true}}
		s := &postService{repo: newRepo(), blockChecker: checker}
		results, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{uri}})
		if err != nil {
			t.Fatalf("GetPosts returned error: %v", err)
		}
		if results[0].Post == nil || results[0].Blocked != nil {
			t.Fatalf("expected Post (no filtering without viewer), got %+v", results[0])
		}
		if checker.called {
			t.Error("AreBlocked should not be called without a viewer DID")
		}
	})

	t.Run("nil block checker -> no filtering", func(t *testing.T) {
		s := &postService{repo: newRepo()} // blockChecker nil
		results, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{uri}, ViewerDID: "did:plc:viewer"})
		if err != nil {
			t.Fatalf("GetPosts returned error: %v", err)
		}
		if results[0].Post == nil || results[0].Blocked != nil {
			t.Fatalf("expected Post (no checker wired), got %+v", results[0])
		}
	})
}

// TestGetPosts_BlockCheckErrorFailsClosed verifies that a block-lookup failure fails the
// whole request rather than surfacing posts that may be blocked (fail closed).
func TestGetPosts_BlockCheckErrorFailsClosed(t *testing.T) {
	uri := didPostURI("p1")
	repo := &mockRepository{
		getViewsByURIsFunc: func(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error) {
			return map[string]*PostView{uri: viewWithAuthor(uri, "did:plc:author")}, nil
		},
	}
	checker := &fakeBlockChecker{err: errors.New("db down")}
	s := &postService{repo: repo, blockChecker: checker}

	results, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{uri}, ViewerDID: "did:plc:viewer"})
	if err == nil {
		t.Fatalf("expected error when block check fails, got results %+v", results)
	}
}

// TestGetPosts_RemovedMarkers is the tombstone matrix of post.get's removal path.
//
// A post the visibility predicate hides is a notFoundPost by default; it is upgraded
// to a #removedPost carrying the moderation code ONLY when the post's OWN community
// removed it and its author has not withdrawn it. Three things have to hold at once
// and each has a distinct failure mode:
//
//   - the fork oracle: a removal published by a community that merely FORKED the post
//     must not become a verdict about the post's own community. Dropping the
//     community comparison would let any community publish a takedown notice for a
//     post it does not host.
//   - the withdrawal rule: a soft-deleted post is GONE, not a tombstone. Emitting a
//     moderation reason for a post its own author took down advertises both the
//     removal and the post's continued existence.
//   - honesty about failure: a lookup that FAILED is not "not removed". Both lookups
//     used to be best-effort, so a database blip silently changed which union member
//     a standing removal produced — unfalsifiable from the wire, and different on
//     every retry.
func TestGetPosts_RemovedMarkers(t *testing.T) {
	const ownCommunity = "did:plc:owncommunity"
	const forkCommunity = "did:plc:forkcommunity"
	uri := didPostURI("rm1")
	code := "rule-violation"

	rawRow := func(deleted bool) map[string]*Post {
		p := &Post{URI: uri, CommunityDID: ownCommunity}
		if deleted {
			at := time.Now()
			p.DeletedAt = &at
		}
		return map[string]*Post{uri: p}
	}
	admission := func(communityDID string, status AdmissionStatus, decisionCode *string) []*Admission {
		return []*Admission{{CommunityDID: communityDID, PostURI: uri, Status: status, DecisionCode: decisionCode}}
	}

	tests := []struct {
		name          string
		admissions    map[string][]*Admission
		admissionsErr error
		rawRows       map[string]*Post
		rawRowsErr    error
		wantErr       bool
		wantRemoved   bool
		wantCode      string
		why           string
	}{
		{
			name:       "pending in its own community is a plain notFound",
			admissions: map[string][]*Admission{uri: admission(ownCommunity, AdmissionStatusPending, nil)},
			rawRows:    rawRow(false),
			why: "a post awaiting a decision has no verdict to report; answering #removedPost would invent a " +
				"moderation act, and answering anything but notFound tells the public a pending post exists",
		},
		{
			name:        "removed by its OWN community is a tombstone carrying the code",
			admissions:  map[string][]*Admission{uri: admission(ownCommunity, AdmissionStatusRemoved, &code)},
			rawRows:     rawRow(false),
			wantRemoved: true,
			wantCode:    code,
			why:         "a removed post is a tombstone the author is owed the reason for, not a blank permalink",
		},
		{
			name:       "removed by a DIFFERENT community is NOT a tombstone (the fork oracle)",
			admissions: map[string][]*Admission{uri: admission(forkCommunity, AdmissionStatusRemoved, &code)},
			rawRows:    rawRow(false),
			why: "the removal belongs to a community that FORKED the post, not to the community that hosts it. " +
				"Dropping the community comparison lets any community publish a takedown verdict about a post " +
				"it does not host — the same fork hazard the read predicate's join key closes",
		},
		{
			name:       "soft-deleted AND removed is a notFound, not a tombstone",
			admissions: map[string][]*Admission{uri: admission(ownCommunity, AdmissionStatusRemoved, &code)},
			rawRows:    rawRow(true),
			why: "the author withdrew the post. Rendering the moderation reason anyway advertises both the " +
				"takedown and the fact that the post still exists in the index",
		},
		{
			name:       "an unindexed URI stays a notFound",
			admissions: map[string][]*Admission{uri: admission(ownCommunity, AdmissionStatusRemoved, &code)},
			rawRows:    map[string]*Post{},
			why: "no indexed row means there is no community to compare the removal against — the admission is " +
				"about a post this AppView has never seen",
		},
		{
			name:          "an admission lookup FAILURE is an error, not a notFound",
			admissionsErr: errors.New("admissions db down"),
			rawRows:       rawRow(false),
			wantErr:       true,
			why: "collapsing the failure into notFound makes the answer depend on database health: the same " +
				"request returns a different union member on every retry and the client cannot tell",
		},
		{
			name:       "a post-row lookup FAILURE is an error, not a notFound",
			admissions: map[string][]*Admission{uri: admission(ownCommunity, AdmissionStatusRemoved, &code)},
			rawRowsErr: errors.New("posts db down"),
			wantErr:    true,
			why:        "same reason: 'we could not find out' is not 'it was not removed'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &mockRepository{rawRows: tt.rawRows, rawRowsErr: tt.rawRowsErr}
			admissions := &fakeAdmissions{
				rec:           &engineRecorder{},
				byPostURIs:    tt.admissions,
				byPostURIsErr: tt.admissionsErr,
			}
			s := &postService{repo: repo, admissions: admissions}

			results, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: []string{uri}})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got results %+v — %s", results, tt.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetPosts returned error: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}

			got := results[0]
			if got.Post != nil {
				t.Fatalf("a hidden post must never come back as a postView, got %+v", got.Post)
			}
			if !tt.wantRemoved {
				if got.Removed != nil {
					t.Fatalf("expected notFound, got removed %+v — %s", got.Removed, tt.why)
				}
				if got.NotFound == nil || got.NotFound.URI != uri || !got.NotFound.NotFound {
					t.Fatalf("expected notFound{%q,true}, got %+v — %s", uri, got.NotFound, tt.why)
				}
				return
			}
			if got.NotFound != nil {
				t.Fatalf("expected removed, got notFound %+v — %s", got.NotFound, tt.why)
			}
			if got.Removed == nil || got.Removed.URI != uri || !got.Removed.Removed {
				t.Fatalf("expected removed{%q,true}, got %+v — %s", uri, got.Removed, tt.why)
			}
			if got.Removed.Code != tt.wantCode {
				t.Errorf("removal code = %q, want %q — %s", got.Removed.Code, tt.wantCode, tt.why)
			}
		})
	}
}

// TestGetPosts_RemovalPathBatchesPostLookups pins that the removal path resolves the
// whole absent set in ONE round trip.
//
// post.get accepts a caller-controlled list of up to MaxGetPostsURIs URIs, and the
// removal path runs over exactly the ones the visibility predicate already refused —
// so a per-URI lookup there is an unauthenticated N+1 whose multiplier the caller
// chooses. The admission lookup was already batched; the post lookup was not.
func TestGetPosts_RemovalPathBatchesPostLookups(t *testing.T) {
	uris := make([]string, 0, MaxGetPostsURIs)
	for i := 0; i < MaxGetPostsURIs; i++ {
		uris = append(uris, didPostURI(fmt.Sprintf("batch%d", i)))
	}

	repo := &mockRepository{rawRows: map[string]*Post{}}
	admissions := &fakeAdmissions{rec: &engineRecorder{}}
	s := &postService{repo: repo, admissions: admissions}

	if _, err := s.GetPosts(context.Background(), GetPostsRequest{URIs: uris}); err != nil {
		t.Fatalf("GetPosts returned error: %v", err)
	}
	if repo.rawBatchCalls != 1 {
		t.Errorf("raw post lookup ran %d times for %d absent URIs, want exactly 1 — post.get is public and the "+
			"URI list is caller-controlled, so a per-URI round trip is an N+1 with a caller-chosen multiplier",
			repo.rawBatchCalls, len(uris))
	}
	admissionLookups := 0
	for _, call := range admissions.rec.calls {
		if call == "GetByPostURIs" {
			admissionLookups++
		}
	}
	if admissionLookups != 1 {
		t.Errorf("admission lookup ran %d times, want exactly 1", admissionLookups)
	}
}

// TestPostResult_Member verifies the union accessor returns exactly the populated member
// and reports the empty (invalid) result so callers can avoid emitting a null union entry.
func TestPostResult_Member(t *testing.T) {
	view := &PostView{URI: "at://x"}
	tests := []struct {
		name   string
		result *PostResult
		want   interface{}
		wantOK bool
	}{
		{"post", foundResult(view), view, true},
		{"not found", notFoundResult("at://missing"), nil, true},
		{"blocked", blockedByAuthorResult("at://b", "did:plc:a"), nil, true},
		{"empty is invalid", &PostResult{}, nil, false},
		// More than one member set is an assembly bug: Member must report it as invalid
		// (not silently pick one), so the handler returns a 500 instead of emitting an
		// ambiguous union entry.
		{"multiple members is invalid", &PostResult{Post: view, NotFound: &NotFoundPost{URI: "at://x", NotFound: true}}, nil, false},
		{"all three members is invalid", &PostResult{
			Post:     view,
			Blocked:  &BlockedPost{URI: "at://b", Blocked: true},
			NotFound: &NotFoundPost{URI: "at://n", NotFound: true},
		}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			member, ok := tt.result.Member()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if member != nil {
					t.Errorf("member = %v, want nil for invalid result", member)
				}
				return
			}
			if member == nil {
				t.Fatal("member = nil for a valid result")
			}
			// For the post case the accessor must return the exact PostView pointer.
			if tt.want != nil && member != tt.want {
				t.Errorf("member = %v, want %v", member, tt.want)
			}
		})
	}
}
