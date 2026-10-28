package daemon

import "testing"

func TestCgroupScope(t *testing.T) {
	for _, tc := range []struct{ cgroup, want string }{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/agy-pool.service\n", "--user"},
		{"0::/system.slice/agy-pool.service\n", "--system"},
		{"12:pids:/system.slice/agy-pool.service\n1:name=systemd:/system.slice/agy-pool.service\n", "--system"},
		{"0::/user.slice/user-1000.slice/session-3.scope\n", ""},
		{"0::/system.slice/other-agy-pool.service.d\n", ""},
	} {
		if got := cgroupScope(tc.cgroup); got != tc.want {
			t.Errorf("cgroupScope(%q) = %q, want %q", tc.cgroup, got, tc.want)
		}
	}
}
