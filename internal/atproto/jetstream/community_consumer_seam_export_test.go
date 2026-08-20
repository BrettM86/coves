package jetstream

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

// TestNoExportedSeamCanReplaceTheGuardedClient is the jetstream half of the
// aggregator test of the same name, and the two exist together because the
// codebase was making two opposite calls about one hazard.
//
// pds/factory.go's withTransportOptions is UNEXPORTED and says why: "the
// resolver seam these tests need must not be reachable from any non-test
// package". WithWellKnownHTTPClient was the same seam left exported, and its own
// doc comment carried the whole defence in a sentence addressed to nobody the
// compiler can reach — "a test that uses this MUST still inject a GUARDED
// client". A rule that only a careful reader enforces is not a rule; the
// .well-known fetch it protects takes a domain off a community record published
// by anyone federated with this instance.
//
// Lowercasing costs the fixtures nothing — every caller is in this package — and
// keeps the seam's real property intact: it chooses WHAT gets classified, never
// whether classification happens.
func TestNoExportedSeamCanReplaceTheGuardedClient(t *testing.T) {
	t.Parallel()

	offenders := exportedHTTPClientParameters(t, ".")

	assert.Emptyf(t, offenders,
		"these exported declarations take an *http.Client, so any package can install an unguarded "+
			"client on this consumer and discard the one newWellKnownClient built: %s. Lowercase them "+
			"— every caller is a fixture in this package, and pds/factory.go's withTransportOptions is "+
			"the shape to copy", strings.Join(offenders, ", "))
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
