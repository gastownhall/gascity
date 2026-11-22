package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"

	"github.com/gastownhall/gascity/internal/execenv"
)

// These tests pin the native open window's contract with the rest of the
// process: a window holder projects scope values into the process env behind
// execenv's gate, every reader outside the window answers at once with the
// ambient value, and a second window waits only as long as its own ctx lets it.

// holdProjectionWindow enters an execenv projection window the way a native
// open does, withholds every ambient variable under prefixes, projects env
// through the scoped open projection, and returns the func that ends the window.
// The test's cleanup ends it too, so a test that fails inside it does not leave
// it standing.
func holdProjectionWindow(t *testing.T, prefixes []string, env map[string]string) (end func()) {
	t.Helper()
	exit, err := execenv.EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatalf("EnterProjectionWindow: %v", err)
	}
	var restores []func()
	unwind := func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
		exit()
	}
	if len(prefixes) > 0 {
		restore, err := withWithheldPrefixesLocked(prefixes...)
		if err != nil {
			unwind()
			t.Fatalf("withholding the window's namespaces: %v", err)
		}
		restores = append(restores, restore)
	}
	if env != nil {
		restore, err := withNativeDoltOpenEnvAndCredentialCommandLocked(env, "")
		if err != nil {
			unwind()
			t.Fatalf("projecting the window's env: %v", err)
		}
		restores = append(restores, restore)
	}
	var once sync.Once
	end = func() { once.Do(unwind) }
	t.Cleanup(end)
	return end
}

// openSeam stands in for nativeDoltOpenBestAvailable. Its first call parks
// inside the open window until release; every later call returns a healthy
// storage at once.
type openSeam struct {
	calls     atomic.Int32
	entered   chan struct{}
	released  chan struct{}
	enteredAt sync.Once
	releaseAt sync.Once
	// seenHost is BEADS_DOLT_SERVER_HOST as the parked call saw it.
	seenHost atomic.Value
}

func healthyOpenSpy() *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		getConfig: func(context.Context, string) (string, error) { return "gc", nil },
	}
}

func installOpenSeam(t *testing.T) *openSeam {
	t.Helper()
	seam := &openSeam{entered: make(chan struct{}), released: make(chan struct{})}
	old := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = old })
	t.Cleanup(seam.release)
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		if seam.calls.Add(1) == 1 {
			seam.seenHost.Store(os.Getenv("BEADS_DOLT_SERVER_HOST"))
			seam.enteredAt.Do(func() { close(seam.entered) })
			<-seam.released
		}
		return healthyOpenSpy(), nil
	}
	return seam
}

func (s *openSeam) release() { s.releaseAt.Do(func() { close(s.released) }) }

func (s *openSeam) awaitParked(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first open never reached the library seam")
	}
}

// nativeOpenEntry is one of the three paths that hold an open window.
type nativeOpenEntry struct {
	name string
	open func(ctx context.Context, scopeRoot string) error
}

func nativeOpenEntries() []nativeOpenEntry {
	scoped := map[string]string{"BEADS_DOLT_SERVER_HOST": "scoped.example.com"}
	closeStore := func(store *NativeDoltStore, err error) error {
		if store != nil {
			_ = store.CloseStore()
		}
		return err
	}
	return []nativeOpenEntry{
		{name: "direct", open: func(ctx context.Context, scopeRoot string) error {
			return closeStore(OpenNativeDoltStoreAt(ctx, scopeRoot, scoped))
		}},
		{name: "without ambient env", open: func(ctx context.Context, scopeRoot string) error {
			return closeStore(OpenNativeDoltStoreAtWithoutAmbientEnv(ctx, scopeRoot))
		}},
		{name: "proxied", open: func(ctx context.Context, scopeRoot string) error {
			return closeStore(OpenNativeDoltStoreAtProxied(ctx, scopeRoot, scoped))
		}},
	}
}

// parkFirstOpen runs entry's open on a goroutine until the seam is parked inside
// its window, and returns the channel the open's error arrives on.
func parkFirstOpen(t *testing.T, entry nativeOpenEntry, seam *openSeam) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	scopeRoot := t.TempDir()
	go func() { done <- entry.open(context.Background(), scopeRoot) }()
	seam.awaitParked(t)
	return done
}

