package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// unroutedReadyWorkReaders maps a scope path to the store the check reads
// there; a path with no entry fails to open.
func unroutedReadyWorkReaders(byPath map[string]beads.StoreReader) func(string) (beads.StoreReader, error) {
	return func(path string) (beads.StoreReader, error) {
		r, ok := byPath[path]
		if !ok {
			return nil, fmt.Errorf("no bead store at %q", path)
		}
		return r, nil
	}
}

// failingListReader is a store whose List always fails.
type failingListReader struct{ beads.StoreReader }

func (failingListReader) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("ledger unreachable")
}

func runUnroutedReadyWorkCheck(t *testing.T, cfg *config.City, cityPath string, byPath map[string]beads.StoreReader) *doctor.CheckResult {
	t.Helper()
	return newUnroutedReadyWorkCheck(cfg, cityPath, unroutedReadyWorkReaders(byPath)).Run(&doctor.CheckContext{})
}

// TestUnroutedReadyWorkWarnsForAScopeWithNoTarget is AC-7's warning half: a
// scope with no unrouted_work_target holding adoptable unrouted work gets an
// advisory warning naming the scope, the count, the sample ids and the
// remedies. Routed, held and assigned beads are not counted.
func TestUnroutedReadyWorkWarnsForAScopeWithNoTarget(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	cityPath := t.TempDir()
	held := plainCreatedBead("ct-3")
	held.Labels = []string{beadmeta.DispatchHoldLabels[0]}
	routed := plainCreatedBead("ct-4")
	routed.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "elsewhere"}
	assigned := plainCreatedBead("ct-5")
	assigned.Assignee = "someone"
	city := beads.NewMemStoreFrom(0, []beads.Bead{plainCreatedBead("ct-1"), plainCreatedBead("ct-2"), held, routed, assigned}, nil)
	rig := beads.NewMemStoreFrom(0, nil, nil)

	result := runUnroutedReadyWorkCheck(t, cfg, cityPath, map[string]beads.StoreReader{cityPath: city, cfg.Rigs[0].Path: rig})

	if result.Status != doctor.StatusWarning || result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("status=%v severity=%v, want an advisory warning: %#v", result.Status, result.Severity, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"city", "2 open bead(s)", "ct-1", "ct-2", "sets no unrouted_work_target"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, unwanted := range []string{"ct-3", "ct-4", "ct-5", "hello-world"} {
		if strings.Contains(details, unwanted) {
			t.Fatalf("details name %q, which is not adoptable unrouted work:\n%s", unwanted, details)
		}
	}
	for _, want := range []string{"unrouted_work_target", "gc sling <target> <id>", "hold"} {
		if !strings.Contains(result.FixHint, want) {
			t.Fatalf("fix hint missing remedy %q: %q", want, result.FixHint)
		}
	}
}

// TestUnroutedReadyWorkSamplesAtMostTenIDs pins the sample bound: the count
// is exact, the ids listed stop at ten.
func TestUnroutedReadyWorkSamplesAtMostTenIDs(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	var seed []beads.Bead
	for i := 1; i <= 12; i++ {
		seed = append(seed, plainCreatedBead(fmt.Sprintf("hw-%02d", i)))
	}
	result := runUnroutedReadyWorkCheck(t, cfg, t.TempDir(), map[string]beads.StoreReader{
		cfg.Rigs[0].Path: beads.NewMemStoreFrom(0, seed, nil),
	})

	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "rig hello-world: 12 open bead(s)") {
		t.Fatalf("details missing the rig scope's exact count:\n%s", details)
	}
	listed := 0
	for i := 1; i <= 12; i++ {
		if strings.Contains(details, fmt.Sprintf("hw-%02d", i)) {
			listed++
		}
	}
	if listed != 10 {
		t.Fatalf("details list %d sample ids, want 10:\n%s", listed, details)
	}
}

// TestUnroutedReadyWorkScopeWithTargetPointsAtTheController pins the other
// remedy: in a scope that adopts, unrouted work still waiting means the
// controller has not adopted it yet.
func TestUnroutedReadyWorkScopeWithTargetPointsAtTheController(t *testing.T) {
	cfg := adoptionRigConfig(t, "hello-world/researcher")
	result := runUnroutedReadyWorkCheck(t, cfg, t.TempDir(), map[string]beads.StoreReader{
		cfg.Rigs[0].Path: beads.NewMemStoreFrom(0, []beads.Bead{plainCreatedBead("hw-1")}, nil),
	})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "rig hello-world: 1 open bead(s)") || !strings.Contains(details, "has not adopted them yet") {
		t.Fatalf("details do not say the controller has not adopted yet:\n%s", details)
	}
	if !strings.Contains(result.FixHint, "route-recovery-quarantine") {
		t.Fatalf("fix hint = %q, want it to point at route-recovery-quarantine", result.FixHint)
	}
}

// TestUnroutedReadyWorkOKWithNoUnroutedWork is AC-7's OK half.
func TestUnroutedReadyWorkOKWithNoUnroutedWork(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	cityPath := t.TempDir()
	routed := plainCreatedBead("ct-1")
	routed.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "elsewhere"}
	result := runUnroutedReadyWorkCheck(t, cfg, cityPath, map[string]beads.StoreReader{
		cityPath:         beads.NewMemStoreFrom(0, []beads.Bead{routed}, nil),
		cfg.Rigs[0].Path: beads.NewMemStoreFrom(0, nil, nil),
	})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %#v", result.Status, result)
	}
}

// TestUnroutedReadyWorkUnreadableStoreIsSkippedNeverOK is AC-7's last
// clause: a store that cannot be opened or listed is reported as a skipped
// scope, and the check is not OK.
func TestUnroutedReadyWorkUnreadableStoreIsSkippedNeverOK(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	cityPath := t.TempDir()
	for name, byPath := range map[string]map[string]beads.StoreReader{
		"open fails": {cfg.Rigs[0].Path: beads.NewMemStoreFrom(0, nil, nil)},
		"list fails": {cityPath: failingListReader{}, cfg.Rigs[0].Path: beads.NewMemStoreFrom(0, nil, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			result := runUnroutedReadyWorkCheck(t, cfg, cityPath, byPath)

			if result.Status == doctor.StatusOK {
				t.Fatalf("status = OK with an unreadable city store: %#v", result)
			}
			details := strings.Join(result.Details, "\n")
			if !strings.Contains(details, "city skipped") {
				t.Fatalf("details do not report the city scope as skipped:\n%s", details)
			}
		})
	}
}
