//go:build unix

package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// A FIFO is the script's completion signal, not a sleep guessing that an op
// started. All processes belong to the fixture provider and its temp dir.
func statusFIFO(t *testing.T, pk opsPack, name string) *os.File {
	t.Helper()
	path := filepath.Join(pk.dir, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func statusRaw(t *testing.T, p runtime.Provider) *Provider {
	t.Helper()
	switch v := p.(type) {
	case *Provider:
		return v
	case *seamBackedProvider:
		return v.raw
	default:
		t.Fatalf("unexpected provider %T", p)
		return nil
	}
}

func statusSignal(t *testing.T, f *os.File) {
	t.Helper()
	done := make(chan error, 1)
	go func() { var b [1]byte; _, err := io.ReadFull(f, b[:]); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("fixture did not acknowledge op")
	}
}

func statusRelease(t *testing.T, f *os.File) {
	t.Helper()
	if _, err := f.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
}

type statusResult struct {
	status runtime.ProgramStatus
	err    error
}

func statusAwait(t *testing.T, done <-chan statusResult) statusResult {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("status caller did not finish")
		return statusResult{}
	}
}

func statusAsync(read func() (runtime.ProgramStatus, error)) <-chan statusResult {
	done := make(chan statusResult, 1)
	go func() { s, err := read(); done <- statusResult{s, err} }()
	return done
}

func TestProgramStatusSlotWaitIsNotARead(t *testing.T) {
	oldBound := programStatusIdleReadBound
	programStatusIdleReadBound = 20 * time.Millisecond
	t.Cleanup(func() { programStatusIdleReadBound = oldBound })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps+`,"program-status"`)
			pk.put(t, "program-status.body", `echo '{"state":"idle"}'`)
			p := ctor(pk.script)
			raw := statusRaw(t, p)
			raw.SetProgramStatusMode(runtime.ProgramStatusOn)
			for range cap(raw.programs.slots) {
				raw.programs.slots <- struct{}{}
			}
			_, err := raw.readProgramStatus(t.Context(), "s", 20*time.Millisecond)
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("slot wait error=%v", err)
			}
			if err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second); err != nil {
				t.Fatal(err)
			}
			raw.programs.mu.Lock()
			e := raw.programs.sessions["s"]
			if !e.at.IsZero() || e.err != nil || e.outcome != (runtime.ProgramStatus{}) || e.applied != 0 {
				t.Errorf("slot wait cached an outcome: at=%v err=%v outcome=%+v applied=%d", e.at, e.err, e.outcome, e.applied)
			}
			raw.programs.mu.Unlock()
			if strings.Contains(pk.log(t), "OP program-status") {
				t.Error("slot wait sent a status op")
			}
			for range cap(raw.programs.slots) {
				<-raw.programs.slots
			}
			got, err := p.(runtime.ProgramStatusProvider).ProgramStatus("s")
			if err != nil || !got.Valid() || strings.Count(pk.log(t), "OP program-status") != 1 {
				t.Errorf("slot wait poisoned the next read: %+v %v", got, err)
			}
		})
	}
}

func TestProgramStatusSlotTimeoutKeepsInFlightAnswer(t *testing.T) {
	// The first read must remain alive while the second exhausts its slot bound.
	oldRead := programStatusReadTimeout
	programStatusReadTimeout = testutil.ExecRaceTimeout
	t.Cleanup(func() { programStatusReadTimeout = oldRead })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			started, release := statusFIFO(t, pk, "started"), statusFIFO(t, pk, "release")
			pk.put(t, "program-status.body", `printf A > "$D/started"; dd bs=1 count=1 < "$D/release" >/dev/null 2>&1; echo '{"state":"idle"}'`)
			raw := statusRaw(t, p)
			for range cap(raw.programs.slots) - 1 {
				raw.programs.slots <- struct{}{}
			}
			done := statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("victim") })
			statusSignal(t, started)
			_, err := raw.readProgramStatus(t.Context(), "victim", 20*time.Millisecond)
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("slot timeout error=%v", err)
			}
			raw.programs.mu.Lock()
			e := raw.programs.sessions["victim"]
			if e.applied != 0 || !e.at.IsZero() || e.err != nil || e.outcome != (runtime.ProgramStatus{}) {
				t.Errorf("slot timeout applied: applied=%d at=%v err=%v outcome=%+v", e.applied, e.at, e.err, e.outcome)
			}
			raw.programs.mu.Unlock()
			statusRelease(t, release)
			got := statusAwait(t, done)
			if got.err != nil || !got.status.Valid() {
				t.Errorf("in-flight answer fenced by slot wait: %+v %v", got.status, got.err)
			}
			cached, err := ps.ProgramStatus("victim")
			if err != nil || cached != got.status || strings.Count(pk.log(t), "OP program-status") != 1 {
				t.Errorf("in-flight answer not cached: %+v %v", cached, err)
			}
			for range cap(raw.programs.slots) - 1 {
				<-raw.programs.slots
			}
		})
	}
}

