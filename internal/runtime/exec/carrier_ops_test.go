package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// These behavioral tests use only symbols present at the PR 1 base, so the
// same files can prove assertion failures there without a compile failure.
const opsIdleCaps = `"carrier.ops","report-activity","report-attachment","idle-boundary"`

var opsConstructors = map[string]func(string) runtime.Provider{
	"raw":   func(s string) runtime.Provider { return NewProvider(s) },
	"seams": NewSeamBacked,
}

type opsPack struct{ dir, script string }

func newOpsPack(t *testing.T, caps string) opsPack {
	t.Helper()
	dir := t.TempDir()
	pk := opsPack{dir: dir}
	pk.put(t, "handshake", `{"version":0,"capabilities":[`+caps+`]}`)
	pk.put(t, "screen", idleClaudePane)
	pk.put(t, "running", "true")
	pk.script = writeScript(t, dir, fmt.Sprintf(`
D=%q
printf 'OP %%s\n' "$*" >> "$D/log"
if [ -f "$D/$1.body" ]; then . "$D/$1.body"; exit; fi
case "$1" in
 protocol) cat "$D/handshake" ;;
 exec) command=$(cat); printf 'EXEC %%s\n' "$command" >> "$D/log"; [ ! -f "$D/exec.code" ] || exit "$(cat "$D/exec.code")" ;;
 nudge) cat > "$D/nudge.stdin" ;;
 peek) cat "$D/screen" ;;
 send-keys|interrupt|clear-scrollback) ;;
 is-running) [ -f "$D/running" ] && echo true || echo false ;;
 observe) if [ -f "$D/running" ]; then echo '{"state":"running","object":{"id":"ops-box","created":"2026-11-07T00:00:00Z"},"agent":"alive","identity":{}}'; else echo '{"state":"absent"}'; fi ;;
 start) cat > "$D/start.stdin"; echo true > "$D/running" ;;
 stop) rm -f "$D/running" ;;
 set-meta) cat > "$D/meta.$3" ;;
 get-meta) [ ! -f "$D/meta.$3" ] || cat "$D/meta.$3" ;;
 is-attached) echo false ;;
 get-last-activity) echo 2026-11-07T00:00:00Z ;;
 *) exit 2 ;;
esac
`, dir))
	return pk
}

func (pk opsPack) put(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(pk.dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (pk opsPack) log(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pk.dir, "log"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func tmuxInvocations(log string) []string {
	var found []string
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimPrefix(strings.TrimPrefix(line, "EXEC "), "OP ")
		if line == "tmux" || line == "'tmux'" || strings.HasPrefix(line, "tmux ") || strings.HasPrefix(line, "'tmux' ") {
			found = append(found, line)
		}
	}
	return found
}

func (pk opsPack) noTmux(t *testing.T) {
	t.Helper()
	if got := tmuxInvocations(pk.log(t)); len(got) > 0 {
		t.Errorf("unexpected tmux commands: %q", got)
	}
}

func TestTmuxDetectorCatchesAnInjectedInvocation(t *testing.T) {
	pk := newOpsPack(t, `"carrier.ops"`)
	_, _, err := NewProvider(pk.script).Exec(context.Background(), "s", []string{"tmux", "has-session"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tmuxInvocations(pk.log(t))) != 1 {
		t.Fatalf("detector missed injected tmux: %s", pk.log(t))
	}
}

func TestOpsCarrierDrivesThroughOps(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, `"carrier.ops"`)
			p := ctor(pk.script)
			if err := p.Nudge("s", []runtime.ContentBlock{{Type: "text", Text: "hello"}}); err != nil {
				t.Fatal(err)
			}
			if got, err := p.Peek("s", 17); err != nil || got != strings.TrimRight(idleClaudePane, "\n") {
				t.Fatalf("Peek = %q, %v", got, err)
			}
			for _, err := range []error{p.SendKeys("s", "Enter", "Down"), p.Interrupt("s"), p.ClearScrollback("s")} {
				if err != nil {
					t.Fatal(err)
				}
			}
			log := pk.log(t)
			for _, op := range []string{"nudge s", "peek s 17", "send-keys s Enter Down", "interrupt s", "clear-scrollback s"} {
				if !strings.Contains(log, "OP "+op+"\n") {
					t.Errorf("missing %q in %s", op, log)
				}
			}
			b, err := os.ReadFile(filepath.Join(pk.dir, "nudge.stdin"))
			if err != nil || string(b) != "hello" {
				t.Errorf("nudge stdin = %q, %v", b, err)
			}
			if strings.Contains(log, "OP exec ") {
				t.Error("dedicated verbs used exec")
			}
			pk.noTmux(t)
		})
	}
}

