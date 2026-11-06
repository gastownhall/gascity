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
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Exact GC API last_output of the failed C75 first-public delivery. No turn or
// resume key existed; the real native Codex welcome screen was ready for input.
//
//go:embed testdata/codex-c75-pristine-screen.txt
var capturedCodexPristine string

// Exact compact Codex 0.162.1 startup captured through the supervisor API on
// frozen95. The introductory command menu is optional in this native layout.
//
//go:embed testdata/codex-95-compact-pristine-screen.txt
var capturedCodexCompactPristine string

// Actual F4 supervisor API current-frame capture for Codex0.162.1 suggest mode.
// Only cwd strings are anonymized; this is unstyled API output, not a native
// -e capture. The existing styled native fixture remains unchanged.
//
//go:embed testdata/codex-f4-suggest-pristine-screen.txt
var capturedCodexSuggestPristine string

// Actual unstyled GC API compact frame; only the private cwd token changes.
// Its literal Try text alone does not prove that the native composer is empty.
//
//go:embed testdata/claude-f5-compact-update-screen.txt
var capturedClaudeCompactAPI string

// Exact current -e capture from a fresh root-owned F5 Claude2.1.308 runtime.
// Only its private cwd token changes. SGR bytes and real dim placeholder remain.
// This frame proves presentation, not availability of a native READY credit.
//
//go:embed testdata/claude-f5-compact-styled-screen.txt
var capturedClaudeCompactStyled string

func claudeCompactWithComposer(t *testing.T, content string) string {
	t.Helper()
	lines := strings.Split(capturedClaudeCompactStyled, "\n")
	found := 0
	for i, line := range lines {
		if strings.ContainsRune(line, '❯') {
			lines[i] = "❯ " + content
			found++
		}
	}
	if found != 1 {
		t.Fatal("actual styled Claude fixture has no single composer")
	}
	return strings.Join(lines, "\n")
}

func TestBootstrapCompactNativeCapturedReadyRequiresIssuedAuthority(t *testing.T) {
	for mode, frame := range map[string]string{"unrestricted": capturedCodexCompactPristine, "suggest": capturedCodexSuggestPristine} {
		t.Run(mode, func(t *testing.T) {
			p, m := bootstrapModel(t)
			m.screen = func(*fenceModel) string { return frame }
			req := runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}
			if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, req); err != nil || got != nil {
				t.Fatalf("compact screen minted pre-start authority: %+v %v", got, err)
			}
			if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
				t.Fatal(err)
			}
			proof := bootstrapProof(t, p)
			if proof.Fence != bootstrapFence || proof.Token == "" {
				t.Fatal("compact native ready lost issued authority")
			}
			recovered := fenceModelProvider(m)
			if got := bootstrapProof(t, recovered); got.Token != proof.Token {
				t.Fatal("compact recovery changed issued native authority")
			}
		})
	}
}

