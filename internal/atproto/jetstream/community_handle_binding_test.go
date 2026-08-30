package jetstream

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
)

// Binding a community's handle to the repo DID that signed its profile record.
//
// # WHAT THIS FILE IS FOR
//
// `communities.handle` is a UNIQUE column and it is the string every reader
// addresses a community by: resolveScopedIdentifier turns !nba@coves.social
// into GetByHandle("c-nba.coves.social"), so whichever DID holds that row IS
// !nba@coves.social as far as subscribe buttons, !mentions and URLs are
// concerned. What binds that column to the repo that actually published the
// record is resolution, and this file pins each of the ways the binding can come
// undone — on the create path and, further down, on the update path.
//
// Untagged and in-package, matching community_origin_test.go next door: the
// create and update paths are driven directly against its in-memory originRepo
// with hostedBy verification disabled, so nothing here touches a database or a
// network. That file's originRepo and profileCommit helpers are reused rather
// than duplicated.
//
// # WHY THE RESOLVER IS THE ATTACK SURFACE AND NOT JUST THE RECORD
//
// The record claiming a handle it does not own is the obvious half, and it has
// its own acceptance test. The half covered here is subtler and needs no
// attacker at all: asking the directory about DID X and storing whatever handle
// comes back, without checking that the answer was ABOUT X. In production
// `identity.NewResolver` wraps the directory in a Postgres cache, so the
// realistic way a wrong pair arrives is not forgery — it is a stale or mis-keyed
// cache row handing back a perfectly well-formed identity for somebody else. The
// row that would result looks healthy in every column and permanently squats a
// handle belonging to another community.

// mismatchedResolver is a directory (or, more plausibly, a cache in front of
// one) that answers about a DID other than the one it was asked about.
//
// fixedResolver next door echoes the subject DID, which is the honest default.
// This one pins a DID explicitly, which is the whole point: it is the only way
// to construct the disagreement the subject check exists to catch.
type mismatchedResolver struct{ id identity.Identity }

func (m mismatchedResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	id := m.id
	return &id, nil
}

// countingResolver echoes the subject DID with a fixed handle and records how
// many times it was asked.
//
// The count is the assertion, not a diagnostic. Whether resolution HAPPENS is
// the entire difference between checking a record's handle and believing it,
// and it is invisible in the stored row when the two agree — which is the case
// that has to be pinned, because the disagreeing case is already covered.
type countingResolver struct {
	handle string
	// pdsURL is what the directory says this repo's PDS is. Empty means
	// untrustedPDS, so the cases that do not care about the PDS read the same
	// as they always did.
	pdsURL string
	calls  int
}

func (c *countingResolver) Resolve(_ context.Context, did string) (*identity.Identity, error) {
	c.calls++
	pdsURL := c.pdsURL
	if pdsURL == "" {
		pdsURL = untrustedPDS
	}
	return &identity.Identity{DID: did, Handle: c.handle, PDSURL: pdsURL}, nil
}

// nilResolver returns (nil, nil) — no identity and no error.
//
// This is not a hypothetical shape invented to be awkward. It is what a
// map-backed fake does on a miss, mockIdentityResolverForUser in this very
// package included, and a real resolver that ever returns a zero value on a
// path its author thought unreachable does the same. A caller reads err, finds
// it nil, and dereferences.
type nilResolver struct{}

func (nilResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	return nil, nil
}

// erroringResolver fails every lookup with the error it was built with, and
// counts the attempts.
//
// failingResolver next door returns a fixed unstructured error, which is right
// for "the directory is down". These cases turn on the error's TYPE — the
// identity package classifies its failures, and the consumer has to route on
// that classification — so the error has to be a parameter.
type erroringResolver struct {
	err   error
	calls int
}

func (r *erroringResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	r.calls++
	return nil, r.err
}

