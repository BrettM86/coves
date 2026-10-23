package communities_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The T0 complement to service_update_test.go (integration): the update
// fields that must be refused before anything is written to the community's
// repo. An update the database would later reject by CHECK constraint
// (visibility, moderation_type) or that create already refuses (an over-long
// description) must not reach putRecord: a record the consumer cannot index
// leaves the PDS and the AppView disagreeing about the community for good.
//
// The PDS here is an httptest server on loopback that counts putRecord calls
// and answers like a PDS. The community's access token is a JWT that expires
// in an hour, so EnsureFreshToken never contacts the PDS and the only request
// the server can see is the record write itself.

const updateValidationCreatorDID = "did:plc:updatevalidationcreator"

// countingProfilePDS answers putRecord with a fixed uri/cid and counts how many
// times it was asked.
func countingProfilePDS(t *testing.T) (url string, putRecords *atomic.Int64) {
	t.Helper()
	putRecords = &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/com.atproto.repo.putRecord" {
			http.Error(w, "unexpected endpoint "+r.URL.Path, http.StatusNotFound)
			return
		}
		putRecords.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uri": "at://" + seededCommunityDID + "/social.coves.community.profile/self",
			"cid": "bafyupdatedprofile",
		})
	}))
	t.Cleanup(server.Close)
	return server.URL, putRecords
}

// unexpiredAccessJWT is an unsigned JWT whose only claim that matters is an
// expiry an hour away, which is all NeedsRefresh reads.
func unexpiredAccessJWT(t *testing.T) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none", "typ": "JWT"}) + "." +
		encode(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}) + ".signature"
}

func TestUpdateCommunity_ValidatesFieldsBeforeWritingToThePDS(t *testing.T) {
	t.Parallel()

	stringPointer := func(s string) *string { return &s }

	tests := []struct {
		name           string
		visibility     *string
		description    *string
		moderationType *string
		// seededModerationType is the stored value before the update; empty means "moderator".
		seededModerationType string
		wantValid            bool
	}{
		{
			name:       "visibility the lexicon does not define",
			visibility: stringPointer("secret"),
		},
		{
			name:        "description one grapheme over the 1000-grapheme limit",
			description: stringPointer(strings.Repeat("a", 1001)),
		},
		{
			// 401 family emoji: 401 graphemes but 10025 bytes.
			name:        "description under 1000 graphemes but over the 10000-byte limit",
			description: stringPointer(strings.Repeat("\U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466", 401)),
		},
		{
			name:           "moderation type the lexicon does not define",
			moderationType: stringPointer("anarchy"),
		},
		{
			// 1000 four-byte graphemes: exactly the grapheme limit and 4000
			// bytes, over the old 3000-byte cap.
			name:           "valid visibility, 1000-grapheme 4000-byte description and moderation type",
			visibility:     stringPointer("unlisted"),
			description:    stringPointer(strings.Repeat("\U0001F600", 1000)),
			moderationType: stringPointer("sortition"),
			wantValid:      true,
		},
		{
			name:                 "switching a sortition community back to moderator",
			moderationType:       stringPointer("moderator"),
			seededModerationType: "sortition",
			wantValid:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pdsURL, putRecords := countingProfilePDS(t)
			seededModerationType := tt.seededModerationType
			if seededModerationType == "" {
				seededModerationType = "moderator"
			}
			repo := newFakeCommunityRepo()
			repo.seed(&communities.Community{
				DID:             seededCommunityDID,
				Handle:          seededCommunityHandle,
				Name:            "gardening",
				DisplayName:     "Gardening",
				Visibility:      "public",
				ModerationType:  seededModerationType,
				OwnerDID:        seededCommunityDID,
				CreatedByDID:    updateValidationCreatorDID,
				HostedByDID:     fakeInstanceDID,
				PDSURL:          pdsURL,
				PDSAccessToken:  unexpiredAccessJWT(t),
				PDSRefreshToken: "refresh-token",
				CreatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			})
			service := communities.NewCommunityServiceWithPDSFactory(
				repo, pdsURL, fakeInstanceDID, fakeInstanceDomain, nil, nil, nil,
				communities.PrivateHostOptions(true)...)

			updated, err := service.UpdateCommunity(context.Background(), communities.UpdateCommunityRequest{
				CommunityDID:   seededCommunityDID,
				UpdatedByDID:   updateValidationCreatorDID,
				Visibility:     tt.visibility,
				Description:    tt.description,
				ModerationType: tt.moderationType,
			})

			if tt.wantValid {
				require.NoError(t, err)
				require.NotNil(t, updated)
				assert.Equal(t, int64(1), putRecords.Load(),
					"a valid update must be written to the community's repo exactly once")
				return
			}

			require.Error(t, err, "the update was accepted and written to the PDS")
			assert.Nil(t, updated)
			assert.True(t, communities.IsValidationError(err),
				"an invalid field must be refused with a validation error the handler maps to 400, got %v", err)
			assert.Equal(t, int64(0), putRecords.Load(),
				"an invalid update must be refused before putRecord: the record would reach the PDS and "+
					"then be rejected by the AppView database")
		})
	}
}