func TestBootstrapCompactPristineLayoutKeepsNativeVetoes(t *testing.T) {
	if !pristineBootstrapScreen(capturedCodexCompactPristine, "codex") {
		t.Fatal("actual compact native startup frame refused")
	}
	const brand = "  >_ OpenAI Codex"
	_, afterBrand, found := strings.Cut(capturedCodexCompactPristine, brand)
	if !found {
		t.Fatal("captured compact frame lacks its native brand")
	}
	withoutBanner := brand + afterBrand
	dim := strings.ReplaceAll(capturedCodexCompactPristine, "› Ask Codex to do anything", "› \x1b[2mAsk Codex to do anything\x1b[22m")
	dim = strings.ReplaceAll(dim, "GPT-6.1-Sol medium", "\x1b[2mGPT-6.1-Sol medium\x1b[22m")
	for name, frame := range map[string]string{"without optional update panel": withoutBanner, "dim native placeholder and status": dim, "actual suggest frame without permission row": capturedCodexSuggestPristine} {
		t.Run(name, func(t *testing.T) {
			if !pristineBootstrapScreen(frame, "codex") {
				t.Fatal("qualified compact layout refused")
			}
		})
	}
	for name, frame := range map[string]string{
		"suggest unknown permissions":      strings.Replace(capturedCodexSuggestPristine, "› Ask Codex to do anything", "permissions: unrecognized mode\n› Ask Codex to do anything", 1),
		"suggest prior history":            "Previous assistant output\n" + capturedCodexSuggestPristine,
		"suggest draft":                    strings.Replace(capturedCodexSuggestPristine, "› Ask Codex to do anything", "› user's draft", 1),
		"suggest native busy":              strings.Replace(capturedCodexSuggestPristine, "› Ask Codex to do anything", "• Working (1s • esc to interrupt)\n› Ask Codex to do anything", 1),
		"suggest spoofed brand":            strings.Replace(capturedCodexSuggestPristine, ">_ OpenAI Codex (v0.162.1)", ">_ Other program (v0.162.1)", 1),
		"suggest missing footer":           strings.Replace(capturedCodexSuggestPristine, "? for shortcuts", "", 1),
		"suggest unknown control":          "\x1b[1G" + capturedCodexSuggestPristine,
		"draft":                            strings.Replace(capturedCodexCompactPristine, "› Ask Codex to do anything", "› user's draft", 1),
		"pasted draft":                     strings.Replace(capturedCodexCompactPristine, "› Ask Codex to do anything", "› [Pasted Content 24 chars]", 1),
		"native busy":                      strings.Replace(capturedCodexCompactPristine, "› Ask Codex to do anything", "• Working (1s • esc to interrupt)\n› Ask Codex to do anything", 1),
		"startup pending":                  strings.Replace(capturedCodexCompactPristine, "› Ask Codex to do anything", "Connecting to MCP servers (0/1)\n› Ask Codex to do anything", 1),
		"prior user input":                 "› previous human input\n" + capturedCodexCompactPristine,
		"prior assistant output":           "The previous task completed.\n" + capturedCodexCompactPristine,
		"assistant output before composer": strings.Replace(capturedCodexCompactPristine, "› Ask Codex to do anything", "Previous answer text\n› Ask Codex to do anything", 1),
		"native question":                  strings.Replace(capturedCodexCompactPristine, "› Ask Codex to do anything", "Which animal should we choose?\n1. Otter\n2. Owl\n› Ask Codex to do anything", 1),
		"second composer":                  capturedCodexCompactPristine + "\n› ",
		"missing brand":                    strings.Replace(capturedCodexCompactPristine, ">_ OpenAI Codex (v0.162.1)", "", 1),
		"unknown permissions":              strings.Replace(capturedCodexCompactPristine, "permissions: YOLO mode", "permissions: unrecognized mode", 1),
		"missing model footer":             strings.Replace(capturedCodexCompactPristine, "GPT-6.1-Sol medium ·", "", 1),
		"missing shortcuts footer":         strings.Replace(capturedCodexCompactPristine, "? for shortcuts", "", 1),
		"truncated update panel":           strings.Replace(capturedCodexCompactPristine, "╰─────────────────────────────────────────────────╯", "", 1),
		"foreign update notice":            strings.Replace(capturedCodexCompactPristine, "Run npm install -g @openai/codex to update.", "Previous assistant message", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if pristineBootstrapScreen(frame, "codex") {
				t.Fatal("unsafe compact native frame accepted")
			}
		})
	}
	p, m := bootstrapModel(t)
	m.screen = func(*fenceModel) string { return capturedCodexCompactPristine }
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	m.pid = "9999"
	if got, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || got != nil {
		t.Fatalf("compact frame recovered another native process: %+v %v", got, err)
	}
}

var bootstrapFence = runtime.InputFence{Generation: 1, Epoch: 1, Token: "bootstrap-model-token"}

func bootstrapModel(t *testing.T) (*Provider, *fenceModel) {
	t.Helper()
	return bootstrapFamilyModel(t, "codex", capturedCodexPristine)
}

