package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production LegacySource and the tool's operator surface.
//
// Everything in this file was previously untested: the tool that DELETES
// production posts had coverage of its state machine and none at all of the
// three seams that actually touch the PDS — the discovery decode, the guarded
// delete, and the not-found idempotence a resumed run leans on. Those are also
// the seams whose bugs are invisible in the state machine's fakes, because the
// fakes are written to the contract rather than to what the PDS does.

// fakeRepoClient is a minimal pds.Client over an in-memory repo, plus the
// swap-guarded delete the real client provides.
type fakeRepoClient struct {
	did      string
	host     string
	records  map[string]*pds.RecordResponse // rkey -> record
	listErr  error
	deleteOp []deleteCall
	deleteFn func(collection, rkey, swap string) error
}

type deleteCall struct {
	collection string
	rkey       string
	swap       string
}

func newFakeRepoClient(did string) *fakeRepoClient {
	return &fakeRepoClient{did: did, host: "http://pds.invalid", records: map[string]*pds.RecordResponse{}}
}

func (c *fakeRepoClient) put(collection, rkey, cid string, value map[string]any) string {
	uri := "at://" + c.did + "/" + collection + "/" + rkey
	c.records[rkey] = &pds.RecordResponse{URI: uri, CID: cid, Value: value}
	return uri
}

func (c *fakeRepoClient) DID() string     { return c.did }
func (c *fakeRepoClient) HostURL() string { return c.host }

func (c *fakeRepoClient) GetRecord(_ context.Context, _ string, rkey string) (*pds.RecordResponse, error) {
	rec, ok := c.records[rkey]
	if !ok {
		return nil, fmt.Errorf("getRecord: %w: no such record", pds.ErrNotFound)
	}
	return rec, nil
}

func (c *fakeRepoClient) ListRecords(_ context.Context, _ string, _ int, cursor string) (*pds.ListRecordsResponse, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	if cursor != "" {
		return &pds.ListRecordsResponse{}, nil
	}
	out := &pds.ListRecordsResponse{}
	for _, rec := range c.records {
		out.Records = append(out.Records, pds.RecordEntry{URI: rec.URI, CID: rec.CID, Value: rec.Value})
	}
	return out, nil
}

func (c *fakeRepoClient) DeleteRecordWithSwap(_ context.Context, collection, rkey, swap string) error {
	c.deleteOp = append(c.deleteOp, deleteCall{collection: collection, rkey: rkey, swap: swap})
	if c.deleteFn != nil {
		return c.deleteFn(collection, rkey, swap)
	}
	delete(c.records, rkey)
	return nil
}

// sourceOver builds a realLegacySource whose community clients are the supplied
// fakes, bypassing the credential plumbing that needs a database.
func sourceOver(clients map[string]*fakeRepoClient, scope string) *realLegacySource {
	return &realLegacySource{
		communityFilter: scope,
		hostedDIDs: func(context.Context) ([]string, error) {
			dids := make([]string, 0, len(clients))
			for did := range clients {
				dids = append(dids, did)
			}
			return dids, nil
		},
		openRepo: func(_ context.Context, did string) (repoClient, error) {
			c, ok := clients[did]
			if !ok {
				return nil, fmt.Errorf("no fake client for %s", did)
			}
			return c, nil
		},
	}
}

// ---- legacyPostFromEntry: the lossless-conversion source -------------------