func TestProgramStatusCompletionPrecedence(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, event := range []string{"off", "cancel", "shadow", "stop", "forget", "replacement"} {
			t.Run(name+"/"+event, func(t *testing.T) {
				pk, p, _ := statusProvider(t, ctor, runtime.ProgramStatusOn)
				raw := statusRaw(t, p)
				started, release := statusFIFO(t, pk, "started"), statusFIFO(t, pk, "release")
				pk.put(t, "program-status.body", `printf A > "$D/started"; dd bs=1 count=1 < "$D/release" >/dev/null 2>&1; echo '{"state":"working"}'`)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := statusAsync(func() (runtime.ProgramStatus, error) {
					return raw.readProgramStatus(ctx, "s", testutil.ExecRaceTimeout)
				})
				statusSignal(t, started)
				switch event {
				case "off":
					raw.SetProgramStatusMode(runtime.ProgramStatusOff)
					cancel() // Off takes precedence even over caller cancellation.
				case "cancel":
					cancel()
				case "shadow":
					raw.SetProgramStatusMode(runtime.ProgramStatusShadow)
				case "stop":
					if err := p.Stop("s"); err != nil {
						t.Fatal(err)
					}
				case "forget", "replacement":
					raw.forgetProgramStatus("s")
					if event == "replacement" {
						raw.programs.mu.Lock()
						raw.programSessionLocked("s")
						raw.programs.mu.Unlock()
					}
				}
				statusRelease(t, release)
				got := statusAwait(t, done)
				if event == "off" {
					if got.err != nil || got.status != (runtime.ProgramStatus{Mode: runtime.ProgramStatusOff}) {
						t.Errorf("off completion was not Unread with nil error: %+v %v", got.status, got.err)
					}
				} else if !errors.Is(got.err, runtime.ErrRuntimeUnavailable) || got.status.Valid() {
					t.Errorf("stale completion was not discarded: %+v %v", got.status, got.err)
				}
				raw.programs.mu.Lock()
				if e := raw.programs.sessions["s"]; e != nil && !e.at.IsZero() {
					t.Error("discard cached an outcome")
				}
				raw.programs.mu.Unlock()
			})
		}
	}
}

