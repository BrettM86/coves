//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type postSearchResponse struct {
	Feed []struct {
		Post struct {
			URI string `json:"uri"`
		} `json:"post"`
	} `json:"feed"`
	Cursor *string `json:"cursor"`
}

func queryPostSearch(p *pipeline, query, communityDID string) (postSearchResponse, error) {
	values := url.Values{"q": {query}}
	if communityDID != "" {
		values.Set("community", communityDID)
	}

	var out postSearchResponse
	err := p.AppView.Query(context.Background(), "social.coves.feed.searchPosts", values, &out)
	return out, err
}

// TestPostSearchContract proves the public search path follows the community's
// admission decision through the real PDS-to-AppView pipeline. The postv2 and
// acceptance ingestion contracts are owned elsewhere; this contract observes
// their composed visibility through the shipped search endpoint.
func TestPostSearchContract(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "psc")
	community := indexedCommunity(t, p, "psc", author.DID)
	needle := "needle" + testkit.UniqueID(t)
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	record := author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, needle+" in a haystack", "content searchable only after admission"))

	awaitStatus(t, p, uri, community.DID, "pending", "the searchable post to reach pending admission state")

	pendingSearchIsEmpty := func() (bool, error) {
		out, err := queryPostSearch(p, needle, community.DID)
		if err != nil {
			return false, err
		}
		return len(out.Feed) == 0, nil
	}
	p.Holds(t, "search must not surface a pending post", pendingSearchIsEmpty)

	community.PutRecord(t, acceptanceCollection, subjectRkey(uri), acceptanceRecord(uri, record.CID))
	awaitStatus(t, p, uri, community.DID, "accepted", "the community's acceptance to admit the searchable post")

	p.Await(t, "search to find the accepted post", func() (bool, error) {
		out, err := queryPostSearch(p, needle, community.DID)
		if err != nil {
			return false, err
		}
		return len(out.Feed) == 1 && out.Feed[0].Post.URI == uri, nil
	}, withReadCadence())

	unscoped, err := queryPostSearch(p, needle, "")
	require.NoErrorf(t, err, "cross-community search for the accepted post must remain available when the optional community parameter is omitted")
	require.Lenf(t, unscoped.Feed, 1,
		"the unique title token must identify exactly the accepted post in cross-community search")
	require.Equalf(t, uri, unscoped.Feed[0].Post.URI,
		"cross-community search returned a different post for the accepted post's unique title token")

	nonsense := "nonsense" + testkit.UniqueID(t)
	empty, err := queryPostSearch(p, nonsense, community.DID)
	require.NoErrorf(t, err, "a scoped search with no matches must return an empty feed rather than an endpoint error")
	require.Emptyf(t, empty.Feed,
		"a scoped nonsense query returned posts from the shared stack; community scoping and full-text matching must both apply")
}