// RawRecord is where the postv2's body comes from. If it were rebuilt from the
// decoded PostRecord instead, langs/tags/crosspostOf/crosspostChain/bridgedStats
// would be dropped before the only copy of the record was deleted.
func TestLegacyPostFromEntry_CarriesTheRawRecordThroughVerbatim(t *testing.T) {
	value := map[string]any{
		"$type":          "social.coves.community.post",
		"community":      "did:plc:community2222222222222222",
		"author":         "did:plc:author11111111111111111",
		"title":          "a post",
		"createdAt":      "2026-01-02T03:04:05Z",
		"langs":          []any{"en"},
		"tags":           []any{"golang"},
		"crosspostOf":    map[string]any{"uri": "at://did:plc:x/social.coves.community.postv2/abc", "cid": "bafyx"},
		"crosspostChain": []any{map[string]any{"uri": "at://did:plc:x/social.coves.community.postv2/abc", "cid": "bafyx"}},
		"bridgedStats":   map[string]any{"upvotes": float64(7)},
	}

	got, err := legacyPostFromEntry("did:plc:community2222222222222222", pds.RecordEntry{
		URI:   "at://did:plc:community2222222222222222/social.coves.community.post/3kabc",
		CID:   "bafylegacy",
		Value: value,
	})
	require.NoError(t, err)

	assert.Equal(t, "did:plc:author11111111111111111", got.AuthorDID)
	assert.Equalf(t, "bafylegacy", got.CID,
		"the entry CID must be carried: it is the value the pre-delete re-read is checked against and the swap guard the delete is sent under")
	for field, want := range value {
		assert.Equalf(t, want, got.RawRecord[field],
			"RawRecord dropped or altered %q. The postv2 is built from this map; a field lost here is lost from the record before the original is deleted", field)
	}
}

func TestLegacyPostFromEntry_RefusesARecordWithNoAuthor(t *testing.T) {
	_, err := legacyPostFromEntry("did:plc:community2222222222222222", pds.RecordEntry{
		URI:   "at://did:plc:community2222222222222222/social.coves.community.post/3kabc",
		CID:   "bafylegacy",
		Value: map[string]any{"$type": "social.coves.community.post", "title": "orphan"},
	})
	require.Errorf(t, err,
		"a legacy record with no author field must fail discovery: there is no repo to re-author it under, and guessing one is the forgery the whole flip removes")
}

func TestLegacyPostFromEntry_RefusesARecordWithNoCID(t *testing.T) {
	_, err := legacyPostFromEntry("did:plc:community2222222222222222", pds.RecordEntry{
		URI:   "at://did:plc:community2222222222222222/social.coves.community.post/3kabc",
		Value: map[string]any{"$type": "social.coves.community.post", "author": "did:plc:author11111111111111111"},
	})
	require.Errorf(t, err,
		"a legacy record listed without a CID must fail discovery: with no CID there is nothing to guard the delete on, and the tool would fall back to deleting whatever stands")
}

// ---- DeleteLegacyPost ------------------------------------------------------

// The delete is the irreversible step, and it must be GUARDED. An unguarded
// delete removes whatever stands — including an edit that landed after the
// postv2 was built from an earlier version.
func TestDeleteLegacyPost_SendsTheSourceCIDAsTheSwapGuard(t *testing.T) {
	community := newFakeRepoClient("did:plc:community2222222222222222")
	uri := community.put(legacyPostCollection, "3kabc", "bafylegacy", map[string]any{"title": "t"})
	source := sourceOver(map[string]*fakeRepoClient{community.did: community}, "")

	err := source.DeleteLegacyPost(context.Background(), posts.LegacyPost{
		URI: uri, CID: "bafylegacy", CommunityDID: community.did,
	}, "bafylegacy")
	require.NoError(t, err)

	require.Lenf(t, community.deleteOp, 1, "exactly one delete must have been issued")
	assert.Equalf(t, "bafylegacy", community.deleteOp[0].swap,
		"the delete was sent WITHOUT the source CID as swapRecord. The PDS is the only place the CID check and the delete happen atomically; "+
			"without the guard, an edit landing between the tool's check and its delete is destroyed")
	assert.Equal(t, "3kabc", community.deleteOp[0].rkey)
	assert.Equal(t, legacyPostCollection, community.deleteOp[0].collection)
}

func TestDeleteLegacyPost_RefusesAnEmptySwapCID(t *testing.T) {
	community := newFakeRepoClient("did:plc:community2222222222222222")
	uri := community.put(legacyPostCollection, "3kabc", "bafylegacy", map[string]any{"title": "t"})
	source := sourceOver(map[string]*fakeRepoClient{community.did: community}, "")

	err := source.DeleteLegacyPost(context.Background(), posts.LegacyPost{
		URI: uri, CID: "bafylegacy", CommunityDID: community.did,
	}, "")
	require.Errorf(t, err,
		"a delete with no swap CID must be refused outright; 'I have no CID to guard on' is exactly the state in which a delete must not proceed")
	assert.Emptyf(t, community.deleteOp, "no delete may reach the PDS when there is nothing to guard it with")
}

