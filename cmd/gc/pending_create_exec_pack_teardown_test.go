package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// execPackScript is a minimal exec session pack whose boxes are files in its
// state dir. Flag files in the same dir switch its answers:
//
//   - fail-stop: stop exits 1;
//   - no-is-running: is-running is an unknown op (exit 2);
//   - get-meta-errors-when-missing: get-meta exits 1 for a box that does not
//     exist (otherwise it prints nothing and exits 0, as for an unset key).
//
// get-meta answers a key from a meta.<name>.<key> file; the pack never writes
// one, so a box carries no identity, as on a pack that keeps no metadata.
const execPackScript = `#!/bin/sh
dir=%DIR%
op=$1
name=$2
case "$op" in
start) touch "$dir/box.$name" ;;
stop)
  if [ -f "$dir/fail-stop" ]; then echo "backend unreachable" >&2; exit 1; fi
  rm -f "$dir/box.$name" ;;
is-running)
  if [ -f "$dir/no-is-running" ]; then exit 2; fi
  if [ -f "$dir/box.$name" ]; then echo true; else echo false; fi ;;
get-meta)
  if [ ! -f "$dir/box.$name" ] && [ -f "$dir/get-meta-errors-when-missing" ]; then
    echo "no such session: $name" >&2; exit 1
  fi
  cat "$dir/meta.$name.$3" 2>/dev/null
  exit 0 ;;
list-running)
  for f in "$dir"/box.*; do
    [ -e "$f" ] || continue
    n=${f#"$dir"/box.}
    case "$n" in "$name"*) echo "$n" ;; esac
  done ;;
*) exit 2 ;;
esac
`

// TestPendingCreateRollback_ExecPackTeardown runs the restart shape over the
// real exec provider (sessionexec.NewSeamBacked) and a scripted pack, so the
// teardown meets the pack's real exit-code semantics rather than a profiled
// fake's.
//
// The last case pins the get-meta contract the teardown relies on
// (docs/reference/exec-session-provider.md, "Session identity"): get-meta
// must print nothing and exit 0 for a session that does not exist. A pack
// whose get-meta exits non-zero there reads as "identity cannot be read", and
// the manual row is held open in creating with its claim until get-meta
// answers or an operator closes it.
func TestPendingCreateRollback_ExecPackTeardown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		boxUp    bool
		flags    []string
		wantHeld bool
	}{
		{name: "box up, get-meta empty", boxUp: true},
		{name: "box up, no is-running", boxUp: true, flags: []string{"no-is-running"}},
		{name: "box gone, get-meta empty when missing"},
		{name: "stop fails", boxUp: true, flags: []string{"fail-stop"}, wantHeld: true},
		{name: "box gone, get-meta errors when missing", flags: []string{"get-meta-errors-when-missing"}, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "pack.sh")
			if err := os.WriteFile(script, []byte(strings.ReplaceAll(execPackScript, "%DIR%", dir)), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range tc.flags {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env, session, name := newRestartedPendingCreate(t, rowManual)
			// The env's fake is not the runtime here; the pack's state dir is.
			box := filepath.Join(dir, "box."+name)
			if tc.boxUp {
				if err := os.WriteFile(box, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sp := sessionexec.NewSeamBacked(script)
			for tick := 1; tick <= 3; tick++ {
				got := reconcileTeardownTick(t, env, sp, session.ID)
				env.clk.Advance(30 * time.Second)
				_, statErr := os.Stat(box)
				boxUp := statErr == nil
				if tc.wantHeld {
					if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" || boxUp != tc.boxUp {
						t.Fatalf("tick %d: status=%s state=%q claim=%q box=%v, want the row held open in creating and the box as it was; stderr=%s",
							tick, got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"], boxUp, env.stderr.String())
					}
					continue
				}
				if got.Status != "closed" || boxUp {
					t.Fatalf("status=%s state=%q box=%v, want the box gone and the row closed after one tick; stderr=%s",
						got.Status, got.Metadata["state"], boxUp, env.stderr.String())
				}
				return
			}
		})
	}
}
