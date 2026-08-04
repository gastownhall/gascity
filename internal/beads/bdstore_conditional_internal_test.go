package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestClassifyConditionalWriteResult exhaustively exercises the pure classifier
// that maps a bd conditional-write invocation's (out, err) to the typed
// ConditionalWriter error surface. bd #4682 (the revision column + --if-revision)
// is unlanded, so the precondition/unsupported substrings are provisional; this
// table pins the DEFENSIBLE interim set (the //go:build integration conformance
// row against a #4682-capable bd is the authoritative guard). Classification is
// message-substring / body-code based, never exit-code based (BdStore has no
// exit-code path; every existing classifier matches on err.Error()).
func TestClassifyConditionalWriteResult(t *testing.T) {
	t.Run("success returns nil", func(t *testing.T) {
		if got := classifyConditionalWriteResult([]byte(`{}`), nil); got != nil {
			t.Fatalf("classify(nil err) = %v, want nil", got)
		}
	})

	t.Run("precondition from flat body", func(t *testing.T) {
		out := []byte(`{"error":"revision precondition failed","code":"precondition-failed","expected_revision":5,"current_revision":8}`)
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 5, 8)
	})

	t.Run("precondition from data-wrapped envelope", func(t *testing.T) {
		out := []byte(`{"schema_version":6,"data":{"code":"precondition-failed","expected_revision":2,"current_revision":9}}`)
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 2, 9)
	})

	t.Run("precondition body wrapped in log noise", func(t *testing.T) {
		out := []byte("bd: writing to dolt\n{\"code\":\"precondition-failed\",\"expected_revision\":1,\"current_revision\":4}\n")
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 1, 4)
	})

	t.Run("precondition recovered from err string when stdout empty", func(t *testing.T) {
		// bd wrote the JSON envelope to stderr, so classifyBDExecResult folded it
		// into err.Error() and stdout is empty.
		err := errors.New(`exit status 1: {"code":"precondition-failed","expected_revision":3,"current_revision":7}`)
		assertPrecondition(t, classifyConditionalWriteResult(nil, err), 3, 7)
	})

	t.Run("precondition from revision fields without code", func(t *testing.T) {
		out := []byte(`{"error":"stale write","expected_revision":10,"current_revision":11}`)
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 10, 11)
	})

	t.Run("precondition message with unparseable body is zero-valued with Raw", func(t *testing.T) {
		err := errors.New("exit status 1: revision precondition failed")
		got := classifyConditionalWriteResult(nil, err)
		var pfe *PreconditionFailedError
		if !errors.As(got, &pfe) {
			t.Fatalf("classify = %v, want *PreconditionFailedError", got)
		}
		if pfe.Expected != 0 || pfe.Current != 0 {
			t.Fatalf("unparseable body: Expected/Current = %d/%d, want 0/0", pfe.Expected, pfe.Current)
		}
		if pfe.Raw == "" {
			t.Fatal("unparseable precondition must set Raw for forensics")
		}
	})

	t.Run("precondition inferred from non-JSON message forms", func(t *testing.T) {
		// The message-fallback substrings (no parseable body): each must classify
		// as a zero-valued precondition so a caller still re-reads rather than
		// hard-failing. Guards the "revision mismatch" and hyphenated
		// "precondition-failed" message forms bd might emit outside a JSON envelope.
		for _, msg := range []string{
			"exit status 1: revision mismatch on ga-1",
			"exit status 1: error: precondition-failed for ga-1 (expected 3, got 5)",
		} {
			got := classifyConditionalWriteResult(nil, errors.New(msg))
			if !IsPreconditionFailed(got) {
				t.Fatalf("classify(%q) = %v, want *PreconditionFailedError", msg, got)
			}
		}
	})

	t.Run("unsupported from machine body code latches", func(t *testing.T) {
		out := []byte(`{"error":"conditional writes not supported","code":"conditional-write-unsupported"}`)
		err := errors.New("exit status 1")
		if got := classifyConditionalWriteResult(out, err); !IsConditionalWriteUnsupported(got) {
			t.Fatalf("classify = %v, want ErrConditionalWriteUnsupported", got)
		}
	})

	t.Run("unsupported from pre-4682 unknown-flag usage error latches", func(t *testing.T) {
		// The exact pflag/stdlib-flag phrasings, which put the flag token
		// immediately after the marker.
		for _, msg := range []string{
			"exit status 1: unknown flag: --if-revision",
			"exit status 1: unknown flag: -if-revision",
			"exit status 1: flag provided but not defined: -if-revision",
			"exit status 1: flag provided but not defined: --if-revision",
			"exit status 1: unknown flag '--if-revision'",
		} {
			err := errors.New(msg)
			if got := classifyConditionalWriteResult(nil, err); !IsConditionalWriteUnsupported(got) {
				t.Fatalf("classify(%q) = %v, want ErrConditionalWriteUnsupported", msg, got)
			}
		}
	})

	t.Run("unknown flag for a DIFFERENT flag must not latch", func(t *testing.T) {
		err := errors.New("exit status 1: unknown flag: --frobnicate")
		got := classifyConditionalWriteResult(nil, err)
		if IsConditionalWriteUnsupported(got) {
			t.Fatalf("classify = %v; a non-if-revision unknown flag must not latch the store incapable", got)
		}
		if IsPreconditionFailed(got) {
			t.Fatalf("classify = %v; unrelated unknown flag misread as precondition", got)
		}
		if got == nil || got.Error() != err.Error() {
			t.Fatalf("classify = %v, want the error surfaced as-is", got)
		}
	})

	t.Run("capable bd usage-echo listing --if-revision must not latch on an unrelated flag error", func(t *testing.T) {
		// A CAPABLE bd, given some other unknown flag, echoes usage that LISTS
		// --if-revision in the flags block. classifyBDExecResult folds the whole
		// stderr into err.Error(), so a floating "contains if-revision" latch would
		// silently degrade every future fenced write on a perfectly capable bd.
		err := errors.New("exit status 1: unknown flag: --reason-code\n" +
			"Usage:\n  bd update [flags]\n\nFlags:\n" +
			"      --if-revision int   apply only when the bead is at this revision\n" +
			"      --json              emit JSON\n")
		got := classifyConditionalWriteResult(nil, err)
		if IsConditionalWriteUnsupported(got) {
			t.Fatalf("classify = %v; a capable bd's usage echo must NOT latch it incapable", got)
		}
		if got == nil || got.Error() != err.Error() {
			t.Fatalf("classify = %v, want the unrelated flag error surfaced as-is", got)
		}
	})

	t.Run("gate refusal from other body code never latches", func(t *testing.T) {
		out := []byte(`{"error":"close authority required","code":"close-authority-required"}`)
		err := errors.New("exit status 1")
		got := classifyConditionalWriteResult(out, err)
		var gre *GateRefusalError
		if !errors.As(got, &gre) {
			t.Fatalf("classify = %v, want *GateRefusalError", got)
		}
		if gre.Code != "close-authority-required" {
			t.Fatalf("GateRefusalError.Code = %q, want %q", gre.Code, "close-authority-required")
		}
		if IsConditionalWriteUnsupported(got) {
			t.Fatal("a policy gate refusal must NOT latch the store incapable")
		}
	})

	t.Run("gate refusal carrying an informational revision is NOT a precondition", func(t *testing.T) {
		// A close-authority refusal may attach the current revision for context.
		// Field-presence keying would misread it as a precondition and spin the
		// CAS-emulation retry loop against a permanent refusal. The machine code
		// must dominate the fields.
		out := []byte(`{"error":"close denied: not lease holder","code":"close-authority","current_revision":7}`)
		err := errors.New("exit status 1")
		got := classifyConditionalWriteResult(out, err)
		if IsPreconditionFailed(got) {
			t.Fatalf("classify = %v; a coded refusal with an informational revision must not read as a precondition", got)
		}
		var gre *GateRefusalError
		if !errors.As(got, &gre) || gre.Code != "close-authority" {
			t.Fatalf("classify = %v, want *GateRefusalError{Code:\"close-authority\"}", got)
		}
	})

	t.Run("ambiguous error outranks a machine code (may have committed)", func(t *testing.T) {
		// A body code must not convert a maybe-committed connection failure into a
		// definitive did-not-commit gate refusal.
		out := []byte(`{"error":"driver: bad connection","code":"storage"}`)
		err := errors.New("exit status 1: driver: bad connection")
		got := classifyConditionalWriteResult(out, err)
		if IsGateRefusal(got) {
			t.Fatalf("classify = %v; an ambiguous (maybe-committed) write must not be reported as a gate refusal", got)
		}
		if got == nil || got.Error() != err.Error() {
			t.Fatalf("classify = %v, want the ambiguous error surfaced as-is", got)
		}
	})

	t.Run("coded gate refusal whose message says 'not found' is NOT swallowed as ErrNotFound", func(t *testing.T) {
		// A policy refusal may mention "not found" in its human text ("lease not
		// found for holder ..."). The machine code must win over the loose
		// not-found substring, or a permanent refusal becomes a silent idempotent
		// success for delete/close callers.
		out := []byte(`{"error":"lease not found for holder agent-7","code":"close-authority-required"}`)
		err := errors.New("exit status 1: lease not found for holder agent-7")
		got := classifyConditionalWriteResult(out, err)
		if errors.Is(got, ErrNotFound) {
			t.Fatalf("classify = %v; a coded gate refusal must not be swallowed as ErrNotFound", got)
		}
		var gre *GateRefusalError
		if !errors.As(got, &gre) || gre.Code != "close-authority-required" {
			t.Fatalf("classify = %v, want *GateRefusalError{Code:\"close-authority-required\"}", got)
		}
	})

	t.Run("code-less not-found maps to ErrNotFound", func(t *testing.T) {
		err := errors.New("exit status 1: no issues found matching the provided IDs")
		if got := classifyConditionalWriteResult(nil, err); !errors.Is(got, ErrNotFound) {
			t.Fatalf("classify = %v, want ErrNotFound", got)
		}
	})

	t.Run("precondition body on stderr wins over an incidental stdout envelope", func(t *testing.T) {
		// bd may split streams: an incidental message-only JSON on stdout, the real
		// coded precondition body folded into err.Error() from stderr. The source
		// carrying a discriminator must win.
		out := []byte(`{"error":"progress: 1 of 2 committed"}`)
		err := errors.New(`exit status 1: {"code":"precondition-failed","expected_revision":3,"current_revision":9}`)
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 3, 9)
	})

	t.Run("precondition body with trailing log noise still parses", func(t *testing.T) {
		out := []byte("{\"code\":\"precondition-failed\",\"expected_revision\":6,\"current_revision\":6}\nWARN: dolt reconnected\n")
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 6, 6)
	})

	t.Run("precondition body behind a bracketed log prefix parses", func(t *testing.T) {
		// extractJSON stops at the first '{' OR '[': a "[WARN]" prefix would make a
		// naive parse reject the object. The multi-object scan must skip it.
		out := []byte("[WARN] dolt reconnect\n{\"code\":\"precondition-failed\",\"expected_revision\":3,\"current_revision\":9}")
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 3, 9)
	})

	t.Run("precondition body after a leading JSON log line parses", func(t *testing.T) {
		// A JSON log line precedes the real envelope; the first-object-only parse
		// would read the log line (no discriminator) and miss the body.
		out := []byte("{\"level\":\"info\",\"msg\":\"connecting\"}\n{\"code\":\"precondition-failed\",\"expected_revision\":4,\"current_revision\":5}")
		err := errors.New("exit status 1")
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 4, 5)
	})

	t.Run("two-source: winning body carries only a code (no revisions)", func(t *testing.T) {
		// The stdout envelope is message-only (no discriminator); the real coded
		// body rides err.Error(). The discriminator-preferring parse must pick it,
		// exercising the Code arm of hasDiscriminator alone.
		out := []byte(`{"error":"progress: 1 of 2 committed"}`)
		err := errors.New(`exit status 1: {"code":"conditional-write-unsupported"}`)
		if got := classifyConditionalWriteResult(out, err); !IsConditionalWriteUnsupported(got) {
			t.Fatalf("classify = %v, want ErrConditionalWriteUnsupported from the err-string body", got)
		}
	})

	t.Run("two-source: winning body carries only revision fields (no code)", func(t *testing.T) {
		// Exercises the revision-field arm of hasDiscriminator alone.
		out := []byte(`{"error":"progress: 1 of 2 committed"}`)
		err := errors.New(`exit status 1: {"expected_revision":10,"current_revision":11}`)
		assertPrecondition(t, classifyConditionalWriteResult(out, err), 10, 11)
	})

	t.Run("ambiguous connection class surfaces as-is", func(t *testing.T) {
		for _, detail := range []string{
			"i/o timeout", "invalid connection", "bad connection",
			"connection reset", "broken pipe", "timed out after 5s", "deadline exceeded",
		} {
			err := fmt.Errorf("exit status 1: %s", detail)
			got := classifyConditionalWriteResult(nil, err)
			if got == nil || got.Error() != err.Error() {
				t.Fatalf("classify(%q) = %v, want the ambiguous error surfaced as-is", detail, got)
			}
			if IsPreconditionFailed(got) || IsConditionalWriteUnsupported(got) {
				t.Fatalf("classify(%q) = %v, ambiguous error misclassified", detail, got)
			}
		}
	})

	t.Run("ambiguous outranks a code-less not-found when both phrases appear", func(t *testing.T) {
		// A maybe-committed connection failure whose text also contains "no issues
		// found" must surface as-is, never as a definitive (idempotent-success)
		// ErrNotFound — the write may have landed.
		err := errors.New("exit status 1: connection reset by peer; no issues found in retry")
		got := classifyConditionalWriteResult(nil, err)
		if errors.Is(got, ErrNotFound) {
			t.Fatalf("classify = %v; an ambiguous (maybe-committed) write must not be reported as not-found", got)
		}
		if got == nil || got.Error() != err.Error() {
			t.Fatalf("classify = %v, want the ambiguous error surfaced as-is", got)
		}
	})

	t.Run("generic error surfaces as-is", func(t *testing.T) {
		err := errors.New("exit status 1: dolt merge conflict on issues")
		got := classifyConditionalWriteResult(nil, err)
		if got == nil || got.Error() != err.Error() {
			t.Fatalf("classify = %v, want the error surfaced as-is", got)
		}
		if IsPreconditionFailed(got) || IsConditionalWriteUnsupported(got) || IsGateRefusal(got) {
			t.Fatalf("classify = %v, generic error misclassified", got)
		}
	})
}

