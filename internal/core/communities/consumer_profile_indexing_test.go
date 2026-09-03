//go:build integration

package communities_test

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// How a community profile record becomes a row.
//
// The firehose is the ONLY way a community that this instance does not host can
// enter the index, and — because UpdateCommunity deliberately does not write
// Postgres — it is also the only way an edit to a local community becomes
// visible. So this consumer is the write path for community data in the general
// case, and the assertions below are about the shape of the row it produces
// rather than about the events it accepts.
//
// # WHY THESE LIVE IN internal/core/communities
//
// They are consumer tests by their entry point and community tests by their
// subject: every assertion is made by reading back through
// communities.Repository, and what they pin is the community domain's
// invariants — self-ownership, the canonical "self" record key, the handle a
// community is addressed by. The consumer's own pure decoders (blob-ref
// extraction, contentVisibility clamping) are unit-tested in place, untagged,
// in internal/atproto/jetstream/community_consumer_test.go; what could not go
// there is anything that needs a real repository, which is all of this.
//
// # WHAT IS NOT HERE
//
// Events the consumer must ignore — other collections, identity and account
// events, a commit with no body — are covered by that same file's
// TestCommunityConsumer_IgnoresUnrelatedCollections, which asserts it against a
// NIL repository, so anything that reached a database call would panic rather
// than pass. That is a stronger statement than a Postgres-backed test can make,
// and it made the version of those cases that used to live alongside these
// redundant.
//
// Verification of the hostedBy claim is switched off in every consumer built
// here (the third constructor argument), matching CI. Verification dials the
// hosting domain's DID document over the network, which no T1 test may depend
// on; its own coverage is in the hostedBy security tests.

// stubIdentityResolver answers handle resolutions from a map instead of from
// the PLC directory.
//
// The consumer's production path resolves a community's handle from its DID
// because handles are mutable and records must not carry them. That is a
// network call to a service this tier may not touch, and — more to the point —
// what these tests care about is what the consumer DOES with the answer, and
// whether it calls at all.
type stubIdentityResolver struct {
	resolutions map[string]string
	lastDID     string
	callCount   int
	shouldFail  bool
}

func newStubIdentityResolver() *stubIdentityResolver {
	return &stubIdentityResolver{resolutions: make(map[string]string)}
}

func (s *stubIdentityResolver) Resolve(_ context.Context, did string) (*identity.Identity, error) {
	s.callCount++
	s.lastDID = did

	if s.shouldFail {
		return nil, errors.New("stub PLC resolution failure")
	}

	handle, configured := s.resolutions[did]
	if !configured {
		return nil, fmt.Errorf("no resolution configured for DID: %s", did)
	}

	return &identity.Identity{
		DID:        did,
		Handle:     handle,
		PDSURL:     "https://pds.example.com",
		ResolvedAt: time.Now(),
		Method:     identity.MethodHTTPS,
	}, nil
}

// newCommunityConsumer builds the consumer over a fresh database clone and
// returns the repository the assertions read through.
func newCommunityConsumer(t *testing.T, resolver *stubIdentityResolver) (
	*jetstream.CommunityEventConsumer, communities.Repository,
) {
	t.Helper()

	repo := postgres.NewCommunityRepository(testkit.DB(t), credentialciphertest.Fixed())
	if resolver == nil {
		// A typed nil in an interface parameter is not nil, and the consumer
		// branches on the interface being nil to decide whether it is in
		// handle-construction mode. Passing the untyped nil is the difference
		// between exercising that branch and panicking inside it.
		return jetstream.NewCommunityEventConsumer(repo, instanceDID, true, nil), repo
	}
	return jetstream.NewCommunityEventConsumer(repo, instanceDID, true, resolver), repo
}

