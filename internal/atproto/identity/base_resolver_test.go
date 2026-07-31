package identity

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	indigoIdentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The base resolver: the layer between a client-supplied string and Indigo's
// directory, and the layer that decides what a failure MEANS.
//
// Identity resolution is the first thing that happens to almost every
// identifier the AppView is handed — a handle in a signup, a DID in a record,
// an at-identifier in a URL — and there are exactly three answers it can give:
// this is not a valid identifier, this identifier does not exist, or resolution
// itself broke. Callers route on the difference. A 400 and a 404 are the user's
// problem; a 502 is ours. The error taxonomy below is therefore the load-bearing
// half of this file, and it is decided by substring-matching the directory's
// error text, which is fragile enough to be worth pinning precisely.
//
// # WHY IN-PACKAGE, AND WHY NO NETWORK
//
// The resolver's collaborator is an indigoIdentity.Directory. Both the type and
// the constructor that injects it are unexported, so the only place a fake
// directory can be handed to a baseResolver is inside the package. The sibling
// identity_cache_test.go is external because it needs testkit and would
// otherwise cycle; nothing here imports anything but Indigo, so there is no
// cycle to avoid and no reason to test through a keyhole.
//
// Nothing in this file leaves the machine. The fake directory answers every
// lookup from a map, and every fixture handle is under .invalid, which RFC 2606
// guarantees resolves nowhere — so a regression that started making real
// lookups would fail rather than quietly succeed against production PLC.

// fakeDirectory is an indigoIdentity.Directory that answers from a map.
//
// The maps are written once at construction and only read afterwards; the call
// logs are written on every lookup, and several tests below share one directory
// across parallel subtests, so the logs are guarded. An unguarded slice append
// here is a genuine data race rather than a theoretical one — the race detector
// caught exactly that when the recording was first added.
type fakeDirectory struct {
	byHandle map[string]*indigoIdentity.Identity
	byDID    map[string]*indigoIdentity.Identity

	// err, when set, is returned by every lookup. Its TEXT is the whole point
	// for the taxonomy tests: baseResolver classifies failures by substring.
	err error

	mu sync.Mutex
	// Every entry point records what it was asked, so a test can say "no
	// lookup happened" and mean it. Recording only purges would let a
	// malformed identifier reach the directory unnoticed — the interesting
	// claim about a rejected identifier is that it never became a DNS query or
	// an HTTPS fetch against somebody else's host.
	looked []string
	purged []string
}

// record logs one call against the directory.
func (d *fakeDirectory) record(log *[]string, call string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	*log = append(*log, call)
}

// lookups returns the resolution calls made so far, in order.
func (d *fakeDirectory) lookups() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.looked...)
}

// purges returns the purge calls made so far, in order.
func (d *fakeDirectory) purges() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.purged...)
}

func (d *fakeDirectory) Lookup(_ context.Context, atid syntax.AtIdentifier) (*indigoIdentity.Identity, error) {
	d.record(&d.looked, "Lookup("+atid.String()+")")
	if d.err != nil {
		return nil, d.err
	}
	if did, err := atid.AsDID(); err == nil {
		return d.lookupDID(did.String())
	}
	handle, err := atid.AsHandle()
	if err != nil {
		return nil, errors.New("not found: neither a handle nor a DID")
	}
	if ident, ok := d.byHandle[handle.String()]; ok {
		return ident, nil
	}
	return nil, errors.New("not found: " + handle.String())
}

func (d *fakeDirectory) LookupHandle(_ context.Context, handle syntax.Handle) (*indigoIdentity.Identity, error) {
	d.record(&d.looked, "LookupHandle("+handle.String()+")")
	if d.err != nil {
		return nil, d.err
	}
	if ident, ok := d.byHandle[handle.String()]; ok {
		return ident, nil
	}
	return nil, errors.New("not found: " + handle.String())
}

func (d *fakeDirectory) LookupDID(_ context.Context, did syntax.DID) (*indigoIdentity.Identity, error) {
	d.record(&d.looked, "LookupDID("+did.String()+")")
	if d.err != nil {
		return nil, d.err
	}
	return d.lookupDID(did.String())
}

