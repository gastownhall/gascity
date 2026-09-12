package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// scopeOwnershipFile records the durable lifecycle owner for scopes that were
// created through the provider-owned beads front door. It deliberately does
// not describe a running process; provider state remains observable from the
// provider itself.
const scopeOwnershipFile = "scope-ownership.json"

const (
	providerScopeLifecycleOwner = "provider"
	providerScopeInitializing   = "provider_initializing"
	providerScopeReady          = "ready"
)

type providerScopeIntent struct {
	Transport string `json:"transport,omitempty"`
	Target    string `json:"target,omitempty"`
}

// providerScopeEndpoint is the upstream a pending external scope must
// initialize against. It is journaled because the endpoint used to live only
// in a process-local registration: a `gc init` that died after journaling its
// pending record left no way to recover the endpoint, and every retry failed
// inside the provider adapter for want of it. A local scope owns its own
// listener and records nothing here.
type providerScopeEndpoint struct {
	Host     string `json:"host,omitempty"`
	Port     string `json:"port,omitempty"`
	Database string `json:"database,omitempty"`
}

type providerScopeOwnershipEntry struct {
	ScopePath      string              `json:"scope_path"`
	LifecycleOwner string              `json:"lifecycle_owner"`
	State          string              `json:"state"`
	Intent         providerScopeIntent `json:"intent,omitempty"`
	// Endpoint stays out of providerScopeIntent on purpose: intent equality is
	// how a retry is checked against its durable record, and an operator who
	// corrects a wrong endpoint is retrying the same topology, not requesting
	// a conflicting one.
	Endpoint providerScopeEndpoint `json:"endpoint,omitzero"`
}

type providerScopeOwnershipJournal struct {
	Version int                                    `json:"version"`
	Scopes  map[string]providerScopeOwnershipEntry `json:"scopes"`
}

func providerScopeOwnershipPath(cityPath string) string {
	return filepath.Join(normalizePathForCompare(cityPath), ".gc", scopeOwnershipFile)
}

func providerScopeOwnershipKey(cityPath, scopeRoot string) string {
	cityPath = normalizePathForCompare(cityPath)
	scopeRoot = normalizePathForCompare(scopeRoot)
	if samePath(cityPath, scopeRoot) {
		return "city"
	}
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil && cfg != nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) != "" && samePath(rig.Path, scopeRoot) {
				return "rig:" + rig.Name
			}
		}
	}
	return "path:" + scopeRoot
}

func normalizeProviderScopeIntent(intent providerScopeIntent) (providerScopeIntent, error) {
	intent.Transport = strings.ToLower(strings.TrimSpace(intent.Transport))
	intent.Target = strings.ToLower(strings.TrimSpace(intent.Target))
	if intent.Transport == "" || intent.Target == "" {
		return providerScopeIntent{}, fmt.Errorf("provider scope ownership requires both transport and target")
	}
	if intent.Transport != "direct" && intent.Transport != "proxied" {
		return providerScopeIntent{}, fmt.Errorf("unsupported provider scope transport %q", intent.Transport)
	}
	if intent.Target != "local" && intent.Target != "external" {
		return providerScopeIntent{}, fmt.Errorf("unsupported provider scope target %q", intent.Target)
	}
	return intent, nil
}

// normalizeProviderScopeEndpoint trims a journaled endpoint and drops a
// partial one. Half an endpoint is worse than none: it would satisfy the
// pending-endpoint check and then fail inside bd.
func normalizeProviderScopeEndpoint(endpoint providerScopeEndpoint) providerScopeEndpoint {
	endpoint.Host = strings.TrimSpace(endpoint.Host)
	endpoint.Port = strings.TrimSpace(endpoint.Port)
	endpoint.Database = strings.TrimSpace(endpoint.Database)
	if endpoint.Host == "" || endpoint.Port == "" {
		endpoint.Host, endpoint.Port = "", ""
	}
	return endpoint
}

// pendingProviderScopeEndpoint returns the endpoint journaled for a scope that
// is still initializing. Ready scopes have bd's own binding to read instead.
func pendingProviderScopeEndpoint(cityPath, scopeRoot string) providerScopeEndpoint {
	entry, owned, err := providerScopeOwnership(cityPath, scopeRoot)
	if err != nil || !owned || entry.State != providerScopeInitializing {
		return providerScopeEndpoint{}
	}
	return entry.Endpoint
}

