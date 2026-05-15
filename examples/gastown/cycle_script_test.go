package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCycleScriptGroupsBySeparator exercises cycle.sh against a stubbed
// tmux that emits a controlled session list and logs switch-client calls.
//
// The script under test groups sessions by name shape:
//
//	"<rig>--*"    -> rig group (witness, refinery, polecat, crew, dispatcher)
//	"<scope>__*"  -> scope group (e.g. gastown__mayor, gastown__deacon)
//	"<base>-N"    -> pool (e.g. dog-1, dog-2)
//	anything else -> catch-all (cycle all sessions on socket)
//
// The cases below cover the bug fixes:
//  1. From <rig>--witness with refinery + polecats dormant, cycling next
//     should reach a crew member instead of stranding (the prior shape
//     narrowed the pattern to ^<rig>--\(witness|refinery|polecat-\)
//     which collapses to a single match).
//  2. <scope>__* sessions cycle as a group (the prior shape only matched
//     bare "mayor"/"deacon" and let scope-prefixed sessions fall through
//     to the catch-all that cycles every session on the socket).
//
// Plus regression coverage for the dog pool (now generic *-N) and the
// single-member no-op case (cycle target == self -> no switch).
func TestCycleScriptGroupsBySeparator(t *testing.T) {
	cycleScript := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "cycle.sh")
	if _, err := os.Stat(cycleScript); err != nil {
		t.Fatalf("cycle.sh not found at %s: %v", cycleScript, err)
	}

	tests := []struct {
		name      string
		current   string
		direction string
		sessions  []string
		// wantTarget = "" means no switch-client call expected
		// (single-member group, or cycle target == self).
		wantTarget string
	}{
		{
			name:      "rig: witness with dormant infra reaches crew",
			current:   "gascity--witness",
			direction: "next",
			sessions: []string{
				"gascity--control-dispatcher",
				"gascity--navani",
				"gascity--witness",
			},
			wantTarget: "gascity--control-dispatcher", // wrap from end
		},
		{
			name:      "rig: prev from witness rotates to crew member",
			current:   "gascity--witness",
			direction: "prev",
			sessions: []string{
				"gascity--control-dispatcher",
				"gascity--navani",
				"gascity--witness",
			},
			wantTarget: "gascity--navani",
		},
		{
			name:      "rig: cycle from crew rotates within rig",
			current:   "myrig--alice",
			direction: "next",
			sessions: []string{
				"myrig--alice",
				"myrig--bob",
				"myrig--witness",
				"otherrig--alice",
			},
			wantTarget: "myrig--bob",
		},
		{
			name:      "rig: cycle does not cross to other rigs",
			current:   "myrig--alice",
			direction: "next",
			sessions: []string{
				"myrig--alice",
				"otherrig--bob",
			},
			wantTarget: "", // alone in myrig group
		},
		{
			name:      "scope: groups all <scope>__* together",
			current:   "gastown__mayor",
			direction: "next",
			sessions: []string{
				"gastown__boot",
				"gastown__deacon",
				"gastown__mayor",
				"otherscope__bar",
			},
			wantTarget: "gastown__boot", // wrap from end
		},
		{
			name:      "scope: prev cycles within scope",
			current:   "gastown__mayor",
			direction: "prev",
			sessions: []string{
				"gastown__boot",
				"gastown__deacon",
				"gastown__mayor",
			},
			wantTarget: "gastown__deacon",
		},
		{
			name:      "pool: cycles same-base -N members",
			current:   "dog-1",
			direction: "next",
			sessions: []string{
				"dog-1",
				"dog-2",
				"dog-3",
				"mayor",
			},
			wantTarget: "dog-2",
		},
		{
			name:      "pool: does not cycle across base names",
			current:   "dog-1",
			direction: "next",
			sessions: []string{
				"dog-1",
				"cat-2",
			},
			wantTarget: "", // alone in dog- pool
		},
		{
			name:      "single-member group is a no-op",
			current:   "alone--singleton",
			direction: "next",
			sessions: []string{
				"alone--singleton",
				"other--something",
			},
			wantTarget: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			switchLog := filepath.Join(t.TempDir(), "tmux-switch.log")

			// Stub tmux: emits the controlled session list for list-sessions,
			// logs switch-client calls so we can assert on the target.
			sessionList := strings.Join(tt.sessions, "\n") + "\n"
			writeExecutable(t, filepath.Join(binDir, "tmux"), `#!/bin/sh
# Drop a leading "-L <socket>" pair if present (cycle.sh sets it via
# GC_TMUX_SOCKET when configured; we leave that env unset here, but be
# defensive in case the harness changes).
if [ "$1" = "-L" ]; then
    shift 2
fi
case "$1" in
  list-sessions)
    cat <<'__GC_CYCLE_SESSIONS_EOF__'
`+sessionList+`__GC_CYCLE_SESSIONS_EOF__
    ;;
  switch-client)
    printf '%s\n' "$*" >> "$TMUX_SWITCH_LOG"
    ;;
esac
exit 0
`)

			env := map[string]string{
				"PATH":            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"TMUX_SWITCH_LOG": switchLog,
				"GC_TMUX_SOCKET":  "", // disable -L flag so stub sees clean argv
			}

			cmd := exec.Command(cycleScript, tt.direction, tt.current, "/dev/pts/test")
			cmd.Env = mergeTestEnv(env)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("cycle.sh failed: %v\n%s", err, out)
			}

			logBytes, _ := os.ReadFile(switchLog)
			log := string(logBytes)

			if tt.wantTarget == "" {
				if log != "" {
					t.Fatalf("expected no switch-client call, got log: %q", log)
				}
				return
			}

			// switch-client argv is "switch-client -c <client> -t <target>".
			// "-t <target>" anchors the assertion regardless of client value.
			want := "-t " + tt.wantTarget
			if !strings.Contains(log, want) {
				t.Fatalf("expected switch to %q, got log: %q", tt.wantTarget, log)
			}
		})
	}
}
