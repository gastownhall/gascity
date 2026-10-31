package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
	"pgregory.net/rapid"
)

// fakeSupervisorProcessTree points supervisorParentPID at a fixed process
// tree for the rest of the test: parents maps each PID to its parent, and a
// PID in readErrs fails its read with that error. Reading any other PID
// fails the test, so a row also pins how far the walk is allowed to go.
func fakeSupervisorProcessTree(t *testing.T, parents map[int]int, readErrs map[int]error) {
	t.Helper()
	old := supervisorParentPID
	supervisorParentPID = func(pid int) (int, error) {
		if err, ok := readErrs[pid]; ok {
			return 0, err
		}
		if parent, ok := parents[pid]; ok {
			return parent, nil
		}
		t.Errorf("supervisorParentPID(%d): PID is outside the fake process tree", pid)
		return 0, fs.ErrNotExist
	}
	t.Cleanup(func() { supervisorParentPID = old })
}

// installSupervisorUnitForTest points HOME at a temp dir, optionally writes
// gc's systemd user unit there, and stubs the systemctl is-active and MainPID
// probes for the rest of the test.
func installSupervisorUnitForTest(t *testing.T, writeUnit, unitActive bool, mainPID int, mainPIDOK bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if writeUnit {
		unitPath := supervisorSystemdServicePath()
		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldActive := supervisorSystemctlActive
	oldMainPID := supervisorSystemctlMainPID
	supervisorSystemctlActive = func(_ string) bool { return unitActive }
	supervisorSystemctlMainPID = func(_ string) (int, bool) { return mainPID, mainPIDOK }
	t.Cleanup(func() {
		supervisorSystemctlActive = oldActive
		supervisorSystemctlMainPID = oldMainPID
	})
}

// TestSupervisorDetermineUnitOwnership exercises
// supervisorDetermineUnitOwnership, which compares a live supervisor PID
// against gc's own systemd user unit for `gc supervisor status` and the
// doctor check (ga-9pjtoy). The unit owns the live supervisor only when that
// PID is its MainPID or a descendant of it. A process that merely sits in the
// unit's cgroup, such as a tmux pane child or a supervisor orphaned when its
// wrapper exited, is "outside_unit", as is any supervisor of an inactive
// unit. When the ancestry cannot be read the result is "outside_unit" too,
// carrying the probe error.
func TestSupervisorDetermineUnitOwnership(t *testing.T) {
	const (
		livePID        = 4242
		wrapperPID     = 4100 // MainPID when ExecStart is an sg wrapper
		paneShellPID   = 4300
		tmuxServerPID  = 3000
		userManagerPID = 1500 // systemd --user, the subreaper orphans reparent to
	)
	errLiveStat := errors.New("open /proc/4242/stat: no such file or directory")
	errShellStat := errors.New("open /proc/4200/stat: no such file or directory")

	cases := []struct {
		name         string
		writeUnit    bool
		unitActive   bool
		mainPID      int
		mainPIDOK    bool
		parents      map[int]int
		readErrs     map[int]error
		wantStatus   string
		wantProbeErr error
	}{
		{
			name:       "no unit installed",
			writeUnit:  false,
			wantStatus: "no_unit",
		},
		{
			name:       "unit installed, active, MainPID matches live supervisor",
			writeUnit:  true,
			unitActive: true,
			mainPID:    livePID,
			mainPIDOK:  true,
			wantStatus: "owned",
		},
		{
			name:       "unit active, live supervisor is the MainPID wrapper's child",
			writeUnit:  true,
			unitActive: true,
			mainPID:    wrapperPID,
			mainPIDOK:  true,
			parents:    map[int]int{livePID: wrapperPID},
			wantStatus: "owned",
		},
		{
			name:       "unit active, live supervisor is a grandchild of MainPID",
			writeUnit:  true,
			unitActive: true,
			mainPID:    wrapperPID,
			mainPIDOK:  true,
			parents:    map[int]int{livePID: 4200, 4200: wrapperPID},
			wantStatus: "owned",
		},
		{
			name:       "unit active, live supervisor forked from a tmux pane left in the unit cgroup",
			writeUnit:  true,
			unitActive: true,
			mainPID:    wrapperPID,
			mainPIDOK:  true,
			parents: map[int]int{
				livePID:        paneShellPID,
				paneShellPID:   tmuxServerPID,
				tmuxServerPID:  userManagerPID,
				userManagerPID: 1,
			},
			wantStatus: "outside_unit",
		},
		{
			name:       "unit active, live supervisor orphaned to systemd --user when its wrapper exited",
			writeUnit:  true,
			unitActive: true,
			mainPID:    wrapperPID,
			mainPIDOK:  true,
			parents:    map[int]int{livePID: userManagerPID, userManagerPID: 1},
			wantStatus: "outside_unit",
		},
		{
			name:       "unit installed but inactive",
			writeUnit:  true,
			unitActive: false,
			mainPID:    livePID,
			mainPIDOK:  true,
			wantStatus: "outside_unit",
		},
		{
			name:       "unit active but MainPID unknown",
			writeUnit:  true,
			unitActive: true,
			mainPID:    0,
			mainPIDOK:  false,
			wantStatus: "outside_unit",
		},
		{
			name:         "unit active but live supervisor ancestry unreadable",
			writeUnit:    true,
			unitActive:   true,
			mainPID:      wrapperPID,
			mainPIDOK:    true,
			readErrs:     map[int]error{livePID: errLiveStat},
			wantStatus:   "outside_unit",
			wantProbeErr: errLiveStat,
		},
		{
			name:         "unit active but ancestry unreadable partway to MainPID",
			writeUnit:    true,
			unitActive:   true,
			mainPID:      wrapperPID,
			mainPIDOK:    true,
			parents:      map[int]int{livePID: 4200},
			readErrs:     map[int]error{4200: errShellStat},
			wantStatus:   "outside_unit",
			wantProbeErr: errShellStat,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installSupervisorUnitForTest(t, tc.writeUnit, tc.unitActive, tc.mainPID, tc.mainPIDOK)
			fakeSupervisorProcessTree(t, tc.parents, tc.readErrs)

			got := supervisorDetermineUnitOwnership(livePID)
			if got.Status != tc.wantStatus {
				t.Fatalf("supervisorDetermineUnitOwnership(%d) = %+v, want Status %q", livePID, got, tc.wantStatus)
			}
			if tc.writeUnit && got.Unit == "" {
				t.Fatalf("supervisorDetermineUnitOwnership(%d) = %+v, want non-empty Unit when a unit is installed", livePID, got)
			}
			if tc.wantProbeErr == nil {
				if got.ProbeErr != nil {
					t.Fatalf("supervisorDetermineUnitOwnership(%d).ProbeErr = %v, want nil", livePID, got.ProbeErr)
				}
				return
			}
			if !errors.Is(got.ProbeErr, tc.wantProbeErr) {
				t.Fatalf("supervisorDetermineUnitOwnership(%d).ProbeErr = %v, want it to wrap %v", livePID, got.ProbeErr, tc.wantProbeErr)
			}
		})
	}
}

