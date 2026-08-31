//go:build integration

package jetstream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a handle collision does to a community profile event.
//
// # THE SWALLOW, AND WHY IT LOOKS REASONABLE
//
// createCommunity ends in repo.Create, and treats communities.IsConflict as
// "already indexed — idempotent, nothing to do" (community_consumer.go). That
// reading is correct for exactly one of the two conflicts IsConflict matches.
//
//   - ErrCommunityAlreadyExists means the DID is already in the table. That IS
//     an idempotent replay: the community this event describes is indexed, the
//     event changed nothing, and returning nil is right. Jetstream redelivers
//     constantly, so this path is walked all the time.
//   - ErrHandleTaken means a DIFFERENT DID already holds this handle. Nothing
//     about that is idempotent. The community in the event was NOT indexed, is
//     not in the table under any DID, and never will be — and the AppView says
//     nothing, logs it as a successful replay, and moves on.
//
// The consequence is not confined to the community. Posts, comments and votes
// naming it are refused as "community not found", which the taxonomy classifies
// UNRESOLVED (correctly — it is ordinarily a delivery race). They bypass the
// serial lane now, but a swallowed collision still turns into a sustained
// dead-letter and redrive flood in three other consumers, none of which points
// anywhere near here.
//
// So the pin is in two halves, and both are needed: the collision must surface,
// and the genuine replay must keep NOT surfacing. A fix that widened the error
// into every conflict would dead-letter every redelivered profile event in the
// system.

const conflictProfileCollection = "social.coves.community.profile"

