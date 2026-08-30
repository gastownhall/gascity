package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/warmup"
)

// failingWarmupCheck is a warm-up-eligible doctor check that always fails, so
// the warm-up runner reaches the mail send. It stands in for any check a city
// fails persistently — a rig whose root branch is missing, say — which makes
// `gc start` mail the mayor on every single boot.
type failingWarmupCheck struct{ name string }

func (c failingWarmupCheck) Name() string { return c.name }

func (c failingWarmupCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	return &doctor.CheckResult{Name: c.name, Status: doctor.StatusError, Message: "no main branch"}
}

func (c failingWarmupCheck) CanFix() bool                     { return false }
func (c failingWarmupCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c failingWarmupCheck) WarmupEligible() bool             { return true }

// runWarmupWithDefaultMailProvider runs the warm-up scan exactly as
// cmd_start.go does — one failing check, the mailer that `gc start` builds —
// and returns the report.
func runWarmupWithDefaultMailProvider(t *testing.T, cityPath string, cfg *config.City) *warmup.WarmupReport {
	t.Helper()
	var stderr bytes.Buffer
	report, err := warmup.RunWarmupChecks(context.Background(), cityPath, cfg, warmup.WarmupOpts{
		Checks: []doctor.Check{failingWarmupCheck{name: "rig-root-branch"}},
		Mailer: defaultMailProvider(cityPath),
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("warmup runner: %v", err)
	}
	if len(report.Failures) == 0 {
		t.Fatalf("the fixture check did not fail, so no warm-up mail was generated; stderr=%q", stderr.String())
	}
	return report
}

// TestWarmupMailWritesTheBindingOnAMigratedCity pins the defect at its write
// site.
//
// defaultMailProvider handed the raw city WORK store to the mail provider, so
// on a converged split city `gc start`'s warm-up mail — a type=message bead,
// which internal/coordclass classifies messaging, an INFRASTRUCTURE class —
// landed in the ledger that class was migrated off. The next boot's containment
// re-check then read it as a stranded infrastructure write and refused to boot,
// and the boot after that stranded one more. A city that fails a doctor check
// every boot therefore accumulates strands faster than any recovery can drain
// them.
//
// Two assertions, one defect from both sides: the write is IN the binding, and
// it is NOT in the retained work store.
func TestWarmupMailWritesTheBindingOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	t.Setenv("GC_MAIL", "")

	provider := defaultMailProvider(cityPath)
	if provider == nil {
		t.Fatal("defaultMailProvider returned nil")
	}
	sent, err := provider.Send("gc-start-warmup", "mayor", "city warm-up: 2 doctor check(s) failed", "body")
	if err != nil {
		t.Fatalf("warm-up mail send: %v", err)
	}

	// The funnel's own handle goes first, so the assertions below read durable
	// bytes rather than state an open connection is holding.
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	if _, err := binding.Get(sent.ID); err != nil {
		t.Errorf("the warm-up mail write did not land in the binding: %v", err)
	}
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the retained work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	if _, err := work.Get(sent.ID); err == nil {
		t.Errorf("the warm-up mail landed in the work store as %s; a messaging-class bead on a converged city belongs in the binding only", sent.ID)
	} else if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("reading the work store for %s: %v", sent.ID, err)
	}
}

// TestWarmupRunStrandsNothingOnAMigratedCity is the production sequence in
// miniature: the warm-up scan writes its mail, and then the very check that
// refuses boot runs over the same city.
//
// This is the whole failure reproduced end to end rather than argued about. On
// the class-blind write the report comes back infraMigrationStranded, naming
// the warm-up mail bead — which is exactly what the controller printed before
// refusing to start.
func TestWarmupRunStrandsNothingOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	t.Setenv("GC_MAIL", "")

	report := runWarmupWithDefaultMailProvider(t, cityPath, cfg)
	if !report.MailSent {
		t.Fatalf("warm-up mail was not sent (err=%v), so this test would pass vacuously", report.MailSendError)
	}

	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	var log bytes.Buffer
	got := checkInfraClassConvergence(cityPath, cfg, "gc start", &log)
	if got.Outcome != infraMigrationConverged {
		t.Errorf("the boot gate reports %s after one warm-up scan, want %s; stranded=%v\n%s",
			got.Outcome, infraMigrationConverged, got.Stranded, log.String())
	}
	if len(got.Stranded) != 0 {
		t.Errorf("one warm-up scan stranded %d infrastructure bead(s): %v", len(got.Stranded), got.Stranded)
	}
}