// TestCommunityProfile_ChecksTheRecordKeyBeforeResolving is about cost, and the
// cost is inflicted by whoever sends the event.
//
// A community profile lives at exactly one record key, "self". Anything else is
// refused permanently, because a record key is immutable and no redrive can
// change it. But on the create path that refusal happens AFTER the directory has
// been consulted, so every malformed event buys a network round trip before
// being thrown away.
//
// That ordering is an amplifier. The rkey is chosen by whoever writes the
// record, so a repo can emit profiles at arbitrary keys as fast as it likes and
// each one costs this AppView one PLC lookup — against a directory we do not
// operate, on the firehose worker, with the answer discarded. Nothing about the
// event is even worth the question: it is rejected on a field already in hand,
// for a reason the resolution cannot affect.
//
// The rkey check is pure, total, and reads one string off the commit. It belongs
// before anything that leaves the process, and that is the general rule this
// pins rather than a micro-optimisation: cheap total checks first, then the
// expensive ones that can fail for reasons outside the event.
//
// The update path is included as a fence. It already checks the key first — it
// has to, since it also has a GetByDID and a create-path fallthrough behind that
// check — so its case documents the ordering rather than changing it, and stops
// a later edit from moving the check down to match the create path.
func TestCommunityProfile_ChecksTheRecordKeyBeforeResolving(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		operation string
		apply     func(*CommunityEventConsumer, context.Context, string, *CommitEvent) error
	}{
		{
			name:      "create",
			operation: "create",
			apply: func(c *CommunityEventConsumer, ctx context.Context, did string, commit *CommitEvent) error {
				return c.createCommunity(ctx, did, commit)
			},
		},
		{
			name:      "update",
			operation: "update",
			apply: func(c *CommunityEventConsumer, ctx context.Context, did string, commit *CommitEvent) error {
				return c.updateCommunity(ctx, did, commit)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, rkey := range []string{"3k2j4h5g6f7d", "custom-profile-name", ""} {
				t.Run("rkey "+rkey, func(t *testing.T) {
					t.Parallel()

					repo := newOriginRepo()
					const did = "did:plc:garbagerkey"
					resolver := &countingResolver{handle: nativeHandle}
					consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

					commit := profileCommit(tc.operation, "nba", "", nil)
					commit.RKey = rkey

					err := tc.apply(consumer, context.Background(), did, commit)

					require.Error(t, err, "a profile at rkey %q must be refused", rkey)
					assert.ErrorIs(t, err, ErrPermanentEvent,
						"a record key is immutable, so no redrive can turn this into a success")
					assert.Zerof(t, resolver.calls,
						"the directory was consulted %d time(s) before the event was thrown away for its "+
							"record key. The key is attacker-chosen and the check is a string comparison "+
							"on a field already in hand, so this hands anyone a PLC lookup per event they "+
							"care to emit", resolver.calls)
					assert.Nil(t, repo.byDID[did], "nothing may be indexed for a refused record key")
				})
			}
		})
	}
}

// TestCreateCommunity_ClassifiesAResolverFailureByItsKind is the redrive
// question: can this event ever succeed if we try again?
//
// Every resolver failure is currently answered "yes" — the error is returned
// unwrapped, and the connector treats anything without ErrPermanentEvent as
// transient. For a directory that is down that is exactly right, and it must
// stay right: an outage must not dead-letter the communities that happened to
// arrive during it.
//
// identity.ErrInvalidIdentifier is a different animal wearing the same coat. It
// means the string handed to the resolver is not a well-formed handle or DID at
// all — syntax.ParseAtIdentifier refused it before any network call was made.
// The input here is the event's own repo DID, which is fixed for the life of the
// event, so the resolver will refuse it identically on every attempt. Retrying
// is not merely useless: the connector burns in-line retries and then a redrive
// budget on it, per event, and a repo emitting malformed DIDs decides how many.
//
// Both cases are asserted together because the distinction is the whole point
// and either one alone can be satisfied by getting it wrong in the other
// direction. Classifying everything permanent would dead-letter real communities
// during a PLC outage; classifying everything transient is where we are now.
func TestCreateCommunity_ClassifiesAResolverFailureByItsKind(t *testing.T) {
	t.Parallel()

	t.Run("a malformed identifier can never succeed", func(t *testing.T) {
		t.Parallel()

		repo := newOriginRepo()
		const did = "did:plc:malformedidentifier"
		resolver := &erroringResolver{err: &identity.ErrInvalidIdentifier{
			Identifier: did,
			Reason:     "invalid identifier format",
		}}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

		err := consumer.createCommunity(context.Background(), did,
			profileCommit("create", "nba", "", nil))

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent,
			"the identifier the resolver refused is the event's own repo DID, which cannot change — so "+
				"every in-line retry and every redrive re-asks a question already answered, and the "+
				"repo emitting the malformed DID decides how often")
		assert.Equal(t, 1, resolver.calls,
			"the classification must come from the resolution attempt itself, not from a guess about "+
				"the DID made before asking")
		assert.Nil(t, repo.byDID[did])
	})

	t.Run("a resolution failure stays retryable", func(t *testing.T) {
		t.Parallel()

		repo := newOriginRepo()
		const did = "did:plc:directorydown"
		resolver := &erroringResolver{err: &identity.ErrResolutionFailed{
			Identifier: did,
			Reason:     "connection refused",
		}}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

		err := consumer.createCommunity(context.Background(), did,
			profileCommit("create", "nba", "", nil))

		require.Error(t, err, "a community whose handle could not be established must not be indexed")
		assert.NotErrorIs(t, err, ErrPermanentEvent,
			"the directory being unreachable is a fact about the network at this instant, and the "+
				"redrive is what repairs it. Dead-lettering here loses every community that arrived "+
				"during a PLC outage, permanently and silently")
		assert.Nil(t, repo.byDID[did])
	})
}

