package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// workerPersonaMarker is the claim instruction every worker persona carries,
// the core pool-worker prompt and the embedded fallback alike. It is what turns
// a client's provider chat into a queue worker.
const workerPersonaMarker = "gc hook --claim"

// implicitProviderCityTOML configures the claude provider without composing the
// core pack, so the claude implicit agent's prompt is the embedded fallback.
const implicitProviderCityTOML = `
[workspace]
name = "gastown"

[providers.claude]
base = "builtin:claude"
`

// writePrimeCity writes a city from cityTOML plus the given city-relative files
// and returns its directory.
func writePrimeCity(t *testing.T, cityTOML string, files map[string]string) string {
	t.Helper()
	cityDir := t.TempDir()
	files["city.toml"] = cityTOML
	for rel, content := range files {
		path := filepath.Join(cityDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", rel, err)
		}
	}
	return cityDir
}

// primeSessionIn runs prime for a session of template in cityDir whose GC_ALIAS
// and GC_SESSION_ORIGIN are alias and origin, the way the session's
// SessionStart hook (hookMode) or an explicit `gc prime` inside it does, and
// returns what prime writes.
func primeSessionIn(t *testing.T, cityDir, template, alias, origin string, hookMode bool) string {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", alias)
	t.Setenv("GC_TEMPLATE", template)
	t.Setenv("GC_SESSION_NAME", "s-gc-chat")
	t.Setenv("GC_SESSION_ID", "gc-chat")
	t.Setenv("GC_SESSION_ORIGIN", origin)

	var stdout, stderr bytes.Buffer
	if code := doPrimeWithMode(nil, &stdout, &stderr, hookMode, false); code != 0 {
		t.Fatalf("doPrimeWithMode() = %d, want 0; stderr=%q", code, stderr.String())
	}
	return stdout.String()
}

// primeImplicitProviderSession runs prime for a session of the claude
// provider's implicit agent, the template every
// `POST /sessions {"kind":"provider","name":"claude"}` session gets.
func primeImplicitProviderSession(t *testing.T, alias, origin string, hookMode bool) string {
	t.Helper()
	cityDir := writePrimeCity(t, implicitProviderCityTOML, map[string]string{})
	return primeSessionIn(t, cityDir, "claude", alias, origin, hookMode)
}

// TestDoPrimeWithHook_ManualProviderSessionGetsNoPoolWorkerPersona pins #7662.
// A client's provider chat runs under the provider's implicit agent only
// because that agent is named after the provider. Priming it as a pool worker
// told the agent to claim queue work and drain-ack, and live it did exactly
// that after the client's first message, ending the conversation.
func TestDoPrimeWithHook_ManualProviderSessionGetsNoPoolWorkerPersona(t *testing.T) {
	worker := primeImplicitProviderSession(t, "my-chat", "ephemeral", true)
	if !strings.Contains(worker, workerPersonaMarker) {
		t.Fatalf("a pool-spawned session of the implicit agent was not primed as a worker; the control case is broken\nprime output: %q", worker)
	}

	chat := primeImplicitProviderSession(t, "my-chat", "manual", true)
	for _, line := range strings.Split(worker, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 24 {
			continue
		}
		if strings.Contains(chat, line) {
			t.Fatalf("a client-created provider session was primed with the worker persona (%q)\nprime output: %q", line, chat)
		}
	}
	if strings.TrimSpace(chat) != "" {
		t.Fatalf("a client-created provider session was primed; want no output, as for an unmanaged hook\nprime output: %q", chat)
	}
}

// TestDoPrimeWithHook_ManualSessionAliasedAsQueueIdentityKeepsWorkerPersona
// pins the exception: a manual session deliberately aliased as the implicit
// agent's own queue identity (`gc session new claude --alias claude`) is
// admitted to that agent's routed work by the work query's origin gate
// (namedSelfTargetAdmit), so it is a worker its operator chose to create and
// keeps the persona that tells it how to claim that work.
func TestDoPrimeWithHook_ManualSessionAliasedAsQueueIdentityKeepsWorkerPersona(t *testing.T) {
	out := primeImplicitProviderSession(t, "claude", "manual", true)
	if !strings.Contains(out, workerPersonaMarker) {
		t.Fatalf("a manual session aliased as the queue identity lost the worker persona, yet the work query still admits it to routed work\nprime output: %q", out)
	}
}

