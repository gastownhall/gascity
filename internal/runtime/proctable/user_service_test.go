package proctable

import "testing"

// TestUserManagerServiceUnit covers the pure cgroup-record classifier the
// detached user-service fence is built on: a recognized user service is
// named with its user manager, and everything else — malformed data, a
// system service, a scope, a hidden manager — proves nothing, so the kill
// paths keep their prior behavior.
func TestUserManagerServiceUnit(t *testing.T) {
	const manager = "/user.slice/user-1000.slice/user@1000.service"
	for _, tc := range []struct {
		name       string
		cgroupFile string
		want       string
	}{
		{name: "unified record", cgroupFile: "0::" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},
		{name: "unified record without trailing newline", cgroupFile: "0::" + manager + "/app.slice/devserver.service", want: "devserver.service"},
		{name: "v1 name=systemd record", cgroupFile: "1:name=systemd:" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},
		{name: "v1 record among several controllers", cgroupFile: "2:cpuset,name=systemd:" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},
		{name: "hybrid prefers the unified record", cgroupFile: "10:rdma:/user.slice\n1:name=systemd:" + manager + "/app.slice/v1.service\n0::" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},
		{name: "hybrid falls back to the v1 record", cgroupFile: "10:rdma:/user.slice\n1:name=systemd:" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},
		{name: "nested slices are skipped", cgroupFile: "0::" + manager + "/app.slice/session-3.slice/devserver.service\n", want: "devserver.service"},
		{name: "service directly under the manager", cgroupFile: "0::" + manager + "/devserver.service\n", want: "devserver.service"},
		{name: "template unit", cgroupFile: "0::" + manager + "/app.slice/port-8080@dev.service\n", want: "port-8080@dev.service"},
		{name: "delegated descendants belong to the service", cgroupFile: "0::" + manager + "/app.slice/devserver.service/gc/leaf\n", want: "devserver.service"},
		{name: "duplicated identical unified records", cgroupFile: "0::" + manager + "/app.slice/devserver.service\n0::" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},
		{name: "duplicated identical v1 records", cgroupFile: "1:name=systemd:" + manager + "/app.slice/devserver.service\n1:name=systemd:" + manager + "/app.slice/devserver.service\n", want: "devserver.service"},

		{name: "empty file", cgroupFile: "", want: ""},
		{name: "blank lines only", cgroupFile: "\n\n", want: ""},
		{name: "malformed line", cgroupFile: "junk\n", want: ""},
		{name: "two-field line", cgroupFile: "0:" + manager + "\n", want: ""},
		{name: "empty path", cgroupFile: "0::\n", want: ""},
		{name: "root path", cgroupFile: "0::/\n", want: ""},
		{name: "manager alone", cgroupFile: "0::" + manager + "\n", want: ""},
		{name: "manager with slices only below it", cgroupFile: "0::" + manager + "/app.slice\n", want: ""},
		{name: "unnumbered manager template", cgroupFile: "0::/user.slice/user-.slice/user@.service/app.slice/devserver.service\n", want: ""},
		{name: "nonnumeric manager uid", cgroupFile: "0::/user.slice/user-.slice/user@10x.service/app.slice/devserver.service\n", want: ""},
		{name: "system service", cgroupFile: "0::/system.slice/devserver.service\n", want: ""},
		{name: "scope under the manager", cgroupFile: "0::" + manager + "/app.slice/tmux-spawn-1.scope\n", want: ""},
		{name: "service-looking descendant of a scope", cgroupFile: "0::" + manager + "/app.slice/run-1.scope/devserver.service\n", want: ""},
		{name: "unrelated controllers only", cgroupFile: "10:memory:/user.slice\n", want: ""},
		{name: "conflicting v1 records", cgroupFile: "1:name=systemd:/user.slice/devserver.service\n1:name=systemd:/user.slice/other.service\n", want: ""},
		{name: "conflicting unified records", cgroupFile: "0::/user.slice/devserver.service\n0::/user.slice/other.service\n", want: ""},
		{name: "cgroup namespace hides the manager", cgroupFile: "0::/app.slice/devserver.service\n", want: ""},
		{name: "non-absolute path", cgroupFile: "0::user.slice/user@1000.service/app.slice/devserver.service\n", want: ""},
		{name: "malformed line beside a valid record", cgroupFile: "junk\n0::" + manager + "/app.slice/devserver.service\n", want: ""},
		{name: "hybrid unified scope wins over a v1 service", cgroupFile: "1:name=systemd:" + manager + "/app.slice/devserver.service\n0::" + manager + "/app.slice/tmux-spawn-1.scope\n", want: ""},
		{name: "bare service suffix", cgroupFile: "0::" + manager + "/app.slice/.service\n", want: ""},
		{name: "prefixed manager name", cgroupFile: "0::/user.slice/user-1000.slice/xuser@1000.service/app.slice/devserver.service\n", want: ""},
		{name: "manager init scope", cgroupFile: "0::" + manager + "/init.scope\n", want: ""},
		{name: "login session scope", cgroupFile: "0::/user.slice/user-1000.slice/session-3.scope\n", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantManager := ""
			if tc.want != "" {
				wantManager = "user@1000.service"
			}
			gotManager, gotUnit := userManagerServiceUnit(tc.cgroupFile)
			if gotManager != wantManager || gotUnit != tc.want {
				t.Fatalf("userManagerServiceUnit(%q) = (%q, %q), want (%q, %q)", tc.cgroupFile, gotManager, gotUnit, wantManager, tc.want)
			}
		})
	}
}
