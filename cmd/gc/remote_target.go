package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// Persistent flags that select a REMOTE city over the HTTP+SSE control
// plane instead of a local city directory. Empty means "no remote selection"
// (fall through to local city discovery). These live here — next to the
// resolver that consumes them — rather than in main.go, so the resolver is a
// self-contained, testable unit; main.go only registers them and resets them
// between runs.
var (
	// cityURLFlag holds --city-url: an ad-hoc remote terminus. Paired with
	// --city-name. Reconciled as the target of the existing --api alias.
	cityURLFlag string
	// cityNameFlag holds --city-name: the remote city name for an ad-hoc
	// --city-url target (never overloads --city, which is a local path/name).
	cityNameFlag string
	// contextFlag holds --context: a named context from ~/.gc/contexts.toml.
	contextFlag string
)

// remoteSource labels which precedence tier produced a remoteTarget, so
// `gc context current` can report the winning tier and what it shadowed.
const (
	remoteSourceContextFlag   = "flag --context"
	remoteSourceURLFlag       = "flag --city-url"
	remoteSourceEnvContext    = "env GC_CITY_CONTEXT"
	remoteSourceEnvURL        = "env GC_CITY_URL"
	remoteSourceStickyDefault = "sticky default"
)

// remoteTarget is a fully-resolved remote city selection: where it is, which
// city it scopes to, and the credential source (if any). It is the client-side
// analog of a resolved local cityPath — resolution decides the base URL, then
// the transport layer (Phase 2) turns it into a fail-closed api.Client.
type remoteTarget struct {
	BaseURL  string                 // validated https (or loopback http) terminus
	CityName string                 // remote city name for /v0/city/{name}/ scoping
	Ctx      *clientcontext.Context // named context that supplied creds; nil for ad-hoc
	Token    string                 // ad-hoc bearer (GC_CITY_URL_TOKEN); only when Ctx==nil
	Source   string                 // winning precedence tier (a remoteSource* label)
}

// remoteSelection is the raw remote selection gathered from flags and env before
// precedence and conflict resolution. Splitting the impure gathering
// (readRemoteSelection) from the pure resolution (resolveRemoteSelection) keeps
// the precedence + conflict table unit-testable without global flag or env
// state — mirroring how city_arg_resolve.go separates lookupCityNameFacts from
// resolveCityNameRef.
type remoteSelection struct {
	urlFlag      string // --city-url (or --api, its alias)
	nameFlag     string // --city-name
	contextFlag  string // --context
	cityFlag     string // --city (local; conflict detection / env-shadow only)
	rigFlag      string // --rig (local; conflict detection / env-shadow only)
	localCityEnv bool   // GC_CITY / GC_CITY_PATH / GC_CITY_ROOT set (conflict detection)
	envURL       string // GC_CITY_URL
	envContext   string // GC_CITY_CONTEXT
	envToken     string // GC_CITY_URL_TOKEN
	noAPI        bool   // GC_NO_API truthy
}

// hasExplicitRemote reports whether any explicit (flag or env) remote selector
// is present. Used to reject a positional city argument combined with a remote
// target, and to short-circuit local discovery.
func (s remoteSelection) hasExplicitRemote() bool {
	return s.urlFlag != "" || s.contextFlag != "" || s.envURL != "" || s.envContext != ""
}