func loadProviderScopeOwnershipJournal(cityPath string) (providerScopeOwnershipJournal, bool, error) {
	path := providerScopeOwnershipPath(cityPath)
	data, err := os.ReadFile(path)
	if scopeArtifactAbsent(err) {
		return providerScopeOwnershipJournal{}, false, nil
	}
	if err != nil {
		return providerScopeOwnershipJournal{}, false, fmt.Errorf("read scope ownership journal: %w", err)
	}
	var journal providerScopeOwnershipJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return providerScopeOwnershipJournal{}, false, fmt.Errorf("parse scope ownership journal: %w", err)
	}
	if journal.Version != 1 || journal.Scopes == nil {
		return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal")
	}
	for key, entry := range journal.Scopes {
		// The record has to be a path gc can act on — absolute and in cleaned
		// form — but not the exact string filepath.EvalSymlinks would produce
		// today. A scope relocated behind a symlink after it was journaled
		// still resolves to the same directory, and every lookup below already
		// compares with samePath, which resolves both sides. Requiring the
		// resolved spelling here rejected the whole journal for one relocated
		// rig, and every start and stop reads the journal before it does
		// anything, so that stranded the proxies of every other scope too.
		if key == "" || entry.ScopePath == "" || !filepath.IsAbs(entry.ScopePath) || filepath.Clean(entry.ScopePath) != entry.ScopePath {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal path for %q", key)
		}
		if strings.HasPrefix(key, "path:") && key != "path:"+entry.ScopePath {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("scope ownership journal path key mismatch for %q", key)
		}
		if key != "city" && !strings.HasPrefix(key, "rig:") && !strings.HasPrefix(key, "path:") {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal key %q", key)
		}
		if strings.HasPrefix(key, "rig:") && strings.TrimSpace(strings.TrimPrefix(key, "rig:")) == "" {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal key %q", key)
		}
		if entry.LifecycleOwner != providerScopeLifecycleOwner {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership lifecycle owner for %q", key)
		}
		switch entry.State {
		case providerScopeInitializing:
			intent, err := normalizeProviderScopeIntent(entry.Intent)
			if err != nil {
				return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid initializing scope ownership for %q: %w", key, err)
			}
			if intent.Target != "external" && entry.Endpoint != (providerScopeEndpoint{}) {
				return providerScopeOwnershipJournal{}, false, fmt.Errorf("local scope ownership for %q records an external endpoint", key)
			}
		case providerScopeReady:
			if entry.Intent != (providerScopeIntent{}) {
				return providerScopeOwnershipJournal{}, false, fmt.Errorf("ready scope ownership for %q retains initialization intent", key)
			}
			if entry.Endpoint != (providerScopeEndpoint{}) {
				return providerScopeOwnershipJournal{}, false, fmt.Errorf("ready scope ownership for %q retains its initialization endpoint", key)
			}
		default:
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership state for %q", key)
		}
	}
	// Two records that name the same directory through different spellings are
	// still a duplicate: whichever one a lookup reached first would decide the
	// scope's state. Compare resolved, the way every lookup does.
	seenPaths := make(map[string]string, len(journal.Scopes))
	for key, entry := range journal.Scopes {
		resolved := normalizePathForCompare(entry.ScopePath)
		if prior, duplicate := seenPaths[resolved]; duplicate {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("duplicate scope ownership paths for %q and %q", prior, key)
		}
		seenPaths[resolved] = key
	}
	return journal, true, nil
}

// providerScopeOwnershipHasInitializingEntry reports whether any durable
// scope in this city still needs bd's fresh provider contract. A pending rig
// is enough to require the newer bd floor even when the city scope itself is
// already ready.
func providerScopeOwnershipHasInitializingEntry(cityPath string) (bool, error) {
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil || !exists {
		return false, err
	}
	for _, entry := range journal.Scopes {
		if entry.State == providerScopeInitializing {
			return true, nil
		}
	}
	return false, nil
}

func providerScopeOwnership(cityPath, scopeRoot string) (providerScopeOwnershipEntry, bool, error) {
	_, entry, owned, err := providerScopeOwnershipRecord(cityPath, scopeRoot)
	return entry, owned, err
}

// providerScopeOwnershipRecord resolves the current configured key first,
// then the unique durable scope path. A detached path record is intentionally
// sufficient while rig.Provision initializes an adopted rig before city.toml
// is written.
func providerScopeOwnershipRecord(cityPath, scopeRoot string) (string, providerScopeOwnershipEntry, bool, error) {
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil || !exists {
		return "", providerScopeOwnershipEntry{}, false, err
	}
	return providerScopeOwnershipRecordFromJournal(journal, cityPath, scopeRoot)
}

// scopeBindingIsProviderOwnedProxied reports whether bd's durable metadata in
// this scope binds it to the proxied-server path. bd owns the proxy and its
// child Dolt process for such a scope whether or not Gas City ever journaled
// an initialization: a workspace migrated in place with
// `bd migrate from-server-to-proxied-server`, or cloned from a proxied city
// (bd git-commits .beads/metadata.json), carries the mode with no journal
// record. Malformed metadata is an error rather than a legacy classification;
// guessing who owns a live Dolt process is how a scope ends up with two.
func scopeBindingIsProviderOwnedProxied(scopeRoot string) (bool, error) {
	path := scopeMetadataJSONPath(scopeRoot)
	data, err := os.ReadFile(path)
	if scopeArtifactAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read beads metadata for scope %q: %w", scopeRoot, err)
	}
	// Decode only the two fields this question needs. contract.LoadMetadataState
	// additionally rejects any backend this build does not register, which is
	// the right answer when opening a store and the wrong one when asking who
	// owns a process: a Postgres or otherwise opaque scope simply is not
	// proxied, and refusing to classify it would break its lifecycle.
	var metadata struct {
		Backend  string `json:"backend"`
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false, fmt.Errorf("parse beads metadata for scope %q: %w", scopeRoot, err)
	}
	return contract.IsProxiedDoltMode(metadata.Backend, metadata.DoltMode), nil
}

