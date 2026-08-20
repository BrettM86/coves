package posts

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultRematerializeBlobClient_TakesNoOptionsFromOutsideThisPackage is the
// same fence as users' TestNewProfileBackfillClient_TakesNoOptionsFromOutside
// ThisPackage, over the constructor that makes the same claim.
//
// The doc comment said "Production passes nothing; it cannot open the guard,
// which only allowPrivateHosts does." The parameter was `opts ...covesoauth.
// Option` and covesoauth.WithPrivateAddressesAllowed() is exported, so the
// second clause was false — any caller in the tree could open the guard on the
// batch tool that copies blobs from whatever host a federated community's PDSURL
// or an author's DID document names.
//
// The seam it existed for did not have to be exported to work:
// newGuardedRematerializeBlobClient is in this package, and this package is
// where the tests that need it live.
func TestDefaultRematerializeBlobClient_TakesNoOptionsFromOutsideThisPackage(t *testing.T) {
	t.Parallel()

	fn := reflect.TypeOf(DefaultRematerializeBlobClient)

	assert.Falsef(t, fn.IsVariadic(),
		"DefaultRematerializeBlobClient is variadic (%s), so a caller outside this package can pass "+
			"covesoauth options — including the exported WithPrivateAddressesAllowed(). Its doc comment "+
			"says the guard 'only allowPrivateHosts' opens; while this parameter exists that is a "+
			"convention, not a type fact, and there is no coves:allow-ssrf-hatch marker on such a call "+
			"for the audit to find. newGuardedRematerializeBlobClient is where the test seam belongs", fn)

	assert.Equalf(t, 1, fn.NumIn(),
		"DefaultRematerializeBlobClient takes %d parameters. The dev gate is the only thing a caller "+
			"outside this package has to say about the client it gets", fn.NumIn())
}