func TestProgramStatusOvertakenReadServesCachedOutcome(t *testing.T) {
	oldRead := programStatusReadTimeout
	programStatusReadTimeout = testutil.ExecRaceTimeout
	t.Cleanup(func() { programStatusReadTimeout = oldRead })
	for name, ctor := range opsConstructors {
		for _, event := range []string{"success", "failure", "older-resets", "newer-resets"} {
			t.Run(name+"/"+event, func(t *testing.T) {
				pk, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
				raw := statusRaw(t, p)
				generation := strings.HasSuffix(event, "resets")
				if generation {
					if _, err := ps.ProgramStatus("s"); err != nil {
						t.Fatal(err)
					}
					raw.programs.mu.Lock()
					raw.programs.sessions["s"].at = time.Time{}
					raw.programs.mu.Unlock()
				}
				aStarted, aRelease := statusFIFO(t, pk, "a.started"), statusFIFO(t, pk, "a.release")
				bStarted, bRelease := statusFIFO(t, pk, "b.started"), statusFIFO(t, pk, "b.release")
				aAnswer, bAnswer := `echo '{"state":"working","incarnation":"run"}'`, `echo '{"state":"idle","incarnation":"run"}'`
				if event == "failure" {
					bAnswer = `exit 1`
				}
				if event == "older-resets" {
					aAnswer = `echo '{"state":"working","incarnation":"new"}'`
				}
				if event == "newer-resets" {
					bAnswer = `echo '{"state":"idle","incarnation":"new"}'`
				}
				pk.put(t, "program-status.body", `if mkdir "$D/first" 2>/dev/null; then printf A > "$D/a.started"; dd bs=1 count=1 < "$D/a.release" >/dev/null 2>&1; `+aAnswer+`; else printf B > "$D/b.started"; dd bs=1 count=1 < "$D/b.release" >/dev/null 2>&1; `+bAnswer+`; fi`)
				a := statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("s") })
				statusSignal(t, aStarted)
				// A second ordinary caller must see the flight or its cached result,
				// never another op. The direct reader below can overtake that flight.
				shared := statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("s") })
				b := statusAsync(func() (runtime.ProgramStatus, error) {
					return raw.readProgramStatus(t.Context(), "s", testutil.ExecRaceTimeout)
				})
				statusSignal(t, bStarted)
				var first, last statusResult
				if event == "older-resets" {
					statusRelease(t, aRelease)
					first = statusAwait(t, a)
					statusRelease(t, bRelease)
					last = statusAwait(t, b)
				} else {
					statusRelease(t, bRelease)
					first = statusAwait(t, b)
					statusRelease(t, aRelease)
					last = statusAwait(t, a)
				}
				joined := statusAwait(t, shared)
				if event == "older-resets" {
					if last.err != nil || !last.status.Valid() || last.status.State != runtime.ProgramIdle || last.status.Nonce == first.status.Nonce {
						t.Errorf("later-ticket read did not apply its own answer: first=%+v last=%+v", first, last)
					}
					if (joined.status != first.status || !errors.Is(joined.err, first.err)) && (joined.status != last.status || !errors.Is(joined.err, last.err)) {
						t.Errorf("shared caller served neither applied outcome: first=%+v last=%+v shared=%+v", first, last, joined)
					}
					cached, err := ps.ProgramStatus("s")
					if cached != last.status || !errors.Is(err, last.err) {
						t.Errorf("cache does not hold the later-ticket answer: %+v %v", cached, err)
					}
				} else {
					if last.status != first.status || !errors.Is(last.err, first.err) || joined.status != first.status || !errors.Is(joined.err, first.err) {
						t.Errorf("overtaken caller did not serve cached outcome: first=%+v last=%+v shared=%+v", first, last, joined)
					}
					cached, err := ps.ProgramStatus("s")
					if cached != first.status || !errors.Is(err, first.err) {
						t.Errorf("cache changed: %+v %v", cached, err)
					}
				}
				wantOps := 2
				if generation {
					wantOps++
				}
				if n := strings.Count(pk.log(t), "OP program-status"); n != wantOps {
					t.Errorf("ops=%d want=%d", n, wantOps)
				}
			})
		}
	}
}

