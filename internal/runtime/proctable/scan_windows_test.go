//go:build windows

package proctable

import (
	"os"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func findLiveRuntime(found []runtime.LiveRuntime, pid int) (runtime.LiveRuntime, bool) {
	for _, live := range found {
		if live.PID == pid {
			return live, true
		}
	}
	return runtime.LiveRuntime{}, false
}

func livePIDs(found []runtime.LiveRuntime) []int {
	pids := make([]int, 0, len(found))
	for _, live := range found {
		pids = append(pids, live.PID)
	}
	return pids
}

// TestScanBySessionIDRefusesTheLiveTableWithoutTheOptIn is the Windows
// counterpart of the Linux #2839 guard: under `go test` the scanner must not
// enumerate the host, because the orphan sweep terminates any runtime missing
// from its (empty, under-test) bead store.
func TestScanBySessionIDRefusesTheLiveTableWithoutTheOptIn(t *testing.T) {
	if _, err := ScanBySessionID(""); err == nil {
		t.Fatal("ScanBySessionID enumerated the live process table under `go test`; the safety guard did not fire")
	}
	if _, err := ScanBySessionIDForExecutables("", []string{"agent"}); err == nil {
		t.Fatal("an executable scope must not lift the live-host scan guard")
	}
	if IsScanRoot(4) {
		t.Fatal("IsScanRoot consulted the live process table under `go test`; expected the safety guard to short-circuit to false")
	}
}

// TestScanBySessionIDStaysRefusedWithAFakeProcfsRoot pins that the Linux test
// seam cannot unlock the Windows scan. SetScanRootForTesting confines the Linux
// scanner to a fake tree, which is what makes lifting its refusal safe there.
// Nothing confines the Windows scanner, so a test that set the root would be
// reading, and handing the orphan sweep, the real host.
func TestScanBySessionIDStaysRefusedWithAFakeProcfsRoot(t *testing.T) {
	defer SetScanRootForTesting(t.TempDir())()

	if found, err := ScanBySessionID(""); err == nil {
		t.Fatalf("ScanBySessionID read the live process table (%d roots) once a fake procfs root was set; that root confines nothing on Windows", len(found))
	}
}

// TestEveryLiveHostReadStaysRefusedWithAFakeProcfsRoot carries the same pin to
// the two reads outside the scanner. RootStartIdentity and
// IsCityInfrastructureRoot read the live host as surely as ScanBySessionID
// does, so the Linux seam must not unlock them either.
func TestEveryLiveHostReadStaysRefusedWithAFakeProcfsRoot(t *testing.T) {
	mock := startArgvMock(t, ManagedDoltScopeWatchdogVerb)

	restoreRoot := SetScanRootForTesting(t.TempDir())
	identity, infrastructure := RootStartIdentity(mock.pid), IsCityInfrastructureRoot(mock.pid)
	restoreRoot()
	if identity != "" {
		t.Errorf("RootStartIdentity read the live host once a fake procfs root was set (got %q)", identity)
	}
	if infrastructure {
		t.Error("IsCityInfrastructureRoot read the live host once a fake procfs root was set")
	}

	// Opted in, both reads answer, so the refusals above are the guard's doing
	// rather than reads that could not have succeeded anyway.
	defer allowLiveWindowsScanForTesting()()
	if got := RootStartIdentity(mock.pid); got != mock.identity {
		t.Errorf("RootStartIdentity(%d) = %q with the live scan allowed, want %q", mock.pid, got, mock.identity)
	}
	if !IsCityInfrastructureRoot(mock.pid) {
		t.Errorf("IsCityInfrastructureRoot(%d) = false with the live scan allowed; the mock's argv is a watchdog's", mock.pid)
	}
}

func TestScanBySessionIDFindsASpawnedChild(t *testing.T) {
	defer allowLiveWindowsScanForTesting()()

	city := t.TempDir()
	wanted := uniqueSessionID(t, "wanted")
	other := uniqueSessionID(t, "other")
	target := startHelper(t, wanted, city)
	decoy := startHelper(t, other, city)

	found, err := ScanBySessionID(wanted)
	if err != nil {
		t.Fatalf("ScanBySessionID(%q): %v", wanted, err)
	}
	live, ok := findLiveRuntime(found, target.pid)
	if !ok {
		t.Fatalf("ScanBySessionID(%q) did not report helper PID %d; reported %v", wanted, target.pid, livePIDs(found))
	}
	if _, ok := findLiveRuntime(found, decoy.pid); ok {
		t.Fatalf("ScanBySessionID(%q) reported helper PID %d, which carries session %q", wanted, decoy.pid, other)
	}
	if live.SessionID != wanted {
		t.Errorf("SessionID = %q, want %q", live.SessionID, wanted)
	}
	if live.City != city {
		t.Errorf("City = %q, want %q (read from GC_CITY_PATH)", live.City, city)
	}
	if live.Epoch != helperEpoch {
		t.Errorf("Epoch = %d, want %d (read from GC_RUNTIME_EPOCH)", live.Epoch, helperEpoch)
	}
	if live.PPID != os.Getpid() {
		t.Errorf("PPID = %d, want this test process %d", live.PPID, os.Getpid())
	}
	if live.Name == "" {
		t.Error("Name is empty; the scan must report the process image")
	}
	if live.ParentIsProviderInfrastructure {
		t.Error("ParentIsProviderInfrastructure = true; this helper's parent is the test binary, not a tmux server")
	}
}

func TestScanAllReportsEveryHelperRoot(t *testing.T) {
	defer allowLiveWindowsScanForTesting()()

	city := t.TempDir()
	first := startHelper(t, uniqueSessionID(t, "all-first"), city)
	second := startHelper(t, uniqueSessionID(t, "all-second"), city)

	found, err := ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	for _, helper := range []*helperProcess{first, second} {
		if _, ok := findLiveRuntime(found, helper.pid); !ok {
			t.Errorf("ScanAll did not report helper PID %d carrying session %q; reported %v", helper.pid, helper.sessionID, livePIDs(found))
		}
	}
	if !slices.IsSorted(livePIDs(found)) {
		t.Errorf("ScanAll returned unsorted PIDs %v", livePIDs(found))
	}
}

func TestIsScanRootRecognisesASpawnedChild(t *testing.T) {
	defer allowLiveWindowsScanForTesting()()

	child := startHelper(t, uniqueSessionID(t, "scan-root"), t.TempDir())
	if !IsScanRoot(child.pid) {
		t.Errorf("IsScanRoot(%d) = false; a helper carrying its own session id outside its parent's envelope is a root", child.pid)
	}
	if IsScanRoot(os.Getpid()) {
		t.Error("IsScanRoot reported this process as a root")
	}
	if IsScanRoot(0) || IsScanRoot(-1) {
		t.Error("IsScanRoot accepted a non-positive PID")
	}
}

// TestProcessTableToleratesProcessesItCannotOpen pins the other half of the
// fail-closed rule against a real host: refusing to open another user's or a
// protected process is ordinary on Windows and must not fail the scan.
func TestProcessTableToleratesProcessesItCannotOpen(t *testing.T) {
	table, err := readProcessTable()
	if err != nil {
		t.Fatalf("readProcessTable: %v", err)
	}
	readable, unreadable := 0, 0
	outcomes := map[inspection]int{}
	for _, row := range table.rows {
		outcomes[row.inspection]++
		if row.envOK {
			readable++
			continue
		}
		unreadable++
		if row.pid < 0 || row.image == "" {
			t.Errorf("unreadable row %+v lost the fields that need no handle", row)
		}
	}
	t.Logf("process table: %d rows, %d with a readable environment, %d not openable by this user", len(table.rows), readable, unreadable)
	t.Logf("inspection outcomes: inspected=%d gone=%d denied=%d failed=%d",
		outcomes[inspected], outcomes[inspectionGone], outcomes[inspectionDenied], outcomes[inspectionFailed])
	for _, row := range table.uninspected {
		t.Logf("could not inspect PID %d (%s): %v", row.pid, row.image, row.inspectionErr)
	}
	if unreadable == 0 {
		t.Skip("every process on this host was readable, so there is nothing to tolerate")
	}
	if readable == 0 {
		t.Fatal("no environment at all was readable; readProcessTable should have refused this table")
	}

	defer allowLiveWindowsScanForTesting()()
	if _, err := ScanAll(); err != nil {
		t.Fatalf("the scan failed although %d of %d processes were merely not openable: %v", unreadable, len(table.rows), err)
	}
}

// TestScanTableBySessionIDAppliesTheRootRules drives the decision half of the
// scanner over a table nobody has to spawn: infrastructure is never a root, a
// child inside its parent's envelope is not a root, and a child of
// infrastructure is.
func TestScanTableBySessionIDAppliesTheRootRules(t *testing.T) {
	env := func(session string) map[string]string {
		return map[string]string{
			"GC_SESSION_ID":    session,
			"GC_CITY_PATH":     "C:\\city",
			"GC_RUNTIME_EPOCH": "3",
		}
	}
	table := indexProcessTable([]windowsProcess{
		{pid: 100, ppid: 900, image: "claude.exe", creation: 10, creationOK: true, env: env("S"), envOK: true},
		{pid: 101, ppid: 100, image: "claude.exe", creation: 20, creationOK: true, env: env("S"), envOK: true},
		{pid: 102, ppid: 900, image: "tmux.exe", creation: 5, creationOK: true, env: env("S"), envOK: true},
		{pid: 103, ppid: 102, image: "claude.exe", creation: 30, creationOK: true, env: env("S"), envOK: true},
		{pid: 104, ppid: 900, image: "claude.exe", creation: 40, creationOK: true, env: env("T"), envOK: true},
		{pid: 105, ppid: 900, image: "notepad.exe", creation: 50, creationOK: true, env: map[string]string{}, envOK: true},
	})

	got := livePIDs(scanTableBySessionID(table, "S"))
	if want := []int{100, 103}; !slices.Equal(got, want) {
		t.Fatalf("scanTableBySessionID(table, \"S\") = %v, want %v", got, want)
	}
	all := livePIDs(scanTableBySessionID(table, ""))
	if want := []int{100, 103, 104}; !slices.Equal(all, want) {
		t.Fatalf("scanTableBySessionID(table, \"\") = %v, want %v", all, want)
	}

	roots := scanTableBySessionID(table, "S")
	byPID := map[int]runtime.LiveRuntime{}
	for _, live := range roots {
		byPID[live.PID] = live
	}
	if byPID[100].PPID != 0 {
		t.Errorf("PID 100 reports PPID %d; its parent is not in the table, so the scan must not stand behind that number", byPID[100].PPID)
	}
	if byPID[100].ParentIsProviderInfrastructure {
		t.Error("PID 100 reports infrastructure parentage although its parent was never seen")
	}
	if byPID[103].PPID != 102 {
		t.Errorf("PID 103 reports PPID %d, want its live tmux parent 102", byPID[103].PPID)
	}
	if !byPID[103].ParentIsProviderInfrastructure {
		t.Error("PID 103's parent is a tmux server; ParentIsProviderInfrastructure must say so")
	}
	if byPID[103].Epoch != 3 || byPID[103].City != "C:\\city" {
		t.Errorf("PID 103 = %+v, want epoch 3 and city C:\\city", byPID[103])
	}
}

func TestIsTableScanRootRejectsInfrastructureAndEnvelopedChildren(t *testing.T) {
	env := map[string]string{"GC_SESSION_ID": "S"}
	rows := []windowsProcess{
		{pid: 100, ppid: 900, image: "claude.exe", creation: 10, creationOK: true, env: env, envOK: true},
		{pid: 101, ppid: 100, image: "claude.exe", creation: 20, creationOK: true, env: env, envOK: true},
		{pid: 102, ppid: 900, image: "tmux.exe", creation: 5, creationOK: true, env: env, envOK: true},
	}
	table := indexProcessTable(rows)
	for _, tc := range []struct {
		pid  int
		want bool
	}{
		{pid: 100, want: true},
		{pid: 101, want: false},
		{pid: 102, want: false},
	} {
		if got := isTableScanRoot(table, table.byPID[tc.pid]); got != tc.want {
			t.Errorf("isTableScanRoot(PID %d) = %v, want %v", tc.pid, got, tc.want)
		}
	}
}