// TestUpdateCommunity_ClassifiesAResolverFailureByItsKind is the same question
// on the update path, where the answer is arrived at differently and one kind of
// failure escapes.
//
// The update path deliberately TOLERATES most resolution failures, and that is
// correct: a community already indexed has a verified handle from its create, so
// a directory that cannot answer says nothing about it, and refusing would
// dead-letter every profile edit made during an outage. resolveVerifiedHandleForUpdate
// implements that by returning the subject mismatch and swallowing everything
// else with a warning.
//
// "Everything else" is the bug. It was written when a mismatch was the only
// permanent failure the shared helper produced; it now also classifies
// identity.ErrInvalidIdentifier as permanent, and that error lands in the
// default arm — logged, discarded, and the edit applied and ACKNOWLEDGED. So the
// one failure that can never succeed on redrive is the one this path treats as
// harmless, while the transient ones it was designed to tolerate are tolerated
// correctly.
//
// The distinction the switch has to make is not "mismatch versus everything
// else" but "can this ever succeed": a malformed repo DID is fixed for the life
// of the event and will be refused identically forever, whereas an unreachable
// directory is a fact about this second.
//
// Both directions are asserted, because either can be satisfied by breaking the
// other. Propagating every error would turn a PLC outage into a dead-letter
// storm across every community being edited; propagating none is where this is
// now.
func TestUpdateCommunity_ClassifiesAResolverFailureByItsKind(t *testing.T) {
	t.Parallel()

	t.Run("a malformed identifier can never succeed", func(t *testing.T) {
		t.Parallel()

		repo := newOriginRepo()
		const did = "did:plc:updatemalformedid"
		seedCommunity(repo, did, nativeHandle, untrustedPDS)
		resolver := &erroringResolver{err: &identity.ErrInvalidIdentifier{
			Identifier: did,
			Reason:     "invalid identifier format",
		}}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

		commit := profileCommit("update", "nba", "", map[string]interface{}{
			"displayName": "Applied Despite A Permanent Failure",
		})
		err := consumer.updateCommunity(context.Background(), did, commit)

		require.Error(t, err,
			"the resolution failed permanently and the edit was applied and acknowledged anyway")
		assert.ErrorIs(t, err, ErrPermanentEvent,
			"a permanent failure that reaches the tolerate-everything arm is not merely mis-swallowed: "+
				"the event is ACKED, so it is never redriven and never dead-lettered — it simply "+
				"disappears, having already written a row from an identity nothing established")

		stored := repo.byDID[did]
		require.NotNil(t, stored, "the existing community must still be there")
		assert.NotEqual(t, "Applied Despite A Permanent Failure", stored.DisplayName,
			"the edit was written from an event that could never be resolved")
		assert.Equal(t, untrustedPDS, stored.PDSURL,
			"and pds_url — the column BridgeTrust reads — was reached by the same path")
	})

	t.Run("a transient failure still applies the edit", func(t *testing.T) {
		t.Parallel()

		repo := newOriginRepo()
		const did = "did:plc:updatedirectorydown"
		seedCommunity(repo, did, nativeHandle, untrustedPDS)
		resolver := &erroringResolver{err: &identity.ErrResolutionFailed{
			Identifier: did,
			Reason:     "connection refused",
		}}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

		commit := profileCommit("update", "nba", "", map[string]interface{}{
			"displayName": "Edited While The Directory Was Down",
		})
		require.NoError(t, consumer.updateCommunity(context.Background(), did, commit),
			"a directory outage must not reject a profile edit: the handle was verified at create, and "+
				"nothing about this community changed — only the network did")

		stored := repo.byDID[did]
		require.NotNil(t, stored)
		// Asserting the edit LANDED, not merely that no error came back. The
		// tolerated path is the one a fix for the case above could most easily
		// break, and "returns nil" alone would still pass if the function
		// started bailing out before writing anything.
		assert.Equal(t, "Edited While The Directory Was Down", stored.DisplayName,
			"the edit was tolerated but never applied, which is the worst of both: the event is acked "+
				"and the change is lost")
		assert.Equal(t, untrustedPDS, stored.PDSURL,
			"a failed resolution must not clobber the stored PDS host")
	})
}

