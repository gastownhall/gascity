package runtime_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/k8s"
	"github.com/gastownhall/gascity/internal/runtime/ssh"
	"github.com/gastownhall/gascity/internal/runtime/subprocess"
	"github.com/gastownhall/gascity/internal/runtime/t3bridge"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// optionalInterfaces are the runtime package's optional provider
// interfaces, by name. TestOptionalInterfacesAreListed keeps it complete.
var optionalInterfaces = map[string]reflect.Type{
	"AttachmentObserverWithError":     reflect.TypeFor[runtime.AttachmentObserverWithError](),
	"BackgroundWorkStopper":           reflect.TypeFor[runtime.BackgroundWorkStopper](),
	"BackendListingProvider":          reflect.TypeFor[runtime.BackendListingProvider](),
	"BackendsProvider":                reflect.TypeFor[runtime.BackendsProvider](),
	"ConditionalProcessTableScanner":  reflect.TypeFor[runtime.ConditionalProcessTableScanner](),
	"DeadRuntimeSessionChecker":       reflect.TypeFor[runtime.DeadRuntimeSessionChecker](),
	"DialogProvider":                  reflect.TypeFor[runtime.DialogProvider](),
	"EnvironmentBatchProvider":        reflect.TypeFor[runtime.EnvironmentBatchProvider](),
	"ExecProvider":                    reflect.TypeFor[runtime.ExecProvider](),
	"FreshByConstruction":             reflect.TypeFor[runtime.FreshByConstruction](),
	"FencedInputProvider":             reflect.TypeFor[runtime.FencedInputProvider](),
	"FencedInteractionProvider":       reflect.TypeFor[runtime.FencedInteractionProvider](),
	"FreshLivenessObserver":           reflect.TypeFor[runtime.FreshLivenessObserver](),
	"IdentitySidecarProvider":         reflect.TypeFor[runtime.IdentitySidecarProvider](),
	"IdleProbeBudgetProvider":         reflect.TypeFor[runtime.IdleProbeBudgetProvider](),
	"IdleSnapshotProvider":            reflect.TypeFor[runtime.IdleSnapshotProvider](),
	"IdleWaitProvider":                reflect.TypeFor[runtime.IdleWaitProvider](),
	"ImmediateNudgeProvider":          reflect.TypeFor[runtime.ImmediateNudgeProvider](),
	"InputClearProvider":              reflect.TypeFor[runtime.InputClearProvider](),
	"InteractionProvider":             reflect.TypeFor[runtime.InteractionProvider](),
	"InterruptBoundaryWaitProvider":   reflect.TypeFor[runtime.InterruptBoundaryWaitProvider](),
	"InterruptedTurnResetProvider":    reflect.TypeFor[runtime.InterruptedTurnResetProvider](),
	"InventoryProvider":               reflect.TypeFor[runtime.InventoryProvider](),
	"KeyedNudgeProvider":              reflect.TypeFor[runtime.KeyedNudgeProvider](),
	"ListingAttestation":              reflect.TypeFor[runtime.ListingAttestation](),
	"LocalTranscriptProvider":         reflect.TypeFor[runtime.LocalTranscriptProvider](),
	"LivenessObserver":                reflect.TypeFor[runtime.LivenessObserver](),
	"LivenessObserverWithError":       reflect.TypeFor[runtime.LivenessObserverWithError](),
	"ProcessTableScanner":             reflect.TypeFor[runtime.ProcessTableScanner](),
	"RelaunchProvider":                reflect.TypeFor[runtime.RelaunchProvider](),
	"Router":                          reflect.TypeFor[runtime.Router](),
	"ServerDeathConfirmer":            reflect.TypeFor[runtime.ServerDeathConfirmer](),
	"ServerLifecycleProvider":         reflect.TypeFor[runtime.ServerLifecycleProvider](),
	"SessionBindingProvider":          reflect.TypeFor[runtime.SessionBindingProvider](),
	"SessionEventProvider":            reflect.TypeFor[runtime.SessionEventProvider](),
	"SessionObjectKiller":             reflect.TypeFor[runtime.SessionObjectKiller](),
	"SessionRosterProvider":           reflect.TypeFor[runtime.SessionRosterProvider](),
	"SleepCapabilityProvider":         reflect.TypeFor[runtime.SleepCapabilityProvider](),
	"SteerCapabilityProvider":         reflect.TypeFor[runtime.SteerCapabilityProvider](),
	"TransportCapabilityProvider":     reflect.TypeFor[runtime.TransportCapabilityProvider](),
	"TurnEventProvider":               reflect.TypeFor[runtime.TurnEventProvider](),
	"TurnFenceProvider":               reflect.TypeFor[runtime.TurnFenceProvider](),
	"TurnInterruptCapabilityProvider": reflect.TypeFor[runtime.TurnInterruptCapabilityProvider](),
	"TurnObservationProvider":         reflect.TypeFor[runtime.TurnObservationProvider](),
	"TurnRunningProvider":             reflect.TypeFor[runtime.TurnRunningProvider](),
	"UnattendedSessionStopper":        reflect.TypeFor[runtime.UnattendedSessionStopper](),
}

