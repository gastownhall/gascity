package beads

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// v59Bd models the pinned schema-v59 bd the incident cohorts ran: `bd update`
// parses --if-status, no verb parses --if-revision (beads#4682 unlanded).
func v59Bd(metadata map[string]string) *scriptedBd {
	return &scriptedBd{
		id:              "ga-1",
		revision:        41,
		status:          "open",
		metadata:        metadata,
		probeIncapable:  true,
		ifStatusCapable: true,
	}
}

func TestAtomicConditionalCloserForBdStoreFollowsTheLiveBd(t *testing.T) {
	t.Run("status-guard capable bd advertises the capability", func(t *testing.T) {
		w := v59Bd(map[string]string{"state": "suspended"})
		closer, ok := AtomicConditionalCloserFor(NewBdStore("/city", w.runner))
		if !ok || closer == nil {
			t.Fatal("AtomicConditionalCloserFor(BdStore over v59 bd) = unavailable; the unsafe split close stays live")
		}
	})

	t.Run("bd without the status guard stays incapable", func(t *testing.T) {
		w := &scriptedBd{id: "ga-1", revision: 1, status: "open", probeIncapable: true}
		if closer, ok := AtomicConditionalCloserFor(NewBdStore("/city", w.runner)); ok || closer != nil {
			t.Fatalf("AtomicConditionalCloserFor(guardless bd) = (%T, %v), want (nil, false)", closer, ok)
		}
	})

	t.Run("a broken bd degrades to incapable rather than claiming the capability", func(t *testing.T) {
		s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
			return nil, errors.New("exec: \"bd\": executable file not found in $PATH")
		})
		if closer, ok := AtomicConditionalCloserFor(s); ok || closer != nil {
			t.Fatalf("AtomicConditionalCloserFor(broken bd) = (%T, %v), want (nil, false)", closer, ok)
		}
		if _, err := s.CloseWithMetadataIfMatch("ga-1", 1, nil); !IsConditionalWriteUnsupported(err) {
			t.Fatalf("CloseWithMetadataIfMatch on a broken bd = %v, want ErrConditionalWriteUnsupported", err)
		}
	})

	t.Run("the capability probe is memoized", func(t *testing.T) {
		helpCalls := 0
		w := v59Bd(nil)
		s := NewBdStore("/city", func(dir, name string, args ...string) ([]byte, error) {
			if len(args) >= 2 && args[1] == "--help" {
				helpCalls++
			}
			return w.runner(dir, name, args...)
		})
		for range 3 {
			if _, ok := AtomicConditionalCloserFor(s); !ok {
				t.Fatal("capability lost between discoveries")
			}
		}
		if helpCalls != 1 {
			t.Fatalf("capability probe ran %d subprocesses across 3 discoveries, want 1 (memoized)", helpCalls)
		}
	})
}

// TestCloseWithMetadataIfMatchCommitsOneGuardedCommand is the core invariant:
// the terminal metadata and the close must ride ONE bd write, guarded on the
// status this store observed. Two writes is the ga-f7v2ft.78.6 shape.
func TestCloseWithMetadataIfMatchCommitsOneGuardedCommand(t *testing.T) {
	w := v59Bd(map[string]string{"state": "suspended", "keep": "me"})
	s := NewBdStore("/city", w.runner)

	closed, err := s.CloseWithMetadataIfMatch("ga-1", 41, map[string]string{
		"state":        "drained",
		"close_reason": "session drained: pool slot retired by reconciler",
	})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("returned row = status %q state %q, want closed/drained", closed.Status, closed.Metadata["state"])
	}
	if closed.Revision == 0 {
		t.Fatal("returned row carries no revision; the committed row must be re-read, not echoed from bd update")
	}
	if w.writeCalls != 1 {
		t.Fatalf("terminal close issued %d bd writes, want exactly 1: %v", w.writeCalls, w.writeArgv)
	}
	if w.metadata["keep"] != "me" {
		t.Fatalf("sibling metadata key erased: %#v", w.metadata)
	}
	argv := w.writeArgv[0]
	for _, want := range [][]string{
		{"update", "--json", "ga-1"},
		{"--status", "closed"},
		{bdStatusGuardFlag, "open"},
		{"--set-metadata", "state=drained"},
	} {
		if !sliceContainsSeq(argv, want...) {
			t.Fatalf("fused close argv missing %v: %v", want, argv)
		}
	}
	// The pinned bd has no --if-revision; issuing it would fail the whole close.
	for _, a := range argv {
		if a == conditionalWriteFlag {
			t.Fatalf("fused close sent %s to a bd that cannot parse it: %v", conditionalWriteFlag, argv)
		}
	}
}

