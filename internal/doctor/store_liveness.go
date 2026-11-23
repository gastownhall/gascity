package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// StoreNotRunningMessage is what doctor reports for a store-reading check it
// did not run because the scope's bd-owned proxied store is stopped.
const StoreNotRunningMessage = "not checked: store not running"

// StoreSuspendedMessage is what doctor reports for a store-reading check it
// did not run because the scope belongs to a suspended rig or city.
const StoreSuspendedMessage = "not checked: suspended"

// ProxiedStoreScope reports whether scopeRoot is bound to a bd-owned proxied
// store, the kind any bd read starts.
func ProxiedStoreScope(scopeRoot string) bool {
	return scopeBindingIsProviderOwnedProxied(scopeRoot)
}

// ProxiedStoreRetiresWhenIdle reports whether a proxied scope's proxy retires
// itself after an idle timeout: its sidecar, which every gc-owned proxied
// scope carries, names a finite one or leaves bd's default in place. A scope
// with no readable sidecar answers false, the conservative "should be up".
//
// A stopped proxy on such a scope is idle-retired rather than down: the next
// bd command restarts it, so under a running city doctor may read it.
func ProxiedStoreRetiresWhenIdle(scopeRoot string) bool {
	sidecar, err := proxyendpoint.ReadSidecar(scopeBeadsDir(scopeRoot))
	if err != nil || !sidecar.Present {
		return false
	}
	return sidecar.IdlePolicy().Kind == proxyendpoint.IdleFinite
}

// StoreSuspendedCheck stands in for a store-reading check whose scope belongs
// to a suspended rig or city. Suspension is the operator asking for the scope
// to be left cold, so doctor does not wake it; the result is StatusOK.
func StoreSuspendedCheck(name string, scopeLabels ...string) Check {
	message := StoreSuspendedMessage
	if len(scopeLabels) > 0 {
		message = fmt.Sprintf("%s (%s)", StoreSuspendedMessage, strings.Join(scopeLabels, ", "))
	}
	return &standInCheck{name: name, result: CheckResult{
		Name:    name,
		Status:  StatusOK,
		Message: message,
		Details: []string{"doctor does not read a suspended scope's bd-owned proxied store: a read would start its proxy and Dolt. Resume the rig or city and re-run doctor to check it."},
	}}
}

type standInCheck struct {
	name   string
	result CheckResult
}

func (c *standInCheck) Name() string { return c.name }

func (c *standInCheck) Run(_ *CheckContext) *CheckResult {
	r := c.result
	r.Details = append([]string(nil), c.result.Details...)
	return &r
}

func (c *standInCheck) CanFix() bool { return false }

func (c *standInCheck) Fix(_ *CheckContext) error { return nil }

func (c *standInCheck) WarmupEligible() bool { return false }

// ProxiedStoreNotRunning reports whether scopeRoot is bound to a bd-owned
// proxied store whose proxy is not running.
//
// Doctor must never start a server, and on the proxied path any bd read of a
// stopped store starts its proxy and Dolt child: BEADS_DOLT_AUTO_START does not
// apply there, and the pair then stays up until its idle timeout (or for good
// on a never-idle scope).
// The answer comes from gc's own endpoint inspection (the proxy record plus the
// process table); nothing is dialed or started. Whether a stopped store is a
// fault (a never-idle proxy that died) or idle-retired (ProxiedStoreRetiresWhenIdle)
// is the caller's question. A scope that is not proxied —
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