// TestCreateCommunity_RefusesAResolutionAboutADifferentDID is the create path's
// half of the subject check.
//
// The record carries no handle, so this is the modern, "correct" path — the one
// that resolves rather than trusting the record — and resolving is not by itself
// enough. Checking the returned handle against "" and the handle.invalid
// placeholder says nothing about WHOSE handle it is; without asking whether the
// identity describes the DID that signed the commit, a resolution about
// did:plc:someoneelse indexes someoneelse's handle under this repo's DID.
//
// The refusal must be PERMANENT. A resolution that names a different subject is
// a contradiction, not an outage: redriving it produces the same answer every
// time, so classifying it transient would put the event in a retry queue that
// can never drain — the availability failure ErrPermanentEvent was introduced
// to prevent. Contrast the handle.invalid case a few lines above it in the
// consumer, which is deliberately transient because the directory really may
// verify the handle later.
func TestCreateCommunity_RefusesAResolutionAboutADifferentDID(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	resolver := mismatchedResolver{identity.Identity{
		DID:    "did:plc:someoneelsentirely",
		Handle: nativeHandle,
		PDSURL: untrustedPDS,
	}}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	const did = "did:plc:stalecachesubject"
	err := consumer.createCommunity(context.Background(), did,
		profileCommit("create", "nba", "", nil))

	require.Error(t, err,
		"the directory answered about did:plc:someoneelsentirely and the handle it named was indexed under %s", did)
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"a resolution naming a different subject answers the same way however often it is redriven; "+
			"classified transient it would occupy the retry queue forever")
	assert.Nil(t, repo.byDID[did],
		"%q was indexed under %s, which is not the DID the directory was describing", nativeHandle, did)
}

// TestCreateCommunity_DoesNotPanicOnANilResolution pins the crash.
//
// A panic here is not merely an ugly failure: this runs inside the firehose
// consumer, so it takes down ingestion for every collection, and the event that
// caused it is replayed on reconnect — a crash loop rather than a dead letter.
// admitRecordOrigin, further down the same file, nil-checks its resolution;
// createCommunity once did not, and two call sites disagreeing about the same
// resolver is exactly the inconsistency the shared predicate exists to end.
//
// The recover is deliberate and does not soften the assertion. A panic in a Go
// test aborts the whole package binary, so without it this one failing test
// would hide the result of every other test in the package. Recovering converts
// the crash
// into a named failure of THIS test and nothing else; it can never make the
// test pass, because the only path through the deferred function is t.Fatalf.
func TestCreateCommunity_DoesNotPanicOnANilResolution(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nilResolver{})
	const did = "did:plc:nilresolution"

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("createCommunity panicked on a (nil, nil) resolution instead of rejecting it: %v\n"+
				"this runs on the firehose worker, so the panic stops ingestion for every collection and "+
				"the event replays on reconnect", recovered)
		}
	}()

	err := consumer.createCommunity(context.Background(), did,
		profileCommit("create", "nba", "", nil))

	require.Error(t, err,
		"a resolution that established no identity must reject the event, not index a community with no handle")
	assert.Nil(t, repo.byDID[did],
		"a community was indexed from a resolution that returned no identity at all")
}

