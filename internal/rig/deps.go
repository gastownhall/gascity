package rig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

// ErrCloneFailed wraps a git-clone failure on the rig-add path so callers can
// classify it across an interface boundary (the async server maps it to a
// clone_failed request.failed error_code, distinct from a generic
// provision_failed). It is errors.Is-matchable; the underlying git error is
// already URL-redacted by git.Clone before it reaches this wrapper.
var ErrCloneFailed = errors.New("git clone failed")

// Deps carries everything the provisioning core needs. It follows the
// internal/sling.SlingDeps discipline: a small set of required infra fields plus
// nil-optional injected funcs, so both the CLI (cmd/gc) and the API-side
// controllerState can drive the same core without internal/rig importing
// package main.
//
// validateDeps requires the three infra fields (FS, CityPath, Cfg) plus the
// four funcs every successful provision reaches (ComposePacks, InitStore,
// InitAndHook, WriteRoutes) — the last of these runs AFTER the config write, so
// a nil there would panic past the rollback and strand half-written topology.
// The remaining funcs are nil-optional ("nil = skip"), matching sling's
// convention; NormalizeScopes is checked at the config-write step because a
// plain re-add skips writing.
type Deps struct {
	// FS is the filesystem seam. cmd/gc passes fsys.OSFS{}; tests pass a fake.
	FS fsys.FS
	// CityPath is the absolute path to the city directory.
	CityPath string
	// Cfg is the city config the caller loaded for edit.
	Cfg *config.City

	// InitStore initializes the rig's bead store (cmd/gc initDirIfReady). It
	// returns deferred=true when live init is punted to the controller/startup.
	// Required.
	InitStore func(cityPath, dir, prefix string) (deferred bool, err error)
	// InitAndHook is the deferred-fallback deeper store init (cmd/gc
	// initAndHookDir); its error is intentionally swallowed (reported as
	// "deferred to controller"). Required — it is reached whenever InitStore
	// defers and the store is not GC_DOLT=skip, a path a caller cannot predict.
	InitAndHook func(cityPath, dir, prefix string) error
	// ComposePacks resolves the rig's bundled imports and returns a commit closure
	// that writes packs.lock only AFTER the city.toml append (cmd/gc
	// ensureBundledRigImportsInstalled), preserving the "city.toml written last"
	// atomicity invariant. Required.
	ComposePacks func(cityPath string, imports []config.BoundImport) (pinned []config.BoundImport, commit func() error, err error)
	// WriteRoutes regenerates every rig's routes.jsonl (cmd/gc
	// collectRigRoutes + writeAllRoutes). Required — it runs after the config
	// write, so a nil here would panic past the topology rollback.
	WriteRoutes func(cityPath string, cfg *config.City) error
	// ProbeBranch returns the rig's git default branch, or "" when unknown.
	// nil = skip the probe.
	ProbeBranch func(rigPath string) string
	// CloneGitURL populates staging dir dstDir from gitURL with the hardened
	// clone (C3/G15). nil = the caller does not support --git-url (the CLI local
	// path, or config-append-only), so the clone step is skipped and the flow
	// stays byte-identical to a local `gc rig add <path>`. When set and it fails,
	// provisioning is fatal and the server orchestration (C4) removes the staging
	// dir on rollback. The ctx is the caller's provisioning deadline (G21);
	// Provision passes context.Background() until it threads a deadline of its
	// own. The staging-dir→rename orchestration and the pre-clone SSRF host fence
	// (internal/ssrf) live in the caller, not here.
	CloneGitURL func(ctx context.Context, gitURL, dstDir string, opts git.CloneOptions) error
	// NormalizeScopes reconciles canonical bd metadata/config/port mirrors before
	// the config write (cmd/gc normalizeCanonicalBdScopeFiles). Runs under
	// rollback protection. nil = fatal at the config-write step for callers that
	// write config.
	NormalizeScopes func(cityPath string, cfg *config.City) error
	// PrepareAdopt readies provider state for --adopt (cmd/gc
	// prepareRigAdoptProviderState). nil = skip; only consulted when req.Adopt.
	PrepareAdopt func(cityPath, rigPath string) error
	// StoreContract reports whether the city uses the bd store contract (cmd/gc
	// cityUsesBdStoreContract). It is a func, not a bool, because InitStore can
	// seed provider state mid-flow and the flow re-evaluates it after init. nil =
	// false.
	StoreContract func(cityPath string) bool
	// DoltSkip reports GC_DOLT=skip (cmd/gc gcDoltSkip). nil = false.
	DoltSkip func() bool
	// PostProvision runs caller-specific side effects after the core writes
	// succeed (CLI: hooks/formulas/.env/reload; API: the mutateAndPoke config
	// commit + reconciler Poke). nil = skip. Its error does NOT trigger rollback
	// (the disk writes are already committed) — Provision captures it in
	// ProvisionResult.PostProvisionErr for the caller to surface.
	PostProvision func(pc ProvisionContext) error

	// OnStep receives incremental provisioning progress. The CLI renders strings;
	// the API emits typed events (G20). nil = no-op. This push seam is the one
	// deliberate departure from SlingDeps, whose warnings ride the return struct.
	OnStep func(step ProvisionStep)
}