// TestSupervisorDetermineUnitOwnershipReadsRealProcessAncestry runs
// supervisorDetermineUnitOwnership with the production parent reader against
// this test process, whose parent is a real MainPID stand-in: the test process
// descends from its parent, and the parent does not descend from it.
func TestSupervisorDetermineUnitOwnershipReadsRealProcessAncestry(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("process ancestry is read from /proc, which only linux provides")
	}
	self, parent := os.Getpid(), os.Getppid()
	if parent <= 1 {
		t.Skipf("test process parent is PID %d, which cannot stand in for a unit MainPID", parent)
	}

	t.Run("child of MainPID is owned", func(t *testing.T) {
		installSupervisorUnitForTest(t, true, true, parent, true)
		got := supervisorDetermineUnitOwnership(self)
		if got.Status != "owned" || got.ProbeErr != nil {
			t.Fatalf("supervisorDetermineUnitOwnership(%d) with MainPID %d = %+v, want owned with no probe error", self, parent, got)
		}
	})
	t.Run("parent of MainPID is outside the unit", func(t *testing.T) {
		installSupervisorUnitForTest(t, true, true, self, true)
		got := supervisorDetermineUnitOwnership(parent)
		if got.Status != "outside_unit" || got.ProbeErr != nil {
			t.Fatalf("supervisorDetermineUnitOwnership(%d) with MainPID %d = %+v, want outside_unit with no probe error", parent, self, got)
		}
	})
}

