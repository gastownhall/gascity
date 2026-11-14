package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestDoSupervisorRunWarnsAsInit pins the startup warning for a
// supervisor that is PID 1 (#4308): gc does not reap orphaned children, so
// a container that runs `gc supervisor run` as its entrypoint with no init
// fills with zombies until gc cannot fork. The warning names the cause and
// an init to run gc under; any other pid prints nothing.
func TestDoSupervisorRunWarnsAsInit(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		want bool
	}{
		{name: "pid 1", pid: 1, want: true},
		{name: "child of an init", pid: 4242, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BEADS_ACTOR", "test")

			origPid, origRun := supervisorGetpid, runSupervisorFunc
			t.Cleanup(func() { supervisorGetpid, runSupervisorFunc = origPid, origRun })
			supervisorGetpid = func() int { return tc.pid }
			ran := false
			runSupervisorFunc = func(io.Writer, io.Writer) int {
				ran = true
				return 0
			}

			var stderr bytes.Buffer
			if rc := doSupervisorRun(io.Discard, &stderr); rc != 0 {
				t.Fatalf("doSupervisorRun = %d, want 0; stderr: %s", rc, stderr.String())
			}
			if !ran {
				t.Fatal("runSupervisorFunc was not invoked: the warning must not stop the supervisor")
			}
			got := stderr.String()
			if tc.want {
				for _, part := range []string{"running as PID 1", "zombies", "tini"} {
					if !strings.Contains(got, part) {
						t.Errorf("stderr = %q, want it to contain %q", got, part)
					}
				}
			} else if got != "" {
				t.Errorf("stderr = %q, want nothing when gc is not PID 1", got)
			}
		})
	}
}