// profileRecord is a valid community profile with the fields every branch of
// the consumer reads. Tests mutate the copy they are given.
//
// Note what is absent: no "did", no "handle", no counts. Those are resolved or
// computed by the AppView, and a record carrying them would be asserting facts
// its author cannot know.
func profileRecord(name string) map[string]interface{} {
	return map[string]interface{}{
		"$type":       "social.coves.community.profile",
		"name":        name,
		"displayName": "Consumer Indexed",
		"description": "a community that arrived over the firehose",
		"createdBy":   "did:plc:communityconsumer",
		"hostedBy":    instanceDID,
		"visibility":  "public",
		"federation": map[string]interface{}{
			"allowExternalDiscovery": true,
		},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// profileEvent wraps a record in the commit envelope Jetstream delivers.
func profileEvent(did, operation, rkey, cid string, record map[string]interface{}) *jetstream.JetstreamEvent {
	return &jetstream.JetstreamEvent{
		Did:    did,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        testkit.TID(),
			Operation:  operation,
			Collection: "social.coves.community.profile",
			RKey:       rkey,
			CID:        cid,
			Record:     record,
		},
	}
}

// TestCommunityConsumer_IgnoresForeignCollections and its sibling below are the
// dispatch guard, against a REAL repository.
//
// internal/atproto/jetstream/community_consumer_test.go already asserts that
// these events are ignored, and does it with a nil repository — which is the
// stronger proof that nothing REACHED a repo call, because anything that did
// would panic. These two are not a duplicate of that, and the difference is the
// reason they are worth their lines: a nil repo cannot distinguish "the
// consumer ignored the event" from "the consumer would have written a row but
// crashed before it could". Here the write would succeed, so asserting the
// community is still absent afterwards is a statement about the DATABASE rather
// than about a panic.
//
// The volume argument is why the guard matters at all. Every consumer shares one
// feed, and account and identity events bypass wantedCollections entirely, so
// the overwhelming majority of what this consumer is handed is something it must
// do nothing with. A dispatch bug here is not a wrong row in one place; it is
// every unrelated event in the firehose landing in the communities table.
func TestCommunityConsumer_IgnoresForeignCollections(t *testing.T) {
	t.Parallel()

	consumer, repo := newCommunityConsumer(t, newStubIdentityResolver())
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "frn")
	communityDID := fixtures.DID(name)

	// Each carries a well-formed community profile as its record and the
	// community's own repo DID, so the ONLY thing telling the consumer to leave
	// it alone is the collection. An event that differed in several ways at once
	// could be ignored for the wrong reason and still pass.
	for _, collection := range []string{
		"social.coves.community.post",
		"social.coves.community.comment",
		"social.coves.actor.profile",
		"social.coves.feed.vote",
		"app.bsky.feed.post",
	} {
		event := profileEvent(communityDID, "create", "self", "bafyforeign", profileRecord(name))
		event.Commit.Collection = collection

		require.NoErrorf(t, consumer.HandleEvent(ctx, event),
			"an event for %s must be ignored, not treated as an error: it will arrive constantly, "+
				"and a consumer that errors on it dead-letters most of the firehose", collection)

		_, err := repo.GetByDID(ctx, communityDID)
		require.Errorf(t, err,
			"an event for %s was indexed as a community profile — the consumer is dispatching on "+
				"something other than the collection", collection)
	}
}

// TestCommunityConsumer_IgnoresNonCommitKinds covers the events that reach this
// consumer no matter what it subscribed to.
//
// Jetstream's wantedCollections filter applies to commits only: identity and
// account events are delivered to every subscriber regardless, which is the same
// property that keeps -p 1 in place for the whole suite (tests/testkit/db.go's
// packageParallelism). A bodyless commit is here for a different reason — it is
// the shape that dereferences a nil Commit if the consumer checks the kind
// without checking the body.
func TestCommunityConsumer_IgnoresNonCommitKinds(t *testing.T) {
	t.Parallel()

	consumer, repo := newCommunityConsumer(t, newStubIdentityResolver())
	ctx := context.Background()

	communityDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "knd"))

	for _, event := range []*jetstream.JetstreamEvent{
		{Kind: "identity", Did: communityDID, TimeUS: time.Now().UnixMicro()},
		{Kind: "account", Did: communityDID, TimeUS: time.Now().UnixMicro()},
		{Kind: "commit", Did: communityDID, TimeUS: time.Now().UnixMicro()},
	} {
		require.NoErrorf(t, consumer.HandleEvent(ctx, event),
			"a %q event must be ignored: it arrives whatever this consumer subscribed to", event.Kind)
	}

	_, err := repo.GetByDID(ctx, communityDID)
	require.Error(t, err, "a non-commit event created a community row")
}