// providerOwnedScopeIntentFromBinding derives a provider-owned scope's
// transport and target from bd's durable binding rather than from the
// ownership journal. The journal clears its intent at ready, and a scope
// classified from metadata alone never carried one, so the binding is the
// only remaining authority. The proxied sidecar — not the proxy's loopback
// listener — decides whether the upstream is external.
func providerOwnedScopeIntentFromBinding(scopeRoot string) (providerScopeIntent, bool, error) {
	proxied, err := scopeBindingIsProviderOwnedProxied(scopeRoot)
	if err != nil || !proxied {
		return providerScopeIntent{}, false, err
	}
	external, err := proxiedScopeHasExternalUpstream(scopeRoot)
	if err != nil {
		return providerScopeIntent{}, false, err
	}
	target := "local"
	if external {
		target = "external"
	}
	return providerScopeIntent{Transport: "proxied", Target: target}, true, nil
}

// providerOwnedScopeIsProxied reports whether a provider-owned scope's bd
// lifecycle runs through beads' proxied-server path. A scope still being
// initialized has no binding to read yet, so its journaled intent answers;
// every other scope answers from bd's own metadata.
func providerOwnedScopeIsProxied(scopeRoot string, entry providerScopeOwnershipEntry) (bool, error) {
	if entry.State == providerScopeInitializing {
		return entry.Intent.Transport == "proxied", nil
	}
	return scopeBindingIsProviderOwnedProxied(scopeRoot)
}

// providerOwnedScopeState resolves the effective ownership record for a scope:
// the journal entry when Gas City recorded the initialization, otherwise a
// synthesized ready record for a scope whose committed handoff or persisted
// proxied binding already puts the lifecycle in bd's hands. Both synthesized
// forms are ready by construction — the durable artifact exists only because
// bd finished writing it — so their topology comes from the binding.
func providerOwnedScopeState(cityPath, scopeRoot string) (providerScopeOwnershipEntry, bool, error) {
	entry, owned, err := providerScopeOwnership(cityPath, scopeRoot)
	if err != nil || owned {
		return entry, owned, err
	}
	transferred, err := committedBeadsHandoffOwnsScope(scopeRoot)
	if err != nil {
		return providerScopeOwnershipEntry{}, false, err
	}
	if !transferred {
		proxied, bindingErr := scopeBindingIsProviderOwnedProxied(scopeRoot)
		if bindingErr != nil || !proxied {
			return providerScopeOwnershipEntry{}, false, bindingErr
		}
	}
	return providerScopeOwnershipEntry{
		ScopePath:      normalizePathForCompare(scopeRoot),
		LifecycleOwner: providerScopeLifecycleOwner,
		State:          providerScopeReady,
	}, true, nil
}

func scopeProviderOwned(cityPath, scopeRoot string) (bool, error) {
	_, owned, err := providerOwnedScopeState(cityPath, scopeRoot)
	return owned, err
}

// providerOwnedOpRetires reports whether a lifecycle operation only retires
// provider processes. Retiring operations reach further than starting ones —
// see providerOwnedLifecycleScopeRoots.
func providerOwnedOpRetires(op string) bool {
	return op == "stop" || op == "shutdown"
}

// providerOwnedOpVisitsEveryScope reports whether a city-wide lifecycle
// operation must attempt every provider-owned scope before it reports a
// failure. Each proxied workspace has its own proxy root, so stopping is
// per-scope work: returning at the first refusal leaves every later scope's
// `bd db-proxy-child` and `dolt sql-server` resident, and a rerun repeats the
// same short-circuit because the refusing scope is still visited first. Health
// answers for the whole city too — one bad scope must not hide the state of
// the others. A starting op keeps the opposite contract: bringing up a rig
// under a city scope that just refused is not a recovery, it is a second owner.
//
// bd's `dolt stop` refuses an unverifiable proxy record unless `--force` is
// passed, and rc.2 exposes that condition (proxy.CanForceStopUnverified) only
// as unstructured message text — the JSON error envelope carries no code
// (beads cmd/bd/dolt.go stop RunE, internal/storage/dbproxy/proxy/shutdown.go).
// Signaling a PID whose identity bd could not confirm is irreversible, so gc
// surfaces the refusal for that scope and keeps going rather than escalating on
// a string match.
func providerOwnedOpVisitsEveryScope(op string) bool {
	return providerOwnedOpRetires(op) || op == "health"
}

// providerOwnedLifecycleScopeRoots lists the scope roots a city-wide provider
// lifecycle operation visits: the city plus every configured rig. A retiring
// operation additionally visits every detached `path:` record in the ownership
// journal — a rig that left city.toml by removal, rename, or an interrupted
// add. Those scopes must not be revived by start or health, but their bd
// processes are still this city's to stop, and for the same reason a city.toml
// that no longer parses must not strand them either.
func providerOwnedLifecycleScopeRoots(cityPath, op string) ([]string, error) {
	roots := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(root string) {
		root = normalizePathForCompare(root)
		if root == "" {
			return
		}
		if _, duplicate := seen[root]; duplicate {
			return
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	add(cityPath)
	// Take the packs as they are on disk. Enumerating the scopes an op visits
	// is a question, not a repair: loadCityConfig would materialize builtin
	// packs as a side effect, and a lifecycle probe has no business rewriting
	// the city's pack tree to answer it.
	cfg, cfgErr := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if cfgErr == nil && cfg != nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) != "" {
				add(rig.Path)
			}
		}
	}
	if !providerOwnedOpRetires(op) {
		// A city.toml that is not there is not a config gc failed to read: a
		// directory with no city config declares no rigs, and that is a
		// complete answer rather than a guess. Callers reach this with bare
		// scope directories — a file-provider city, a GC_DOLT=skip city, a
		// fixture that only ever had a .beads dir — and refusing them would
		// fail lifecycle operations that have nothing to do with rigs. A
		// city.toml that exists and will not parse is the other thing: a
		// starting op must not guess past it.
		if cfgErr != nil && cityConfigFilePresent(cityPath) {
			return nil, cfgErr
		}
		return roots, nil
	}
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return roots, nil
	}
	// Every journaled scope, whatever its key. A `rig:`-keyed record is
	// normally covered by cfg.Rigs above, but cfg.Rigs is empty whenever
	// city.toml will not parse — and that is precisely the case this branch
	// exists for. Filtering to `path:` records there left a normally-added
	// rig's `bd db-proxy-child` and `dolt sql-server` running after gc stop.
	// add dedupes, so configured rigs keep their configured order.
	journaled := make([]string, 0, len(journal.Scopes))
	for _, entry := range journal.Scopes {
		journaled = append(journaled, entry.ScopePath)
	}
	sort.Strings(journaled)
	for _, root := range journaled {
		add(root)
	}
	return roots, nil
}

