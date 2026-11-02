package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// newSplitHookBudgetFixture is newHookBudgetFixture's city converged onto a
// whole-split infra binding, as maintainer-city is: sessions, mail, nudges and
// graph beads live in the binding, and the file work store holds none of them.
func newSplitHookBudgetFixture(t *testing.T) hookBudgetFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_INJECT_CONTEXT", "")

	cityDir := t.TempDir()
	writeHookBudgetCityTOML(t, cityDir)
	split := fmt.Sprintf(`
[storage.classes]
work = %q
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = %q
path = %q
`, config.StorageWorkBinding, config.StorageProviderSQLiteBeads, filepath.Join(t.TempDir(), "store"))
	cityTOML := filepath.Join(cityDir, "city.toml")
	body, err := os.ReadFile(cityTOML)
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	if err := os.WriteFile(cityTOML, append(body, split...), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)
	stubInfraControllerPing(t, 0)
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loading the split city config: %v", err)
	}
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityDir, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture city did not converge (%s): %s", report.Outcome, log.String())
	}
	resetHookBudgetProcessState(t)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{hookBudgetIdentity}, "", "", "", true, false, 0, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew = %d; stderr=%s", code, stderr.String())
	}
	work, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessions, err := cliSessionStore(work, cfg, cityDir).ListByLabel(session.LabelSession, 0)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("binding session beads = %d (%v), want 1", len(sessions), err)
	}
	resetHookBudgetProcessState(t)

	t.Setenv("GC_SESSION_ID", sessions[0].ID)
	t.Setenv("GC_SESSION_NAME", hookBudgetIdentity)
	t.Setenv("GC_ALIAS", hookBudgetIdentity)
	t.Setenv("GC_AGENT", hookBudgetIdentity)
	t.Setenv("GC_TEMPLATE", hookBudgetIdentity)
	t.Setenv(managedSessionHookEnv, "1")
	for _, seam := range []*func(string) (*api.Client, string){&mailCheckAPIClient, &activeWorkStepAPIClient} {
		prev := *seam
		*seam = func(string) (*api.Client, string) { return nil, "controller-down" }
		t.Cleanup(func() { *seam = prev })
	}
	prevPoller := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })
	return hookBudgetFixture{cityDir: cityDir, sessionID: sessions[0].ID}
}

// TestSeatHooksReadTheBindingOnASplitCity runs the three seat hooks on a city
// converged onto its infra binding, with everything they now do concurrently
// (the storage routes resolved alongside the city store open, prime's pollers
// reap, the step lookup): each finds the session, mail and nudge that live only
// in the binding, and none of them lands in, or is read from, the work store.
// A session, mail or nudge read sent to the work store would find nothing
// there, so the hook's output would lack it.
func TestSeatHooksReadTheBindingOnASplitCity(t *testing.T) {
	f := newSplitHookBudgetFixture(t)
	f.sendMail(t, "status please")
	f.queueNudge(t)

	for _, sc := range []struct {
		name string
		run  func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int
		want []string
	}{
		{name: "prime --hook", run: primeHook, want: []string{hookBudgetIdentity, "status please"}},
		{name: "nudge drain --inject", run: nudgeDrainInject, want: []string{"review queued work"}},
		{name: "mail check --inject", run: mailCheckInject, want: []string{"status please"}},
	} {
		resetHookBudgetProcessState(t)
		endStoreMemo := beginInvocationStoreMemo()
		var stdout, stderr bytes.Buffer
		code := sc.run(t, f, &stdout, &stderr)
		if err := endStoreMemo(); err != nil {
			t.Errorf("%s: releasing the invocation's stores: %v", sc.name, err)
		}
		if code != 0 {
			t.Fatalf("%s: exit %d; stderr=%s", sc.name, code, stderr.String())
		}
		for _, want := range sc.want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%s on a split city wrote no %q, which lives only in the binding:\n%s\nstderr: %s", sc.name, want, stdout.String(), stderr.String())
			}
		}
	}

	resetHookBudgetProcessState(t)
	work, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	leaked, err := work.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("listing the work store: %v", err)
	}
	for _, b := range leaked {
		t.Errorf("the work store holds %s (type %s, labels %v); a relocated class must be served from the binding only", b.ID, b.Type, b.Labels)
	}
}