func TestProgramStatusOvertakenIdleWaitUsesStatus(t *testing.T) {
	// Leave time for the explicit FIFO handoff, not just the normal idle poll.
	oldBound := programStatusIdleReadBound
	programStatusIdleReadBound = testutil.ExecRaceTimeout
	t.Cleanup(func() { programStatusIdleReadBound = oldBound })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps+`,"program-status"`)
			pk.put(t, "screen", busyClaudePane)
			started, release := statusFIFO(t, pk, "started"), statusFIFO(t, pk, "release")
			pk.put(t, "program-status.body", `if mkdir "$D/first" 2>/dev/null; then printf A > "$D/started"; dd bs=1 count=1 < "$D/release" >/dev/null 2>&1; echo '{"state":"working"}'; else echo '{"state":"idle"}'; fi`)
			p := ctor(pk.script)
			statusRaw(t, p).SetProgramStatusMode(runtime.ProgramStatusOn)
			done := make(chan error, 1)
			go func() { done <- p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second) }()
			statusSignal(t, started)
			if got, err := p.(runtime.ProgramStatusProvider).ProgramStatus("s"); err != nil || got.State != runtime.ProgramIdle {
				t.Fatalf("newer read: %+v %v", got, err)
			}
			statusRelease(t, release)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("overtaken idle wait fell back to busy screen: %v", err)
				}
			case <-time.After(testutil.ExecRaceTimeout):
				t.Fatal("idle wait did not finish")
			}
			if n := strings.Count(pk.log(t), "OP program-status"); n < 3 {
				t.Errorf("no status op after overtaken read: ops=%d", n)
			}
		})
	}
}

func TestProgramStatusHigherTicketReadAppliesOwnAnswer(t *testing.T) {
	oldRead := programStatusReadTimeout
	programStatusReadTimeout = testutil.ExecRaceTimeout
	t.Cleanup(func() { programStatusReadTimeout = oldRead })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			raw := statusRaw(t, p)
			pk.put(t, "program-status.body", `echo '{"state":"working","incarnation":"a"}'`)
			if got, err := ps.ProgramStatus("s"); err != nil || !got.Valid() || got.State != runtime.ProgramWorking {
				t.Fatalf("seed: %+v %v", got, err)
			}
			raw.programs.mu.Lock()
			raw.programs.sessions["s"].at = time.Time{}
			raw.programs.mu.Unlock()
			xStarted, xRelease := statusFIFO(t, pk, "x.started"), statusFIFO(t, pk, "x.release")
			rStarted, rRelease := statusFIFO(t, pk, "r.started"), statusFIFO(t, pk, "r.release")
			pk.put(t, "program-status.body", `if mkdir "$D/x" 2>/dev/null; then printf X > "$D/x.started"; dd bs=1 count=1 < "$D/x.release" >/dev/null 2>&1; echo '{"state":"idle","incarnation":"b"}'; else printf R > "$D/r.started"; dd bs=1 count=1 < "$D/r.release" >/dev/null 2>&1; echo '{"state":"working","incarnation":"b"}'; fi`)
			x := statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("s") })
			statusSignal(t, xStarted)
			// The script's acknowledgement proves X already holds its ticket.
			r := statusAsync(func() (runtime.ProgramStatus, error) {
				return raw.readProgramStatus(t.Context(), "s", testutil.ExecRaceTimeout)
			})
			statusSignal(t, rStarted)
			statusRelease(t, xRelease)
			gotX := statusAwait(t, x)
			if gotX.err != nil || !gotX.status.Valid() || gotX.status.State != runtime.ProgramIdle {
				t.Fatalf("display read X: %+v %v", gotX.status, gotX.err)
			}
			statusRelease(t, rRelease)
			gotR := statusAwait(t, r)
			if gotR.err != nil || !gotR.status.Valid() || gotR.status.State != runtime.ProgramWorking {
				t.Errorf("higher-ticket read was served the overtaken outcome: %+v %v", gotR.status, gotR.err)
			}
			cached, err := ps.ProgramStatus("s")
			if err != nil || cached.State != runtime.ProgramWorking || cached != gotR.status {
				t.Errorf("cache does not hold the higher-ticket answer: %+v %v", cached, err)
			}
			if n := strings.Count(pk.log(t), "OP program-status"); n != 3 {
				t.Errorf("ops=%d want=3", n)
			}
		})
	}
}

