package herdr

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeNudgePane is a stand-in herdr CLI for deliverNudge's closed-loop
// delivery: a shell script whose `agent read` / `agent get` answers are a
// function of how many pastes (`pane run`) and Enters (`pane send-keys`) have
// occurred so far, so tests can script swallowed pastes, swallowed submits,
// and agent-status lag. Thresholds:
//   - pasteLandsAt: the nth paste is the first one that becomes visible
//   - enterLandsAt: the nth Enter is the first one that actually submits
//   - statusFlips: whether agent_status ever leaves "idle" after the submit
//     (false models hook/first-token latency outlasting the whole loop)
type fakeNudgePane struct {
	bin    string
	pastes string // one byte appended per `pane run`
	enters string // one byte appended per `pane send-keys`
}

func newFakeNudgePane(t *testing.T, pasteLandsAt, enterLandsAt int, statusFlips bool) *fakeNudgePane {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake herdr CLI is a POSIX shell script")
	}
	dir := t.TempDir()
	f := &fakeNudgePane{
		bin:    filepath.Join(dir, "herdr"),
		pastes: filepath.Join(dir, "pastes"),
		enters: filepath.Join(dir, "enters"),
	}
	flip := "0"
	if statusFlips {
		flip = "1"
	}
	script := `#!/bin/sh
shift 2 # drop --session <name>
pastes=0; [ -f '` + f.pastes + `' ] && pastes=$(wc -c < '` + f.pastes + `' | tr -d ' ')
enters=0; [ -f '` + f.enters + `' ] && enters=$(wc -c < '` + f.enters + `' | tr -d ' ')
submitted=0
if [ "$pastes" -ge ` + strconv.Itoa(pasteLandsAt) + ` ] && [ "$enters" -ge ` + strconv.Itoa(enterLandsAt) + ` ]; then submitted=1; fi
case "$1 $2" in
"pane run") printf x >> '` + f.pastes + `' ;;
"pane send-keys") printf x >> '` + f.enters + `' ;;
"agent read")
	if [ "$submitted" -eq 1 ]; then text="turn running: streaming output"
	elif [ "$pastes" -ge ` + strconv.Itoa(pasteLandsAt) + ` ]; then text="prompt with [Pasted text #1]"
	else text="empty prompt"; fi
	printf '{"result":{"read":{"text":"%s"}}}' "$text" ;;
"agent get")
	status=idle
	if [ ` + flip + ` -eq 1 ] && [ "$submitted" -eq 1 ]; then status=busy; fi
	printf '{"result":{"agent":{"name":"%s","pane_id":"p1","agent_status":"%s"}}}' "$3" "$status" ;;
*) printf '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake herdr: %v", err)
	}
	return f
}

func (f *fakeNudgePane) count(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return len(b)
}

// TestDeliverNudgeClosedLoop pins deliverNudge's two-phase delivery contract:
// the paste retries only while the screen provably didn't take it, and the
// submit phase retries the Enter only — never the paste. The load-bearing
// assertion in every case is the paste count: a landed submit whose agent
// still reports idle (hook/first-token latency, seconds under town-restart
// load) must not trigger a re-paste, because each re-paste+re-Enter queues a
// duplicate turn. Regression: 2026-07-06, a named session received its
// startup prime 4× because the old single loop re-delivered per idle poll.
func TestDeliverNudgeClosedLoop(t *testing.T) {
	restore := submitSettleDelay
	submitSettleDelay = time.Millisecond
	t.Cleanup(func() { submitSettleDelay = restore })

	tests := []struct {
		name         string
		pasteLandsAt int
		enterLandsAt int
		statusFlips  bool
		wantErr      string // substring; "" means success
		wantPastes   int
		wantEnters   int
	}{
		{
			name:         "status lag: submit landed but agent never leaves idle → one paste, success via box-consumed",
			pasteLandsAt: 1, enterLandsAt: 1, statusFlips: false,
			wantPastes: 1, wantEnters: 1,
		},
		{
			name:         "swallowed paste re-pastes until it lands",
			pasteLandsAt: 2, enterLandsAt: 1, statusFlips: true,
			wantPastes: 2, wantEnters: 1,
		},
		{
			name:         "swallowed Enter retries the Enter only, never the paste",
			pasteLandsAt: 1, enterLandsAt: 3, statusFlips: true,
			wantPastes: 1, wantEnters: 3,
		},
		{
			name:         "paste never lands → error before any Enter is spent",
			pasteLandsAt: 99, enterLandsAt: 1, statusFlips: true,
			wantErr: "paste never landed", wantPastes: submitMaxAttempts, wantEnters: 0,
		},
		{
			name:         "submit never confirms → error, still exactly one paste",
			pasteLandsAt: 1, enterLandsAt: 99, statusFlips: true,
			wantErr: "submit unconfirmed", wantPastes: 1, wantEnters: submitMaxAttempts,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeNudgePane(t, tt.pasteLandsAt, tt.enterLandsAt, tt.statusFlips)
			c := &client{session: "s", bin: f.bin}
			err := c.deliverNudge(context.Background(), "p1", "mayor", "STARTUP NUDGE")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("deliverNudge: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("deliverNudge err = %v, want substring %q", err, tt.wantErr)
			}
			if got := f.count(t, f.pastes); got != tt.wantPastes {
				t.Errorf("pastes = %d, want %d", got, tt.wantPastes)
			}
			if got := f.count(t, f.enters); got != tt.wantEnters {
				t.Errorf("enters = %d, want %d", got, tt.wantEnters)
			}
		})
	}
}