// notOptional are the package's exported interfaces that are not optional
// provider capabilities.
var notOptional = []string{"Attachment", "Carrier", "MetaStore", "Place", "Provider", "ProfileBackend", "Runtime", "Transport"}

// implemented lists the optional interfaces p implements, sorted.
func implemented(p any) []string {
	var got []string
	for name, it := range optionalInterfaces {
		if reflect.TypeOf(p).Implements(it) {
			got = append(got, name)
		}
	}
	slices.Sort(got)
	return got
}

// Kills an optional interface added to the package and left off the list,
// which the profile and forwarding tests would then not check.
func TestOptionalInterfacesAreListed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range g.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if _, iface := ts.Type.(*ast.InterfaceType); !iface {
					continue
				}
				if _, listed := optionalInterfaces[ts.Name.Name]; !listed && !slices.Contains(notOptional, ts.Name.Name) {
					t.Errorf("%s: interface %s is neither an optional interface nor listed as not one", file, ts.Name.Name)
				}
			}
		}
	}
}

// productionBackends builds each profiled backend's provider as the runtime
// registry does (cmd/gc/runtime_registry.go): the seam-backed provider, or
// herdr's own. tmux's is its raw provider, which the seam-backed one embeds
// and adds no method to (tmux TestSeamBackedCarriesTheRawSurface):
// constructing it is a tmux test resource.
func productionBackends(t *testing.T) map[runtime.Profile]runtime.Provider {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
users: [{name: u, user: {}}]
current-context: c
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	kube, err := k8s.NewSeamBacked()
	if err != nil {
		t.Fatal(err)
	}
	ep, err := ssh.ParseEndpoint("u@h")
	if err != nil {
		t.Fatal(err)
	}
	return map[runtime.Profile]runtime.Provider{
		runtime.ProfileTmux:       &tmux.Provider{},
		runtime.ProfileACP:        acp.NewSeamBacked(acp.Config{}),
		runtime.ProfileSubprocess: subprocess.NewSeamBacked(),
		runtime.ProfileHerdr:      herdr.New("city", t.TempDir(), t.TempDir(), 0, 0),
		runtime.ProfileK8s:        kube,
		runtime.ProfileExec:       exec.NewSeamBacked(execConnectionPack(t)),
		runtime.ProfileSSH:        ssh.NewSeamBacked(ep),
		runtime.ProfileT3Bridge:   t3bridge.NewSeamBacked(),
	}
}

// execConnectionPack is an exec pack that declares the exec connection and
// input fencing (proc.exec, input-fence) alone, the tmux-in-a-box shape the
// exec profile models, and answers every other op as /bin/true does.
func execConnectionPack(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "pack")
	body := "#!/bin/sh\n[ \"$1\" = protocol ] && echo '{\"version\":0,\"capabilities\":[\"proc.exec\",\"input-fence\"]}'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// Kills a profile that drifts from its backend: one optional interface more
