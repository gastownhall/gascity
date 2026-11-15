//go:build linux || darwin

package relay

import (
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// dieExecStatus relays the die-exec helper and returns how it ended.
func dieExecStatus(t *testing.T, sig syscall.Signal, args ...string) (Status, []byte) {
	t.Helper()
	h := startRelay(t, harnessOptions{}, "die-exec", append([]string{strconv.Itoa(int(sig))}, args...)...)
	return h.status(), h.output()
}

// The exec path ends the process by signals the Go runtime would otherwise
// survive: an unsubscribed runtime ignores SIGUSR1 and SIGUSR2 and turns
// SIGQUIT into a traceback (and bash, macOS's /bin/sh, ignores SIGQUIT).
func TestDieByExecDiesBySignal(t *testing.T) {
	for _, sig := range []syscall.Signal{unix.SIGUSR1, unix.SIGUSR2, unix.SIGTERM, unix.SIGQUIT} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			if st, out := dieExecStatus(t, sig); st != (Status{Signal: sig}) {
				t.Fatalf("status %+v, want death by %v; output %q", st, sig, out)
			}
		})
	}
}

// A signal the process ignores would not end it after exec either (SIG_IGN
// survives exec), nor would one whose default action stops or ignores:
// dieByExec returns, and the helper reports surviving.
func TestDieByExecReturnsWhenSignalWouldNotEndProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		args []string
	}{
		{"ignored SIGUSR2", unix.SIGUSR2, []string{"ignored"}},
		{"stop SIGTSTP", unix.SIGTSTP, nil},
		{"default-ignored SIGWINCH", unix.SIGWINCH, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if st, out := dieExecStatus(t, tc.sig, tc.args...); st != (Status{Code: helperFailCode}) {
				t.Fatalf("status %+v, want the helper to survive and exit %d; output %q", st, helperFailCode, out)
			}
		})
	}
}