func assertPrecondition(t *testing.T, got error, wantExpected, wantCurrent int64) {
	t.Helper()
	var pfe *PreconditionFailedError
	if !errors.As(got, &pfe) {
		t.Fatalf("classify = %v, want *PreconditionFailedError", got)
	}
	if pfe.Expected != wantExpected {
		t.Fatalf("PreconditionFailedError.Expected = %d, want %d", pfe.Expected, wantExpected)
	}
	if pfe.Current != wantCurrent {
		t.Fatalf("PreconditionFailedError.Current = %d, want %d", pfe.Current, wantCurrent)
	}
}

// TestConditionalWritesCapableProbe covers the lazy four-verb capability probe
// and the runtime unsupported latch that is authoritative over it.
func TestConditionalWritesCapableProbe(t *testing.T) {
	// A --help body advertising --if-revision for the given verb.
	capableHelp := func(verb string) []byte {
		return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n  --if-revision int   apply only at this revision\n")
	}
	incapableHelp := func(verb string) []byte {
		return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n  --json   emit JSON\n")
	}

	t.Run("all four verbs advertise the flag -> capable, probed once", func(t *testing.T) {
		var calls int
		seen := map[string]int{}
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			calls++
			verb := args[0]
			seen[verb]++
			return capableHelp(verb), nil
		})
		ok, err := s.conditionalWritesCapable()
		if err != nil || !ok {
			t.Fatalf("conditionalWritesCapable = (%v, %v), want (true, nil)", ok, err)
		}
		if calls != 4 {
			t.Fatalf("probe ran %d subprocesses, want 4 (one per verb)", calls)
		}
		for _, verb := range []string{"update", "close", "assign", "delete"} {
			if seen[verb] != 1 {
				t.Fatalf("verb %q probed %d times, want 1", verb, seen[verb])
			}
		}
		// Memoized: a second call issues no new subprocesses.
		if ok2, _ := s.conditionalWritesCapable(); !ok2 {
			t.Fatal("second call lost the capable verdict")
		}
		if calls != 4 {
			t.Fatalf("probe re-ran subprocesses on the memoized path: %d calls, want 4", calls)
		}
	})

	t.Run("a later verb missing the flag -> incapable", func(t *testing.T) {
		var calls int
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			calls++
			verb := args[0]
			if verb == "delete" {
				return incapableHelp(verb), nil
			}
			return capableHelp(verb), nil
		})
		ok, err := s.conditionalWritesCapable()
		if err != nil || ok {
			t.Fatalf("conditionalWritesCapable = (%v, %v), want (false, nil)", ok, err)
		}
		if calls != 4 {
			t.Fatalf("probe ran %d subprocesses, want 4 (delete is the 4th)", calls)
		}
		// The incapable verdict must also be memoized: a second call re-probes
		// nothing, or a mid-process bd swap could flip the verdict.
		if ok2, _ := s.conditionalWritesCapable(); ok2 {
			t.Fatal("second call flipped the incapable verdict")
		}
		if calls != 4 {
			t.Fatalf("incapable verdict not memoized: %d calls after a second query, want 4", calls)
		}
	})

	t.Run("first verb missing the flag short-circuits", func(t *testing.T) {
		var calls int
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			calls++
			return incapableHelp(args[0]), nil
		})
		if ok, _ := s.conditionalWritesCapable(); ok {
			t.Fatal("want incapable when the first verb lacks --if-revision")
		}
		if calls != 1 {
			t.Fatalf("probe ran %d subprocesses, want 1 (short-circuit on first miss)", calls)
		}
	})

	t.Run("help subprocess error -> incapable with the failure surfaced", func(t *testing.T) {
		s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
			return nil, errors.New("exec: bd not found")
		})
		if ok, err := s.conditionalWritesCapable(); ok || err == nil || !strings.Contains(err.Error(), "bd not found") {
			t.Fatalf("conditionalWritesCapable = (%v, %v), want (false, the runner error) so a broken bd is never reported as an old bd", ok, err)
		}
		// The verdict is memoized fail-closed; the failure cause is memoized
		// with it (condWriteProbeErr) for every later capability answer.
		if ok, _ := s.conditionalWritesCapable(); ok {
			t.Fatal("memoized incapable verdict lost on the second call")
		}
	})

	t.Run("latch is authoritative over a capable probe", func(t *testing.T) {
		var calls int
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			calls++
			return capableHelp(args[0]), nil
		})
		if ok, _ := s.conditionalWritesCapable(); !ok {
			t.Fatal("precondition: probe should report capable")
		}
		s.markConditionalWritesUnsupported()
		if ok, _ := s.conditionalWritesCapable(); ok {
			t.Fatal("latch must override a capable probe verdict")
		}
	})

	t.Run("latch before first probe returns incapable without probing", func(t *testing.T) {
		var calls int
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			calls++
			return capableHelp(args[0]), nil
		})
		s.markConditionalWritesUnsupported()
		if ok, _ := s.conditionalWritesCapable(); ok {
			t.Fatal("a latched store must report incapable")
		}
		if calls != 0 {
			t.Fatalf("latched store ran %d probe subprocesses, want 0", calls)
		}
	})
}