// scopeArtifactAbsent reports whether err says a scope artifact simply is not
// there. ENOTDIR belongs with ENOENT: when .beads is a regular file — a broken
// scope a caller is usually already in the middle of diagnosing — nothing under
// it exists, and answering "the file is malformed" buries the real failure
// under an ownership error it did not cause.
func scopeArtifactAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// cityConfigFilePresent reports whether the city declares a config at all.
// It separates "there is no city.toml" from "city.toml will not load", which
// are the same error value out of loadCityConfig and very different answers to
// "which scopes does this op visit".
func cityConfigFilePresent(cityPath string) bool {
	_, err := os.Stat(filepath.Join(normalizePathForCompare(cityPath), "city.toml"))
	return err == nil
}

// cityHasProviderOwnedScope reports whether stopping this city has any
// provider-owned bd process to retire. Proxied cities publish no GC-managed
// Dolt port, so a port probe is not an answer to this question.
func cityHasProviderOwnedScope(cityPath string) (bool, error) {
	roots, err := providerOwnedLifecycleScopeRoots(cityPath, "stop")
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		owned, err := scopeProviderOwned(cityPath, root)
		if err != nil {
			return false, err
		}
		if owned {
			return true, nil
		}
	}
	return false, nil
}

// proxiedScopeProviderRoot resolves the directory bd roots a proxied scope's
// proxy and Dolt child at: the sidecar's root_path when bd wrote one, else the
// documented default .beads/dolt.
func proxiedScopeProviderRoot(scopeRoot string) (string, error) {
	beadsDir := filepath.Join(normalizePathForCompare(scopeRoot), ".beads")
	data, err := os.ReadFile(filepath.Join(beadsDir, "proxied_server_client_info.json"))
	if scopeArtifactAbsent(err) {
		return filepath.Join(beadsDir, "dolt"), nil
	}
	if err != nil {
		return "", fmt.Errorf("read proxied server binding: %w", err)
	}
	var sidecar struct {
		RootPath string `json:"root_path"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return "", fmt.Errorf("parse proxied server binding: %w", err)
	}
	root := strings.TrimSpace(sidecar.RootPath)
	if root == "" {
		return filepath.Join(beadsDir, "dolt"), nil
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(beadsDir, root)
	}
	return filepath.Clean(root), nil
}

// validateProviderOwnedProxiedScopeStore refuses to serve a scope that claims
// bd's proxied-server mode but has no store behind it. bd commits
// metadata.json and gitignores the store, so a clone of a proxied workspace
// carries the mode without the data; the first bd command against it would
// silently create an empty store and the scope would read as an empty tracker.
// Journaled scopes are exempt: their record means Gas City is driving the
// initialization, and a pending one legitimately has no store yet.
func validateProviderOwnedProxiedScopeStore(cityPath, scopeRoot string) error {
	if _, journaled, err := providerScopeOwnership(cityPath, scopeRoot); err != nil || journaled {
		return err
	}
	intent, proxied, err := providerOwnedScopeIntentFromBinding(scopeRoot)
	if err != nil || !proxied {
		return err
	}
	if intent.Target == "external" {
		// An external upstream holds the data. The local proxy root is
		// scaffolding bd recreates on demand.
		return nil
	}
	root, err := proxiedScopeProviderRoot(scopeRoot)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(root)
	switch {
	case statErr == nil:
		return nil
	case errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("beads scope %q declares bd proxied-server mode but its provider store %s does not exist; bd commits .beads/metadata.json and ignores the store itself, so a clone carries the mode without the data. Refusing to create an empty store: initialize it explicitly with --beads-transport proxied --beads-target local, or restore %s", scopeRoot, root, root)
	default:
		return fmt.Errorf("inspect proxied provider store %s: %w", root, statErr)
	}
}

func cityScopeProviderOwned(cityPath string) (bool, error) {
	return scopeProviderOwned(cityPath, cityPath)
}

func validateProviderScopeOwnership(cityPath string, cfg *config.City) error {
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	expected := map[string]string{"city": normalizePathForCompare(cityPath)}
	if cfg == nil {
		for key, entry := range journal.Scopes {
			if key == "city" && samePath(entry.ScopePath, expected[key]) {
				continue
			}
			if strings.HasPrefix(key, "path:") {
				continue
			}
			if want, ok := expected[key]; !ok || !samePath(entry.ScopePath, want) {
				return fmt.Errorf("scope ownership journal path drift for %q", key)
			}
		}
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		expected["rig:"+rig.Name] = normalizePathForCompare(rig.Path)
	}
	for key, entry := range journal.Scopes {
		if strings.HasPrefix(key, "path:") {
			continue
		}
		want, ok := expected[key]
		if !ok || !samePath(entry.ScopePath, want) {
			return fmt.Errorf("scope ownership journal path drift for %q", key)
		}
	}
	if _, cityProviderOwned := journal.Scopes["city"]; cityProviderOwned {
		for key, root := range expected {
			if key == "city" {
				continue
			}
			if _, _, recorded, err := providerScopeOwnershipRecord(cityPath, root); err != nil {
				return err
			} else if recorded {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, ".beads", "metadata.json")); scopeArtifactAbsent(err) {
				return fmt.Errorf("fresh scope %q has no provider ownership record", key)
			} else if err != nil {
				return fmt.Errorf("inspect scope %q ownership marker: %w", key, err)
			}
		}
	}
	return nil
}

// ensureFreshRigProviderOwnership records a newly added, still-uninitialized
// rig before startup validation can route it through a legacy lifecycle. The
// city entry is intentionally left alone: this boundary exists for a fresh
// rig added to an established city.
func ensureFreshRigProviderOwnership(cityPath string, cfg *config.City) error {
	if cfg == nil || !cityUsesManagedDoltBeadsLifecycle(cityPath) {
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	// Do not resolve a city's initialization topology until there is a fresh
	// rig that needs one. Existing embedded and non-Dolt cities remain valid
	// unchanged cities when no new scope is being added.
	freshRigs := make([]config.Rig, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if key, _, owned, err := providerScopeOwnershipRecord(cityPath, rig.Path); err != nil {
			return err
		} else if owned {
			if err := attachProviderScopeOwnershipRecord(cityPath, rig.Name, rig.Path, key); err != nil {
				return err
			}
		}
		initialized, err := scopeHasPersistedBeadsIdentity(rig.Path)
		if err != nil {
			return fmt.Errorf("inspect fresh rig %q: %w", rig.Name, err)
		}
		if !initialized {
			freshRigs = append(freshRigs, rig)
		}
	}
	if len(freshRigs) == 0 {
		return nil
	}
	cityInitialized, err := scopeHasPersistedBeadsIdentity(cityPath)
	if err != nil {
		return fmt.Errorf("inspect city beads identity: %w", err)
	}
	if inherits, err := cityGrantsProviderOwnershipToFreshScopes(cityPath, cityInitialized); err != nil {
		return err
	} else if !inherits {
		return nil
	}
	var intent providerScopeIntent
	if cityInitialized {
		intent, err = providerOwnershipIntentFromPersistedCity(cityPath)
	} else {
		intent, err = freshScopeProviderOwnershipIntent(cityPath, *cfg)
	}
	if err != nil {
		return err
	}
	for _, rig := range freshRigs {
		if key, _, owned, err := providerScopeOwnershipRecord(cityPath, rig.Path); err != nil {
			return err
		} else if owned {
			if err := attachProviderScopeOwnershipRecord(cityPath, rig.Name, rig.Path, key); err != nil {
				return err
			}
		} else {
			if err := persistProviderScopeOwnership(cityPath, rig.Path, intent); err != nil {
				return fmt.Errorf("record provider ownership for fresh rig %q: %w", rig.Name, err)
			}
		}
	}
	return nil
}

func attachProviderScopeOwnershipRecord(cityPath, rigName, scopeRoot, actualKey string) error {
	if !strings.HasPrefix(actualKey, "path:") || strings.TrimSpace(rigName) == "" {
		return nil
	}
	want := "rig:" + rigName
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil || !exists {
			return err
		}
		entry, ok := journal.Scopes[actualKey]
		if !ok || !samePath(entry.ScopePath, scopeRoot) {
			return fmt.Errorf("missing detached ownership for rig %q", rigName)
		}
		if existing, collision := journal.Scopes[want]; collision && !samePath(existing.ScopePath, scopeRoot) {
			return fmt.Errorf("scope ownership journal key collision for %q", want)
		}
		delete(journal.Scopes, actualKey)
		journal.Scopes[want] = entry
		return writeProviderScopeOwnershipJournal(cityPath, journal)
	})
}

// freshScopeProviderOwnershipIntent keeps every fresh rig on the city's
// durable pending intent while first initialization is incomplete. Falling
// back to the default here would silently turn an explicit direct/external
// city into a proxied/local rig after a failed preflight.
func freshScopeProviderOwnershipIntent(cityPath string, cfg config.City) (providerScopeIntent, error) {
	entry, owned, err := providerScopeOwnership(cityPath, cityPath)
	if err != nil {
		return providerScopeIntent{}, err
	}
	if owned && entry.State == providerScopeInitializing {
		return normalizeProviderScopeIntent(entry.Intent)
	}
	return (hostedDoltInitOptions{}).providerOwnershipIntent(cfg)
}

// ensureProviderScopeOwnershipBeforeInit is the common Provision boundary for
// a new scope. It runs before InitStore can invoke bd and create metadata, so
// a failed provision cannot subsequently be mistaken for a legacy store.
func ensureProviderScopeOwnershipBeforeInit(cityPath, scopeRoot string) error {
	if !cityUsesManagedDoltBeadsLifecycle(cityPath) {
		return nil
	}
	if _, owned, err := providerScopeOwnership(cityPath, scopeRoot); err != nil || owned {
		return err
	}
	initialized, err := scopeHasPersistedBeadsIdentity(scopeRoot)
	if err != nil {
		return fmt.Errorf("inspect scope beads identity: %w", err)
	}
	if initialized {
		return nil
	}
	if !cityConfigFilePresent(cityPath) {
		// The transport and target of a fresh provider-owned scope are read
		// from the city config. With no city.toml there is nothing to read and
		// nothing to infer: recording an intent here would pin a topology gc
		// invented. Leave the scope unjournaled so it keeps the legacy
		// lifecycle, which is what a directory with no city config had before
		// provider ownership existed.
		return nil
	}
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return fmt.Errorf("load city config for provider ownership: %w", err)
	}
	var intent providerScopeIntent
	if samePath(cityPath, scopeRoot) {
		intent, err = (hostedDoltInitOptions{}).providerOwnershipIntent(*cfg)
	} else {
		cityInitialized, inspectErr := scopeHasPersistedBeadsIdentity(cityPath)
		if inspectErr != nil {
			return fmt.Errorf("inspect city beads identity: %w", inspectErr)
		}
		if inherits, inheritErr := cityGrantsProviderOwnershipToFreshScopes(cityPath, cityInitialized); inheritErr != nil {
			return inheritErr
		} else if !inherits {
			return nil
		}
		if cityInitialized {
			intent, err = providerOwnershipIntentFromPersistedCity(cityPath)
		} else {
			intent, err = freshScopeProviderOwnershipIntent(cityPath, *cfg)
		}
	}
	if err != nil {
		return err
	}
	if err := persistProviderScopeOwnership(cityPath, scopeRoot, intent); err != nil {
		return fmt.Errorf("record provider ownership before store initialization: %w", err)
	}
	return nil
}

// cityGrantsProviderOwnershipToFreshScopes reports whether a scope added to
// this city inherits provider ownership. Only a city whose own lifecycle bd
// already owns — journaled, or bound to bd's proxied-server mode (R1) — hands
// that ownership on.
//
// An existing GC-managed direct city does not. Its rigs are databases on the
// city's one managed server; journaling a new one as provider-owned direct/
// local ran a bare `bd init --server` in the rig, which gives it a second Dolt
// process of its own, split the city between two lifecycle owners, and left
// the dolt pack's orders and backups covering only the managed half. 07-design
// §0/§4 keeps those cities working unchanged until the journaled handoff.
//
// An uninitialized city is not grandfathered: it has no durable binding yet,
// and `gc init` records the pending intent that this function's caller reads.
func cityGrantsProviderOwnershipToFreshScopes(cityPath string, cityInitialized bool) (bool, error) {
	if !cityInitialized {
		return true, nil
	}
	return cityScopeProviderOwned(cityPath)
}

// providerOwnershipIntentFromPersistedCity derives the topology a new rig
// inherits from the city's durable beads binding. City.toml is only a legacy
// compatibility input, so it must not reclassify an existing direct city as
// the fresh proxied-local default.
func providerOwnershipIntentFromPersistedCity(cityPath string) (providerScopeIntent, error) {
	metadata, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if err != nil {
		return providerScopeIntent{}, fmt.Errorf("load city beads metadata: %w", err)
	}
	if !ok {
		return providerScopeIntent{}, fmt.Errorf("missing city beads metadata")
	}
	if contract.IsDoltBackend(strings.TrimSpace(metadata.Backend)) && strings.EqualFold(strings.TrimSpace(metadata.DoltMode), "embedded") {
		// Embedded Beads metadata has no server transport a fresh rig can
		// inherit. Preserve the city exactly as it is and initialize the new
		// provider-owned rig with the normal fresh-scope default.
		return providerScopeIntent{Transport: "proxied", Target: "local"}, nil
	}
	state, configured, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		return providerScopeIntent{}, fmt.Errorf("load city beads config: %w", err)
	}
	target, err := providerOwnershipTargetFromBinding(cityPath, metadata.DoltMode, state, configured)
	if err != nil {
		return providerScopeIntent{}, err
	}
	resolved, err := contract.ResolveInitIntent(
		contract.InitScopeState{Initialized: true, Backend: metadata.Backend, DoltMode: metadata.DoltMode, Target: target},
		contract.InitIntent{}, contract.InitIntent{}, contract.InitIntent{}, contract.InitIntent{},
	)
	if err != nil {
		return providerScopeIntent{}, fmt.Errorf("resolve persisted city beads topology: %w", err)
	}
	if resolved.PreserveBackend {
		return providerScopeIntent{}, fmt.Errorf("city beads backend %q is authoritative; cannot initialize a managed Dolt rig", metadata.Backend)
	}
	return normalizeProviderScopeIntent(providerScopeIntent{Transport: resolved.Intent.Transport, Target: resolved.Intent.Target})
}

// providerOwnershipTargetFromBinding determines whether a durable bd binding
// owns a local process. Endpoint fields alone cannot answer that question:
// transferred direct scopes can retain a canonical loopback endpoint, while
// external proxied scopes record their upstream only in bd's sidecar.
func providerOwnershipTargetFromBinding(cityPath, doltMode string, state contract.ConfigState, configured bool) (string, error) {
	transport, err := persistedProviderTransport(doltMode)
	if err != nil {
		return "", err
	}
	if transport == "proxied" {
		external, err := proxiedScopeHasExternalUpstream(cityPath)
		if err != nil {
			return "", err
		}
		if external {
			return "external", nil
		}
		return "local", nil
	}
	// bd records the upstream a direct scope was initialized against in the
	// scope's own metadata, and a provider-owned scope carries no gc endpoint
	// keys to say so instead. Without reading it, a rig added to a city bound to
	// someone else's Dolt server inherited "local" and got a store of its own on
	// this machine — the operator's beads split across two servers.
	if binding, ok, err := contract.ReadPersistedServerBinding(fsys.OSFS{}, scopeMetadataJSONPath(cityPath)); err != nil {
		return "", fmt.Errorf("read city beads server binding: %w", err)
	} else if ok && (strings.TrimSpace(binding.DoltHost) != "" || strings.TrimSpace(binding.DoltSocket) != "") {
		return "external", nil
	}
	if !configured {
		// An old direct scope without a canonical binding predates provider
		// ownership. Leave its conservative legacy target local.
		return "local", nil
	}
	// GC's legacy direct lifecycle owns a managed_city binding even though it
	// writes auto-start=false to keep bd from starting a competing server.
	// That marker remains authoritative when a fresh rig inherits the city.
	if state.EndpointOrigin == contract.EndpointOriginManagedCity {
		return "local", nil
	}
	disabled, err := contract.ReadAutoStartDisabled(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		return "", fmt.Errorf("read city beads auto-start policy: %w", err)
	}
	if !disabled {
		return "local", nil
	}
	return "external", nil
}

func persistedProviderTransport(doltMode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(doltMode)) {
	case "", "server":
		return "direct", nil
	case "proxied-server":
		return "proxied", nil
	default:
		return "", fmt.Errorf("persisted dolt mode %q is unsupported", doltMode)
	}
}

// proxiedScopeHasExternalUpstream reads bd's durable proxied-server client
// sidecar. It intentionally does not infer locality from host spelling: a
// loopback TCP or Unix-socket upstream can still be external to this scope.
func proxiedScopeHasExternalUpstream(cityPath string) (bool, error) {
	path := filepath.Join(cityPath, ".beads", "proxied_server_client_info.json")
	data, err := os.ReadFile(path)
	if scopeArtifactAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read proxied server binding: %w", err)
	}
	var sidecar struct {
		External *struct {
			Host   string `json:"host"`
			Port   int    `json:"port"`
			Socket string `json:"socket"`
		} `json:"external"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return false, fmt.Errorf("parse proxied server binding: %w", err)
	}
	if sidecar.External == nil {
		return false, nil
	}
	if strings.TrimSpace(sidecar.External.Socket) != "" {
		return true, nil
	}
	if strings.TrimSpace(sidecar.External.Host) == "" || sidecar.External.Port < 1 || sidecar.External.Port > 65535 {
		return false, fmt.Errorf("invalid external proxied server binding in %s", path)
	}
	return true, nil
}

