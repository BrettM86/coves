package main

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// wiringSource is wiring.go as compiled into this test binary (embedded, so a
// build overlay of wiring.go is what the test sees).
//
//go:embed wiring.go
var wiringSource []byte

// TestBuildServicesGivesTheUserServiceTheRoleRecordDeleteQueueAsItsErasureFollowUp
// guards the wiring: the user service must run the role record delete queue
// after an erasure commits, or an erased account's role records wait for the
// periodic sweep.
func TestBuildServicesGivesTheUserServiceTheRoleRecordDeleteQueueAsItsErasureFollowUp(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "wiring.go", wiringSource, 0)
	require.NoError(t, err)

	var buildServices *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil && function.Name.Name == "buildServices" {
			buildServices = function
		}
	}
	require.NotNil(t, buildServices, "wiring.go has no buildServices method")

	var newUserServiceCalls int
	followUpIsQueue := false
	ast.Inspect(buildServices.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isPackageCall(call, "users", "NewUserService") {
			return true
		}
		newUserServiceCalls++
		for _, argument := range call.Args {
			option, ok := argument.(*ast.CallExpr)
			if !ok || !isPackageCall(option, "users", "WithErasureFollowUp") || len(option.Args) != 1 {
				continue
			}
			if field, ok := option.Args[0].(*ast.SelectorExpr); ok && field.Sel.Name == "roleRecordDeleteQueue" {
				followUpIsQueue = true
			}
		}
		return true
	})
	require.Equal(t, 1, newUserServiceCalls, "buildServices builds the user service once")
	require.True(t, followUpIsQueue, "users.NewUserService is not passed users.WithErasureFollowUp(a.roleRecordDeleteQueue)")
}

// isPackageCall reports whether call is pkg.name(...).
func isPackageCall(call *ast.CallExpr, pkg, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == pkg
}
