package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// systemdUnitName is the unit name of the packaged and contrib/systemd service files.
const systemdUnitName = "agy-pool.service"

// systemdScope reports how systemd supervises pid: "--user", "--system", or "" when it does not.
// A supervised daemon must be stopped and restarted through systemctl: a plain SIGTERM is undone by
// Restart=, and a CLI-spawned replacement fights the unit for the PID lock.
func systemdScope(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	return cgroupScope(string(b))
}

func cgroupScope(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "/"+systemdUnitName) {
			if strings.Contains(line, "/user@") {
				return "--user"
			}
			return "--system"
		}
	}
	return ""
}

// systemctl runs a systemctl verb on the agy-pool unit in scope.
func systemctl(scope, verb string) error {
	out, err := exec.Command("systemctl", scope, verb, systemdUnitName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s %s: %w: %s", scope, verb, systemdUnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
