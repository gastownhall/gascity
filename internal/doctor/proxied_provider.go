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

// proxiedBackupRefusal is the single statement of why a proxied scope has no
// backup. Both the per-rig dolt-backup message and the city-level advisory say
// it, and they must not drift: `backup*` is in rc.2's proxied refusal matrix
// (beads cmd/bd/proxy_capability.go), so neither gc nor bd can produce a
// recovery point for such a scope.
const proxiedBackupRefusal = "bd v1.3.0-rc.2 refuses backup on proxied scopes"

// targetIsProviderOwnedProxied reports whether a resolved connection target
// describes a locally bd-owned proxied topology — proxied-server mode with no
// external upstream for gc to dial.
func targetIsProviderOwnedProxied(target contract.DoltConnectionTarget) bool {
	return strings.EqualFold(strings.TrimSpace(target.DoltMode), "proxied-server") && !target.External
}

// scopeBindingIsProviderOwnedProxied reports whether bd's committed metadata
// binds scopeRoot to the proxied-server path. It reads metadata.json directly
// and decodes only the two fields the question needs: contract's loader also
// rejects any backend this build does not register, which is the right answer
// when opening a store and the wrong one when asking who owns a process.
// Unreadable or malformed metadata is "not proxied" — doctor reports what it
// can prove, and every caller's fallback is the ordinary lens.
func scopeBindingIsProviderOwnedProxied(scopeRoot string) bool {
	data, err := os.ReadFile(filepath.Join(pathutil.NormalizePathForCompare(scopeRoot), ".beads", "metadata.json"))
	if err != nil {
		return false
	}
	var metadata struct {
		Backend  string `json:"backend"`
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false
	}
	return contract.IsProxiedDoltMode(metadata.Backend, metadata.DoltMode)
}

// bdOwnedStoreNoun names a bd-owned scope's topology for an operator-facing
// message, so the reason a check does not apply says which shape it is.
func bdOwnedStoreNoun(scopeRoot string) string {
	if scopeBindingIsProviderOwnedProxied(scopeRoot) {
		return "bd-owned proxied store"
	}
	return "bd-owned store"
}

// bdOwnedBackupCoverageNote states what backup coverage a bd-owned scope
// actually has, which differs by transport.
//
// The proxied path has none at all: rc.2 lists backup* in its refusal matrix
// (beads cmd/bd/proxy_capability.go), so neither gc nor bd can produce a
// recovery point and the store under the proxy root is the only copy. A direct
// bd-owned scope is merely not gc's to register — `bd backup` still works
// against it — so its note must not borrow the proxied claim.
func bdOwnedBackupCoverageNote(scopeRoot string) string {
	if scopeBindingIsProviderOwnedProxied(scopeRoot) {
		return "no gc or bd backup exists for it (" + proxiedBackupRefusal + ")"
	}
	return "Dolt backups are not gc's to register here; back it up through bd"
}

// scopeIsProviderOwned reports whether bd owns this scope's Dolt lifecycle.
//
// Two signals, the same ones cmd/gc classifies on: the city's ownership journal
// names the scope, or bd's committed metadata binds it to the proxied path — an
// arm that matters on its own because a workspace migrated in place, or cloned
// from a proxied city, arrives with no journal entry at all.
//
// The transport is not part of the question. A bd-owned direct scope runs its
// Dolt under bd's root exactly as a proxied one does; only the process in front
// of it differs.
func scopeIsProviderOwned(cityPath, scopeRoot string) bool {
	return scopeJournaledToProvider(cityPath, scopeRoot) || scopeBindingIsProviderOwnedProxied(scopeRoot)
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
	return scopeJournalStateIs(cityPath, scopeRoot, "provider_initializing")
}

// scopeJournaledToProvider reports whether the journal records scopeRoot at all,
// in any state. Ownership is recorded on the first write and cleared only when
// the scope is detached, so state answers "how far did init get", not "whose
// lifecycle is this".
func scopeJournaledToProvider(cityPath, scopeRoot string) bool {
	return scopeJournalStateIs(cityPath, scopeRoot, "")
}

// scopeJournalStateIs reads gc's ownership journal and reports whether it holds
// a provider record for scopeRoot, optionally narrowed to one state. An
// unreadable or malformed journal is not a signal: doctor reports what it can
// prove.
func scopeJournalStateIs(cityPath, scopeRoot, state string) bool {
	data, err := os.ReadFile(filepath.Join(pathutil.NormalizePathForCompare(cityPath), ".gc", "scope-ownership.json"))
	if err != nil {
		return false
	}
	var journal scopeOwnershipJournal
	if err := json.Unmarshal(data, &journal); err != nil || journal.Version != 1 {
		return false
	}
	for _, entry := range journal.Scopes {
		if entry.ScopePath == "" || entry.LifecycleOwner != "provider" {
			continue
		}
		if state != "" && entry.State != state {
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