// TestWarmupBootPathWorkBeadStaysOnTheWorkLedger is the mandatory
// reverse-direction control.
//
// A fix that routes every warm-up write at the binding passes the two tests
// above and is wrong: work-class beads belong on the work ledger on a split
// city, and moving them there is the same defect pointed the other way. This
// asserts the work class is untouched — both through the class resolver the
// fix uses and through a real write to the boot path's own store.
func TestWarmupBootPathWorkBeadStaysOnTheWorkLedger(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })

	// The resolver the fix routes through must hand back the EXACT work store
	// value for the work class, on the same converged city that relocates
	// messaging.
	routed := resolveClassStore(cliStorageRoutes(cityPath), work, nil, cityPath, config.BeadClassWork, nil)
	if routed != work {
		t.Errorf("work resolved to %p on a converged city, want the work store %p", routed, work)
	}

	// Written THROUGH the resolver, not around it: a write to the raw work
	// store could never land anywhere else, so it would assert nothing about
	// routing. This one follows whatever the resolver decided.
	task := mustCreateInfraBead(t, routed, beads.Bead{Title: "a warm-up-era task", Type: "task"})
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	if _, err := work.Get(task.ID); err != nil {
		t.Errorf("the work-class bead is not in the work store: %v", err)
	}
	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	if _, err := binding.Get(task.ID); err == nil {
		t.Errorf("the work-class bead %s landed in the infrastructure binding; work must stay on the work ledger", task.ID)
	} else if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("reading the binding for %s: %v", task.ID, err)
	}
}

// TestWarmupMailStaysOnTheWorkStoreWithoutAStorageSection is the identity
// control: every city that authors no [storage] section — which is every city
// but the split ones — must behave byte-identically after the fix.
func TestWarmupMailStaysOnTheWorkStoreWithoutAStorageSection(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	captureCLIStorageStderr(t)
	t.Setenv("GC_MAIL", "")

	provider := defaultMailProvider(cityPath)
	if provider == nil {
		t.Fatal("defaultMailProvider returned nil")
	}
	sent, err := provider.Send("gc-start-warmup", "mayor", "city warm-up: 2 doctor check(s) failed", "body")
	if err != nil {
		t.Fatalf("warm-up mail send: %v", err)
	}
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	if _, err := work.Get(sent.ID); err != nil {
		t.Errorf("an unsplit city's warm-up mail is not in its work store: %v", err)
	}
}

// TestWarmupMailRefusesOnAnUnconvergedCity is the property that stops the
// strand growing before any recovery runs.
//
// On a city whose config names a binding it has not migrated onto, the warm-up
// mail must not be written at all: not to the binding (which cannot serve) and
// above all not to the work store, which is the write that strands. Warm-up is
// fail-open by construction — cmd_start.go discards both return values and the
// runner records MailSendError and continues — so refusing here costs the boot
// nothing.
func TestWarmupMailRefusesOnAnUnconvergedCity(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	stderr := captureCLIStorageStderr(t)
	t.Setenv("GC_MAIL", "")

	provider := defaultMailProvider(cityPath)
	if provider == nil {
		t.Fatal("defaultMailProvider returned nil")
	}
	// The refusal is asserted structurally rather than by its text: mail's
	// sender-address read goes through session.ResolveAddress, whose error
	// hygiene deliberately keeps the cause out of Error() while still
	// unwrapping to it. isStandingStorageRefusal is the predicate that survives
	// that, and it says the precise thing — this is the build's verdict about
	// the CITY, not a fault in one read. The remedy the operator acts on is on
	// stderr, asserted next.
	if _, err := provider.Send("gc-start-warmup", "mayor", "city warm-up: 2 doctor check(s) failed", "body"); err == nil {
		t.Error("warm-up mail was accepted on a city that has not converged onto its binding")
	} else if !isStandingStorageRefusal(err) {
		t.Errorf("the refused warm-up send failed for some other reason than the storage refusal: %v", err)
	}
	if !strings.Contains(stderr.String(), storageMigrationCommand) {
		t.Errorf("the refusal never reached the operator: %q", stderr.String())
	}
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}

	// The assertion that matters: nothing message-shaped reached the work
	// ledger. A write here is the one the next boot's containment re-check
	// reads as a stranded infrastructure bead.
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	rows, err := work.List(beads.ListQuery{Type: "message", IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("listing message beads in the work store: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused city's warm-up mail still landed in the work store (%d message bead(s)); that is the write that strands the next boot", len(rows))
	}
}