// or fewer than the provider production builds, other capabilities, another
// answer from a fixed trait (sleep capability, fresh reads, an identity
// sidecar, a transport, an idle-proof budget, a turn-interrupt mode, input
// fencing, response fencing, local transcripts, a steer mode), or no
// backend checked.
func TestFakeProfilesMatchTheirBackends(t *testing.T) {
	backends := productionBackends(t)
	for p := runtime.ProfileTmux; p <= runtime.ProfileT3Bridge; p++ {
		backend, ok := backends[p]
		if !ok {
			t.Errorf("profile %d: no production backend to match", p)
			continue
		}
		fake := runtime.NewFakeProfile(p, runtime.FullFake{Fake: runtime.NewFake()})
		if got, want := implemented(fake), implemented(backend); !slices.Equal(got, want) {
			t.Errorf("profile %d (%T) implements %v; its backend implements %v", p, backend, got, want)
		}
		if got, want := fake.Capabilities(), backend.Capabilities(); got != want {
			t.Errorf("profile %d (%T) reports %+v; its backend reports %+v", p, backend, got, want)
		}
		if got, want := traits(fake), traits(backend); got != want {
			t.Errorf("profile %s (%T) answers %s; its backend answers %s", p, backend, got, want)
		}
	}
}

// traits are p's answers to the optional interfaces that report a backend's
// fixed traits, "-" for one it does not implement.
func traits(p runtime.Provider) string {
	out := []string{"-", "-", "-", "-", "-", "-", "-", "-", "-", "-"}
	if s, ok := p.(runtime.SleepCapabilityProvider); ok {
		out[0] = string(s.SleepCapability("s"))
	}
	if f, ok := p.(runtime.FreshByConstruction); ok {
		out[1] = strconv.FormatBool(f.LivenessReadsFresh())
	}
	if f, ok := p.(runtime.IdentitySidecarProvider); ok {
		out[2] = strconv.FormatBool(f.LocalIdentitySidecar())
	}
	if f, ok := p.(runtime.TransportCapabilityProvider); ok {
		out[3] = strconv.FormatBool(f.SupportsTransport("acp")) + "/" + strconv.FormatBool(f.SupportsTransport("tmux"))
	}
	if f, ok := p.(runtime.IdleProbeBudgetProvider); ok {
		out[4] = f.IdleProbeBudget().String()
	}
	if f, ok := p.(runtime.TurnInterruptCapabilityProvider); ok {
		out[5] = string(f.TurnInterruptMode("s"))
	}
	if f, ok := p.(runtime.FencedInputProvider); ok {
		out[6] = string(f.InputFencing("s"))
	}
	if f, ok := p.(runtime.LocalTranscriptProvider); ok {
		out[7] = strconv.FormatBool(f.LocalTranscripts())
	}
	if f, ok := p.(runtime.SteerCapabilityProvider); ok {
		out[8] = string(f.SteerMode("s"))
	}
	if f, ok := p.(runtime.FencedInteractionProvider); ok {
		out[9] = string(f.ResponseFencing("s"))
	}
	return strings.Join(out, " ")
}