func TestOpsCarrierNeverFallsBack(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, `"carrier.ops"`)
			pk.put(t, "nudge.body", "echo refused >&2; exit 1")
			if err := ctor(pk.script).Nudge("s", []runtime.ContentBlock{{Type: "text", Text: "x"}}); err == nil {
				t.Error("failed nudge reported success")
			}
			if strings.Count(pk.log(t), "OP nudge s\n") != 1 {
				t.Errorf("nudge not sent exactly once: %s", pk.log(t))
			}
			pk.noTmux(t)
		})
	}
}

func TestOpsCarrierRefusesFencedInput(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, extra := range []string{"", `,"input-fence"`} {
			t.Run(name+extra, func(t *testing.T) {
				pk := newOpsPack(t, `"carrier.ops","proc.exec"`+extra)
				p := ctor(pk.script)
				f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "t"}
				fp := p.(runtime.FencedInputProvider)
				if got := fp.InputFencing("s"); got != runtime.InputFencingNone {
					t.Errorf("InputFencing = %v", got)
				}
				ctx := context.Background()
				for _, err := range []error{fp.NudgeFenced(ctx, "s", nil, runtime.FencedNudge{Fence: f}), fp.SendKeysFenced(ctx, "s", f, "Enter"), fp.InterruptFenced(ctx, "s", f), p.(runtime.DialogProvider).DismissKnownDialogs(runtime.WithInputFence(ctx, f), "s", time.Millisecond)} {
					if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
						t.Errorf("fenced input = %v", err)
					}
				}
				for _, line := range strings.Split(strings.TrimSpace(pk.log(t)), "\n") {
					if line != "OP protocol" {
						t.Errorf("refused fence sent an op: %s", line)
					}
				}
				pk.noTmux(t)
			})
		}
	}
}

func TestOpsCarrierWeldedStart(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps+`,"proc.exec","proc.provision","input-fence"`)
			p := ctor(pk.script)
			var raw *Provider
			if name == "seams" {
				raw = p.(*seamBackedProvider).raw
			} else {
				raw = p.(*Provider)
			}
			_, transport := raw.Seams()
			if transport.Capabilities().SeparableLaunch || transport.Name() != "ops" {
				t.Errorf("transport = %s, %+v", transport.Name(), transport.Capabilities())
			}
			cfg := runtime.Config{Command: "sleep 300", AcceptStartupDialogs: new(bool), Env: map[string]string{"GC_RUNTIME_EPOCH": "1", "GC_CONTINUATION_EPOCH": "1", "GC_INSTANCE_TOKEN": "t"}}
			if err := p.Start(context.Background(), "s", cfg); err != nil {
				t.Fatal(err)
			}
			if err := p.(runtime.RelaunchProvider).Relaunch(context.Background(), "s", cfg); err != nil {
				t.Fatal(err)
			}
			log := pk.log(t)
			if strings.Count(log, "OP start s\n") != 2 || strings.Contains(log, "OP provision ") {
				t.Errorf("not welded start/relaunch: %s", log)
			}
			if strings.Count(log, "OP set-meta s GC_READY_PROMPT_PREFIX\n") != 2 {
				t.Errorf("prompt not published once per start: %s", log)
			}
			pk.noTmux(t)
		})
	}
}

func TestOpsCarrierV2SeamBackedPeek(t *testing.T) {
	pk := newOpsPack(t, `"carrier.ops","observe.v1","report-attachment"`)
	p := NewSeamBacked(pk.script)
	if got, err := p.Peek("s", 120); err != nil || got == "" {
		t.Fatalf("running Peek = %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(pk.dir, "running")); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Peek("s", 120); err != nil || got != "" {
		t.Fatalf("absent Peek = %q, %v", got, err)
	}
	if strings.Count(pk.log(t), "OP peek s 120\n") != 1 {
		t.Errorf("unexpected peek count: %s", pk.log(t))
	}
	pk.noTmux(t)
}

func TestOpsCarrierReadyPromptAcrossInstances(t *testing.T) {
	fastIdlePoll(t)
	for name, ctor := range opsConstructors {
		for _, prefix := range []string{"", "› "} {
			t.Run(name+prefix, func(t *testing.T) {
				pk := newOpsPack(t, opsIdleCaps)
				p := ctor(pk.script)
				if err := p.Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: prefix, AcceptStartupDialogs: new(bool)}); err != nil {
					t.Fatal(err)
				}
				effective := prefix
				if effective == "" {
					effective = "❯ "
				}
				pk.put(t, "screen", "previous output\n\n"+effective+"\n")
				p = ctor(pk.script)
				if err := p.(runtime.IdleWaitProvider).WaitForIdle(context.Background(), "s", time.Second); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(pk.log(t), "OP get-meta s GC_READY_PROMPT_PREFIX\n") {
					t.Error("no metadata recovery")
				}
				pk.noTmux(t)
			})
		}
		for _, mode := range []string{"missing", "error", "unsupported-v2"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				caps := opsIdleCaps
				if mode == "unsupported-v2" {
					caps += `,"observe.v1"`
				}
				pk := newOpsPack(t, caps)
				if mode == "error" {
					pk.put(t, "get-meta.body", "exit 1")
				}
				if mode == "unsupported-v2" {
					pk.put(t, "get-meta.body", "exit 2")
				}
				p := ctor(pk.script).(runtime.IdleWaitProvider)
				for range 2 {
					if err := p.WaitForIdle(context.Background(), "s", time.Second); err != nil {
						t.Fatal(err)
					}
				}
				if strings.Count(pk.log(t), "OP get-meta s GC_READY_PROMPT_PREFIX\n") != 2 {
					t.Errorf("missing or failed prefix was cached: %s", pk.log(t))
				}
				pk.noTmux(t)
			})
		}
	}
}

