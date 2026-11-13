package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// preflightRow reports whether out carries a report row tagged tag for check.
func preflightRow(out, tag, check string) bool {
	prefix := fmt.Sprintf("%s%-*s%s", strings.Repeat(" ", preflightMargin), preflightTagColumn, tag, check)
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok && (rest == "" || rest[0] == ' ') {
			return true
		}
	}
	return false
}

// TestPreflightNamesEveryBlockerInOneRun is S-3's first acceptance item: the
// preflight names every blocker, not the first. One city carries six
// refusals, none of which depends on another; a report that stopped at the
// first would send the operator through six windows to find them. The
// closing line names exactly those six: the shared-store check reads the
// registry the supervisor check could not, so it is skipped rather than
// naming that one fault a second time.
func TestPreflightNamesEveryBlockerInOneRun(t *testing.T) {
	build := func(t *testing.T) storageOperatorRequest {
		request, cfg, _ := preflightReadyCity(t, 0)
		// remote stage
		if err := writeRemoteStage(request.CityPath, remoteStage{ControllerCity: "controller-city", ControllerPath: "/srv/controller-city"}); err != nil {
			t.Fatal(err)
		}
		// supervisor
		home := t.TempDir()
		t.Setenv("GC_HOME", home)
		if err := os.WriteFile(filepath.Join(home, "cities.toml"), []byte("[[cities\nthis is not toml"), 0o600); err != nil {
			t.Fatal(err)
		}
		// runtime
		stubSessionInventory(t, nil, false, nil)
		// served binding
		if err := writeBornSplitServedNote(request.CityPath, bornSplitServedNote{
			Binding:  "elsewhere",
			Provider: config.StorageProviderSQLiteBeads,
			Location: filepath.Join(t.TempDir(), "already-serving.db"),
		}); err != nil {
			t.Fatal(err)
		}
		// holds (D9)
		source, err := openInfraMigrationSource(request.CityPath)
		if err != nil {
			t.Fatal(err)
		}
		running := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
		waiting := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on it", Type: "task"})
		if err := source.DepAdd(waiting.ID, running.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		// destination
		destination, err := openInfraDestination(mustResolveInfraTarget(t, request.CityPath, cfg))
		if err != nil {
			t.Fatal(err)
		}
		mustCreateInfraBead(t, destination, beads.Bead{Title: "a bead some other owner put here", Type: "session"})
		if err := closeBeadStoreHandle(destination); err != nil {
			t.Fatal(err)
		}
		return request
	}
	// In report order.
	want := []struct{ check, reason string }{
		{"remote stage", "remote stage of city controller-city"},
		{"supervisor", "reading the supervisor registry"},
		{"runtime", "could not list every session"},
		{"served binding", "elsewhere"},
		{"holds", "a work store cannot hold a cross-store gate"},
		{"destination", "did not write"},
	}

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(build(t), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
	}
	out := stdout.String()
	var checks []string
	for _, w := range want {
		checks = append(checks, w.check)
		if !preflightRow(out, preflightBlockTag, w.check) {
			t.Errorf("the report does not block on %q:\n%s", w.check, out)
		}
		if !strings.Contains(out, w.reason) {
			t.Errorf("the report does not name %q's refusal %q:\n%s", w.check, w.reason, out)
		}
	}
	if closing := "the migration would refuse (" + strings.Join(checks, ", ") + ")."; !strings.Contains(out, closing) {
		t.Errorf("the closing line does not name exactly the six blockers; want %q:\n%s", closing, out)
	}
	if !preflightRow(out, preflightSkipTag, "shared store") || !strings.Contains(out, "needs supervisor") || strings.Count(out, preflightSkipTag) != 1 {
		t.Errorf("want one skipped check, the shared store, which reads the registry the supervisor check could not:\n%s", out)
	}

	// The migration refuses the same city: the report is a rehearsal of a
	// real refusal, not a stricter one. The migration stops at its first,
	// the remote stage; TestPreflightRefusesEveryCityTheMigrationRefuses pins
	// each of the others one city at a time.
	var migrateStdout, migrateStderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), build(t), &migrateStdout, &migrateStderr); code == 0 {
		t.Fatalf("the migration accepted the city the preflight blocked: %s", migrateStdout.String())
	}
	if !strings.Contains(migrateStderr.String(), want[0].reason) {
		t.Errorf("the migration refused for another reason than its first check, the remote stage: %s", migrateStderr.String())
	}
}

