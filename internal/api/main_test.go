package api

import (
	"os"
	"testing"

	"github.com/coderouter/coderouter/internal/db"
)

// TestMain lowers the password work factor, for the same reason the db suite
// does: the sign-in tests create accounts and the production cost makes them
// slow enough that people stop running them.
func TestMain(m *testing.M) {
	db.SetBcryptCostForTests(4)
	os.Exit(m.Run())
}
