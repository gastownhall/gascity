package tmux

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Exact GC API last_output of the failed C75 first-public delivery. No turn or
// resume key existed; the real native Codex welcome screen was ready for input.
//
//go:embed testdata/codex-c75-pristine-screen.txt
var capturedCodexPristine string

var bootstrapFence = runtime.InputFence{Generation: 1, Epoch: 1, Token: "bootstrap-model-token"}

func bootstrapModel(t *testing.T) (*Provider, *fenceModel) {
	t.Helper()
	m := newFenceModel(nil, fenceEnv(bootstrapFence)).ownFenced(bootstrapFence)
	m.screen = func(*fenceModel) string { return capturedCodexPristine }
	p := fenceModelProvider(m)
	cfg := runtime.Config{Command: "codex", ProviderName: "codex", Env: fenceEnv(bootstrapFence), BootstrapInputOrigin: runtime.BootstrapInputFreshBuiltin}
	if err := p.tm.prepareBootstrapInput(fenceModelSession, cfg); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range p.tm.seedBootstrapInputCommands(fenceModelSession) {
		if _, err := p.tm.runCtxRaw(t.Context(), cmd...); err != nil {
			t.Fatal(err)
		}
	}
	return p, m
}

func bootstrapProof(t *testing.T, p *Provider) *runtime.BootstrapInputProof {
	t.Helper()
	proof, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence})
	if err != nil || proof == nil {
		t.Fatalf("expected exact first-input proof: %+v, %v", proof, err)
	}
	return proof
}

func TestBootstrapNativeCapturedReadyNeedsSuccessfulCreatorStart(t *testing.T) {
	p, m := bootstrapModel(t)
	req := runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}
	if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, req); err != nil || got != nil {
		t.Fatalf("candidate is not a successful Start: %+v %v", got, err)
	}
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	proof := bootstrapProof(t, p)
	if proof.Fence.Digest() != bootstrapFence.Digest() || proof.Token == "" {
		t.Fatal("proof lost incarnation identity")
	}
	// Controller loss drops memory, not the exact issued unspent READY credit.
	recovered := fenceModelProvider(m)
	got := bootstrapProof(t, recovered)
	if got.Token != proof.Token || got.Fence != proof.Fence {
		t.Fatal("controller recovery minted or changed first-input credit")
	}
}

func TestBootstrapStartupTrustOrRawInputBurnsCandidateBeforePromotion(t *testing.T) {
	for _, input := range [][]string{{"send-keys", "-t", "=lab:@1.%1", "Enter"}, {"send-keys", "-t", "=lab:@1.%1", "-l", "startup text"}} {
		t.Run(strings.Join(input[3:], "-"), func(t *testing.T) {
			p, _ := bootstrapModel(t)
			if _, err := p.tm.runCtx(t.Context(), input...); err != nil {
				t.Fatal(err)
			}
			if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
				t.Fatalf("startup input was re-seeded: %+v %v", got, err)
			}
			if _, ok := p.tm.bootstrapCredit(fenceModelSession); ok {
				t.Fatal("spent startup candidate remained cached as successful credit")
			}
		})
	}
}

func TestBootstrapFreshProvenanceRejectsOpaqueOrPromptedStarts(t *testing.T) {
	base := runtime.Config{Command: "codex", ProviderName: "codex", Env: fenceEnv(bootstrapFence), BootstrapInputOrigin: runtime.BootstrapInputFreshBuiltin}
	for name, edit := range map[string]func(*runtime.Config){
		"resume/adopt":  func(c *runtime.Config) { c.BootstrapInputOrigin = "" },
		"opaque family": func(c *runtime.Config) { c.ProviderName = "custom" },
		"startup nudge": func(c *runtime.Config) { c.Nudge = "input" },
		"argv prompt":   func(c *runtime.Config) { c.PromptSuffix = "input" },
		"setup":         func(c *runtime.Config) { c.SessionSetup = []string{"input"} },
		"script":        func(c *runtime.Config) { c.SessionSetupScript = "input" },
		"live":          func(c *runtime.Config) { c.SessionLive = []string{"input"} },
	} {
		t.Run(name, func(t *testing.T) {
			p := fenceModelProvider(newFenceModel(nil, fenceEnv(bootstrapFence)))
			cfg := base
			edit(&cfg)
			if err := p.tm.prepareBootstrapInput(fenceModelSession, cfg); err != nil {
				t.Fatal(err)
			}
			if len(p.tm.seedBootstrapInputCommands(fenceModelSession)) != 0 {
				t.Fatal("unsupported launch received a first-input credit")
			}
		})
	}
}

