package programstatus

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// replay runs a golden capture through Scanner, ParseBody and Store, read by
// read as tmux delivered it, and returns the store and the root state after
// every applied report ("-" when no root, "probe"/"prompt" for those events).
func replay(t *testing.T, name string) (*Store, []string) {
	t.Helper()
	c := loadCapture(t, name)
	s := NewStore(testNonce)
	var sc Scanner
	var trace []string
	n := 0
	for _, chunk := range c.chunks() {
		sc.Feed(chunk, func(e Event) {
			n++
			switch e.Kind {
			case EventProbe:
				s.Probe()
				trace = append(trace, "probe")
			case EventPromptStart:
				s.PromptStart()
				trace = append(trace, "prompt")
			case EventReport:
				r, err := ParseBody(e.Body)
				if err != nil {
					if errors.Is(err, ErrDropped) {
						trace = append(trace, "dropped")
						return
					}
					t.Fatalf("ParseBody(%q): %v", e.Body, err)
				}
				s.Apply(r, at(n))
				trace = append(trace, rootWord(s.Status()))
			}
		})
	}
	if sc.Discarded() != 0 {
		t.Fatalf("%s: %d sequences discarded", name, sc.Discarded())
	}
	return s, trace
}

func rootWord(s runtime.ProgramStatus) string {
	switch {
	case s.State == "":
		return "-"
	case s.Kind != "":
		return string(s.State) + ":" + string(s.Kind)
	default:
		return string(s.State)
	}
}

func assertTrace(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace\n got %v\nwant %v", got, want)
	}
}

// TestGoldenClaudeBELTurns replays Claude Code 2.1.311 behind the
// probe-answering shim: BEL-terminated reports with app=claude-code, a
// permission and a question dialog, a queued prompt, an Esc interrupt
// (idle, not done) and clear three times at exit.
func TestGoldenClaudeBELTurns(t *testing.T) {
	s, trace := replay(t, "claude-turns")
	assertTrace(t, trace, []string{
		"probe", "idle", "working", "blocked:permission", "working", "blocked:question", "working",
		"done", "working", "done", "working", "idle", "-", "-", "-",
	})
	got := s.Status()
	if !got.Probed || got.TurnStarts != 3 || got.TurnEnds != 3 || got.LastError || got.Children != 0 {
		t.Fatalf("final status %+v", got)
	}
}

func TestGoldenClaudePermissionText(t *testing.T) {
	c := loadCapture(t, "claude-turns")
	var sc Scanner
	var msgs []string
	sc.Feed(c.data, func(e Event) {
		if e.Kind != EventReport {
			return
		}
		r, err := ParseBody(e.Body)
		if err != nil {
			t.Fatalf("ParseBody(%q): %v", e.Body, err)
		}
		if r.State == runtime.ProgramBlocked {
			if r.App != "claude-code" {
				t.Errorf("app %q", r.App)
			}
			msgs = append(msgs, string(r.Kind)+"|"+r.Msg)
		}
	})
	assertTrace(t, msgs, []string{
		"permission|approve Bash: touch /tmp/osc7501x-work/claude-approved-marker",
		"question|answer: Which color do you prefer? (Red · Blue)",
	})
}

// TestGoldenCodexSTTurns replays Codex 0.165.0: ST-terminated, unprobed,
// with app before kind on its approval report.
func TestGoldenCodexSTTurns(t *testing.T) {
	s, trace := replay(t, "codex-turns")
	assertTrace(t, trace, []string{"idle", "working", "blocked:permission", "working", "done", "idle", "-"})
	got := s.Status()
	if got.Probed || got.TurnStarts != 1 || got.TurnEnds != 1 {
		t.Fatalf("final status %+v", got)
	}
}

// TestGoldenClaudeChildRecords replays two background subagents reported as
// flat-id child records with titles, then cleared by id.
func TestGoldenClaudeChildRecords(t *testing.T) {
	c := loadCapture(t, "claude-children")
	s := NewStore(testNonce)
	var sc Scanner
	maxChildren, maxWorking := 0, 0
	n := 0
	sc.Feed(c.data, func(e Event) {
		n++
		if e.Kind != EventReport {
			return
		}
		r, err := ParseBody(e.Body)
		if err != nil {
			t.Fatalf("ParseBody(%q): %v", e.Body, err)
		}
		s.Apply(r, at(n))
		st := s.Status()
		maxChildren = max(maxChildren, st.Children)
		maxWorking = max(maxWorking, st.WorkingChildren)
	})
	if maxChildren != 2 || maxWorking != 2 {
		t.Fatalf("children peaked at %d (%d working), want 2", maxChildren, maxWorking)
	}
	got := s.Status()
	if got.Children != 0 || got.TurnStarts != 1 || got.TurnEnds != 1 || got.State != "" {
		t.Fatalf("final status %+v", got)
	}
}

func TestGoldenClaudeErrorIsSticky(t *testing.T) {
	s, trace := replay(t, "claude-error")
	assertTrace(t, trace, []string{"probe", "idle", "working", "error", "-", "-", "-"})
	if got := s.Status(); !got.LastError || got.TurnEnds != 1 {
		t.Fatalf("final status %+v", got)
	}
	if got := s.Text().LastErrorMsg; got != "API Error: 400 mock invalid_request_error" {
		t.Fatalf("last error msg %q", got)
	}
}

func TestGoldenSyntheticSplitsThroughStore(t *testing.T) {
	c := loadCapture(t, "synthetic-split")
	s := NewStore(testNonce)
	var sc Scanner
	var question string
	for _, chunk := range c.chunks() {
		sc.Feed(chunk, func(e Event) {
			if e.Kind != EventReport {
				return
			}
			r, err := ParseBody(e.Body)
			if err != nil {
				t.Fatalf("ParseBody(%q): %v", e.Body, err)
			}
			if r.Kind == runtime.ProgramBlockQuestion {
				question = r.Msg
			}
			s.Apply(r, at(1))
		})
	}
	if question != "Allow `rm -rf build`?" {
		t.Fatalf("question msg %q", question)
	}
	if got := s.Status(); got.TurnStarts != 2 || got.State != "" {
		t.Fatalf("final status %+v", got)
	}
}
