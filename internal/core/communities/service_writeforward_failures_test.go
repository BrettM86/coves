package communities_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
)

// What subscribing, unsubscribing, blocking and unblocking do when something
// goes wrong.
//
// service_writeforward_test.go (integration) owns the happy paths: it proves
// against a real PDS that the right record, under the right collection, lands
// in the right repository. It cannot cheaply cover the failure branches,
// because provoking a 409 or an expired token from a real PDS mid-test means
// arranging a race or corrupting a session. Those branches are where the
// interesting decisions live — half of BlockCommunity is a conflict-recovery
// path that nothing exercised before this file — so they are covered here
// against a fake client.
//
// The recurring question in every case below is what the CALLER is told.
// Subscribe and block are user-initiated writes to somebody else's repository
// over a token that expires, so the service is constantly choosing between
// three answers: this is your fault (400), your session is dead (401), or the
// write already happened (409/200). Choosing wrong is not a lost write; it is a
// client that retries forever, or one that reports success on nothing.

// fakeUserPDS is the pds.Client the community service gets for a user session.
type fakeUserPDS struct {
	t   *testing.T
	did string

	createErr error
	deleteErr error

	created []fakeWrite
	deleted []fakeWrite
}

type fakeWrite struct {
	collection string
	rkey       string
	record     map[string]interface{}
}

func (f *fakeUserPDS) CreateRecord(_ context.Context, collection, rkey string, record any) (string, string, error) {
	if f.createErr != nil {
		return "", "", f.createErr
	}
	asMap, ok := record.(map[string]interface{})
	require.Truef(f.t, ok, "the service passed a %T rather than a record map", record)
	f.created = append(f.created, fakeWrite{collection: collection, rkey: rkey, record: asMap})
	return "at://" + f.did + "/" + collection + "/" + rkey, "bafycid" + rkey, nil
}

func (f *fakeUserPDS) DeleteRecord(_ context.Context, collection, rkey string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, fakeWrite{collection: collection, rkey: rkey})
	return nil
}

func (f *fakeUserPDS) DID() string     { return f.did }
func (f *fakeUserPDS) HostURL() string { return "https://pds.invalid" }

func (f *fakeUserPDS) ListRecords(context.Context, string, int, string) (*pds.ListRecordsResponse, error) {
	f.t.Fatal("the community service listed records, which none of these paths does")
	return nil, nil
}

func (f *fakeUserPDS) GetRecord(context.Context, string, string) (*pds.RecordResponse, error) {
	f.t.Fatal("the community service read a record back on a write path")
	return nil, nil
}

func (f *fakeUserPDS) PutRecord(context.Context, string, string, any, string) (string, string, error) {
	f.t.Fatal("subscriptions and blocks are created and deleted, never updated in place")
	return "", "", nil
}

func (f *fakeUserPDS) UploadBlob(context.Context, []byte, string) (*blobs.BlobRef, error) {
	f.t.Fatal("no blob belongs on a subscribe or block path")
	return nil, nil
}

const subscriberDID = "did:plc:subscriber0000000000"

func userSession(t *testing.T) *oauth.ClientSessionData {
	t.Helper()
	did, err := syntax.ParseDID(subscriberDID)
	require.NoError(t, err)
	return &oauth.ClientSessionData{AccountDID: did, AccessToken: "test-access-token"}
}

// writeForwardService wires the service to a fake repository and a fake PDS
// client, and seeds one public community to act on.
func writeForwardService(t *testing.T) (communities.Service, *fakeCommunityRepo, *fakeUserPDS) {
	t.Helper()
	repo := newFakeCommunityRepo()
	repo.seed(&communities.Community{
		DID:        seededCommunityDID,
		Handle:     seededCommunityHandle,
		Name:       "gardening",
		Visibility: "public",
	})
	userPDS := &fakeUserPDS{t: t, did: subscriberDID}
	service := communities.NewCommunityServiceWithPDSFactory(
		repo, "http://pds.invalid", fakeInstanceDID, fakeInstanceDomain, nil,
		func(context.Context, *oauth.ClientSessionData) (pds.Client, error) { return userPDS, nil }, nil,
		communities.PrivateHostOptions(true)...)
	return service, repo, userPDS
}