func TestOpsCarrierReadyPromptCRLF(t *testing.T) {
	fastIdlePoll(t)
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, opsIdleCaps)
			pk.put(t, "meta.GC_READY_PROMPT_PREFIX", "› \r\n")
			pk.put(t, "screen", "previous output\n\n› \n")
			p := ctor(pk.script)
			var raw *Provider
			if name == "seams" {
				raw = p.(*seamBackedProvider).raw
			} else {
				raw = p.(*Provider)
			}
			prefix, found, err := raw.readPublishedReadyPrompt(context.Background(), "s")
			if err != nil || !found || prefix != "› " {
				t.Errorf("recovered prefix = %q, %t, %v", prefix, found, err)
			}
			if err := p.(runtime.IdleWaitProvider).WaitForIdle(context.Background(), "s", 200*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			pk.noTmux(t)
		})
	}
}

func TestOpsCarrierIdleReadings(t *testing.T) {
	fastIdlePoll(t)
	for name, ctor := range opsConstructors {
		for _, mode := range []string{"idle", "busy", "unclassifiable", "capture-error", "hung", "cancel"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				pk := newOpsPack(t, opsIdleCaps)
				switch mode {
				case "busy":
					pk.put(t, "screen", busyClaudePane)
				case "unclassifiable":
					pk.put(t, "screen", "not a prompt\n")
				case "capture-error":
					pk.put(t, "peek.body", "exit 1")
				case "hung", "cancel":
					pk.put(t, "peek.body", `echo started > "$D/peek.started"; exec sleep 30`)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancel" {
					go func() {
						for {
							if _, err := os.Stat(filepath.Join(pk.dir, "peek.started")); err == nil {
								cancel()
								return
							}
							select {
							case <-ctx.Done():
								return
							case <-time.After(time.Millisecond):
							}
						}
					}()
				}
				start := time.Now()
				err := ctor(pk.script).(runtime.IdleWaitProvider).WaitForIdle(ctx, "s", 200*time.Millisecond)
				switch mode {
				case "idle":
					if err != nil {
						t.Fatal(err)
					}
				case "cancel":
					if !errors.Is(err, context.Canceled) {
						t.Errorf("cancel = %v", err)
					}
				default:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("timeout = %v", err)
					}
					if errors.Is(err, runtime.ErrWaitUnobserved) != (mode != "busy") {
						t.Errorf("unobserved for %s = %v", mode, err)
					}
				}
				if time.Since(start) > time.Second {
					t.Errorf("wait exceeded hard bound: %s", time.Since(start))
				}
				pk.noTmux(t)
			})
		}
	}
}

func TestOpsCarrierSleepCapability(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, missing := range []string{"", "report-activity", "report-attachment", "idle-boundary", "carrier.ops", "malformed", "with-proc.exec"} {
			t.Run(name+"/"+missing, func(t *testing.T) {
				caps := []string{"carrier.ops", "report-activity", "report-attachment", "idle-boundary"}
				if missing == "with-proc.exec" {
					caps = append(caps, "proc.exec")
				}
				var kept []string
				for _, cap := range caps {
					if cap != missing {
						kept = append(kept, cap)
					}
				}
				b, _ := json.Marshal(kept)
				pk := newOpsPack(t, strings.TrimSuffix(strings.TrimPrefix(string(b), "["), "]"))
				if missing == "malformed" {
					pk.put(t, "handshake", "bad json")
				}
				p := ctor(pk.script)
				want := runtime.SessionSleepCapabilityTimedOnly
				budget := time.Duration(0)
				if missing == "" || missing == "with-proc.exec" {
					want = runtime.SessionSleepCapabilityFull
					budget = 5 * time.Second
				}
				if got := p.(runtime.SleepCapabilityProvider).SleepCapability("s"); got != want {
					t.Errorf("sleep = %s, want %s", got, want)
				}
				if got := p.(runtime.IdleProbeBudgetProvider).IdleProbeBudget(); got != budget {
					t.Errorf("budget = %s, want %s", got, budget)
				}
				pk.noTmux(t)
			})
		}
	}
}

