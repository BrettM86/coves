package main

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// mainSource is main.go as compiled into this test binary (embedded, so a
// build overlay of main.go is what the test sees).
//
//go:embed main.go
var mainSource []byte

// TestRunStartsTheAccountStatusRecheckJob guards the wiring: run() must start
// the account status recheck job with a rechecker, or failed #account
// confirmations are never retried and nothing reports it.
func TestRunStartsTheAccountStatusRecheckJob(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "main.go", mainSource, 0)
	require.NoError(t, err)

	var run *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.Name == "run" {
			run = function
		}
	}
	require.NotNil(t, run, "main.go has no func run()")

	var started []string
	ast.Inspect(run.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "startAccountStatusRecheckJob" {
			return true
		}
		position := fileSet.Position(call.Pos()).String()
		require.GreaterOrEqual(t, len(call.Args), 3, "startAccountStatusRecheckJob call at %s has no rechecker argument", position)
		if rechecker, isIdent := call.Args[2].(*ast.Ident); isIdent && rechecker.Name == "nil" {
			t.Errorf("startAccountStatusRecheckJob at %s is passed a nil rechecker", position)
		}
		started = append(started, position)
		return true
	})
	require.NotEmpty(t, started, "run() never calls startAccountStatusRecheckJob")
}
