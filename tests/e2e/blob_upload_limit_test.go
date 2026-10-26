//go:build e2e

package e2e

import (
	"bytes"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// Blob uploads are direct PDS calls and touch no Jetstream collection.
func TestHermeticPDSesAcceptSixMiBBlobUploads(t *testing.T) {
	for _, test := range []struct {
		name   string
		newPDS func(testkit.TestingT, ...testkit.PDSOption) *testkit.PDS
	}{
		{"primary", testkit.NewPDS},
		{"federated", testkit.NewFederatedPDS},
	} {
		t.Run(test.name, func(t *testing.T) {
			pds := test.newPDS(t)
			account := pds.CreateAccount(t, testkit.WithHandle(pds.Endpoint.Handle(testkit.UniqueID(t))))
			blob := account.UploadBlob(t, bytes.Repeat([]byte{'a'}, 6291456), "application/octet-stream")
			require.Equal(t, int64(6291456), blob.Size)
		})
	}
}
