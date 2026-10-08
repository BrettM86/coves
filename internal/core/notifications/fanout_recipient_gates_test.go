package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type notificationTestBridgeHosts struct {
	trustedURLs map[string]bool
	checkedURLs []string
}

func (checker *notificationTestBridgeHosts) TrustsPDS(pdsURL string) bool {
	// BridgeHostChecker requires a nil receiver to trust no host.
	if checker == nil {
		return false
	}
	checker.checkedURLs = append(checker.checkedURLs, pdsURL)
	return checker.trustedURLs[pdsURL]
}

func TestFanoutCommentCreate_RecipientEligibility(t *testing.T) {
	const recipientDID = "did:plc:timegaterecipient"
	const recipientPDSURL = "https://bridge.test"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		setup        func(*commentFanoutLookups)
		bridgeHosts  BridgeHostChecker
		checkTrusted bool
	}{
		{"erased_recipient", func(lookups *commentFanoutLookups) {
			lookups.erasedAccounts = map[string]bool{recipientDID: true}
		}, nil, false},
		{"aggregator_recipient", func(lookups *commentFanoutLookups) {
			lookups.aggregatorAccounts = map[string]bool{recipientDID: true}
		}, nil, false},
		{"trusted_bridge_recipient", func(lookups *commentFanoutLookups) {
			lookups.userPDSURLs = map[string]string{recipientDID: recipientPDSURL}
		}, &notificationTestBridgeHosts{trustedURLs: map[string]bool{recipientPDSURL: true}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups := commentFanoutLookups{indexedUsers: map[string]bool{recipientDID: true}}
			test.setup(&lookups)
			comment := notificationTimeGateComment(createdAt)
			intents, err := FanoutCommentCreate(context.Background(), lookups, test.bridgeHosts, comment)
			require.NoError(t, err)
			require.Empty(t, intents, "an ineligible recipient must not receive a postReply")
			if test.checkTrusted {
				checker, ok := test.bridgeHosts.(*notificationTestBridgeHosts)
				require.True(t, ok, "the trusted-bridge case needs the recording checker")
				require.Equal(t, []string{recipientPDSURL}, checker.checkedURLs,
					"bridge trust must be checked against the recipient's users.pds_url")
			}
		})
	}
}

func TestFanoutCommentCreate_UntrustedOrNilBridgeCheckerAllowsRecipient(t *testing.T) {
	const recipientDID = "did:plc:timegaterecipient"
	const recipientPDSURL = "https://native.pds.test"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		bridgeHosts BridgeHostChecker
	}{
		{"untrusted_PDS", &notificationTestBridgeHosts{trustedURLs: map[string]bool{"https://bridge.test": true}}},
		{"nil_checker", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationTimeGateComment(createdAt)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{recipientDID: true},
				userPDSURLs:  map[string]string{recipientDID: recipientPDSURL},
			}
			intents, err := FanoutCommentCreate(context.Background(), lookups, test.bridgeHosts, comment)
			require.NoError(t, err)
			require.Equal(t, []Intent{{
				Reason: ReasonPostReply, RecipientDID: recipientDID, ActorDID: comment.AuthorDID,
				RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
				RootPostURI: comment.RootURI, RecordCreatedAt: createdAt,
			}}, intents, "an untrusted or unchecked PDS must not suppress the reply")
		})
	}
}

// Only the recipient's hosting suppresses. Replies from users on a trusted
// bridge PDS to users on a native PDS are the bridge's main traffic.
func TestFanoutCommentCreate_BridgeHostedActorStillNotifiesNativeRecipient(t *testing.T) {
	const actorDID = "did:plc:timegatecommenter"
	const recipientDID = "did:plc:timegaterecipient"
	const bridgePDSURL = "https://bridge.test"
	const nativePDSURL = "https://native.pds.test"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	comment := notificationTimeGateComment(createdAt)
	require.Equal(t, actorDID, comment.AuthorDID)
	lookups := commentFanoutLookups{
		indexedUsers: map[string]bool{actorDID: true, recipientDID: true},
		userPDSURLs:  map[string]string{actorDID: bridgePDSURL, recipientDID: nativePDSURL},
	}
	bridgeHosts := &notificationTestBridgeHosts{trustedURLs: map[string]bool{bridgePDSURL: true}}
	// The actor is indexed on a host the checker trusts, so a rule that also
	// checked the actor's hosting would suppress this reply.
	require.True(t, bridgeHosts.trustedURLs[lookups.userPDSURLs[actorDID]], "the actor must be on a trusted bridge PDS")

	intents, err := FanoutCommentCreate(context.Background(), lookups, bridgeHosts, comment)
	require.NoError(t, err)
	require.Equal(t, []Intent{{
		Reason: ReasonPostReply, RecipientDID: recipientDID, ActorDID: actorDID,
		RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
		RootPostURI: comment.RootURI, RecordCreatedAt: createdAt,
	}}, intents, "a bridge-hosted actor must not suppress a reply to a native-PDS recipient")
}