func persistProviderScopeOwnership(cityPath, scopeRoot string, intent providerScopeIntent) error {
	return persistProviderScopeOwnershipWithEndpoint(cityPath, scopeRoot, intent, providerScopeEndpoint{})
}

// persistProviderScopeOwnershipWithEndpoint records a pending scope together
// with the external upstream it must initialize against, so a retry in a new
// process can recover the endpoint from the journal instead of dying for want
// of it. A zero endpoint never clears one already recorded: a resume path
// legitimately has no selector input.
func persistProviderScopeOwnershipWithEndpoint(cityPath, scopeRoot string, intent providerScopeIntent, endpoint providerScopeEndpoint) error {
	intent, err := normalizeProviderScopeIntent(intent)
	if err != nil {
		return err
	}
	endpoint = normalizeProviderScopeEndpoint(endpoint)
	if intent.Target != "external" && endpoint != (providerScopeEndpoint{}) {
		return fmt.Errorf("provider scope target %q owns its own endpoint", intent.Target)
	}
	cityPath = normalizePathForCompare(cityPath)
	scopeRoot = normalizePathForCompare(scopeRoot)
	if cityPath == "" || scopeRoot == "" {
		return fmt.Errorf("scope ownership requires city and scope paths")
	}
	key := providerScopeOwnershipKey(cityPath, scopeRoot)
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil {
			return err
		}
		if !exists {
			journal = providerScopeOwnershipJournal{Version: 1, Scopes: map[string]providerScopeOwnershipEntry{}}
		}
		actualKey, current, recorded, err := providerScopeOwnershipRecordFromJournal(journal, cityPath, scopeRoot)
		if err != nil {
			return err
		}
		if recorded {
			if actualKey != key {
				// A detached path record retains this physical scope while it is
				// absent from city.toml or being adopted before that write.
				key = actualKey
			}
			if !samePath(current.ScopePath, scopeRoot) {
				return fmt.Errorf("scope ownership journal path drift for %q", key)
			}
			if current.LifecycleOwner != providerScopeLifecycleOwner {
				return fmt.Errorf("conflicting lifecycle owner for scope %q", scopeRoot)
			}
			if current.State == providerScopeReady {
				return fmt.Errorf("scope %q is already provider-owned and ready", scopeRoot)
			}
			if current.Intent != intent {
				return fmt.Errorf("conflicting provider initialization intent for scope %q", scopeRoot)
			}
			if endpoint == (providerScopeEndpoint{}) || endpoint == current.Endpoint {
				return nil
			}
			// A retry that supplies an endpoint is still the same topology; it
			// is repairing the one input the journal could not recover.
			current.Endpoint = endpoint
			journal.Scopes[key] = current
			return writeProviderScopeOwnershipJournal(cityPath, journal)
		}
		journal.Scopes[key] = providerScopeOwnershipEntry{
			ScopePath: scopeRoot, LifecycleOwner: providerScopeLifecycleOwner,
			State: providerScopeInitializing, Intent: intent, Endpoint: endpoint,
		}
		return writeProviderScopeOwnershipJournal(cityPath, journal)
	})
}