// TestCreateCommunity_ResolvesEvenWhenTheRecordCarriesAHandle keeps the
// short-circuit deleted.
//
// Resolution was once guarded by `if profile.Handle == ""`, so a record that
// carried a handle never reached the directory at all and the field was stored
// exactly as its author wrote it. social.coves.community.profile does not
// declare a handle property, so no PDS validates one; a foreign PDS that has
// never seen a social.coves.* lexicon accepts the extra field and puts it on the
// firehose untouched. That branch was never an optimisation — it was the AppView
// agreeing to be told, by an anonymous author, which entry of a UNIQUE column it
// should occupy.
//
// The case here is the AGREEING one: record and directory name the same handle,
// so the row that results is identical either way and every assertion about it
// would pass with the short-circuit back in place. The call count is the only
// place the rule is observable at all, which makes it the load-bearing line: a
// later refactor that reinstates "skip resolution when the record already has
// one" — which will look like a harmless saving of a network round trip — turns
// this into a 0 and shows up nowhere else in the suite.
//
// The disagreeing half of the rule lives in
// TestCreateCommunity_RefusesAResolutionAboutADifferentDID above and in the
// acceptance test over in internal/core/communities. Both halves are needed:
// one proves resolution happens, the other proves what happens when it
// contradicts the record.
func TestCreateCommunity_ResolvesEvenWhenTheRecordCarriesAHandle(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	resolver := &countingResolver{handle: nativeHandle}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	const did = "did:plc:carriesitsownhandle"
	commit := profileCommit("create", "nba", "", map[string]interface{}{"handle": nativeHandle})
	require.NoError(t, consumer.createCommunity(context.Background(), did, commit),
		"the record's handle and the directory's agree, so nothing here may be rejected")

	stored := repo.byDID[did]
	require.NotNil(t, stored, "the community must still be indexed")
	assert.Equal(t, nativeHandle, stored.Handle)

	assert.Equal(t, 1, resolver.calls,
		"the directory was never asked: the handle in the row is the record author's assertion, "+
			"not something this AppView established")
}

// TestCreateCommunity_IgnoresARecordHandleTheDirectoryContradicts states what
// the record's handle field is worth now that the consumer resolves
// unconditionally: nothing.
//
// The event is ACCEPTED. The community is indexed under the handle the
// directory names for the repo, and the string the record claimed appears
// nowhere in the row. The claim is inert — read, disagreed with, and dropped —
// rather than authoritative, and that is the entire fix. The vulnerability was
// never that a record could CLAIM c-nba.coves.social; it was that the claim was
// STORED. communities.handle is UNIQUE and resolveScopedIdentifier addresses
// communities by it, so whoever held that row held !nba@coves.social. Storing
// the resolved handle instead closes it completely: there is no input from the
// record to the column any more.
//
// # WHY NOT REFUSE THE EVENT
//
// Refusing on a claim/resolution disagreement was implemented and then
// reverted, and it will be proposed again, so here is the argument.
//
// It adds no security. The stored handle already comes from resolution, so the
// namespace cannot be taken whatever the record says. And verifyHostedByClaim
// runs AFTER the assignment, on the VERIFIED handle — so a record claiming
// c-nba.coves.social with hostedBy did:web:coves.social, published by a repo
// that resolves to c-atk.attacker.example, is refused there in production for
// the real reason: attacker.example is not coves.social. The string comparison
// was never what stopped it.
//
// What it costs is a PERMANENT false positive on ordinary drift. A record is a
// snapshot and handles are mutable, so the field goes stale the moment a
// community renames — and this AppView's own CreateCommunity writes that field
// (service.go), so our own communities carry one. A stale field contradicts its
// own correct resolution and dead-letters the community without redrive. The
// update path's GetByDID-NotFound fallthrough routes an update for an unindexed
// repo into this create path, so those records arrive here too. Dead-lettering
// a legitimate community for renaming is a worse outcome than indexing a
// forgery whose forged part is discarded.
//
// A disagreement is still a signal worth surfacing, so the consumer logs a
// warning. It is just not a reason to drop the event.
func TestCreateCommunity_IgnoresARecordHandleTheDirectoryContradicts(t *testing.T) {
	t.Parallel()

	// The handle the repo genuinely owns, in a namespace of its own. The
	// attacker is not forging an identity; it is making a claim about ours.
	const resolvedHandle = "c-nba.example.net"

	repo := newOriginRepo()
	resolver := fixedResolver{identity.Identity{Handle: resolvedHandle, PDSURL: untrustedPDS}}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	const did = "did:plc:contradictsthedirectory"
	commit := profileCommit("create", "nba", "", map[string]interface{}{"handle": nativeHandle})
	require.NoError(t, consumer.createCommunity(context.Background(), did, commit),
		"a claim the directory disagrees with is dropped, not fatal: the same disagreement is what an "+
			"ordinary rename produces, and refusing it dead-letters the community permanently")

	stored := repo.byDID[did]
	require.NotNil(t, stored, "the community must be indexed")
	assert.Equal(t, resolvedHandle, stored.Handle,
		"the row must carry the handle this AppView established, not the one the record asserted")
	assert.NotEqual(t, nativeHandle, stored.Handle,
		"%q was stored from the record: this is the namespace takeover, and it is the one thing "+
			"that may never happen", nativeHandle)
}