func TestCommunityConsumer_IndexesAProfileFromTheFirehose(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "idx")
	communityDID := fixtures.DID(name)

	resolver := newStubIdentityResolver()
	resolver.resolutions[communityDID] = "c-" + name + "." + instanceDomain
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(communityDID, "create", "self", "bafyindexedprofile", profileRecord(name))))

	indexed, err := repo.GetByDID(ctx, communityDID)
	require.NoError(t, err, "the event was accepted but no row appeared")

	assert.Equal(t, "Consumer Indexed", indexed.DisplayName)
	assert.Equal(t, "public", indexed.Visibility)
	assert.True(t, indexed.AllowExternalDiscovery)

	// V2 self-ownership. The community's repo DID IS the community, so there is
	// no separate owner to get wrong — but the column exists, and a consumer
	// that filled it from the record's "owner" field instead of from the event's
	// repo DID would let a record name somebody else as its owner.
	assert.Equal(t, indexed.DID, indexed.OwnerDID, "a V2 community owns itself")

	// The record URI is built from the EVENT's repo DID, not from anything in
	// the record. A regression here would file every federated community's
	// profile as living in whichever repo the AppView happened to think it
	// owned, and nothing else looks at this column.
	assert.Equal(t, "at://"+communityDID+"/social.coves.community.profile/self", indexed.RecordURI)
	assert.Equal(t, "bafyindexedprofile", indexed.RecordCID,
		"the row must record the commit's CID: it is how a later event is told apart from a replay")
}

func TestCommunityConsumer_UpdatesAnIndexedProfile(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "upc")
	communityDID := fixtures.DID(name)
	handle := "c-" + name + "." + instanceDomain

	resolver := newStubIdentityResolver()
	resolver.resolutions[communityDID] = handle
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	_, err := repo.Create(ctx, &communities.Community{
		DID:                    communityDID,
		Handle:                 handle,
		Name:                   name,
		DisplayName:            "Original Name",
		Description:            "Original description",
		OwnerDID:               communityDID,
		CreatedByDID:           "did:plc:communityconsumer",
		HostedByDID:            instanceDID,
		Visibility:             "public",
		AllowExternalDiscovery: true,
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	})
	require.NoError(t, err)

	record := profileRecord(name)
	record["displayName"] = "Updated Name"
	record["description"] = "Updated description"
	record["visibility"] = "unlisted"
	record["federation"] = map[string]interface{}{"allowExternalDiscovery": false}

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(communityDID, "update", "self", "bafyupdatedprofile", record)))

	updated, err := repo.GetByDID(ctx, communityDID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", updated.DisplayName)
	assert.Equal(t, "Updated description", updated.Description)
	assert.Equal(t, "unlisted", updated.Visibility)
	assert.False(t, updated.AllowExternalDiscovery,
		"turning discovery OFF is the direction that matters: a community that asked to stop being "+
			"listed and stayed listed is a privacy failure, not a stale field")
}

func TestCommunityConsumer_DeletesAnIndexedProfile(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "del")
	communityDID := fixtures.DID(name)

	consumer, repo := newCommunityConsumer(t, nil)
	ctx := context.Background()

	_, err := repo.Create(ctx, &communities.Community{
		DID:          communityDID,
		Handle:       "c-" + name + "." + instanceDomain,
		Name:         name,
		OwnerDID:     communityDID,
		CreatedByDID: "did:plc:communityconsumer",
		HostedByDID:  instanceDID,
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// A delete commit carries no record at all, which is worth exercising
	// separately: every other branch dereferences commit.Record.
	require.NoError(t, consumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Did:    communityDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        testkit.TID(),
			Operation:  "delete",
			Collection: "social.coves.community.profile",
			RKey:       "self",
		},
	}))

	_, err = repo.GetByDID(ctx, communityDID)
	assert.True(t, errors.Is(err, communities.ErrCommunityNotFound),
		"the community is still indexed after its profile record was deleted, got %v", err)
}