// --- Phase 5: fenced verbs + retry wrapper + metadata-CAS emulation ---

// scriptedBd is a white-box fake bd backend for the fenced-write tests. It models
// one bead's revision/status/metadata/existence and interprets the argv BdStore
// emits for show/update/close/delete plus the capability probe (--help). Unlike
// bdstore_test.go's fakeRunner (keyed on the exact argv string), it applies
// mutations to backing state BEFORE returning, so a writeHook can express the
// committed-but-ambiguous cell and the re-read-on-transient path (DESIGN §7.4).
type scriptedBd struct {
	mu             sync.Mutex
	id             string
	revision       int64
	status         string
	metadata       map[string]string
	deleted        bool
	getCalls       int
	writeCalls     int
	writeArgv      [][]string
	sawDoltPrefix  bool
	probeIncapable bool
	// writeHook, if non-nil, runs at the start of each write call holding mu. It
	// may mutate backing and, by returning handled=true, short-circuit the
	// default fence-and-apply with a canned (out, err).
	writeHook func(w *scriptedBd, verb string, ifRev int64) (out []byte, err error, handled bool)
}

func (w *scriptedBd) runner(_, _ string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "--dolt-auto-commit" {
		w.mu.Lock()
		w.sawDoltPrefix = true
		w.mu.Unlock()
		args = args[2:]
	}
	if len(args) == 0 {
		return nil, errors.New("scriptedBd: empty argv")
	}
	if len(args) >= 2 && args[1] == "--help" {
		return w.helpOutput(args[0]), nil
	}
	switch args[0] {
	case "show":
		return w.handleShow()
	case "update", "close", "delete":
		return w.handleWrite(args[0], args)
	default:
		return nil, fmt.Errorf("scriptedBd: unhandled verb %q", args[0])
	}
}

