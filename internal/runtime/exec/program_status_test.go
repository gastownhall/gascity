package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestParseProgramStatus(t *testing.T) {
	for _, tc := range []struct {
		body string
		bad  bool
	}{
		{`{"state":"working"}`, false},
		{`{"state":""}`, false},
		{`{"state":"future"}`, false},
		{`{"state":"blocked","kind":"future","unknown":"anything"}`, false},
		{`{"state":"idle"} {}`, true},
		{`{"state":"idle"} garbage`, true},
		{`null`, true},
		{`[]`, true},
		{`1`, true},
		{`{}`, true},
		{`{"state":null}`, true},
		{`{"state":1}`, true},
		{`{"state":"idle","kind":1}`, true},
		{`{"state":"idle","app":1}`, true},
		{`{"state":"idle","progress":"1"}`, true},
		{`{"state":"working","progress":101}`, false},
		{`{"state":"working","progress":0.5}`, false},
		{`{"state":"idle","children":-1}`, true},
		{`{"state":"idle","children":0.1}`, true},
		{`{"state":"idle","children":2147483648}`, true},
		{`{"state":"idle","children":1e-1000}`, true},
		{`{"state":"working","progress":1e1000}`, false},
		{`{"state":"working","progress":1.00000000000000000000000000000000000000000000000000000000000000001}`, false},
		{`{"state":"idle","children":1,"working_children":2}`, true},
		{`{"state":"idle","working_children":"1"}`, true},
		{`{"state":"idle","exited":1}`, true},
		{`{"state":"idle","incarnation":42}`, true},
		{`{"state":"idle","incarnation":1}`, true},
		{`{"state":"idle","incarnation":true}`, true},
		{`{"state":"idle","incarnation":{}}`, true},
		{`{"state":"idle","incarnation":[]}`, true},
		{`{"state":"idle","incarnation":null}`, false},
		{`{"state":"idle","incarnation":""}`, false},
		{`{"state":"idle","incarnation":"with space"}`, true},
		{`{"state":"idle","incarnation":"` + strings.Repeat("a", 129) + `"}`, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			got, err := ParseProgramStatus([]byte(tc.body))
			if err != nil && got != (ProgramStatusAnswer{}) {
				t.Errorf("malformed answer carries claims: %+v", got)
			}
			if (err != nil) != tc.bad {
				t.Errorf("error=%v, malformed=%v", err, tc.bad)
			}
			if err != nil && len(tc.body) > 1 && strings.Contains(err.Error(), tc.body) {
				t.Error("error includes answer bytes")
			}
		})
	}
	for _, number := range []string{"1e1000", "1e-1000", "1.00000000000000000000000000000000000000000000000000000000000000001"} {
		a, err := ParseProgramStatus([]byte(`{"state":"working","progress":` + number + `}`))
		if err != nil || a.Progress != -1 {
			t.Errorf("progress %s: %+v %v, want -1", number, a, err)
		}
	}
	a, err := ParseProgramStatus([]byte(`{"state":"blocked","kind":"auth","app":"psdemo","progress":40,"children":2,"working_children":1,"incarnation":"run"}`))
	if err != nil || a.Kind != runtime.ProgramBlockAuth || a.Progress != 40 || a.Children != 2 || a.App != "psdemo" {
		t.Fatalf("%+v %v", a, err)
	}
	for _, tc := range []struct {
		body string
		want ProgramStatusAnswer
	}{
		{`{"state":"blocked","kind":"telepathy","app":"bad space","progress":-1}`, ProgramStatusAnswer{State: runtime.ProgramBlocked, Progress: -1}},
		{`{"state":"idle","kind":"auth","progress":40}`, ProgramStatusAnswer{State: runtime.ProgramIdle, Progress: -1}},
		{`{"state":"working","incarnation":"run","title":"secret text","msg":"ignored"}`, ProgramStatusAnswer{State: runtime.ProgramWorking, Progress: -1, Incarnation: "run"}},
		{`{"state":"","kind":null,"app":null,"progress":null,"children":null,"working_children":null,"exited":null,"incarnation":null}`, ProgramStatusAnswer{Progress: -1}},
		{`{"state":"future"}`, ProgramStatusAnswer{Progress: -1, UnknownState: true}},
		{`{"state":"idle"}`, ProgramStatusAnswer{State: runtime.ProgramIdle, Progress: -1}},
		{`{"state":"idle","incarnation":""}`, ProgramStatusAnswer{State: runtime.ProgramIdle, Progress: -1}},
		{`{"state":"idle","incarnation":null}`, ProgramStatusAnswer{State: runtime.ProgramIdle, Progress: -1}},
	} {
		got, err := ParseProgramStatus([]byte(tc.body))
		if err != nil || got != tc.want {
			t.Errorf("normalize %s: %+v %v, want %+v", tc.body, got, err, tc.want)
		}
	}
	a, _ = ParseProgramStatus([]byte(`{"state":"unknown"}`))
	if !a.UnknownState {
		t.Error("unknown state was cleared")
	}
}

