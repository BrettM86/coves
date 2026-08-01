//go:build integration

package jetstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
)

// The hostedBy claim is the community consumer's trust boundary. A community
// record is written to its OWN repository and says which instance hosts it, so
// nothing stops a hostile repo from claiming `hostedBy: did:web:nintendo.com`
// while its handle sits on some other domain. verifyHostedByClaim is what
// refuses that, and these tests are its coverage:
//
//   - the domain of the handle must equal the domain in the did:web;
//   - hostedBy must be a did:web at all (a did:plc names an account, not a
//     host, so there is no domain to compare against);
//   - the registrable-domain extraction has to survive multi-part public
//     suffixes, or `c-gaming.coves.co.uk` reads as hosted by `co.uk` and every
//     .co.uk instance can impersonate every other.
//
// They run against real Postgres because half of what is being asserted is a
// negative about the index: a rejected event must leave NO community row
// behind. A stubbed repository can only report what it was told, not what
// survived.
//
// # WHY VERIFICATION IS PASSED AS AN ARGUMENT, NOT READ FROM THE ENVIRONMENT
//
// The deployed AppView reads SKIP_DID_WEB_VERIFICATION, and both .env.dev and
// .env.ci set it to true — so in CI the wired-up consumer performs no
// verification at all. These tests do not go through that wiring: they
// construct CommunityEventConsumer directly and pass skipVerification
// explicitly, so each case pins the behaviour it names regardless of how the
// surrounding stack is configured. That independence is the point; a test that
// inherited the CI setting would assert nothing here.
//
// The cases that DO enable verification are still hermetic. Every one of them
// fails on the did:web format check or the domain comparison, both of which run
// before verifyDIDDocument would fetch anything, so no DID document is ever
// requested over the network.