// A delete of an already-gone record is SUCCESS. It is the step a crash after
// the migrated checkpoint retries, so idempotence is the contract — and a
// resumed run that treated not-found as a failure could never finish.
func TestDeleteLegacyPost_NotFoundIsSuccess(t *testing.T) {
	community := newFakeRepoClient("did:plc:community2222222222222222")
	community.deleteFn = func(string, string, string) error {
		return fmt.Errorf("deleteRecord: %w: no such record", pds.ErrNotFound)
	}
	source := sourceOver(map[string]*fakeRepoClient{community.did: community}, "")

	err := source.DeleteLegacyPost(context.Background(), posts.LegacyPost{
		URI:          "at://" + community.did + "/social.coves.community.post/3kgone",
		CID:          "bafylegacy",
		CommunityDID: community.did,
	}, "bafylegacy")
	assert.NoErrorf(t, err,
		"a delete of an already-gone record must report success: it is the step a crash after the migrated checkpoint retries, and a resumed run that "+
			"treated not-found as failure could never reach done")
}

// A LOST SWAP IS NOT SUCCESS. It means the record changed under us, which is the
// exact case the guard exists to catch.
func TestDeleteLegacyPost_SwapConflictIsAnError(t *testing.T) {
	community := newFakeRepoClient("did:plc:community2222222222222222")
	community.deleteFn = func(string, string, string) error {
		return fmt.Errorf("deleteRecord: %w: InvalidSwap", pds.ErrSwapConflict)
	}
	source := sourceOver(map[string]*fakeRepoClient{community.did: community}, "")

	err := source.DeleteLegacyPost(context.Background(), posts.LegacyPost{
		URI:          "at://" + community.did + "/social.coves.community.post/3kabc",
		CID:          "bafylegacy",
		CommunityDID: community.did,
	}, "bafystale")
	require.Errorf(t, err,
		"a lost swap must surface as an error: the record carries a different CID than the postv2 was built from, so the delete would destroy unmigrated content")
	assert.Containsf(t, err.Error(), "changed",
		"the error must say the record changed, so a 3am operator is not left decoding 'InvalidSwap'")
}

// ---- scope enforcement -----------------------------------------------------

// The -community filter must narrow DISCOVERY, and it must also make a delete
// outside the scope impossible: the ledger reconcile pass reaches records the
// discovery pass never listed.
func TestListLegacyPosts_CommunityFilterNarrowsDiscovery(t *testing.T) {
	inScope := newFakeRepoClient("did:plc:inscope22222222222222222")
	outOfScope := newFakeRepoClient("did:plc:outscope3333333333333333")
	inScope.put(legacyPostCollection, "3kin", "bafyin", map[string]any{
		"$type": legacyPostCollection, "author": "did:plc:author11111111111111111", "title": "in",
	})
	outOfScope.put(legacyPostCollection, "3kout", "bafyout", map[string]any{
		"$type": legacyPostCollection, "author": "did:plc:author11111111111111111", "title": "out",
	})

	source := sourceOver(map[string]*fakeRepoClient{
		inScope.did:    inScope,
		outOfScope.did: outOfScope,
	}, inScope.did)

	found, err := source.ListLegacyPosts(context.Background())
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equalf(t, inScope.did, found[0].CommunityDID,
		"a scoped run listed a record outside its scope; discovery is where the staged rollout's boundary starts")
}

func TestDeleteLegacyPost_RefusesACommunityOutsideTheScope(t *testing.T) {
	inScope := newFakeRepoClient("did:plc:inscope22222222222222222")
	outOfScope := newFakeRepoClient("did:plc:outscope3333333333333333")
	outOfScope.put(legacyPostCollection, "3kout", "bafyout", map[string]any{"title": "out"})

	source := sourceOver(map[string]*fakeRepoClient{
		inScope.did:    inScope,
		outOfScope.did: outOfScope,
	}, inScope.did)

	err := source.DeleteLegacyPost(context.Background(), posts.LegacyPost{
		URI:          "at://" + outOfScope.did + "/social.coves.community.post/3kout",
		CID:          "bafyout",
		CommunityDID: outOfScope.did,
	}, "bafyout")
	require.Errorf(t, err,
		"a staged run deleted a record belonging to a community outside its scope. The ledger reconcile pass reaches rows discovery never listed, so the "+
			"scope has to be enforced at the delete, not only at discovery")
	assert.Emptyf(t, outOfScope.deleteOp, "no delete may reach a community outside the run's scope")
}

