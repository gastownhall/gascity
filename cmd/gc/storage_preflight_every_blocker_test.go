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
// independent refusals; a report that stopped at the first would send the
// operator through six windows to find them.
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
	want := map[string]string{
		"remote stage":   "remote stage of city controller-city",
		"supervisor":     "reading the supervisor registry",
		"runtime":        "could not list every session",
		"served binding": "elsewhere",
		"holds":          "a work store cannot hold a cross-store gate",
		"destination":    "did not write",
	}

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(build(t), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
	}
	out := stdout.String()
	for check, reason := range want {
		if !preflightRow(out, preflightBlockTag, check) {
			t.Errorf("the report does not block on %q:\n%s", check, out)
		}
		if !strings.Contains(out, reason) {
			t.Errorf("the report does not name %q's refusal %q:\n%s", check, reason, out)
		}
	}
	closing := out[strings.LastIndex(out, "the migration would refuse ("):]
	for check := range want {
		if !strings.Contains(closing, check) {
			t.Errorf("the closing line does not name %q: %q", check, closing)
		}
	}
	if strings.Contains(out, preflightSkipTag) {
		t.Errorf("six independent checks blocked, and one was skipped as if it depended on another:\n%s", out)
	}

	// The migration refuses the same city: the report is a rehearsal of a
	// real refusal, not a stricter one.
	var migrateStdout, migrateStderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), build(t), &migrateStdout, &migrateStderr); code == 0 {
		t.Fatalf("the migration accepted the city the preflight blocked: %s", migrateStdout.String())
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
		for _, check := range []string{"rig scopes", "served binding", "cutover", "work store", "destination"} {
			if !preflightRow(out, preflightSkipTag, check) {
				t.Errorf("%q, which reads the binding, is not skipped:\n%s", check, out)
			}
		}
		if !strings.Contains(out, "needs topology") {
			t.Errorf("the skipped checks do not name the topology:\n%s", out)
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
}
