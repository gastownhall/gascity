package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The cross-process harness's CLI-vs-legacy case (R6.4): the test binary
// re-execs itself as a `gc session` CLI starting a session through its
// Manager, while this process runs the controller's legacy start of the same
// row. They share the SQLite store and the city's lock dir.

const cliStartRoleEnv = "GC_TEST_ROLE"

// unshardedEnv is this process's environment without the test runner's shard
// assignment, so a re-exec'd role is not filtered out of its own run.
func unshardedEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "TEST_SHARD_") || strings.HasPrefix(e, "TEST_TOTAL_SHARDS=")
	})
}

// openRequireSQLite opens the SQLite store under dir in conditional_writes=require.
func openRequireSQLite(t *testing.T, dir string) beads.Store {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	s := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	if err := beads.StampOpenedStore(s, "SQLiteStore", gate.Require, nil, nil); err != nil {
		t.Fatalf("StampOpenedStore: %v", err)
	}
	return s
}

// stdinGatedProvider's Start reports, then waits for a line on stdin: the CLI
// is mid-start, holding the runtime lease.
type stdinGatedProvider struct{ *runtime.Fake }

func (p stdinGatedProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	fmt.Println("starting")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	return p.Fake.Start(ctx, name, cfg)
}

// TestRuntimeLeaseCLIStartRole is the re-exec'd process, a no-op otherwise.
//   - cli-start: a `gc session` CLI starting the row through its Manager.
//   - legacy-start: the controller's legacy start of the row.
//
// Either blocks inside its provider Start, holding the lease, until a line
// arrives on stdin.
func TestRuntimeLeaseCLIStartRole(t *testing.T) {
	role := os.Getenv(cliStartRoleEnv)
	if role != "cli-start" && role != "legacy-start" {
		return
	}
	store := openRequireSQLite(t, os.Getenv("GC_TEST_LEASE_STORE"))
	city, id := os.Getenv("GC_TEST_LEASE_CITY"), os.Getenv("GC_TEST_LEASE_ID")
	sp := stdinGatedProvider{runtime.NewFake()}
	if role == "cli-start" {
		mgr := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(city), sessionpkg.WithRuntimeLeaseTTL(time.Minute))
		fmt.Printf("started err=%v\n", mgr.Start(context.Background(), id, "worker", runtime.Config{}, sessionpkg.ResumeOperator))
		return
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	woken := executePlannedStartsTraced(context.Background(), []startCandidate{{info: sessiontest.SeedBead(t, bead), tp: tp}},
		&config.City{Agents: []config.Agent{{Name: "worker"}}}, map[string]TemplateParams{"worker": tp}, sp, store,
		"test-city", city, clk, events.Discard, time.Minute, io.Discard, io.Discard, nil)
	fmt.Printf("started woken=%d\n", woken)
}

// TestRuntimeLeaseCLIVsLegacyStart races a CLI's start and the controller's
// legacy start of one row across two processes, in both directions: whichever
// is mid-start holds the lease, and the other defers (the legacy start) or
// fails retryably (the CLI) without writing, then starts under a newer epoch.
func TestRuntimeLeaseCLIVsLegacyStart(t *testing.T) {
	// spawn runs role as a second process over f's store and city.
	spawn := func(t *testing.T, f *leaseStartFixture, storeDir, role string) (expect func(string), proceed func()) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeLeaseCLIStartRole$", "-test.count=1")
		cmd.Env = append(unshardedEnv(), cliStartRoleEnv+"="+role, "GC_TEST_LEASE_STORE="+storeDir,
			"GC_TEST_LEASE_CITY="+f.city, "GC_TEST_LEASE_ID="+f.cand.info.ID)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		lines := make(chan string, 16)
		go func() {
			for sc := bufio.NewScanner(out); sc.Scan(); {
				lines <- sc.Text()
			}
			close(lines)
		}()
		expect = func(want string) {
			t.Helper()
			deadline := time.After(time.Minute)
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatalf("the %s process exited before reporting %q", role, want)
					}
					if strings.HasPrefix(line, want) {
						return
					}
				case <-deadline:
					t.Fatalf("the %s process never reported %q", role, want)
				}
			}
		}
		proceed = func() {
			if _, err := io.WriteString(in, "go\n"); err != nil {
				t.Fatal(err)
			}
		}
		return expect, proceed
	}

	t.Run("CLI mid-start, legacy start defers", func(t *testing.T) {
		storeDir := t.TempDir()
		f := newLeaseStartFixture(t, openRequireSQLite(t, storeDir))
		expect, proceed := spawn(t, f, storeDir, "cli-start")
		expect("starting")
		if got := f.run(); got != 0 || !strings.Contains(f.log.String(), "outcome=deferred_by_runtime_lease") {
			t.Fatalf("legacy start while the CLI starts = %d, log %q; want deferred", got, f.log.String())
		}
		if meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata; meta["generation"] != "1" || meta[sessionpkg.RuntimeLeaseEpochKey] != "1" || len(f.sp.starts) != 0 {
			t.Fatalf("row %v, starts %q; want the CLI's epoch 1 and no legacy PreWake or Start", meta, f.sp.starts)
		}
		proceed()
		expect("started err=<nil>")
		if got := f.run(); got != 1 {
			t.Fatalf("legacy start after the CLI's = %d, want 1; log %q", got, f.log.String())
		}
		if meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata; meta[sessionpkg.RuntimeLeaseEpochKey] != "2" || meta[sessionpkg.RuntimeLeaseHolderKey] != "" {
			t.Fatalf("record after both starts = %v, want epoch 2 released", meta)
		}
	})

	t.Run("controller mid-start, CLI refused", func(t *testing.T) {
		defer sessionpkg.SetOperatorLeaseWaitForTest(500 * time.Millisecond)()
		storeDir := t.TempDir()
		f := newLeaseStartFixture(t, openRequireSQLite(t, storeDir))
		expect, proceed := spawn(t, f, storeDir, "legacy-start")
		expect("starting")
		mgr := sessionpkg.NewManagerWithOptions(f.store, runtime.NewFake(), sessionpkg.WithCityPath(f.city), sessionpkg.WithRuntimeLeaseTTL(time.Minute))
		if err := mgr.Start(context.Background(), f.cand.info.ID, "worker", runtime.Config{}, sessionpkg.ResumeOperator); !errors.Is(err, sessionpkg.ErrSessionStarting) {
			t.Fatalf("CLI start while the controller starts = %v, want ErrSessionStarting", err)
		}
		if meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata; meta[sessionpkg.RuntimeLeaseEpochKey] != "1" || meta["generation"] != "2" {
			t.Fatalf("row %v, want the controller's epoch 1 and only its PreWake", meta)
		}
		proceed()
		expect("started woken=1")
		if meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata; meta[sessionpkg.RuntimeLeaseHolderKey] != "" {
			t.Fatalf("record after the controller's start = %v, want released", meta)
		}
	})
}
