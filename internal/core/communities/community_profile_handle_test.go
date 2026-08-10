//go:build integration

package communities_test

import (
	"context"
	"strings"
	"testing"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The handle in the record CreateCommunity writes to the PDS.
//
// # WHY A MISSING MAP KEY IS A PIPELINE OUTAGE
//
// The community profile record is the ONLY thing that tells the AppView a
// community exists — the community consumer indexes repos it has never seen, and
// there is no signup step for communities. When the record carries no `handle`,
// the consumer falls through to resolving one from the DID document
// (community_consumer.go createCommunity), and on an egress-blocked stack that
// resolution cannot reach the PLC directory. What comes back is the reserved
// "handle.invalid".
//
// communities.handle carries a UNIQUE constraint. So the FIRST community indexed
// that way takes "handle.invalid", every subsequent one collides with it, and
// the collision is currently swallowed as an idempotent replay — the community
// is silently dropped, and every post, comment and vote naming it dead-letters
// as "community not found". One absent map key, and the visible symptom is a
// flood of unrelated transient failures four layers away.
//
// Writing the handle is not a departure from atProto's "handles are mutable,
// resolve them from DIDs" guidance. That guidance is about trusting a stranger's
// self-reported handle; this AppView PROVISIONED the account and asked the PDS
// for this exact handle, so putting it in the record states a fact it already
// holds. The consumer's resolution path stays for federated communities, where
// it is the only option available.
func TestCreateCommunity_ProfileRecordCarriesTheHandle(t *testing.T) {
	t.Parallel()

	service, _, pdsServer := newCommunityService(t)
	ctx := context.Background()

	// "c-" plus the name must stay inside the PDS' 18-character local-label cap,
	// which is why UniqueIDWithPrefix is the only generator allowed here.
	name := testkit.UniqueIDWithPrefix(t, "hc")
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
		Name:         name,
		DisplayName:  "Handle Carrier",
		Description:  "a community whose profile record must name its handle",
		Visibility:   "public",
		CreatedByDID: "did:plc:handlecarriertest",
	})
	require.NoError(t, err)
	require.NotEmpty(t, community.Handle, "fixture: the service must have derived a handle")

	// Read the record back out of the community's own repo — the bytes the
	// firehose will carry, not the service's in-memory view of them. The
	// consumer parses exactly this.
	session := pdsServer.Login(t, community.Handle, community.PDSPassword)
	record := session.GetRecord(t, "social.coves.community.profile", "self")

	handle, ok := record.Value["handle"].(string)
	require.Truef(t, ok,
		"the profile record carries no `handle` field: %#v.\n"+
			"Without it the consumer must resolve one from the DID document, which on an egress-blocked stack yields "+
			"the reserved \"handle.invalid\" — and communities.handle is UNIQUE, so the second community indexed that "+
			"way collides with the first and is dropped, taking every post that names it into the dead-letter queue "+
			"as \"community not found\"", record.Value)

	assert.Equal(t, community.Handle, handle,
		"the record's handle must be the one the service provisioned and reports; a record that disagrees with the "+
			"AppView's own row is worse than a missing field, because the index and the firehose would then be "+
			"describing two different communities")

	// The shape is asserted independently of the service's own string building,
	// so a change to the derivation has to be a deliberate edit here too.
	assert.Truef(t, strings.HasPrefix(handle, "c-"+name+"."),
		"the handle %q must be the c-{name}.{domain} form the provisioner builds; a handle in any other shape does not "+
			"resolve to this community's DID, and every client addressing it by handle 404s while the row looks healthy", handle)
	assert.NotContains(t, handle, "handle.invalid",
		"the reserved unverifiable-identity handle must never reach a record: it is the value that collides on the UNIQUE constraint")
}