// TestSupervisorStatusReportsUnverifiedUnitOwnership pins that `gc
// supervisor status` names the probe error, instead of claiming the
// supervisor runs outside its unit, when the supervisor's ancestry could not
// be read.
func TestSupervisorStatusReportsUnverifiedUnitOwnership(t *testing.T) {
	const livePID = 4242
	clearGCEnv(t)
	t.Setenv("GC_HOME", shortTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t, "gc-run-"))
	installSupervisorUnitForTest(t, true, true, 4100, true)
	errStat := errors.New("open /proc/4242/stat: no such file or directory")
	fakeSupervisorProcessTree(t, nil, map[int]error{livePID: errStat})
	startTestSupervisorSocket(t, supervisorSocketPath(), func(cmd string) string {
		if cmd == "ping" {
			return strconv.Itoa(livePID) + "\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	if code := supervisorStatusWithOptions(&stdout, &stderr, false); code != 0 {
		t.Fatalf("status = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := "Supervisor is running (PID 4242)\n" +
		"Warning: could not verify that systemd unit " + supervisorSystemdServiceName() +
		" owns the supervisor: reading the parent of PID 4242: " + errStat.Error() + "\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestGatherSupervisorUnitOwnership pins that the doctor check receives every
// field of supervisorDetermineUnitOwnership, including the probe error that
// makes an unverified "outside_unit" a warning rather than an error.
func TestGatherSupervisorUnitOwnership(t *testing.T) {
	const livePID, wrapperPID = 4242, 4100
	installSupervisorUnitForTest(t, true, true, wrapperPID, true)
	errStat := errors.New("open /proc/4242/stat: no such file or directory")
	fakeSupervisorProcessTree(t, nil, map[int]error{livePID: errStat})

	got := gatherSupervisorUnitOwnership(livePID)
	if !errors.Is(got.ProbeErr, errStat) {
		t.Errorf("gatherSupervisorUnitOwnership(%d).ProbeErr = %v, want it to wrap %v", livePID, got.ProbeErr, errStat)
	}
	got.ProbeErr = nil
	want := doctor.SupervisorUnitOwnership{
		Status:     "outside_unit",
		Unit:       supervisorSystemdServiceName(),
		UnitActive: true,
		UnitPID:    wrapperPID,
	}
	if got != want {
		t.Errorf("gatherSupervisorUnitOwnership(%d) = %+v, want %+v", livePID, got, want)
	}
}

// TestSupervisorProcessDescendsFrom pins the parent walk behind unit
// ownership: the hop bound, where the walk stops, and how a failed parent
// read surfaces.
func TestSupervisorProcessDescendsFrom(t *testing.T) {
	// chain links hops PIDs from 100 upward, so 100 is the PID and
	// 100+hops is its ancestor hops parent reads away.
	chain := func(hops int) map[int]int {
		parents := make(map[int]int, hops)
		for pid := 100; pid < 100+hops; pid++ {
			parents[pid] = pid + 1
		}
		return parents
	}
	errRead := errors.New("permission denied")

	cases := []struct {
		name     string
		pid      int
		ancestor int
		parents  map[int]int
		readErrs map[int]error
		want     bool
		wantErr  error
	}{
		{name: "the PID is the ancestor", pid: 42, ancestor: 42, want: true},
		{name: "ancestor exactly at the hop bound", pid: 100, ancestor: 100 + supervisorUnitAncestryMaxHops, parents: chain(supervisorUnitAncestryMaxHops), want: true},
		{name: "ancestor one hop past the bound", pid: 100, ancestor: 101 + supervisorUnitAncestryMaxHops, parents: chain(supervisorUnitAncestryMaxHops + 1), want: false},
		{name: "walk stops at PID 1 without reading its parent", pid: 42, ancestor: 7, parents: map[int]int{42: 1}, want: false},
		{name: "parent cycle from malformed /proc stays bounded", pid: 42, ancestor: 7, parents: map[int]int{42: 43, 43: 42}, want: false},
		{name: "ancestor 1 never matches", pid: 42, ancestor: 1, want: false},
		{name: "unknown MainPID never matches", pid: 42, ancestor: 0, want: false},
		{name: "non-positive PID never matches", pid: 0, ancestor: 0, want: false},
		{name: "failed read ends the walk with its error", pid: 42, ancestor: 7, parents: map[int]int{42: 43}, readErrs: map[int]error{43: errRead}, want: false, wantErr: errRead},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeSupervisorProcessTree(t, tc.parents, tc.readErrs)

			got, err := supervisorProcessDescendsFrom(tc.pid, tc.ancestor)
			if got != tc.want {
				t.Errorf("supervisorProcessDescendsFrom(%d, %d) = %v, want %v", tc.pid, tc.ancestor, got, tc.want)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("supervisorProcessDescendsFrom(%d, %d) error = %v, want nil", tc.pid, tc.ancestor, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("supervisorProcessDescendsFrom(%d, %d) error = %v, want it to wrap %v", tc.pid, tc.ancestor, err, tc.wantErr)
			}
			for pid := range tc.readErrs {
				if !strings.Contains(err.Error(), "PID "+strconv.Itoa(pid)) {
					t.Errorf("supervisorProcessDescendsFrom(%d, %d) error = %q, want it to name PID %d", tc.pid, tc.ancestor, err, pid)
				}
			}
		})
	}
}

// TestSupervisorProcessDescendsFromMatchesHopBoundModel checks the walk
// against a direct model on random process chains that end at init: a PID
// descends from an ancestor exactly when the ancestor is the PID itself or
// one of its first supervisorUnitAncestryMaxHops ancestors.
func TestSupervisorProcessDescendsFromMatchesHopBoundModel(t *testing.T) {
	old := supervisorParentPID
	t.Cleanup(func() { supervisorParentPID = old })

	rapid.Check(t, func(rt *rapid.T) {
		depth := rapid.IntRange(0, 2*supervisorUnitAncestryMaxHops).Draw(rt, "depth")
		chain := rapid.SliceOfNDistinct(rapid.IntRange(2, 1<<22), depth+1, depth+1, rapid.ID[int]).Draw(rt, "chain")
		parents := make(map[int]int, len(chain))
		for i, pid := range chain {
			parents[pid] = 1
			if i+1 < len(chain) {
				parents[pid] = chain[i+1]
			}
		}
		supervisorParentPID = func(pid int) (int, error) {
			parent, ok := parents[pid]
			if !ok {
				rt.Fatalf("supervisorParentPID(%d): read past the chain %v", pid, chain)
			}
			return parent, nil
		}

		at := rapid.IntRange(-1, depth).Draw(rt, "ancestorIndex")
		ancestor := rapid.IntRange(2, 1<<22).Filter(func(pid int) bool {
			_, inChain := parents[pid]
			return !inChain
		}).Draw(rt, "outsider")
		if at >= 0 {
			ancestor = chain[at]
		}
		want := at >= 0 && at <= supervisorUnitAncestryMaxHops

		got, err := supervisorProcessDescendsFrom(chain[0], ancestor)
		if err != nil {
			rt.Fatalf("supervisorProcessDescendsFrom(%d, %d) error = %v, want nil", chain[0], ancestor, err)
		}
		if got != want {
			rt.Fatalf("supervisorProcessDescendsFrom(%d, %d) = %v, want %v (ancestor index %d in chain %v)", chain[0], ancestor, got, want, at, chain)
		}
	})
}
