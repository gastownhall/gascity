package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// newPostRequest creates a POST httptest request with the X-GC-Request header
// set, satisfying the CSRF protection middleware.
func newPostRequest(url string, body io.Reader) *http.Request {
	req := httptest.NewRequest("POST", url, body)
	req.Header.Set("X-GC-Request", "true")
	return req
}

// fakeState implements State for testing.
type fakeState struct {
	cfg           *config.City
	rawCfg        *config.City // optional: raw config for provenance detection
	sp            *runtime.Fake
	stores        map[string]beads.Store
	cityBeadStore beads.Store   // city-level store for session beads
	cityMailProv  mail.Provider // city-level mail provider (all mail is city-scoped)
	eventProv     events.Provider
	cityName      string
	cityPath      string
	startedAt     time.Time
	quarantined   map[string]bool
	autos         []orders.Order
	services      workspacesvc.Registry
	pokeCount     int
	extmsgSvc     *extmsg.Services
	adapterReg    *extmsg.AdapterRegistry
}

func newFakeState(t *testing.T) *fakeState {
	t.Helper()
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	return &fakeState{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{
				{Name: "worker", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
			},
			NamedSessions: []config.NamedSession{
				{Template: "worker", Dir: "myrig"},
			},
			Rigs: []config.Rig{
				{Name: "myrig", Path: "/tmp/myrig"},
			},
			Providers: map[string]config.ProviderSpec{
				"test-agent": {DisplayName: "Test Agent"},
			},
		},
		sp:           runtime.NewFake(),
		stores:       map[string]beads.Store{"myrig": store},
		cityMailProv: mp,
		eventProv:    events.NewFake(),
		cityName:     "test-city",
		cityPath:     t.TempDir(),
		startedAt:    time.Now(),
	}
}

func (f *fakeState) Config() *config.City                { return f.cfg }
func (f *fakeState) SessionProvider() runtime.Provider   { return f.sp }
func (f *fakeState) BeadStore(rig string) beads.Store    { return f.stores[rig] }
func (f *fakeState) BeadStores() map[string]beads.Store  { return f.stores }
func (f *fakeState) MailProvider(_ string) mail.Provider { return f.cityMailProv }
func (f *fakeState) MailProviders() map[string]mail.Provider {
	if f.cityMailProv == nil {
		return map[string]mail.Provider{}
	}
	return map[string]mail.Provider{f.cityName: f.cityMailProv}
}
func (f *fakeState) EventProvider() events.Provider           { return f.eventProv }
func (f *fakeState) CityName() string                         { return f.cityName }
func (f *fakeState) CityPath() string                         { return f.cityPath }
func (f *fakeState) Version() string                          { return "test" }
func (f *fakeState) StartedAt() time.Time                     { return f.startedAt }
func (f *fakeState) IsQuarantined(sessionName string) bool    { return f.quarantined[sessionName] }
func (f *fakeState) ClearCrashHistory(sessionName string)     { delete(f.quarantined, sessionName) }
func (f *fakeState) CityBeadStore() beads.Store               { return f.cityBeadStore }
func (f *fakeState) Orders() []orders.Order                   { return f.autos }
func (f *fakeState) Poke()                                    { f.pokeCount++ }
func (f *fakeState) ServiceRegistry() workspacesvc.Registry   { return f.services }
func (f *fakeState) ExtMsgServices() *extmsg.Services         { return f.extmsgSvc }
func (f *fakeState) AdapterRegistry() *extmsg.AdapterRegistry { return f.adapterReg }

func (f *fakeState) RawConfig() *config.City {
	if f.rawCfg != nil {
		return f.rawCfg
	}
	return f.cfg // fallback: raw == expanded when no packs
}

// fakeMutatorState extends fakeState with StateMutator for testing mutations.
type fakeMutatorState struct {
	*fakeState
	suspended map[string]bool
}

func newFakeMutatorState(t *testing.T) *fakeMutatorState {
	t.Helper()
	return &fakeMutatorState{
		fakeState: newFakeState(t),
		suspended: make(map[string]bool),
	}
}

func (f *fakeMutatorState) SuspendAgent(name string) error { f.suspended[name] = true; return nil }
func (f *fakeMutatorState) ResumeAgent(name string) error  { delete(f.suspended, name); return nil }
func (f *fakeMutatorState) EnableOrder(name, rig string) error {
	enabled := true
	return f.SetOrderOverrideEnabled(name, rig, &enabled)
}

func (f *fakeMutatorState) DisableOrder(name, rig string) error {
	enabled := false
	return f.SetOrderOverrideEnabled(name, rig, &enabled)
}