// unbuildableClientService is the same service with a PDS factory that always
// fails, which is what a revoked DPoP key or an unreachable session store looks
// like from here.
func unbuildableClientService(t *testing.T, cause error) (communities.Service, *fakeCommunityRepo) {
	t.Helper()
	repo := newFakeCommunityRepo()
	repo.seed(&communities.Community{
		DID: seededCommunityDID, Handle: seededCommunityHandle, Name: "gardening", Visibility: "public",
	})
	service := communities.NewCommunityServiceWithPDSFactory(
		repo, "http://pds.invalid", fakeInstanceDID, fakeInstanceDomain, nil,
		func(context.Context, *oauth.ClientSessionData) (pds.Client, error) { return nil, cause }, nil,
		communities.PrivateHostOptions(true)...)
	return service, repo
}

func TestSubscribeToCommunity_Failures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("refuses a private community", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		repo.seed(&communities.Community{
			DID:        "did:plc:privatecommunity00000",
			Handle:     "c-secret." + fakeInstanceDomain,
			Name:       "secret",
			Visibility: "private",
		})

		_, err := service.SubscribeToCommunity(ctx, userSession(t), "did:plc:privatecommunity00000", 3)
		require.ErrorIs(t, err, communities.ErrUnauthorized,
			"a private community is invitation-only, and the check has to happen before the write: "+
				"the record lands in the USER's repo, so once it is written the AppView has no way to "+
				"un-write it and the consumer would index the subscription anyway")
		assert.Empty(t, userPDS.created)
	})

	t.Run("stops at a community that does not exist", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)

		_, err := service.SubscribeToCommunity(ctx, userSession(t), "!nosuchthing@"+fakeInstanceDomain, 3)
		require.Error(t, err)
		assert.ErrorContains(t, err, "subscribe:",
			"the error must say which operation failed; three call sites resolve identifiers the same "+
				"way and an unprefixed 'community not found' names none of them")
		assert.Empty(t, userPDS.created)
	})

	t.Run("propagates a repository failure rather than treating it as absence", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		repo.err = errRepositoryUnavailable

		_, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 3)
		require.ErrorIs(t, err, errRepositoryUnavailable)
		assert.Empty(t, userPDS.created)
	})

	t.Run("reports a session it cannot build a client for", func(t *testing.T) {
		t.Parallel()
		service, _ := unbuildableClientService(t, errors.New("dpop key rotated"))

		_, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 3)
		require.ErrorContains(t, err, "dpop key rotated")
	})

	t.Run("maps an expired session to unauthorized", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.createErr = pds.ErrSessionExpired

		_, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 3)
		require.ErrorIs(t, err, communities.ErrUnauthorized,
			"the handler turns this into a 401 so the client re-authenticates; anything else is a 500 "+
				"and the client retries a request that cannot succeed until the user signs in")
	})

	t.Run("does not map an unrelated PDS failure to unauthorized", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.createErr = errors.New("pds is down")

		_, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityDID, 3)
		require.Error(t, err)
		assert.NotErrorIs(t, err, communities.ErrUnauthorized,
			"an outage told to the user as 'sign in again' sends them through a login flow that will "+
				"not help")
		assert.ErrorContains(t, err, "pds is down")
	})
}

func TestUnsubscribeFromCommunity_Failures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("reports a subscription that is not there", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)

		err := service.UnsubscribeFromCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorIs(t, err, communities.ErrSubscriptionNotFound,
			"the record key comes from the indexed subscription, so there is nothing to delete and "+
				"nothing to guess: a silent success would tell a client its unsubscribe worked")
		assert.Empty(t, userPDS.deleted)
	})

	// The AppView stores the record URI it saw on the firehose. If that URI is
	// malformed — a federated client wrote something odd, or an older row
	// predates a format — there is no rkey to delete by, and issuing a delete
	// with an empty key would target a record named "" in the user's repo.
	t.Run("refuses a subscription whose record URI carries no record key", func(t *testing.T) {
		t.Parallel()
		for _, recordURI := range []string{"", "at://", "not-a-uri", "at://did:plc:x"} {
			service, repo, userPDS := writeForwardService(t)
			repo.subscription = &communities.Subscription{
				UserDID:      subscriberDID,
				CommunityDID: seededCommunityDID,
				RecordURI:    recordURI,
				SubscribedAt: time.Now(),
			}

			err := service.UnsubscribeFromCommunity(ctx, userSession(t), seededCommunityDID)
			require.Errorf(t, err, "record URI %q yields no rkey", recordURI)
			assert.ErrorContains(t, err, "invalid subscription record URI")
			assert.Emptyf(t, userPDS.deleted,
				"a delete was issued for record URI %q, which can only name the wrong record or none",
				recordURI)
		}
	})

	t.Run("maps an expired session to unauthorized", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		repo.subscription = &communities.Subscription{
			UserDID:      subscriberDID,
			CommunityDID: seededCommunityDID,
			RecordURI:    "at://" + subscriberDID + "/social.coves.community.subscription/3kabc",
		}
		userPDS.deleteErr = pds.ErrUnauthorized

		require.ErrorIs(t, service.UnsubscribeFromCommunity(ctx, userSession(t), seededCommunityDID),
			communities.ErrUnauthorized)
	})

	t.Run("reports a session it cannot build a client for", func(t *testing.T) {
		t.Parallel()
		service, repo := unbuildableClientService(t, errors.New("session store unreachable"))
		repo.subscription = &communities.Subscription{
			UserDID:      subscriberDID,
			CommunityDID: seededCommunityDID,
			RecordURI:    "at://" + subscriberDID + "/social.coves.community.subscription/3kabc",
		}

		require.ErrorContains(t, service.UnsubscribeFromCommunity(ctx, userSession(t), seededCommunityDID),
			"session store unreachable")
	})
}