// communityProfileEvent builds the commit a community's profile write produces.
// The handle is carried in the record, which is the shape the AppView's own
// CreateCommunity produces once it stops omitting the field
// (internal/core/communities/community_profile_handle_test.go).
func communityProfileEvent(did, handle, name, rev string) *JetstreamEvent {
	return &JetstreamEvent{
		Did:    did,
		Kind:   "commit",
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Rev:        rev,
			Operation:  "create",
			Collection: conflictProfileCollection,
			RKey:       "self",
			CID:        "bafyconflict" + rev,
			Record: map[string]interface{}{
				"$type":       conflictProfileCollection,
				"handle":      handle,
				"name":        name,
				"displayName": "Conflict " + name,
				"createdBy":   "did:plc:conflicttestcreator",
				"hostedBy":    "did:web:test.local",
				"visibility":  "public",
				"federation":  map[string]interface{}{"allowExternalDiscovery": true},
				"createdAt":   time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
}

func TestCommunityConsumer_HandleTakenByAnotherDID_IsAPermanentRefusal(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	// skipVerification: the hostedBy/handle-domain check is a different security
	// property with its own tests, and leaving it on would reject these events
	// before the conflict path is ever reached.
	consumer := NewCommunityEventConsumer(postgres.NewCommunityRepository(db), "did:web:test.local", true, nil)

	suffix := testkit.UniqueID(t)
	contested := fmt.Sprintf("c-first%s.test.local", suffix)
	incumbent := fixtures.DID("incumbent" + suffix)
	newcomer := fixtures.DID("newcomer" + suffix)

	require.NoError(t, consumer.HandleEvent(ctx, communityProfileEvent(incumbent, contested, "first"+suffix, "3lconflicta")),
		"fixture: the first community must index cleanly")

	// A SECOND, DIFFERENT community claiming the same handle. In production this
	// is what two communities resolving to "handle.invalid" look like, but it is
	// equally what a genuine handle race or a hostile duplicate looks like — and
	// none of them is an idempotent replay.
	err := consumer.HandleEvent(ctx, communityProfileEvent(newcomer, contested, "second"+suffix, "3lconflictb"))

	require.Errorf(t, err,
		"a profile event whose handle is already held by a DIFFERENT community (%s) was accepted as an idempotent replay. "+
			"The community was never indexed, and every post naming it will dead-letter as \"community not found\" with "+
			"nothing in the logs pointing here", incumbent)
	assert.ErrorIsf(t, err, ErrPermanentEvent,
		"the refusal must be PERMANENT. A handle held by another DID does not resolve itself by waiting, so a transient "+
			"classification spends the connector's full inline retry budget (~4.2s, blocking the consumer) and then ten "+
			"redrives, per delivery, forever")
	assert.Containsf(t, err.Error(), contested,
		"the error must name the contested handle: it is the only thing that makes this diagnosable from a log line")

	// The newcomer is genuinely absent, and the incumbent is untouched — a
	// refusal that had partially applied would be worse than the swallow.
	var newcomerRows, incumbentRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, newcomer).Scan(&newcomerRows))
	assert.Zero(t, newcomerRows, "the refused community must not be indexed")

	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM communities WHERE did = $1 AND handle = $2`, incumbent, contested).Scan(&incumbentRows))
	assert.Equal(t, 1, incumbentRows, "the community that legitimately holds the handle must be untouched by the refusal")
}

func TestCommunityConsumer_SameDIDReplay_StaysSilent(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	consumer := NewCommunityEventConsumer(postgres.NewCommunityRepository(db), "did:web:test.local", true, nil)

	suffix := testkit.UniqueID(t)
	handle := fmt.Sprintf("c-replay%s.test.local", suffix)
	did := fixtures.DID("replay" + suffix)

	event := communityProfileEvent(did, handle, "replay"+suffix, "3lreplaya")
	require.NoError(t, consumer.HandleEvent(ctx, event))

	// THE OTHER HALF OF THE NARROWING, and the reason it has to be pinned
	// alongside the collision rather than left implied. The connector rewinds
	// its cursor five seconds after every reconnect and the AppView consumes
	// overlapping feeds, so this exact commit is guaranteed to be redelivered —
	// constantly, for every community. A fix that widened the refusal to every
	// conflict would dead-letter all of it.
	require.NoError(t, consumer.HandleEvent(ctx, event),
		"a redelivered profile event for the SAME DID is a genuine idempotent replay and must stay a silent no-op; "+
			"refusing it would dead-letter every community's profile on every cursor rewind")

	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, did).Scan(&rows))
	assert.Equal(t, 1, rows, "a replay must not produce a second row")
}

func TestCommunityConsumer_UnverifiableHandle_IsNotStored(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	// A record with NO handle — the federated shape, where resolution is the
	// only option — and a resolver that cannot verify the DID's handle. atProto
	// identity resolution reports that as the reserved "handle.invalid" rather
	// than as an error, so the consumer receives a perfectly well-formed
	// identity naming a handle that is not one.
	did := fixtures.DID("unverified" + testkit.UniqueID(t))
	resolver := &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		did: {DID: did, Handle: invalidHandle, PDSURL: "https://pds.example.invalid"},
	}}
	consumer := NewCommunityEventConsumer(postgres.NewCommunityRepository(db), "did:web:test.local", true, resolver)

	event := communityProfileEvent(did, "", "unverified", "3lunverified")
	delete(event.Commit.Record, "handle")

	err := consumer.HandleEvent(ctx, event)

	// This is the guard authorpost.go already applies on the user path, for the
	// identical reason: "handle.invalid" is a PLACEHOLDER, not a handle, and the
	// column it would land in is UNIQUE. Store it once and the next unverifiable
	// community collides with it — which is the collision the test above pins,
	// arriving from a completely different direction and with no attacker
	// involved.
	require.Errorf(t, err,
		"a community whose handle could not be verified was indexed anyway. \"handle.invalid\" is the reserved "+
			"placeholder for exactly this case, and communities.handle is UNIQUE — so the FIRST one indexed takes the "+
			"placeholder and every later one collides with it")
	assert.NotErrorIsf(t, err, ErrPermanentEvent,
		"an unverifiable handle is a RESOLUTION failure, not a property of the record: the PLC directory may be "+
			"unreachable right now and verifiable in a minute, so the redrive has to be allowed to succeed")

	var stored int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM communities WHERE handle = $1`, invalidHandle).Scan(&stored))
	assert.Zerof(t, stored,
		"%q was written into communities.handle; the next community that cannot resolve will collide with it", invalidHandle)
}