func (f *fakeMutatorState) SetOrderOverrideEnabled(name, rig string, enabled *bool) error {
	for i := range f.cfg.Orders.Overrides {
		if f.cfg.Orders.Overrides[i].Name == name && f.cfg.Orders.Overrides[i].Rig == rig {
			f.cfg.Orders.Overrides[i].Enabled = enabled
			return nil
		}
	}
	f.cfg.Orders.Overrides = append(f.cfg.Orders.Overrides, config.OrderOverride{
		Name:    name,
		Rig:     rig,
		Enabled: enabled,
	})
	return nil
}

func (f *fakeMutatorState) SuspendRig(name string) error {
	cfg := f.Config()
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: rig %q", configedit.ErrNotFound, name)
	}
	tmpl := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		if a.Dir != name {
			continue
		}
		expanded := expandAgent(a, f.cityName, tmpl, f.sp)
		for _, ea := range expanded {
			sessionName := agent.SessionNameFor(f.cityName, ea.qualifiedName, tmpl)
			_ = f.sp.SetMeta(sessionName, "suspended", "true")
		}
	}
	return nil
}

func (f *fakeMutatorState) ResumeRig(name string) error {
	cfg := f.Config()
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: rig %q", configedit.ErrNotFound, name)
	}
	tmpl := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		if a.Dir != name {
			continue
		}
		expanded := expandAgent(a, f.cityName, tmpl, f.sp)
		for _, ea := range expanded {
			sessionName := agent.SessionNameFor(f.cityName, ea.qualifiedName, tmpl)
			_ = f.sp.RemoveMeta(sessionName, "suspended")
		}
	}
	return nil
}

func (f *fakeMutatorState) SuspendCity() error { f.cfg.Workspace.Suspended = true; return nil }
func (f *fakeMutatorState) ResumeCity() error  { f.cfg.Workspace.Suspended = false; return nil }
func (f *fakeMutatorState) CreateAgent(a config.Agent) error {
	f.cfg.Agents = append(f.cfg.Agents, a)
	return nil
}

func (f *fakeMutatorState) UpdateAgent(name string, patch AgentUpdate) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range f.cfg.Agents {
		if f.cfg.Agents[i].Dir == dir && f.cfg.Agents[i].Name == base {
			if patch.Provider != "" {
				f.cfg.Agents[i].Provider = patch.Provider
			}
			if patch.Scope != "" {
				f.cfg.Agents[i].Scope = patch.Scope
			}
			if patch.Suspended != nil {
				f.cfg.Agents[i].Suspended = *patch.Suspended
			}
			return nil
		}
	}
	return fmt.Errorf("%w: agent %q", configedit.ErrNotFound, name)
}