func (w *scriptedBd) helpOutput(verb string) []byte {
	if w.probeIncapable {
		return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n  --json   emit JSON\n")
	}
	return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n" +
		"  --if-revision int   apply only at this revision\n  --json\n")
}

func (w *scriptedBd) handleShow() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.getCalls++
	if w.deleted {
		return nil, errors.New("exit status 1: no issues found matching the provided IDs")
	}
	return w.showJSONLocked(), nil
}

func (w *scriptedBd) showJSONLocked() []byte {
	status := w.status
	if status == "" {
		status = "open"
	}
	md, _ := json.Marshal(w.metadata)
	return []byte(fmt.Sprintf(`[{"id":%q,"status":%q,"revision":%d,"metadata":%s}]`,
		w.id, status, w.revision, md))
}

func (w *scriptedBd) handleWrite(verb string, args []string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeCalls++
	w.writeArgv = append(w.writeArgv, append([]string(nil), args...))
	ifRev, hasFence := parseIfRevisionArg(args)
	if w.writeHook != nil {
		if out, err, handled := w.writeHook(w, verb, ifRev); handled {
			return out, err
		}
	}
	if w.deleted {
		return nil, errors.New("exit status 1: no issues found matching the provided IDs")
	}
	if hasFence && ifRev != w.revision {
		return w.preconditionBodyLocked(ifRev), errors.New("exit status 1")
	}
	switch verb {
	case "update":
		w.applySetMetadataLocked(args)
	case "close":
		w.status = "closed"
	case "delete":
		w.deleted = true
	}
	w.revision++
	return []byte(`{"ok":true}`), nil
}

func (w *scriptedBd) preconditionBodyLocked(ifRev int64) []byte {
	return []byte(fmt.Sprintf(
		`{"error":"revision precondition failed","code":%q,"expected_revision":%d,"current_revision":%d}`,
		bdConditionalCodePreconditionFailed, ifRev, w.revision))
}

func (w *scriptedBd) applySetMetadataLocked(args []string) {
	if w.metadata == nil {
		w.metadata = map[string]string{}
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--set-metadata" {
			if eq := strings.IndexByte(args[i+1], '='); eq >= 0 {
				w.metadata[args[i+1][:eq]] = args[i+1][eq+1:]
			}
		}
	}
}

func parseIfRevisionArg(args []string) (int64, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == conditionalWriteFlag {
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// disableConditionalWriteSleep no-ops the backoff seam for the duration of the
// test and restores it after. Per the Phase-5 design rule, tests that touch this
// package-level seam must not run in parallel.
func disableConditionalWriteSleep(t *testing.T) {
	t.Helper()
	prev := conditionalWriteSleep
	conditionalWriteSleep = func(time.Duration) {}
	t.Cleanup(func() { conditionalWriteSleep = prev })
}

func argvContains(argv [][]string, want ...string) bool {
	for _, call := range argv {
		if sliceContainsSeq(call, want...) {
			return true
		}
	}
	return false
}

func sliceContainsSeq(hay []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range hay {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestUpdateIfMatchSuccessAppliesFence(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	s := NewBdStore("/city", w.runner)
	title := "renamed"
	if err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch at current revision: %v", err)
	}
	if w.revision != 2 {
		t.Fatalf("revision not bumped: got %d, want 2", w.revision)
	}
	if !argvContains(w.writeArgv, "update", "--json", "ga-1", "--title", "renamed", conditionalWriteFlag, "1") {
		t.Fatalf("fenced update argv missing expected flags: %v", w.writeArgv)
	}
}

func TestUpdateIfMatchEmptyOptsIsTypedErrorNoWrite(t *testing.T) {
	// Pinned cross-store contract: an empty fenced update is invalid input
	// (ErrEmptyConditionalUpdate) — never a silent nil, never a fence write.
	w := &scriptedBd{id: "ga-1", revision: 1}
	s := NewBdStore("/city", w.runner)
	if err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{}); !errors.Is(err, ErrEmptyConditionalUpdate) {
		t.Fatalf("empty UpdateIfMatch: got %v, want ErrEmptyConditionalUpdate", err)
	}
	if w.writeCalls != 0 {
		t.Fatalf("empty UpdateIfMatch issued %d writes, want 0", w.writeCalls)
	}
}

func TestUpdateIfMatchPreconditionOverridesExpected(t *testing.T) {
	// The bd body carries a deliberately WRONG expected_revision; the verb must
	// override Expected with the caller's own stale argument (the conformance
	// harness asserts this), while Current stays from the body.
	w := &scriptedBd{id: "ga-1", revision: 5}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		return []byte(`{"code":"precondition-failed","expected_revision":4242,"current_revision":5}`),
			errors.New("exit status 1"), true
	}
	s := NewBdStore("/city", w.runner)

	err := s.UpdateIfMatch("ga-1", 3, UpdateOpts{Title: strptr("x")})
	var pfe *PreconditionFailedError
	if !errors.As(err, &pfe) {
		t.Fatalf("UpdateIfMatch stale: got %v, want *PreconditionFailedError", err)
	}
	if pfe.ID != "ga-1" {
		t.Fatalf("PreconditionFailedError.ID = %q, want ga-1", pfe.ID)
	}
	if pfe.Expected != 3 {
		t.Fatalf("PreconditionFailedError.Expected = %d, want 3 (caller's stale revision, not the body's 4242)", pfe.Expected)
	}
	if pfe.Current != 5 {
		t.Fatalf("PreconditionFailedError.Current = %d, want 5 (from the bd body)", pfe.Current)
	}
}

func TestConditionalVerbsIncapableReturnUnsupportedNoWrite(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, probeIncapable: true}
	s := NewBdStore("/city", w.runner)
	if err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: strptr("x")}); !IsConditionalWriteUnsupported(err) {
		t.Fatalf("UpdateIfMatch on incapable bd: got %v, want ErrConditionalWriteUnsupported", err)
	}
	if err := s.CloseIfMatch("ga-1", 1); !IsConditionalWriteUnsupported(err) {
		t.Fatalf("CloseIfMatch on incapable bd: got %v, want ErrConditionalWriteUnsupported", err)
	}
	if err := s.DeleteIfMatch("ga-1", 1); !IsConditionalWriteUnsupported(err) {
		t.Fatalf("DeleteIfMatch on incapable bd: got %v, want ErrConditionalWriteUnsupported", err)
	}
	if w.writeCalls != 0 {
		t.Fatalf("incapable store issued %d fenced writes, want 0 (never fall through to an unconditional write)", w.writeCalls)
	}
}

