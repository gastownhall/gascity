package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file drives Store.Close over a REAL beads.BdStore — the store a
// schema-v59 managed-Dolt city actually runs (beads.NewBdStoreWithPrefix, with
// or without a CachingStore in front) — instead of a synthetic capability
// double. Both ga-f7v2ft.78.6 incident cohorts ran that configuration, and the
// existing staleAwakeDuringCloseStore regression cannot speak to it: that
// double implements CloseWithMetadataIfMatch itself, so it always steers Close
// onto the atomic arm no matter what BdStore can do.
//
// fakeBd is a small stateful bd simulation: one row, a --help surface the
// production capability probes read, and the four verbs BdStore issues on the
// close path (show / update / close). It is deliberately NOT a general bd
// emulator — it answers exactly what the terminal-close path asks.
type fakeBd struct {
	mu       sync.Mutex
	id       string
	status   string
	metadata map[string]string
	revision int64

	// argv records every non-help invocation in order.
	argv [][]string
	// mutations counts the bd commands that changed the row: the split close
	// sequence issues two, the fused terminal close issues one.
	mutations int
	// plainCloses counts `bd close` invocations — the second leg of the unsafe
	// split sequence, and zero once the fused path is taken.
	plainCloses int

	// ifStatusCapable advertises `bd update --if-status` (present in the pinned
	// schema-v59 bd). ifRevisionCapable advertises `--if-revision`, which that
	// bd does NOT have (beads#4682 is unlanded) — the default here matches the
	// real binary.
	ifStatusCapable   bool
	ifRevisionCapable bool

	// staleAwakeBeforeClose stamps state=awake exactly once, immediately before
	// a `bd close` executes: the deterministic stand-in for the controller cycle
	// that wrote its older awake observation between ClosePatch and Close in
	// both incidents.
	staleAwakeBeforeClose bool
	firedStale            bool
}

func newFakeBd(id string, metadata map[string]string) *fakeBd {
	md := map[string]string{}
	for k, v := range metadata {
		md[k] = v
	}
	return &fakeBd{
		id:              id,
		status:          "open",
		metadata:        md,
		revision:        41,
		ifStatusCapable: true,
	}
}

func (f *fakeBd) runner(_, _ string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "--dolt-auto-commit" {
		args = args[2:]
	}
	if len(args) == 0 {
		return nil, errors.New("fakeBd: empty argv")
	}
	if len(args) >= 2 && args[1] == "--help" {
		return f.help(args[0]), nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, append([]string(nil), args...))
	switch args[0] {
	case "show":
		return f.rowJSONLocked(true), nil
	case "update":
		return f.updateLocked(args)
	case "close":
		return f.closeLocked(), nil
	default:
		return nil, fmt.Errorf("fakeBd: unhandled verb %q", args[0])
	}
}

func (f *fakeBd) help(verb string) []byte {
	flags := "  --json   emit JSON\n"
	if f.ifRevisionCapable {
		flags += "      --if-revision int   apply only at this revision\n"
	}
	if verb == "update" && f.ifStatusCapable {
		flags += "      --if-status string   apply only at this status\n"
	}
	return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n" + flags)
}

// rowJSONLocked renders the row the way bd does. `bd show --json` carries the
// revision token; the `bd update --json` echo does not (verified against
// /opt/beads/releases/gc-bf97b73749ac-20260805/bd), so withRevision mirrors
// that split rather than papering over it.
func (f *fakeBd) rowJSONLocked(withRevision bool) []byte {
	md, _ := json.Marshal(f.metadata)
	rev := ""
	if withRevision {
		rev = fmt.Sprintf(`,"revision":%d`, f.revision)
	}
	return []byte(fmt.Sprintf(
		`[{"id":%q,"title":"probe","status":%q,"issue_type":%q,"labels":[%q],"created_at":"2026-01-02T03:04:05Z","metadata":%s%s}]`,
		f.id, f.status, BeadType, LabelSession, md, rev))
}