func bootstrapFamilyModel(t *testing.T, family, frame string) (*Provider, *fenceModel) {
	t.Helper()
	m := newFenceModel(nil, fenceEnv(bootstrapFence)).ownFenced(bootstrapFence)
	m.screen = func(*fenceModel) string { return frame }
	if family == "claude" {
		m.env["GC_PROVIDER"] = family
	}
	p := fenceModelProvider(m)
	cfg := runtime.Config{Command: family, ProviderName: family, Env: fenceEnv(bootstrapFence), BootstrapInputOrigin: runtime.BootstrapInputFreshBuiltin}
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
	for mode, frame := range map[string]string{"intro": capturedCodexPristine, "unrestricted": capturedCodexCompactPristine, "suggest": capturedCodexSuggestPristine} {
		t.Run(mode, func(t *testing.T) {
			p, m := bootstrapModel(t)
			m.screen = func(*fenceModel) string { return frame }
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
		})
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

// bootstrapProviderStartExecutor adds native create/list dispatch around the
// existing fence model. Provider.Start, its real startup-dialog parser, native
// credit commands and the input guard remain production code. This executor
// never launches tmux, a provider, or a model.
type bootstrapProviderStartExecutor struct {
	model           *fenceModel
	created         bool
	candidateIssued bool
	sourceFile      bool
	captureArmed    bool
}

func (e *bootstrapProviderStartExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return e.execute(args)
}

func (e *bootstrapProviderStartExecutor) execute(args []string) (string, error) {
	args = slices.Clone(args)
	for len(args) > 0 && (args[0] == "-u" || args[0] == "-N") {
		args = args[1:]
	}
	if len(args) > 1 && args[0] == "-L" {
		args = args[2:]
	}
	if separator := slices.Index(args, ";"); separator >= 0 {
		first, err := e.execute(args[:separator])
		if err != nil {
			return "", err
		}
		rest, err := e.execute(args[separator+1:])
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(first + "\n" + rest), nil
	}
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "source-file":
		e.sourceFile = true
		content, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		return e.execute(shellquote.Split(string(content)))
	case "new-session":
		if e.created {
			return "", ErrSessionExists
		}
		e.created = true
		return e.model.paneID, nil
	case "has-session":
		if !e.created {
			return "", ErrSessionNotFound
		}
		return "", nil
	case "pipe-pane":
		e.captureArmed = true
	case "set-option":
		if slices.Contains(args, bootstrapInputOption) && strings.Contains(args[len(args)-1], ".candidate.") {
			e.candidateIssued = true
		}
	}
	return e.model.execute(args)
}

const bootstrapTrustScreen = `› Ask Codex to do anything

  Folder access
  /Users/u/src/project

  Trust this folder? Codex can read, edit, and run files here, subject to your
  permission settings. Folder settings can run code automatically, even
  without a model request. Continue only if you trust these files. Your trust
  decision will be saved.

› 1. Trust and continue
  2. Quit

  enter continue · esc quit`

// This control exercises the real capture/relay create command list and its
// secret-env source-file transport, as used by the supervisor's native Start.
func TestBootstrapRawProviderStartPreservesCandidateThroughVerifiedStartupControls(t *testing.T) {
	update := func(selected int) string {
		first, second := "› ", "  "
		if selected == 2 {
			first, second = "  ", "› "
		}
		return "Update available · 0.162.1 → 0.166.0\n" + first +
			"1. Update now (runs `npm install -g @openai/codex`)\n" + second +
			"2. Skip\n  3. Skip until next version\nenter continue · esc skip"
	}
	hooks := func(selected int) string {
		first, second := "› ", "  "
		if selected == 2 {
			first, second = "  ", "› "
		}
		return "  Hooks need review\n  4 hooks are new or changed.\n  Hooks can run outside the sandbox after you trust them.\n" + first +
			"1. Review hooks\n" + second + "2. Trust all and continue\n  3. Continue without trusting (hooks won't run)\n  enter confirm · esc skip"
	}
	for _, dialog := range []string{"none", "trust", "update", "hooks"} {
		t.Run(dialog, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			env := fenceEnv(bootstrapFence)
			env[testSecretKey] = testSecret
			model := newFenceModel(nil, env)
			waiting := dialog != "none"
			selected := 1
			var keys []string
			candidateBeforeKey := true
			model.screen = func(*fenceModel) string {
				if waiting {
					if dialog == "update" {
						return update(selected)
					}
					if dialog == "hooks" {
						return hooks(selected)
					}
					return bootstrapTrustScreen
				}
				return capturedCodexCompactPristine
			}
			model.onEvent = func(m *fenceModel, event string) {
				if strings.HasPrefix(event, "key:") {
					key := strings.TrimPrefix(event, "key:")
					keys = append(keys, key)
					candidateBeforeKey = candidateBeforeKey && strings.Contains(m.bootstrap, ".candidate.")
					if key == "Down" {
						selected = 2
					}
					if key == "Enter" {
						waiting = false
					}
				}
			}
			provider := fenceModelProvider(model)
			provider.tm.SetProgramStatusMode(runtime.ProgramStatusOn)
			executor := &bootstrapProviderStartExecutor{model: model}
			provider.tm.exec = executor
			provider.tm.capture.probe = func() (captureCapability, error) {
				return captureCapability{companion: testCompanion, relay: programstatus.RelayVersion, tmux: testTmuxBin, shell: testShell}, nil
			}
			cfg := runtime.Config{
				Command: "codex", ProviderName: "codex", Env: env,
				ReadyPromptPrefix: "› ", AcceptStartupDialogs: boolPtr(true), MouseOn: true,
				ProgramStatus:        runtime.ProgramStatusArm{App: "codex", Capture: true, Relay: true},
				BootstrapInputOrigin: runtime.BootstrapInputFreshBuiltin,
			}
			if err := provider.Start(t.Context(), fenceModelSession, cfg); err != nil {
				t.Fatalf("actual raw Provider.Start: %v", err)
			}
			if !executor.created || !executor.candidateIssued || !executor.sourceFile || !executor.captureArmed {
				t.Fatalf("raw Start coverage create=%v candidate=%v source-file=%v capture=%v", executor.created, executor.candidateIssued, executor.sourceFile, executor.captureArmed)
			}
			if waiting || !pristineBootstrapScreen(model.screen(model), "codex") {
				t.Fatal("startup did not reach complete pristine compact UI")
			}
			proof, err := provider.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence})
			if err != nil {
				t.Fatalf("observing post-start authority: %v", err)
			}
			want := []string(nil)
			if dialog == "trust" {
				want = []string{"Enter"}
			}
			if dialog == "update" || dialog == "hooks" {
				want = []string{"Down", "Enter"}
			}
			if !slices.Equal(keys, want) || !candidateBeforeKey || proof == nil || proof.Fence != bootstrapFence {
				t.Fatalf("verified startup keys=%v want=%v candidate-before-key=%v proof-present=%v", keys, want, candidateBeforeKey, proof != nil)
			}
		})
	}
}