// TestPreflightSkipsOnlyWhatDependsOnABlock: a check that reads what a
// blocked check failed to produce is skipped and names that check; every
// other check still runs.
func TestPreflightSkipsOnlyWhatDependsOnABlock(t *testing.T) {
	t.Run("the work store cannot be read", func(t *testing.T) {
		request, cfg, _ := preflightReadyCity(t, 1)
		prev := openInfraMigrationSource
		openInfraMigrationSource = func(string) (beads.Store, error) { return nil, errStorageTestSourceUnreadable }
		t.Cleanup(func() { openInfraMigrationSource = prev })
		destination, err := openInfraDestination(mustResolveInfraTarget(t, request.CityPath, cfg))
		if err != nil {
			t.Fatal(err)
		}
		mustCreateInfraBead(t, destination, beads.Bead{Title: "in the way", Type: "session"})
		if err := closeBeadStoreHandle(destination); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
		}
		out := stdout.String()
		for _, check := range []string{"work store", "destination"} {
			if !preflightRow(out, preflightBlockTag, check) {
				t.Errorf("no block on %q:\n%s", check, out)
			}
		}
		for _, check := range []string{"edge payloads", "holds"} {
			if !preflightRow(out, preflightSkipTag, check) {
				t.Errorf("%q is not skipped:\n%s", check, out)
			}
		}
		if !strings.Contains(out, "needs work store") {
			t.Errorf("the skipped checks do not name the check they need:\n%s", out)
		}
	})

	t.Run("the edge payloads cannot be read", func(t *testing.T) {
		request, _, _ := preflightReadyCity(t, 0)
		backing, _, _, _ := seedInfraEdgeSource(t)
		prev := openInfraMigrationSource
		openInfraMigrationSource = func(string) (beads.Store, error) { return mutePayloadSource{Store: backing}, nil }
		t.Cleanup(func() { openInfraMigrationSource = prev })

		var stdout, stderr bytes.Buffer
		if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
		}
		out := stdout.String()
		if !preflightRow(out, preflightBlockTag, "edge payloads") || !preflightRow(out, preflightSkipTag, "holds") || !strings.Contains(out, "needs edge payloads") {
			t.Errorf("an edge-payload block does not skip the holds, which are planned over the edges:\n%s", out)
		}
		if !preflightRow(out, preflightPassTag, "destination") {
			t.Errorf("the destination, which reads no edge, was not checked:\n%s", out)
		}
	})

	t.Run("the topology names no binding", func(t *testing.T) {
		request, cfg, _ := preflightReadyCity(t, 1)
		cfg.Storage.Classes.Nudges = config.StorageWorkBinding
		if err := writeRemoteStage(request.CityPath, remoteStage{ControllerCity: "controller-city", ControllerPath: "/srv/controller-city"}); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
		}
		out := stdout.String()
		for _, check := range []string{"topology", "remote stage"} {
			if !preflightRow(out, preflightBlockTag, check) {
				t.Errorf("no block on %q:\n%s", check, out)
			}
		}
		for _, check := range []string{"rig scopes", "served binding", "cutover", "work store", "edge payloads", "holds", "destination"} {
			if !preflightRow(out, preflightSkipTag, check) {
				t.Errorf("%q, which reads the binding or what the work store holds, is not skipped:\n%s", check, out)
			}
		}
		if !strings.Contains(out, "needs topology") || !strings.Contains(out, "needs work store") {
			t.Errorf("the skipped checks do not name the check each needs:\n%s", out)
		}
	})

	t.Run("the supervisor registry cannot be read", func(t *testing.T) {
		request, _, _ := preflightReadyCity(t, 1)
		home := t.TempDir()
		t.Setenv("GC_HOME", home)
		if err := os.WriteFile(filepath.Join(home, "cities.toml"), []byte("[[cities\nthis is not toml"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
		}
		out := stdout.String()
		if !preflightRow(out, preflightBlockTag, "supervisor") || !preflightRow(out, preflightSkipTag, "shared store") || !strings.Contains(out, "needs supervisor") {
			t.Errorf("the shared-store check, which reads the registry the supervisor check could not, is not skipped for it:\n%s", out)
		}
		if strings.Contains(out, "rule out a shared store") || !strings.Contains(out, "the migration would refuse (supervisor).") {
			t.Errorf("one unreadable registry is reported as more than one blocker:\n%s", out)
		}
	})

	t.Run("the binding root cannot be read", func(t *testing.T) {
		request, cfg, _ := preflightReadyCity(t, 1)
		// A file where the binding root's directory belongs: every path in it
		// fails to stat, and not as an absence.
		if err := os.WriteFile(mustResolveInfraTarget(t, request.CityPath, cfg).Root, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
		}
		out := stdout.String()
		if !preflightRow(out, preflightBlockTag, "binding root") {
			t.Fatalf("the unreadable binding root does not block:\n%s", out)
		}
		for _, check := range []string{"cutover", "destination"} {
			if !preflightRow(out, preflightSkipTag, check) {
				t.Errorf("%q, which reads the binding root, is not skipped:\n%s", check, out)
			}
		}
		if !strings.Contains(out, "needs binding root") || !strings.Contains(out, "the migration would refuse (binding root).") {
			t.Errorf("one unreadable binding root is reported as more than one blocker:\n%s", out)
		}
		if !preflightRow(out, preflightPassTag, "work store") {
			t.Errorf("the work store, which is not in the binding root, was not checked:\n%s", out)
		}
	})
}

