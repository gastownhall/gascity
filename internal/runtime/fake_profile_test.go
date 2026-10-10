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
	"AttachmentObserverWithError":    reflect.TypeFor[runtime.AttachmentObserverWithError](),
	"BackendListingProvider":         reflect.TypeFor[runtime.BackendListingProvider](),
	"BackendsProvider":               reflect.TypeFor[runtime.BackendsProvider](),
	"ConditionalProcessTableScanner": reflect.TypeFor[runtime.ConditionalProcessTableScanner](),
	"DeadRuntimeSessionChecker":      reflect.TypeFor[runtime.DeadRuntimeSessionChecker](),
	"DialogProvider":                 reflect.TypeFor[runtime.DialogProvider](),
	"EnvironmentBatchProvider":       reflect.TypeFor[runtime.EnvironmentBatchProvider](),
	"ExecProvider":                   reflect.TypeFor[runtime.ExecProvider](),
	"FreshByConstruction":            reflect.TypeFor[runtime.FreshByConstruction](),
	"FreshLivenessObserver":          reflect.TypeFor[runtime.FreshLivenessObserver](),
	"IdentitySidecarProvider":        reflect.TypeFor[runtime.IdentitySidecarProvider](),
	"IdleSnapshotProvider":           reflect.TypeFor[runtime.IdleSnapshotProvider](),
	"IdleWaitProvider":               reflect.TypeFor[runtime.IdleWaitProvider](),
	"ImmediateNudgeProvider":         reflect.TypeFor[runtime.ImmediateNudgeProvider](),
	"InputClearProvider":             reflect.TypeFor[runtime.InputClearProvider](),
	"InteractionProvider":            reflect.TypeFor[runtime.InteractionProvider](),
	"InterruptBoundaryWaitProvider":  reflect.TypeFor[runtime.InterruptBoundaryWaitProvider](),
	"InterruptedTurnResetProvider":   reflect.TypeFor[runtime.InterruptedTurnResetProvider](),
	"InventoryProvider":              reflect.TypeFor[runtime.InventoryProvider](),
	"ListingAttestation":             reflect.TypeFor[runtime.ListingAttestation](),
	"LivenessObserver":               reflect.TypeFor[runtime.LivenessObserver](),
	"LivenessObserverWithError":      reflect.TypeFor[runtime.LivenessObserverWithError](),
	"ProcessTableScanner":            reflect.TypeFor[runtime.ProcessTableScanner](),
	"RelaunchProvider":               reflect.TypeFor[runtime.RelaunchProvider](),
	"Router":                         reflect.TypeFor[runtime.Router](),
	"ServerDeathConfirmer":           reflect.TypeFor[runtime.ServerDeathConfirmer](),
	"ServerLifecycleProvider":        reflect.TypeFor[runtime.ServerLifecycleProvider](),
	"SessionEventProvider":           reflect.TypeFor[runtime.SessionEventProvider](),
	"SessionObjectKiller":            reflect.TypeFor[runtime.SessionObjectKiller](),
	"SessionRosterProvider":          reflect.TypeFor[runtime.SessionRosterProvider](),
	"SleepCapabilityProvider":        reflect.TypeFor[runtime.SleepCapabilityProvider](),
	"TransportCapabilityProvider":    reflect.TypeFor[runtime.TransportCapabilityProvider](),
	"UnattendedSessionStopper":       reflect.TypeFor[runtime.UnattendedSessionStopper](),
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
		runtime.ProfileExec:       exec.NewSeamBacked("/bin/true"),
		runtime.ProfileSSH:        ssh.NewSeamBacked(ep),
		runtime.ProfileT3Bridge:   t3bridge.NewSeamBacked(),
	}
}

// Kills a profile that drifts from its backend: one optional interface more
// or fewer than the provider production builds, other capabilities, another
// answer from a fixed trait (sleep capability, fresh reads, an identity
// sidecar, a transport), or no backend checked.
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
	out := []string{"-", "-", "-", "-"}
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
