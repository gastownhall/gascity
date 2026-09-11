package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// proxiedProviderStoreMessage is the OK message for a scope whose store is the
// bd CLI front door by design rather than by degradation.
const proxiedProviderStoreMessage = "bd-owned proxied store (bd CLI front door)"

// pendingScopeInitMessage names the one repair for a scope whose provider-owned
// initialisation never reached the ready state.
const pendingScopeInitMessage = "beads scope initialisation pending — rerun gc start"

// targetIsProviderOwnedProxied reports whether a resolved connection target
// describes a locally bd-owned proxied topology — proxied-server mode with no
// external upstream for gc to dial.
func targetIsProviderOwnedProxied(target contract.DoltConnectionTarget) bool {
	return strings.EqualFold(strings.TrimSpace(target.DoltMode), "proxied-server") && !target.External
}

// scopeOwnershipJournal mirrors the fields doctor needs from
// .gc/scope-ownership.json. cmd/gc owns the schema and its validation; doctor
// reads the file directly because the journal lives in package main.
type scopeOwnershipJournal struct {
	Version int `json:"version"`
	Scopes  map[string]struct {
		ScopePath      string `json:"scope_path"`
		LifecycleOwner string `json:"lifecycle_owner"`
		State          string `json:"state"`
	} `json:"scopes"`
}

// scopeInitializationPending reports whether the city's ownership journal
// records scopeRoot as still initializing. An unreadable or malformed journal
// is not a pending signal: doctor reports what it can prove.
func scopeInitializationPending(cityPath, scopeRoot string) bool {
	data, err := os.ReadFile(filepath.Join(pathutil.NormalizePathForCompare(cityPath), ".gc", "scope-ownership.json"))
	if err != nil {
		return false
	}
	var journal scopeOwnershipJournal
	if err := json.Unmarshal(data, &journal); err != nil || journal.Version != 1 {
		return false
	}
	for _, entry := range journal.Scopes {
		if entry.State != "provider_initializing" || entry.ScopePath == "" {
			continue
		}
		if pathutil.SamePath(entry.ScopePath, scopeRoot) {
			return true
		}
	}
	return false
}

// pendingScopeInitResult returns the typed pending-initialisation warning for
// a scope that never finished provider-owned init, or nil when the scope is
// settled and the ordinary lens applies.
func pendingScopeInitResult(name, cityPath, scopeRoot string) *CheckResult {
	if !scopeInitializationPending(cityPath, scopeRoot) {
		return nil
	}
	return &CheckResult{
		Name:    name,
		Status:  StatusWarning,
		Message: pendingScopeInitMessage,
		FixHint: "run `gc start` to finish provider-owned beads initialisation",
	}
}