// TestDoPrimeWithHook_ManualConfiguredAgentSessionKeepsItsPrompt pins that the
// gate keys on the implicit agent: a manual session of a configured [[agent]]
// is primed with that agent's own prompt, as before.
func TestDoPrimeWithHook_ManualConfiguredAgentSessionKeepsItsPrompt(t *testing.T) {
	cityDir := writePrimeCity(t, implicitProviderCityTOML+`
[[agent]]
name = "helper"
provider = "claude"
prompt_template = "prompts/helper.template.md"
`, map[string]string{"prompts/helper.template.md": "You are the helper agent of {{ .AgentName }}.\n"})

	out := primeSessionIn(t, cityDir, "helper", "my-chat", "manual", true)
	if !strings.Contains(out, "You are the helper agent of") {
		t.Fatalf("a manual session of a configured agent lost its prompt\nprime output: %q", out)
	}
}

// TestDoPrimeExplicit_ManualProviderSessionStillGetsWorkerPrompt pins that only
// the hook is gated: an explicit `gc prime` from inside a client's provider chat
// is someone asking for the prompt, and it still gets it.
func TestDoPrimeExplicit_ManualProviderSessionStillGetsWorkerPrompt(t *testing.T) {
	out := primeImplicitProviderSession(t, "my-chat", "manual", false)
	if !strings.Contains(out, workerPersonaMarker) {
		t.Fatalf("explicit gc prime in a client's provider chat lost the worker prompt\nprime output: %q", out)
	}
}

// coreComposedProviderCity loads a city that composes the core pack and
// configures the claude and codex providers, so each provider's implicit agent
// carries the core pool-worker prompt_template as in a real city, and returns
// it with the build params the controller resolves session templates under.
func coreComposedProviderCity(t *testing.T) (*config.City, *agentBuildParams) {
	t.Helper()
	cityDir := writePrimeCity(t, implicitProviderCityTOML+`
[providers.codex]
base = "builtin:codex"
`, map[string]string{})
	writeBuiltinImportsFixture(t, cityDir, "core")
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	return cfg, newAgentBuildParams("gastown", cityDir, cfg, runtime.NewFake(), testBeaconTime, nil, io.Discard)
}

// controllerStartupPrompts resolves info's template the way the controller does
// when it starts or resumes the session, and returns every channel that carries
// the startup prompt: the launch payload (prompt suffix and nudge) on a fresh
// start and the prompt nudge on a resume.
func controllerStartupPrompts(t *testing.T, bp *agentBuildParams, cfg *config.City, agent *config.Agent, info session.Info) (string, map[string]string) {
	t.Helper()
	tp, err := resolveDiscoveredSessionTemplate(bp, cfg, agent, info)
	if err != nil {
		t.Fatalf("resolveDiscoveredSessionTemplate(%s): %v", info.ID, err)
	}
	launch, _, err := templateParamsToConfigWithDelivery(tp)
	if err != nil {
		t.Fatalf("templateParamsToConfigWithDelivery(%s): %v", info.ID, err)
	}
	return tp.Prompt, map[string]string{
		"launch":         launch.PromptSuffix + "\n" + launch.Nudge,
		"resume nudge":   restartPromptNudge(tp.Prompt, tp.Hints.Nudge),
		"startup prompt": tp.Prompt,
	}
}

// TestResolveDiscoveredSessionTemplate_ClientProviderChatLaunchesWithoutWorkerPersona
// pins #7662 on the controller's launch channel. The SessionStart hook stands
// down when the launch already delivered the startup prompt, so whenever the
// controller starts or resumes a client's provider chat (a wake, or a restart
// after a crash or a city restart), the prompt it delivers must carry no worker
// persona either. The chat keeps its beacon, without the "Run `gc prime`"
// instruction a hookless provider gets, since an explicit `gc prime` hands the
// persona back.
func TestResolveDiscoveredSessionTemplate_ClientProviderChatLaunchesWithoutWorkerPersona(t *testing.T) {
	cfg, bp := coreComposedProviderCity(t)
	for _, template := range []string{"claude", "codex"} {
		t.Run(template, func(t *testing.T) {
			agent := findAgentByTemplate(cfg, template)
			if agent == nil || !agent.Implicit || !strings.HasSuffix(agent.PromptTemplate, "pool-worker.template.md") {
				t.Fatalf("implicit %s agent = %+v, want the core pool-worker prompt_template", template, agent)
			}

			rows := map[string]session.Info{
				"pool worker": {
					ID: "gc-worker", Template: template, SessionNameMetadata: "s-gc-worker",
					SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "1",
				},
				"manual session aliased as the queue identity": {
					ID: "gc-queue", Template: template, SessionNameMetadata: "s-gc-queue",
					SessionOrigin: "manual", Alias: template,
				},
			}
			for name, info := range rows {
				_, channels := controllerStartupPrompts(t, bp, cfg, agent, info)
				for channel, text := range channels {
					if !strings.Contains(text, workerPersonaMarker) {
						t.Errorf("%s: %s carries no worker persona, but this session is a worker\n%s: %q", name, channel, channel, text)
					}
				}
			}

			chats := map[string]session.Info{
				"client provider chat": {
					ID: "gc-chat", Template: template, SessionNameMetadata: "s-gc-chat",
					SessionOrigin: "manual", Alias: "my-chat",
				},
				// Predates the manual-origin backfill: persisted as ephemeral,
				// without the pool markers every pool worker carries.
				"legacy client provider chat": {
					ID: "gc-legacy-chat", Template: template, SessionNameMetadata: "s-gc-legacy-chat",
					SessionOrigin: "ephemeral", Alias: "my-legacy-chat",
				},
			}
			for name, chat := range chats {
				prompt, channels := controllerStartupPrompts(t, bp, cfg, agent, chat)
				for channel, text := range channels {
					if strings.Contains(text, workerPersonaMarker) {
						t.Errorf("%s: the controller delivers the worker persona to a client's provider chat through its %s\n%s: %q", name, channel, channel, text)
					}
				}
				if want := runtime.FormatBeaconAt("gastown", chat.Alias, false, testBeaconTime); prompt != want {
					t.Errorf("%s: startup prompt = %q, want the beacon alone %q", name, prompt, want)
				}
			}
		})
	}
}

