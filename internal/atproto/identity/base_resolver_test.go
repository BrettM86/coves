package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// Its own misses return Indigo's sentinels — ErrHandleNotFound for a handle,
// ErrDIDNotFound for a DID — because that is what the real directory returns
// and because classification now reads the sentinel. A fake that answered a
// bare errors.New would make every absence in this file resolve as breakage,
// which is the correct new behaviour for unrecognised errors and a useless
// model of a directory.
//
// The maps are written once at construction and only read afterwards; the call
// logs are written on every lookup, and several tests below share one directory
// across parallel subtests, so the logs are guarded. An unguarded slice append
// here is a genuine data race rather than a theoretical one — the race detector
// caught exactly that when the recording was first added.
type fakeDirectory struct {
	byHandle map[string]*indigoIdentity.Identity
	byDID    map[string]*indigoIdentity.Identity

	// err, when set, is returned by every lookup. Its IDENTITY is the whole
	// point for the taxonomy tests: baseResolver classifies failures by
	// errors.Is against Indigo's sentinels, so what a fixture wraps decides the
	// answer and its wording decides nothing.
	err error

	// resolvesToEmptyDID is the handle for which ResolveHandle answers ("",
	// nil) — no DID and NO ERROR. That is not a hypothetical fake behaviour; it
	// is Indigo's, and it is the subject of one test below. See
	// TestBaseResolver_SkippedDNSEmptyDID.
	resolvesToEmptyDID string

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

// The four methods below mirror indigoIdentity.BaseDirectory's real structure
// rather than answering from the maps directly, because the STRUCTURE is what
// two of the tests in this file are about. Indigo's LookupHandle is a
// three-step chain — resolve the handle to a DID, fetch that DID's document,
// then check the document declares the handle back — and a failure mode that
// only exists between two of those steps cannot be reproduced by a fake that
// collapses them into a map read.

func (d *fakeDirectory) Lookup(_ context.Context, atid syntax.AtIdentifier) (*indigoIdentity.Identity, error) {
	d.record(&d.looked, "Lookup("+atid.String()+")")
	if d.err != nil {
		return nil, d.err
	}
	if did, err := atid.AsDID(); err == nil {
		return d.lookupDID(context.Background(), did)
	}
	handle, err := atid.AsHandle()
	if err != nil {
		return nil, fmt.Errorf("%w: neither a handle nor a DID", indigoIdentity.ErrInvalidHandle)
	}
	return d.lookupHandle(context.Background(), handle)
}

func (d *fakeDirectory) LookupHandle(ctx context.Context, handle syntax.Handle) (*indigoIdentity.Identity, error) {
	d.record(&d.looked, "LookupHandle("+handle.String()+")")
	if d.err != nil {
		return nil, d.err
	}
	return d.lookupHandle(ctx, handle)
}

func (d *fakeDirectory) LookupDID(ctx context.Context, did syntax.DID) (*indigoIdentity.Identity, error) {
	d.record(&d.looked, "LookupDID("+did.String()+")")
	if d.err != nil {
		return nil, d.err
	}
	return d.lookupDID(ctx, did)
}

// lookupHandle is indigoIdentity.BaseDirectory.LookupHandle, step for step.
func (d *fakeDirectory) lookupHandle(ctx context.Context, handle syntax.Handle) (*indigoIdentity.Identity, error) {
	handle = handle.Normalize()

	did, err := d.ResolveHandle(ctx, handle)
	if err != nil {
		return nil, err
	}

	// No check that did is non-empty, and that omission is Indigo's, not a
	// simplification: it is the whole defect this fake exists to reproduce.
	doc, err := d.ResolveDID(ctx, did)
	if err != nil {
		return nil, err
	}

	ident := indigoIdentity.ParseIdentity(doc)
	declared, err := ident.DeclaredHandle()
	if err != nil {
		return nil, fmt.Errorf("could not verify handle/DID match: %w", err)
	}
	if declared != handle {
		return nil, fmt.Errorf("%w: %s != %s", indigoIdentity.ErrHandleMismatch, declared, handle)
	}
	ident.Handle = declared
	return &ident, nil
}

// lookupDID is indigoIdentity.BaseDirectory.LookupDID: the document decides the
// handle, and a handle that does not resolve back to this DID is downgraded to
// handle.invalid rather than trusted.
func (d *fakeDirectory) lookupDID(ctx context.Context, did syntax.DID) (*indigoIdentity.Identity, error) {
	doc, err := d.ResolveDID(ctx, did)
	if err != nil {
		return nil, err
	}

	ident := indigoIdentity.ParseIdentity(doc)
	declared, err := ident.DeclaredHandle()
	switch {
	case errors.Is(err, indigoIdentity.ErrHandleNotDeclared):
		ident.Handle = syntax.HandleInvalid
	case err != nil:
		return nil, fmt.Errorf("could not parse handle from DID document: %w", err)
	default:
		resolved, resolveErr := d.ResolveHandle(ctx, declared)
		if resolveErr != nil || resolved != ident.DID {
			ident.Handle = syntax.HandleInvalid
		} else {
			ident.Handle = declared
		}
	}
	return &ident, nil
}

// ResolveHandle is the handle-to-DID leg: DNS TXT, then the .well-known
// document, in the real one.
//
// The ("", nil) answer is the point. Indigo's version ends by picking the most
// helpful of the two legs' errors, and when the DNS leg was SKIPPED — which is
// what SkipDNSDomainSuffixes does for the dev and CI handle domains — its error
// is nil, so the tail returns that nil alongside the empty DID it never
// filled in.
func (d *fakeDirectory) ResolveHandle(_ context.Context, handle syntax.Handle) (syntax.DID, error) {
	if d.err != nil {
		return "", d.err
	}
	if d.resolvesToEmptyDID != "" && handle.String() == d.resolvesToEmptyDID {
		return "", nil
	}
	ident, ok := d.byHandle[handle.String()]
	if !ok {
		return "", fmt.Errorf("%w: %s", indigoIdentity.ErrHandleNotFound, handle)
	}
	return ident.DID, nil
}

// ResolveDID fetches a DID document.
//
// The unsupported-method branch is reproduced verbatim from Indigo
// (atproto/identity/did.go): the message carries no sentinel at all, which is
// why an empty DID arriving here surfaces as unclassifiable breakage rather
// than as the absence it really is.
func (d *fakeDirectory) ResolveDID(_ context.Context, did syntax.DID) (*indigoIdentity.DIDDocument, error) {
	if d.err != nil {
		return nil, d.err
	}
	switch did.Method() {
	case "plc", "web":
	default:
		return nil, fmt.Errorf("DID method not supported: %s", did.Method())
	}
	ident, ok := d.byDID[did.String()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", indigoIdentity.ErrDIDNotFound, did)
	}
	doc := ident.DIDDocument()
	return &doc, nil
}

// ResolveDIDRaw completes indigoIdentity.Resolver. Nothing in this package
// calls it; the fake implements the whole interface so it cannot drift into
// being a different shape from the thing it stands in for.
func (d *fakeDirectory) ResolveDIDRaw(ctx context.Context, did syntax.DID) (json.RawMessage, error) {
	doc, err := d.ResolveDID(ctx, did)
	if err != nil {
		return nil, err
	}
	return json.Marshal(doc)
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

	// AlsoKnownAs is what makes the handle VERIFIABLE: LookupHandle only trusts
	// a handle the DID document declares back. A fixture without it would make
	// every successful resolution in this file fail the mismatch check.
	ident := &indigoIdentity.Identity{
		DID:         did,
		Handle:      handle,
		AlsoKnownAs: []string{"at://" + fixtureHandle},
	}
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

// TestBaseResolver_ErrorTaxonomy pins the rule that separates "this account
// does not exist" from "resolution broke".
//
// The distinction is drawn with errors.Is against Indigo's own sentinels, not
// by reading its error TEXT. Text-matching is what this used to do, and it was
// wrong in both directions: "HTTP well-known status 503 for dev404.example.com"
// contains "404" and became a not-found, while any breakage whose message
// happens to contain "not found" did the same. Both mistakes point the same
// way — an outage reported to the caller as a missing account, then cached as
// an absence, leaving a real account invisible after the outage ends.
func TestBaseResolver_ErrorTaxonomy(t *testing.T) {
	t.Parallel()

	t.Run("absence", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"bare handle-not-found sentinel", indigoIdentity.ErrHandleNotFound},
			{
				"wrapped handle-not-found sentinel",
				fmt.Errorf("resolving %s: %w", fixtureHandle, indigoIdentity.ErrHandleNotFound),
			},
			{"bare DID-not-found sentinel", indigoIdentity.ErrDIDNotFound},
			{
				"wrapped DID-not-found sentinel",
				fmt.Errorf("looking up %s: %w", fixtureDID, indigoIdentity.ErrDIDNotFound),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resolver := &baseResolver{directory: &fakeDirectory{err: tc.err}}

				_, err := resolver.Resolve(context.Background(), fixtureHandle)
				var notFound *ErrNotFound
				require.ErrorAsf(t, err, &notFound,
					"%v means the identity is absent, and the caller answers 404. Classified as a "+
						"resolution failure instead it becomes a 502, and a signup with a typo in the "+
						"handle looks like our outage", tc.err)
				assert.Contains(t, notFound.Error(), tc.err.Error(),
					"the underlying reason must survive: a bare \"not found\" is unactionable in a log")
			})
		}
	})

	t.Run("breakage", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"connection refused", errors.New("dial tcp: connection refused")},
			{"deadline", errors.New("context deadline exceeded")},
			{
				"upstream 500 under the resolution-failed sentinel",
				fmt.Errorf("%w: HTTP well-known status 500 for %s",
					indigoIdentity.ErrHandleResolutionFailed, fixtureHandle),
			},
			{"handle/DID mismatch", indigoIdentity.ErrHandleMismatch},
			// The case that gave this test its teeth. Everything about this
			// error says outage — it is the resolution-failed sentinel, and the
			// status is 503 — but the HOST is dev404.example.com, so the text
			// contains "404". Text-matching read that as a missing account.
			{
				"a 503 whose host name contains 404",
				fmt.Errorf("%w: HTTP well-known status 503 for dev404.example.com",
					indigoIdentity.ErrHandleResolutionFailed),
			},
			// And the mirror image: a message that reads like an absence but
			// carries no sentinel at all. Only Indigo's own sentinels are
			// evidence of absence; an unrecognised error is unclassified, and
			// unclassified must fail safe as breakage.
			{"prose that merely sounds absent", errors.New("something not found-ish")},
			{"NoRecordsFound with no sentinel", errors.New("NoRecordsFound")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				resolver := &baseResolver{directory: &fakeDirectory{err: tc.err}}

				_, err := resolver.Resolve(context.Background(), fixtureHandle)
				var failed *ErrResolutionFailed
				require.ErrorAsf(t, err, &failed,
					"%v is our problem, not the client's. Reported as not-found it would be cached as "+
						"an absence and the account would stay invisible after the outage ended", tc.err)

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

// TestBaseResolver_SkippedDNSEmptyDID pins the one failure mode that made the
// resolveHandle endpoint answer 502 for a handle that had simply never been
// created.
//
// # THE DEFECT, END TO END
//
// Indigo's ResolveHandle runs two legs, DNS TXT and the .well-known document,
// and ends by choosing the more helpful of their two errors. The dev and CI
// stacks list their handle domain in SkipDNSDomainSuffixes, so the DNS leg
// never runs and its error stays nil. For a handle nobody registered, the
// well-known leg gets a 404 from the PDS and reports ErrHandleNotFound — and
// the tail then asks whether the DNS error is ErrHandleNotFound, finds a nil
// that is not, and returns it. The caller receives an empty DID and NO ERROR.
//
// LookupHandle does not check for that, so it hands "" to ResolveDID, which
// falls through to its default branch and answers "DID method not supported: ".
// That message carries no sentinel, so this resolver classifies it as breakage,
// the users service wraps it, and an unauthenticated caller who mistyped their
// own handle is told the server is broken.
//
// # WHY IT IS PINNED HERE RATHER THAN AT THE ENDPOINT
//
// The endpoint's 502 is a symptom; the misclassification is the bug, and this
// is the layer that owns the absent-versus-broken decision. It is also the
// layer that can fix it without asking every caller to recognise a sentinel-less
// string, which is what makes the assertion below the contract rather than a
// regression note: an empty DID from ResolveHandle MEANS the handle resolved to
// nothing, whatever the error slot says.
func TestBaseResolver_SkippedDNSEmptyDID(t *testing.T) {
	t.Parallel()

	const unregistered = "ghost.invalid"
	resolver, _ := newFakeBackedResolver(t, fixturePDS)
	dir := resolver.directory.(*fakeDirectory)
	dir.resolvesToEmptyDID = unregistered

	_, err := resolver.Resolve(context.Background(), unregistered)

	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound,
		"a handle that resolves to no DID is ABSENT. Reported as a resolution failure it becomes a "+
			"502 at the API boundary, so a person who mistyped their handle at the login form is told "+
			"our service is down — and on a stack that skips DNS for its own handle domain, which dev "+
			"and CI both do, that is every unregistered handle rather than a rare case")

	var failed *ErrResolutionFailed
	assert.NotErrorAs(t, err, &failed)

	assert.NotContains(t, notFound.Error(), "DID method not supported",
		"the reason must describe the handle, not the empty string Indigo tried to look up next: "+
			"that message is an artifact of the defect and means nothing to anyone reading a log")
}

// TestBaseResolver_EmptyDIDIsNotAnExcuseToStopVerifying guards the obvious
// wrong fix for the case above: treating any ResolveHandle answer as absence,
// or skipping the DID-document check to avoid the empty-DID path. A handle that
// DOES resolve must still come back fully verified.
func TestBaseResolver_EmptyDIDIsNotAnExcuseToStopVerifying(t *testing.T) {
	t.Parallel()

	resolver, _ := newFakeBackedResolver(t, fixturePDS)
	dir := resolver.directory.(*fakeDirectory)
	dir.resolvesToEmptyDID = "ghost.invalid"

	got, err := resolver.Resolve(context.Background(), fixtureHandle)
	require.NoError(t, err)
	assert.Equal(t, fixtureDID, got.DID)
	assert.Equal(t, fixtureHandle, got.Handle,
		"still read back from the DID document, so the handle stays bidirectionally verified")
	assert.Equal(t, fixturePDS, got.PDSURL)
}
