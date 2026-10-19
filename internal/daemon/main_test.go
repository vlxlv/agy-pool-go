package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

// TestMain keeps this package off the user's real state: RunForeground's startup quota sweep would
// otherwise load the live pool and refresh every account upstream with its real tokens.
// config resolves the data dir in init(), so it is set directly here; the env vars cover the helper
// processes these tests re-execute.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agy-daemon-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("AGY_DATA_DIR", dir)
	os.Setenv("AGY_TEST_MODE", "1")
	config.SetTestMode(true)
	if err := config.ConfigureDataDir(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