func TestUpdateIfMatchRuntimeUnsupportedLatches(t *testing.T) {
	// The probe passes, but the write rejects --if-revision at runtime (a bd
	// downgraded under a drifted PATH). The verb must return unsupported AND latch
	// the store so no further fenced write is even attempted.
	w := &scriptedBd{id: "ga-1", revision: 1}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		return nil, errors.New("exit status 1: unknown flag: --if-revision"), true
	}
	s := NewBdStore("/city", w.runner)

	if err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: strptr("x")}); !IsConditionalWriteUnsupported(err) {
		t.Fatalf("runtime unsupported: got %v, want ErrConditionalWriteUnsupported", err)
	}
	if w.writeCalls != 1 {
		t.Fatalf("first UpdateIfMatch issued %d writes, want 1", w.writeCalls)
	}
	// Latched: the second verb short-circuits before any write.
	if err := s.CloseIfMatch("ga-1", 1); !IsConditionalWriteUnsupported(err) {
		t.Fatalf("after latch: got %v, want ErrConditionalWriteUnsupported", err)
	}
	if w.writeCalls != 1 {
		t.Fatalf("latched store attempted another write: writeCalls = %d, want 1", w.writeCalls)
	}
}

func TestCloseIfMatchAndDeleteIfMatchFenceArgvAndApply(t *testing.T) {
	t.Run("close success", func(t *testing.T) {
		w := &scriptedBd{id: "ga-1", revision: 2, status: "open"}
		s := NewBdStore("/city", w.runner)
		if err := s.CloseIfMatch("ga-1", 2); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
		if w.status != "closed" || w.revision != 3 {
			t.Fatalf("close not applied: status=%q revision=%d", w.status, w.revision)
		}
		if !argvContains(w.writeArgv, "close", "--force", "--json", "ga-1", conditionalWriteFlag, "2") {
			t.Fatalf("fenced close argv missing expected flags: %v", w.writeArgv)
		}
	})
	t.Run("delete success", func(t *testing.T) {
		w := &scriptedBd{id: "ga-1", revision: 4, status: "open"}
		s := NewBdStore("/city", w.runner)
		if err := s.DeleteIfMatch("ga-1", 4); err != nil {
			t.Fatalf("DeleteIfMatch: %v", err)
		}
		if !w.deleted {
			t.Fatal("delete not applied")
		}
		if !argvContains(w.writeArgv, "delete", "--force", "--json", "ga-1", conditionalWriteFlag, "4") {
			t.Fatalf("fenced delete argv missing expected flags: %v", w.writeArgv)
		}
	})
	t.Run("close precondition on stale revision", func(t *testing.T) {
		w := &scriptedBd{id: "ga-1", revision: 9, status: "open"}
		s := NewBdStore("/city", w.runner)
		err := s.CloseIfMatch("ga-1", 2)
		var pfe *PreconditionFailedError
		if !errors.As(err, &pfe) || pfe.Expected != 2 {
			t.Fatalf("CloseIfMatch stale: got %v, want *PreconditionFailedError{Expected:2}", err)
		}
		if w.status == "closed" {
			t.Fatal("stale CloseIfMatch closed the bead anyway")
		}
	})
}

func TestConditionalWriteAppliesDoltlitePrefix(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"backend":"doltlite"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	s := NewBdStore(dir, w.runner)
	if err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: strptr("x")}); err != nil {
		t.Fatalf("UpdateIfMatch on doltlite store: %v", err)
	}
	if !w.sawDoltPrefix {
		t.Fatal("fenced write on a doltlite backend did not carry the --dolt-auto-commit off prefix")
	}
}

func TestRunConditionalWriteRetriesSerializationSameRevision(t *testing.T) {
	// A serialization conflict rolled the write back; the re-read shows the
	// revision unchanged, so the retry replays the same fence and succeeds.
	disableConditionalWriteSleep(t)
	var writes int
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		writes++
		if writes == 1 {
			return nil, errors.New("exit status 1: Error 1213 (40001): serialization failure"), true
		}
		return nil, nil, false // fall through to default apply
	}
	s := NewBdStore("/city", w.runner)

	if err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: strptr("x")}); err != nil {
		t.Fatalf("serialization retry: got %v, want nil (retry should succeed)", err)
	}
	if w.writeCalls != 2 {
		t.Fatalf("write attempts = %d, want 2 (one serialization + one success)", w.writeCalls)
	}
	if w.getCalls != 1 {
		t.Fatalf("re-read count = %d, want 1 (revision re-read before retry)", w.getCalls)
	}
}

func TestRunConditionalWriteSerializationReReadMovedIsPreconditionNoReplay(t *testing.T) {
	// The serialization retry re-reads and finds the revision moved (someone else
	// committed). The fence is now permanently stale, so it must surface a
	// precondition WITHOUT replaying the fenced write.
	disableConditionalWriteSleep(t)
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		w.revision = 7 // an interleaving writer advanced the bead
		return nil, errors.New("exit status 1: Error 1213 (40001): serialization failure"), true
	}
	s := NewBdStore("/city", w.runner)

	err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: strptr("x")})
	var pfe *PreconditionFailedError
	if !errors.As(err, &pfe) {
		t.Fatalf("moved-revision serialization: got %v, want *PreconditionFailedError", err)
	}
	if pfe.Expected != 1 || pfe.Current != 7 {
		t.Fatalf("precondition = {Expected:%d, Current:%d}, want {1, 7}", pfe.Expected, pfe.Current)
	}
	if w.writeCalls != 1 {
		t.Fatalf("write attempts = %d, want 1 (a stale fence must never be replayed)", w.writeCalls)
	}
}