// ---------------------------------------------------------------------------
// The UPDATE path.
//
// Everything above is about createCommunity. updateCommunity was never migrated
// with it, and the gap is not cosmetic: it still guards resolution with
// `if profile.Handle == ""`, still dereferences the resolved identity with no
// nil check, and never calls identity.VerifiedHandle at all. All three bugs the
// create path had are still live one function down.
//
// The consequences differ in one important way, and it shapes these tests.
// postgresCommunityRepo.Update does NOT write the handle column — its SQL lists
// display_name, description, facets, blobs, visibility, discovery, moderation,
// warnings, record_uri, record_cid, origin and pds_url, and nothing else — so an
// update cannot move a community onto another community's handle however hard
// the record tries. The takeover is closed here by the schema.
//
// It DOES write pds_url, via COALESCE(NULLIF($13,''), pds_url). And pds_url is a
// trust input: BridgeTrust decides whether a post's bridged vote counts are
// admissible by reading the community row's PDS host. So the live exposure on
// this path is not the handle — it is that an UNVERIFIED resolution currently
// feeds that column.
//
// The handle assertions below are still worth making, and their status is worth
// stating plainly: originRepo.Update writes the whole struct, unlike Postgres,
// so they pin the CONSUMER's intent rather than an observable outcome. That is
// deliberate. The SQL is the guarantee; this is the layer above it agreeing, so
// an Update that later did learn to write the column would not silently inherit
// a consumer that hands it the record's word.
// ---------------------------------------------------------------------------

// seedCommunity puts an existing row in the fake so the update path runs as an
// update.
//
// updateCommunity falls through to createCommunity when GetByDID reports
// NotFound, which is right for a repo this AppView has never indexed and
// completely wrong for these tests: with no seeded row every case below would
// silently exercise the create path, which is already correct — so all five
// would pass while updateCommunity stayed broken.
func seedCommunity(repo *originRepo, did, handle, pdsURL string) {
	repo.byDID[did] = &communities.Community{
		DID:      did,
		Handle:   handle,
		Name:     "nba",
		OwnerDID: did,
		PDSURL:   pdsURL,
	}
}