func (f *fakeMutatorState) DeleteAgent(name string) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range f.cfg.Agents {
		if f.cfg.Agents[i].Dir == dir && f.cfg.Agents[i].Name == base {
			f.cfg.Agents = append(f.cfg.Agents[:i], f.cfg.Agents[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: agent %q", configedit.ErrNotFound, name)
}

func (f *fakeMutatorState) CreateRig(r config.Rig) error {
	f.cfg.Rigs = append(f.cfg.Rigs, r)
	return nil
}

func (f *fakeMutatorState) UpdateRig(name string, patch RigUpdate) error {
	for i := range f.cfg.Rigs {
		if f.cfg.Rigs[i].Name == name {
			if patch.Path != "" {
				f.cfg.Rigs[i].Path = patch.Path
			}
			if patch.Prefix != "" {
				f.cfg.Rigs[i].Prefix = patch.Prefix
			}
			if patch.Suspended != nil {
				f.cfg.Rigs[i].Suspended = *patch.Suspended
			}
			return nil
		}
	}
	return fmt.Errorf("%w: rig %q", configedit.ErrNotFound, name)
}

func (f *fakeMutatorState) DeleteRig(name string) error {
	for i := range f.cfg.Rigs {
		if f.cfg.Rigs[i].Name == name {
			f.cfg.Rigs = append(f.cfg.Rigs[:i], f.cfg.Rigs[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: rig %q", configedit.ErrNotFound, name)
}

func (f *fakeMutatorState) CreateProvider(name string, spec config.ProviderSpec) error {
	if f.cfg.Providers == nil {
		f.cfg.Providers = make(map[string]config.ProviderSpec)
	}
	if _, exists := f.cfg.Providers[name]; exists {
		return fmt.Errorf("%w: provider %q", configedit.ErrAlreadyExists, name)
	}
	f.cfg.Providers[name] = spec
	return nil
}

func (f *fakeMutatorState) UpdateProvider(name string, patch ProviderUpdate) error {
	if f.cfg.Providers == nil {
		return fmt.Errorf("%w: provider %q", configedit.ErrNotFound, name)
	}
	spec, ok := f.cfg.Providers[name]
	if !ok {
		return fmt.Errorf("%w: provider %q", configedit.ErrNotFound, name)
	}
	if patch.DisplayName != nil {
		spec.DisplayName = *patch.DisplayName
	}
	if patch.Base != nil {
		spec.Base = *patch.Base
	}
	if patch.Command != nil {
		spec.Command = *patch.Command
	}
	if patch.Args != nil {
		spec.Args = make([]string, len(patch.Args))
		copy(spec.Args, patch.Args)
	}
	if patch.ArgsAppend != nil {
		spec.ArgsAppend = make([]string, len(patch.ArgsAppend))
		copy(spec.ArgsAppend, patch.ArgsAppend)
	}
	if patch.PromptMode != nil {
		spec.PromptMode = *patch.PromptMode
	}
	if patch.PromptFlag != nil {
		spec.PromptFlag = *patch.PromptFlag
	}
	if patch.ReadyDelayMs != nil {
		spec.ReadyDelayMs = *patch.ReadyDelayMs
	}
	if len(patch.Env) > 0 {
		if spec.Env == nil {
			spec.Env = make(map[string]string, len(patch.Env))
		}
		for k, v := range patch.Env {
			spec.Env[k] = v
		}
	}
	if patch.OptionsSchemaMerge != nil {
		spec.OptionsSchemaMerge = *patch.OptionsSchemaMerge
	}
	if patch.OptionsSchema != nil {
		spec.OptionsSchema = append([]config.ProviderOption(nil), patch.OptionsSchema...)
	}
	f.cfg.Providers[name] = spec
	return nil
}

func (f *fakeMutatorState) DeleteProvider(name string) error {
	if f.cfg.Providers == nil {
		return fmt.Errorf("%w: provider %q", configedit.ErrNotFound, name)
	}
	if _, ok := f.cfg.Providers[name]; !ok {
		return fmt.Errorf("%w: provider %q", configedit.ErrNotFound, name)
	}
	delete(f.cfg.Providers, name)
	return nil
}

func (f *fakeMutatorState) SetAgentPatch(patch config.AgentPatch) error {
	for i := range f.cfg.Patches.Agents {
		if f.cfg.Patches.Agents[i].Dir == patch.Dir && f.cfg.Patches.Agents[i].Name == patch.Name {
			f.cfg.Patches.Agents[i] = patch
			return nil
		}
	}
	f.cfg.Patches.Agents = append(f.cfg.Patches.Agents, patch)
	return nil
}

func (f *fakeMutatorState) DeleteAgentPatch(name string) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range f.cfg.Patches.Agents {
		if f.cfg.Patches.Agents[i].Dir == dir && f.cfg.Patches.Agents[i].Name == base {
			f.cfg.Patches.Agents = append(f.cfg.Patches.Agents[:i], f.cfg.Patches.Agents[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: agent patch %q", configedit.ErrNotFound, name)
}

func (f *fakeMutatorState) SetRigPatch(patch config.RigPatch) error {
	for i := range f.cfg.Patches.Rigs {
		if f.cfg.Patches.Rigs[i].Name == patch.Name {
			f.cfg.Patches.Rigs[i] = patch
			return nil
		}
	}
	f.cfg.Patches.Rigs = append(f.cfg.Patches.Rigs, patch)
	return nil
}

func (f *fakeMutatorState) DeleteRigPatch(name string) error {
	for i := range f.cfg.Patches.Rigs {
		if f.cfg.Patches.Rigs[i].Name == name {
			f.cfg.Patches.Rigs = append(f.cfg.Patches.Rigs[:i], f.cfg.Patches.Rigs[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: rig patch %q", configedit.ErrNotFound, name)
}

func (f *fakeMutatorState) SetProviderPatch(patch config.ProviderPatch) error {
	for i := range f.cfg.Patches.Providers {
		if f.cfg.Patches.Providers[i].Name == patch.Name {
			f.cfg.Patches.Providers[i] = patch
			return nil
		}
	}
	f.cfg.Patches.Providers = append(f.cfg.Patches.Providers, patch)
	return nil
}

func (f *fakeMutatorState) DeleteProviderPatch(name string) error {
	for i := range f.cfg.Patches.Providers {
		if f.cfg.Patches.Providers[i].Name == name {
			f.cfg.Patches.Providers = append(f.cfg.Patches.Providers[:i], f.cfg.Patches.Providers[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: provider patch %q", configedit.ErrNotFound, name)
}

func intPtr(n int) *int { return &n }