func TestRunConditionalWriteAmbiguousSurfacesAsIsNoRetry(t *testing.T) {
	// A committed-but-ambiguous write: the backend applied the change, then the
	// connection dropped. It must surface as-is (never a precondition/unsupported)
	// and never be retried, so the caller's self-win re-read decides.
	disableConditionalWriteSleep(t)
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{}}
	w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		w.metadata["k"] = "committed" // the write landed
		w.revision++
		return nil, errors.New("exit status 1: i/o timeout"), true
	}
	s := NewBdStore("/city", w.runner)

	err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Metadata: map[string]string{"k": "committed"}})
	if err == nil {
		t.Fatal("ambiguous write must surface an error, not nil")
	}
	if IsPreconditionFailed(err) || IsConditionalWriteUnsupported(err) {
		t.Fatalf("ambiguous write misclassified: %v", err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("ambiguous error not surfaced as-is: %v", err)
	}
	if w.writeCalls != 1 {
		t.Fatalf("ambiguous write retried: writeCalls = %d, want 1", w.writeCalls)
	}
	if w.metadata["k"] != "committed" {
		t.Fatalf("backing lost the committed write: %q", w.metadata["k"])
	}
}

func TestCompareAndSetMetadataKeyWin(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	s := NewBdStore("/city", w.runner)
	ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "first")
	if err != nil || !ok {
		t.Fatalf("claim absent key: (%v, %v), want (true, nil)", ok, err)
	}
	if w.metadata["k"] != "first" {
		t.Fatalf("value after CAS = %q, want first", w.metadata["k"])
	}
	if !argvContains(w.writeArgv, "update", "--set-metadata", "k=first", conditionalWriteFlag, "1") {
		t.Fatalf("CAS fenced update argv missing expected flags: %v", w.writeArgv)
	}
}

func TestCompareAndSetMetadataKeyEmptyExpectedClaimsAbsentOrEmptyOnly(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	s := NewBdStore("/city", w.runner)
	if ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "one"); err != nil || !ok {
		t.Fatalf("claim absent: (%v, %v), want (true, nil)", ok, err)
	}
	// Empty-valued key: expected "" still claims it.
	w.mu.Lock()
	w.metadata["k"] = ""
	w.revision++
	w.mu.Unlock()
	if ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "two"); err != nil || !ok {
		t.Fatalf("claim empty-valued: (%v, %v), want (true, nil)", ok, err)
	}
	// Non-empty key: expected "" must NOT claim it.
	if ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "three"); err != nil || ok {
		t.Fatalf("claim non-empty with empty expected: (%v, %v), want (false, nil)", ok, err)
	}
	if w.metadata["k"] != "two" {
		t.Fatalf("value after rejected CAS = %q, want two", w.metadata["k"])
	}
}

func TestCompareAndSetMetadataKeyValueMismatchNoWrite(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{"k": "A"}}
	s := NewBdStore("/city", w.runner)
	ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "B", "C")
	if err != nil {
		t.Fatalf("value-mismatch CAS returned error: %v", err)
	}
	if ok {
		t.Fatal("value-mismatch CAS returned true, want false")
	}
	if w.writeCalls != 0 {
		t.Fatalf("value-mismatch CAS issued %d writes, want 0", w.writeCalls)
	}
	if w.metadata["k"] != "A" {
		t.Fatalf("value mutated on a lost CAS: %q, want A", w.metadata["k"])
	}
}

func TestCompareAndSetMetadataKeyPreconditionRetryThenWin(t *testing.T) {
	// The first fenced write hits a precondition because an unrelated-key write
	// bumped the revision; our key value is untouched, so the retry re-reads and
	// wins.
	disableConditionalWriteSleep(t)
	var writes int
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{"k": "start"}}
	w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		writes++
		if writes == 1 {
			w.revision++ // unrelated-key writer moved the bead; our value stays "start"
			return w.preconditionBodyLocked(1), errors.New("exit status 1"), true
		}
		return nil, nil, false // retry falls through to default apply
	}
	s := NewBdStore("/city", w.runner)

	ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "start", "won")
	if err != nil || !ok {
		t.Fatalf("precondition-retry CAS: (%v, %v), want (true, nil)", ok, err)
	}
	if w.metadata["k"] != "won" {
		t.Fatalf("value after retry win = %q, want won", w.metadata["k"])
	}
	if w.writeCalls != 2 {
		t.Fatalf("CAS write attempts = %d, want 2", w.writeCalls)
	}
}

func TestCompareAndSetMetadataKeyExhaustionIsTypedNotPrecondition(t *testing.T) {
	// Persistent cross-key interference: every fenced write hits a precondition
	// because the revision keeps moving, but our value never mismatches. The loop
	// must exhaust to *CASRetriesExhaustedError — NOT a precondition and NOT
	// (false, nil).
	disableConditionalWriteSleep(t)
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{"k": "start"}}
	w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		w.revision++ // every attempt races a fresh unrelated write
		return w.preconditionBodyLocked(0), errors.New("exit status 1"), true
	}
	s := NewBdStore("/city", w.runner)

	ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "start", "won")
	if ok {
		t.Fatal("exhausted CAS returned true")
	}
	if !IsCASRetriesExhausted(err) {
		t.Fatalf("exhaustion error = %v, want *CASRetriesExhaustedError", err)
	}
	if IsPreconditionFailed(err) {
		t.Fatalf("exhaustion must NOT be a precondition (the value never mismatched): %v", err)
	}
	var cre *CASRetriesExhaustedError
	errors.As(err, &cre)
	if cre.Attempts != casEmulationMaxAttempts {
		t.Fatalf("CASRetriesExhaustedError.Attempts = %d, want %d", cre.Attempts, casEmulationMaxAttempts)
	}
	if w.writeCalls != casEmulationMaxAttempts {
		t.Fatalf("CAS write attempts = %d, want %d", w.writeCalls, casEmulationMaxAttempts)
	}
}

func TestCompareAndSetMetadataKeyIncapable(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, probeIncapable: true}
	s := NewBdStore("/city", w.runner)
	ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "v")
	if ok || !IsConditionalWriteUnsupported(err) {
		t.Fatalf("CAS on incapable bd: (%v, %v), want (false, ErrConditionalWriteUnsupported)", ok, err)
	}
	if w.writeCalls != 0 || w.getCalls != 0 {
		t.Fatalf("incapable CAS touched the store: writes=%d gets=%d, want 0/0", w.writeCalls, w.getCalls)
	}
}

func TestCompareAndSetMetadataKeyRuntimeUnsupportedLatches(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		return nil, errors.New("exit status 1: unknown flag: --if-revision"), true
	}
	s := NewBdStore("/city", w.runner)

	if ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "v"); ok || !IsConditionalWriteUnsupported(err) {
		t.Fatalf("first CAS: (%v, %v), want (false, ErrConditionalWriteUnsupported)", ok, err)
	}
	gets, writes := w.getCalls, w.writeCalls
	// Latched: the second CAS short-circuits before any Get or write.
	if ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "v"); ok || !IsConditionalWriteUnsupported(err) {
		t.Fatalf("second CAS after latch: (%v, %v), want (false, ErrConditionalWriteUnsupported)", ok, err)
	}
	if w.getCalls != gets || w.writeCalls != writes {
		t.Fatalf("latched CAS touched the store again: gets %d->%d, writes %d->%d", gets, w.getCalls, writes, w.writeCalls)
	}
}