func TestBootstrapStartupCandidateRejectsChangedBookends(t *testing.T) {
	for _, mutation := range []string{"spent", "ready", "claimed", "missing", "fence", "pid", "pane", "question", "intervening ordinary input", "canceled context", "no parser observation"} {
		t.Run(mutation, func(t *testing.T) {
			p, m := bootstrapModel(t)
			m.screen = func(*fenceModel) string { return bootstrapTrustScreen }
			credit, _ := p.tm.bootstrapCredit(fenceModelSession)
			s := &bootstrapStartupInput{name: fenceModelSession, credit: credit}
			if err := s.verify(t.Context(), p.tm, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.peek(t.Context(), p.tm, s.target, 80); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch mutation {
			case "spent":
				m.bootstrap = "spent"
			case "ready":
				m.bootstrap = strings.Replace(m.bootstrap, ".candidate.", ".ready.", 1)
			case "claimed":
				m.bootstrap = strings.Replace(m.bootstrap, ".candidate.", ".claimed.", 1)
			case "missing":
				m.bootstrap = ""
			case "fence":
				m.own = "foreign.%1"
			case "pid":
				m.pid = "9999"
			case "pane":
				m.paneID = "%99"
			case "question":
				m.screen = func(*fenceModel) string { return "Which animal?\n› Otter\n  Owl\nEnter to select" }
			case "intervening ordinary input":
				if _, err := p.tm.runCtx(ctx, "send-keys", "-t", s.target, "Escape"); err != nil {
					t.Fatal(err)
				}
			case "canceled context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "no parser observation":
				s.observed = false
			}
			_, before, _ := m.snapshot()
			if err := s.send(ctx, p.tm, s.target, []string{"Enter"}); err == nil {
				t.Fatal("changed startup bookend allowed input")
			}
			_, after, _ := m.snapshot()
			if !slices.Equal(before, after) {
				t.Fatalf("changed startup bookend sent input: %v", after[len(before):])
			}
		})
	}
}

func TestBootstrapStartupCandidateNativeGuardClosesFinalRace(t *testing.T) {
	for _, mutation := range []string{"spent", "fence", "pid", "dead"} {
		t.Run(mutation, func(t *testing.T) {
			p, m := bootstrapModel(t)
			m.screen = func(*fenceModel) string { return bootstrapTrustScreen }
			credit, _ := p.tm.bootstrapCredit(fenceModelSession)
			s := &bootstrapStartupInput{name: fenceModelSession, credit: credit}
			if err := s.verify(t.Context(), p.tm, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.peek(t.Context(), p.tm, s.target, 80); err != nil {
				t.Fatal(err)
			}
			m.beforeGuard = func(m *fenceModel, _ int) {
				switch mutation {
				case "spent":
					m.bootstrap = "spent"
				case "fence":
					m.own = "foreign.%1"
				case "pid":
					m.pid = "9999"
				case "dead":
					m.dead = true
				}
			}
			if err := s.send(t.Context(), p.tm, s.target, []string{"Enter"}); err == nil {
				t.Fatal("native race reported successful startup input")
			}
			bare, events, _ := m.snapshot()
			if len(bare) != 0 || len(events) != 0 {
				t.Fatalf("native race delivered: bare=%v events=%v", bare, events)
			}
		})
	}
}

func TestBootstrapGenericDialogStillConsumesPromotedCredit(t *testing.T) {
	p, m := bootstrapModel(t)
	if err := p.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	waiting := true
	m.screen = func(*fenceModel) string {
		if waiting {
			return bootstrapTrustScreen
		}
		return capturedCodexCompactPristine
	}
	m.onEvent = func(_ *fenceModel, event string) {
		if event == "key:Enter" {
			waiting = false
		}
	}
	if err := p.tm.DismissKnownDialogs(t.Context(), fenceModelSession, time.Second); err != nil {
		t.Fatal(err)
	}
	if waiting || m.bootstrap != "spent" {
		t.Fatal("generic post-start dialog failed to consume credit")
	}
	if proof, err := p.ObserveBootstrapInput(t.Context(), fenceModelSession, runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}); err != nil || proof != nil {
		t.Fatal("generic post-start input regranted authority")
	}
}

// Exact owned native capture1402B SHA5120dfec5b8466b4f1a111b2072d6f3d6843f84cc6f8eb781702444ea4ab4017.
// Only displayed cwd tokens are anonymized; dim version, all44SGR and both
// OSC8 hyperlink wrappers are retained. No metadata nonce is in this frame.
//
//go:embed testdata/codex-f567-styled-pristine-screen.txt
var capturedF567StyledPristine string

func TestBootstrapActualStyledNativeFrameIsPristine(t *testing.T) {
	claude := capturedClaudeCompactStyled
	for name, frame := range map[string]string{
		"Claude actual compact styled frame":   claude,
		"Claude compact without update notice": strings.Replace(claude, "✔ Update installed · Restart to update", "", 1),
		"Claude compact dim version":           strings.Replace(claude, "v2.1.308", "\x1b[2mv2.1.308\x1b[22m", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if !pristineBootstrapScreen(frame, "claude") {
				t.Fatal("actual styled compact Claude layout rejected")
			}
		})
	}
	if pristineBootstrapScreen(capturedClaudeCompactAPI, "claude") {
		t.Fatal("unstyled API Try text alone proved an empty native composer")
	}
	if !compactCodexBootstrapScreen(capturedF567StyledPristine) {
		t.Fatal("exact styled native compact frame rejected structurally")
	}
	if !pristineBootstrapScreen(capturedF567StyledPristine, "codex") {
		t.Fatal("exact styled native frame rejected as pristine")
	}
}