func TestProgramStatusIdleWaitCountsHigherTicketAnswer(t *testing.T) {
	oldBound, oldRead := programStatusIdleReadBound, programStatusReadTimeout
	programStatusIdleReadBound, programStatusReadTimeout = testutil.ExecRaceTimeout, testutil.ExecRaceTimeout
	t.Cleanup(func() { programStatusIdleReadBound, programStatusReadTimeout = oldBound, oldRead })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps+`,"program-status"`)
			pk.put(t, "screen", busyClaudePane)
			p := ctor(pk.script)
			raw := statusRaw(t, p)
			raw.SetProgramStatusMode(runtime.ProgramStatusOn)
			ps := p.(runtime.ProgramStatusProvider)
			pk.put(t, "program-status.body", `echo '{"state":"working","incarnation":"a"}'`)
			if got, err := ps.ProgramStatus("s"); err != nil || !got.Valid() || got.State != runtime.ProgramWorking {
				t.Fatalf("seed: %+v %v", got, err)
			}
			raw.programs.mu.Lock()
			raw.programs.sessions["s"].at = time.Time{}
			raw.programs.mu.Unlock()
			xStarted, xRelease := statusFIFO(t, pk, "x.started"), statusFIFO(t, pk, "x.release")
			rStarted, rRelease := statusFIFO(t, pk, "r.started"), statusFIFO(t, pk, "r.release")
			pk.put(t, "program-status.body", `if mkdir "$D/x" 2>/dev/null; then printf X > "$D/x.started"; dd bs=1 count=1 < "$D/x.release" >/dev/null 2>&1; echo '{"state":"idle","incarnation":"b"}'; elif mkdir "$D/r" 2>/dev/null; then printf R > "$D/r.started"; dd bs=1 count=1 < "$D/r.release" >/dev/null 2>&1; echo '{"state":"working","incarnation":"b"}'; else echo '{"state":"idle","incarnation":"b"}'; fi`)
			x := statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("s") })
			statusSignal(t, xStarted)
			done := make(chan error, 1)
			go func() { done <- p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", testutil.ExecRaceTimeout) }()
			statusSignal(t, rStarted)
			statusRelease(t, xRelease)
			gotX := statusAwait(t, x)
			if gotX.err != nil || !gotX.status.Valid() || gotX.status.State != runtime.ProgramIdle {
				t.Fatalf("display read X: %+v %v", gotX.status, gotX.err)
			}
			statusRelease(t, rRelease)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("idle wait did not end on status: %v", err)
				}
			case <-time.After(2 * testutil.ExecRaceTimeout):
				t.Fatal("idle wait did not finish")
			}
			// X is done and no other caller runs; every op after R belongs to the wait.
			if n := strings.Count(pk.log(t), "OP program-status"); n-3 < 2 {
				t.Errorf("idle wait counted the overtaken idle: %d status ops after R, want at least 2", n-3)
			}
		})
	}
}