func within(t *testing.T, what string, limit time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not return within %v", what, limit)
	}
}

// A window holder uses the bare os env for its projected view; every reader of
// this package answers from the ambient view, at once, whatever window stands.
func TestAmbientReadersAnswerPromptlyDuringEveryOpenWindow(t *testing.T) {
	for _, entry := range nativeOpenEntries() {
		t.Run(entry.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
			t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
			t.Setenv("BD_EVENTS_JOURNAL", "0")
			scopeRoot := t.TempDir()
			writeScopeJournalConfig(t, scopeRoot, "events-journal: true\n")
			beadsDir := filepath.Join(scopeRoot, ".beads")

			seam := installOpenSeam(t)
			openDone := parkFirstOpen(t, entry, seam)
			if got, _ := seam.seenHost.Load().(string); got == "ambient.example.com" {
				t.Fatalf("the open window did not project its own BEADS_DOLT_SERVER_HOST: the parked open saw the ambient %q", got)
			}

			within(t, "ProcessEnvSnapshotExcludingNativeDoltOpen", time.Second, func() {
				env := ProcessEnvSnapshotExcludingNativeDoltOpen()
				if got := beadsDoltServerHostFromEnv(env); got != "ambient.example.com" {
					t.Errorf("BEADS_DOLT_SERVER_HOST in the snapshot = %q, want ambient.example.com", got)
				}
				if got := envValues(env, "BD_EVENTS_JOURNAL"); len(got) != 1 || got[0] != "0" {
					t.Errorf("BD_EVENTS_JOURNAL in the snapshot = %v, want [0]", got)
				}
			})
			within(t, "AmbientNativeDoltOpenEnv", time.Second, func() {
				if got := AmbientNativeDoltOpenEnv("BEADS_DOLT_SERVER_TLS"); got != "1" {
					t.Errorf("AmbientNativeDoltOpenEnv(BEADS_DOLT_SERVER_TLS) = %q, want 1", got)
				}
				if got := AmbientNativeDoltOpenEnv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
					t.Errorf("AmbientNativeDoltOpenEnv(BEADS_DOLT_SERVER_HOST) = %q, want ambient.example.com", got)
				}
			})
			within(t, "ambientEventsJournalEnabledFor", time.Second, func() {
				if ambientEventsJournalEnabledFor(beadsDir) {
					t.Error("ambientEventsJournalEnabledFor = true over the ambient BD_EVENTS_JOURNAL=0")
				}
			})

			seam.release()
			select {
			case err := <-openDone:
				if err != nil {
					t.Fatalf("the parked open: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the parked open did not return after the seam was released")
			}
		})
	}
}

// A second open that needs the gate while a first holds it waits only as long
// as its own ctx lets it, fails with the ctx's error, and touches nothing: the
// library is not called, and the process env is left as the first open set it.
func TestSecondOpenWindowIsBoundByItsOwnContext(t *testing.T) {
	for _, entry := range nativeOpenEntries() {
		t.Run(entry.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
			seam := installOpenSeam(t)
			openDone := parkFirstOpen(t, entry, seam)
			inWindow := os.Getenv("BEADS_DOLT_SERVER_HOST")

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			secondErr := make(chan error, 1)
			go func() { secondErr <- entry.open(ctx, t.TempDir()) }()
			select {
			case err := <-secondErr:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("second open error = %v, want one wrapping context.DeadlineExceeded", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the second open is still waiting for the window long after its own ctx expired")
			}
			if n := seam.calls.Load(); n != 1 {
				t.Fatalf("the library was opened %d times, want only the first open's call", n)
			}
			if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != inWindow {
				t.Fatalf("BEADS_DOLT_SERVER_HOST = %q after the refused second open, want the first window's %q untouched", got, inWindow)
			}

			seam.release()
			if err := <-openDone; err != nil {
				t.Fatalf("the first open: %v", err)
			}
			if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
				t.Fatalf("BEADS_DOLT_SERVER_HOST = %q after the first open, want the ambient value restored", got)
			}
		})
	}
}