func TestBootstrapActualStyledFrameRecoversOnlyIssuedReady(t *testing.T) {
	t.Run("Claude actual styled compact layout", func(t *testing.T) {
		creator, m := bootstrapFamilyModel(t, "claude", capturedClaudeCompactStyled)
		req := runtime.BootstrapInputRequest{Family: "claude", Fence: bootstrapFence}
		if got, err := creator.ObserveBootstrapInput(t.Context(), fenceModelSession, req); err != nil || got != nil {
			t.Fatal("compact Claude frame minted pre-start authority")
		}
		if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
			t.Fatal(err)
		}
		issued, ok := creator.tm.bootstrapCredit(fenceModelSession)
		if !ok {
			t.Fatal("Claude issuer has no credit")
		}
		proof, err := fenceModelProvider(m).ObserveBootstrapInput(t.Context(), fenceModelSession, req)
		if err != nil || proof == nil || proof.Token != issued.token || proof.Fence != bootstrapFence {
			t.Fatal("compact Claude recovery lost exact issued authority")
		}
		m.pid = "9999"
		if other, err := fenceModelProvider(m).ObserveBootstrapInput(t.Context(), fenceModelSession, req); err != nil || other != nil {
			t.Fatal("compact Claude frame recovered another native process")
		}
	})
	creator, m := bootstrapModel(t)
	m.screen = func(*fenceModel) string { return capturedF567StyledPristine }
	if err := creator.tm.promoteBootstrapInput(t.Context(), fenceModelSession); err != nil {
		t.Fatal(err)
	}
	issued, ok := creator.tm.bootstrapCredit(fenceModelSession)
	if !ok {
		t.Fatal("issuer has no credit")
	}
	recovered := fenceModelProvider(m) // Same native READY; controller memory is empty.
	req := runtime.BootstrapInputRequest{Family: "codex", Fence: bootstrapFence}
	proof, err := recovered.ObserveBootstrapInput(t.Context(), fenceModelSession, req)
	if err != nil || proof == nil {
		t.Fatal("exact styled native READY did not recover across controller replacement")
	}
	if proof.Fence != bootstrapFence || proof.Token != issued.token {
		t.Fatal("recovery changed issued authority")
	}
	m.pid = "9999"
	if other, err := fenceModelProvider(m).ObserveBootstrapInput(t.Context(), fenceModelSession, req); err != nil || other != nil {
		t.Fatal("styled frame recovered another native process")
	}
}