// localCityEnvPresent reports whether any explicit local city env var is set.
// It only checks presence (not validity) — a set-but-invalid value still
// signals intent to target a local city, which must not silently coexist with
// a remote env selector.
func localCityEnvPresent() bool {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// readRemoteSelection gathers the raw remote selection from the persistent
// flags and the environment. It is the impure companion to the pure
// resolveRemoteSelection, kept separate so the precedence table is testable
// without global state.
func readRemoteSelection() remoteSelection {
	noAPI, _ := classifyGCNoAPI(os.Getenv("GC_NO_API"))
	return remoteSelection{
		urlFlag:      strings.TrimSpace(cityURLFlag),
		nameFlag:     strings.TrimSpace(cityNameFlag),
		contextFlag:  strings.TrimSpace(contextFlag),
		cityFlag:     strings.TrimSpace(cityFlag),
		rigFlag:      strings.TrimSpace(rigFlag),
		localCityEnv: localCityEnvPresent(),
		envURL:       strings.TrimSpace(os.Getenv("GC_CITY_URL")),
		envContext:   strings.TrimSpace(os.Getenv("GC_CITY_CONTEXT")),
		envToken:     os.Getenv("GC_CITY_URL_TOKEN"),
		noAPI:        noAPI,
	}
}

// remoteFlagPresent reports whether an explicit remote FLAG (--city-url or
// --context) is set. Only a remote flag shares the flag tier with a positional
// city argument, so this — not the presence of a lower-precedence remote env —
// is what conflicts with a positional (a remote env is shadowed by the
// higher-precedence positional/flag, per Decision 4).
func remoteFlagPresent() bool {
	return strings.TrimSpace(cityURLFlag) != "" || strings.TrimSpace(contextFlag) != ""
}

// resolveStickyDefaultTarget loads the contexts file and resolves the sticky
// default context, if any. It is the impure companion to resolveStickyDefault,
// consulted by resolveContext only after local city discovery finds nothing.
func resolveStickyDefaultTarget() (*remoteTarget, bool, error) {
	file, err := clientcontext.Load(DefaultPath())
	if err != nil {
		return nil, false, err
	}
	return resolveStickyDefault(file)
}

// errRemoteNotSupportedYet is the capability-gate error: a remote target
// resolved, but this command only operates a local city. Remote-capable READ
// commands route through resolveContextAllowRemote + resolveReadRoute instead
// and never reach this gate.
func errRemoteNotSupportedYet() error {
	return fmt.Errorf("this command does not support a remote city (--city-url/--context) yet; remote support is being enabled incrementally")
}

// remotePositionalConflictErr rejects a positional city/rig argument combined
// with an explicit remote target, so the positional can never silently shadow
// the requested remote city.
func remotePositionalConflictErr(arg string) error {
	return fmt.Errorf("conflicting targets: positional argument %q cannot be combined with a remote city (--city-url/--context or GC_CITY_URL/GC_CITY_CONTEXT); drop one", arg)
}

// resolveRemoteTarget resolves an explicit remote target from the current flags
// and environment against ~/.gc/contexts.toml. It is the impure entry point the
// command context resolver calls; see resolveRemoteSelection for the semantics
// of the (target, handled, err) result. It does NOT consult the sticky default
// (subordinate to local discovery) — resolveContext does that after local
// discovery finds nothing.
func resolveRemoteTarget() (*remoteTarget, bool, error) {
	sel := readRemoteSelection()
	if !sel.hasExplicitRemote() {
		// No explicit remote selector: a purely-local command (including a bare
		// --city/--rig) never needs — and must not depend on — the remote
		// contexts registry. Fall through to local discovery without touching it.
		return nil, false, nil
	}
	// The contexts file is required ONLY to resolve a named context; an ad-hoc
	// --city-url / GC_CITY_URL target is self-contained. Loading it
	// unconditionally would couple a local command that merely has a remote env
	// set (but is shadowed by a local flag) to the parse health of contexts.toml.
	file := &clientcontext.File{}
	if sel.contextFlag != "" || sel.envContext != "" {
		loaded, err := clientcontext.Load(DefaultPath())
		if err != nil {
			return nil, false, err
		}
		file = loaded
	}
	return resolveRemoteSelection(sel, file)
}

// DefaultPath is the on-disk location of the client contexts registry,
// ~/.gc/contexts.toml, resolved through the shared supervisor home seam
// (GC_HOME override). This is the single place the pure clientcontext leaf is
// bound to a concrete path.
func DefaultPath() string {
	return filepath.Join(supervisor.DefaultHome(), "contexts.toml")
}

// resolveRemoteSelection applies the Decision-4 precedence and conflict rules to
// a gathered selection against the loaded contexts file. It returns:
//   - (target, true, nil)  when an explicit flag/env tier selects a remote city
//   - (nil, false, nil)    when no explicit remote selector is present (or a
//     higher-precedence LOCAL flag shadows a remote env), so the caller falls
//     through to local city discovery
//   - (nil, false, err)    on a loud conflict or an invalid target
//
// The sticky `default` tier is intentionally NOT handled here: it is subordinate
// to local city discovery (Decision 4), so the caller consults resolveStickyDefault
// only after local discovery finds nothing.
func resolveRemoteSelection(sel remoteSelection, file *clientcontext.File) (*remoteTarget, bool, error) {
	remoteFlag := sel.urlFlag != "" || sel.contextFlag != ""
	// A local --city or --rig flag both outranks a remote env selector (flag >
	// env) and, alongside a remote flag, is a same-tier remote+local conflict.
	localFlag := sel.cityFlag != "" || sel.rigFlag != ""

	if remoteFlag {
		if localFlag {
			return nil, false, remoteVsLocalFlagErr()
		}
		if sel.urlFlag != "" && sel.contextFlag != "" {
			return nil, false, remoteVsRemoteFlagErr("--city-url", "--context")
		}
		target, err := resolveRemoteFlagTier(sel, file)
		if err != nil {
			return nil, false, err
		}
		if err := guardNoAPI(sel); err != nil {
			return nil, false, err
		}
		return target, true, nil
	}

	// No remote flag. A local flag (--city/--rig) outranks any remote ENV
	// selector (explicit flag > explicit env), so defer to local resolution.
	if localFlag {
		return nil, false, nil
	}

	if sel.envURL != "" || sel.envContext != "" {
		if sel.localCityEnv {
			return nil, false, remoteVsLocalEnvErr()
		}
		if sel.envURL != "" && sel.envContext != "" {
			return nil, false, remoteVsRemoteEnvErr()
		}
		target, err := resolveRemoteEnvTier(sel, file)
		if err != nil {
			return nil, false, err
		}
		if err := guardNoAPI(sel); err != nil {
			return nil, false, err
		}
		return target, true, nil
	}

	return nil, false, nil
}

// resolveRemoteFlagTier builds a target from the --context or --city-url flag.
func resolveRemoteFlagTier(sel remoteSelection, file *clientcontext.File) (*remoteTarget, error) {
	if sel.contextFlag != "" {
		if sel.nameFlag != "" {
			return nil, contextCityNameConflictErr()
		}
		if sel.envToken != "" {
			return nil, tokenWithContextErr()
		}
		return targetFromContext(file, sel.contextFlag, remoteSourceContextFlag)
	}
	// Ad-hoc --city-url. A bearer from GC_CITY_URL_TOKEN is honored only here,
	// where there is no context credential to conflict with.
	return targetFromURL(sel.urlFlag, sel.nameFlag, sel.envToken, remoteSourceURLFlag)
}

// resolveRemoteEnvTier builds a target from GC_CITY_CONTEXT or GC_CITY_URL.
func resolveRemoteEnvTier(sel remoteSelection, file *clientcontext.File) (*remoteTarget, error) {
	if sel.envContext != "" {
		if sel.envToken != "" {
			return nil, tokenWithContextErr()
		}
		return targetFromContext(file, sel.envContext, remoteSourceEnvContext)
	}
	return targetFromURL(sel.envURL, sel.nameFlag, sel.envToken, remoteSourceEnvURL)
}

// targetFromContext resolves a named context and validates it into a target.
func targetFromContext(file *clientcontext.File, name, source string) (*remoteTarget, error) {
	ctx, ok := file.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("context %q is not defined in %s (run 'gc context list')", name, DefaultPath())
	}
	if err := ctx.Validate(); err != nil {
		return nil, err
	}
	return &remoteTarget{
		BaseURL:  ctx.URL,
		CityName: ctx.EffectiveCity(),
		Ctx:      ctx,
		Source:   source,
	}, nil
}

