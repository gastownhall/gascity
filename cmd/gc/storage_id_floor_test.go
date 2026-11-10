package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeHashConfig is a bd config surface in memory, recording every write.
type fakeHashConfig struct {
	values map[string]string
	sets   []string
	setErr error
	getErr error
}

func (c *fakeHashConfig) ConfigGet(key string) (string, error) {
	if c.getErr != nil {
		return "", c.getErr
	}
	return c.values[key], nil
}

func (c *fakeHashConfig) ConfigSet(key, value string) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.sets = append(c.sets, key+"="+value)
	c.values[key] = value
	return nil
}

func stubCityHashConfig(t *testing.T, surface cityHashConfig) {
	t.Helper()
	prev := cityHashConfigOpener
	t.Cleanup(func() { cityHashConfigOpener = prev })
	cityHashConfigOpener = func(string) (cityHashConfig, error) { return surface, nil }
}

// TestCityIDFloorIsOnePastTheLongestOwnSuffix pins the floor at its boundary:
// one past the longest hash suffix among the carried ids of the store's own
// prefix, wisp and child ids included, other prefixes ignored.
func TestCityIDFloorIsOnePastTheLongestOwnSuffix(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		want int
	}{
		{name: "none", want: 0},
		{name: "only another prefix", ids: []string{"gp-k0nvi1x", "gcg-12"}, want: 0},
		{name: "MC's shape", ids: []string{"mc-abc", "mc-wisp-4m43", "mc-wisp-k0nvi", "mc-q1w2e3", "mc-wisp-d0j-satellite-dup", "gp-k0nvi1x9"}, want: 7},
		{name: "a child id counts its parent's hash", ids: []string{"mc-ab12.3"}, want: 5},
		{name: "a longer suffix under another prefix is ignored", ids: []string{"mc-abc", "mcx-abcdefgh"}, want: 4},
		{name: "a cross-store gate id is not a bd hash", ids: []string{"mc-xsg-ddb51ee0b716", "mc-q1w2"}, want: 5},
		{name: "only gate ids", ids: []string{"mc-xsg-ddb51ee0b716", "mc-xsg-0a1b2c3d4e5f"}, want: 0},
		{name: "a suffix bd never mints (too short, upper case, punctuation)", ids: []string{"mc-ab", "mc-ABCD", "mc-ab_cd"}, want: 0},
		{name: "an 8-character suffix: the floor stops at bd's cap", ids: []string{"mc-abcdefgh"}, want: 8},
	} {
		if got := cityIDFloor(tc.ids, "mc"); got != tc.want {
			t.Errorf("%s: cityIDFloor(%v) = %d, want %d", tc.name, tc.ids, got, tc.want)
		}
	}
}

// TestNoIDMintedAtTheFloorCanBeACarriedID is the floor's property, at its
// boundary: an id whose hash suffix is at least the floor long is never one
// the copy carried (whatever its shape: plain, wisp, child), and one a single
// character shorter can be.
func TestNoIDMintedAtTheFloorCanBeACarriedID(t *testing.T) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	r := rand.New(rand.NewSource(7))
	hash := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	shape := func(h string) string {
		switch r.Intn(3) {
		case 0:
			return "mc-" + h
		case 1:
			return "mc-wisp-" + h
		default:
			return "mc-" + h + "." + strconv.Itoa(1+r.Intn(9))
		}
	}
	for round := 0; round < 200; round++ {
		carried := map[string]bool{}
		var ids []string
		for i := 0; i < 50; i++ {
			id := shape(hash(3 + r.Intn(4)))
			carried[id] = true
			ids = append(ids, id)
		}
		floor := cityIDFloor(ids, "mc")
		for i := 0; i < 200; i++ {
			if id := shape(hash(floor + r.Intn(3))); carried[id] {
				t.Fatalf("round %d: %s, minted at the floor %d, is a carried id", round, id, floor)
			}
		}
		// The boundary: the longest carried suffix is exactly floor-1, so a
		// floor one lower would admit a re-mint of that id.
		longest := 0
		for _, id := range ids {
			longest = max(longest, len(rigIDHashSuffix(id)))
		}
		if longest != floor-1 {
			t.Fatalf("round %d: the longest carried suffix is %d, the floor %d; the floor must sit exactly one past it", round, longest, floor)
		}
	}
}

