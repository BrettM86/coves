package aggregator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoExportedSeamCanReplaceTheGuardedClient closes the last way a production
// caller in another package can end up with an unguarded registration handler.
//
// # THE CODEBASE WAS MAKING TWO OPPOSITE CALLS ABOUT ONE HAZARD
//
// pds/factory.go's transport seam is UNEXPORTED for exactly this reason, and
// says so: "the resolver seam these tests need must not be reachable from any
// non-test package". SetHTTPClient was the same hazard with the opposite answer
// — an exported method that discards the client NewRegisterHandler built and
// installs whatever it is handed, guard and all. Its own doc comment conceded
// the footgun and deferred the fix. This is the fix.
//
// # UNEXPORTING IS THE WHOLE MECHANISM, AND IT IS ENOUGH
//
// Every caller of the seam is a fixture in THIS package: three install a pinned
// dialer so a made-up domain reaches an httptest listener, which is why the seam
// has to keep REPLACING rather than wrapping — a wrapped guard would refuse that
// listener's loopback address or fail to resolve the name at all under an
// egress-blocked CI. Lowercasing it costs those fixtures nothing and puts the
// hazard out of reach of every package that is not this one, which is where
// production lives.
//
// # WHY THIS IS ASSERTED OVER THE SOURCE
//
// Reflection sees exported methods and not package-level functions, and neither
// is visible to a test that only calls things. Parsing the package's own
// declarations states the property directly — "nothing exported from here takes
// a client" — and it goes on holding for the next seam somebody adds, which is
// the failure mode a test naming one identifier cannot cover.
func TestNoExportedSeamCanReplaceTheGuardedClient(t *testing.T) {
	t.Parallel()

	offenders := exportedHTTPClientParameters(t, ".")

	assert.Emptyf(t, offenders,
		"these exported declarations take an *http.Client, so any package can hand this one an "+
			"unguarded client and discard the one NewRegisterHandler built: %s. Lowercase them — every "+
			"caller is a fixture in this package, and pds/factory.go's withTransportOptions is the "+
			"shape to copy", strings.Join(offenders, ", "))
}

// exportedHTTPClientParameters returns the exported declarations in dir's
// non-test Go files that accept an *http.Client.
func exportedHTTPClientParameters(t *testing.T, dir string) []string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	require.NoError(t, err, "parsing the package's own sources")
	require.NotEmpty(t, pkgs, "the parser found no package, so this test would pass vacuously")

	var offenders []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() {
					continue
				}
				for _, param := range fn.Type.Params.List {
					star, ok := param.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					sel, ok := star.X.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok || ident.Name != "http" || sel.Sel.Name != "Client" {
						continue
					}
					offenders = append(offenders, fn.Name.Name)
				}
			}
		}
	}
	return offenders
}
