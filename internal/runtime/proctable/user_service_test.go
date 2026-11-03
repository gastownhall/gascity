package proctable

import "testing"

func TestUserManagerServiceUnit(t *testing.T) {
	const (
		detached = "/user.slice/user-1000.slice/user@1000.service/app.slice/detached-worker.service"
		scope    = "/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-af4f6362.scope"
		own      = "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service"
		manager  = "/user.slice/user-1000.slice/user@1000.service"
		system   = "/system.slice/foo.service"
		nested   = "/user.slice/user-1000.slice/user@1000.service/app.slice/outer.service/inner.service"
	)
	tests := []struct {
		name string
		path string
		unit string
		ok   bool
	}{
		{name: "detached systemd-run service", path: detached, unit: "detached-worker.service", ok: true},
		{name: "session tmux scope", path: scope},
		{name: "session's own service", path: own, unit: "agent.service", ok: true},
		{name: "user manager itself", path: manager},
		{name: "system service outside the user manager", path: system},
		{name: "innermost nested service", path: nested, unit: "inner.service", ok: true},
		{name: "init scope under the manager", path: "/user.slice/user-1000.slice/user@1000.service/init.scope"},
		{name: "instantiated unit that is not the user manager", path: "/user.slice/user-1000.slice/user@1000.service/app.slice/foo@bar.service", unit: "foo@bar.service", ok: true},
		{name: "empty"},
		{name: "root only", path: "/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UserManagerServiceUnit(tc.path)
			if ok != tc.ok || got != tc.unit {
				t.Fatalf("UserManagerServiceUnit(%q) = %q, %v; want %q, %v", tc.path, got, ok, tc.unit, tc.ok)
			}
		})
	}
}