// TestCommunityConsumer_RejectsAnyProfileRKeyButSelf pins the V2 record-key
// rule at the point it is enforced.
//
// A community profile lives at exactly one key in its repo: "self". That is
// what makes a community's profile addressable without an index — anyone
// holding the DID can construct the AT-URI — and it is what makes the record
// updatable in place rather than accumulating versions. V1 used a TID, and
// pre-production means no compatibility: a TID-keyed record is not an older
// community to migrate, it is a record no part of this system can address.
//
// The rejection is PERMANENT rather than retryable, and that distinction is
// asserted: a record key is immutable, so redriving the event a thousand times
// produces the same failure. Classifying it as transient would put it in the
// retry queue forever.
func TestCommunityConsumer_RejectsAnyProfileRKeyButSelf(t *testing.T) {
	t.Parallel()

	consumer, repo := newCommunityConsumer(t, nil)
	ctx := context.Background()

	t.Run("a V1 TID record key", func(t *testing.T) {
		name := testkit.UniqueIDWithPrefix(t, "v1k")
		communityDID := fixtures.DID(name)

		err := consumer.HandleEvent(ctx,
			profileEvent(communityDID, "create", "3k2j4h5g6f7d", "bafyv1community", profileRecord(name)))

		require.Error(t, err, "a TID-keyed community profile must be rejected")
		assert.ErrorContains(t, err,
			"invalid community profile rkey: expected 'self', got '3k2j4h5g6f7d' (V1 communities not supported)")
		assert.True(t, errors.Is(err, jetstream.ErrPermanentEvent),
			"an immutable record key can never succeed on redrive; classifying it as transient would "+
				"keep the event in the retry queue forever")

		_, err = repo.GetByDID(ctx, communityDID)
		assert.True(t, errors.Is(err, communities.ErrCommunityNotFound),
			"the rejected community was indexed anyway")
	})

	t.Run("an arbitrary record key", func(t *testing.T) {
		name := testkit.UniqueIDWithPrefix(t, "ark")
		communityDID := fixtures.DID(name)

		err := consumer.HandleEvent(ctx,
			profileEvent(communityDID, "create", "custom-profile-name", "bafycustomrkey", profileRecord(name)))

		require.Error(t, err)
		_, err = repo.GetByDID(ctx, communityDID)
		assert.True(t, errors.Is(err, communities.ErrCommunityNotFound))
	})

	t.Run("an update to a key that is not self", func(t *testing.T) {
		// The update path has its own copy of the check, and it runs BEFORE the
		// record is parsed. An update is the dangerous direction: the community
		// already exists, so a missed check would overwrite a real row from a
		// record written at a key its author chose.
		name := testkit.UniqueIDWithPrefix(t, "urk")
		communityDID := fixtures.DID(name)

		require.NoError(t, consumer.HandleEvent(ctx,
			profileEvent(communityDID, "create", "self", "bafybeforeupdate", profileRecord(name))))

		record := profileRecord(name)
		record["displayName"] = "Overwritten Through The Wrong Key"
		err := consumer.HandleEvent(ctx,
			profileEvent(communityDID, "update", "wrong-rkey", "bafyafterupdate", record))
		require.Error(t, err)

		unchanged, err := repo.GetByDID(ctx, communityDID)
		require.NoError(t, err, "the original community must still be there")
		assert.Equal(t, "Consumer Indexed", unchanged.DisplayName,
			"the rejected update was applied anyway")
	})
}

// TestCommunityConsumer_ResolvesEvenWhenTheRecordCarriesAHandle covers the
// legacy shape — a profile record with a "handle" field in it — and says what
// that field is now worth.
//
// A record's handle is a CLAIM, never a value. Handles are mutable and DIDs are
// not, so a record should not carry one at all; more to the point,
// social.coves.community.profile does not declare the property, so no PDS on
// the network validates it and any repo can write any string there. Records
// that carry one still exist — this AppView's own CreateCommunity writes it, for
// reasons tests/e2e/community_contract_test.go documents — but nothing reads it
// as authority. The handle stored is whatever the DID's own directory entry
// names, unconditionally; a record that disagrees is logged and otherwise
// ignored. See the note below on why a disagreement is a warning and not a
// refusal.
//
// This is the agreeing case, and the assertion carrying the fix is the call
// count rather than the handle. When record and directory name the same string
// the resulting row is identical whether or not resolution happened, so a
// future refactor that reinstates the short-circuit — "the record already has a
// handle, skip the round trip" — would leave every column correct and be
// invisible everywhere except here, as a 0.
//
// TestCommunityConsumer_NeverStoresAHandleTheRepoDoesNotOwn below is the other
// half of the same rule: what happens when they disagree.
func TestCommunityConsumer_ResolvesEvenWhenTheRecordCarriesAHandle(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "hnd")
	communityDID := fixtures.DID(name)
	handle := "c-" + name + "." + instanceDomain

	resolver := newStubIdentityResolver()
	resolver.resolutions[communityDID] = handle
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	record := profileRecord(name)
	record["handle"] = handle

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(communityDID, "create", "self", "bafyhandlecarried", record)))

	indexed, err := repo.GetByDID(ctx, communityDID)
	require.NoError(t, err)
	assert.Equal(t, handle, indexed.Handle)

	assert.Equal(t, 1, resolver.callCount,
		"the record's handle was stored without ever being checked against the DID that published it")
	assert.Equal(t, communityDID, resolver.lastDID,
		"the claim must be measured against the SIGNING repo's identity; asking about anything else "+
			"checks a handle nobody has to own")
}