// Kills a full fake whose reads do not follow its sessions: liveness,
// zombies, environment (failing as a GetMeta would), a zombie kill and a
// broken fake.
func TestFullFakeAnswersFromItsState(t *testing.T) {
	f := runtime.NewFake()
	b := runtime.FullFake{Fake: f}
	if l, err := b.ObserveLivenessWithError("s", nil); err != nil || l.Running {
		t.Fatalf("unstarted: %+v, %v; want not running", l, err)
	}
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := f.SetMeta("s", "GC_SESSION_ID", "id"); err != nil {
		t.Fatal(err)
	}
	if l := b.ObserveLiveness("s", nil); !l.Running || !l.Alive {
		t.Fatalf("started: %+v, want running and alive", l)
	}
	f.Zombies["s"] = true
	if l, _ := b.ObserveLivenessSince("s", nil, time.Time{}); !l.Running || l.Alive {
		t.Fatalf("zombie: %+v, want running, not alive", l)
	}
	if env, err := b.GetAllEnvironment("s"); err != nil || env["GC_SESSION_ID"] != "id" {
		t.Fatalf("environment %v, %v; want the session's metadata", env, err)
	}
	if r, err := b.KillZombieObject("s", "$1", "1", "2"); r != runtime.SessionObjectKilled || err != nil || f.IsRunning("s") {
		t.Fatalf("zombie kill %v, %v; want killed and stopped", r, err)
	}
	if err := f.Start(t.Context(), "e", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.GetMetaErrors = map[string]map[string]error{"e": {"GC_SESSION_ID": runtime.ErrRuntimeUnavailable}}
	if _, err := b.GetAllEnvironment("e"); err == nil {
		t.Fatal("an environment read that a GetMeta error fails succeeded")
	}
	broken := runtime.FullFake{Fake: runtime.NewFailFake()}
	if _, err := broken.ObserveLivenessWithError("s", nil); err == nil {
		t.Fatal("a broken fake's error-bearing read succeeded")
	}
}

// A profiled tmux must preserve composer clearing, including its caller's
// cancellation context and the backend's refusal to clear protected input.
func TestTmuxProfileForwardsInputClear(t *testing.T) {
	for _, result := range []error{nil, runtime.ErrInputClearSkipped} {
		b := &profileInputClearSpy{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, result: result}
		p := runtime.NewFakeProfile(runtime.ProfileTmux, b)
		clearer, ok := p.(runtime.InputClearProvider)
		if !ok {
			t.Fatal("tmux profile lacks InputClearProvider")
		}
		ctx := t.Context()
		const window = 2 * time.Second
		if err := clearer.ClearInput(ctx, "session", window); !errors.Is(err, result) {
			t.Fatalf("ClearInput error = %v, want %v", err, result)
		}
		if b.calls != 1 || b.ctx != ctx || b.name != "session" || b.window != window {
			t.Fatalf("ClearInput did not forward context, session and restore window: %+v", b)
		}
	}
}

type profileInputClearSpy struct {
	runtime.FullFake
	ctx    context.Context
	name   string
	window time.Duration
	result error
	calls  int
}

func (b *profileInputClearSpy) ClearInput(ctx context.Context, name string, window time.Duration) error {
	b.ctx, b.name, b.window = ctx, name, window
	b.calls++
	return b.result
}

// turnFake is a FullFake whose turns test scripts set.
func turnFake() (*runtime.Fake, runtime.TurnFenceProvider) {
	f := runtime.NewFake()
	return f, runtime.FullFake{Fake: f}
}

// turnCalls are f's recorded turn-fence calls as "Method Key Value".
func turnCalls(f *runtime.Fake) []string {
	var out []string
	for _, c := range f.SnapshotCalls() {
		switch c.Method {
		case "ObserveTurn", "InterruptTurn", "WaitTurnEnded":
			out = append(out, strings.TrimSpace(c.Method+" "+c.Key+" "+c.Value))
		}
	}
	return out
}

// Kills a script that does not pop, does not repeat its last entry, or reads
// an unscripted session as running; and read errors that apply out of order or
// consume a script entry.
func TestFullFakeTurnScriptPopsAndRepeatsItsLastEntry(t *testing.T) {
	f, tf := turnFake()
	f.TurnScripts = map[string][]string{"s": {"t1", "", "t2"}}
	f.TurnReadErrors = map[string][]error{"s": {nil, runtime.ErrRuntimeUnavailable, nil}}
	ctx := t.Context()
	var got []string
	for range 5 {
		st, err := tf.ObserveTurn(ctx, "s")
		switch {
		case err != nil:
			got = append(got, "err:"+err.Error())
		default:
			got = append(got, st.TurnID)
		}
	}
	want := []string{"t1", "err:" + runtime.ErrRuntimeUnavailable.Error(), "", "t2", "t2"}
	if !slices.Equal(got, want) {
		t.Fatalf("reads = %q, want %q", got, want)
	}
	if st, err := tf.ObserveTurn(ctx, "unscripted"); err != nil || st.Active() {
		t.Fatalf("unscripted read = %+v, %v; want no turn", st, err)
	}
	if calls := turnCalls(f); len(calls) != 6 || calls[0] != "ObserveTurn" {
		t.Fatalf("calls = %q, want six bare ObserveTurn calls", calls)
	}
}

// Kills an InterruptTurn that interrupts a turn it did not read, that hides
// what it read, or that records the wrong effect.
func TestFullFakeInterruptTurnFencesOnItsRead(t *testing.T) {
	f, tf := turnFake()
	f.TurnScripts = map[string][]string{"s": {"t2", "", "t1", "t1"}}
	ctx := t.Context()

	_, err := tf.InterruptTurn(ctx, "s", "t1")
	var nc *runtime.TurnNotCurrentError
	if !errors.As(err, &nc) || nc.Current.TurnID != "t2" || nc.TurnID != "t1" || nc.Session != "s" {
		t.Fatalf("mismatch = %v, want not-current reading t2", err)
	}
	if _, err := tf.InterruptTurn(ctx, "s", "t1"); !errors.As(err, &nc) || nc.Current.Active() {
		t.Fatalf("idle = %v, want not-current reading no turn", err)
	}
	ack, err := tf.InterruptTurn(ctx, "s", "t1")
	if err != nil || ack.Fenced {
		t.Fatalf("match = %+v, %v; want sent, unfenced", ack, err)
	}
	f.FencedTurnInterrupts = true
	if ack, err := tf.InterruptTurn(ctx, "s", "t1"); err != nil || !ack.Fenced {
		t.Fatalf("fenced match = %+v, %v; want sent, fenced", ack, err)
	}
	boom := errors.New("boom")
	f.InterruptTurnErrors = map[string]error{"s": boom}
	if _, err := tf.InterruptTurn(ctx, "s", "t1"); !errors.Is(err, boom) {
		t.Fatalf("failing match = %v, want boom", err)
	}
	want := []string{"InterruptTurn t1 not_current", "InterruptTurn t1 not_current", "InterruptTurn t1 sent", "InterruptTurn t1 sent", "InterruptTurn t1 error"}
	if got := turnCalls(f); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// Kills a WaitTurnEnded that sleeps, stops before the turn changes, misses a
// replacement turn, or waits forever on a script that never ends the turn.
func TestFullFakeWaitTurnEndedReadsTheScript(t *testing.T) {
	f, tf := turnFake()
	ctx := t.Context()
	f.TurnScripts = map[string][]string{"ends": {"t1", "t1", ""}, "replaced": {"t1", "t2"}, "stuck": {"t1", "t1"}, "flaky": {"t1", ""}}
	f.TurnReadErrors = map[string][]error{"flaky": {runtime.ErrRuntimeUnavailable}}

	if st, err := tf.WaitTurnEnded(ctx, "ends", "t1", time.Hour); err != nil || st.Active() {
		t.Fatalf("ends = %+v, %v; want no turn", st, err)
	}
	if st, err := tf.WaitTurnEnded(ctx, "replaced", "t1", time.Hour); err != nil || st.TurnID != "t2" {
		t.Fatalf("replaced = %+v, %v; want t2", st, err)
	}
	st, err := tf.WaitTurnEnded(ctx, "stuck", "t1", time.Hour)
	if !errors.Is(err, runtime.ErrTurnNotEnded) || st.TurnID != "t1" {
		t.Fatalf("stuck = %+v, %v; want ErrTurnNotEnded with t1", st, err)
	}
	if st, err := tf.WaitTurnEnded(ctx, "flaky", "t1", time.Hour); err != nil || st.Active() {
		t.Fatalf("flaky = %+v, %v; want the read after the outage", st, err)
	}
	want := []string{"WaitTurnEnded t1", "WaitTurnEnded t1", "WaitTurnEnded t1", "WaitTurnEnded t1"}
	if got := turnCalls(f); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := tf.WaitTurnEnded(canceled, "stuck", "t1", time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v, want context.Canceled", err)
	}
}

// Kills a broken fake that reads a turn, and a plain Fake that implements the
// capability, which would move every protocol test off its path.
func TestFullFakeTurnFenceWhenBrokenAndPlainFakeLacksIt(t *testing.T) {
	broken := runtime.FullFake{Fake: runtime.NewFailFake()}
	broken.TurnScripts = map[string][]string{"s": {"t1"}}
	ctx := t.Context()
	if _, err := broken.ObserveTurn(ctx, "s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("broken ObserveTurn = %v, want ErrRuntimeUnavailable", err)
	}
	if _, err := broken.InterruptTurn(ctx, "s", "t1"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("broken InterruptTurn = %v, want ErrRuntimeUnavailable", err)
	}
	if _, err := broken.WaitTurnEnded(ctx, "s", "t1", time.Hour); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("broken WaitTurnEnded = %v, want ErrRuntimeUnavailable", err)
	}
	if got := turnCalls(broken.Fake); !slices.Equal(got, []string{"ObserveTurn", "InterruptTurn t1 error", "WaitTurnEnded t1"}) {
		t.Errorf("broken calls = %q", got)
	}
	if _, ok := any(runtime.NewFake()).(runtime.TurnFenceProvider); ok {
		t.Error("the plain Fake implements TurnFenceProvider")
	}
	if _, ok := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}).(runtime.TurnFenceProvider); !ok {
		t.Error("the t3bridge profile lacks TurnFenceProvider")
	}
	if _, ok := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}).(runtime.TurnFenceProvider); ok {
		t.Error("the tmux profile implements TurnFenceProvider")
	}
}