func (d *fakeDirectory) lookupDID(did string) (*indigoIdentity.Identity, error) {
	if ident, ok := d.byDID[did]; ok {
		return ident, nil
	}
	return nil, errors.New("not found: " + did)
}

func (d *fakeDirectory) Purge(_ context.Context, atid syntax.AtIdentifier) error {
	d.record(&d.purged, atid.String())
	return nil
}

const (
	fixtureHandle = "alice.invalid"
	fixtureDID    = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	fixturePDS    = "https://pds.invalid"
)

// indigoIdentityFixture is what a healthy Indigo lookup returns: a DID, a
// bidirectionally-verified handle, and a services map carrying the PDS.
func indigoIdentityFixture(t *testing.T, pdsURL string) *indigoIdentity.Identity {
	t.Helper()
	did, err := syntax.ParseDID(fixtureDID)
	require.NoError(t, err)
	handle, err := syntax.ParseHandle(fixtureHandle)
	require.NoError(t, err)

	ident := &indigoIdentity.Identity{DID: did, Handle: handle}
	if pdsURL != "" {
		ident.Services = map[string]indigoIdentity.ServiceEndpoint{
			"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: pdsURL},
		}
	}
	return ident
}

func newFakeBackedResolver(t *testing.T, pdsURL string) (*baseResolver, *fakeDirectory) {
	t.Helper()
	ident := indigoIdentityFixture(t, pdsURL)
	dir := &fakeDirectory{
		byHandle: map[string]*indigoIdentity.Identity{fixtureHandle: ident},
		byDID:    map[string]*indigoIdentity.Identity{fixtureDID: ident},
	}
	return &baseResolver{directory: dir}, dir
}

func TestBaseResolver_ResolveReturnsTheWholeIdentity(t *testing.T) {
	t.Parallel()
	resolver, _ := newFakeBackedResolver(t, fixturePDS)

	for _, identifier := range []string{fixtureHandle, fixtureDID, "  " + fixtureHandle + "  "} {
		t.Run(identifier, func(t *testing.T) {
			t.Parallel()
			got, err := resolver.Resolve(context.Background(), identifier)
			require.NoError(t, err)

			assert.Equal(t, fixtureDID, got.DID)
			assert.Equal(t, fixtureHandle, got.Handle,
				"the handle comes back from the DID document, not from what the caller typed — that is "+
					"what makes it bidirectionally verified rather than merely echoed")
			assert.Equal(t, fixturePDS, got.PDSURL,
				"without the PDS endpoint the identity is unusable: every read and write against this "+
					"account needs a host to send it to")
			assert.False(t, got.ResolvedAt.IsZero(), "the cache TTL is measured from this")
		})
	}
}

// TestBaseResolver_MethodIsAlwaysHTTPS pins something that is currently a lie of
// omission rather than a defect: Indigo does not report whether it answered from
// DNS or from a .well-known fetch, so the base resolver stamps every result
// MethodHTTPS. MethodDNS exists in the type and nothing ever produces it.
//
// It is asserted because the field is not decorative. The caching resolver
// overwrites Method with MethodCache on a hit, and tests/live's
// purgeIdentityCache uses "Method != cache" to prove a purge really re-resolved
// — a claim that only holds while a fresh resolution is reliably not "cache".
func TestBaseResolver_MethodIsAlwaysHTTPS(t *testing.T) {
	t.Parallel()
	resolver, _ := newFakeBackedResolver(t, fixturePDS)

	got, err := resolver.Resolve(context.Background(), fixtureHandle)
	require.NoError(t, err)
	assert.Equal(t, MethodHTTPS, got.Method,
		"a freshly resolved identity must not be labelled as coming from the cache, or every "+
			"cache-bypass assertion in the tree becomes unfalsifiable")
	assert.NotEqual(t, MethodCache, got.Method)
}