func TestCompareAndSetMetadataKeyContention(t *testing.T) {
	// The concurrency leg: 16 racers CAS the same starting value to distinct
	// values. Exactly one wins; the rest observe the winner's value and lose
	// cleanly (false, nil), never error, never a second winner. Run under -race.
	disableConditionalWriteSleep(t)
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{"k": "start"}}
	s := NewBdStore("/city", w.runner)

	const racers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		val := "racer-" + strconv.Itoa(i)
		go func(val string) {
			defer wg.Done()
			<-start
			ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "start", val)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case ok:
				winners = append(winners, val)
			}
		}(val)
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("contention must resolve to true/false, not error: %v", errs)
	}
	if len(winners) != 1 {
		t.Fatalf("exactly one racer must win, got %d: %v", len(winners), winners)
	}
	if w.metadata["k"] != winners[0] {
		t.Fatalf("final value %q does not match the sole winner %q", w.metadata["k"], winners[0])
	}
}

func strptr(s string) *string { return &s }

func TestCompareAndSetMetadataKeyAmbiguousSurfacesAsIs(t *testing.T) {
	// A committed-but-ambiguous fenced write inside the CAS loop must surface
	// as-is — NEVER be retried and NEVER be re-checked as a value loss. Retrying
	// or re-reading would report (false,nil) after our write may have landed,
	// stranding a claim the caller actually won (red-team finding 3).
	disableConditionalWriteSleep(t)
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{"k": "start"}}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		return nil, errors.New("exit status 1: i/o timeout"), true
	}
	s := NewBdStore("/city", w.runner)

	ok, err := s.CompareAndSetMetadataKey("ga-1", "k", "start", "won")
	if ok {
		t.Fatal("ambiguous CAS returned true")
	}
	if err == nil || IsPreconditionFailed(err) {
		t.Fatalf("ambiguous CAS = (%v, %v), want (false, the ambiguous error as-is)", ok, err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("ambiguous CAS error not surfaced as-is: %v", err)
	}
	if w.writeCalls != 1 {
		t.Fatalf("ambiguous CAS retried: writeCalls = %d, want 1", w.writeCalls)
	}
	if w.getCalls != 1 {
		t.Fatalf("ambiguous CAS re-read after the write: getCalls = %d, want 1", w.getCalls)
	}
}

func TestRunConditionalWriteSerializationExhaustionSurfacesRaw(t *testing.T) {
	// Persistent serialization failure with a stable revision must retry exactly
	// conditionalWriteMaxAttempts times and then surface the raw transient error —
	// bounded, and never masked as a precondition or unsupported (red-team
	// finding 4: guards the retry-bound off-by-one / unbounded-loop mutants).
	disableConditionalWriteSleep(t)
	w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		return nil, errors.New("exit status 1: Error 1213 (40001): serialization failure"), true
	}
	s := NewBdStore("/city", w.runner)

	err := s.UpdateIfMatch("ga-1", 1, UpdateOpts{Title: strptr("x")})
	if err == nil {
		t.Fatal("persistent serialization failure returned nil")
	}
	if IsPreconditionFailed(err) || IsConditionalWriteUnsupported(err) {
		t.Fatalf("serialization exhaustion misclassified: %v", err)
	}
	if !strings.Contains(err.Error(), "serialization failure") {
		t.Fatalf("serialization error not surfaced raw: %v", err)
	}
	if w.writeCalls != conditionalWriteMaxAttempts {
		t.Fatalf("serialization write attempts = %d, want %d", w.writeCalls, conditionalWriteMaxAttempts)
	}
	if w.getCalls != conditionalWriteMaxAttempts-1 {
		t.Fatalf("serialization re-reads = %d, want %d (one before each retry)", w.getCalls, conditionalWriteMaxAttempts-1)
	}
}

func TestDeleteIfMatchOnMissingSurfacesNotFound(t *testing.T) {
	// A fenced delete of an already-gone bead surfaces ErrNotFound (idempotent,
	// consistent with unconditional Delete) — not swallowed to nil, not a
	// precondition (red-team finding 5).
	w := &scriptedBd{id: "ga-1", revision: 1, deleted: true}
	s := NewBdStore("/city", w.runner)
	err := s.DeleteIfMatch("ga-1", 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteIfMatch on missing bead: got %v, want ErrNotFound", err)
	}
	if IsPreconditionFailed(err) {
		t.Fatalf("missing-bead delete misread as precondition: %v", err)
	}
}

func TestConditionalWriteGateRefusalStampsIDAndVerb(t *testing.T) {
	// A policy gate refusal (e.g. bd's close-authority guard) must surface as a
	// *GateRefusalError with the ID and refused verb stamped for forensics, and
	// must NOT latch the store (red-team finding 5).
	w := &scriptedBd{id: "ga-1", revision: 2, status: "open"}
	w.writeHook = func(_ *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		return []byte(`{"error":"close authority required","code":"close-authority-required"}`),
			errors.New("exit status 1"), true
	}
	s := NewBdStore("/city", w.runner)

	err := s.CloseIfMatch("ga-1", 2)
	var gre *GateRefusalError
	if !errors.As(err, &gre) {
		t.Fatalf("gate refusal: got %v, want *GateRefusalError", err)
	}
	if gre.ID != "ga-1" || gre.Verb != "close" || gre.Code != "close-authority-required" {
		t.Fatalf("GateRefusalError = {ID:%q, Verb:%q, Code:%q}, want {ga-1, close, close-authority-required}", gre.ID, gre.Verb, gre.Code)
	}
	if IsConditionalWriteUnsupported(err) {
		t.Fatal("a policy gate refusal must NOT latch the store incapable")
	}
	// A second fenced write still runs (store not latched): the probe is memoized
	// but the write is attempted.
	if err := s.CloseIfMatch("ga-1", 2); !errors.As(err, &gre) {
		t.Fatalf("second gate refusal: got %v, want *GateRefusalError (store must not have latched)", err)
	}
	if w.writeCalls != 2 {
		t.Fatalf("gate refusal latched the store: writeCalls = %d, want 2", w.writeCalls)
	}
}

