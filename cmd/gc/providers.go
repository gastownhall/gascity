package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	eventsexec "github.com/gastownhall/gascity/internal/events/exec"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	mailexec "github.com/gastownhall/gascity/internal/mail/exec"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
	sessionsubprocess "github.com/gastownhall/gascity/internal/runtime/subprocess"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// sessionProviderName returns the session provider name.
// Priority: GC_SESSION env var → city.toml [session].provider → "" (default: tmux).
func sessionProviderName() string {
	if v := os.Getenv("GC_SESSION"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp); err == nil && cfg.Session.Provider != "" {
			return cfg.Session.Provider
		}
	}
	return ""
}

// tmuxConfigFromSession converts a config.SessionConfig into a
// sessiontmux.Config with resolved durations and defaults. If the
// config has no explicit socket name, cityName is used.
func tmuxConfigFromSession(sc config.SessionConfig, cityName, _ string) sessiontmux.Config {
	socketName := sc.Socket
	if socketName == "" {
		socketName = cityName
	}
	return sessiontmux.Config{
		SetupTimeout:       sc.SetupTimeoutDuration(),
		NudgeReadyTimeout:  sc.NudgeReadyTimeoutDuration(),
		NudgeRetryInterval: sc.NudgeRetryIntervalDuration(),
		NudgeLockTimeout:   sc.NudgeLockTimeoutDuration(),
		DebounceMs:         sc.DebounceMsOrDefault(),
		DisplayMs:          sc.DisplayMsOrDefault(),
		SocketName:         socketName,
	}
}

func providerStateDir(providerName, cityPath string) string {
	if cityPath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(cityPath)))
	return filepath.Join(supervisor.RuntimeDir(), providerName, hex.EncodeToString(sum[:4]))
}

// newSessionProviderByName constructs a runtime.Provider from a provider name.
// cityName is used to auto-default the tmux socket when none is configured.
// cityPath is used to isolate socket-based providers per city.
// Returns error instead of os.Exit, making it safe for the hot-reload path.
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "subprocess" → headless child processes
//   - "acp" → ACP (Agent Client Protocol) JSON-RPC over stdio
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - "k8s" → native Kubernetes provider (client-go)
//   - default → real tmux provider
func newSessionProviderByName(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	if strings.HasPrefix(name, "exec:") {
		return sessionexec.NewProvider(strings.TrimPrefix(name, "exec:")), nil
	}
	switch name {
	case "fake":
		return runtime.NewFake(), nil
	case "fail":
		return runtime.NewFailFake(), nil
	case "subprocess":
		if cityPath != "" {
			return sessionsubprocess.NewProviderWithDir(providerStateDir("subprocess", cityPath)), nil
		}
		return sessionsubprocess.NewProvider(), nil
	case "acp":
		cfg := sessionacp.Config{
			HandshakeTimeout:  sc.ACP.HandshakeTimeoutDuration(),
			NudgeBusyTimeout:  sc.ACP.NudgeBusyTimeoutDuration(),
			OutputBufferLines: sc.ACP.OutputBufferLinesOrDefault(),
		}
		if cityPath != "" {
			return sessionacp.NewProviderWithDir(providerStateDir("acp", cityPath), cfg), nil
		}
		return sessionacp.NewProvider(cfg), nil
	case "k8s":
		return sessionk8s.NewProvider()
	case "hybrid":
		return newHybridProvider(sc, cityName, cityPath)
	default:
		return sessiontmux.NewProviderWithConfig(tmuxConfigFromSession(sc, cityName, cityPath)), nil
	}
}

// newSessionProvider returns a runtime.Provider based on the session provider
// name (env var → city.toml → default). When the city-level provider is not
// "acp" but some agents have session = "acp", returns an auto.Provider that
// routes per-session. Startup path — exits on error.
func newSessionProvider() runtime.Provider {
	var sc config.SessionConfig
	var cityName string
	var cityPath string
	var agents []config.Agent
	var sessionTemplate string
	if cp, err := resolveCity(); err == nil {
		cityPath = cp
		if cfg, err := loadCityConfig(cp); err == nil {
			sc = cfg.Session
			cityName = cfg.Workspace.Name
			if cityName == "" {
				cityName = filepath.Base(cp)
			}
			agents = cfg.Agents
			sessionTemplate = cfg.Workspace.SessionTemplate
		}
	}
	provName := sessionProviderName()
	sp, err := newSessionProviderByName(provName, sc, cityName, cityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}
	// If the city-level provider is not ACP but some agents need ACP,
	// wrap in an auto provider that routes per-session.
	// NOTE: agents comes from loadCityConfig which applies pack overrides,
	// so the Session field from overrides is already resolved here.
	if provName != "acp" && hasACPAgents(agents) {
		acpSP, acpErr := newSessionProviderByName("acp", sc, cityName, cityPath)
		if acpErr != nil {
			fmt.Fprintf(os.Stderr, "acp provider: %v\n", acpErr) //nolint:errcheck // best-effort stderr
			os.Exit(1)
		}
		autoSP := sessionauto.New(sp, acpSP)
		// Pre-register routes for known ACP agents so one-off commands
		// (gc status, gc agent nudge, etc.) route correctly.
		// Best-effort store for bead-derived session name lookup.
		var store beads.Store
		if cityPath != "" {
			store, _ = openCityStoreAt(cityPath)
		}
		for _, a := range agents {
			if a.Session == "acp" {
				sessName := lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), sessionTemplate)
				autoSP.RouteACP(sessName)
			}
		}
		return autoSP
	}
	return sp
}