func TestProgramStatusResetTriggers(t *testing.T) {
	age := programStatusCacheAge
	programStatusCacheAge = 0
	t.Cleanup(func() { programStatusCacheAge = age })
	for name, ctor := range opsConstructors {
		for _, trigger := range []string{"appears", "disappears", "live-after-exited"} {
			t.Run(name+"/"+trigger, func(t *testing.T) {
				pk, _, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
				inc := "run"
				if trigger == "appears" {
					inc = ""
				}
				read := func(state, incarnation string, exited bool) runtime.ProgramStatus {
					pk.put(t, "answer", fmt.Sprintf(`{"state":%q,"incarnation":%q,"exited":%v}`, state, incarnation, exited))
					got, err := ps.ProgramStatus("s")
					if err != nil {
						t.Fatal(err)
					}
					return got
				}
				var before runtime.ProgramStatus
				for _, state := range []string{"idle", "working", "idle", "working"} {
					before = read(state, inc, false)
				}
				if before.TurnStarts != 2 {
					t.Fatalf("baseline: %+v", before)
				}
				switch trigger {
				case "appears":
					inc = "run"
				case "disappears":
					inc = ""
				case "live-after-exited":
					if got := read("working", inc, true); got.Validity != runtime.ProgramStatusExited {
						t.Fatalf("exit: %+v", got)
					}
				}
				got := read("working", inc, false)
				if got.TurnStarts != 1 || got.TurnEnds != 0 || got.Seq != 1 || got.AdvancedPast(before.Baseline()) {
					t.Errorf("reset credited old history: before=%+v after=%+v", before, got)
				}
				if inc == "" {
					if got.Nonce != "" {
						t.Errorf("disappeared incarnation kept nonce: %+v", got)
					}
				} else if got.Nonce == "" || got.Nonce == before.Nonce {
					t.Errorf("reset did not replace nonce: before=%+v after=%+v", before, got)
				}
			})
		}
	}
}

func statusProvider(t *testing.T, ctor func(string) runtime.Provider, mode runtime.ProgramStatusMode) (opsPack, runtime.Provider, runtime.ProgramStatusProvider) {
	t.Helper()
	pk := newOpsPack(t, `"program-status"`)
	pk.put(t, "program-status.body", `[ -f "$D/running" ] || exit 66; cat "$D/answer"`)
	pk.put(t, "answer", `{"state":"idle","incarnation":"run"}`)
	p := ctor(pk.script)
	p.(interface {
		SetProgramStatusMode(runtime.ProgramStatusMode)
	}).SetProgramStatusMode(mode)
	return pk, p, p.(runtime.ProgramStatusProvider)
}

func TestProgramStatusOffReadsNothing(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, _, ps := statusProvider(t, ctor, runtime.ProgramStatusOff)
			got, err := ps.ProgramStatus("s")
			if err != nil || got.Mode != runtime.ProgramStatusOff || got.Validity != "" || pk.log(t) != "" {
				t.Fatalf("%+v %v log=%s", got, err, pk.log(t))
			}
		})
	}
}