// TestCloseWithMetadataIfMatchRefusesWhenTheStatusGuardLoses proves a racing
// writer that moves the status is a precondition, not a blind overwrite, and
// that nothing was written.
func TestCloseWithMetadataIfMatchRefusesWhenTheStatusGuardLoses(t *testing.T) {
	w := v59Bd(map[string]string{"state": "suspended"})
	w.writeHook = func(w *scriptedBd, verb string, _ int64) ([]byte, error, bool) {
		if verb == "update" {
			// Another actor advanced the row between our show and bd's
			// in-transaction guard read.
			w.status = "in_progress"
		}
		return nil, nil, false
	}
	s := NewBdStore("/city", w.runner)

	closed, err := s.CloseWithMetadataIfMatch("ga-1", 41, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("guard loss = %v, want *PreconditionFailedError", err)
	}
	if closed.ID != "" {
		t.Fatalf("guard loss returned a bead %#v, want the zero value", closed)
	}
	if w.status == "closed" || w.metadata["state"] == "drained" {
		t.Fatalf("guard loss still mutated the row: status %q metadata %#v", w.status, w.metadata)
	}
	if w.writeCalls != 1 {
		t.Fatalf("guard loss replayed the write %d times; a lost fence is a signal, not a transient", w.writeCalls)
	}
	var pfe *PreconditionFailedError
	if errors.As(err, &pfe); pfe.ID != "ga-1" || pfe.Expected != 41 {
		t.Fatalf("precondition = %#v, want ID ga-1 Expected 41", pfe)
	}
}

// TestCloseWithMetadataIfMatchHonorsARealRevisionToken keeps the caller's fence
// meaningful whenever both sides carry one.
func TestCloseWithMetadataIfMatchHonorsARealRevisionToken(t *testing.T) {
	w := v59Bd(map[string]string{"state": "suspended"})
	s := NewBdStore("/city", w.runner)

	_, err := s.CloseWithMetadataIfMatch("ga-1", 7, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("stale revision = %v, want *PreconditionFailedError", err)
	}
	if w.writeCalls != 0 {
		t.Fatalf("stale revision issued %d writes, want 0", w.writeCalls)
	}
	var pfe *PreconditionFailedError
	errors.As(err, &pfe)
	if pfe.Expected != 7 || pfe.Current != 41 {
		t.Fatalf("precondition Expected/Current = %d/%d, want 7/41", pfe.Expected, pfe.Current)
	}
}

// TestCloseWithMetadataIfMatchAcceptsAnAbsentRevisionToken is the CachingStore
// case: only `bd show` projects bd's row_lock as `revision`, so a bead primed
// from `bd list` carries 0. Refusing it would wedge every session close on a
// bd-backed city; the status CAS is the durable fence either way.
func TestCloseWithMetadataIfMatchAcceptsAnAbsentRevisionToken(t *testing.T) {
	w := v59Bd(map[string]string{"state": "awake"})
	s := NewBdStore("/city", w.runner)

	closed, err := s.CloseWithMetadataIfMatch("ga-1", 0, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch with no caller revision: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("row = status %q state %q, want closed/drained", closed.Status, closed.Metadata["state"])
	}
	if !sliceContainsSeq(w.writeArgv[0], bdStatusGuardFlag, "open") {
		t.Fatalf("close without a revision token dropped the status fence: %v", w.writeArgv[0])
	}
}

