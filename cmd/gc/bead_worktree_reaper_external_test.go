package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func addExternalReapWorktree(t *testing.T, rigRoot, parent, beadID string) string {
	t.Helper()
	worktreePath := filepath.Join(parent, beadID)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatalf("mkdir external worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-"+beadID, worktreePath)
	backdateWorktreeGitFile(t, worktreePath, 24*time.Hour)
	return worktreePath
}

func TestReapClosedBeadWorktrees_ReapsRegisteredExternalWorktreeFromClosedBeadMetadata(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	worktreePath := addExternalReapWorktree(t, rigRoot, filepath.Join(t.TempDir(), "formula-worktrees"), "ga-ext001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-ext001",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: worktreePath,
		},
	}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-ext001" {
		t.Fatalf("Reaped = %+v, want exactly ga-ext001\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("external worktree %s still present after reap (stat err=%v)", worktreePath, err)
	}
}

func TestReapClosedBeadWorktrees_ReapsSharedTerminalExternalWorktreeOnce(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	worktreePath := addExternalReapWorktree(t, rigRoot, filepath.Join(t.TempDir(), "formula-worktrees"), "ga-ext001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{
			ID:     "ga-attempt1",
			Status: "closed",
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:         worktreePath,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			},
		},
		{
			ID:     "ga-ext001",
			Status: "closed",
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:         worktreePath,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			},
		},
	}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-ext001" {
		t.Fatalf("Reaped = %+v, want exactly path-named source anchor ga-ext001\nProtected = %+v\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	if len(report.Protected) != 0 {
		t.Fatalf("Protected = %+v, want no ownership mismatch for shared terminal worktree", report.Protected)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("shared external worktree %s still present after reap (stat err=%v)", worktreePath, err)
	}
}

func TestReapClosedBeadWorktrees_RejectsUnsafeExternalCandidates(t *testing.T) {
	t.Run("rig root", func(t *testing.T) {
		cityPath, rigRoot := initReapRig(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:     "ga-root01",
			Status: "closed",
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey: rigRoot,
			},
		}}, nil)
		injectLiveness(t, liveWorktreeState{scanned: true})

		report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &bytes.Buffer{})

		if len(report.Reaped) != 0 || len(report.Protected) != 0 {
			t.Fatalf("rig root considered for reaping: %+v", report)
		}
		if _, err := os.Stat(rigRoot); err != nil {
			t.Fatalf("rig root was removed or damaged: %v", err)
		}
	})

	t.Run("plain directory", func(t *testing.T) {
		cityPath, rigRoot := initReapRig(t)
		plainDir := filepath.Join(t.TempDir(), "ga-plain01")
		if err := os.MkdirAll(plainDir, 0o755); err != nil {
			t.Fatalf("mkdir plain directory: %v", err)
		}
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:     "ga-plain01",
			Status: "closed",
			Metadata: map[string]string{
				beadmeta.LegacyWorkDirMetadataKey: plainDir,
			},
		}}, nil)
		injectLiveness(t, liveWorktreeState{scanned: true})

		report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &bytes.Buffer{})

		if len(report.Reaped) != 0 || len(report.Protected) != 0 {
			t.Fatalf("plain directory considered for reaping: %+v", report)
		}
		if _, err := os.Stat(plainDir); err != nil {
			t.Fatalf("plain directory was removed: %v", err)
		}
	})

	t.Run("registered external worktree without metadata", func(t *testing.T) {
		cityPath, rigRoot := initReapRig(t)
		worktreePath := addExternalReapWorktree(t, rigRoot, filepath.Join(t.TempDir(), "unowned"), "ga-unowned1")
		store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-unowned1", Status: "closed"}}, nil)
		injectLiveness(t, liveWorktreeState{scanned: true})

		report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &bytes.Buffer{})

		if len(report.Reaped) != 0 || len(report.Protected) != 0 {
			t.Fatalf("unowned external worktree considered for reaping: %+v", report)
		}
		if _, err := os.Stat(worktreePath); err != nil {
			t.Fatalf("unowned external worktree was removed: %v", err)
		}
	})
}