// TestBlockCommunity_ConflictRecovery covers the most intricate branch in the
// package, and the one with the least coverage before this file.
//
// Blocking deliberately does NOT check for an existing block first — two
// concurrent requests would both pass such a check — so a duplicate is
// discovered as a 409 from the PDS, and the service has to decide what a 409
// means. There are three answers and they are not interchangeable:
//
//   - the block is in our index: the user already blocked this community, so
//     answer with the existing block and let the client treat it as success;
//   - the block is NOT in our index: the PDS has it and the firehose has not
//     caught up, which is normal eventual consistency, so answer with a typed
//     conflict the handler turns into a 409;
//   - the index could not be read at all: that is an outage and must not be
//     disguised as either of the above, because "already blocked" would be a
//     lie and a 409 would tell the client to stop retrying.
func TestBlockCommunity_ConflictRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("answers with the indexed block when the PDS says duplicate", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		userPDS.createErr = pds.ErrConflict
		repo.block = &communities.CommunityBlock{
			UserDID:      subscriberDID,
			CommunityDID: seededCommunityDID,
			RecordURI:    "at://" + subscriberDID + "/social.coves.community.block/3kexisting",
			BlockedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}

		block, err := service.BlockCommunity(ctx, userSession(t), seededCommunityDID)
		require.NoError(t, err,
			"blocking something already blocked is what a client does when it retries; it must be "+
				"idempotent rather than an error the user sees")
		require.NotNil(t, block)
		assert.Equal(t, "at://"+subscriberDID+"/social.coves.community.block/3kexisting", block.RecordURI,
			"the answer must be the block that exists, not a fresh one with a made-up URI")
	})

	t.Run("answers already-blocked when the index has not caught up", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.createErr = pds.ErrConflict
		// repo.block stays nil, so GetBlock answers ErrBlockNotFound.

		_, err := service.BlockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorIs(t, err, communities.ErrBlockAlreadyExists,
			"the PDS has the record and the firehose has not delivered it yet. That is a 409, not a "+
				"500: nothing is broken and the client should not retry")
	})

	t.Run("does not disguise a datastore outage as already-blocked", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		userPDS.createErr = pds.ErrConflict
		repo.err = errRepositoryUnavailable

		_, err := service.BlockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorIs(t, err, errRepositoryUnavailable,
			"the real failure must reach the operator. Collapsing it into ErrBlockAlreadyExists would "+
				"turn every block attempt during a database outage into a cheerful 409")
		assert.NotErrorIs(t, err, communities.ErrBlockAlreadyExists)
	})
}

func TestBlockCommunity_Failures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("stops at a community that does not exist", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)

		_, err := service.BlockCommunity(ctx, userSession(t), "!nosuchthing@"+fakeInstanceDomain)
		require.Error(t, err)
		assert.ErrorContains(t, err, "block:")
		assert.Empty(t, userPDS.created)
	})

	t.Run("maps an expired session to unauthorized", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.createErr = pds.ErrForbidden

		_, err := service.BlockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorIs(t, err, communities.ErrUnauthorized)
	})

	t.Run("reports a session it cannot build a client for", func(t *testing.T) {
		t.Parallel()
		service, _ := unbuildableClientService(t, errors.New("dpop key rotated"))

		_, err := service.BlockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorContains(t, err, "dpop key rotated")
	})

	t.Run("propagates an unrelated PDS failure", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)
		userPDS.createErr = errors.New("pds is down")

		_, err := service.BlockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorContains(t, err, "pds is down")
		assert.NotErrorIs(t, err, communities.ErrBlockAlreadyExists)
	})
}