// TestCommunityConsumer_NeverStoresAHandleTheRepoDoesNotOwn is the namespace
// takeover, written down.
//
// social.coves.community.profile does not declare a "handle" property, so no
// PDS on the network validates one. A foreign PDS has never seen a
// social.coves.* lexicon at all; it accepts the extra field and puts it on the
// firehose verbatim. That makes the record's handle an ASSERTION BY THE AUTHOR
// about a namespace the author does not necessarily own — and communities.handle
// is UNIQUE, which is what turns an assertion into a land grab.
//
// The event below is the whole attack. A repo that legitimately owns
// c-<something>.attacker.example writes a profile at rkey "self" in its OWN repo
// claiming handle c-<name>.coves.social, and hostedBy did:web:coves.social. Every
// gate that existed before this fix passed it: the rkey is "self", the record
// parses, and verifyHostedByClaim measured the CLAIMED handle's domain against
// the hostedBy domain and then against coves.social's own did.json — all of
// which agree, because the attacker copied them from the real instance. Nothing
// in that chain took the signing repo's DID as an input, so nothing in it could
// tell this event apart from the real community's.
//
// What the row would then do is the reason this is p1 rather than a wrong field.
// resolveScopedIdentifier (service.go) turns !name@coves.social into
// GetByHandle("c-name.coves.social") — so the identifier in every URL, every
// !mention and the client's subscribe button resolves to the attacker's DID, and
// subscriptions, posts and votes addressed to it land in a repo we do not host.
//
// # WHAT IS ASSERTED, AND WHAT IS DELIBERATELY NOT
//
// The claimed handle must be UNOCCUPIED afterwards. That is the whole contract.
// GetByHandle is the exact call resolveScopedIdentifier makes, so a row under
// that string — held by ANY did — is the takeover; and because
// communities.handle is UNIQUE, even an otherwise harmless row there is a
// permanent denial of registration, since the real community's own create would
// then hit ErrHandleTaken and be dead-lettered as permanent. Takeover and DoS
// are the same assertion.
//
// The spoofing repo's OWN community may exist. It is indexed under the handle
// the directory names for it — its real one, in its own namespace — because
// that is what the consumer stores now: the resolved handle, never the claimed
// one. So the attacker gets what it was always entitled to, a community at its
// own address, and nothing at ours.
//
// # WHY THE EVENT IS NOT REFUSED
//
// An earlier version of this test asserted the stronger thing: an error, and no
// row at all. That was implemented, and then reverted, and the reversal is worth
// recording here because someone will propose reinstating it.
//
// The refusal added no security. The stored handle already comes from
// resolution, so the claim cannot reach the column whatever it says — and
// verifyHostedByClaim runs on the VERIFIED handle, so this very event is
// refused in production for the honest reason: the record says
// hostedBy did:web:coves.social while the repo resolves into
// attacker.example, and those domains do not match. The string comparison was
// not what stopped the attack; resolving unconditionally was.
//
// What it cost was a PERMANENT false positive on ordinary drift. A record is a
// snapshot and handles are mutable, so the field goes stale the moment a
// community renames — and this AppView's own CreateCommunity writes that field,
// so our own communities carry one. Under the refusal, a legitimate rename made
// a community's stored record contradict its own correct resolution, and the
// event was dead-lettered without redrive. Dead-lettering real communities for
// renaming is a worse failure than indexing a forgery whose forged part is
// discarded before it reaches the database.
//
// So the disagreement is logged as a warning and the event proceeds. What must
// never happen is the claimed handle being stored, and that is what this test
// pins.
//
// In production this exact event is refused anyway, by verifyHostedByClaim —
// after the bind it measures the VERIFIED handle's domain against the hostedBy
// claim, and attacker.example is not coves.social. Every consumer built in this
// file has verification switched off, matching CI, so what is asserted here is
// deliberately narrower and correspondingly more precise: not "the event is
// refused", which depends on configuration, but "the claimed handle is never
// stored", which holds on every configuration there is.
func TestCommunityConsumer_NeverStoresAHandleTheRepoDoesNotOwn(t *testing.T) {
	t.Parallel()

	squatted := testkit.UniqueIDWithPrefix(t, "sqt")
	claimedHandle := "c-" + squatted + "." + instanceDomain

	// The attacker's repo is not a forgery and does not need to be. It is an
	// ordinary, well-formed atProto identity that resolves bidirectionally to a
	// handle it genuinely owns — in a namespace that has nothing to do with
	// ours. That is precisely what makes the record's claim a lie: the authority
	// for this DID, asked, names a different handle.
	attackerDID := fixtures.DID(squatted)
	ownedHandle := "c-" + testkit.UniqueIDWithPrefix(t, "atk") + ".attacker.example"

	resolver := newStubIdentityResolver()
	resolver.resolutions[attackerDID] = ownedHandle
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	record := profileRecord(squatted)
	record["handle"] = claimedHandle

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(attackerDID, "create", "self", "bafyspoofedhandle", record)),
		"a claim the directory disagrees with is dropped, not fatal: the same disagreement is what a "+
			"legitimate rename produces, and refusing it dead-letters real communities")

	// The one that matters, and the reason this file exists. GetByHandle is the
	// exact call resolveScopedIdentifier makes for !name@coves.social, so a row
	// here — under ANY did — is the takeover, and a row here at all is the
	// denial of registration.
	_, err := repo.GetByHandle(ctx, claimedHandle)
	assert.True(t, communities.IsNotFound(err),
		"%q is occupied: the repo that claimed it now answers for !%s@%s, and the real community "+
			"can never register the name, got %v", claimedHandle, squatted, instanceDomain, err)

	// The attacker's own community is indexed, at the attacker's own address.
	// Asserting this rather than leaving it unstated is what stops a future
	// implementation from satisfying the line above by refusing the event: that
	// was tried, and the doc comment says why it was reverted.
	indexed, err := repo.GetByDID(ctx, attackerDID)
	require.NoError(t, err, "the repo's own community must still be indexed")
	assert.Equal(t, ownedHandle, indexed.Handle,
		"the row must carry the handle the directory names for this repo, never the one it asserted")
}