// TestUpdateCommunity_DoesNotPanicOnANilResolution is the update path's copy of
// the crash, and it is the more dangerous copy.
//
// A create that panics loses one community. This runs for every profile EDIT of
// every community already indexed, on the firehose worker, so the panic takes
// ingestion down for every collection and the event replays on reconnect — a
// crash loop rather than a dead letter.
//
// The event must also be ACCEPTED, which is the difference from the create path.
// There, a resolution that established nothing meant there was no handle to
// index under, and refusing was the only honest answer. Here a verified handle
// is already stored, from the create that put the row there, so a directory
// outage says nothing about this community — only that the network is
// unreachable this second. Rejecting would dead-letter every profile edit made
// during one. That is the contract
// TestUpdateCommunity_ProvenanceIsResolvedAfresh's "a directory outage drops the
// origin, not the event" already states for a resolver that ERRORS; a resolver
// that returns nothing is the same fact arriving in a shape that crashes.
//
// On the recover: a panic aborts the whole package binary, so without it this
// one failure would hide every other test's result. It cannot make the test
// pass — the only path through the deferred function is t.Fatalf.
func TestUpdateCommunity_DoesNotPanicOnANilResolution(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	const did = "did:plc:updatenilresolution"
	seedCommunity(repo, did, nativeHandle, untrustedPDS)
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nilResolver{})

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("updateCommunity panicked on a (nil, nil) resolution: %v\n"+
				"this runs on the firehose worker for every profile edit, so the panic stops ingestion "+
				"for every collection and the event replays on reconnect", recovered)
		}
	}()

	// No handle in the record, which is what sends the current code into the
	// resolution branch it then dereferences without a nil check.
	err := consumer.updateCommunity(context.Background(), did,
		profileCommit("update", "nba", "", nil))

	require.NoError(t, err,
		"a resolution that established nothing must not reject the edit: this community's handle was "+
			"verified when it was created, and a directory that cannot answer right now says nothing "+
			"about it")

	stored := repo.byDID[did]
	require.NotNil(t, stored, "the community must still be there")
	assert.Equal(t, nativeHandle, stored.Handle,
		"the stored handle was replaced by what the resolution failed to establish")
	assert.Equal(t, untrustedPDS, stored.PDSURL,
		"a failed resolution must not clobber the stored PDS host: BridgeTrust reads that column to "+
			"decide whether this community's bridged vote counts are admissible")
}

// TestUpdateCommunity_ResolvesEvenWhenTheRecordCarriesAHandle removes the
// short-circuit from the update path, for the same reason it was removed from
// the create path.
//
// `if profile.Handle == ""` means a record that names a handle is never resolved
// at all — so on this path the consumer never learns the repo's current PDS
// host, and never notices that the identity it would have got back describes a
// different DID. The one field it is about to write is then decided by the
// record's author.
func TestUpdateCommunity_ResolvesEvenWhenTheRecordCarriesAHandle(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	const did = "did:plc:updatecarrieshandle"
	seedCommunity(repo, did, nativeHandle, untrustedPDS)

	resolver := &countingResolver{handle: nativeHandle}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	// No origin on the record, deliberately: admitRecordOrigin resolves on its
	// own when a record carries one, which would satisfy this count without
	// updateCommunity's own branch having changed at all.
	commit := profileCommit("update", "nba", "", map[string]interface{}{"handle": nativeHandle})
	require.NoError(t, consumer.updateCommunity(context.Background(), did, commit))

	assert.Equal(t, 1, resolver.calls,
		"the directory was never asked, because the record named a handle. Everything this path "+
			"decides — the PDS host it writes, whether the identity is even about this repo — is then "+
			"decided by the record's author")
}

// TestUpdateCommunity_RefusesAResolutionAboutADifferentDID brings the subject
// check to the update path.
//
// A caching resolver sits in front of the directory in production, so the
// realistic way a wrong identity arrives is a stale or mis-keyed cache row
// handing back somebody else's perfectly well-formed one. On the create path
// that would have indexed the wrong handle; here it writes the wrong repo's PDS
// host onto an existing community, which is worse in a quiet way — the row keeps
// its correct handle and looks entirely healthy while BridgeTrust makes
// admissibility decisions about it from another repo's provenance.
//
// PERMANENT: a resolution naming a different subject reproduces on every
// redrive, so a transient classification would park the event in a retry queue
// that can never drain.
func TestUpdateCommunity_RefusesAResolutionAboutADifferentDID(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	const did = "did:plc:updatestalecache"
	seedCommunity(repo, did, nativeHandle, untrustedPDS)

	resolver := mismatchedResolver{identity.Identity{
		DID:    "did:plc:someoneelsentirely",
		Handle: "c-nba.example.net",
		PDSURL: trustedBridgePDS,
	}}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	err := consumer.updateCommunity(context.Background(), did,
		profileCommit("update", "nba", "", nil))

	require.Error(t, err,
		"the directory answered about did:plc:someoneelsentirely and the edit was applied under %s", did)
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"a resolution naming a different subject answers the same way however often it is redriven")
	assert.Equal(t, untrustedPDS, repo.byDID[did].PDSURL,
		"another repo's PDS host was written onto this community. BridgeTrust reads that column to "+
			"decide whether bridged vote counts are admissible, so a wrong value here is a trust "+
			"decision made from somebody else's provenance")
}