// TestPreflightDoesNotCallABlockedConvergedCityNothingToMigrate: a converged
// city clears, but not when another check blocks; the closing line names it.
func TestPreflightDoesNotCallABlockedConvergedCityNothingToMigrate(t *testing.T) {
	request, cfg, _ := preflightReadyCity(t, 2)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture migration reported %s: %s", got.Outcome, log.String())
	}
	if err := writeRemoteStage(request.CityPath, remoteStage{ControllerCity: "controller-city", ControllerPath: "/srv/controller-city"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Nothing to migrate") || !strings.Contains(out, "the migration would refuse (remote stage)") {
		t.Errorf("a blocked converged city reads as one with nothing to do:\n%s", out)
	}
}

// TestSplitPreflightRunsTheChecksThatReadNoStoreOnAStoppedCity: a city
// whose provider is down cannot have its stores read without starting it,
// which the rehearsal never does. Every check that reads no store still
// runs, so a blocker there is named before the window, and each that would
// read one is skipped and says why.
func TestSplitPreflightRunsTheChecksThatReadNoStoreOnAStoppedCity(t *testing.T) {
	request, _, provider := splitReadyCity(t, 10, nativeDoltLane)
	stubMigrationProviderDown(t, true)
	opened := false
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		opened = true
		return nil, errors.New("opened")
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	if err := writeRemoteStage(request.CityPath, remoteStage{ControllerCity: "controller-city", ControllerPath: "/srv/controller-city"}); err != nil {
		t.Fatal(err)
	}
	stubSessionInventory(t, nil, false, nil)

	code, summary, out := runSplitPreflight(t, request)
	if code != 1 || summary.ready() {
		t.Fatalf("exit %d, %+v:\n%s", code, summary, out)
	}
	for _, check := range []string{"remote stage", "runtime"} {
		if !slices.Contains(summary.Blocks, check) || !preflightRow(out, preflightBlockTag, check) {
			t.Errorf("blocks %v: %q, a check that reads no store, did not run on the stopped city:\n%s", summary.Blocks, check, out)
		}
	}
	if !preflightRow(out, preflightPassTag, "topology") || !preflightRow(out, preflightPassTag, "destination") {
		t.Errorf("the topology and destination checks read no bead store and did not run:\n%s", out)
	}
	if !preflightRow(out, preflightSkipTag, "work store") || !strings.Contains(out, "needs a readable store") {
		t.Errorf("the work store is not reported as skipped for want of a readable store:\n%s", out)
	}
	if opened || provider.starts != 0 {
		t.Errorf("the rehearsal opened the work store (%t) or started the provider (%d)", opened, provider.starts)
	}
	if summary.Unread == "" || !strings.Contains(out, "the split would refuse (") || !strings.Contains(out, "this command starts nothing. "+summary.UnreadRemedy+".") {
		t.Errorf("the footer does not name both the blocks and what lets a later run read the stores:\n%s", out)
	}
}