// TestCommunityConsumer_NeverStoresAClaimedHandleOnUpdateEither is the same
// contract for the path the create-side fix did not touch.
//
// An attacker with a community already indexed does not have to publish a new
// profile to try this — it can EDIT the one it has, and updateCommunity is a
// separate function that was never migrated. It still short-circuits resolution
// when the record names a handle, still assigns that handle onto the row it
// loaded, and never asks whether the identity behind the repo agrees. So the
// claim reaches further on this path than on the other one.
//
// What stops it is one level down, and it is worth being precise about, because
// it is not the consumer: postgresCommunityRepo.Update does not write the handle
// column at all. Its SQL sets display name, description, facets, blobs,
// visibility, discovery, moderation, warnings, record URI and CID, origin and
// pds_url — and the in-memory assignment above it dies at that boundary. The
// takeover is closed here by the schema rather than by the code that runs first.
//
// That is exactly why this test exists at the acceptance level, driving the real
// repository, rather than only against the in-memory fake in the jetstream
// package. The fake writes the whole struct, so it cannot tell the difference
// between "the consumer refused to store the claim" and "the consumer tried and
// the SQL declined". Only the real Update can say which, and what an operator
// needs guaranteed is the OUTCOME: !<name>@coves.social still resolves to
// nothing.
//
// The resolver assertion is the part that fails today. It is not decorative:
// the update path writes pds_url from whatever it resolves, and BridgeTrust
// reads that column to decide whether a community's bridged vote counts are
// admissible — so a path that never resolves, or that believes an unverified
// resolution, is making a trust decision from the record author's assertion.
func TestCommunityConsumer_NeverStoresAClaimedHandleOnUpdateEither(t *testing.T) {
	t.Parallel()

	squatted := testkit.UniqueIDWithPrefix(t, "usq")
	claimedHandle := "c-" + squatted + "." + instanceDomain

	attackerDID := fixtures.DID(squatted)
	resolvedHandle := "c-" + testkit.UniqueIDWithPrefix(t, "urs") + ".attacker.example"

	resolver := newStubIdentityResolver()
	resolver.resolutions[attackerDID] = resolvedHandle
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	// The repo earns its row honestly first, under the handle the directory
	// names for it. Seeding through the consumer rather than through repo.Create
	// matters: it is what makes the update below an UPDATE, instead of falling
	// through updateCommunity's GetByDID-NotFound branch into the create path —
	// which is already fixed and would prove nothing.
	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(attackerDID, "create", "self", "bafybeforeclaim", profileRecord(squatted))))

	seeded, err := repo.GetByDID(ctx, attackerDID)
	require.NoError(t, err, "the community must be indexed before the update is meaningful")
	require.Equal(t, resolvedHandle, seeded.Handle)

	callsAfterCreate := resolver.callCount

	// Now the edit that makes the claim.
	record := profileRecord(squatted)
	record["handle"] = claimedHandle
	record["displayName"] = "Renamed Into Somebody Else's Namespace"

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(attackerDID, "update", "self", "bafyclaimedonupdate", record)),
		"a claim the directory disagrees with is dropped, not fatal — on this path most of all, since a "+
			"stored record's handle field goes stale on every legitimate rename")

	_, err = repo.GetByHandle(ctx, claimedHandle)
	assert.True(t, communities.IsNotFound(err),
		"%q is occupied after a profile EDIT claimed it: !%s@%s now resolves to a repo that does not "+
			"own the name, and the real community can never register it, got %v",
		claimedHandle, squatted, instanceDomain, err)

	updated, err := repo.GetByDID(ctx, attackerDID)
	require.NoError(t, err, "the community must still be indexed: the claim is dropped, not the event")
	assert.Equal(t, resolvedHandle, updated.Handle,
		"the edit moved the row onto the handle its record asked for")

	assert.Greater(t, resolver.callCount, callsAfterCreate,
		"the update path never consulted the directory, because the record named a handle. It writes "+
			"pds_url from what it resolves, and BridgeTrust reads that column to decide whether this "+
			"community's bridged vote counts are admissible — so with no resolution that trust input "+
			"comes from the record's author")
	assert.Equal(t, attackerDID, resolver.lastDID,
		"the resolution must be about the SIGNING repo; asking about anything else checks a handle "+
			"nobody has to own")
}