func TestBootstrapNativeBusyDraftHistoryAndWrongIncarnationRefuse(t *testing.T) {
	if !pristineBootstrapScreen(capturedCodexPristine, "codex") {
		t.Fatal("actual first-input screen is unrecognized")
	}
	for name, screen := range map[string]string{
		"busy":        strings.Replace(capturedCodexPristine, "› Ask Codex to do anything", "• Working (1s • esc to interrupt)\n› Ask Codex to do anything", 1),
		"draft":       strings.Replace(capturedCodexPristine, "› Ask Codex to do anything", "› user's draft", 1),
		"prior input": "› previous human input\n" + capturedCodexPristine,
		"shell":       "$ ",
	} {
		t.Run(name, func(t *testing.T) {
			if pristineBootstrapScreen(screen, "codex") {
				t.Fatal("not a pristine native ready boundary")
			}
		})
	}
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	m.pid = "9999" // pane options survive respawn, but the root process changed.
	if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
		t.Fatalf("old process credit survived respawn: %+v %v", got, err)
	}
}

func TestBootstrapFirstFencedInputConsumesCreditAndRefusesReplay(t *testing.T) {
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	proof := bootstrapProof(t, p)
	g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
	g.bootstrap = proof
	if err := g.verify(t.Context(), p.tm); err != nil {
		t.Fatal(err)
	}
	text := []string{"send-keys", "-t", "=lab:@1.%1", "-l", "first complete input"}
	if _, err := g.run(t.Context(), p.tm, text); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}); err != nil {
		t.Fatal("same operation must finish its submit:", err)
	}
	_, before, _ := m.snapshot()
	replay := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
	replay.bootstrap = proof
	if _, err := replay.run(t.Context(), p.tm, text); !errors.Is(err, runtime.ErrNudgeNotSent) {
		t.Fatalf("first-input replay was not refused unsent: %v", err)
	}
	_, after, buffers := m.snapshot()
	if strings.Join(before, "|") != strings.Join(after, "|") || buffers != 0 {
		t.Fatalf("replay leaked input or staged buffers: before=%v after=%v buffers=%d", before, after, buffers)
	}
	if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
		t.Fatalf("consumed credit observed again: %+v %v", got, err)
	}
}

func TestBootstrapConcurrentFrozenCreditInputsDeliverAtMostOne(t *testing.T) {
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	proof := bootstrapProof(t, p)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, text := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
			g.bootstrap = proof
			<-start
			_, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", text})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, runtime.ErrNudgeNotSent):
			refused++
		default:
			t.Fatal(err)
		}
	}
	_, events, buffers := m.snapshot()
	if success != 1 || refused != 1 || len(events) != 1 || buffers != 0 {
		t.Fatalf("one native credit must admit only once: success=%d refused=%d events=%v buffers=%d", success, refused, events, buffers)
	}
}

func TestBootstrapInterveningInputAfterTextNeverReportsUnsent(t *testing.T) {
	p, _ := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
	g.bootstrap = bootstrapProof(t, p)
	if _, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", "owned text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.tm.runCtx(t.Context(), "send-keys", "-t", "=lab:@1.%1", "C-u"); err != nil {
		t.Fatal(err)
	}
	_, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "Enter"})
	if !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) || errors.Is(err, runtime.ErrNudgeNotSent) {
		t.Fatalf("partial delivery was incorrectly reusable: %v", err)
	}
}

func TestBootstrapProofCannotAuthorizeBareKeys(t *testing.T) {
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	ctx := runtime.WithBootstrapInput(context.Background(), bootstrapProof(t, p))
	if err := p.SendKeysFenced(ctx, fenceModelSession, bootstrapFence, "Enter"); !errors.Is(err, runtime.ErrInputNotSent) {
		t.Fatalf("bootstrap proof authorized a non-nudge input: %v", err)
	}
	_, events, _ := m.snapshot()
	if len(events) != 0 {
		t.Fatal("bare keys reached the native pane")
	}
}