func TestProgramStatusHandshakeFailsThenSucceeds(t *testing.T) {
	oldNow := handshakeNow
	now := time.Now()
	handshakeNow = func() time.Time { return now }
	t.Cleanup(func() { handshakeNow = oldNow })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, _, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			pk.put(t, "handshake", "malformed")
			if got, err := ps.ProgramStatus("s"); err != nil || got.Validity != "" {
				t.Fatalf("%+v %v", got, err)
			}
			pk.put(t, "handshake", `{"version":0,"capabilities":["program-status"]}`)
			if got, _ := ps.ProgramStatus("s"); got.Valid() {
				t.Error("failed handshake was not cached")
			}
			now = now.Add(handshakeRetryAfter)
			if got, err := ps.ProgramStatus("s"); err != nil || !got.Valid() {
				t.Errorf("%+v %v", got, err)
			}
			if strings.Count(pk.log(t), "OP program-status") != 1 {
				t.Error("status op sent during failed handshake")
			}
		})
	}
}

func TestProgramStatusCacheDropsOnStopAndStart(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			first, err := ps.ProgramStatus("s")
			if err != nil || !first.Valid() {
				t.Fatalf("initial reading: %+v %v", first, err)
			}
			if err := p.Stop("s"); err != nil {
				t.Fatal(err)
			}
			pk.put(t, "answer", `{"state":"working","incarnation":"run"}`)
			stopped, err := ps.ProgramStatus("s")
			if !errors.Is(err, runtime.ErrSessionNotFound) || stopped.Valid() {
				t.Errorf("stop kept cache: %+v %v", stopped, err)
			}
			if err := p.Start(t.Context(), "s", runtime.Config{AcceptStartupDialogs: new(bool)}); err != nil {
				t.Fatal(err)
			}
			started, err := ps.ProgramStatus("s")
			if err != nil || !started.Valid() || started.Nonce == first.Nonce {
				t.Errorf("start kept cache: %+v %v", started, err)
			}
			if count := strings.Count(pk.log(t), "OP program-status"); count != 3 {
				t.Errorf("status ops=%d", count)
			}
		})
	}
}

func TestProgramStatusUndeclaredRunsNoOp(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
			t.Run(fmt.Sprint(name, mode), func(t *testing.T) {
				pk, p, ps := statusProvider(t, ctor, mode)
				pk.put(t, "handshake", `{"version":0}`)
				for range 2 {
					got, err := ps.ProgramStatus("s")
					if err != nil || got.Mode != mode || got.Validity != "" {
						t.Fatalf("%+v %v", got, err)
					}
				}
				_ = p
				if got := pk.log(t); got != "OP protocol\n" {
					t.Errorf("ops %q", got)
				}
			})
		}
	}
}

func TestProgramStatusValidityTable(t *testing.T) {
	age := programStatusCacheAge
	programStatusCacheAge = 0
	t.Cleanup(func() { programStatusCacheAge = age })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, _, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			for _, tc := range []struct {
				answer string
				v      runtime.ProgramStatusValidity
			}{
				{`{"state":"","incarnation":"run"}`, runtime.ProgramStatusSilent},
				{`{"state":"idle","incarnation":"run"}`, runtime.ProgramStatusValid},
				{`{"state":"","incarnation":"run"}`, runtime.ProgramStatusCleared},
				{`{"state":"unknown","incarnation":"run","kind":"permission","app":"tool","progress":50,"children":2,"working_children":1}`, runtime.ProgramStatusSilent},
				{`{"state":"error","incarnation":"run"}`, runtime.ProgramStatusValid},
				{`{"state":"working","incarnation":"run"}`, runtime.ProgramStatusValid},
				{`{"state":"working","incarnation":"run","exited":true}`, runtime.ProgramStatusExited},
			} {
				pk.put(t, "answer", tc.answer)
				got, err := ps.ProgramStatus("s")
				if err != nil || got.Validity != tc.v {
					t.Fatalf("%s: %+v %v", tc.answer, got, err)
				}
				if got.Validity != runtime.ProgramStatusValid && (got.State != "" || got.Kind != "" || got.App != "" || got.Children != 0 || got.WorkingChildren != 0 || got.Progress != 0) {
					t.Errorf("invalid reading carries claims: %+v", got)
				}
				if got.Probed || got.ReaderVerified || got.HoldDeclared || got.SettleDeclared {
					t.Errorf("invented capture: %+v", got)
				}
				if got.Validity == runtime.ProgramStatusExited && !got.LastError {
					t.Error("exit lost the sticky error")
				}
			}
		})
	}
}

