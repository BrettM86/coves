//go:build integration

package routes_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