func markProviderScopeOwnershipReady(cityPath, scopeRoot string) error {
	cityPath = normalizePathForCompare(cityPath)
	scopeRoot = normalizePathForCompare(scopeRoot)
	key := providerScopeOwnershipKey(cityPath, scopeRoot)
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("missing scope ownership journal for %q", scopeRoot)
		}
		actualKey, entry, recorded, err := providerScopeOwnershipRecordFromJournal(journal, cityPath, scopeRoot)
		if err != nil {
			return err
		}
		if !recorded || entry.LifecycleOwner != providerScopeLifecycleOwner {
			return fmt.Errorf("missing provider ownership for scope %q", scopeRoot)
		}
		key = actualKey
		if !samePath(entry.ScopePath, scopeRoot) {
			return fmt.Errorf("scope ownership journal path drift for %q", key)
		}
		if entry.State == providerScopeReady {
			return nil
		}
		if entry.State != providerScopeInitializing {
			return fmt.Errorf("invalid provider ownership state for scope %q", scopeRoot)
		}
		entry.State = providerScopeReady
		entry.Intent = providerScopeIntent{}
		entry.Endpoint = providerScopeEndpoint{}
		journal.Scopes[key] = entry
		if err := writeProviderScopeOwnershipJournal(cityPath, journal); err != nil {
			return err
		}
		clearSelectorExternalInitOptions(cityPath, scopeRoot)
		return nil
	})
}

