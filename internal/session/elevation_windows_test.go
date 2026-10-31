//go:build windows

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

const (
	testIntegrityMedium = 0x2000
	testIntegrityHigh   = 0x3000

	// testBoot stands in for the running boot's identity.
	testBoot = 1_790_000_000_000_000_000
	// testRecorderPID is the gc a pastElevatedStart record names.
	testRecorderPID = 900
)

// elevationStub stands in for this process's integrity level and identity, the
// running boot, which recorded gcs are still running, and the elevated
// processes a snapshot would find. It logs the process lookups in order.
type elevationStub struct {
	level      uint32
	pid        int
	created    int64
	boot       int64
	running    map[int]bool
	runningErr error
	survivors  []proctable.ProcessRecord

	calls []string
	asked []time.Time
}

func stubElevation(t *testing.T, level uint32, survivors []proctable.ProcessRecord) *elevationStub {
	t.Helper()
	stub := &elevationStub{
		level:     level,
		pid:       4100,
		created:   time.Now().Add(-time.Minute).UnixNano(),
		boot:      testBoot,
		running:   map[int]bool{},
		survivors: survivors,
	}
	prevLevel, prevSince, prevBoot, prevStart, prevRunning := currentIntegrity, elevatedProcessesSince, currentBoot, currentProcessStart, processRunning
	currentIntegrity = func() (uint32, error) { return stub.level, nil }
	currentBoot = func() (int64, error) { return stub.boot, nil }
	currentProcessStart = func() (int, int64, error) { return stub.pid, stub.created, nil }
	processRunning = func(pid int, _ int64) (bool, error) {
		stub.calls = append(stub.calls, fmt.Sprintf("running %d", pid))
		if stub.runningErr != nil {
			return false, stub.runningErr
		}
		return stub.running[pid], nil
	}
	elevatedProcessesSince = func(since time.Time) ([]proctable.ProcessRecord, error) {
		stub.calls = append(stub.calls, "survivors")
		stub.asked = append(stub.asked, since)
		return stub.survivors, nil
	}
	forgetOwnElevatedStarts()
	t.Cleanup(func() {
		currentIntegrity, elevatedProcessesSince, currentBoot, currentProcessStart, processRunning = prevLevel, prevSince, prevBoot, prevStart, prevRunning
		forgetOwnElevatedStarts()
	})
	return stub
}

func forgetOwnElevatedStarts() {
	ownElevatedStarts.Lock()
	ownElevatedStarts.byDir = nil
	ownElevatedStarts.Unlock()
}

// pastElevatedStart is a record from this boot, written by a gc that started
// sessions at since.
func pastElevatedStart(since time.Time) elevatedStart {
	return elevatedStart{
		since:     since.UnixNano(),
		integrity: testIntegrityHigh,
		pid:       testRecorderPID,
		created:   since.Add(-time.Minute).UnixNano(),
		boot:      testBoot,
	}
}

func writeElevatedRecord(t *testing.T, cityPath string, record elevatedStart) string {
	t.Helper()
	return writeElevatedRecordText(t, cityPath, record.fileName(), string(record.encode()))
}