// TestCommunityConsumer_ResolvesAMissingHandleFromPLC is the modern path: no
// handle in the record, so the consumer asks the DID for one.
func TestCommunityConsumer_ResolvesAMissingHandleFromPLC(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "plc")
	communityDID := fixtures.DID(name)
	resolvedHandle := "c-" + name + "." + instanceDomain

	resolver := newStubIdentityResolver()
	resolver.resolutions[communityDID] = resolvedHandle
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(communityDID, "create", "self", "bafyplcresolved", profileRecord(name))))

	assert.Equal(t, 1, resolver.callCount, "the handle must be resolved exactly once per indexed community")
	assert.Equal(t, communityDID, resolver.lastDID, "resolution must be asked about the community's own DID")

	indexed, err := repo.GetByDID(ctx, communityDID)
	require.NoError(t, err)
	assert.Equal(t, resolvedHandle, indexed.Handle)

	// The PDS URL learned during resolution has to be stored, and the reason is
	// not obvious: BridgeTrust gates bridged vote counts on a post's community
	// row naming the PDS its repo lives on. A federated community indexed here
	// with an empty pds_url makes that gate default-deny for good.
	assert.Equal(t, "https://pds.example.com", indexed.PDSURL,
		"the resolved PDS host must be persisted, or bridged stats are denied for this community forever")
}

