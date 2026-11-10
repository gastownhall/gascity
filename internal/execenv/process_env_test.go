package execenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	projectionProbe = "GC_EXECENV_PROJECTION_PROBE"
	promptly        = time.Second
)

// enterWindow opens a projection window for the test and registers its exit as
// cleanup, so a failed assertion never leaves the process-wide gate held for
// the next test.
func enterWindow(t *testing.T) (exit func()) {
	t.Helper()
	exit, err := EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatalf("EnterProjectionWindow: %v", err)
	}
	t.Cleanup(exit)
	return exit
}

// within runs fn and fails the test if it has not returned inside promptly.
// A reader that waits for a window to end would hang the test binary instead,
// so the wait is bounded here and the stuck goroutine is left to the cleanup.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(promptly):
		t.Fatalf("%s did not return within %v while a projection window was held", what, promptly)
	}
}

// assertGateIdle fails unless no window is held: the sequence is even and the
// token is back in the channel.
func assertGateIdle(t *testing.T) {
	t.Helper()
	if seq := projectionSeq.Load(); seq%2 != 0 {
		t.Fatalf("projection sequence = %d, want even (no window held)", seq)
	}
	if n := len(projectionToken); n != 1 {
		t.Fatalf("projection token channel holds %d tokens, want 1 (no window held)", n)
	}
}

func TestReadersReturnPromptlyWithTheAmbientValueDuringAProjection(t *testing.T) {
	t.Setenv(projectionProbe, "ambient")
	const projectedOnly = "GC_EXECENV_PROJECTION_ADDED"
	if _, ok := os.LookupEnv(projectedOnly); ok {
		t.Fatalf("%s is set in the test environment", projectedOnly)
	}

	exit := enterWindow(t)
	if err := os.Setenv(projectionProbe, "projected"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv(projectedOnly, "projected"); err != nil {
		t.Fatal(err)
	}

	check := func(phase string) {
		t.Helper()
		var env []string
		var value, getenv string
		var present, addedPresent bool
		within(t, "Environ", func() { env = Environ() })
		within(t, "LookupEnv", func() {
			value, present = LookupEnv(projectionProbe)
			_, addedPresent = LookupEnv(projectedOnly)
		})
		within(t, "Getenv", func() { getenv = Getenv(projectionProbe) })
		if !slices.Contains(env, projectionProbe+"=ambient") || slices.Contains(env, projectionProbe+"=projected") {
			t.Fatalf("%s: Environ carries the probe as %v, want the ambient value only", phase, probeEntries(env, projectionProbe))
		}
		if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, projectedOnly+"=") }) {
			t.Fatalf("%s: Environ shows %s, which only the window added", phase, projectedOnly)
		}
		if !present || value != "ambient" {
			t.Fatalf("%s: LookupEnv(probe) = %q, %v; want %q, true", phase, value, present, "ambient")
		}
		if addedPresent {
			t.Fatalf("%s: LookupEnv shows %s, which only the window added", phase, projectedOnly)
		}
		if getenv != "ambient" {
			t.Fatalf("%s: Getenv(probe) = %q, want %q", phase, getenv, "ambient")
		}
	}

	check("inside the window")

	if err := os.Setenv(projectionProbe, "ambient"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(projectedOnly); err != nil {
		t.Fatal(err)
	}
	check("restored, before exit")

	exit()
	check("after exit")
}

func probeEntries(env []string, key string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		return !strings.HasPrefix(entry, key+"=")
	})
}

func TestEnvironReturnsAFreshSliceInAndOutOfAWindow(t *testing.T) {
	t.Setenv(projectionProbe, "ambient")
	inWindow := func() {
		first := Environ()
		for i := range first {
			first[i] = "scribbled"
		}
		second := Environ()
		if !slices.Contains(second, projectionProbe+"=ambient") {
			t.Fatalf("a caller that rewrote its Environ slice changed the next reader's: %v", probeEntries(second, projectionProbe))
		}
	}
	inWindow()
	enterWindow(t)
	inWindow()
}

func TestLookupSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		snapshot []string
		key      string
		want     string
		wantOK   bool
	}{
		{"present", []string{"A=1", "B=2"}, "B", "2", true},
		{"absent", []string{"A=1"}, "B", "", false},
		{"first duplicate wins", []string{"A=first", "B=2", "A=second"}, "A", "first", true},
		{"entry without equals is ignored", []string{"NOEQUALS", "A=1"}, "NOEQUALS", "", false},
		{"entry without equals does not shadow a later entry", []string{"A", "A=1"}, "A", "1", true},
		{"empty key is absent", []string{"=value", "A=1"}, "", "", false},
		{"empty key is absent with no entries", nil, "", "", false},
		{"key containing equals never matches", []string{"A=B=C", "A=1"}, "A=B", "", false},
		{"value keeps later equals", []string{"A=B=C"}, "A", "B=C", true},
		{"key that is the prefix of another", []string{"GC_EXECENV_LONGER=long", "GC_EXECENV=short"}, "GC_EXECENV", "short", true},
		{"prefix of an entry is not a match", []string{"GC_EXECENV_LONGER=long"}, "GC_EXECENV", "", false},
		{"set but empty", []string{"A="}, "A", "", true},
		{"unset", []string{"B="}, "A", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := lookupSnapshot(tc.snapshot, tc.key)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("lookupSnapshot(%q, %q) = %q, %v; want %q, %v", tc.snapshot, tc.key, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLookupSnapshotNamesFollowTheHostsCase(t *testing.T) {
	t.Parallel()
	got, ok := lookupSnapshot([]string{"Gc_Execenv_Case=value"}, "GC_EXECENV_CASE")
	if want := runtime.GOOS == "windows"; ok != want || (ok && got != "value") {
		t.Fatalf("lookupSnapshot with a differently cased name = %q, %v; want found = %v on %s", got, ok, want, runtime.GOOS)
	}
}

// TestLookupSnapshotAgreesWithOSLookupEnv holds the odd-path lookup to the
// reader it stands in for, over the shapes os.Setenv can create.
func TestLookupSnapshotAgreesWithOSLookupEnv(t *testing.T) {
	t.Setenv("GC_EXECENV_AGREE", "short")
	t.Setenv("GC_EXECENV_AGREE_LONGER", "long")
	t.Setenv("GC_EXECENV_AGREE_EMPTY", "")
	t.Setenv("GC_EXECENV_AGREE_EQUALS", "a=b=c")
	if err := os.Unsetenv("GC_EXECENV_AGREE_UNSET"); err != nil {
		t.Fatal(err)
	}
	snapshot := os.Environ()
	for _, key := range []string{
		"GC_EXECENV_AGREE", "GC_EXECENV_AGREE_LONGER", "GC_EXECENV_AGREE_EMPTY",
		"GC_EXECENV_AGREE_EQUALS", "GC_EXECENV_AGREE_UNSET", "GC_EXECENV_AGREE_", "GC_EXECENV", "",
		"GC_EXECENV_AGREE=short", "PATH", "HOME",
	} {
		wantValue, wantOK := os.LookupEnv(key)
		gotValue, gotOK := lookupSnapshot(snapshot, key)
		if gotValue != wantValue || gotOK != wantOK {
			t.Errorf("lookupSnapshot(os.Environ(), %q) = %q, %v; os.LookupEnv = %q, %v", key, gotValue, gotOK, wantValue, wantOK)
		}
	}
}

func TestLookupEnvInAWindowKeepsWhatTheWindowWithheldAndAdded(t *testing.T) {
	const withheld, added = "GC_EXECENV_WITHHELD", "GC_EXECENV_ADDED"
	t.Setenv(withheld, "ambient")
	t.Setenv("GC_EXECENV_EMPTY", "")
	if err := os.Unsetenv(added); err != nil {
		t.Fatal(err)
	}

	exit := enterWindow(t)
	if err := os.Unsetenv(withheld); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GC_EXECENV_EMPTY"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv(added, "projected"); err != nil {
		t.Fatal(err)
	}

	if value, ok := LookupEnv(withheld); !ok || value != "ambient" {
		t.Errorf("LookupEnv(%s) = %q, %v inside the window that withheld it; want %q, true", withheld, value, ok, "ambient")
	}
	if value, ok := LookupEnv("GC_EXECENV_EMPTY"); !ok || value != "" {
		t.Errorf("LookupEnv(GC_EXECENV_EMPTY) = %q, %v inside the window that unset it; want an empty, present value", value, ok)
	}
	if value, ok := LookupEnv(added); ok {
		t.Errorf("LookupEnv(%s) = %q, %v inside the window that added it; want absent", added, value, ok)
	}
	if got := Getenv(added); got != "" {
		t.Errorf("Getenv(%s) = %q inside the window that added it; want empty", added, got)
	}
	_ = os.Unsetenv(added)
	_ = os.Setenv(withheld, "ambient")
	_ = os.Setenv("GC_EXECENV_EMPTY", "")
	exit()
}

func TestEnterProjectionWindowWithAnExpiredContextTouchesNothing(t *testing.T) {
	assertGateIdle(t)
	seq := projectionSeq.Load()
	snapshot := cleanEnvSnapshot.Load()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exit, err := EnterProjectionWindow(ctx)
	if err == nil {
		exit()
		t.Fatal("EnterProjectionWindow with a canceled context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one that wraps context.Canceled", err)
	}
	if exit != nil {
		t.Errorf("a failed EnterProjectionWindow returned a non-nil exit")
	}
	assertGateIdle(t)
	if got := projectionSeq.Load(); got != seq {
		t.Errorf("projection sequence moved from %d to %d on a failed Enter", seq, got)
	}
	if cleanEnvSnapshot.Load() != snapshot {
		t.Error("a failed Enter replaced the clean snapshot")
	}
}

func TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiredAsItWasTaken(t *testing.T) {
	// select picks at random when both cases are ready, so an expired context
	// can win the token; the gate must hand it back and fail. Repeat so the
	// token-won branch is taken however the runtime chooses.
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if exit, err := EnterProjectionWindow(ctx); err == nil {
			exit()
			t.Fatalf("attempt %d: EnterProjectionWindow with a canceled context succeeded", i)
		}
		assertGateIdle(t)
	}
}

func TestEnterProjectionWindowReturnsTheContextErrorWhileAnotherWindowIsHeld(t *testing.T) {
	exit := enterWindow(t)
	seq := projectionSeq.Load()
	snapshot := cleanEnvSnapshot.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	second, err := EnterProjectionWindow(ctx)
	if err == nil {
		second()
		t.Fatal("a second window was granted while the first was held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one that wraps context.DeadlineExceeded", err)
	}
	if waited := time.Since(start); waited > promptly {
		t.Errorf("the failed Enter took %v, want about the context's 20ms", waited)
	}
	if got := projectionSeq.Load(); got != seq || got%2 != 1 {
		t.Errorf("projection sequence = %d after the failed Enter, want the held window's %d", got, seq)
	}
	if cleanEnvSnapshot.Load() != snapshot {
		t.Error("the failed Enter replaced the held window's clean snapshot")
	}

	exit()
	assertGateIdle(t)
}

func TestSecondWindowRunsOnceTheFirstExits(t *testing.T) {
	exit := enterWindow(t)

	type result struct {
		exit func()
		err  error
	}
	got := make(chan result, 1)
	go func() {
		second, err := EnterProjectionWindow(context.Background())
		got <- result{second, err}
	}()
	select {
	case r := <-got:
		if r.exit != nil {
			r.exit()
		}
		t.Fatal("the second window was granted while the first was held")
	case <-time.After(50 * time.Millisecond):
	}

	exit()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("second window after the first exited: %v", r.err)
		}
		if seq := projectionSeq.Load(); seq%2 != 1 {
			t.Errorf("projection sequence = %d while the second window is held, want odd", seq)
		}
		r.exit()
	case <-time.After(5 * time.Second):
		t.Fatal("the second window was not granted after the first exited")
	}
	assertGateIdle(t)
}