// TestUpdateCommunity_DoesNotTrustAnUnverifiedResolutionsPDS is the case that
// makes the update-path change worth doing at all.
//
// handle.invalid means the second leg of resolution did not complete — DNS was
// unreachable, or the handle did not verify back to this DID. What came back is
// an identity nothing has confirmed, and the current code takes its PDSURL and
// writes it into the column BridgeTrust reads. No attacker is required for that
// to be wrong: it is an unconfirmed fact being promoted into a trust input.
//
// The event is still ACCEPTED, and that pairing is the point. Unverified is
// transient — the directory may answer next time — so refusing would dead-letter
// ordinary edits during a DNS wobble. What must not happen is STORING the
// unverified part. Drop the untrustworthy field; keep the event.
func TestUpdateCommunity_DoesNotTrustAnUnverifiedResolutionsPDS(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	const did = "did:plc:updateunverified"
	seedCommunity(repo, did, nativeHandle, untrustedPDS)

	// A confident-looking resolution: it names a PDS, and the one it names is
	// the TRUSTED bridge host — so believing it would upgrade this community's
	// provenance on the strength of an identity nobody verified.
	resolver := &countingResolver{handle: identity.InvalidHandle, pdsURL: trustedBridgePDS}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	err := consumer.updateCommunity(context.Background(), did,
		profileCommit("update", "nba", "", nil))

	require.NoError(t, err,
		"an unverifiable handle is a fact about the network, not about this community: refusing here "+
			"dead-letters every profile edit made while DNS is unhappy")

	stored := repo.byDID[did]
	require.NotNil(t, stored)
	assert.Equal(t, untrustedPDS, stored.PDSURL,
		"the PDS host from an UNVERIFIED resolution was written to the column BridgeTrust reads. "+
			"Nothing confirmed that identity — indigo returns %q precisely to say so — and this one "+
			"named the trusted bridge host, so believing it promotes a community's provenance on no "+
			"evidence at all", identity.InvalidHandle)
	assert.Equal(t, nativeHandle, stored.Handle,
		"the placeholder must never reach the row: communities.handle is UNIQUE, so the first one to "+
			"store it takes it and every later unverifiable community collides")
}

// TestUpdateCommunity_BackfillsThePDSFromAVerifiedResolution is the other
// direction, and it exists so the fix above cannot be "stop writing pds_url".
//
// The backfill is load-bearing: rows indexed before pds_url was populated carry
// an empty value, which makes BridgeTrust default-deny their posts' bridged
// stats forever, and this path is how they get repaired. A community that
// genuinely migrates PDS is the same shape. So a VERIFIED resolution naming a
// new host must still write it — the rule is about whether the resolution was
// confirmed, not about whether the column may change.
func TestUpdateCommunity_BackfillsThePDSFromAVerifiedResolution(t *testing.T) {
	t.Parallel()

	const movedPDS = "https://pds.moved.example"

	repo := newOriginRepo()
	const did = "did:plc:updateverifiedpds"
	seedCommunity(repo, did, nativeHandle, untrustedPDS)

	resolver := &countingResolver{handle: nativeHandle, pdsURL: movedPDS}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver)

	require.NoError(t, consumer.updateCommunity(context.Background(), did,
		profileCommit("update", "nba", "", nil)))

	assert.Equal(t, movedPDS, repo.byDID[did].PDSURL,
		"a verified resolution naming a new PDS host must still update the row: rows indexed before "+
			"pds_url existed carry an empty value, and this path is what repairs them — without it "+
			"BridgeTrust default-denies their posts' bridged stats forever")
}
