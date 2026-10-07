package beads

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// lockedBuffer is a bytes.Buffer safe to write from the guard goroutine and read
// from the test's.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDefaultLogger points slog.Default() at a buffer for the rest of the
// test. It is the logger every site falls back to when it was handed none,
// which is exactly the path under test, so the test drives it rather than a
// seam beside it.
//
// The cleanup restores the log package too (round3 review, inertness).
// slog.SetDefault with a non-default handler also points log's output at that
// handler and zeroes log's flags, and setting the previous default back does
// NOT undo either — so every later log.Print and slog.Default() line in this
// test binary went into this test's buffer, which nobody reads.
func captureDefaultLogger(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	previous := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return buf
}

// TestCaptureDefaultLoggerRestoresTheLogPackage pins the cleanup above: once a
// test that captured the default logger is over, the log package writes where
// it wrote before, with the flags it had.
func TestCaptureDefaultLoggerRestoresTheLogPackage(t *testing.T) {
	writer, flags := log.Writer(), log.Flags()
	t.Run("capture", func(t *testing.T) {
		buf := captureDefaultLogger(t)
		slog.Default().Warn("captured")
		if !strings.Contains(buf.String(), "captured") {
			t.Fatalf("the capture caught nothing: %q", buf.String())
		}
	})
	if log.Writer() != writer {
		t.Errorf("after the capturing test, log writes to %T, not the %T it wrote to before: every later log line "+
			"in this binary lands in a dead test's buffer", log.Writer(), writer)
	}
	if log.Flags() != flags {
		t.Errorf("after the capturing test, log.Flags() = %d, want %d", log.Flags(), flags)
	}
}

// assertHeadMovedLine requires one WARN line in the lane's single incident
// wording, from site, carrying the hash an operator needs.
func assertHeadMovedLine(t *testing.T, logged, site, hash string) {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, "msg="+ProxiedHeadMovedMessage) {
			continue
		}
		if !strings.Contains(line, "level=WARN") {
			t.Fatalf("the head_moved line is not at WARN: %s", line)
		}
		if !strings.Contains(line, `site="`+site+`"`) && !strings.Contains(line, "site="+site+" ") {
			continue
		}
		if !strings.Contains(line, hash) {
			t.Fatalf("the head_moved line from %s omits the hash %s: %s", site, hash, line)
		}
		return
	}
	t.Fatalf("no %s line from site %q was logged:\n%s", ProxiedHeadMovedMessage, site, logged)
}

// TestProxiedHeadMovedIsLoudAtEverySite is council pr2 E-S2.
//
// head_moved is detection after the fact, so a site that meets it and says
// nothing has thrown the detection away. It reaches exactly one of three
// consumers, depending on which library open produced it, and before this each
// had its own shape of silence: the factory logged only through a caller's
// Logger (the controller's rig stores pass none), the read path turned it into
// one caller's error and a quiet stand-down, and the guard's recovery turned it
// into Undecided while doctor kept reporting the older stand-down reason.
func TestProxiedHeadMovedIsLoudAtEverySite(t *testing.T) {
	moved := func() error {
		return NewProxiedVerdictError(ProxiedVerdictHeadMoved,
			`opening the linked library against database "beads" moved HEAD from before0000 to after11111`, nil)
	}

	t.Run("the factory, for an open that passed no logger (the controller's rig stores)", func(t *testing.T) {
		t.Setenv(nativeForceFallbackEnv, "")
		t.Setenv(proxiedNativeEnv, "1")
		logged := captureDefaultLogger(t)
		scope := proxiedScopeFixture(t)
		fallback := NewMemStore()

		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:        scope,
			Provider:         "bd",
			LongLived:        true,
			PreflightChecker: refusingPreflightChecker(t),
			OpenBdStore:      func() (Store, error) { return fallback, nil },
			OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
				return nil, proxiedOpenReportFixture(), moved()
			},
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		if result.Store != Store(fallback) {
			t.Fatalf("store = %T, want the bd fallback", result.Store)
		}
		assertHeadMovedLine(t, logged.String(), ProxiedIncidentSiteOpen, "after11111")
	})

	t.Run("the read path's reopen", func(t *testing.T) {
		logged := captureDefaultLogger(t)
		admitted := newAdmissionFixture(t, "-1")
		pin, err := Admit(context.Background(), AdmissionInput{
			ScopeRoot:    admitted.scopeRoot,
			Database:     "beads",
			ProcessTable: admitted.processTable(),
			Probe: func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
				return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
			},
			Observed:  NewGenerationSet(),
			Recovered: NewGenerationSet(),
			SkipMemo:  true,
		})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
		native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly(),
			WithNativeReopen(func(context.Context) (NativeStorage, error) { return nil, moved() }))
		native.idPrefix = "prx"
		bdLeaf := newNativeDoltStoreForTest(storage)
		bdLeaf.idPrefix = "prx"
		store, err := NewProxiedStore(native, bdLeaf, pin)
		if err != nil {
			t.Fatalf("NewProxiedStore: %v", err)
		}
		t.Cleanup(func() { _ = store.CloseStore() })

		// The next read must reconnect, and the reconnect's library open is
		// the one that met a moved HEAD.
		if !native.markPoolStale() {
			t.Fatal("markPoolStale refused a handle with a reopen hook")
		}
		_, err = store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictHeadMoved {
			t.Fatalf("List err = %v, want the head_moved verdict", err)
		}
		if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictHeadMoved {
			t.Fatalf("store verdict = %v, want head_moved", verdict)
		}
		assertHeadMovedLine(t, logged.String(), ProxiedIncidentSiteReadReopen, "after11111")
	})

	t.Run("the guard's recovery, and doctor's account of it", func(t *testing.T) {
		logged := captureDefaultLogger(t)
		f := newGuardFixture(t)
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) { return nil, Pin{}, moved() }
		f.start()
		f.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))

		if step := f.tick(); step != proxiedGuardUndecided {
			t.Fatalf("a recovery that met head_moved reported %s, want undecided (the verdict is non-terminal)", step)
		}
		assertHeadMovedLine(t, logged.String(), ProxiedIncidentSiteGuardRecovery, "after11111")
		verdict := f.store.Verdict()
		if verdict == nil || verdict.Verdict != ProxiedVerdictHeadMoved {
			t.Fatalf("store verdict = %v, want head_moved: doctor would report the stand-down's older "+
				"reason for a handle whose recoveries keep moving HEAD", verdict)
		}
		if !f.store.Demoted() {
			t.Fatal("the store promoted itself on a recovery that moved HEAD")
		}
	})

	t.Run("a recovery's newer reason never overwrites a terminal latch", func(t *testing.T) {
		f := newGuardFixture(t)
		f.store.standDown(NewSchemaSkewVerdictError(ProxiedSkewLaneMain, ProxiedSkewDirAhead, "migrated"))
		f.store.noteDemotedVerdict(NewNonTerminalProxiedVerdictError(ProxiedVerdictHeadMoved, "later", nil))
		if verdict := f.store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictSchemaSkew {
			t.Fatalf("store verdict = %v, want the terminal schema_skew kept", verdict)
		}
	})
}
