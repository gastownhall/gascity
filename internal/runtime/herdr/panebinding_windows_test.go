//go:build windows

package herdr

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

func stubPaneShellChildren(t *testing.T, fn func(bin string, shellPID int) ([]paneChild, error)) {
	t.Helper()
	prev := paneShellChildren
	paneShellChildren = fn
	t.Cleanup(func() { paneShellChildren = prev })
}

// TestShellProbeReadsBusynessFromTheShellsConsole pins the Windows probe.
// herdr lists only the pane's shell, whose image is never one of the Unix
// interactive shells, so the verdict must come from the shell's children and
// not from that list: busy while one of them shares the shell's console.
func TestShellProbeReadsBusynessFromTheShellsConsole(t *testing.T) {
	var children []paneChild
	var listErr error
	var asked []int
	stubPaneShellChildren(t, func(_ string, shellPID int) ([]paneChild, error) {
		asked = append(asked, shellPID)
		return children, listErr
	})
	p := New("probe", t.TempDir(), t.TempDir(), 0, 0)
	fg := []proc{{PID: 4242, Name: "powershell.exe"}}
	child := func(pid int, name string, attached bool) paneChild {
		return paneChild{ChildProcess: proctable.ChildProcess{PID: pid, Name: name, Created: time.Now()}, Attached: attached}
	}

	if got, err := p.shellProbe(4242, fg); err != nil || got != (paneProbe{Exists: true}) {
		t.Fatalf("a shell with no children probes %+v, %v; want idle", got, err)
	}
	children = []paneChild{child(4300, "pwsh.exe", true)}
	if got, err := p.shellProbe(4242, fg); err != nil || got != (paneProbe{Exists: true, Busy: true}) {
		t.Fatalf("a shell running a child in its console probes %+v, %v; want busy", got, err)
	}
	// A background job, or a helper given a window of its own, runs beside the
	// pane. Counted, it kept an idle prompt busy for as long as it lived.
	children = []paneChild{child(4301, "powershell.exe", false), child(4302, "PING.EXE", false)}
	if got, err := p.shellProbe(4242, fg); err != nil || got != (paneProbe{Exists: true}) {
		t.Fatalf("a shell whose only children run beside the pane probes %+v, %v; want idle", got, err)
	}
	children = append(children, child(4303, "claude.exe", true))
	if got, err := p.shellProbe(4242, fg); err != nil || got != (paneProbe{Exists: true, Busy: true}) {
		t.Fatalf("a shell running an agent beside a background job probes %+v, %v; want busy", got, err)
	}
	if len(asked) != 4 || asked[0] != 4242 || asked[3] != 4242 {
		t.Fatalf("children were listed for %v; want the pane shell 4242 each time", asked)
	}

	// A table that could not be read proves nothing, and must not read as
	// either verdict.
	listErr = errors.New("snapshot unavailable")
	if got, err := p.shellProbe(4242, fg); err == nil {
		t.Fatalf("a failed child listing probes %+v with no error", got)
	}

	// No shell PID is the zero probe, as it is on Unix, without a table read.
	asked = nil
	if got, err := p.shellProbe(0, nil); err != nil || got != (paneProbe{}) || len(asked) != 0 {
		t.Fatalf("shell PID 0 probes %+v, %v after %d listings; want the zero probe and none", got, err, len(asked))
	}
}

// TestMarkAttachedReadsTheShellsConsole pins how a pane shell's children are
// marked from their console hosts. A child that shares the shell's console is
// attached and one on another is not; a child that exited since the snapshot
// is dropped; and wherever a console cannot be read, the shell's or the
// child's, the child counts as attached, since an unknown child read as idle
// would reap a pane still running it.
func TestMarkAttachedReadsTheShellsConsole(t *testing.T) {
	const shellPID = 4242
	created := time.Unix(1700000000, 0)
	children := []proctable.ChildProcess{
		{PID: 4300, Name: "pwsh.exe", Created: created},
		{PID: 4301, Name: "powershell.exe", Created: created},
		{PID: 4302, Name: "claude.exe", Created: created},
		{PID: 4303, Name: "PING.EXE", Created: created},
	}
	denied := errors.New("access is denied")
	for _, tc := range []struct {
		name      string
		shellHost int
		shellErr  error
		want      map[int]bool // PID to Attached; a PID left out is dropped
	}{
		{name: "the shell's console reads", shellHost: 900, want: map[int]bool{4300: true, 4301: false, 4302: true}},
		{name: "the shell's console cannot be read", shellErr: denied, want: map[int]bool{4300: true, 4301: true, 4302: true, 4303: true}},
		{name: "the shell has no console", shellHost: 0, want: map[int]bool{4300: true, 4301: true, 4302: true, 4303: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fenced []time.Time
			consoleHost := func(pid int, when time.Time) (int, error) {
				if pid == shellPID {
					return tc.shellHost, tc.shellErr
				}
				fenced = append(fenced, when)
				switch pid {
				case 4300:
					return 900, nil
				case 4301:
					return 901, nil
				case 4302:
					return 0, denied
				default:
					return 0, proctable.ErrProcessGone
				}
			}
			got := map[int]bool{}
			for _, child := range markAttached(shellPID, children, consoleHost) {
				got[child.PID] = child.Attached
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("marked %v; want %v", got, tc.want)
			}
			for _, when := range fenced {
				if !when.Equal(created) {
					t.Errorf("a child's console was read at creation time %v; want its own, %v", when, created)
				}
			}
		})
	}
}