// hasACPAgents reports whether any agent in the config uses session = "acp".
func hasACPAgents(agents []config.Agent) bool {
	for _, a := range agents {
		if a.Session == "acp" {
			return true
		}
	}
	return false
}

// displayProviderName returns a human-readable provider name for logging.
func displayProviderName(name string) string {
	if name == "" {
		return "tmux (default)"
	}
	return name
}

// rawBeadsProvider returns the raw bead store provider name from config.
// Priority: GC_BEADS env var → city.toml [beads].provider → "bd" default.
// This is the unmodified config value; use beadsProvider() for lifecycle
// routing which remaps "bd" → exec:.
func rawBeadsProvider(cityPath string) string {
	if v := os.Getenv("GC_BEADS"); v != "" {
		return v
	}
	// Try to read provider from city.toml.
	cfg, err := loadCityConfig(cityPath)
	if err == nil && cfg.Beads.Provider != "" {
		return cfg.Beads.Provider
	}
	return "bd"
}

// beadsProvider returns the bead store provider name for lifecycle operations.
// Maps "bd" → "exec:<cityPath>/.gc/system/bin/gc-beads-bd" so all lifecycle operations
// route through the exec: protocol. Other providers pass through unchanged.
//
// Related env vars:
//   - GC_DOLT=skip — the gc-beads-bd script checks this and exits 2 for all
//     operations. Used by testscript and integration tests.
func beadsProvider(cityPath string) string {
	raw := rawBeadsProvider(cityPath)
	if raw == "bd" {
		return "exec:" + filepath.Join(cityPath, citylayout.SystemBinRoot, "gc-beads-bd")
	}
	return raw
}

// mailProviderName returns the mail provider name.
// Priority: GC_MAIL env var → city.toml [mail].provider → "" (default: beadmail).
func mailProviderName() string {
	if v := os.Getenv("GC_MAIL"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp); err == nil && cfg.Mail.Provider != "" {
			return cfg.Mail.Provider
		}
	}
	return ""
}

// newMailProvider returns a mail.Provider based on the mail provider name
// (env var → city.toml → default) and the given bead store (used as the
// default backend).
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - default → beadmail (backed by beads.Store, no subprocess)
func newMailProvider(store beads.Store) mail.Provider {
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") {
		return mailexec.NewProvider(strings.TrimPrefix(v, "exec:"))
	}
	switch v {
	case "fake":
		return mail.NewFake()
	case "fail":
		return mail.NewFailFake()
	default:
		return beadmail.New(store)
	}
}

// openCityMailProvider opens the city's bead store and wraps it in a
// mail.Provider. Returns (nil, exitCode) on failure.
func openCityMailProvider(stderr io.Writer, cmdName string) (mail.Provider, int) {
	// For exec: and test doubles, no store needed.
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newMailProvider(nil), 0
	}

	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		return nil, code
	}
	return newMailProvider(store), 0
}

// eventsProviderName returns the events provider name.
// Priority: GC_EVENTS env var → city.toml [events].provider → "" (default: file JSONL).
func eventsProviderName() string {
	if v := os.Getenv("GC_EVENTS"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp); err == nil && cfg.Events.Provider != "" {
			return cfg.Events.Provider
		}
	}
	return ""
}

// newEventsProvider returns an events.Provider based on the events provider
// name (env var → city.toml → default) and the given events file path (used
// as the default backend).
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - default → file-backed JSONL provider
func newEventsProvider(eventsPath string, stderr io.Writer) (events.Provider, error) {
	v := eventsProviderName()
	if strings.HasPrefix(v, "exec:") {
		return eventsexec.NewProvider(strings.TrimPrefix(v, "exec:"), stderr), nil
	}
	switch v {
	case "fake":
		return events.NewFake(), nil
	case "fail":
		return events.NewFailFake(), nil
	default:
		return events.NewFileRecorder(eventsPath, stderr)
	}
}

// openCityEventsProvider resolves the city and returns an events.Provider.
// Returns (nil, exitCode) on failure.
func openCityEventsProvider(stderr io.Writer, cmdName string) (events.Provider, int) {
	// For exec: and test doubles, no city needed.
	v := eventsProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		p, err := newEventsProvider("", stderr)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
			return nil, 1
		}
		return p, 0
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	eventsPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	p, err := newEventsProvider(eventsPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return p, 0
}

// newHybridProvider constructs a composite provider that routes sessions to
// tmux (local) or k8s (remote) based on session name. The GC_HYBRID_REMOTE_MATCH
// env var controls which sessions go to k8s. If unset, all sessions route to
// local tmux.
func newHybridProvider(sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	local := sessiontmux.NewProviderWithConfig(tmuxConfigFromSession(sc, cityName, cityPath))
	remote, err := sessionk8s.NewProvider()
	if err != nil {
		return nil, fmt.Errorf("hybrid: k8s backend: %w", err)
	}
	pattern := sc.RemoteMatch
	if v := os.Getenv("GC_HYBRID_REMOTE_MATCH"); v != "" {
		pattern = v
	}
	return sessionhybrid.New(local, remote, func(name string) bool {
		return pattern != "" && strings.Contains(name, pattern)
	}), nil
}
