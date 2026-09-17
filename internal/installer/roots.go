package installer

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

// InstallerRoots defines explicit directories for binary, config, data, and system installations.
type InstallerRoots struct {
	UserHome   string
	BinDir     string
	ConfigDir  string
	DataDir    string
	LibexecDir string
	SystemRoot string
}

var (
	installerRootsMu   sync.RWMutex
	testInstallerRoots *InstallerRoots
)

// SetTestInstallerRoots sets the hermetic roots used by installer and uninstaller in tests.
func SetTestInstallerRoots(roots *InstallerRoots) func() {
	installerRootsMu.Lock()
	orig := testInstallerRoots
	testInstallerRoots = roots
	installerRootsMu.Unlock()

	return func() {
		installerRootsMu.Lock()
		testInstallerRoots = orig
		installerRootsMu.Unlock()
	}
}

// GetInstallerRoots returns the active installer roots.
// In test mode, all paths are strictly hermetic and derive from synthetic sandbox roots or temp directory.
// In production mode, paths derive from the real user home and system defaults.
func GetInstallerRoots() InstallerRoots {
	installerRootsMu.RLock()
	custom := testInstallerRoots
	installerRootsMu.RUnlock()
	if custom != nil {
		return *custom
	}

	if config.IsTestMode() {
		sandbox := config.GetSyntheticSandboxRoot()
		if sandbox == "" {
			sandbox = filepath.Join(os.TempDir(), "agy_test_sandbox")
		}
		testHome := filepath.Join(sandbox, "test_home")
		return InstallerRoots{
			UserHome:   testHome,
			BinDir:     filepath.Join(testHome, ".local", "bin"),
			ConfigDir:  filepath.Join(testHome, ".config", "agy-pool"),
			DataDir:    filepath.Join(testHome, ".local", "share", "agy-pool"),
			LibexecDir: filepath.Join(testHome, ".local", "libexec", "agy-pool"),
			SystemRoot: filepath.Join(sandbox, "test_system"),
		}
	}

	home := os.Getenv("HOME")
	if home == "" {
		home = config.GetRealHostUserHome()
	}
	return InstallerRoots{
		UserHome:   home,
		BinDir:     filepath.Join(home, ".local", "bin"),
		ConfigDir:  filepath.Join(home, ".config", "agy-pool"),
		DataDir:    filepath.Join(home, ".local", "share", "agy-pool"),
		LibexecDir: filepath.Join(home, ".local", "libexec", "agy-pool"),
		SystemRoot: "",
	}
}