// TestSplitPreflightOnAStoppedCityWithAMarkerReadsNoStore: the convergence
// marker a backed-out migration leaves blocks the split rehearsal on a city
// whose provider is down too, and the block reads no store. Boot's check of
// what the binding holds reads the work store, which on a stopped managed or
// proxied city would start the server and leave it running (ga-683m7f), so
// the report names that check as not run instead of answering it.
func TestSplitPreflightOnAStoppedCityWithAMarkerReadsNoStore(t *testing.T) {
	request, _, provider := splitReadyCity(t, 30, nativeDoltLane)
	split := splitHypotheticalConfig(request.Cfg)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, split, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migrating: %s: %s", got.Outcome, log.String())
	}
	for _, note := range []string{infraClearedNotePath(request.CityPath), bornSplitServedNotePath(request.CityPath)} {
		if err := os.Remove(note); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	stubMigrationProviderDown(t, true)
	opened := 0
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		opened++
		return prev(cityPath)
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	code, summary, out := runSplitPreflight(t, request)
	if code != 1 || !slices.Contains(summary.Blocks, "cutover") || !preflightRow(out, preflightBlockTag, "cutover") {
		t.Fatalf("exit %d, blocks %v: the marker does not block the split:\n%s", code, summary.Blocks, out)
	}
	if opened != 0 || provider.starts != 0 {
		t.Fatalf("the rehearsal of a city whose provider is down opened the work store %d time(s) and started the provider %d time(s):\n%s", opened, provider.starts, out)
	}
	target := mustResolveInfraTarget(t, request.CityPath, split)
	for _, want := range []string{target.MarkerPath(), "this run reads none"} {
		if !strings.Contains(out, want) {
			t.Errorf("the cutover block does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "boot's check confirms") {
		t.Errorf("the report answers boot's check, which reads the work store this run did not read:\n%s", out)
	}
	assertCopyChecksSkippedForTheCutover(t, out)
}

// assertCopyChecksSkippedForTheCutover: a cutover the convergence marker
// blocks leaves the copy's checks unrun, and the report says so rather than
// leaving their rows out.
func assertCopyChecksSkippedForTheCutover(t *testing.T, out string) {
	t.Helper()
	for _, check := range []string{"work store", "edge payloads", "holds", "destination"} {
		if !preflightRow(out, preflightSkipTag, check) {
			t.Errorf("%q, which waits on the cutover, is not reported skipped:\n%s", check, out)
		}
	}
	if !strings.Contains(out, "needs cutover") {
		t.Errorf("the skipped copy checks do not name the cutover:\n%s", out)
	}
}

// TestSplitPreflightOnAStoppedCityReadsNoRigStore: the rig census reads each
// rig's store, so on a city whose provider is down it is skipped, not run:
// a rig store open on a stopped city starts its server as a work-store open
// does.
func TestSplitPreflightOnAStoppedCityReadsNoRigStore(t *testing.T) {
	request, _, provider := splitReadyCity(t, 10, nativeDoltLane)
	request.Cfg.Rigs = []config.Rig{{Name: "alpha", Prefix: "ga", Path: t.TempDir()}}
	stubMigrationProviderDown(t, true)
	rigOpens := 0
	prev := rigImportStoreOpener
	rigImportStoreOpener = func(cityPath string, rig config.Rig) (beads.Store, error) {
		rigOpens++
		return prev(cityPath, rig)
	}
	t.Cleanup(func() { rigImportStoreOpener = prev })

	code, summary, out := runSplitPreflight(t, request)
	if code != 1 || summary.ready() {
		t.Fatalf("exit %d, %+v:\n%s", code, summary, out)
	}
	if rigOpens != 0 || provider.starts != 0 {
		t.Fatalf("the rehearsal of a city whose provider is down opened a rig store %d time(s) and started the provider %d time(s):\n%s", rigOpens, provider.starts, out)
	}
	if !preflightRow(out, preflightSkipTag, preflightRigScopesCheck) {
		t.Errorf("the rig census is not reported skipped:\n%s", out)
	}
}

// TestSplitPreflightOnAStoppedCityReadsNoSharedRows: naming the rows another
// city's scope holds in a shared work store reads that store, so on a city
// whose provider is down the census is skipped, not run: the open would start
// the stopped server and leave it running (ga-683m7f). The shared-store check
// itself reads no store, so it still runs, attested or not.
func TestSplitPreflightOnAStoppedCityReadsNoSharedRows(t *testing.T) {
	for _, attested := range []bool{false, true} {
		t.Run(fmt.Sprintf("attested=%t", attested), func(t *testing.T) {
			request, _, provider := splitReadyCity(t, 10, nativeDoltLane)
			stubMigrationProviderDown(t, true)
			sharedStoreCities(t, request)
			want := preflightBlockTag
			if attested {
				request.Force = storageForce{SharedStores: []string{sharedToken}}
				want = preflightPassTag
			}
			opened := 0
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
				opened++
				return prev(cityPath)
			}
			t.Cleanup(func() { openInfraMigrationSource = prev })

			code, summary, out := runSplitPreflight(t, request)
			if code != 1 || summary.ready() {
				t.Fatalf("exit %d, %+v:\n%s", code, summary, out)
			}
			if opened != 0 || provider.starts != 0 {
				t.Fatalf("the rehearsal of a city whose provider is down opened the work store %d time(s) and started the provider %d time(s):\n%s", opened, provider.starts, out)
			}
			if !preflightRow(out, want, "shared store") {
				t.Errorf("the shared-store check, which reads no store, is not reported %s:\n%s", want, out)
			}
			if !preflightRow(out, preflightSkipTag, "shared rows") || !strings.Contains(out, "needs a readable store") {
				t.Errorf("the shared-row census is not reported skipped for want of a readable store:\n%s", out)
			}
		})
	}
}