func TestBootstrapUnsupportedStartNeverReusesOldCreatorCredit(t *testing.T) {
	p, _ := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	if err := p.tm.prepareBootstrapInput(fenceModelSession, runtime.Config{Command: "codex resume", ProviderName: "codex", Env: fenceEnv(bootstrapFence)}); err != nil {
		t.Fatal(err)
	}
	if len(p.tm.seedBootstrapInputCommands(fenceModelSession)) != 0 {
		t.Fatal("unsupported later launch reused a prior creator seed")
	}
	later := bootstrapFence
	later.Generation++
	if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: later}); err != nil || got != nil {
		t.Fatalf("unsupported later-generation input opportunity revived: %+v %v", got, err)
	}
}

func TestBootstrapRunLiveOpaqueCommandsInvalidateBeforeTheirEffects(t *testing.T) {
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	proof := bootstrapProof(t, p)
	marker := filepath.Join(t.TempDir(), "owned-live-script-ran")
	cfg := runtime.Config{SessionLive: []string{"printf ran > " + shellquote.Quote(marker)}}
	if err := p.RunLive(fenceModelSession, cfg); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ran" {
		t.Fatalf("live command did not execute after taint: %q %v", data, err)
	}
	if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
		t.Fatalf("opaque live effects left a first-input opportunity: %+v %v", got, err)
	}
	g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
	g.bootstrap = proof
	if _, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", "stale first input"}); !errors.Is(err, runtime.ErrNudgeNotSent) {
		t.Fatalf("frozen proof survived live script: %v", err)
	}
	_, events, buffers := m.snapshot()
	if len(events) != 0 || buffers != 0 {
		t.Fatal("stale proof applied input or retained a staged buffer")
	}
}

func TestBootstrapRunLiveNoOpKeepsExactCredit(t *testing.T) {
	p, _ := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	before := bootstrapProof(t, p)
	if err := p.RunLive(fenceModelSession, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	after := bootstrapProof(t, p)
	if before.Token != after.Token || before.Fence != after.Fence {
		t.Fatal("no-op live update changed the original first-input credit")
	}
}

type bootstrapTaintFailure struct {
	executor
	mode string
}

func (f bootstrapTaintFailure) executeCtx(ctx context.Context, args []string) (string, error) {
	if slices.Contains(args, bootstrapInputOption) && slices.Contains(args, "set-option") && f.mode == "set failed" {
		return "", errors.New("bounded native taint write failed")
	}
	if slices.Contains(args, "display-message") && strings.Contains(args[len(args)-1], bootstrapInputOption) && f.mode == "readback unknown" {
		return "unknown", nil
	}
	return f.executor.executeCtx(ctx, args)
}

func TestBootstrapRunLiveUnconfirmedTaintPreventsOpaqueEffects(t *testing.T) {
	for _, mode := range []string{"set failed", "readback unknown"} {
		t.Run(mode, func(t *testing.T) {
			p, m := bootstrapModel(t)
			if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			proof := bootstrapProof(t, p)
			p.tm.exec = bootstrapTaintFailure{executor: m, mode: mode}
			marker := filepath.Join(t.TempDir(), "must-not-run")
			if err := p.RunLive(fenceModelSession, runtime.Config{SessionLive: []string{"printf unsafe > " + shellquote.Quote(marker)}}); err == nil {
				t.Fatal("opaque script ran without native taint acknowledgment")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unconfirmed taint allowed opaque side effects: %v", err)
			}
			p.tm.exec = m
			g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
			g.bootstrap = proof
			if _, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", "frozen input"}); !errors.Is(err, runtime.ErrNudgeNotSent) {
				t.Fatalf("failed taint left creator permission alive: %v", err)
			}
		})
	}
}

// A controller restart before all input can recover the already issued native
// capability. It must not recover a claimed/spent capability after any input.
func TestBootstrapControllerRestartRecoversSameReadyNonceOnlyOnce(t *testing.T) {
	creator, m := bootstrapModel(t)
	if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	before := bootstrapProof(t, creator)
	marker := m.bootstrap
	recovered := fenceModelProvider(m)
	proof := bootstrapProof(t, recovered)
	if proof.Token != before.Token || proof.Fence != before.Fence || m.bootstrap != marker {
		t.Fatal("native READY recovery changed the original authority")
	}
	g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
	g.bootstrap = proof
	if _, err := g.run(t.Context(), recovered.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", "first complete input"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(t.Context(), recovered.tm, []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}); err != nil {
		t.Fatal(err)
	}
	afterRestart := fenceModelProvider(m)
	if got, err := afterRestart.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
		t.Fatalf("claimed credit recovered after controller loss: %+v %v", got, err)
	}
	replay := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
	replay.bootstrap = proof
	if _, err := replay.run(t.Context(), afterRestart.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", "duplicate input"}); !errors.Is(err, runtime.ErrNudgeNotSent) {
		t.Fatalf("frozen proof bypassed spent native authority: %v", err)
	}
	_, events, buffers := m.snapshot()
	if len(events) != 2 || buffers != 0 {
		t.Fatalf("expected one native text+submit only: events=%v buffers=%d", events, buffers)
	}
}