func TestBootstrapActualStyledFrameKeepsContentAndControlVetoes(t *testing.T) {
	claude := capturedClaudeCompactStyled
	for name, frame := range map[string]string{
		"Claude prior history":             "Previous assistant answer\n" + claude,
		"Claude draft":                     claudeCompactWithComposer(t, "user draft"),
		"Claude typed placeholder":         capturedClaudeCompactAPI,
		"Claude typed actual placeholder":  claudeCompactWithComposer(t, `Try "fix typecheck errors"`),
		"Claude staged paste":              claudeCompactWithComposer(t, "[Pasted text #1 +2 lines]"),
		"Claude native busy":               strings.Replace(claude, "✔ Update installed · Restart to update", "✻ Thinking… (1s · 2 tokens)", 1),
		"Claude question":                  strings.Replace(claude, "✔ Update installed · Restart to update", "Which animal?\n1. Otter\n2. Owl", 1),
		"Claude changed update":            strings.Replace(claude, "✔ Update installed · Restart to update", "Update unknown · Press Enter", 1),
		"Claude changed brand":             strings.Replace(claude, "Claude Code", "Other Tool", 1),
		"Claude unknown permission footer": strings.Replace(claude, "bypass permissions on", "unknown permissions on", 1),
		"Claude duplicate prompt":          claude + "\n❯ ",
		"Claude unknown control":           claude + "\x1b[999z",
	} {
		t.Run(name, func(t *testing.T) {
			if pristineBootstrapScreen(frame, "claude") {
				t.Fatal("unsafe compact Claude frame granted pristine input")
			}
		})
	}
	const link = "https://github.com/openai/codex/releases/latest"
	frame := capturedF567StyledPristine
	at := strings.LastIndex(frame, link)
	if at < 0 {
		t.Fatal("captured displayed release link absent")
	}
	lines := strings.Split(frame, "\n")
	composer := -1
	for i, line := range lines {
		if strings.Contains(line, "Ask Codex to do anything") {
			composer = i
		}
	}
	if composer < 0 {
		t.Fatal("captured composer absent")
	}
	withComposer := func(line string) string {
		changedScreen := append([]string(nil), lines...)
		changedScreen[composer] = line
		return strings.Join(changedScreen, "\n")
	}
	for name, unsafe := range map[string]string{
		"altered displayed link":        frame[:at] + "https://example.invalid/other" + frame[at+len(link):],
		"SGR inside hidden OSC8 target": strings.Replace(frame, "\x1b]8;;"+link, "\x1b]8;;https://git\x1b[0mhub.com/openai/codex/releases/latest", 1),
		"altered OSC8 target":           strings.Replace(frame, "\x1b]8;;"+link, "\x1b]8;;https://example.invalid/other", 1),
		"unknown OSC":                   strings.Replace(frame, "\x1b]8;;"+link, "\x1b]9;;"+link, 1),
		"incomplete OSC8":               strings.Replace(frame, "\x1b]8;;\x1b\\", "\x1b]8;", 1),
		"unknown CSI inside cwd":        strings.Replace(frame, "~/rfc8191/qualified-city", "~/rfc8191/\x1b[999zqualified-city", 1),
		"unknown CSI":                   frame + "\x1b[999z",
		"draft":                         withComposer("› user draft"),
		"native busy":                   withComposer("• Working (1s • esc to interrupt)\n› Ask Codex to do anything"),
		"startup pending":               withComposer("Connecting to MCP servers (0/1)\n› Ask Codex to do anything"),
		"history":                       "Previous assistant response.\n" + frame,
	} {
		t.Run(name, func(t *testing.T) {
			if pristineBootstrapScreen(unsafe, "codex") {
				t.Fatal("unsafe styled native frame granted pristine input")
			}
		})
	}
}