// resumedForRoutedWorkPrompt builds desired state over a store holding one open
// session of template, described by metadata, and in-progress work routed to
// template and assigned to that session, so the template's pool resumes the
// session, and returns the startup prompt the pool realization resolves for it.
func resumedForRoutedWorkPrompt(t *testing.T, cfg *config.City, bp *agentBuildParams, template string, metadata map[string]string) string {
	t.Helper()
	store := beads.NewMemStore()
	sessionName := metadata["session_name"]
	if _, err := store.Create(beads.Bead{
		Title:    sessionName,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "template:" + template},
		Metadata: metadata,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "routed work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: sessionName,
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	result := buildDesiredState("gastown", bp.cityPath, testBeaconTime, cfg, runtime.NewFake(), store, &stderr)
	tp, ok := result.State[sessionName]
	if !ok {
		t.Fatalf("desired state missing session %q; keys=%v stderr=%q", sessionName, mapKeys(result.State), stderr.String())
	}
	return tp.Prompt
}

// TestBuildDesiredState_ClientProviderChatResumedForRoutedWorkLaunchesWithoutWorkerPersona
// pins #7662 on the pool realization. Routed work assigned to a client's
// provider chat makes the chat a resume target of its agent's pool, and the
// pool realization resolves it into desired state ahead of the overlay, which
// then skips it, so the realization must start the chat with the beacon alone
// too, while the workers it resumes keep their persona.
func TestBuildDesiredState_ClientProviderChatResumedForRoutedWorkLaunchesWithoutWorkerPersona(t *testing.T) {
	cfg, bp := coreComposedProviderCity(t)
	for _, template := range []string{"claude", "codex"} {
		t.Run(template, func(t *testing.T) {
			workers := map[string]map[string]string{
				"pool worker": {
					"template": template, "session_name": "s-gc-worker", "state": "awake",
					"session_origin": "ephemeral", poolManagedMetadataKey: boolMetadata(true), "pool_slot": "1",
				},
				"manual session aliased as the queue identity": {
					"template": template, "session_name": "s-gc-queue", "state": "awake",
					"session_origin": "manual", "manual_session": "true", "alias": template,
				},
			}
			for name, metadata := range workers {
				if prompt := resumedForRoutedWorkPrompt(t, cfg, bp, template, metadata); !strings.Contains(prompt, workerPersonaMarker) {
					t.Errorf("%s: startup prompt carries no worker persona, but this session is a worker\nprompt: %q", name, prompt)
				}
			}

			prompt := resumedForRoutedWorkPrompt(t, cfg, bp, template, map[string]string{
				"template": template, "session_name": "s-gc-chat", "state": "awake",
				"session_origin": "manual", "manual_session": "true", "alias": "my-chat",
			})
			if strings.Contains(prompt, workerPersonaMarker) {
				t.Errorf("the pool realization delivers the worker persona to a client's provider chat resumed for its routed work\nprompt: %q", prompt)
			}
			if want := runtime.FormatBeaconAt("gastown", "my-chat", false, testBeaconTime); prompt != want {
				t.Errorf("client provider chat startup prompt = %q, want the beacon alone %q", prompt, want)
			}
		})
	}
}