func TestProgramStatusLifecycleOpsFenceReads(t *testing.T) {
	// A status read overlapping an op must survive until the test releases it.
	oldRead := programStatusReadTimeout
	programStatusReadTimeout = testutil.ExecRaceTimeout
	t.Cleanup(func() { programStatusReadTimeout = oldRead })
	for name, ctor := range opsConstructors {
		for _, op := range []string{"start", "failed-start", "stop", "failed-stop", "relaunch", "separable-start", "fresh-start", "fresh-provision"} {
			if op == "separable-start" && name == "raw" {
				continue
			} // Raw Start is welded.
			for _, late := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/late=%v", name, op, late), func(t *testing.T) {
					caps := `"program-status","report-attachment","observe.v1"`
					separable := op == "relaunch" || op == "separable-start" || op == "fresh-provision"
					if separable {
						caps += `,"proc.provision","proc.exec"`
					}
					pk := newOpsPack(t, caps)
					started, release := statusFIFO(t, pk, "op.started"), statusFIFO(t, pk, "op.release")
					readStarted, readRelease := statusFIFO(t, pk, "read.started"), statusFIFO(t, pk, "read.release")
					stop := strings.Contains(op, "stop")
					failed := strings.HasPrefix(op, "failed-")
					body := `printf A > "$D/op.started"; dd bs=1 count=1 < "$D/op.release" >/dev/null 2>&1; echo '{"state":"idle"}' > "$D/answer"; `
					if stop {
						body += `rm -f "$D/running"; `
					} else {
						body += `echo true > "$D/running"; `
					}
					if failed {
						body += `exit 1`
					}
					switch op {
					case "relaunch", "separable-start":
						pk.put(t, "provision.body", `cat >/dev/null; echo true > "$D/running"`)
						pk.put(t, "exec.body", `cmd=$(cat); case "$cmd" in *has-session*) exit 1 ;; *new-session*|*respawn-pane*) `+body+` ;; esac`)
					case "fresh-provision":
						pk.put(t, "provision.body", `cat >/dev/null; `+body)
						pk.put(t, "exec.body", `cmd=$(cat); case "$cmd" in *has-session*) exit 1 ;; esac`)
					default:
						file := "start.body"
						if stop {
							file = "stop.body"
						}
						pk.put(t, file, body)
					}
					fresh := strings.HasPrefix(op, "fresh-")
					if fresh {
						pk.put(t, "observe.body", `echo '{"state":"absent"}'`)
					}
					pk.put(t, "answer", `{"state":"working"}`)
					readBody := `[ -f "$D/running" ] || exit 66; cat "$D/answer"`
					if late {
						readBody = `printf A > "$D/read.started"; dd bs=1 count=1 < "$D/read.release" >/dev/null 2>&1; echo '{"state":"working"}'`
					}
					pk.put(t, "program-status.body", readBody)
					p := ctor(pk.script)
					raw := statusRaw(t, p)
					raw.SetProgramStatusMode(runtime.ProgramStatusOn)
					ps := p.(runtime.ProgramStatusProvider)
					opDone := make(chan error, 1)
					go func() {
						cfg := runtime.Config{AcceptStartupDialogs: new(bool), FreshOnly: fresh}
						switch {
						case stop:
							opDone <- p.Stop("s")
						case op == "relaunch":
							opDone <- p.(runtime.RelaunchProvider).Relaunch(t.Context(), "s", cfg)
						default:
							opDone <- p.Start(t.Context(), "s", cfg)
						}
					}()
					statusSignal(t, started)
					var readDone <-chan statusResult
					if late {
						readDone = statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("s") })
						statusSignal(t, readStarted)
					} else {
						if got, err := ps.ProgramStatus("s"); err != nil || got.State != runtime.ProgramWorking {
							t.Fatalf("overlap reading: %+v %v", got, err)
						}
					}
					statusRelease(t, release)
					select {
					case err := <-opDone:
						if (err != nil) != failed {
							t.Fatalf("lifecycle result: %v want failure=%v", err, failed)
						}
					case <-time.After(testutil.ExecRaceTimeout):
						t.Fatal("lifecycle op did not finish")
					}
					if late {
						statusRelease(t, readRelease)
						got := statusAwait(t, readDone)
						if !errors.Is(got.err, runtime.ErrRuntimeUnavailable) || got.status.Valid() {
							t.Errorf("overlapping read applied after op: %+v %v", got.status, got.err)
						}
						pk.put(t, "program-status.body", `[ -f "$D/running" ] || exit 66; cat "$D/answer"`)
					}
					before := strings.Count(pk.log(t), "OP program-status")
					got, err := ps.ProgramStatus("s")
					if stop {
						if !errors.Is(err, runtime.ErrSessionNotFound) || got.Valid() {
							t.Errorf("stop kept overlap answer: %+v %v", got, err)
						}
					} else if err != nil || got.State != runtime.ProgramIdle || got.TurnEnds != 0 {
						t.Errorf("lifecycle kept old program state: %+v %v", got, err)
					}
					if n := strings.Count(pk.log(t), "OP program-status"); n != before+1 {
						t.Errorf("lifecycle served overlap cache: ops before=%d after=%d", before, n)
					}
				})
			}
		}
	}
}