// TestReserveCityIDsWithNothingCarriedWritesNothing: a manifest with no id of
// the store's prefix needs no floor, and none is written.
func TestReserveCityIDsWithNothingCarriedWritesNothing(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})
	got, err := reserveCityIDs("/city", []string{"gp-abcdef"})
	if err != nil || got.Floor != 0 || len(conf.sets) != 0 || !strings.Contains(got.String(), "needs one") {
		t.Fatalf("reserve = %+v (%v), writes %v; want nothing", got, err, conf.sets)
	}
}

// TestReserveCityIDsRaisesNeverLowersAndIsIdempotent: the first run raises
// min_hash_length (and max_hash_length, when the floor passes bd's default
// ceiling); a second run writes nothing; a floor already higher is kept.
func TestReserveCityIDsRaisesNeverLowersAndIsIdempotent(t *testing.T) {
	carried := []string{"mc-abc", "mc-wisp-abcdefg"} // floor 8, one above the max of 7 this store sets
	conf := &fakeHashConfig{values: map[string]string{bdMaxHashLengthKey: "7"}}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})
	got, err := reserveCityIDs("/city", carried)
	if err != nil {
		t.Fatal(err)
	}
	if got.Floor != 8 || strings.Join(conf.sets, ",") != "min_hash_length=8,max_hash_length=8" || got.String() != "city work store min_hash_length 0 -> 8, max_hash_length 7 -> 8" {
		t.Fatalf("first run = %+v (%q), writes %v; want min and max raised to 8, both reported", got, got, conf.sets)
	}
	conf.sets = nil
	got, err = reserveCityIDs("/city", carried)
	if err != nil || len(conf.sets) != 0 || got.Floor != 8 || len(got.Changes) != 0 {
		t.Fatalf("second run = %+v (%v), writes %v; want none", got, err, conf.sets)
	}
	if !strings.Contains(got.String(), "already 8") {
		t.Fatalf("second run says %q", got)
	}
	higher := &fakeHashConfig{values: map[string]string{bdMinHashLengthKey: "8"}}
	stubCityHashConfig(t, cityHashConfig{Conf: higher, Prefix: "mc"})
	got, err = reserveCityIDs("/city", []string{"mc-q1w2e"})
	if err != nil || len(higher.sets) != 0 || got.Floor != 8 {
		t.Fatalf("a higher floor = %+v (%v), writes %v; want it kept, nothing written", got, err, higher.sets)
	}
}

// TestTheFloorNeverPassesBdsCap: carried cross-store gate ids (graph rows
// whose suffix is 12 hex characters) do not lift the floor, and no floor or
// max_hash_length passes the 8 characters bd mints: bd's CLI and native
// minting ignore more, and a hosted `bd serve` pads every id to it with
// zeros. The helper is S4b's too.
func TestTheFloorNeverPassesBdsCap(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})
	got, err := reserveCityIDs("/city", []string{"mc-xsg-ddb51ee0b716", "mc-wisp-xsg-ab12cd34ef56", "mc-q1w2e3"})
	if err != nil || got.Floor != 7 || strings.Join(conf.sets, ",") != "min_hash_length=7" {
		t.Fatalf("reserve = %+v (%v), writes %v; want min_hash_length 7 from the one bd id, the gates ignored", got, err, conf.sets)
	}
	if rig := rigImportHashFloor([]string{"gp-xsg-ddb51ee0b716", "gp-abcdefgh"}); rig != 8 {
		t.Fatalf("the rig floor = %d, want bd's cap of 8", rig)
	}
	capped := &fakeHashConfig{values: map[string]string{bdMaxHashLengthKey: "4"}}
	if floor, _, err := raiseBdHashFloor(capped, 13); err != nil || floor != 8 || strings.Join(capped.sets, ",") != "min_hash_length=8,max_hash_length=8" {
		t.Fatalf("raise to 13 = %d (%v), writes %v; want both capped at 8", floor, err, capped.sets)
	}
}