// TestHostedByVerification_DomainMatching covers the domain comparison itself,
// and what the index looks like on either side of it.
func TestHostedByVerification_DomainMatching(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("rejects community with mismatched hostedBy domain", func(t *testing.T) {
		// Verification ENABLED. The identity resolver is nil because the
		// consumer derives the handle domain from the record, not by resolving
		// anything.
		consumer := NewCommunityEventConsumer(repo, "did:web:coves.social", false, nil)

		uniqueSuffix := testkit.UniqueID(t)
		communityDID := fixtures.DID(uniqueSuffix)
		uniqueHandle := fmt.Sprintf("c-gaming%s.coves.social", uniqueSuffix)

		// The attack: a coves.social handle claiming to be hosted by Nintendo.
		event := &JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      uniqueHandle,
					"name":        "gaming",
					"displayName": "Nintendo Gaming",
					"description": "Fake Nintendo community",
					"createdBy":   "did:plc:attacker123",
					"hostedBy":    "did:web:nintendo.com", // spoofed
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected verification error for mismatched hostedBy domain, got nil")
		}

		// Rejection must be permanent: the mismatch is inherent to the record,
		// so retrying it forever only fills the dead-letter queue.
		if !strings.Contains(err.Error(), "doesn't match hostedBy domain") {
			t.Errorf("Expected a domain-mismatch rejection, got: %v", err)
		}

		// The negative that matters: nothing was indexed.
		if _, getErr := repo.GetByDID(ctx, communityDID); getErr == nil {
			t.Fatal("Community should not have been indexed, but was found in database")
		}
	})

	t.Run("accepts community with matching hostedBy domain", func(t *testing.T) {
		// Verification SKIPPED. This case is about the happy path through
		// indexing — the handle and the hostedBy agree, and there is no real
		// did:web document to fetch for a fixture domain.
		consumer := NewCommunityEventConsumer(repo, "did:web:coves.social", true, nil)

		uniqueSuffix := testkit.UniqueID(t)
		communityDID := fixtures.DID(uniqueSuffix)
		uniqueHandle := fmt.Sprintf("c-gaming%s.coves.social", uniqueSuffix)

		event := &JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      uniqueHandle,
					"name":        "gaming",
					"displayName": "Gaming Community",
					"description": "Legitimate coves.social community",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:coves.social",
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		if err := consumer.HandleEvent(ctx, event); err != nil {
			t.Fatalf("Expected verification to succeed, got error: %v", err)
		}

		community, getErr := repo.GetByDID(ctx, communityDID)
		if getErr != nil {
			t.Fatalf("Community should have been indexed: %v", getErr)
		}
		// The claim is persisted verbatim: downstream federation decisions read
		// hosted_by_did, so a dropped or rewritten value would silently move
		// the community to another instance.
		if community.HostedByDID != "did:web:coves.social" {
			t.Errorf("Expected hostedByDID 'did:web:coves.social', got '%s'", community.HostedByDID)
		}
	})

	t.Run("rejects hostedBy with non-did:web format", func(t *testing.T) {
		// Verification ENABLED. A did:plc identifies an account rather than a
		// host, so there is no domain to compare and the claim is unverifiable
		// by construction — it must be refused rather than trusted.
		consumer := NewCommunityEventConsumer(repo, "did:web:coves.social", false, nil)

		uniqueSuffix := testkit.UniqueID(t)
		communityDID := fixtures.DID(uniqueSuffix)
		uniqueHandle := fmt.Sprintf("c-gaming%s.coves.social", uniqueSuffix)

		event := &JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      uniqueHandle,
					"name":        "gaming",
					"displayName": "Test Community",
					"description": "Test",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:plc:xyz123", // must be did:web
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		err := consumer.HandleEvent(ctx, event)
		if err == nil {
			t.Fatal("Expected verification error for non-did:web hostedBy, got nil")
		}
		if !strings.Contains(err.Error(), "did:web") {
			t.Errorf("Expected a did:web method rejection, got: %v", err)
		}

		if _, getErr := repo.GetByDID(ctx, communityDID); getErr == nil {
			t.Fatal("Community should not have been indexed, but was found in database")
		}
	})

	t.Run("skip verification flag bypasses all checks", func(t *testing.T) {
		// This is the dev/CI configuration (SKIP_DID_WEB_VERIFICATION=true) seen
		// from the inside: with the flag on, a record that the case above
		// rejects is indexed. Pinning it here is what stops the flag from
		// quietly becoming a no-op — and what documents the exposure that comes
		// with enabling it.
		consumer := NewCommunityEventConsumer(repo, "did:web:coves.social", true, nil)

		uniqueSuffix := testkit.UniqueID(t)
		communityDID := fixtures.DID(uniqueSuffix)
		uniqueHandle := fmt.Sprintf("c-gaming%s.example.com", uniqueSuffix)

		event := &JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      uniqueHandle,
					"name":        "gaming",
					"displayName": "Test",
					"description": "Test",
					"createdBy":   "did:plc:user123",
					"hostedBy":    "did:web:nintendo.com", // mismatched, but unchecked
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		if err := consumer.HandleEvent(ctx, event); err != nil {
			t.Fatalf("Expected success with skipVerification=true, got error: %v", err)
		}

		if _, getErr := repo.GetByDID(ctx, communityDID); getErr != nil {
			t.Fatalf("Community should have been indexed: %v", getErr)
		}
	})
}