// The window is released on every way out of an open: the one that returned
// normally, the one that errored, and the one whose library call panicked.
func TestOpenWindowIsReleasedAfterEveryWayOut(t *testing.T) {
	errOpen := errors.New("the library refused the open")
	for _, entry := range nativeOpenEntries() {
		for _, tc := range []struct {
			name   string
			first  func(context.Context) (beadslib.Storage, error)
			panics bool
		}{
			{name: "returned", first: func(context.Context) (beadslib.Storage, error) { return healthyOpenSpy(), nil }},
			{name: "errored", first: func(context.Context) (beadslib.Storage, error) { return nil, errOpen }},
			{name: "panicked", first: func(context.Context) (beadslib.Storage, error) { panic("library panic") }, panics: true},
		} {
			t.Run(entry.name+"/"+tc.name, func(t *testing.T) {
				t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
				old := nativeDoltOpenBestAvailable
				t.Cleanup(func() { nativeDoltOpenBestAvailable = old })
				var calls atomic.Int32
				nativeDoltOpenBestAvailable = func(ctx context.Context, _ string) (beadslib.Storage, error) {
					if calls.Add(1) == 1 {
						return tc.first(ctx)
					}
					return healthyOpenSpy(), nil
				}

				func() {
					defer func() {
						if r := recover(); r != nil && !tc.panics {
							panic(r)
						}
					}()
					_ = entry.open(context.Background(), t.TempDir())
				}()

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := entry.open(ctx, t.TempDir()); err != nil {
					t.Fatalf("the open after the %s one: %v (the window was not released)", tc.name, err)
				}
				if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
					t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want the ambient value restored", got)
				}
			})
		}
	}
}

// A bd child runs while an open window stands: it starts at once, sees the
// ambient environment, and is bound by its own budget, never by the window.
func TestBdStoreCommandRunsDuringAnOpenWindow(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
	seam := installOpenSeam(t)
	openDone := parkFirstOpen(t, nativeOpenEntries()[0], seam)

	t.Run("sees the ambient env", func(t *testing.T) {
		run := ExecCommandRunnerWithEnvContext(context.Background(), nil)
		var out []byte
		var err error
		within(t, "the child command", 5*time.Second, func() {
			out, err = run(t.TempDir(), "sh", "-c", `printf %s "$BEADS_DOLT_SERVER_HOST"`)
		})
		if err != nil {
			t.Fatalf("child command: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "ambient.example.com" {
			t.Fatalf("the child saw BEADS_DOLT_SERVER_HOST=%q, want ambient.example.com", got)
		}
	})
	t.Run("is bound by its own budget", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		run := ExecCommandRunnerWithEnvContext(ctx, nil)
		start := time.Now()
		var err error
		within(t, "the budgeted child command", 10*time.Second, func() {
			_, err = run(t.TempDir(), "sh", "-c", "sleep 30")
		})
		if err == nil {
			t.Fatal("a 30s sleep under a 300ms budget returned no error")
		}
		if took := time.Since(start); took > 8*time.Second {
			t.Fatalf("the child took %v under a 300ms budget", took)
		}
	})

	seam.release()
	if err := <-openDone; err != nil {
		t.Fatalf("the parked open: %v", err)
	}
}

// A reconnect whose open cannot enter the gate before the read budget ends
// returns the budget error, with the ctx error behind it.
func TestReadReconnectReturnsTheBudgetErrorWhileAnotherOpenHoldsTheWindow(t *testing.T) {
	seam := installOpenSeam(t)
	openDone := parkFirstOpen(t, nativeOpenEntries()[0], seam)

	scopeRoot := t.TempDir()
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("[mysql] i/o timeout")))
	store.reopen = func(ctx context.Context) (beadslib.Storage, error) {
		storage, _, err := openNativeStorage(ctx, scopeRoot, nil, false)
		return storage, err
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- store.withReadRetryContext(ctx, func(context.Context, beadslib.Storage) error {
			return errors.New("[mysql] i/o timeout")
		})
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read error = %v, want one wrapping context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "retry budget exhausted") {
			t.Fatalf("read error = %q, want the read retry budget error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read is still waiting on another open's window long after its budget ended")
	}
	if n := seam.calls.Load(); n != 1 {
		t.Fatalf("the library was opened %d times, want only the parked open's call", n)
	}

	seam.release()
	if err := <-openDone; err != nil {
		t.Fatalf("the parked open: %v", err)
	}
}