// TestReserveCityIDsOnHostedBdNamesTheOutOfBandSet: a hosted bd store that
// refuses the write is refused with the value to set on the hosted project,
// out of band (owner decision O10), and nothing is removed.
func TestReserveCityIDsOnHostedBdNamesTheOutOfBandSet(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}, setErr: errors.New("bd config set: 405 method not allowed")}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc", Hosted: "https://beads.example.test"})
	_, err := reserveCityIDs("/city", []string{"mc-q1w2e3"})
	if err == nil {
		t.Fatal("a refused write was not refused")
	}
	for _, want := range []string{"hosted bd at https://beads.example.test", "405 method not allowed", "min_hash_length=7", "max_hash_length at least 7", "out of band (owner decision O10)", "nothing was removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	local := &fakeHashConfig{values: map[string]string{}, setErr: errors.New("boom")}
	stubCityHashConfig(t, cityHashConfig{Conf: local, Prefix: "mc"})
	if _, err := reserveCityIDs("/city", []string{"mc-q1w2e3"}); err == nil || strings.Contains(err.Error(), "O10") {
		t.Fatalf("a local store's refusal = %v, want one that does not send the operator to the hosted path", err)
	}
}

// TestTheDefaultCityHashSurface: a file store and a generic exec store are
// skipped and say why; a city with no bd store is skipped; a bd city's surface
// names its prefix and, for hosted bd, its endpoint.
func TestTheDefaultCityHashSurface(t *testing.T) {
	city := func(t *testing.T, provider string) string {
		t.Helper()
		dir := t.TempDir()
		toml := "[workspace]\nname = \"mc\"\n"
		if provider != "" {
			toml += "\n[beads]\nprovider = \"" + provider + "\"\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, tc := range []struct {
		provider, skipped string
	}{
		{"file", "mints from a sequence"},
		{"exec:/usr/local/bin/my-store", "generic exec store"},
		{"", "holds no bd store"},
	} {
		surface, err := defaultCityHashConfigOpener(city(t, tc.provider))
		if err != nil || surface.Conf != nil || !strings.Contains(surface.Skipped, tc.skipped) {
			t.Errorf("provider %q: surface %+v (%v), want skipped: %q", tc.provider, surface, err, tc.skipped)
		}
	}
	dir := city(t, "")
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"config.yaml":   "issue-prefix: mc\n",
		"metadata.json": `{"backend":"http","storage_endpoint":"https://beads.example.test","storage_database":"mc"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, ".beads", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	surface, err := defaultCityHashConfigOpener(dir)
	if err != nil {
		t.Fatal(err)
	}
	if surface.Conf == nil || surface.Prefix != "mc" || surface.Hosted != "https://beads.example.test" {
		t.Fatalf("a hosted bd city's surface = %+v, want its bd config, prefix mc and endpoint", surface)
	}
}

// floorCity is a converged split city whose manifest carried mc- ids, its
// work store a fake bd config surface.
func floorCity(t *testing.T, conf *fakeHashConfig) (storageOperatorRequest, infraBindingTarget) {
	t.Helper()
	request, _ := exclusionCity(t)
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	mem := source.(*beads.MemStore)
	mem.HonorExplicitIDs = true
	for _, id := range []string{"mc-q1w2e3", "mc-wisp-ab1"} {
		if _, err := mem.Create(beads.Bead{ID: id, Title: id, Type: "session", Labels: []string{"gc:session"}}); err != nil {
			t.Fatal(err)
		}
	}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})
	return request, mustResolveInfraTarget(t, request.CityPath, request.Cfg)
}

// TestTheMigrationRaisesTheCityFloorBeforeItClears: the floor is set before
// the clear removes a row, and a floor that cannot be set stops the run
// before the clear, with every row still in the work store; the next run,
// once the floor can be set, finishes.
func TestTheMigrationRaisesTheCityFloorBeforeItClears(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}, setErr: errors.New("bd config set: refused")}
	request, target := floorCity(t, conf)
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "raising the city work store's minimum id length to 7") {
		t.Fatalf("migrate = %d, want the refused floor to stop it: %s", code, stderr)
	}
	source, _ := openInfraMigrationSource(request.CityPath)
	if _, err := source.Get("mc-q1w2e3"); err != nil {
		t.Fatalf("a run whose floor failed removed a row: %v", err)
	}
	if manifest, _, err := readInfraCopyManifestRecord(target); err != nil || !manifest.IDs["mc-q1w2e3"] {
		t.Fatalf("the copy was not proven before the floor: %v", err)
	}

	conf.setErr = nil
	prevDelete := infraClearBeforeDelete
	t.Cleanup(func() { infraClearBeforeDelete = prevDelete })
	infraClearBeforeDelete = func(string) error {
		if conf.values[bdMinHashLengthKey] != "7" {
			return fmt.Errorf("a row went before the floor was set (min_hash_length=%q)", conf.values[bdMinHashLengthKey])
		}
		return nil
	}
	code, stderr = migrateFor(t, request)
	if code != 0 || !strings.Contains(stderr, "city work store min_hash_length 0 -> 7") {
		t.Fatalf("migrate = %d: %s", code, stderr)
	}
	if _, err := source.Get("mc-q1w2e3"); err == nil {
		t.Fatal("the clear did not run")
	}
}

// TestTheMigrationResumesFromTheFloorBoundary: a run killed right after the
// floor (the "floor" boundary) resumes, writes no second floor, and clears.
func TestTheMigrationResumesFromTheFloorBoundary(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}}
	request, _ := floorCity(t, conf)
	prev := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prev })
	infraEngineCrashPoint = func(p string) error {
		if p == "floor" {
			return errors.New("killed after the floor")
		}
		return nil
	}
	if code, stderr := migrateFor(t, request); code == 0 || !strings.Contains(stderr, "killed after the floor") {
		t.Fatalf("migrate = %d: %s", code, stderr)
	}
	infraEngineCrashPoint = prev
	conf.sets = nil
	if code, stderr := migrateFor(t, request); code != 0 || len(conf.sets) != 0 {
		t.Fatalf("resume = %d, writes %v: %s", code, conf.sets, stderr)
	}
}

// TestTheSQLiteBindingNeverMintsACarriedID: a carried row under the binding's
// own prefix (a city reverted and split again) raises the binding's sequence,
// so a row the binding mints after the copy never takes its id.
func TestTheSQLiteBindingNeverMintsACarriedID(t *testing.T) {
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		t.Fatal(err)
	}
	stubInfraControllerPing(t, 0)
	carried := prefix + "-50"
	source := d9SQLiteSource(t, []beads.Bead{{ID: carried, Title: "carried back by --to-work", Type: "session", Labels: []string{"gc:session"}}}, nil)
	prevOpen := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prevOpen })
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", got.Outcome, log.String())
	}
	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	for i := 0; i < 60; i++ {
		minted, err := binding.Create(beads.Bead{Title: "new session", Type: "session", Labels: []string{"gc:session"}})
		if err != nil {
			t.Fatal(err)
		}
		if minted.ID == carried {
			t.Fatalf("the binding minted the carried id %s", carried)
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(minted.ID, prefix+"-"))
		if n <= 50 {
			t.Fatalf("the binding minted %s, at or below the carried %s", minted.ID, carried)
		}
	}
}

// TestRepairSequenceIDFloorTakesNoOtherSelector: --id-floor names the city
// work store, so --floor, --dir and --prefix are refused with it.
func TestRepairSequenceIDFloorTakesNoOtherSelector(t *testing.T) {
	for _, extra := range [][]string{{"--floor", "7"}, {"--dir", "/tmp/x"}, {"--prefix", "mc"}} {
		var stdout, stderr bytes.Buffer
		cmd := newStorageRepairSequenceCmd(storageCommandSurface{Namespace: "storage"}, &stdout, &stderr)
		cmd.SetArgs(append([]string{"--" + storageIDFloorFlag}, extra...))
		cmd.SetContext(context.Background())
		if err := cmd.Execute(); err == nil || !strings.Contains(stderr.String(), "takes no "+extra[0]) {
			t.Errorf("--id-floor %v = %v, stderr %q; want refused", extra, err, stderr.String())
		}
	}
}

// TestRepairSequenceIDFloorIsIdempotentUnderTheLock: the verb raises the
// floor past the manifest's ids, a second run writes nothing, it refuses while
// another process holds the controller lock, and it refuses a city whose copy
// recorded no manifest.
func TestRepairSequenceIDFloorIsIdempotentUnderTheLock(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}}
	request, target := floorCity(t, conf)
	prev := clearRetainedInfraCopiesFn
	clearRetainedInfraCopiesFn = func(string, infraBindingTarget, io.Writer) (infraClearResult, error) { return infraClearResult{}, nil }
	t.Cleanup(func() { clearRetainedInfraCopiesFn = prev })
	stubCityHashConfig(t, cityHashConfig{Conf: &fakeHashConfig{values: map[string]string{}}, Prefix: "mc"})
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("migrate = %d: %s", code, stderr)
	}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})

	run := func() (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := doStorageRepairIDFloor(context.Background(), request, &stdout, &stderr, "gc storage repair-sequence")
		return code, stdout.String(), stderr.String()
	}
	if code, out, errOut := run(); code != 0 || !strings.Contains(out, "min_hash_length 0 -> 7") || conf.values[bdMinHashLengthKey] != "7" {
		t.Fatalf("first run = %d %q %q, values %v", code, out, errOut, conf.values)
	}
	conf.sets = nil
	if code, out, _ := run(); code != 0 || len(conf.sets) != 0 || !strings.Contains(out, "already 7") {
		t.Fatalf("second run = %d %q, writes %v; want nothing written", code, out, conf.sets)
	}

	held, err := acquireControllerLock(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	conf.values[bdMinHashLengthKey] = ""
	code, _, errOut := run()
	_ = held.Close()
	if code == 0 || len(conf.sets) != 0 || !strings.Contains(errOut, "holds this city's controller lock (.gc/controller.lock)") {
		t.Fatalf("a run under a held controller lock = %d, writes %v: %s", code, conf.sets, errOut)
	}

	if err := os.Remove(target.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(); code == 0 || !strings.Contains(errOut, "records no copy manifest") {
		t.Fatalf("a run with no manifest = %d: %s", code, errOut)
	}
}

// TestAConvergedRerunAppliesTheFloor: a city an earlier build converged and
// cleared without a floor gets one from the next `gc storage migrate`. That
// rerun removes nothing and is how an interrupted relowering is finished, so
// a floor it cannot set (the write refused, or the read failing) is a loud
// warning naming the value, and the run carries on: it rewrites the
// served-binding note and exits 0.
func TestAConvergedRerunAppliesTheFloor(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{}}
	request, _ := floorCity(t, conf)
	stubCityHashConfig(t, cityHashConfig{Skipped: "a build without the floor"})
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("the cutover = %d: %s", code, stderr)
	}
	if len(conf.sets) != 0 {
		t.Fatalf("precondition: the cutover set %v", conf.sets)
	}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc", Hosted: "https://beads.example.test"})
	note := bornSplitServedNotePath(request.CityPath)
	for name, fail := range map[string]func(){
		"the write refused": func() { conf.setErr = errors.New("bd config set: refused") },
		"the read failing":  func() { conf.getErr = errors.New("bd config get: 502 bad gateway") },
	} {
		conf.setErr, conf.getErr = nil, nil
		fail()
		if err := os.Remove(note); err != nil {
			t.Fatal(err)
		}
		code, stderr := migrateFor(t, request)
		if code != 0 || !strings.Contains(stderr, "WARNING: this city has converged, but the id floor of its work store could not be set") || !strings.Contains(stderr, "min_hash_length=7") || !strings.Contains(stderr, "Nothing was removed and this run carries on") {
			t.Fatalf("%s: a converged rerun = %d, want 0 with a warning naming min_hash_length=7: %s", name, code, stderr)
		}
		if _, err := os.Stat(note); err != nil {
			t.Fatalf("%s: the rerun stopped before the served-binding note: %v", name, err)
		}
	}
	conf.setErr, conf.getErr = nil, nil
	if code, stderr := migrateFor(t, request); code != 0 || conf.values[bdMinHashLengthKey] != "7" || strings.Contains(stderr, "WARNING") {
		t.Fatalf("a converged rerun = %d, min_hash_length %q: %s", code, conf.values[bdMinHashLengthKey], stderr)
	}
	conf.sets = nil
	if code, stderr := migrateFor(t, request); code != 0 || len(conf.sets) != 0 {
		t.Fatalf("a second converged rerun = %d, writes %v: %s", code, conf.sets, stderr)
	}
}

// TestAMaxAboveBdsCapIsBroughtDownTo8: an earlier import may have set
// max_hash_length, or both settings, past 8 (bd mints no more; a hosted bd
// serve pads every id to it). The reservation brings each back to 8, the
// floor's one documented lowering, and a second run writes nothing.
func TestAMaxAboveBdsCapIsBroughtDownTo8(t *testing.T) {
	conf := &fakeHashConfig{values: map[string]string{bdMaxHashLengthKey: "13"}}
	stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})
	got, err := reserveCityIDs("/city", []string{"mc-q1w2e"})
	if err != nil || got.Floor != 6 || strings.Join(conf.sets, ",") != "min_hash_length=6,max_hash_length=8" {
		t.Fatalf("reserve = %+v (%v), writes %v; want min 6 and max brought down to 8", got, err, conf.sets)
	}
	conf.sets = nil
	if _, err := reserveCityIDs("/city", []string{"mc-q1w2e"}); err != nil || len(conf.sets) != 0 {
		t.Fatalf("a second run writes %v (%v), want nothing", conf.sets, err)
	}
	at := &fakeHashConfig{values: map[string]string{bdMaxHashLengthKey: "8"}}
	if _, _, err := raiseBdHashFloor(at, 6); err != nil || strings.Join(at.sets, ",") != "min_hash_length=6" {
		t.Fatalf("a max of exactly 8 writes %v (%v), want it left alone", at.sets, err)
	}
	// A min above the cap as well (an earlier import's 13/13) comes down to
	// 8 with the max: the floor's one documented lowering. A second run
	// writes nothing.
	both := &fakeHashConfig{values: map[string]string{bdMinHashLengthKey: "13", bdMaxHashLengthKey: "13"}}
	if floor, _, err := raiseBdHashFloor(both, 6); err != nil || floor != 8 || strings.Join(both.sets, ",") != "min_hash_length=8,max_hash_length=8" {
		t.Fatalf("min and max 13 = floor %d (%v), writes %v; want both brought down to 8", floor, err, both.sets)
	}
	both.sets = nil
	if floor, _, err := raiseBdHashFloor(both, 6); err != nil || floor != 8 || len(both.sets) != 0 {
		t.Fatalf("a second run = floor %d (%v), writes %v; want nothing", floor, err, both.sets)
	}
}

// TestTheCityFloorReportSaysWhatItWrote: the report names every setting the
// reservation wrote, from what to what, and a lowering says it was capped at
// bd's maximum; only a run that wrote nothing reports the floor already in
// force. A lowering from 13 read as "already 8" would tell `repair-sequence
// --id-floor` and the migrate output that nothing changed after a write to the
// store's bd config.
func TestTheCityFloorReportSaysWhatItWrote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		min, max string
		writes   string
		report   string
	}{
		{
			name: "both above the cap", min: "13", max: "13",
			writes: "min_hash_length=8,max_hash_length=8",
			report: "city work store min_hash_length 13 -> 8 (capped at bd's maximum), max_hash_length 13 -> 8 (capped at bd's maximum)",
		},
		{
			name: "only the max above the cap", min: "6", max: "13",
			writes: "max_hash_length=8",
			report: "city work store min_hash_length already 6, max_hash_length 13 -> 8 (capped at bd's maximum)",
		},
		{
			name: "the min raised under a max above the cap", max: "13",
			writes: "min_hash_length=6,max_hash_length=8",
			report: "city work store min_hash_length 0 -> 6, max_hash_length 13 -> 8 (capped at bd's maximum)",
		},
		{
			name: "the floor in force", min: "6", max: "8",
			report: "city work store min_hash_length already 6",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			if tc.min != "" {
				values[bdMinHashLengthKey] = tc.min
			}
			if tc.max != "" {
				values[bdMaxHashLengthKey] = tc.max
			}
			conf := &fakeHashConfig{values: values}
			stubCityHashConfig(t, cityHashConfig{Conf: conf, Prefix: "mc"})
			got, err := reserveCityIDs("/city", []string{"mc-k0nvi"})
			if err != nil || strings.Join(conf.sets, ",") != tc.writes || got.String() != tc.report {
				t.Fatalf("reserve = %q (%v), writes %v; want %q writing %q", got, err, conf.sets, tc.report, tc.writes)
			}
		})
	}
}