func writeElevatedRecordText(t *testing.T, cityPath, name, text string) string {
	t.Helper()
	dir := elevatedSessionsPath(cityPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func readElevatedRecords(t *testing.T, cityPath string) []elevatedStart {
	t.Helper()
	records, err := readElevatedStarts(elevatedSessionsPath(cityPath))
	if err != nil {
		t.Fatalf("readElevatedStarts: %v", err)
	}
	return records
}

func TestAnUnelevatedStartRecordsNothing(t *testing.T) {
	city := t.TempDir()
	stubElevation(t, testIntegrityMedium, nil)

	if err := recordElevatedSessionStart(city); err != nil {
		t.Fatalf("recordElevatedSessionStart: %v", err)
	}
	if records := readElevatedRecords(t, city); len(records) != 0 {
		t.Fatalf("a start at medium integrity was recorded as elevated: %v", records)
	}
}

func TestAnElevatedStartIsRecorded(t *testing.T) {
	city := t.TempDir()
	stub := stubElevation(t, testIntegrityHigh, nil)

	before := time.Now()
	if err := recordElevatedSessionStart(city); err != nil {
		t.Fatalf("recordElevatedSessionStart: %v", err)
	}
	records := readElevatedRecords(t, city)
	if len(records) != 1 {
		t.Fatalf("an elevated start left %d records, want 1", len(records))
	}
	got := records[0]
	if got.integrity != testIntegrityHigh || got.pid != int64(stub.pid) || got.created != stub.created || got.boot != stub.boot {
		t.Fatalf("record = %+v, want integrity %#x, gc PID %d created %d, boot %d", got, testIntegrityHigh, stub.pid, stub.created, stub.boot)
	}
	if got.since > before.UnixNano() {
		t.Fatalf("recorded since %d is after the start began (%d); a session it creates could predate the record", got.since, before.UnixNano())
	}
	if err := got.validate(time.Now()); err != nil {
		t.Fatalf("the record written does not validate: %v", err)
	}
}

// TestEachElevatedGCKeepsItsOwnRecord covers a lost update: a
// second elevated gc recording a start must not move the city's earliest start
// past a session the first may still have running.
func TestEachElevatedGCKeepsItsOwnRecord(t *testing.T) {
	city := t.TempDir()
	stub := stubElevation(t, testIntegrityHigh, nil)
	if err := recordElevatedSessionStart(city); err != nil {
		t.Fatalf("first gc: %v", err)
	}
	first := readElevatedRecords(t, city)
	if len(first) != 1 {
		t.Fatalf("the first gc left %d records, want 1", len(first))
	}

	// A second gc, later: Windows' clock advances in ticks, so wait past one.
	time.Sleep(50 * time.Millisecond)
	stub.pid, stub.created = stub.pid+1, stub.created+1
	if err := recordElevatedSessionStart(city); err != nil {
		t.Fatalf("second gc: %v", err)
	}
	if records := readElevatedRecords(t, city); len(records) != 2 {
		t.Fatalf("two elevated gcs left %d records, want one each", len(records))
	}

	stub.level = testIntegrityMedium
	if err := checkElevatedSessions(city); err != nil {
		t.Fatalf("checkElevatedSessions: %v", err)
	}
	if len(stub.asked) != 1 || stub.asked[0].UnixNano() != first[0].since {
		t.Fatalf("elevated processes were asked for since %v, want once, since the first gc's start %d", stub.asked, first[0].since)
	}
}

// TestAGCRecordsOncePerCity keeps a long-running elevated gc to one record per
// city: the record of its first start covers its later ones.
func TestAGCRecordsOncePerCity(t *testing.T) {
	city := t.TempDir()
	stubElevation(t, testIntegrityHigh, nil)
	for range 3 {
		if err := recordElevatedSessionStart(city); err != nil {
			t.Fatalf("recordElevatedSessionStart: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if records := readElevatedRecords(t, city); len(records) != 1 {
		t.Fatalf("three starts by one gc left %d records, want 1", len(records))
	}
}

// TestAGCRecordsAgainOnceItsRecordIsGone writes a record removed from under a
// running gc again, rather than assuming it is still there.
func TestAGCRecordsAgainOnceItsRecordIsGone(t *testing.T) {
	city := t.TempDir()
	stubElevation(t, testIntegrityHigh, nil)
	if err := recordElevatedSessionStart(city); err != nil {
		t.Fatalf("recordElevatedSessionStart: %v", err)
	}
	records := readElevatedRecords(t, city)
	if len(records) != 1 {
		t.Fatalf("an elevated start left %d records, want 1", len(records))
	}
	if err := os.Remove(records[0].path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := recordElevatedSessionStart(city); err != nil {
		t.Fatalf("recordElevatedSessionStart: %v", err)
	}
	if records := readElevatedRecords(t, city); len(records) != 1 {
		t.Fatalf("a start after the record was removed left %d records, want 1", len(records))
	}
}

// TestAnInvalidRecordRefusesTheStart covers accepted nonsense: a record no gc
// could have written, such as integrity 0, refuses the start and names itself,
// rather than letting the gate pass.
func TestAnInvalidRecordRefusesTheStart(t *testing.T) {
	valid := pastElevatedStart(time.Now().Add(-time.Hour))
	with := func(change func(*elevatedStart)) string {
		record := valid
		change(&record)
		return string(record.encode())
	}
	cases := map[string]string{
		"not a record":             "not a record\n",
		"integrity zero":           with(func(r *elevatedStart) { r.integrity = 0 }),
		"medium integrity":         with(func(r *elevatedStart) { r.integrity = testIntegrityMedium }),
		"above system integrity":   with(func(r *elevatedStart) { r.integrity = 0x5000 }),
		"no start time":            with(func(r *elevatedStart) { r.since = 0 }),
		"a start time in future":   with(func(r *elevatedStart) { r.since = time.Now().Add(time.Hour).UnixNano() }),
		"no gc":                    with(func(r *elevatedStart) { r.pid = 0 }),
		"no gc creation time":      with(func(r *elevatedStart) { r.created = 0 }),
		"no boot":                  with(func(r *elevatedStart) { r.boot = 0 }),
		"a missing field":          strings.Join(strings.Fields(string(valid.encode()))[:4], " ") + "\n",
		"an extra field":           strings.TrimSpace(string(valid.encode())) + " extra=1\n",
		"fields out of order":      strings.Replace(strings.Replace(string(valid.encode()), "since=", "tmp=", 1), "integrity=", "since=", 1),
		"a non-numeric start time": strings.Replace(string(valid.encode()), "since=", "since=x", 1),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			path := writeElevatedRecordText(t, city, valid.fileName(), text)
			stub := stubElevation(t, testIntegrityMedium, nil)

			err := checkElevatedSessions(city)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "delete it once") {
				t.Fatalf("checkElevatedSessions = %v, want a refusal naming %s", err, path)
			}
			if len(stub.calls) != 0 {
				t.Fatalf("the gate looked past an invalid record to %v", stub.calls)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("the invalid record was removed: %v", err)
			}
		})
	}
}

// TestARunningRecorderRefusesALowerStart covers an in-flight start: while the
// elevated gc that wrote a record is running, a lower start is
// refused and the record kept, even with no elevated process created since it.
// That gc may be about to start a session, or have one starting that no
// snapshot shows yet.
func TestARunningRecorderRefusesALowerStart(t *testing.T) {
	city := t.TempDir()
	path := writeElevatedRecord(t, city, pastElevatedStart(time.Now().Add(-time.Second)))
	stub := stubElevation(t, testIntegrityMedium, nil)
	stub.running[testRecorderPID] = true

	err := checkElevatedSessions(city)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("PID %d", testRecorderPID)) {
		t.Fatalf("checkElevatedSessions = %v, want a refusal naming the running gc", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record of a gc still running was removed: %v", err)
	}
	if len(stub.asked) != 0 {
		t.Fatalf("elevated processes were listed while a recorder could still start one: %v", stub.calls)
	}
}

// TestRecordersAreConfirmedExitedBeforeTheSnapshot pins the order that lets
// "no survivors" mean something: a snapshot taken before a recorder was seen
// to exit can miss a session that recorder started in between.
func TestRecordersAreConfirmedExitedBeforeTheSnapshot(t *testing.T) {
	city := t.TempDir()
	first := pastElevatedStart(time.Now().Add(-time.Hour))
	second := pastElevatedStart(time.Now().Add(-time.Minute))
	second.pid++
	writeElevatedRecord(t, city, first)
	writeElevatedRecord(t, city, second)
	stub := stubElevation(t, testIntegrityMedium, nil)

	if err := checkElevatedSessions(city); err != nil {
		t.Fatalf("checkElevatedSessions: %v", err)
	}
	want := []string{fmt.Sprintf("running %d", first.pid), fmt.Sprintf("running %d", second.pid), "survivors"}
	if strings.Join(stub.calls, ", ") != strings.Join(want, ", ") {
		t.Fatalf("lookups = %v, want %v: every recorder confirmed exited before the snapshot", stub.calls, want)
	}
	if records := readElevatedRecords(t, city); len(records) != 0 {
		t.Fatalf("%d records survived with no recorder running and nothing elevated left", len(records))
	}
}

// TestARecordFromAnEarlierBootIsRetired covers a permanent refusal: nothing a
// record describes survives a restart, so a record from an
// earlier boot is retired without looking at a process, however many of this
// boot's elevated logon processes were created since it, and whatever its
// times say after the clock moved.
func TestARecordFromAnEarlierBootIsRetired(t *testing.T) {
	city := t.TempDir()
	old := pastElevatedStart(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	old.boot = testBoot - 1
	ahead := pastElevatedStart(time.Now().Add(time.Hour))
	ahead.boot, ahead.pid = testBoot-2, testRecorderPID+1
	oldPath := writeElevatedRecord(t, city, old)
	aheadPath := writeElevatedRecord(t, city, ahead)
	stub := stubElevation(t, testIntegrityMedium, []proctable.ProcessRecord{{PID: 17044, Name: "ctfmon.exe"}, {PID: 11464, Name: "taskhostw.exe"}})
	stub.running[testRecorderPID] = true // its PID can name anything in this boot

	if err := checkElevatedSessions(city); err != nil {
		t.Fatalf("checkElevatedSessions = %v, want nil: no session from an earlier boot is running", err)
	}
	for _, path := range []string{oldPath, aheadPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the record from an earlier boot %s was kept: %v", path, err)
		}
	}
	if len(stub.calls) != 0 {
		t.Fatalf("the gate looked at processes for records from an earlier boot: %v", stub.calls)
	}
}

func TestAnElevatedSessionThatCouldSurviveRefusesALowerStart(t *testing.T) {
	city := t.TempDir()
	since := time.Now().Add(-time.Hour).Truncate(time.Second)
	path := writeElevatedRecord(t, city, pastElevatedStart(since))
	stub := stubElevation(t, testIntegrityMedium, []proctable.ProcessRecord{{PID: 4242, Name: "claude.exe"}})

	err := checkElevatedSessions(city)
	if err == nil || !strings.Contains(err.Error(), "PID 4242 (claude.exe)") || !strings.Contains(err.Error(), "Restart") {
		t.Fatalf("checkElevatedSessions = %v, want a refusal naming PID 4242 and advising a restart", err)
	}
	if len(stub.asked) != 1 || !stub.asked[0].Equal(since) {
		t.Fatalf("elevated processes were asked for since %v, want exactly once since the recorded %v", stub.asked, since)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the record was dropped while an elevated process could still be a session: %v", statErr)
	}
}

func TestTheRecordIsRetiredOnceNothingElevatedSurvives(t *testing.T) {
	city := t.TempDir()
	path := writeElevatedRecord(t, city, pastElevatedStart(time.Now().Add(-time.Hour)))
	stubElevation(t, testIntegrityMedium, nil)

	if err := checkElevatedSessions(city); err != nil {
		t.Fatalf("checkElevatedSessions = %v, want nil once no elevated process survives", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the record survived with nothing elevated left to be a session: %v", err)
	}
}

// TestAnEquallyElevatedStartNeedsNoCheck also keeps the record: what a gc at
// the record's level sees as elevated says nothing about that level.
func TestAnEquallyElevatedStartNeedsNoCheck(t *testing.T) {
	city := t.TempDir()
	path := writeElevatedRecord(t, city, pastElevatedStart(time.Now().Add(-time.Hour)))
	stub := stubElevation(t, testIntegrityHigh, []proctable.ProcessRecord{{PID: 4242, Name: "claude.exe"}})

	if err := checkElevatedSessions(city); err != nil {
		t.Fatalf("checkElevatedSessions = %v, want nil: this gc can read what it would be looking for", err)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("an equally elevated gc looked up %v", stub.calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an equally elevated gc dropped the record: %v", err)
	}
}

func TestARecorderThatCannotBeCheckedRefusesTheStart(t *testing.T) {
	city := t.TempDir()
	path := writeElevatedRecord(t, city, pastElevatedStart(time.Now().Add(-time.Hour)))
	stub := stubElevation(t, testIntegrityMedium, nil)
	stub.runningErr = errors.New("Access is denied.")

	err := checkElevatedSessions(city)
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether") {
		t.Fatalf("checkElevatedSessions = %v, want a refusal: the recorder may still be running", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record was removed on an unanswered check: %v", err)
	}
}

// TestARecordsPathThatIsNotADirectoryRefuses keeps the single-file record an
// earlier build wrote from being read as no records at all.
func TestARecordsPathThatIsNotADirectoryRefuses(t *testing.T) {
	city := t.TempDir()
	dir := elevatedSessionsPath(city)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(dir, []byte("1790000000000000000 12288\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stubElevation(t, testIntegrityMedium, nil)

	if err := checkElevatedSessions(city); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("checkElevatedSessions = %v, want a refusal naming %s", err, dir)
	}
}

func createElevationTestSession(mgr *Manager, city string) error {
	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "chat", Command: "claude", WorkDir: city, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	return err
}

// TestCreateRefusesBeforeScanningWhileAnElevatedSessionCouldSurvive drives the
// gate through a real session start: the refusal comes before the scan and the
// start, and neither runs.
func TestCreateRefusesBeforeScanningWhileAnElevatedSessionCouldSurvive(t *testing.T) {
	city := t.TempDir()
	writeElevatedRecord(t, city, pastElevatedStart(time.Now().Add(-time.Hour)))
	stubElevation(t, testIntegrityMedium, []proctable.ProcessRecord{{PID: 4242, Name: "claude.exe"}})
	sp := &orphanScanProvider{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp, WithCityPath(city))

	if err := createElevationTestSession(mgr, city); err == nil || !strings.Contains(err.Error(), "elevated") {
		t.Fatalf("CreateSession = %v, want the elevated-session refusal", err)
	}
	if len(sp.events) != 0 {
		t.Fatalf("events = %v; nothing may be scanned or started past the refusal", sp.events)
	}
}

// TestCreateFromAnElevatedGCRecordsTheStart pins the other half end to end.
func TestCreateFromAnElevatedGCRecordsTheStart(t *testing.T) {
	city := t.TempDir()
	stubElevation(t, testIntegrityHigh, nil)
	sp := &orphanScanProvider{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp, WithCityPath(city))

	if err := createElevationTestSession(mgr, city); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if records := readElevatedRecords(t, city); len(records) != 1 {
		t.Fatalf("a session started from an elevated gc left %d records, want 1", len(records))
	}
}

// scanlessProvider exposes a runtime as a bare runtime.Provider: the shape of a
// provider that cannot scan the process table, such as ACP.
type scanlessProvider struct{ runtime.Provider }

// TestTheElevationFenceCoversAProviderThatCannotScan covers a capability
// bypass: the fence is about the city, so a start through a provider
// without the optional scanner is still recorded when elevated, and still
// refused when a lower gc could miss an elevated session.
func TestTheElevationFenceCoversAProviderThatCannotScan(t *testing.T) {
	city := t.TempDir()
	stub := stubElevation(t, testIntegrityHigh, nil)
	create := func() error {
		return createElevationTestSession(NewManagerWithOptions(beads.NewMemStore(), scanlessProvider{runtime.NewFake()}, WithCityPath(city)), city)
	}

	if err := create(); err != nil {
		t.Fatalf("elevated CreateSession: %v", err)
	}
	if records := readElevatedRecords(t, city); len(records) != 1 {
		t.Fatalf("an elevated start through a provider that cannot scan left %d records, want 1", len(records))
	}

	stub.level = testIntegrityMedium
	stub.survivors = []proctable.ProcessRecord{{PID: 4242, Name: "claude.exe"}}
	if err := create(); err == nil || !strings.Contains(err.Error(), "elevated") {
		t.Fatalf("lower CreateSession = %v, want the elevated-session refusal", err)
	}
}