func addExternalWorktree(t *testing.T, rigRoot, parent, beadID string) string {
	t.Helper()
	path := filepath.Join(parent, beadID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir external worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-"+beadID, path)
	backdateWorktreeGitFile(t, path, 24*time.Hour)
	return path
}

type reapEventRecorder struct {
	events []events.Event
}

func (r *reapEventRecorder) Record(event events.Event) {
	r.events = append(r.events, event)
}

func externalReapSkippedReasons(t *testing.T, rec *reapEventRecorder) []events.BeadWorktreeReapSkippedPayload {
	t.Helper()
	var got []events.BeadWorktreeReapSkippedPayload
	for _, event := range rec.events {
		if event.Type != events.BeadWorktreeReapSkipped {
			continue
		}
		var payload events.BeadWorktreeReapSkippedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode reap-skipped payload: %v", err)
		}
		got = append(got, payload)
	}
	return got
}

// TestReapClosedBeadWorktrees_ReportsDirtyExternalFormulaWorktree proves a
// formula-owned worktree outside the conventional city subtree no longer
// disappears from the reaper's report merely because it is protected. The
// path remains and the event names both the owning bead and the unsafe state.
func TestReapClosedBeadWorktrees_ReportsDirtyExternalFormulaWorktree(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addExternalWorktree(t, rigRoot, filepath.Join(t.TempDir(), "formula-worktrees"), "ga-external1")
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("dirty external worktree: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "ga-external1", Status: "closed",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:         wt,
			beadmeta.FormulaContractMetadataKey: "graph.v2",
		},
	}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})
	rec := &reapEventRecorder{}

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, rec, nil, &stderr)

	if len(report.Reaped) != 0 || len(report.Protected) != 1 {
		t.Fatalf("Reaped=%+v Protected=%+v, want one protected external worktree\nstderr:\n%s", report.Reaped, report.Protected, stderr.String())
	}
	decision := report.Protected[0]
	if decision.BeadID != "ga-external1" || decision.Path != wt || !strings.Contains(decision.Reason, "uncommitted=true") {
		t.Fatalf("protected decision = %+v, want path %s, owner ga-external1, and dirty reason", decision, wt)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dirty external worktree was removed: %v", err)
	}
	payloads := externalReapSkippedReasons(t, rec)
	if len(payloads) != 1 || payloads[0].BeadID != "ga-external1" || payloads[0].Path != wt || !strings.Contains(payloads[0].Reason, "uncommitted=true") {
		t.Fatalf("reap-skipped payloads = %+v, want actionable owner/path/dirty reason", payloads)
	}
}

// TestReapClosedBeadWorktrees_LeavesUnreferencedExternalWorktreeAlone proves
// registration in the same repository is not ownership: an unrelated user
// worktree outside the city subtree is invisible unless a closed formula bead
// records that exact path.
func TestReapClosedBeadWorktrees_LeavesUnreferencedExternalWorktreeAlone(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addExternalWorktree(t, rigRoot, filepath.Join(t.TempDir(), "user-worktrees"), "ga-user0001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-other001", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})
	rec := &reapEventRecorder{}

	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, rec, nil, &bytes.Buffer{})

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("unrelated external worktree was classified: Reaped=%+v Protected=%+v", report.Reaped, report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("unrelated external worktree was touched: %v", err)
	}
	if got := externalReapSkippedReasons(t, rec); len(got) != 0 {
		t.Fatalf("unrelated external worktree emitted events: %+v", got)
	}
}

// TestReapClosedBeadWorktrees_ReportsExternalOwnershipMismatch proves a stale
// or forged metadata path fails closed and remains actionable even when the
// path is a directory but is not a worktree registered to the configured rig.
func TestReapClosedBeadWorktrees_ReportsExternalOwnershipMismatch(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	path := filepath.Join(t.TempDir(), "not-a-worktree")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir non-worktree: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "ga-mismatch1", Status: "closed",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:         path,
			beadmeta.FormulaContractMetadataKey: "graph.v2",
		},
	}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})
	rec := &reapEventRecorder{}

	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, rec, nil, &bytes.Buffer{})

	if len(report.Reaped) != 0 || len(report.Protected) != 1 {
		t.Fatalf("Reaped=%+v Protected=%+v, want mismatch protected", report.Reaped, report.Protected)
	}
	if got := report.Protected[0]; got.BeadID != "ga-mismatch1" || got.Path != path || !strings.Contains(got.Reason, "not a registered worktree") {
		t.Fatalf("mismatch decision = %+v, want owning bead, path, and registration reason", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ownership-mismatch path was removed: %v", err)
	}
	payloads := externalReapSkippedReasons(t, rec)
	if len(payloads) != 1 || payloads[0].BeadID != "ga-mismatch1" || !strings.Contains(payloads[0].Reason, "not a registered worktree") {
		t.Fatalf("mismatch events = %+v, want actionable owner and reason", payloads)
	}
}
