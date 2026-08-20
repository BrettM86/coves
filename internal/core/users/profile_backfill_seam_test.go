package users

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNewProfileBackfillClient_TakesNoOptionsFromOutsideThisPackage makes the
// constructor's doc comment a fact about the type rather than a description of
// current habits.
//
// That comment says "Production passes nothing; it cannot open the guard." The
// first half was observation and the second half was false: the parameter was
// `opts ...covesoauth.Option`, an EXPORTED type, and covesoauth.
// WithPrivateAddressesAllowed() is an exported option that does precisely what
// the sentence promises cannot happen — from cmd/server, from a handler, from
// anywhere, with no coves:allow-ssrf-hatch marker for the audit to find.
//
// A confidently wrong comment in a security file is worse than no comment: it is
// the thing a reviewer checks INSTEAD of the code. So the seam moved to an
// unexported constructor, this package's tests call that, and the exported one
// now takes the gate and nothing else — which is what the sentence claims.
//
// Reflection, because the property is about a SIGNATURE and a signature is not
// otherwise observable from a test; a call that tried to pass an option simply
// would not compile, and a test that does not compile is not a test.
func TestNewProfileBackfillClient_TakesNoOptionsFromOutsideThisPackage(t *testing.T) {
	t.Parallel()

	fn := reflect.TypeOf(NewProfileBackfillClient)

	assert.Falsef(t, fn.IsVariadic(),
		"NewProfileBackfillClient is variadic (%s), so a caller outside this package can pass "+
			"covesoauth options — including WithPrivateAddressesAllowed(), which is exported. Its doc "+
			"comment says production 'cannot open the guard'; while this parameter exists that is a "+
			"convention rather than a type fact, and a security comment that overstates its enforcement "+
			"is what a reviewer trusts instead of reading the code. newProfileBackfillClient is where the "+
			"test seam belongs", fn)

	assert.Equalf(t, 1, fn.NumIn(),
		"NewProfileBackfillClient takes %d parameters. The dev gate is the only thing a caller outside "+
			"this package has to say about the client it gets", fn.NumIn())
}