func TestBaseResolver_RejectsIdentifiersItCannotParse(t *testing.T) {
	t.Parallel()
	resolver, dir := newFakeBackedResolver(t, fixturePDS)

	for _, tc := range []struct {
		name       string
		identifier string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"a sentence", "not a handle"},
		{"a bare word with no dot", "alice"},
		{"a URL", "https://alice.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolver.Resolve(context.Background(), tc.identifier)

			var invalid *ErrInvalidIdentifier
			require.ErrorAsf(t, err, &invalid,
				"%q must be rejected as malformed rather than looked up: callers turn this into a 400, "+
					"and anything else turns a client's typo into a network round trip and a 502",
				tc.identifier)
			assert.Contains(t, invalid.Error(), "invalid identifier")
			assert.Emptyf(t, dir.lookups(),
				"%q reached the directory. Parsing is the gate in front of the network here, and a "+
					"malformed identifier that gets past it costs a DNS lookup or an HTTPS fetch "+
					"against a host the client made up", tc.identifier)
			assert.Empty(t, dir.purges())
		})
	}
}

// TestBaseResolver_ErrorTaxonomy pins the substring matching that separates
// "this account does not exist" from "resolution broke".
//
// The distinction is made by looking for "not found", "NoRecordsFound" or "404"
// anywhere in the directory's error text. That is brittle by construction — a
// wording change upstream silently reclassifies a 404 as a 502 — which is
// exactly why the accepted forms are enumerated here rather than left to a
// single happy-path case.
func TestBaseResolver_ErrorTaxonomy(t *testing.T) {
	t.Parallel()

	t.Run("absence", func(t *testing.T) {
		t.Parallel()
		for _, message := range []string{
			"not found",
			"handle not found",
			"NoRecordsFound",
			"unexpected status 404",
			"DID not found",
		} {
			t.Run(message, func(t *testing.T) {
				t.Parallel()
				resolver := &baseResolver{directory: &fakeDirectory{err: errors.New(message)}}

				_, err := resolver.Resolve(context.Background(), fixtureHandle)
				var notFound *ErrNotFound
				require.ErrorAsf(t, err, &notFound,
					"%q means the identity is absent, and the caller answers 404. Classified as a "+
						"resolution failure instead it becomes a 502, and a signup with a typo in the "+
						"handle looks like our outage", message)
				assert.Contains(t, notFound.Error(), message,
					"the underlying reason must survive: a bare \"not found\" is unactionable in a log")
			})
		}
	})

	t.Run("breakage", func(t *testing.T) {
		t.Parallel()
		for _, message := range []string{
			"dial tcp: connection refused",
			"context deadline exceeded",
			"unexpected status 500",
			"handle/DID mismatch",
		} {
			t.Run(message, func(t *testing.T) {
				t.Parallel()
				resolver := &baseResolver{directory: &fakeDirectory{err: errors.New(message)}}

				_, err := resolver.Resolve(context.Background(), fixtureHandle)
				var failed *ErrResolutionFailed
				require.ErrorAsf(t, err, &failed,
					"%q is our problem, not the client's. Reported as not-found it would be cached as "+
						"an absence and the account would stay invisible after the outage ended", message)

				var notFound *ErrNotFound
				assert.NotErrorAs(t, err, &notFound)
			})
		}
	})

	// The mismatch case above is the one worth stating out loud: Indigo's
	// ErrHandleMismatch means the handle resolves to a DID whose document
	// disowns it. It is a real, common state (a handle moved) and it lands in
	// the breakage bucket rather than the absence one, so callers surface it as
	// a server error. Pinned rather than endorsed.
}

func TestBaseResolver_ResolveHandle(t *testing.T) {
	t.Parallel()

	t.Run("returns the DID and the PDS a client needs to talk to it", func(t *testing.T) {
		t.Parallel()
		resolver, _ := newFakeBackedResolver(t, fixturePDS)

		did, pdsURL, err := resolver.ResolveHandle(context.Background(), fixtureHandle)
		require.NoError(t, err)
		assert.Equal(t, fixtureDID, did)
		assert.Equal(t, fixturePDS, pdsURL)
	})

	t.Run("propagates the failure rather than an empty pair", func(t *testing.T) {
		t.Parallel()
		resolver, _ := newFakeBackedResolver(t, fixturePDS)

		did, pdsURL, err := resolver.ResolveHandle(context.Background(), "bob.invalid")
		var notFound *ErrNotFound
		require.ErrorAs(t, err, &notFound)
		assert.Empty(t, did, "a caller that ignores the error must not get a usable-looking empty DID")
		assert.Empty(t, pdsURL)
	})
}