func TestUnblockCommunity_Failures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("reports a block that is not there", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)

		err := service.UnblockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorIs(t, err, communities.ErrBlockNotFound)
		assert.Empty(t, userPDS.deleted)
	})

	t.Run("refuses a block whose record URI carries no record key", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		repo.block = &communities.CommunityBlock{
			UserDID: subscriberDID, CommunityDID: seededCommunityDID, RecordURI: "at://",
		}

		err := service.UnblockCommunity(ctx, userSession(t), seededCommunityDID)
		require.ErrorContains(t, err, "invalid block record URI")
		assert.Empty(t, userPDS.deleted)
	})

	t.Run("maps an expired session to unauthorized", func(t *testing.T) {
		t.Parallel()
		service, repo, userPDS := writeForwardService(t)
		repo.block = &communities.CommunityBlock{
			UserDID:      subscriberDID,
			CommunityDID: seededCommunityDID,
			RecordURI:    "at://" + subscriberDID + "/social.coves.community.block/3kabc",
		}
		userPDS.deleteErr = pds.ErrSessionExpired

		require.ErrorIs(t, service.UnblockCommunity(ctx, userSession(t), seededCommunityDID),
			communities.ErrUnauthorized)
	})

	t.Run("stops at a community that does not exist", func(t *testing.T) {
		t.Parallel()
		service, _, _ := writeForwardService(t)

		err := service.UnblockCommunity(ctx, userSession(t), "!nosuchthing@"+fakeInstanceDomain)
		require.Error(t, err)
		assert.ErrorContains(t, err, "unblock:")
	})

	t.Run("reports a session it cannot build a client for", func(t *testing.T) {
		t.Parallel()
		service, repo := unbuildableClientService(t, errors.New("session store unreachable"))
		repo.block = &communities.CommunityBlock{
			UserDID:      subscriberDID,
			CommunityDID: seededCommunityDID,
			RecordURI:    "at://" + subscriberDID + "/social.coves.community.block/3kabc",
		}

		require.ErrorContains(t, service.UnblockCommunity(ctx, userSession(t), seededCommunityDID),
			"session store unreachable")
	})
}

// TestWriteForward_RecordsNameTheirCollectionAndSubject is the T0 restatement of
// the one claim service_writeforward_test.go makes against a real PDS, kept here
// because it is free and because the collection name is the single most
// consequential string in these methods: a subscription written under the
// PROCEDURE nsid succeeds, returns 200, and is never indexed by anything.
func TestWriteForward_RecordsNameTheirCollectionAndSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("subscription", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)

		subscription, err := service.SubscribeToCommunity(ctx, userSession(t), seededCommunityHandle, 4)
		require.NoError(t, err)
		require.Len(t, userPDS.created, 1)
		write := userPDS.created[0]

		assert.Equal(t, "social.coves.community.subscription", write.collection,
			"social.coves.community.subscribe is the XRPC procedure; a record written under it is "+
				"invisible to every consumer")
		assert.Equal(t, "social.coves.community.subscription", write.record["$type"])
		assert.Equal(t, seededCommunityDID, write.record["subject"],
			"the record must name the resolved DID: a handle here breaks the moment the community "+
				"renames, and the consumer joins on DID")
		assert.Equal(t, 4, write.record["contentVisibility"])
		assert.Equal(t, seededCommunityDID, subscription.CommunityDID)
		assert.Equal(t, subscriberDID, subscription.UserDID)

		_, err = syntax.ParseTID(write.rkey)
		assert.NoErrorf(t, err, "the record key %q is not a TID", write.rkey)
	})

	t.Run("block", func(t *testing.T) {
		t.Parallel()
		service, _, userPDS := writeForwardService(t)

		block, err := service.BlockCommunity(ctx, userSession(t), "!gardening@"+fakeInstanceDomain)
		require.NoError(t, err)
		require.Len(t, userPDS.created, 1)
		write := userPDS.created[0]

		assert.Equal(t, "social.coves.community.block", write.collection)
		assert.Equal(t, "social.coves.community.block", write.record["$type"])
		assert.Equal(t, seededCommunityDID, write.record["subject"])
		assert.Equal(t, seededCommunityDID, block.CommunityDID,
			"the block returned to the client must name the community that was resolved, not the "+
				"scoped string the client typed")

		_, err = syntax.ParseTID(write.rkey)
		assert.NoErrorf(t, err, "the record key %q is not a TID", write.rkey)
	})
}