func (f *fakeBd) updateLocked(args []string) ([]byte, error) {
	if want, ok := flagValue(args, "--if-status"); ok && want != f.status {
		return nil, fmt.Errorf(
			`exit status 13: Error updating %s: status mismatch: %s has status %q, expected %q`,
			f.id, f.id, f.status, want)
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--set-metadata" {
			if eq := strings.IndexByte(args[i+1], '='); eq >= 0 {
				f.metadata[args[i+1][:eq]] = args[i+1][eq+1:]
			}
		}
	}
	if status, ok := flagValue(args, "--status"); ok {
		f.status = status
	}
	f.revision++
	f.mutations++
	return f.rowJSONLocked(false), nil
}

func (f *fakeBd) closeLocked() []byte {
	f.plainCloses++
	if f.staleAwakeBeforeClose && !f.firedStale {
		f.firedStale = true
		f.metadata["state"] = string(StateAwake)
		f.revision++
	}
	f.status = "closed"
	f.revision++
	f.mutations++
	return f.rowJSONLocked(false)
}

func flagValue(args []string, flag string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func (f *fakeBd) snapshot() (status string, state string, mutations, plainCloses int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.metadata["state"], f.mutations, f.plainCloses
}

// TestCloseOverBdStoreCannotStrandAwakeStateOnAClosedRow is the ga-f7v2ft.78.6
// regression at the store the incidents actually ran. A stale controller write
// lands in the window between ClosePatch and Close; the durable row must never
// come to rest closed with a nonterminal lifecycle state, and the terminal
// write must be a single fenced bd command rather than a metadata write
// followed by a separate close.
func TestCloseOverBdStoreCannotStrandAwakeStateOnAClosedRow(t *testing.T) {
	fake := newFakeBd("s-a1-wisp-377", map[string]string{"state": string(StateSuspended)})
	fake.staleAwakeBeforeClose = true
	front := NewStore(beads.SessionStore{Store: beads.NewBdStore("/city", fake.runner)})

	closed, err := front.Close("s-a1-wisp-377", "drained", time.Date(2026, 8, 8, 10, 17, 35, 0, time.UTC))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("Close reported not-closed for an open suspended session")
	}

	status, state, mutations, plainCloses := fake.snapshot()
	if status != "closed" || state != string(StateDrained) {
		t.Fatalf("durable row = closed=%v state=%q; want closed=true state=%s (closed+awake is the incident signature)",
			status == "closed", state, StateDrained)
	}
	if mutations != 1 {
		t.Fatalf("terminal close issued %d mutating bd commands, want 1 fused command; argv=%v", mutations, fake.argv)
	}
	if plainCloses != 0 {
		t.Fatalf("terminal close fell back to %d unfenced `bd close` calls, want 0", plainCloses)
	}
}

// TestBdStoreAdvertisesAtomicTerminalClose pins the capability discovery the
// fix turns on: session.Store.Close only takes the atomic arm when
// AtomicConditionalCloserFor resolves a closer through the typed class wrapper.
func TestBdStoreAdvertisesAtomicTerminalClose(t *testing.T) {
	fake := newFakeBd("s-1", map[string]string{"state": string(StateAwake)})
	store := beads.SessionStore{Store: beads.NewBdStore("/city", fake.runner)}
	if _, ok := beads.AtomicConditionalCloserFor(store); !ok {
		t.Fatal("AtomicConditionalCloserFor(SessionStore{BdStore}) = unavailable; the split close sequence stays live on real v59")
	}
}

// TestCloseOverBdStoreYieldsToAWinningCloser proves the fused write is fenced:
// when another actor closes the row first, the terminal close reports
// already-closed instead of blindly re-closing, and never falls back to the
// unfenced split sequence.
func TestCloseOverBdStoreYieldsToAWinningCloser(t *testing.T) {
	fake := newFakeBd("s-2", map[string]string{"state": string(StateSuspended)})
	front := NewStore(beads.SessionStore{Store: beads.NewBdStore("/city", fake.runner)})

	fake.mu.Lock()
	fake.status = "closed"
	fake.mu.Unlock()

	closed, err := front.Close("s-2", "drained", time.Now())
	if err != nil {
		t.Fatalf("Close on an already-closed row: %v", err)
	}
	if closed {
		t.Fatal("Close reported closed for a row another actor had already closed")
	}
	if _, _, mutations, plainCloses := fake.snapshot(); mutations != 0 || plainCloses != 0 {
		t.Fatalf("already-closed row took %d mutations / %d plain closes, want 0/0", mutations, plainCloses)
	}
}
