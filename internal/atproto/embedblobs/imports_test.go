package embedblobs

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLeafImportsOnlyStdlibAndGoCID(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		files++
		source, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, spec := range source.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err)
			require.False(t, path == "Coves" || strings.HasPrefix(path, "Coves/"), "%s imports %s", entry.Name(), path)
			if path == "github.com/ipfs/go-cid" {
				continue
			}
			pkg, err := build.Default.Import(path, ".", build.FindOnly)
			require.NoError(t, err, "%s imports %s", entry.Name(), path)
			require.True(t, pkg.Goroot, "%s imports non-stdlib package %s", entry.Name(), path)
		}
	}
	require.Positive(t, files, "no non-test Go source files found")
}