func TestCarrierSelectionTable(t *testing.T) {
	for _, caps := range []string{"", `"proc.exec"`, `"carrier.future"`, `"carrier.ops"`, `"carrier.ops","carrier.ops"`, `"carrier.ops","carrier.tmux"`, `"carrier.ops","proc.exec","input-fence","proc.provision"`} {
		t.Run(caps, func(t *testing.T) {
			pk := newOpsPack(t, caps)
			p := NewProvider(pk.script)
			if err := p.Nudge("s", []runtime.ContentBlock{{Type: "text", Text: "x"}}); err != nil {
				t.Fatal(err)
			}
			ops := strings.Contains(caps, `"carrier.ops"`)
			if strings.Contains(pk.log(t), "OP nudge s\n") != ops {
				t.Errorf("wrong selection: %s", pk.log(t))
			}
			if ops {
				pk.noTmux(t)
			} else if len(tmuxInvocations(pk.log(t))) == 0 {
				t.Error("undeclared pack did not use tmux")
			}
		})
	}
}

func TestCarrierSelectionAfterHandshakeRetry(t *testing.T) {
	old := handshakeNow
	now := time.Now()
	handshakeNow = func() time.Time { return now }
	t.Cleanup(func() { handshakeNow = old })
	pk := newOpsPack(t, `"carrier.ops"`)
	pk.put(t, "handshake", "bad json")
	p := NewProvider(pk.script)
	content := []runtime.ContentBlock{{Type: "text", Text: "x"}}
	if err := p.Nudge("s", content); err != nil {
		t.Fatal(err)
	}
	if len(tmuxInvocations(pk.log(t))) == 0 {
		t.Fatal("failure window did not retain legacy path")
	}
	pk.put(t, "handshake", `{"version":0,"capabilities":["carrier.ops"]}`)
	pk.put(t, "log", "")
	if err := p.Nudge("s", content); err != nil {
		t.Fatal(err)
	}
	if len(tmuxInvocations(pk.log(t))) == 0 || strings.Contains(pk.log(t), "OP nudge s\n") {
		t.Errorf("failure cache did not retain tmux: %s", pk.log(t))
	}
	if strings.Contains(pk.log(t), "OP protocol\n") {
		t.Errorf("handshake retried before failure cache expired: %s", pk.log(t))
	}
	now = now.Add(HandshakeRetryAfter + time.Second)
	pk.put(t, "log", "")
	for range 2 {
		if err := p.Nudge("s", content); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(pk.log(t), "OP nudge s\n") != 2 {
		t.Errorf("retry did not select ops: %s", pk.log(t))
	}
	pk.noTmux(t)
}

func TestUndeclaredCarrierCompatibility(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, mode := range []string{"success", "unsupported", "declared-exit-2", "transport"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				caps := ""
				if mode == "declared-exit-2" {
					caps = `"proc.exec"`
				}
				pk := newOpsPack(t, caps)
				if mode == "unsupported" || mode == "declared-exit-2" {
					pk.put(t, "exec.code", "2")
				}
				if mode == "transport" {
					pk.put(t, "exec.body", `printf 'OP exec transport\n' >> "$D/log"; exec sleep 30`)
				}
				p := ctor(pk.script)
				if mode == "transport" {
					if raw, ok := p.(*Provider); ok {
						raw.timeout = 100 * time.Millisecond
					} else {
						p.(*seamBackedProvider).raw.timeout = 100 * time.Millisecond
					}
				}
				err := p.Nudge("s", []runtime.ContentBlock{{Type: "text", Text: "x"}})
				if (err != nil) != (mode == "transport") {
					t.Errorf("Nudge = %v", err)
				}
				if mode == "transport" {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("transport error = %v, want deadline exceeded", err)
					}
					if !strings.Contains(pk.log(t), "OP exec transport\n") {
						t.Errorf("exec was not attempted: %s", pk.log(t))
					}
				}
				if strings.Contains(pk.log(t), "OP nudge s\n") != (mode == "unsupported") {
					t.Errorf("fallback changed: %s", pk.log(t))
				}
				if mode != "transport" && len(tmuxInvocations(pk.log(t))) == 0 {
					t.Error("legacy path did not send tmux")
				}
			})
		}
	}
}