// TestResolveConditionalWriterBdStore covers the seam's prober adapter over
// BdStore: the four-verb --help probe answers Auto/Require capability, and the
// runtime unsupported latch is authoritative over a capable probe verdict.
func TestResolveConditionalWriterBdStore(t *testing.T) {
	capableHelp := func(verb string) []byte {
		return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n  --if-revision int   apply only at this revision\n")
	}
	incapableHelp := func(verb string) []byte {
		return []byte("Usage:\n  bd " + verb + " [flags]\n\nFlags:\n  --json   emit JSON\n")
	}

	t.Run("auto over a capable bd resolves the store as the writer", func(t *testing.T) {
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			return capableHelp(args[0]), nil
		})
		s.stampConditionalWritesMode(gate.Auto, false)
		w, diag, err := ResolveConditionalWriter(s)
		if err != nil || diag != nil {
			t.Fatalf("auto∧capable = diag %v err %v, want nil/nil", diag, err)
		}
		if got, ok := w.(*BdStore); !ok || got != s {
			t.Fatalf("writer = %T, want the resolved *BdStore itself", w)
		}
	})

	t.Run("auto over an incapable bd degrades with the probe reason", func(t *testing.T) {
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			return incapableHelp(args[0]), nil
		})
		s.stampConditionalWritesMode(gate.Auto, false)
		w, diag, err := ResolveConditionalWriter(s)
		if w != nil || err != nil {
			t.Fatalf("auto∧incapable = (%v, _, %v), want (nil, diag, nil)", w, err)
		}
		if diag == nil || diag.PreflightGate != "conditional_writes" || diag.Store != "BdStore" {
			t.Fatalf("diag = %+v, want conditional_writes/BdStore", diag)
		}
		if !strings.Contains(diag.PreflightReason, conditionalWriteFlag) {
			t.Fatalf("PreflightReason = %q, want it to name %s", diag.PreflightReason, conditionalWriteFlag)
		}
	})

	t.Run("require over an incapable bd refuses closed", func(t *testing.T) {
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			return incapableHelp(args[0]), nil
		})
		s.stampConditionalWritesMode(gate.Require, false)
		w, diag, err := ResolveConditionalWriter(s)
		if w != nil || diag == nil {
			t.Fatalf("require∧incapable = (%v, %v, _), want (nil, diag, refusal)", w, diag)
		}
		if !IsConditionalWritesRequired(err) {
			t.Fatalf("err = %v, want *ConditionalWritesRequiredError", err)
		}
		var cre *ConditionalWritesRequiredError
		if !errors.As(err, &cre) || cre.StoreKind != "BdStore" {
			t.Fatalf("StoreKind = %v, want BdStore", err)
		}
	})

	t.Run("runtime latch outranks a capable probe verdict", func(t *testing.T) {
		s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			return capableHelp(args[0]), nil
		})
		s.stampConditionalWritesMode(gate.Auto, false)
		if w, _, _ := ResolveConditionalWriter(s); w == nil {
			t.Fatal("pre-latch resolve should return the writer")
		}
		s.markConditionalWritesUnsupported()
		w, diag, err := ResolveConditionalWriter(s)
		if w != nil || err != nil || diag == nil {
			t.Fatalf("post-latch = (%v, %v, %v), want (nil, diag, nil)", w, diag, err)
		}
		if !strings.Contains(diag.PreflightReason, "latched") {
			t.Fatalf("PreflightReason = %q, want the latch reason, not the stale probe verdict", diag.PreflightReason)
		}
	})
}

// TestResolveConditionalWriterBdStoreProbeFailureReason pins red-team F1: a
// probe SUBPROCESS failure (bd missing/broken) must surface as "capability
// probe failed", never as the lacks---if-revision reason — the two demand
// opposite operator responses (fix the environment vs upgrade bd). The cause
// is memoized, so every later resolve reports it, not just the one that ran
// the probe.
func TestResolveConditionalWriterBdStoreProbeFailureReason(t *testing.T) {
	s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("exec: bd not found")
	})
	s.stampConditionalWritesMode(gate.Require, false)
	for _, call := range []string{"first", "memoized"} {
		_, diag, err := ResolveConditionalWriter(s)
		if diag == nil || !IsConditionalWritesRequired(err) {
			t.Fatalf("%s resolve = (diag %v, err %v), want refusal with diagnostic", call, diag, err)
		}
		if !strings.Contains(diag.PreflightReason, "capability probe failed") ||
			!strings.Contains(diag.PreflightReason, "bd not found") {
			t.Fatalf("%s resolve reason = %q, want the probe-failure cause", call, diag.PreflightReason)
		}
		if strings.Contains(diag.PreflightReason, "lacks") {
			t.Fatalf("%s resolve reason = %q, misreports a broken bd as an old bd", call, diag.PreflightReason)
		}
	}
}

// TestCompareAndSetMetadataKeyExhaustionFinalReadDetectsValueLoss pins the
// review's M9 finding: when the final precondition conflict was caused by a
// competitor landing on the TARGET key, the emulation must report the genuine
// value loss (false, nil) after a final re-read — not exhaustion, which would
// send the caller back into a race it already definitively lost.
func TestCompareAndSetMetadataKeyExhaustionFinalReadDetectsValueLoss(t *testing.T) {
	restoreSleep := conditionalWriteSleep
	conditionalWriteSleep = func(time.Duration) {}
	defer func() { conditionalWriteSleep = restoreSleep }()

	w := &scriptedBd{id: "ga-1", revision: 1, metadata: map[string]string{}}
	attempts := 0
	w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		attempts++
		// Every fenced attempt conflicts (an unrelated writer keeps bumping
		// the revision); before the final re-read, a competitor has landed on
		// the target key itself.
		w.revision++
		if attempts == casEmulationMaxAttempts {
			w.metadata["k"] = "competitor"
		}
		return []byte(`{"error":"revision precondition failed","code":"precondition-failed"}`), errors.New("exit status 1"), true
	}
	s := NewBdStore("/city", w.runner)

	swapped, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "mine")
	if err != nil {
		t.Fatalf("CompareAndSetMetadataKey error = %v, want the (false, nil) value loss", err)
	}
	if swapped {
		t.Fatal("swapped = true under permanent conflict")
	}
}

// TestCompareAndSetMetadataKeyExhaustionWithoutValueLossStaysTyped pins the
// counterpart: pure cross-key interference (the target key never changes)
// still exhausts with the typed transient, never a false (false, nil) loss.
func TestCompareAndSetMetadataKeyExhaustionWithoutValueLossStaysTyped(t *testing.T) {
	restoreSleep := conditionalWriteSleep
	conditionalWriteSleep = func(time.Duration) {}
	defer func() { conditionalWriteSleep = restoreSleep }()

	w := &scriptedBd{id: "ga-1", revision: 1, metadata: map[string]string{}}
	w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
		w.revision++
		return []byte(`{"error":"revision precondition failed","code":"precondition-failed"}`), errors.New("exit status 1"), true
	}
	s := NewBdStore("/city", w.runner)

	swapped, err := s.CompareAndSetMetadataKey("ga-1", "k", "", "mine")
	if swapped || !IsCASRetriesExhausted(err) {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (false, *CASRetriesExhaustedError)", swapped, err)
	}
}