// removeProviderScopeOwnershipRecord detaches a configured rig's label before
// city.toml is mutated. The path remains durable identity so a failed config
// write stays retryable and an adopted re-add can reattach before Provision
// writes city.toml.
func removeProviderScopeOwnershipRecord(cityPath, key string) error {
	if !strings.HasPrefix(key, "rig:") || strings.TrimSpace(strings.TrimPrefix(key, "rig:")) == "" {
		return fmt.Errorf("invalid removable provider scope key %q", key)
	}
	// Legacy cities have no journal. Avoid creating the journal lock (and its
	// containing .gc directory) merely because a legacy rig is removed.
	_, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil || !exists {
		return err
	}
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil || !exists {
			return err
		}
		entry, ok := journal.Scopes[key]
		if !ok {
			return nil
		}
		pathKey := "path:" + entry.ScopePath
		if existing, collision := journal.Scopes[pathKey]; collision && !samePath(existing.ScopePath, entry.ScopePath) {
			return fmt.Errorf("scope ownership journal path collision for %q", pathKey)
		}
		delete(journal.Scopes, key)
		journal.Scopes[pathKey] = entry
		return writeProviderScopeOwnershipJournal(cityPath, journal)
	})
}

func providerScopeOwnershipRecordFromJournal(journal providerScopeOwnershipJournal, cityPath, scopeRoot string) (string, providerScopeOwnershipEntry, bool, error) {
	key := providerScopeOwnershipKey(cityPath, scopeRoot)
	var physicalKey string
	for candidate, entry := range journal.Scopes {
		if !samePath(entry.ScopePath, scopeRoot) {
			continue
		}
		if physicalKey != "" {
			return "", providerScopeOwnershipEntry{}, false, fmt.Errorf("duplicate scope ownership paths for %q and %q", physicalKey, candidate)
		}
		physicalKey = candidate
	}
	if entry, ok := journal.Scopes[key]; ok {
		if !samePath(entry.ScopePath, scopeRoot) {
			return "", providerScopeOwnershipEntry{}, false, fmt.Errorf("scope ownership journal path drift for %q", key)
		}
		return key, entry, true, nil
	}
	if physicalKey == "" {
		return "", providerScopeOwnershipEntry{}, false, nil
	}
	return physicalKey, journal.Scopes[physicalKey], true, nil
}

func withProviderScopeOwnershipLock(cityPath string, fn func() error) error {
	path := filepath.Join(normalizePathForCompare(cityPath), ".gc", "scope-ownership.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create scope ownership lock directory: %w", err)
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open scope ownership lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("scope ownership journal is busy")
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	return fn()
}

func writeProviderScopeOwnershipJournal(cityPath string, journal providerScopeOwnershipJournal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode scope ownership journal: %w", err)
	}
	data = append(data, '\n')
	path := providerScopeOwnershipPath(cityPath)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create scope ownership directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, "."+scopeOwnershipFile+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary scope ownership journal: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary scope ownership journal mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write temporary scope ownership journal: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary scope ownership journal: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary scope ownership journal: %w", err)
	}
	temporaryOpen = false
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace scope ownership journal: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open scope ownership directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		closeErr := directory.Close()
		return errors.Join(fmt.Errorf("sync scope ownership directory: %w", err), closeErr)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close scope ownership directory after sync: %w", err)
	}
	return nil
}