func TestBootstrapControllerRecoveryRejectsUnissuedOrForeignMarkers(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"candidate": func(value string) string { return strings.Replace(value, ".ready.", ".candidate.", 1) },
		"claimed":   func(value string) string { return strings.Replace(value, ".ready.", ".claimed.", 1) },
		"spent":     func(string) string { return "spent" },
		"absent":    func(string) string { return "" },
		"legacy": func(string) string {
			return "ready.0123456789abcdef0123456789abcdef." + bootstrapFence.Digest() + ".%1.4242"
		},
		"version": func(value string) string { return strings.Replace(value, "v1.", "v2.", 1) },
		"origin":  func(value string) string { return strings.Replace(value, "fresh_builtin_conversation", "resume", 1) },
		"family":  func(value string) string { return strings.Replace(value, ".codex.", ".claude.", 1) },
		"nonce": func(value string) string {
			fields := strings.Split(value, ".")
			fields[4] = "not-a-nonce"
			return strings.Join(fields, ".")
		},
		"short nonce": func(value string) string {
			fields := strings.Split(value, ".")
			fields[4] = "0123456789abcdef"
			return strings.Join(fields, ".")
		},
		"noncanonical nonce": func(value string) string {
			fields := strings.Split(value, ".")
			fields[4] = "ABCDEF0123456789ABCDEF0123456789"
			return strings.Join(fields, ".")
		},
		"fence": func(value string) string {
			return strings.Replace(value, bootstrapFence.Digest(), "0000000000000000", 1)
		},
		"pane":        func(value string) string { return strings.Replace(value, ".%1.", ".%9.", 1) },
		"pid":         func(value string) string { return strings.TrimSuffix(value, ".4242") + ".9999" },
		"extra field": func(value string) string { return value + ".extra" },
	} {
		t.Run(name, func(t *testing.T) {
			creator, m := bootstrapModel(t)
			if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			m.bootstrap = edit(m.bootstrap)
			p := fenceModelProvider(m)
			if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
				t.Fatalf("invalid marker recovered authority: %+v %v", got, err)
			}
			if _, ok := p.tm.bootstrapCredit(fenceModelSession); ok {
				t.Fatal("invalid marker populated controller credit")
			}
		})
	}
}

func TestBootstrapControllerRecoveryRejectsLaterEpochEvenWithMatchingNativeMarker(t *testing.T) {
	for _, later := range []runtime.InputFence{
		{Generation: 2, Epoch: 1, Token: "later"},
		{Generation: 1, Epoch: 2, Token: "later"},
	} {
		creator, m := bootstrapModel(t)
		if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
			t.Fatal(err)
		}
		m.bootstrap = strings.Replace(m.bootstrap, bootstrapFence.Digest(), later.Digest(), 1)
		m.own = later.Digest() + ".%1"
		p := fenceModelProvider(m)
		if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: later}); err != nil || got != nil {
			t.Fatalf("later incarnation recovered first-input credit: %+v %v", got, err)
		}
		cfg := runtime.Config{Command: "codex", ProviderName: "codex", Env: fenceEnv(later), BootstrapInputOrigin: runtime.BootstrapInputFreshBuiltin}
		if err := p.tm.prepareBootstrapInput(fenceModelSession, cfg); err != nil {
			t.Fatal(err)
		}
		if len(p.tm.seedBootstrapInputCommands(fenceModelSession)) != 0 {
			t.Fatal("later incarnation seeded new authority")
		}
	}
}

