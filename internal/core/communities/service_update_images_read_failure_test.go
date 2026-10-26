package communities_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

func TestService_UpdateProfileReadFailureStopsBeforeImageSideEffects(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", http.StatusServiceUnavailable, `{"error":"ProfileReadUnavailable","message":"community profile read fixture unavailable"}`},
		{"missing value", http.StatusOK, `{}`},
		{"null response", http.StatusOK, `null`},
		{"null value", http.StatusOK, `{"value":null}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo, blobService := newFakeCommunityRepo(), &recordingMediaBlobs{}
			refreshedToken := mediaPipelineJWT(t)
			claims, err := json.Marshal(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
			require.NoError(t, err)
			expiredToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			var mutex sync.Mutex
			var reads, writes int
			var correctRead bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutex.Lock()
				defer mutex.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/xrpc/com.atproto.server.refreshSession":
					if r.Header.Get("Authorization") != "Bearer community-refresh-token" {
						t.Error("token refresh must authenticate with the community refresh credential")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"did": mediaCommunityDID, "handle": "c-gardening." + fakeInstanceDomain,
						"accessJwt": refreshedToken, "refreshJwt": "renewed-community-refresh-token",
					})
				case "/xrpc/com.atproto.repo.getRecord":
					reads++
					correctRead = r.Method == http.MethodGet && r.Header.Get("Authorization") == "Bearer "+refreshedToken &&
						r.URL.Query().Get("repo") == mediaCommunityDID && r.URL.Query().Get("collection") == "social.coves.community.profile" && r.URL.Query().Get("rkey") == "self"
					w.WriteHeader(testCase.status)
					_, _ = w.Write([]byte(testCase.body))
				case "/xrpc/com.atproto.repo.putRecord":
					writes++
					_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://" + mediaCommunityDID + "/social.coves.community.profile/self", "cid": "bafytestrecord"})
				default:
					t.Errorf("unexpected PDS endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			community := repo.seed(&communities.Community{
				DID: mediaCommunityDID, Name: "gardening", Handle: "c-gardening." + fakeInstanceDomain,
				OwnerDID: mediaCommunityDID, CreatedByDID: aValidCreateRequest().CreatedByDID, HostedByDID: fakeInstanceDID,
				PDSAccessToken: expiredToken, PDSRefreshToken: "community-refresh-token", PDSURL: server.URL,
				CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			})
			pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 2, MaxLongEdge: media.DefaultMaxLongEdge})
			require.NoError(t, err)
			service := communities.NewCommunityService(repo, server.URL, fakeInstanceDID, fakeInstanceDomain,
				communities.NewPDSAccountProvisioner(fakeInstanceDomain, server.URL, communities.PrivateHostOptions(true)...),
				nil, blobService, pipeline, communities.PrivateHostOptions(true)...)
			description := "Read failure must stop this edit"
			_, err = service.UpdateCommunity(context.Background(), communities.UpdateCommunityRequest{
				CommunityDID: community.DID, UpdatedByDID: community.CreatedByDID, Description: &description,
				AvatarBlob: testkit.TestPNG(16, 16), AvatarMimeType: "image/png",
			})
			assert.Error(t, err, "a failed current-profile read must refuse the update")
			if err != nil {
				assert.ErrorContains(t, err, "failed to update community on PDS", "keep the existing PDS-update error mapping")
				if testCase.status == http.StatusServiceUnavailable {
					assert.ErrorContains(t, err, "ProfileReadUnavailable", "identify the PDS profile-read failure rather than an unrelated error")
				}
			}
			mutex.Lock()
			defer mutex.Unlock()
			assert.Equal(t, 1, reads, "read the community's current profile before uploading")
			assert.True(t, correctRead, "GET the canonical profile with the refreshed community access credential")
			assert.Zero(t, len(blobService.uploads), "a failed profile read must not upload the valid requested avatar")
			assert.Zero(t, writes, "a failed profile read must not putRecord")
		})
	}
}
