package main

// The class contract, warned at boot (ga-f7v2ft.164 step 1).
//
// The .162 slice made one class state one requirement. This is the enumeration
// around it: every store this controller serves, every class it serves, and the
// capabilities the coming contract will demand of that pairing. Step 1 only
// says so; step 3 refuses. So the tests below assert two things in tension — the
// warning has to name enough for an operator to act on it, and it must not stop
// a boot that starts today.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// contractPreflightLog runs the contract preflight against cs and returns every
// line it wrote.
func contractPreflightLog(cs *controllerState) []string {
	var lines []string
	cs.rolloutLogf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	cs.preflightStoreContract()
	return lines
}

// TestStoreContractPreflightNamesStoreClassAndCapability is the disclosure
// itself. A store that cannot fence, cannot atomically close and emits nothing
// is a store the contract will refuse — and today an operator has no way to
// learn that before the reconciler quietly runs at half strength on it.
func TestStoreContractPreflightNamesStoreClassAndCapability(t *testing.T) {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	cs := &controllerState{cfg: &config.City{}, cityPath: t.TempDir(), cityBeadStore: store}

	lines := contractPreflightLog(cs)
	if len(lines) == 0 {
		t.Fatal("a store missing every session-class capability produced no contract warning")
	}
	joined := strings.Join(lines, "")
	for _, want := range []string{
		"store contract violation",
		`store "city"`,
		"MemStore",
		"sessions",
		"ConditionalWriter",
		"AtomicConditionalCloser",
		storeContractEmissionCapability,
		"remediation",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the contract warnings never mention %q:\n%s", want, joined)
		}
	}
}

// TestStoreContractPreflightWarnsAndDoesNotRefuse pins step 1's whole blast
// radius: log noise. The refusal is step 3, after the soak, and a preflight that
// started failing boots here would be exactly the destabilization the migration
// order exists to prevent.
func TestStoreContractPreflightWarnsAndDoesNotRefuse(t *testing.T) {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	cs := &controllerState{cfg: &config.City{}, cityPath: t.TempDir(), cityBeadStore: store}

	for _, line := range contractPreflightLog(cs) {
		if !strings.Contains(line, "WARN") {
			t.Errorf("the contract preflight wrote a non-WARN line; boot refusal belongs to step 3: %q", line)
		}
	}
}

// TestStoreContractPreflightIsSilentOnAConformingSplitCity is the control. The
// warning is only evidence while a conforming city produces none of it, and a
// converged split city built through the production opener — fenced sqlite
// binding, recorder-wired at boot, CachingStore over the work ledger — conforms
// on every class.
func TestStoreContractPreflightIsSilentOnAConformingSplitCity(t *testing.T) {
	routes, work, cfg, cityPath := serveConvergedSplitCity(t, "auto")
	ep := events.NewFake()
	cs := &controllerState{
		cfg:           cfg,
		cityPath:      cityPath,
		cityBeadStore: wrapWithCachingStore(context.Background(), work, ep, false),
		storageRoutes: routes.withControllerEmission(ep),
		eventProv:     ep,
	}

	if lines := contractPreflightLog(cs); len(lines) != 0 {
		t.Fatalf("a conforming split city produced contract warnings, so the warning means nothing:\n%s", strings.Join(lines, ""))
	}
}

// TestStoreContractPreflightEnumeratesRoutedEngines extends the .162 disclosure
// surface to the contract question. Enumerating work stores alone is what let a
// split city report a clean boot while nothing at all was known about the store
// its session rows live in; the routed engine has to appear under the same
// storage/<binding> id the status block gave it.
func TestStoreContractPreflightEnumeratesRoutedEngines(t *testing.T) {
	routes, work, cfg, cityPath := serveConvergedSplitCity(t, "auto")
	ep := events.NewFake()
	// Deliberately NOT wired: this is what an opener that forgets emission
	// leaves behind, and the enumeration is what makes it visible.
	cs := &controllerState{
		cfg:           cfg,
		cityPath:      cityPath,
		cityBeadStore: wrapWithCachingStore(context.Background(), work, ep, false),
		storageRoutes: routes,
		eventProv:     ep,
	}

	joined := strings.Join(contractPreflightLog(cs), "")
	if !strings.Contains(joined, `store "storage/infra"`) {
		t.Fatalf("the contract preflight does not enumerate the routed engine:\n%s", joined)
	}
	if !strings.Contains(joined, storeContractEmissionCapability) {
		t.Fatalf("an unwired routed engine did not produce an emission violation:\n%s", joined)
	}
	if strings.Contains(joined, "ConditionalWriter") || strings.Contains(joined, "AtomicConditionalCloser") {
		t.Errorf("the fenced sqlite binding was reported as missing a fence it has:\n%s", joined)
	}
}

// TestSetControllerStateRunsTheContractPreflight is the boot call site. Every
// test above calls the preflight directly, so all of them would stay green on a
// build that never ran it — and a contract nobody asks about at boot is the
// state ga-f7v2ft.162 found the split city in.
//
// setControllerState is the moment, and the only one: the routes cross at
// construction, so this is the first point the class front doors resolve to the
// stores that will actually serve them.
func TestSetControllerStateRunsTheContractPreflight(t *testing.T) {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	var lines []string
	cs := &controllerState{
		cfg:           &config.City{},
		cityPath:      t.TempDir(),
		cityBeadStore: store,
		rolloutLogf:   func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	}

	(&CityRuntime{}).setControllerState(cs)

	for _, line := range lines {
		if strings.Contains(line, "store contract violation") {
			return
		}
	}
	t.Fatalf("installing the controller state ran no contract preflight; boot says nothing about a store the contract will refuse:\n%s", strings.Join(lines, ""))
}

// TestStoreContractPreflightReportsOneLinePerStoreAndCapability keeps the
// enumeration readable. A store serves five infrastructure classes; five
// identical lines about one missing capability is not five violations, it is one
// violation an operator has to read five times.
func TestStoreContractPreflightReportsOneLinePerStoreAndCapability(t *testing.T) {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	cs := &controllerState{cfg: &config.City{}, cityPath: t.TempDir(), cityBeadStore: store}

	lines := contractPreflightLog(cs)
	seen := map[string]int{}
	for _, line := range lines {
		seen[line]++
	}
	for line, count := range seen {
		if count > 1 {
			t.Errorf("the contract preflight repeated one line %d times: %q", count, line)
		}
	}
	// city store: no fence, no atomic closer, no emission. Nothing else is
	// configured, so three is the whole ledger.
	if len(lines) != 3 {
		t.Errorf("contract warnings = %d, want one per missing capability on the one configured store:\n%s", len(lines), strings.Join(lines, ""))
	}
}