func TestBootstrapControllerRecoveryRequiresStableNativeBookends(t *testing.T) {
	for name, change := range map[string]func(*fenceModel){
		"spent during observation": func(m *fenceModel) { m.bootstrap = "spent" },
		"nonce replaced": func(m *fenceModel) {
			fields := strings.Split(m.bootstrap, ".")
			fields[4] = "0123456789abcdef0123456789abcdef"
			m.bootstrap = strings.Join(fields, ".")
		},
		"process replaced": func(m *fenceModel) { m.pid = "9999" },
		"pane dead":        func(m *fenceModel) { m.dead = true },
		"fence replaced":   func(m *fenceModel) { m.own = "0000000000000000.%1" },
	} {
		t.Run(name, func(t *testing.T) {
			creator, m := bootstrapModel(t)
			if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			m.beforeRead = func(m *fenceModel, n int) {
				if n == 2 {
					change(m)
				}
			}
			p := fenceModelProvider(m)
			if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
				t.Fatalf("changed native state recovered authority: %+v %v", got, err)
			}
			if _, ok := p.tm.bootstrapCredit(fenceModelSession); ok {
				t.Fatal("unstable state populated controller credit")
			}
		})
	}
}

func TestBootstrapControllerRecoveryKeepsNativeBusyAndDraftVetoes(t *testing.T) {
	for name, screen := range map[string]string{
		"busy":       strings.Replace(capturedCodexPristine, "› Ask Codex to do anything", "• Working (1s • esc to interrupt)\n› Ask Codex to do anything", 1),
		"draft":      strings.Replace(capturedCodexPristine, "› Ask Codex to do anything", "› user's draft", 1),
		"not native": "$ ",
	} {
		t.Run(name, func(t *testing.T) {
			creator, m := bootstrapModel(t)
			if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			m.screen = func(*fenceModel) string { return screen }
			p := fenceModelProvider(m)
			if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
				t.Fatalf("non-pristine native state recovered authority: %+v %v", got, err)
			}
		})
	}
}

func TestBootstrapFailedOpaqueWriteCanOnlyRecoverGenuinelyUnspentReadyCredit(t *testing.T) {
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	original := bootstrapProof(t, p)
	p.tm.exec = bootstrapTaintFailure{executor: m, mode: "set failed"}
	marker := filepath.Join(t.TempDir(), "must-not-run")
	if err := p.RunLive(fenceModelSession, runtime.Config{SessionLive: []string{"printf unsafe > " + shellquote.Quote(marker)}}); err == nil {
		t.Fatal("opaque write proceeded without invalidation")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opaque effects occurred: %v", err)
	}
	p.tm.exec = m
	if _, ok := p.tm.bootstrapCredit(fenceModelSession); ok {
		t.Fatal("failed live write preserved cached permission")
	}
	// No effects occurred and native READY was never changed. Only a fresh
	// complete observer proof may cache the same definitely unspent authority.
	recovered := bootstrapProof(t, fenceModelProvider(m))
	if recovered.Token != original.Token || recovered.Fence != original.Fence {
		t.Fatal("recovery changed native authority")
	}
}

func TestBootstrapIndependentRecoveredControllersRaceForSameNativeCredit(t *testing.T) {
	creator, m := bootstrapModel(t)
	if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	controllers := []*Provider{fenceModelProvider(m), fenceModelProvider(m)}
	proofs := []*runtime.BootstrapInputProof{bootstrapProof(t, controllers[0]), bootstrapProof(t, controllers[1])}
	if proofs[0].Token != proofs[1].Token {
		t.Fatal("controllers recovered different native authority")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, p := range controllers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g := newPaneGuard(fenceModelSession, bootstrapFence.Digest(), true)
			g.bootstrap = proofs[i]
			<-start
			_, err := g.run(t.Context(), p.tm, []string{"send-keys", "-t", "=lab:@1.%1", "-l", "one native input"})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, runtime.ErrNudgeNotSent):
			refused++
		default:
			t.Fatal(err)
		}
	}
	_, events, buffers := m.snapshot()
	if success != 1 || refused != 1 || len(events) != 1 || buffers != 0 {
		t.Fatalf("native CAS admitted more than one controller: success=%d refused=%d events=%v buffers=%d", success, refused, events, buffers)
	}
}