// targetFromURL validates an ad-hoc URL+city-name into a target. It synthesizes
// an anonymous context purely to reuse clientcontext's URL/name validation
// (one source of truth), but the resulting target carries no context (Ctx=nil):
// ad-hoc targets have no credential_command/grant_command, only an optional
// GC_CITY_URL_TOKEN bearer.
func targetFromURL(rawURL, cityName, token, source string) (*remoteTarget, error) {
	if cityName == "" {
		return nil, fmt.Errorf("a remote --city-url/GC_CITY_URL target requires --city-name to name the remote city")
	}
	probe := clientcontext.Context{Name: cityName, URL: rawURL, City: cityName}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	return &remoteTarget{
		BaseURL:  rawURL,
		CityName: cityName,
		Token:    token,
		Source:   source,
	}, nil
}

// resolveStickyDefault resolves the file's sticky `default` context into a
// target. It is consulted only when local city discovery finds nothing, so the
// git-like "local beats the sticky default" rule holds. Returns handled=false
// when no default is set; a dangling default is a loud error.
func resolveStickyDefault(file *clientcontext.File) (*remoteTarget, bool, error) {
	if file == nil || file.Default == "" {
		return nil, false, nil
	}
	target, err := targetFromContext(file, file.Default, remoteSourceStickyDefault)
	if err != nil {
		return nil, false, err
	}
	return target, true, nil
}

// guardNoAPI rejects GC_NO_API combined with a resolved remote target: the
// escape hatch means "never route through the API", which cannot coexist with
// an explicitly requested remote city (its only route IS the API). Failing
// loudly here prevents the GC_NO_API nil-return in apiClient from silently
// rerouting a remote op to local disk (gate G2).
func guardNoAPI(sel remoteSelection) error {
	if sel.noAPI {
		return remoteNoAPIConflictErr()
	}
	return nil
}

func remoteVsLocalFlagErr() error {
	return fmt.Errorf("conflicting targets: a remote city (--city-url/--context) cannot be combined with a local --city; pick one")
}

func remoteVsRemoteFlagErr(a, b string) error {
	return fmt.Errorf("conflicting remote targets: %s and %s cannot both be set; pick one", a, b)
}

func remoteVsLocalEnvErr() error {
	return fmt.Errorf("conflicting targets: a remote city (GC_CITY_URL/GC_CITY_CONTEXT) cannot be combined with a local city env (GC_CITY/GC_CITY_PATH/GC_CITY_ROOT); unset one")
}

func remoteVsRemoteEnvErr() error {
	return fmt.Errorf("conflicting remote targets: GC_CITY_URL and GC_CITY_CONTEXT cannot both be set; unset one")
}

func contextCityNameConflictErr() error {
	return fmt.Errorf("--city-name cannot be combined with --context; the context defines its own city")
}

func tokenWithContextErr() error {
	return fmt.Errorf("GC_CITY_URL_TOKEN is only honored with an ad-hoc --city-url/GC_CITY_URL target, not with a context credential")
}

func remoteNoAPIConflictErr() error {
	return fmt.Errorf("GC_NO_API disables API routing and cannot be combined with a remote city; unset GC_NO_API or drop the remote target")
}
