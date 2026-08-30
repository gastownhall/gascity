package exec

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// occupiedBoxScript is an adapter whose box for `name` is already up and whose
// start op refuses in wording the phrase detector does NOT recognize. It
// answers `is-running` structurally, the way a pack that can report occupancy
// does.
func occupiedBoxScript(startLog, stopLog, refusal string) string {
	return `
op="$1"
name="$2"

case "$op" in
  is-running) echo "true" ;;
  start)
    cat > /dev/null
    echo "$name" >> "` + startLog + `"
    echo "` + refusal + `" >&2
    exit 1
    ;;
  stop) echo "stop $name" >> "` + stopLog + `" ;;
  *) exit 2 ;;
esac
`
}

// vacantBoxScript is the same adapter with the box absent: `is-running` answers
// "false" and the start op fails for an ordinary reason.
func vacantBoxScript(startLog, stopLog string) string {
	return `
op="$1"
name="$2"

case "$op" in
  is-running) echo "false" ;;
  start)
    cat > /dev/null
    echo "$name" >> "` + startLog + `"
    echo "readiness timeout" >&2
    exit 1
    ;;
  stop) echo "stop $name" >> "` + stopLog + `" ;;
  *) exit 2 ;;
esac
`
}

// TestStartNeverTearsDownABoxThatWasAlreadyOccupied is the ga-vcjr9
// composition guard between identity-stable session names and the start-failure
// teardown.
//
// Before identity-stable naming every start attempt used a fresh bead-ID name,
// so a collision meant "somebody else's box" and was rare. Now a pool retry
// deliberately re-targets the slot's own name, which makes collisions
// steady-state — and exec is the only provider that infers ErrSessionExists
// from the adapter's *stderr wording*. A pack whose refusal is phrased outside
// startCollisionPhrases falls through that guard, and
// cleanupAfterStartFailure then tears down a live agent's box. Losing an
// agent's work is strictly worse than the leak this whole effort is closing.
//
// So the teardown stops depending on the wording. Occupancy is established
// structurally before the attempt, and a box that was already up is never one
// this attempt may destroy — whatever the adapter said on the way out.
func TestStartNeverTearsDownABoxThatWasAlreadyOccupied(t *testing.T) {
	// Deliberately none of startCollisionPhrases.
	refusals := map[string]string{
		"in use":     "session is in use by another agent",
		"conflict":   "409 conflict: name taken",
		"busy":       "box busy",
		"no message": "",
	}

	for label, refusal := range refusals {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			startLog := filepath.Join(dir, "start.log")
			stopLog := filepath.Join(dir, "stop.log")
			p := NewProvider(writeScript(t, dir, occupiedBoxScript(startLog, stopLog, refusal)))

			err := p.Start(context.Background(), "test-sess", runtime.Config{})
			if err == nil {
				t.Fatal("Start succeeded, want the adapter's refusal")
			}
			if got := readLog(t, stopLog); got != "" {
				t.Fatalf("stop log = %q, want no teardown: that box was up before this attempt and belongs to a live session", got)
			}
		})
	}
}

// TestStartStillTearsDownAVacantBoxItFailedToFill is the discriminating control
// for the guard above. Same adapter shape, same failing start — the only
// difference is that occupancy was structurally answered "false" before the
// attempt, so the box that exists afterwards is this attempt's own and the
// ga-vcjr9 teardown must still fire. Without this control the guard could be
// satisfied by never tearing anything down, which is the leak it replaced.
func TestStartStillTearsDownAVacantBoxItFailedToFill(t *testing.T) {
	dir := t.TempDir()
	startLog := filepath.Join(dir, "start.log")
	stopLog := filepath.Join(dir, "stop.log")
	p := NewProvider(writeScript(t, dir, vacantBoxScript(startLog, stopLog)))

	err := p.Start(context.Background(), "test-sess", runtime.Config{})
	if err == nil {
		t.Fatal("Start succeeded, want the adapter's start failure")
	}
	if errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start error = %v, want the adapter's failure, not a collision", err)
	}
	if got := readLog(t, startLog); !strings.Contains(got, "test-sess") {
		t.Fatalf("start log = %q, want the start op attempted against a vacant box", got)
	}
	if got := readLog(t, stopLog); !strings.Contains(got, "stop test-sess") {
		t.Fatalf("stop log = %q, want the box this attempt created torn down", got)
	}
}

// TestStartPreCheckDoesNotChangeBehaviourForPacksThatCannotReportOccupancy
// bounds the blast radius. A pack with no `is-running` op answers exit 2
// (unknown op), which is not an occupancy answer in either direction. Such a
// pack must keep exactly today's behavior — attempt the start, tear down on
// failure, and fall back to the stderr phrase detector for collisions — rather
// than losing the teardown that stopped the original ga-vcjr9 pod leak.
func TestStartPreCheckDoesNotChangeBehaviourForPacksThatCannotReportOccupancy(t *testing.T) {
	dir := t.TempDir()
	createFile := filepath.Join(dir, "create.log")
	stopFile := filepath.Join(dir, "stop.log")
	// startFailureScript answers every op other than start/stop with exit 2.
	p := NewProvider(writeScript(t, dir, startFailureScript(createFile, stopFile, "readiness timeout")))

	err := p.Start(context.Background(), "test-sess", runtime.Config{})
	if err == nil {
		t.Fatal("Start succeeded, want the adapter's start failure")
	}
	if got := readLog(t, stopFile); !strings.Contains(got, "stop test-sess") {
		t.Fatalf("stop log = %q, want the ga-vcjr9 teardown preserved for occupancy-blind packs", got)
	}
}

// TestBoxOccupancy pins the three-valued contract directly: only an explicit
// "true"/"false" is an answer. Anything else — an op error, an unknown op, a
// pack that prints nothing — is "I could not tell", and callers must not read
// it as vacancy.
func TestBoxOccupancy(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantOccupied bool
		wantDefinite bool
	}{
		{
			name:         "true",
			body:         "case \"$1\" in is-running) echo true ;; *) exit 2 ;; esac",
			wantOccupied: true,
			wantDefinite: true,
		},
		{
			name:         "false",
			body:         "case \"$1\" in is-running) echo false ;; *) exit 2 ;; esac",
			wantOccupied: false,
			wantDefinite: true,
		},
		{
			name:         "unknown op",
			body:         "exit 2",
			wantOccupied: false,
			wantDefinite: false,
		},
		{
			name:         "op error",
			body:         "echo 'adapter unreachable' >&2; exit 1",
			wantOccupied: false,
			wantDefinite: false,
		},
		{
			name:         "silent",
			body:         "exit 0",
			wantOccupied: false,
			wantDefinite: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := NewProvider(writeScript(t, dir, tc.body))
			occupied, definite := p.boxOccupancy("test-sess")
			if occupied != tc.wantOccupied || definite != tc.wantDefinite {
				t.Fatalf("boxOccupancy = (%v, %v), want (%v, %v)", occupied, definite, tc.wantOccupied, tc.wantDefinite)
			}
		})
	}
}
