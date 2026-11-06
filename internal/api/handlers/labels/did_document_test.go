package labels_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	labelhandlers "Coves/internal/api/handlers/labels"
	"Coves/internal/core/labels"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigoidentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed K-256 keys. The private multibase is a throwaway generated for this
// test; the public multibase beside it is that key's public half.
const (
	labelSigningKeyPrivateMultibase = "z3vLhUAjHvPNd6Z3rb5QQ2Dnd51QfVsvSxnTwYfpx7RdhCf8"
	labelSigningKeyPublicMultibase  = "zQ3shgJ7Pd7JWkPhsfnk4nNADhHZPr9QjXsWZNssxPApcfsxf"
	instanceAtprotoPublicMultibase  = "zQ3shZXbRaEdLTpPrF8kXoRaeU3uaZyUQWdZFBxeSFe656pj2"
)

func TestDIDDocumentHandlerPublishesLabelerIdentity(t *testing.T) {
	signingKey, err := atcrypto.ParsePrivateMultibase(labelSigningKeyPrivateMultibase)
	require.NoError(t, err)
	publisher, err := labels.NewPublisher(labels.Config{
		SourceDID:       "did:web:coves.social",
		LabelerEndpoint: "https://coves.social",
		AtprotoKey:      instanceAtprotoPublicMultibase,
		Key:             signingKey,
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	labelhandlers.NewDIDDocumentHandler(publisher).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/.well-known/did.json", nil))

	require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")

	var document indigoidentity.DIDDocument
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &document))
	identity := indigoidentity.ParseIdentity(&document)

	assert.Equal(t, "did:web:coves.social", identity.DID.String())

	labelKey, err := identity.GetPublicKey("atproto_label")
	require.NoError(t, err, "DID document declares no #atproto_label key")
	assert.Equal(t, labelSigningKeyPublicMultibase, labelKey.Multibase())

	atprotoKey, err := identity.GetPublicKey("atproto")
	require.NoError(t, err, "DID document declares no #atproto key")
	assert.Equal(t, instanceAtprotoPublicMultibase, atprotoKey.Multibase())
	assert.NotEqual(t, labelKey.Multibase(), atprotoKey.Multibase())

	assert.Equal(t, "https://coves.social", identity.GetServiceEndpoint("atproto_labeler"))
	assert.Equal(t, "AtprotoLabeler", identity.Services["atproto_labeler"].Type)
}