func TestProjectionExitIsIdempotent(t *testing.T) {
	exit, err := EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seq := projectionSeq.Load()
	exit()
	after := projectionSeq.Load()
	if after != seq+1 {
		t.Fatalf("exit moved the sequence from %d to %d, want one bump", seq, after)
	}
	assertGateIdle(t)

	next, err := EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A second exit from the first window must not release the second one's
	// token or end its odd phase.
	exit()
	if got := projectionSeq.Load(); got%2 != 1 {
		t.Errorf("a repeated exit ended the next window: sequence = %d", got)
	}
	if n := len(projectionToken); n != 0 {
		t.Errorf("a repeated exit released the next window's token: %d in the channel", n)
	}
	next()
	assertGateIdle(t)
}

func TestProjectionTokenIsReleasedWhenTheWindowBodyPanics(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the window body's panic did not propagate")
			}
		}()
		exit, err := EnterProjectionWindow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer exit()
		panic("window body failed")
	}()
	assertGateIdle(t)

	exit, err := EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatalf("a window after a panicked one: %v", err)
	}
	exit()
}

func TestProjectionWindowsAreNotReentrant(t *testing.T) {
	enterWindow(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if exit, err := EnterProjectionWindow(ctx); err == nil {
		exit()
		t.Fatal("a nested window on the holder's goroutine was granted")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one that wraps context.DeadlineExceeded", err)
	}
}

// TestWindowChurnNeverLeaksAProjectionToReaders runs windows back to back, each
// withholding the ambient sentinel and projecting its own, against many readers.
// The assertions are on values, so a leak fails without the race detector.
func TestWindowChurnNeverLeaksAProjectionToReaders(t *testing.T) {
	const (
		ambientKey   = "GC_EXECENV_CHURN_AMBIENT"
		projectedFmt = "GC_EXECENV_CHURN_PROJECTED_%d"
		windows      = 300
		readers      = 8
	)
	t.Setenv(ambientKey, "ambient")
	assertGateIdle(t)

	stop := make(chan struct{})
	var failed atomic.Pointer[string]
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		failed.CompareAndSwap(nil, &msg)
	}
	var wg sync.WaitGroup
	var reads atomic.Int64
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				env := Environ()
				if n := countEntries(env, ambientKey+"=ambient"); n != 1 {
					fail("Environ carried the ambient sentinel %d times, want once", n)
					return
				}
				if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "GC_EXECENV_CHURN_PROJECTED_") }) {
					fail("Environ showed a projected sentinel")
					return
				}
				if value, ok := LookupEnv(ambientKey); !ok || value != "ambient" {
					fail("LookupEnv(ambient) = %q, %v", value, ok)
					return
				}
				if value := Getenv(ambientKey); value != "ambient" {
					fail("Getenv(ambient) = %q", value)
					return
				}
				for i := 0; i < windows; i += 97 {
					if _, ok := LookupEnv(fmt.Sprintf(projectedFmt, i)); ok {
						fail("LookupEnv saw projected sentinel %d", i)
						return
					}
				}
				reads.Add(1)
			}
		}()
	}

	for i := 0; i < windows && failed.Load() == nil; i++ {
		exit, err := EnterProjectionWindow(context.Background())
		if err != nil {
			t.Fatalf("window %d: %v", i, err)
		}
		projected := fmt.Sprintf(projectedFmt, i)
		_ = os.Unsetenv(ambientKey)
		_ = os.Setenv(projected, "projected")
		if i%16 == 0 {
			time.Sleep(time.Millisecond)
		}
		_ = os.Unsetenv(projected)
		_ = os.Setenv(ambientKey, "ambient")
		exit()
	}
	close(stop)
	wg.Wait()

	if msg := failed.Load(); msg != nil {
		t.Fatal(*msg)
	}
	if reads.Load() == 0 {
		t.Fatal("no reader completed a pass")
	}
	assertGateIdle(t)
}

func countEntries(env []string, entry string) int {
	n := 0
	for _, e := range env {
		if e == entry {
			n++
		}
	}
	return n
}
