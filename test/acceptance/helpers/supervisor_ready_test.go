package acceptancehelpers

import "testing"

// Only the control socket answering with the started supervisor's PID counts
// as ready. `gc supervisor status` also reports "running" from its
// service-manager and API fallbacks while that supervisor holds its lock but
// has not bound the socket yet, and `gc init`/`gc start` probe only the
// socket (ga-96smfk.84).
func TestSupervisorStatusConfirmsPID(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		pid  int
		want bool
	}{
		{name: "socket answered with our pid", out: "Supervisor is running (PID 4242)\n", pid: 4242, want: true},
		{name: "our pid plus an ownership note", out: "Supervisor is running (PID 4242)\nWarning: running outside systemd unit x\n", pid: 4242, want: true},
		{name: "another pid", out: "Supervisor is running (PID 42420)\n", pid: 4242, want: false},
		{name: "service manager fallback", out: "Supervisor is running (pid unavailable: control socket unreachable; liveness confirmed via service_manager)\n", pid: 4242, want: false},
		{name: "api fallback", out: "Supervisor is running (pid unavailable: control socket unreachable; liveness confirmed via api)\n", pid: 4242, want: false},
		{name: "not running", out: "Supervisor is not running\n", pid: 4242, want: false},
		{name: "no pid to confirm", out: "Supervisor is running (PID 4242)\n", pid: 0, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupervisorStatusConfirmsPID(tc.out, tc.pid); got != tc.want {
				t.Fatalf("SupervisorStatusConfirmsPID(%q, %d) = %v, want %v", tc.out, tc.pid, got, tc.want)
			}
		})
	}
}
