package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanoutCommentCreate_BlocksEitherDirection(t *testing.T) {
	const recipientDID = "did:plc:timegaterecipient"
	const actorDID = "did:plc:timegatecommenter"
	const thirdPartyDID = "did:plc:timegatethirdparty"
	createdAt := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		block      commentFanoutBlock
		wantIntent bool
	}{
		{"recipient_blocks_actor", commentFanoutBlock{recipientDID, actorDID}, false},
		{"actor_blocks_recipient", commentFanoutBlock{actorDID, recipientDID}, false},
		{"actor_blocks_third_party", commentFanoutBlock{actorDID, thirdPartyDID}, true},
		{"third_party_blocks_recipient", commentFanoutBlock{thirdPartyDID, recipientDID}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			comment := notificationTimeGateComment(createdAt)
			lookups := commentFanoutLookups{
				indexedUsers: map[string]bool{recipientDID: true},
				blocks:       map[commentFanoutBlock]bool{test.block: true},
			}
			intents, err := FanoutCommentCreate(context.Background(), lookups, nil, comment)
			require.NoError(t, err)
			if !test.wantIntent {
				require.Empty(t, intents, "a block between actor and recipient must suppress their postReply")
				return
			}
			require.Equal(t, []Intent{{
				Reason: ReasonPostReply, RecipientDID: recipientDID, ActorDID: actorDID,
				RecordURI: comment.URI, RecordCID: comment.CID, SubjectURI: comment.RootURI,
				RootPostURI: comment.RootURI, RecordCreatedAt: createdAt,
			}}, intents, "a block involving a third party must not suppress the postReply")
		})
	}
}