func TestBaseResolver_ResolveDID(t *testing.T) {
	t.Parallel()

	t.Run("builds a document carrying the PDS service entry", func(t *testing.T) {
		t.Parallel()
		resolver, _ := newFakeBackedResolver(t, fixturePDS)

		doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
		require.NoError(t, err)
		assert.Equal(t, fixtureDID, doc.DID)
		require.Len(t, doc.Service, 1)
		assert.Equal(t, "#atproto_pds", doc.Service[0].ID)
		assert.Equal(t, "AtprotoPersonalDataServer", doc.Service[0].Type,
			"the type is what a consumer matches on to find the PDS among other services")
		assert.Equal(t, fixturePDS, doc.Service[0].ServiceEndpoint)
	})

	t.Run("omits the service entry for a DID that declares no PDS", func(t *testing.T) {
		t.Parallel()
		resolver, _ := newFakeBackedResolver(t, "")

		doc, err := resolver.ResolveDID(context.Background(), fixtureDID)
		require.NoError(t, err)
		assert.Empty(t, doc.Service,
			"an entry with an empty endpoint would read as \"the PDS is at \\\"\\\"\" — callers check "+
				"for the entry's presence, not for a non-empty URL")
	})

	t.Run("rejects a string that is not a DID before looking anything up", func(t *testing.T) {
		t.Parallel()
		resolver, dir := newFakeBackedResolver(t, fixturePDS)

		for _, notADID := range []string{"", fixtureHandle, "did:", "plc:abc"} {
			_, err := resolver.ResolveDID(context.Background(), notADID)
			var invalid *ErrInvalidIdentifier
			require.ErrorAsf(t, err, &invalid, "%q is not a DID", notADID)
		}
		assert.Empty(t, dir.lookups(), "none of those strings may become a DID-document fetch")
	})

	// Unlike Resolve, ResolveDID has no absence branch: every directory failure
	// becomes ErrResolutionFailed, so a DID that simply does not exist is
	// reported as breakage. Pinned because it is an asymmetry between two
	// methods of one interface, and a caller that switches on ErrNotFound will
	// never match here.
	t.Run("reports a missing DID as a resolution failure, not an absence", func(t *testing.T) {
		t.Parallel()
		resolver, _ := newFakeBackedResolver(t, fixturePDS)

		_, err := resolver.ResolveDID(context.Background(), "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb")
		var failed *ErrResolutionFailed
		require.ErrorAs(t, err, &failed)
		var notFound *ErrNotFound
		assert.NotErrorAs(t, err, &notFound,
			"IF THIS FAILED, ResolveDID gained the absence branch Resolve has. That is an improvement; "+
				"update this test to assert the new taxonomy rather than reverting it")
	})
}

func TestBaseResolver_PurgeIsANoOp(t *testing.T) {
	t.Parallel()
	resolver, dir := newFakeBackedResolver(t, fixturePDS)

	// The base resolver holds nothing, so a purge succeeds without doing
	// anything. It must still succeed: the caching resolver purges its own
	// store and then calls straight through, and an error here would make every
	// purge look like a failure.
	require.NoError(t, resolver.Purge(context.Background(), fixtureHandle))
	assert.Empty(t, dir.lookups(), "a purge is not a resolution and must not become one")
	assert.Empty(t, dir.purges(),
		"the base resolver deliberately does not forward purges to Indigo's directory; if that "+
			"changes, the caching resolver's Purge stops being the whole story")
}

func TestNewBaseResolver_ConfiguresIndigoWithTheGivenDirectory(t *testing.T) {
	t.Parallel()

	// Cheap, but it guards the one wiring mistake that would be invisible until
	// production: a resolver built against the default (public) PLC rather than
	// the one it was handed.
	resolver := newBaseResolver("http://plc.invalid:3002", &http.Client{})
	base, ok := resolver.(*baseResolver)
	require.True(t, ok)
	dir, ok := base.directory.(*indigoIdentity.BaseDirectory)
	require.True(t, ok)
	assert.Equal(t, "http://plc.invalid:3002", dir.PLCURL)
}