func TestProgramStatusOpExitCodes(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, tc := range []struct {
			body string
			want error
		}{
			{`exit 66`, runtime.ErrSessionNotFound},
			{`exit 2`, runtime.ErrRuntimeUnavailable},
			{`exit 1`, runtime.ErrRuntimeUnavailable},
			{`echo 'secret malformed bytes'`, runtime.ErrRuntimeUnavailable},
			{`dd if=/dev/zero bs=65537 count=1 2>/dev/null`, runtime.ErrRuntimeUnavailable},
		} {
			t.Run(fmt.Sprint(name, tc.body), func(t *testing.T) {
				pk, _, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
				pk.put(t, "program-status.body", tc.body)
				_, err := ps.ProgramStatus("s")
				if !errors.Is(err, tc.want) {
					t.Errorf("%v, want %v", err, tc.want)
				}
				if strings.Contains(fmt.Sprint(err), "secret malformed") {
					t.Error("leaked answer")
				}
			})
		}
	}
}

func TestProgramStatusCacheSharesAndCachesFailures(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			var wg sync.WaitGroup
			for range 20 {
				wg.Go(func() {
					got, err := ps.ProgramStatus("s")
					if err != nil || !got.Valid() {
						t.Errorf("%+v %v", got, err)
					}
				})
			}
			wg.Wait()
			if n := strings.Count(pk.log(t), "OP program-status"); n != 1 {
				t.Errorf("%d ops, want shared read", n)
			}
			setter := p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			})
			setter.SetProgramStatusMode(runtime.ProgramStatusShadow)
			pk.put(t, "program-status.body", `exit 1`)
			for range 3 {
				_, err := ps.ProgramStatus("s")
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Error(err)
				}
			}
			if n := strings.Count(pk.log(t), "OP program-status"); n != 2 {
				t.Errorf("cached error issued %d total ops", n)
			}
			setter.SetProgramStatusMode(runtime.ProgramStatusOn)
			pk.put(t, "program-status.body", `cat "$D/answer"`)
			got, err := ps.ProgramStatus("s")
			if err != nil || got.Mode != runtime.ProgramStatusOn || !got.Valid() {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestProgramStatusDerivedTransitionsAndReplacement(t *testing.T) {
	age := programStatusCacheAge
	programStatusCacheAge = 0
	t.Cleanup(func() { programStatusCacheAge = age })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk, p, ps := statusProvider(t, ctor, runtime.ProgramStatusOn)
			read := func(state, inc string) runtime.ProgramStatus {
				pk.put(t, "answer", fmt.Sprintf(`{"state":%q,"incarnation":%q}`, state, inc))
				got, err := ps.ProgramStatus("s")
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			idle := read("idle", "run")
			working := read("working", "run")
			same := read("working", "run")
			if same.Seq != working.Seq || same.Since != working.Since || !working.AdvancedPast(idle.Baseline()) {
				t.Fatalf("unchanged/baseline: %+v %+v", working, same)
			}
			pk.put(t, "program-status.body", `exit 1`)
			if _, err := ps.ProgramStatus("s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Error(err)
			}
			pk.put(t, "program-status.body", `[ -f "$D/running" ] || exit 66; cat "$D/answer"`)
			resumed := read("working", "run")
			if resumed.Seq != working.Seq || resumed.Since != working.Since || resumed.TurnStarts != working.TurnStarts {
				t.Errorf("failure changed history: %+v", resumed)
			}
			func() {
				oldNow := programStatusNow
				programStatusNow = func() time.Time { return working.Since.Add(-time.Second) }
				defer func() { programStatusNow = oldNow }()
				skewed := read("working", "run")
				if skewed.Since != working.Since || skewed.HoldLive(time.Time{}, programStatusNow()) {
					t.Errorf("clock step refreshed/held history: %+v", skewed)
				}
			}()
			blocked := read("blocked", "run")
			done := read("done", "run")
			if blocked.TurnStarts != 1 || done.TurnEnds != 1 || done.QuietSince.IsZero() {
				t.Errorf("turns: %+v %+v", blocked, done)
			}
			newer := read("done", "replacement")
			if newer.Nonce == done.Nonce || newer.AdvancedPast(done.Baseline()) || newer.TurnStarts != 0 {
				t.Errorf("replacement: %+v", newer)
			}
			noinc := read("working", "")
			if noinc.Nonce != "" || noinc.AdvancedPast(done.Baseline()) {
				t.Errorf("unidentified program: %+v", noinc)
			}
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOff)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			after := read("working", "run")
			if after.Nonce == working.Nonce || after.TurnStarts != 1 {
				t.Errorf("off reset: %+v", after)
			}
			var raw *Provider
			switch v := p.(type) {
			case *Provider:
				raw = v
			case *seamBackedProvider:
				raw = v.raw
			}
			second := NewProvider(pk.script, WithProgramStatusMode(runtime.ProgramStatusOn))
			other, err := second.ProgramStatus("s")
			if err != nil || other.Nonce == after.Nonce {
				t.Errorf("other instance: %+v %v", other, err)
			}
			raw.forgetProgramStatus("s")
			reset := read("working", "run")
			if reset.Nonce == after.Nonce {
				t.Error("stop/start generation reused")
			}
			if reset.HoldLive(time.Time{}, reset.Since.Add(-time.Second)) {
				t.Error("future timestamp holds")
			}
		})
	}
}

func TestStatusReaderSameSourceAndFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, sequence := range [][]idleObservation{
			{{readingIdle, sourceStatus}, {readingIdle, sourcePane}, {readingIdle, sourcePane}},
			{{readingIdle, sourcePane}, {readingIdle, sourceStatus}, {readingIdle, sourceStatus}},
		} {
			calls := 0
			reader := observationFunc(func(context.Context, string, string) (idleObservation, error) {
				result := sequence[min(calls, len(sequence)-1)]
				calls++
				return result, nil
			})
			if err := waitForIdleReadings(t.Context(), reader, time.Now, "s", "❯ ", time.Second); err != nil || calls != 3 {
				t.Errorf("calls %d error %v", calls, err)
			}
		}
		p := NewProvider("unused", WithProgramStatusMode(runtime.ProgramStatusOn))
		calls := 0
		r := &statusPreferringReader{p: p, pane: paneIdleReader{readingCarrier{screen: idleClaudePane}}, read: func(context.Context, string, time.Duration) (runtime.ProgramStatus, error) {
			calls++
			if calls == 1 {
				return runtime.ProgramStatus{Mode: runtime.ProgramStatusOn, Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle}, nil
			}
			return runtime.ProgramStatus{}, runtime.ErrRuntimeUnavailable
		}}
		if err := waitForIdleReadings(t.Context(), r, time.Now, "s", "❯ ", time.Second); err != nil || calls != 2 {
			t.Errorf("fallback: %v, %d reads", err, calls)
		}
	})
}

type observationFunc func(context.Context, string, string) (idleObservation, error)

func (f observationFunc) readIdle(ctx context.Context, name, prefix string) (idleObservation, error) {
	return f(ctx, name, prefix)
}