func TestCommunityConsumer_FailsRatherThanGuessWhenPLCResolutionFails(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "pfl")
	communityDID := fixtures.DID(name)

	resolver := newStubIdentityResolver()
	resolver.shouldFail = true
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	err := consumer.HandleEvent(ctx,
		profileEvent(communityDID, "create", "self", "bafyplcfailed", profileRecord(name)))

	// There is deliberately NO fallback. Constructing a handle from the record's
	// own fields would be easy and would be wrong: for a federated community the
	// constructed handle would name the wrong domain, and the row would then
	// look perfectly healthy while addressing a community nobody can reach. A
	// failed event is retried on backfill; a wrong row is not.
	require.Error(t, err, "PLC resolution failed and the community was indexed anyway")
	assert.ErrorContains(t, err, "failed to resolve handle from PLC")
	assert.Equal(t, 1, resolver.callCount, "the failure must come from the resolution attempt itself")

	_, err = repo.GetByDID(ctx, communityDID)
	assert.True(t, communities.IsNotFound(err),
		"nothing may be indexed for a community whose handle could not be established, got %v", err)
}

func TestCommunityConsumer_RefusesAProfileWhoseHandleCannotBeBuilt(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "hby")
	communityDID := fixtures.DID(name)

	// No resolver: the consumer falls back to constructing the handle from
	// hostedBy, which only works for a did:web. A did:plc there yields an empty
	// handle, and the repository — not the consumer — is what refuses it.
	consumer, repo := newCommunityConsumer(t, nil)
	ctx := context.Background()

	record := profileRecord(name)
	record["hostedBy"] = "did:plc:invalid"

	err := consumer.HandleEvent(ctx,
		profileEvent(communityDID, "create", "self", "bafybadhostedby", record))

	require.Error(t, err, "a community with no derivable handle must not be indexed")
	assert.ErrorContains(t, err, "handle is required")

	_, err = repo.GetByDID(ctx, communityDID)
	assert.True(t, communities.IsNotFound(err))
}

// TestCommunityConsumer_IndexesASubscriptionAndCountsIt covers the other
// collection this consumer owns, and the aggregate it maintains.
//
// The subscriber count is denormalised onto the community row because every
// listing renders it. A database trigger derives it from each indexed
// subscription relationship, regardless of which repository path wrote it.
func TestCommunityConsumer_IndexesASubscriptionAndCountsIt(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "sub")
	communityDID := fixtures.DID(name)

	consumer, repo := newCommunityConsumer(t, nil)
	ctx := context.Background()

	_, err := repo.Create(ctx, &communities.Community{
		DID:          communityDID,
		Handle:       "c-" + name + "." + instanceDomain,
		Name:         name,
		OwnerDID:     communityDID,
		CreatedByDID: "did:plc:communityconsumer",
		HostedByDID:  instanceDID,
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// The collection is the RECORD TYPE, social.coves.community.subscription —
	// not social.coves.community.subscribe, which is the XRPC procedure that
	// creates it. Writing the procedure NSID as a collection produces a record
	// no consumer is subscribed to: the write succeeds, the client sees 200, and
	// the subscription silently never indexes.
	userDID := "did:plc:subscriber" + name
	require.NoError(t, consumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Did:    userDID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        testkit.TID(),
			Operation:  "create",
			Collection: "social.coves.community.subscription",
			RKey:       testkit.TID(),
			CID:        "bafysubscription",
			Record: map[string]interface{}{
				"subject":           communityDID,
				"contentVisibility": 3,
				"createdAt":         time.Now().UTC().Format(time.RFC3339),
			},
		},
	}))

	subscription, err := repo.GetSubscription(ctx, userDID, communityDID)
	require.NoError(t, err, "the subscription event was accepted but nothing was indexed")
	assert.Equal(t, userDID, subscription.UserDID)
	assert.Equal(t, communityDID, subscription.CommunityDID)

	counted, err := repo.GetByDID(ctx, communityDID)
	require.NoError(t, err)
	assert.Equal(t, 1, counted.SubscriberCount,
		"the denormalised subscriber count must move with the subscription that caused it")
}
