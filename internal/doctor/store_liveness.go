package doctor

import (
	"fmt"
	"strings"
)

// StoreNotRunningMessage is what doctor reports for a store-reading check it
// did not run because the scope's bd-owned proxied store is stopped.
const StoreNotRunningMessage = "not checked: store not running"

// ProxiedStoreNotRunning reports whether scopeRoot is bound to a bd-owned
// proxied store whose proxy is not running.
//
// Doctor must never start a server, and on the proxied path any bd read of a
// stopped store starts its proxy and Dolt child: BEADS_DOLT_AUTO_START does not
// apply there, and a gc-owned scope's zero idle timeout keeps them up for good.
// The answer comes from gc's own endpoint inspection (the proxy record plus the
// process table); nothing is dialed or started. A scope that is not proxied —
// direct, doltlite, or not yet bound — is never reported as not running: its
// store does not start itself on a read.
func ProxiedStoreNotRunning(scopeRoot string) bool {
	return scopeBindingIsProviderOwnedProxied(scopeRoot) && !proxiedScopeProxyLive(scopeRoot)
}

// StoreNotRunningCheck stands in for a store-reading check whose scope's
// proxied store is stopped. It keeps the check's name, so the report still
// lists it, with StoreNotRunningMessage, and has no fix: the check's real
// answer needs a store doctor will not start.
//
// cityRunning is whether the city's controller is up. For a stopped city a
// stopped store is expected and the result is StatusOK; for a running city it
// is a fault (a dead proxy) and the result is a warning, so it is not hidden.
func StoreNotRunningCheck(name string, cityRunning bool, scopeLabels ...string) Check {
	return &storeNotRunningCheck{name: name, cityRunning: cityRunning, scopes: scopeLabels}
}

type storeNotRunningCheck struct {
	name        string
	cityRunning bool
	scopes      []string
}

func (c *storeNotRunningCheck) Name() string { return c.name }

func (c *storeNotRunningCheck) Run(_ *CheckContext) *CheckResult {
	message := StoreNotRunningMessage
	if len(c.scopes) > 0 {
		message = fmt.Sprintf("%s (%s)", StoreNotRunningMessage, strings.Join(c.scopes, ", "))
	}
	r := &CheckResult{
		Name:    c.name,
		Status:  StatusOK,
		Message: message,
		Details: []string{"doctor reads a bd-owned proxied store only while its proxy is running; a read would start the proxy and its Dolt, and doctor never starts servers. Run `gc start` and re-run doctor to check it."},
	}
	if c.cityRunning {
		r.Status = StatusWarning
		r.Message = message + " while the city's controller is running"
		r.FixHint = "the store's proxy is not running under a running city; `gc start` (or `gc bd dolt start` in the scope) restarts it, and the proxied-endpoint line in `gc doctor` shows why it is down"
	}
	return r
}

func (c *storeNotRunningCheck) CanFix() bool { return false }

func (c *storeNotRunningCheck) Fix(_ *CheckContext) error { return nil }

func (c *storeNotRunningCheck) WarmupEligible() bool { return false }