// Kills a FullFake that interrupts or waits on an empty turn id, or that
// consumes a script entry doing so.
func TestFullFakeTurnFenceRefusesAnEmptyTurnID(t *testing.T) {
	f, tf := turnFake()
	f.TurnScripts = map[string][]string{"s": {"", "t1"}}
	ctx := t.Context()
	if _, err := tf.InterruptTurn(ctx, "s", ""); !errors.Is(err, runtime.ErrTurnUnnamed) || errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("InterruptTurn(\"\") = %v, want only ErrTurnUnnamed", err)
	}
	if _, err := tf.WaitTurnEnded(ctx, "s", "", time.Hour); !errors.Is(err, runtime.ErrTurnUnnamed) {
		t.Fatalf("WaitTurnEnded(\"\") = %v, want ErrTurnUnnamed", err)
	}
	if st, err := tf.ObserveTurn(ctx, "s"); err != nil || st.Active() {
		t.Fatalf("next read = %+v, %v; want the script's first entry, no turn", st, err)
	}
	want := []string{"InterruptTurn  error", "WaitTurnEnded", "ObserveTurn"}
	if got := turnCalls(f); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// Kills a FullFake WaitTurnEnded that reads past a session the runtime no
// longer hosts instead of treating it as the turn's end, as the bridge does.
func TestFullFakeWaitTurnEndedSessionGoneEndsTheTurn(t *testing.T) {
	f, tf := turnFake()
	f.TurnScripts = map[string][]string{"s": {"t1"}}
	f.TurnReadErrors = map[string][]error{"s": {runtime.ErrSessionNotFound}}
	if st, err := tf.WaitTurnEnded(t.Context(), "s", "t1", time.Hour); err != nil || st.Active() {
		t.Fatalf("session gone mid-wait = %+v, %v; want no turn, nil", st, err)
	}
}

