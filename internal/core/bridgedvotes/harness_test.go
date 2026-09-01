//go:build integration

package bridgedvotes_test

import (
	"os"
	"testing"

	"Coves/tests/testkit"
)

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}
