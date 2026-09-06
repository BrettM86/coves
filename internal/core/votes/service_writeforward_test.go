//go:build integration

package votes_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"
	"Coves/tests/testkit"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.RequirePDS)) }

// faultingVotePDS forwards every write to the real PDS. A duplicate create
// forces a real transaction rejection; a lost reply happens only after commit.
type faultingVotePDS struct {
	pds.CommitClient
	reject    bool
	loseReply bool
}

func (c *faultingVotePDS) ApplyWrites(ctx context.Context, writes []pds.Write, swap string) (*pds.ApplyWritesResult, error) {
	if c.reject {
		// Copy before injecting a duplicate so this fault never mutates the service slice.
		writes = append(append([]pds.Write(nil), writes...), writes[1])
	}
	result, err := c.CommitClient.ApplyWrites(ctx, writes, swap)
	if err == nil && c.loseReply {
		return nil, context.DeadlineExceeded
	}
	return result, err
}

func TestVoteDirectionReplacementOnRealPDS(t *testing.T) {
	server := testkit.NewPDS(t)
	account := server.CreateAccount(t, testkit.WithHandlePrefix("vat"))
	client, err := pds.NewFromAccessToken(server.URL(), account.DID, account.AccessToken, pds.PrivateHostOptions(true)...)
	require.NoError(t, err)
	commitClient, ok := client.(pds.CommitClient)
	require.True(t, ok, "the real PDS client must expose atomic commits")
	proxy := &faultingVotePDS{CommitClient: commitClient}
	cache := votes.NewVoteCache(time.Hour, nil)
	service := votes.NewServiceWithPDSFactory(nil, cache, nil, func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) { return proxy, nil })
	did, err := syntax.ParseDID(account.DID)
	require.NoError(t, err)
	session := &oauth.ClientSessionData{AccountDID: did, AccessToken: account.AccessToken}
	ctx := context.Background()
	request := votes.CreateVoteRequest{Subject: votes.StrongRef{URI: "at://" + account.DID + "/social.coves.community.post/" + testkit.TID(), CID: "bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Direction: "up"}
	up, err := service.CreateVote(ctx, session, request)
	require.NoError(t, err)
	oldKey := up.URI[strings.LastIndex(up.URI, "/")+1:]
	proxy.reject = true
	request.Direction = "down"
	_, err = service.CreateVote(ctx, session, request)
	require.Error(t, err, "the duplicate create must make the real PDS reject the whole replacement")
	original, err := client.GetRecord(ctx, "social.coves.feed.vote", oldKey)
	require.NoError(t, err, "rejection must roll back deletion of the original vote")
	require.Equal(t, "up", original.Value["direction"])
	records, err := client.ListRecords(ctx, "social.coves.feed.vote", 100, "")
	require.NoError(t, err)
	require.Len(t, records.Records, 1)
	proxy.reject = false
	proxy.loseReply = true
	down, err := service.CreateVote(ctx, session, request)
	require.NoError(t, err, "a committed replacement must be recovered after its response is lost")
	require.NotEmpty(t, down.CID)
	require.NotEqual(t, up.URI, down.URI)
	records, err = client.ListRecords(ctx, "social.coves.feed.vote", 100, "")
	require.NoError(t, err)
	require.Len(t, records.Records, 1)
	require.Equal(t, down.URI, records.Records[0].URI)
	require.Equal(t, "down", cache.GetVote(account.DID, request.Subject.URI).Direction)
	proxy.loseReply = false
	off, err := service.CreateVote(ctx, session, request)
	require.NoError(t, err)
	require.Empty(t, off.URI)
	records, err = client.ListRecords(ctx, "social.coves.feed.vote", 100, "")
	require.NoError(t, err)
	require.Empty(t, records.Records)
}