// TestBidirectionalDIDVerification exercises the indexing path against handles
// on a domain served by a local DID document.
//
// CAVEAT, and it is a large one: both cases below construct the consumer with
// skipVerification=true, so the mock server is never contacted and the
// alsoKnownAs check the test is named for never runs. The second case asserts
// that a DID document WITHOUT alsoKnownAs is accepted, which is the opposite of
// the production requirement. What the cases actually prove is that a handle
// and hostedBy on a host:port domain index correctly and round-trip their
// hostedBy claim.
//
// The reason it is shaped this way is that httptest.NewTLSServer issues a
// self-signed certificate, so a consumer with verification enabled would fail
// on TLS rather than on alsoKnownAs, and the failure would say nothing about
// bidirectional verification either. Making these cases mean what their names
// say needs the verifier's HTTP client to be injectable so the test can hand it
// the mock server's certificate pool — a change to production code that is out
// of scope for relocating the file.
func TestBidirectionalDIDVerification(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	t.Run("indexes a community whose domain serves a DID document with alsoKnownAs", func(t *testing.T) {
		mockServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/did.json" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, `{
					"id": "did:web:example.com",
					"alsoKnownAs": ["at://example.com"],
					"verificationMethod": [],
					"service": []
				}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer mockServer.Close()

		// The domain is the server's host:port, which is what a did:web would
		// name for a non-standard port.
		mockDomain := strings.TrimPrefix(mockServer.URL, "https://")

		consumer := NewCommunityEventConsumer(repo, fmt.Sprintf("did:web:%s", mockDomain), true, nil)

		uniqueSuffix := testkit.UniqueID(t)
		communityDID := fixtures.DID(uniqueSuffix)
		uniqueHandle := fmt.Sprintf("c-gaming%s.%s", uniqueSuffix, mockDomain)

		event := &JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      uniqueHandle,
					"name":        "gaming",
					"displayName": "Gaming Community",
					"description": "Test community with bidirectional verification",
					"createdBy":   "did:plc:user123",
					"hostedBy":    fmt.Sprintf("did:web:%s", mockDomain),
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		if err := consumer.HandleEvent(ctx, event); err != nil {
			t.Fatalf("Expected verification to succeed, got error: %v", err)
		}

		community, getErr := repo.GetByDID(ctx, communityDID)
		if getErr != nil {
			t.Fatalf("Community should have been indexed: %v", getErr)
		}
		if community.HostedByDID != fmt.Sprintf("did:web:%s", mockDomain) {
			t.Errorf("Expected hostedByDID 'did:web:%s', got '%s'", mockDomain, community.HostedByDID)
		}
	})

	t.Run("indexes a community whose domain serves a DID document without alsoKnownAs", func(t *testing.T) {
		mockServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/did.json" {
				// No alsoKnownAs. With verification enabled this is what a
				// bidirectional check would reject; with it skipped, nobody
				// looks.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, `{
					"id": "did:web:example.com",
					"verificationMethod": [],
					"service": []
				}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer mockServer.Close()

		mockDomain := strings.TrimPrefix(mockServer.URL, "https://")

		consumer := NewCommunityEventConsumer(repo, fmt.Sprintf("did:web:%s", mockDomain), true, nil)

		uniqueSuffix := testkit.UniqueID(t)
		communityDID := fixtures.DID(uniqueSuffix)
		uniqueHandle := fmt.Sprintf("c-gaming%s.%s", uniqueSuffix, mockDomain)

		event := &JetstreamEvent{
			Did:    communityDID,
			TimeUS: time.Now().UnixMicro(),
			Kind:   "commit",
			Commit: &CommitEvent{
				Rev:        "rev123",
				Operation:  "create",
				Collection: "social.coves.community.profile",
				RKey:       "self",
				CID:        "bafy123abc",
				Record: map[string]interface{}{
					"handle":      uniqueHandle,
					"name":        "gaming",
					"displayName": "Gaming Community",
					"description": "Test community without alsoKnownAs",
					"createdBy":   "did:plc:user123",
					"hostedBy":    fmt.Sprintf("did:web:%s", mockDomain),
					"visibility":  "public",
					"federation": map[string]interface{}{
						"allowExternalDiscovery": true,
					},
					"memberCount":     0,
					"subscriberCount": 0,
					"createdAt":       time.Now().Format(time.RFC3339),
				},
			},
		}

		if err := consumer.HandleEvent(ctx, event); err != nil {
			t.Fatalf("Expected verification to succeed with skipVerification:true, got error: %v", err)
		}
	})
}