// TestCloseWithMetadataIfMatchYieldsToAWinningCloser proves an already-closed
// row is reported as a lost fence — never re-closed, which would rewrite
// closed_at and claim a close this call did not perform.
func TestCloseWithMetadataIfMatchYieldsToAWinningCloser(t *testing.T) {
	w := v59Bd(map[string]string{"state": "drained"})
	w.status = "closed"
	s := NewBdStore("/city", w.runner)

	if _, err := s.CloseWithMetadataIfMatch("ga-1", 41, map[string]string{"state": "drained"}); !IsPreconditionFailed(err) {
		t.Fatalf("already-closed row = %v, want *PreconditionFailedError", err)
	}
	if w.writeCalls != 0 {
		t.Fatalf("already-closed row took %d writes, want 0", w.writeCalls)
	}
}

// TestCloseWithMetadataIfMatchRefusesAnUnclosedResult ports the unconditional
// close's gastownhall/beads#3948 honesty guard: bd can exit 0 and still leave
// the row open when an import-revert race rolls the write back.
func TestCloseWithMetadataIfMatchRefusesAnUnclosedResult(t *testing.T) {
	w := v59Bd(map[string]string{"state": "suspended"})
	w.writeHook = func(w *scriptedBd, verb string, _ int64) ([]byte, error, bool) {
		if verb == "update" {
			w.metadata["state"] = "drained"
			w.revision++
			return []byte(`[{"id":"ga-1","status":"closed"}]`), nil, true
		}
		return nil, nil, false
	}
	s := NewBdStore("/city", w.runner)

	_, err := s.CloseWithMetadataIfMatch("ga-1", 41, map[string]string{"state": "drained"})
	if err == nil || !strings.Contains(err.Error(), "not closed") {
		t.Fatalf("unclosed result = %v, want an import-revert refusal", err)
	}
}

func TestCloseWithMetadataIfMatchMissingBeadIsNotFound(t *testing.T) {
	w := v59Bd(nil)
	w.deleted = true
	s := NewBdStore("/city", w.runner)

	if _, err := s.CloseWithMetadataIfMatch("ga-1", 41, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing bead = %v, want ErrNotFound", err)
	}
}

// TestIsBdConditionalPreconditionRecognizesGuardMismatch pins the classifier
// input the fused close depends on: bd's exit-13 --if-status refusal must read
// as a precondition, not as an opaque failure the caller cannot recover from.
func TestIsBdConditionalPreconditionRecognizesGuardMismatch(t *testing.T) {
	for _, msg := range []string{
		`exit status 13: Error updating ga-1: status mismatch: ga-1 has status "closed", expected "open"`,
		`exit status 13: {"error":"1 of 1 issues failed to update","failed":[{"id":"ga-1","error":"updating issue: status mismatch","guard_mismatch":true}],"schema_version":1}`,
		`exit status 13: Error updating ga-1: assignee mismatch: ga-1 is held by "other"`,
	} {
		if got := classifyConditionalWriteResult(nil, errors.New(msg)); !IsPreconditionFailed(got) {
			t.Fatalf("classify(%q) = %v, want *PreconditionFailedError", msg, got)
		}
	}
}

// TestCachingStoreForwardsBdAtomicTerminalClose proves the production sandwich
// (CachingStore over BdStore) surfaces the capability and preserves the cache's
// evict-and-notify rules — the shape a real city runs.
func TestCachingStoreForwardsBdAtomicTerminalClose(t *testing.T) {
	w := v59Bd(map[string]string{"state": "suspended"})
	var events []string
	cache := NewCachingStoreForTest(NewBdStore("/city", w.runner), func(eventType, _ string, _ json.RawMessage) {
		events = append(events, eventType)
	})
	closer, ok := AtomicConditionalCloserFor(cache)
	if !ok {
		t.Fatal("CachingStore over a v59 BdStore does not forward the atomic terminal close")
	}
	closed, err := closer.CloseWithMetadataIfMatch("ga-1", 41, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("cached fused close: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("cached fused close row = %q/%q", closed.Status, closed.Metadata["state"])
	}
	if w.writeCalls != 1 {
		t.Fatalf("cached fused close issued %d bd writes, want 1: %v", w.writeCalls, w.writeArgv)
	}
	if len(events) != 1 || events[0] != "bead.closed" {
		t.Fatalf("cached fused close notified %v, want one bead.closed", events)
	}
}