// Kills a t3bridge profile whose turn capabilities differ from production's
// static V1 row, or that a FullFake script can change: the row is a fixed
// trait the profile answers itself.
func TestT3BridgeProfileAnswersProductionsTurnCapabilities(t *testing.T) {
	f := runtime.NewFake()
	f.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"s": {{}}}
	fake, ok := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f}).(runtime.TurnObservationProvider)
	if !ok {
		t.Fatal("the t3bridge profile lacks TurnObservationProvider")
	}
	want := t3bridge.NewSeamBacked().(runtime.TurnObservationProvider).TurnCapabilities("s")
	if want == (runtime.TurnCapabilities{}) {
		t.Fatal("production t3bridge declares no turn capabilities")
	}
	if got := fake.TurnCapabilities("s"); got != want {
		t.Fatalf("t3bridge profile TurnCapabilities = %+v, production declares %+v", got, want)
	}
}

// A profiled tmux must reach the backend's background-work stop with its
// caller's context, and pass its answer back.
func TestTmuxProfileForwardsBackgroundWorkStop(t *testing.T) {
	for _, result := range []error{nil, runtime.ErrInteractionUnsupported} {
		b := &profileBackgroundStopSpy{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, result: result}
		p := runtime.NewFakeProfile(runtime.ProfileTmux, b)
		stopper, ok := p.(runtime.BackgroundWorkStopper)
		if !ok {
			t.Fatal("tmux profile lacks BackgroundWorkStopper")
		}
		ctx := t.Context()
		if err := stopper.StopBackgroundWork(ctx, "session"); !errors.Is(err, result) {
			t.Fatalf("StopBackgroundWork error = %v, want %v", err, result)
		}
		if b.calls != 1 || b.ctx != ctx || b.name != "session" {
			t.Fatalf("StopBackgroundWork did not forward context and session: %+v", b)
		}
	}
}

type profileBackgroundStopSpy struct {
	runtime.FullFake
	ctx    context.Context
	name   string
	result error
	calls  int
}

func (b *profileBackgroundStopSpy) StopBackgroundWork(ctx context.Context, name string) error {
	b.ctx, b.name = ctx, name
	b.calls++
	return b.result
}
