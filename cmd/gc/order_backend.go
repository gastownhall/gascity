package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderScopeBackend names the backend of the bead store at scopeRoot, in the
// vocabulary an order's requires_backend uses: "dolt", "doltlite",
// "postgres", "http", or whatever other backend the metadata names.
//
// The scope's own .beads/metadata.json answers when it names a backend. A rig
// with no metadata.json of its own inherits its city's; metadata that exists
// is the scope's own answer even when it names nothing. A scope with no
// backend named anywhere uses the city's configured [beads] backend, which
// defaults to dolt. Metadata is read raw rather than through the validating
// loader, because that loader refuses a backend this build does not project
// (http on a build that wires no remote backend), and an order's
// applicability must still be answerable there.
func orderScopeBackend(cityPath, scopeRoot string) string {
	if strings.TrimSpace(scopeRoot) == "" {
		scopeRoot = cityPath
	}
	path := scopeMetadataJSONPath(scopeRoot)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) && strings.TrimSpace(cityPath) != "" && !samePath(cityPath, scopeRoot) {
		path = scopeMetadataJSONPath(cityPath)
	}
	var meta struct {
		Backend  string `json:"backend"`
		Database string `json:"database"`
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &meta) //nolint:errcheck // malformed metadata names no backend
	}
	backend := strings.ToLower(strings.TrimSpace(meta.Backend))
	if backend == "" {
		backend = beadsBackend(cityPath)
	}
	if backend == "bd" {
		backend = "dolt"
	}
	if backend == "dolt" && strings.EqualFold(strings.TrimSpace(meta.Database), "doltlite") {
		// A doltlite scope may leave backend at the dolt default and name the
		// engine in database instead (cmd/gc/cmd_bd_store_bridge.go).
		return "doltlite"
	}
	return backend
}

// orderScopeBackendFn is the scope-backend resolver every order path uses. A
// var so tests can count resolutions.
var orderScopeBackendFn = orderScopeBackend

// orderInapplicableBackend reports whether order a declares requires_backend
// and its bead scope uses a backend it does not name. It returns the scope's
// backend when the order is inapplicable there. An order that declares
// nothing, or whose scope cannot be resolved, is never inapplicable: the
// dispatcher's own resolution reports a scope it cannot resolve.
func orderInapplicableBackend(cityPath string, cfg *config.City, a orders.Order) (string, bool) {
	if a.RequiresBackend == nil {
		return "", false
	}
	target, err := resolveOrderStoreTarget(cityPath, cfg, a)
	if err != nil {
		return "", false
	}
	backend := orderScopeBackendFn(cityPath, target.ScopeRoot)
	if a.AppliesToBackend(backend) {
		return "", false
	}
	return backend, true
}

// orderInapplicableLabel is how gc order list and gc doctor show an order
// that does not apply to its scope's backend.
func orderInapplicableLabel(backend string) string {
	return "inapplicable (backend " + backend + ")"
}

// orderInapplicableReason is the one-line explanation every refusal of an
// inapplicable order gives: the label, what the order requires, and what the
// scope uses.
func orderInapplicableReason(a orders.Order, backend string) string {
	return fmt.Sprintf("order %q is %s: it requires backend %s, and its scope uses %s",
		a.ScopedName(), orderInapplicableLabel(backend), strings.Join(a.RequiresBackend, " or "), backend)
}
