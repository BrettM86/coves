//go:build integration

package communities_test

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
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

	repo := postgres.NewCommunityRepository(testkit.DB(t))
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

// TestCommunityConsumer_KeepsTheHandleTheRecordCarries covers the legacy shape:
// a profile record with a "handle" field in it.
//
// Handles are mutable and DIDs are not, so a record should not carry one at all
// — but records that do exist, and the consumer takes the record's word for it
// rather than resolving. What is asserted is that it is stored verbatim, since
// this is the string every client uses to address the community.
func TestCommunityConsumer_KeepsTheHandleTheRecordCarries(t *testing.T) {
	t.Parallel()

	name := testkit.UniqueIDWithPrefix(t, "hnd")
	communityDID := fixtures.DID(name)
	handle := "c-" + name + "." + instanceDomain

	// The resolver would answer with something else entirely. It must not be
	// consulted: a record carrying its own handle short-circuits resolution, and
	// this proves that rather than assuming it.
	resolver := newStubIdentityResolver()
	resolver.resolutions[communityDID] = "c-resolver-would-say-this." + instanceDomain
	consumer, repo := newCommunityConsumer(t, resolver)
	ctx := context.Background()

	record := profileRecord(name)
	record["handle"] = handle

	require.NoError(t, consumer.HandleEvent(ctx,
		profileEvent(communityDID, "create", "self", "bafyhandlecarried", record)))

	indexed, err := repo.GetByDID(ctx, communityDID)
	require.NoError(t, err)
	assert.Equal(t, handle, indexed.Handle)
	assert.Zero(t, resolver.callCount, "a record that carries a handle must not trigger PLC resolution")
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
// listing renders it, so it is only ever correct if this path maintains it. A
// subscription indexed without the increment is invisible until someone
// recounts.
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