// ProvisionRequest is the caller's rig-add intent. It mirrors the current
// doRigAddWithResult parameters minus the fs and the io.Writers.
type ProvisionRequest struct {
	Name           string
	Path           string // resolved rig path; the caller does any CWD-relative resolution
	Prefix         string // explicit prefix override; "" derives from Name
	DefaultBranch  string // explicit override; "" probes via Deps.ProbeBranch
	Includes       []string
	StartSuspended bool
	Adopt          bool
	// GitURL, when set, is the remote the rig is cloned from at provisioning time
	// via Deps.CloneGitURL (C3/G15). It is consumed by the clone and never
	// persisted — no config.Rig field records it — so an embedded credential
	// (https://user:token@host) cannot land in city.toml. Empty = no clone (the
	// local `gc rig add <path>` path).
	GitURL string
	// RecurseSubmodules opts the provisioning clone back into submodule fetch.
	// Off by default: a submodule URL is a second untrusted-URL surface the
	// pre-clone SSRF host fence never saw.
	RecurseSubmodules bool
}

// ProvisionResult carries the structured outcome the caller renders (CLI
// strings) or projects onto events/JSON (API). It replaces the stdout/stderr
// writers the CLI function used to take.
type ProvisionResult struct {
	// Deferred reports that bead-store init was punted to the controller.
	Deferred bool
	// Warnings holds warn-and-continue messages (non-fatal steps).
	Warnings []string
	// Steps is the ordered progress trace (also delivered live via Deps.OnStep).
	Steps []ProvisionStep
	// PostProvisionErr holds a non-nil error returned by Deps.PostProvision. The
	// disk writes are already committed when PostProvision runs, so this is not a
	// rollback trigger; the caller decides how to surface it. Always nil on the
	// CLI path (its PostProvision always returns nil).
	PostProvisionErr error
}

// ProvisionStep is one unit of provisioning progress
// (e.g. "beads-init", "packs", "config", "routes").
type ProvisionStep struct {
	Name   string // stable machine name
	Detail string // human-readable detail
	Warn   bool   // true when the step reports a warn-and-continue condition
}

// ProvisionContext is handed to Deps.PostProvision after the core writes succeed.
type ProvisionContext struct {
	RigPath  string
	Rig      config.Rig
	Deferred bool
	// Cfg is the post-write effective config (nextCfg). Treat it as
	// read-only-except-Rigs: it is a shallow copy of the caller's config (or, on
	// a plain re-add, the caller's config itself), so mutating a nested field
	// would corrupt shared state. An API PostProvision installing controller
	// state should re-load or deep-copy rather than retaining this pointer.
	Cfg *config.City
}

// validateRequest rejects a structurally-invalid rig-add request before any
// provisioning work. Name and Path are always required; the caller is
// responsible for resolving Path to an absolute location.
func validateRequest(req ProvisionRequest) error {
	if req.Name == "" {
		return errors.New("rig: ProvisionRequest.Name is required")
	}
	if req.Path == "" {
		return errors.New("rig: ProvisionRequest.Path is required")
	}
	if !filepath.IsAbs(req.Path) {
		// The caller resolves any CWD-relative input; an absolute path keeps a
		// server-side provisioner from resolving client input against the daemon
		// CWD. The CLI always passes an absolute path (resolveRigAddPath).
		return errors.New("rig: ProvisionRequest.Path must be absolute")
	}
	return nil
}

// validateDeps enforces the required infra fields. Injected funcs are validated
// lazily at their step (see the Deps field docs), matching sling's validateDeps.
func validateDeps(d Deps) error {
	if d.FS == nil {
		return depErr("FS")
	}
	if d.CityPath == "" {
		return depErr("CityPath")
	}
	if d.Cfg == nil {
		return depErr("Cfg")
	}
	if d.ComposePacks == nil {
		return depErr("ComposePacks")
	}
	if d.InitStore == nil {
		return depErr("InitStore")
	}
	if d.InitAndHook == nil {
		return depErr("InitAndHook")
	}
	if d.WriteRoutes == nil {
		return depErr("WriteRoutes")
	}
	return nil
}

// depErr is the error shape validateDeps returns for a missing required field.
func depErr(field string) error {
	return fmt.Errorf("rig: Deps.%s is required", field)
}