// ---- ReadLegacyPost --------------------------------------------------------

func TestReadLegacyPost_ReportsTheCurrentCIDAndAbsence(t *testing.T) {
	community := newFakeRepoClient("did:plc:community2222222222222222")
	uri := community.put(legacyPostCollection, "3kabc", "bafycurrent", map[string]any{
		"$type": legacyPostCollection, "author": "did:plc:author11111111111111111", "title": "t",
	})
	source := sourceOver(map[string]*fakeRepoClient{community.did: community}, "")

	got, found, err := source.ReadLegacyPost(context.Background(), uri)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equalf(t, "bafycurrent", got.CID,
		"the fresh read must report the CID the record carries NOW; it is the whole point of re-reading before the delete")

	_, found, err = source.ReadLegacyPost(context.Background(), "at://"+community.did+"/social.coves.community.post/3kgone")
	require.NoErrorf(t, err, "a record that is simply gone is not an error — a resumed run whose delete already landed meets exactly this")
	assert.Falsef(t, found, "an absent record must be reported as absent, not as an error and not as an empty record")
}

// ---- the duplicated scope list ---------------------------------------------

// The tool resumes the SAME sessions cmd/server mints, so the two scope lists
// must agree. A comment in main.go already claims they must; nothing checked it,
// and cmd/server's own test cannot — it is a different package.
func TestOAuthScopes_MatchTheServerScopeList(t *testing.T) {
	toolScopes := oauthScopes()

	require.NotEmpty(t, toolScopes)
	assert.Equalf(t, serverOAuthScopesForComparison(), toolScopes,
		"the tool's OAuth scope list has drifted from cmd/server's. A session this tool resumes carries the scopes the SERVER granted, so a scope the "+
			"tool believes it has and the server never asked for is refused at the first write — mid-migration, after records have already been deleted.\n"+
			"tool:   %v\nserver: %v", toolScopes, serverOAuthScopesForComparison())
}

// serverOAuthScopesForComparison is cmd/server's oauthScopes() transcribed. It
// is a literal copy on purpose: the two binaries are different packages, so the
// only way to compare them in a test is to state one of them here and let this
// assertion fail when either moves.
func serverOAuthScopesForComparison() []string {
	return []string{
		"atproto",
		"blob:*/*",
		"repo:social.coves.community.postv2?action=create&action=update&action=delete",
		"repo:social.coves.community.post?action=create&action=update&action=delete",
		"repo:social.coves.community.comment?action=create&action=update&action=delete",
		"repo:social.coves.community.profile?action=create&action=update&action=delete",
		"repo:social.coves.community.subscription?action=create&action=update&action=delete",
		"repo:social.coves.actor.profile?action=create&action=update&action=delete",
		"repo:social.coves.feed.vote?action=create&action=delete",
		"repo:social.coves.actor.block?action=create&action=delete",
		communities.CommunityBlockOAuthScope,
	}
}

// The scope that lets the tool DELETE the legacy records has to be there, and it
// is the one a well-meaning cleanup of the deprecated collection would remove
// first.
func TestOAuthScopes_GrantTheLegacyDeleteTheDrainDependsOn(t *testing.T) {
	var legacy string
	for _, s := range oauthScopes() {
		if strings.HasPrefix(s, "repo:"+posts.LegacyPostCollection) {
			legacy = s
		}
	}
	require.NotEmptyf(t, legacy,
		"the tool has no scope for %s. Every legacy record is deleted through a session minted with these scopes, so without it the drain strands "+
			"the entire corpus undeleteable", posts.LegacyPostCollection)
	assert.Containsf(t, legacy, "action=delete",
		"the legacy-post scope %q grants no delete; the drain's final step is exactly that delete", legacy)
}