func TestProgramStatusSwitchDuringRealWait(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, ops := range []bool{false, true} {
			t.Run(fmt.Sprint(name, ops), func(t *testing.T) {
				caps := `"report-activity","report-attachment","idle-boundary","program-status","proc.exec"`
				if ops {
					caps += `,"carrier.ops"`
				}
				pk := newOpsPack(t, caps)
				started, release := statusFIFO(t, pk, "started"), statusFIFO(t, pk, "release")
				pk.put(t, "exec.body", `cmd=$(cat); printf 'EXEC %s\n' "$cmd" >> "$D/log"; case "$cmd" in *show-environment*) printf 'GC_READY_PROMPT_PREFIX=❯ \n' ;; *capture-pane*) cat "$D/screen" ;; esac`)
				pk.put(t, "program-status.body", `printf A > "$D/started"; dd bs=1 count=1 < "$D/release" >/dev/null 2>&1; echo '{"state":"idle"}'`)
				p := ctor(pk.script)
				setter := p.(interface {
					SetProgramStatusMode(runtime.ProgramStatusMode)
				})
				setter.SetProgramStatusMode(runtime.ProgramStatusOn)
				done := make(chan error, 1)
				go func() { done <- p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", 2*time.Second) }()
				signal := make(chan error, 1)
				go func() { var b [1]byte; _, err := io.ReadFull(started, b[:]); signal <- err }()
				select {
				case err := <-signal:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("status op never started")
				}
				setter.SetProgramStatusMode(runtime.ProgramStatusShadow)
				if _, err := release.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if count := strings.Count(pk.log(t), "OP program-status"); count != 1 {
					t.Errorf("mode change sent %d status ops", count)
				}
				if ops {
					pk.noTmux(t)
				}
			})
		}
	}
}

func TestProgramStatusOutOfOrderOutcomeDiscarded(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, event := range []string{"newer-read", "stop", "replacement"} {
			t.Run(name+"/"+event, func(t *testing.T) {
				pk := newOpsPack(t, opsIdleCaps+`,"program-status"`)
				started, release := statusFIFO(t, pk, "started"), statusFIFO(t, pk, "release")
				pk.put(t, "program-status.body", `[ -f "$D/running" ] || exit 66; if mkdir "$D/first" 2>/dev/null; then printf A > "$D/started"; dd bs=1 count=1 < "$D/release" >/dev/null 2>&1; echo '{"state":"working","incarnation":"run"}'; else echo '{"state":"idle","incarnation":"run"}'; fi`)
				p := ctor(pk.script)
				p.(interface {
					SetProgramStatusMode(runtime.ProgramStatusMode)
				}).SetProgramStatusMode(runtime.ProgramStatusOn)
				ps := p.(runtime.ProgramStatusProvider)
				done := statusAsync(func() (runtime.ProgramStatus, error) { return ps.ProgramStatus("s") })
				signal := make(chan error, 1)
				go func() { var b [1]byte; _, err := io.ReadFull(started, b[:]); signal <- err }()
				select {
				case err := <-signal:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(testutil.ExecRaceTimeout):
					t.Fatal("first read did not start")
				}
				switch event {
				case "newer-read":
					if err := p.(runtime.IdleWaitProvider).WaitForIdle(t.Context(), "s", time.Second); err != nil {
						t.Fatal(err)
					}
				case "stop", "replacement":
					if err := p.Stop("s"); err != nil {
						t.Fatal(err)
					}
					if event == "replacement" {
						if err := p.Start(t.Context(), "s", runtime.Config{AcceptStartupDialogs: new(bool)}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := release.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
				completed := statusAwait(t, done)
				if event == "newer-read" {
					if completed.err != nil || completed.status.State != runtime.ProgramIdle || completed.status.TurnStarts != 0 {
						t.Errorf("overtaken read did not serve newer outcome: %+v %v", completed.status, completed.err)
					}
				} else if !errors.Is(completed.err, runtime.ErrRuntimeUnavailable) {
					t.Errorf("older read was not discarded: %v", completed.err)
				}
				got, err := ps.ProgramStatus("s")
				if event == "stop" {
					if !errors.Is(err, runtime.ErrSessionNotFound) || got.Valid() {
						t.Errorf("stopped session revived by old outcome: %+v %v", got, err)
					}
				} else if err != nil || got.State != runtime.ProgramIdle || got.TurnStarts != 0 {
					t.Errorf("old outcome replaced newer: %+v %v", got, err)
				}
			})
		}
	}
}