// Input through any controller spends the native pane capability, even when
// that controller has not yet observed or cached bootstrap authority.
func TestBootstrapOrdinaryInputBeforeRecoverySpendsNativeCredit(t *testing.T) {
	for name, input := range map[string]func(context.Context, *Provider) error{
		"raw key": func(ctx context.Context, p *Provider) error {
			_, err := p.tm.runCtx(ctx, "send-keys", "-t", "=lab:@1.%1", "C-u")
			return err
		},
		"raw paste": func(ctx context.Context, p *Provider) error {
			buffer, err := p.tm.loadPasteBuffer(ctx, "ordinary raw input")
			if err != nil {
				return err
			}
			_, err = p.tm.runCtx(ctx, "paste-buffer", "-d", "-r", "-b", buffer, "-t", "=lab:@1.%1")
			return err
		},
		"fenced key": func(ctx context.Context, p *Provider) error {
			return p.fenced(ctx, fenceModelSession, bootstrapFence, false, func(ctx context.Context) error {
				_, err := p.tm.runCtx(ctx, "send-keys", "-t", "=lab:@1.%1", "C-u")
				return err
			})
		},
		"fenced paste": func(ctx context.Context, p *Provider) error {
			return p.fenced(ctx, fenceModelSession, bootstrapFence, true, func(ctx context.Context) error {
				_, err := p.tm.runCtx(ctx, "send-keys", "-t", "=lab:@1.%1", "-l", "ordinary fenced input")
				return err
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			creator, m := bootstrapModel(t)
			if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			p := fenceModelProvider(m)
			if _, ok := p.tm.bootstrapCredit(fenceModelSession); ok {
				t.Fatal("new controller unexpectedly has creator memory")
			}
			if err := input(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if m.bootstrap != "spent" {
				t.Fatal("ordinary GC input did not spend native READY authority")
			}
			if _, ok := p.tm.bootstrapCredit(fenceModelSession); ok {
				t.Fatal("ordinary input reconstructed bootstrap cache")
			}
			_, before, buffers := m.snapshot()
			if len(before) != 1 || buffers != 0 {
				t.Fatalf("ordinary input did not occur exactly once: events=%v buffers=%d", before, buffers)
			}
			observer := fenceModelProvider(m)
			if got, err := observer.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
				t.Fatalf("input through empty-cache controller revived authority: %+v %v", got, err)
			}
			_, after, remaining := m.snapshot()
			if strings.Join(before, "|") != strings.Join(after, "|") || remaining != 0 {
				t.Fatal("refused recovery performed additional input")
			}
		})
	}
}

// Keep exact legacy respawn/capture expectations while explicitly checking the
// new mandatory native invalidation immediately before their effects.
func assertBootstrapSpendCall(t *testing.T, got []string, target string) {
	t.Helper()
	want := []string{"-u", "set-option", "-p", "-t", target, bootstrapInputOption, "spent"}
	if !slices.Equal(got, want) {
		t.Fatalf("native bootstrap spend call=%q, want %q", got, want)
	}
}

func TestBootstrapRespawnSpendsNativeReadyBeforeReplacement(t *testing.T) {
	creator, m := bootstrapModel(t)
	if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	before := len(m.calls)
	replacement := fenceModelProvider(m)
	if err := replacement.tm.RespawnPaneWithWorkDir("%1", "/work", "codex"); err != nil {
		t.Fatal(err)
	}
	calls := m.calls[before:]
	wantSpend := []string{"set-option", "-p", "-t", "%1", bootstrapInputOption, "spent"}
	if len(calls) != 2 || !slices.Equal(calls[0], wantSpend) || calls[1][0] != "respawn-pane" {
		t.Fatalf("respawn must spend exact pane before effects: %q", calls)
	}
	if m.bootstrap != "spent" {
		t.Fatal("respawn retained old READY authority")
	}
	observer := fenceModelProvider(m)
	if got, err := observer.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
		t.Fatalf("replacement revived native authority: %+v %v", got, err)
	}
}

func TestBootstrapRespawnInvalidationFailurePreventsReplacement(t *testing.T) {
	creator, m := bootstrapModel(t)
	if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	original := bootstrapProof(t, creator)
	before := len(m.calls)
	replacement := fenceModelProvider(m)
	replacement.tm.exec = bootstrapTaintFailure{executor: m, mode: "set failed"}
	if err := replacement.tm.RespawnPaneWithWorkDir("%1", "/work", "codex"); err == nil {
		t.Fatal("respawn proceeded without invalidating old native credit")
	}
	for _, call := range m.calls[before:] {
		if slices.Contains(call, "respawn-pane") {
			t.Fatal("replacement effects occurred after spend failure")
		}
	}
	// The refused operation performed no effect and never changed the marker;
	// any later recovery can only recover the same still-unspent authority.
	recovered := bootstrapProof(t, fenceModelProvider(m))
	if recovered.Token != original.Token {
		t.Fatal("failed replacement minted a different first-input credit")
	}
}
