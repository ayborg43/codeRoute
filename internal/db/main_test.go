package db

import (
	"os"
	"testing"
)

// TestMain lowers the password work factor for the suite. Correctness does not
// depend on the cost, and at the production setting these tests spend minutes
// on key stretching alone.
func TestMain(m *testing.M) {
	SetBcryptCostForTests(4)
	os.Exit(m.Run())
}