// TestExtractDomainFromHandle drives extractDomainFromHandle through the
// consumer, one handle shape per case.
//
// The multi-part public suffixes are the reason this table exists. Extracting a
// registrable domain by taking the last two labels turns
// `c-gaming.coves.co.uk` into `co.uk`, at which point any .co.uk instance can
// claim to host any other .co.uk community. The negative case with
// `did:web:co.uk` pins that specific error.
//
// Verification is enabled exactly for the cases expected to FAIL, so the domain
// comparison actually runs; the cases expected to succeed use fixture domains
// with no DID document to serve, so they skip it. Either way no case reaches
// the network: a domain mismatch is decided before the document would be
// fetched.
func TestExtractDomainFromHandle(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := postgres.NewCommunityRepository(db)
	ctx := context.Background()

	testCases := []struct {
		name          string
		handle        string
		hostedByDID   string
		shouldSucceed bool
	}{
		{
			name:          "DNS-style handle with subdomain",
			handle:        "c-gaming.coves.social",
			hostedByDID:   "did:web:coves.social",
			shouldSucceed: true,
		},
		{
			name:          "Simple two-part domain",
			handle:        "gaming.coves.social",
			hostedByDID:   "did:web:coves.social",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part subdomain",
			handle:        "c-gaming.test.example.com",
			hostedByDID:   "did:web:example.com",
			shouldSucceed: true,
		},
		{
			name:          "Mismatched domain",
			handle:        "c-gaming.coves.social",
			hostedByDID:   "did:web:example.com",
			shouldSucceed: false,
		},
		{
			name:          "Multi-part TLD: .co.uk",
			handle:        "c-gaming.coves.co.uk",
			hostedByDID:   "did:web:coves.co.uk",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part TLD: .com.au",
			handle:        "c-gaming.example.com.au",
			hostedByDID:   "did:web:example.com.au",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part TLD: Reject incorrect .co.uk extraction",
			handle:        "c-gaming.coves.co.uk",
			hostedByDID:   "did:web:co.uk", // wrong: should be coves.co.uk
			shouldSucceed: false,
		},
		{
			name:          "Multi-part TLD: .org.uk",
			handle:        "c-gaming.myinstance.org.uk",
			hostedByDID:   "did:web:myinstance.org.uk",
			shouldSucceed: true,
		},
		{
			name:          "Multi-part TLD: .ac.uk",
			handle:        "c-gaming.university.ac.uk",
			hostedByDID:   "did:web:university.ac.uk",
			shouldSucceed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			skipVerification := tc.shouldSucceed
			consumer := NewCommunityEventConsumer(repo, "did:web:coves.social", skipVerification, nil)

			uniqueSuffix := testkit.UniqueID(t)
			communityDID := fixtures.DID(uniqueSuffix)

			event := &JetstreamEvent{
				Did:    communityDID,
				TimeUS: time.Now().UnixMicro(),
				Kind:   "commit",
				Commit: &CommitEvent{
					Rev:        "rev123",
					Operation:  "create",
					Collection: "social.coves.community.profile",
					RKey:       "self",
					CID:        "bafy123abc",
					Record: map[string]interface{}{
						"handle":      tc.handle,
						"name":        "test",
						"displayName": "Test",
						"description": "Test",
						"createdBy":   "did:plc:user123",
						"hostedBy":    tc.hostedByDID,
						"visibility":  "public",
						"federation": map[string]interface{}{
							"allowExternalDiscovery": true,
						},
						"memberCount":     0,
						"subscriberCount": 0,
						"createdAt":       time.Now().Format(time.RFC3339),
					},
				},
			}

			err := consumer.HandleEvent(ctx, event)
			if tc.shouldSucceed && err != nil {
				t.Errorf("Expected success for %s, got error: %v", tc.handle, err)
			} else if !tc.shouldSucceed && err == nil {
				t.Errorf("Expected failure for %s, got success", tc.handle)
			}
		})
	}
}
