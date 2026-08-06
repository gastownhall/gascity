//go:build linux

package sqlite

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

const (
	sqliteFenceChildModeEnv = "GC_SQLITE_FENCE_CHILD_MODE"
	sqliteFenceChildPathEnv = "GC_SQLITE_FENCE_CHILD_PATH"
	sqliteFenceChildGenEnv  = "GC_SQLITE_FENCE_CHILD_GENERATION"
	sqliteFenceChildPlanEnv = "GC_SQLITE_FENCE_CHILD_PLAN"
	sqliteFenceBoundaryEnv  = "GC_SQLITE_FENCE_BOUNDARY"

	sqliteFenceChildSource         = "source"
	sqliteFenceChildBoundaryHolder = "boundary-holder"
	sqliteFenceChildGraphSnapshot  = "graph-snapshot"
	sqliteFenceChildLegacySnapshot = "legacy-snapshot"
	sqliteFenceChildWriter         = "writer"
	sqliteFenceChildReader         = "reader"
	sqliteFenceChildCheckpoint     = "checkpoint"
	sqliteFenceChildFenceHolder    = "fence-holder"
	sqliteFenceChildLockCensus     = "lock-census"
	sqliteFenceChildSHMReaderChurn = "shm-reader-churn"
	sqliteFenceChildRollbackSource = "rollback-source"
	sqliteFenceChildRollbackCrash  = "rollback-crash"
)

// TestSQLiteFenceHelperProcess is entered only by the real-process fence
// composition test below. The parent and source never share SQLite descriptors.
func TestSQLiteFenceHelperProcess(t *testing.T) {
	mode := os.Getenv(sqliteFenceChildModeEnv)
	if mode == "" {
		return
	}
	path := os.Getenv(sqliteFenceChildPathEnv)
	if path == "" {
		t.Fatal("missing SQLite fence child path")
	}
	switch mode {
	case sqliteFenceChildSource:
		runSQLiteFenceSourceChild(t, path)
	case sqliteFenceChildBoundaryHolder:
		runSQLiteFenceBoundaryHolderChild(t, path)
	case sqliteFenceChildGraphSnapshot:
		runSQLiteFenceGraphSnapshotChild(t, path)
	case sqliteFenceChildLegacySnapshot:
		runSQLiteFenceLegacySnapshotChild(t, path)
	case sqliteFenceChildWriter:
		runSQLiteFenceWriterChild(t, path)
	case sqliteFenceChildReader:
		runSQLiteFenceReaderChild(t, path)
	case sqliteFenceChildCheckpoint:
		runSQLiteFenceCheckpointChild(t, path)
	case sqliteFenceChildFenceHolder:
		runSQLiteFenceHolderChild(t, path)
	case sqliteFenceChildLockCensus:
		runSQLiteFenceLockCensusChild(t, path)
	case sqliteFenceChildSHMReaderChurn:
		runSQLiteFenceSHMReaderChurnChild(t, path)
	case sqliteFenceChildRollbackSource:
		runSQLiteFenceRollbackSourceChild(t, path)
	case sqliteFenceChildRollbackCrash:
		runSQLiteFenceRollbackCrashChild(t, path)
	default:
		t.Fatalf("unknown SQLite fence child mode %q", mode)
	}
}

func TestSQLiteLegacySnapshotSIGKILLAtBoundaries(t *testing.T) {
	for _, boundary := range []string{
		"legacy-snapshot-root-created",
		"legacy-snapshot-copy-before",
		"legacy-snapshot-copy-after",
		"legacy-private-recovery-before",
		"legacy-private-recovery-after",
		"legacy-claim-release-before",
		"legacy-claim-release-after",
	} {
		t.Run(boundary, func(t *testing.T) {
			city := t.TempDir()
			databasePath := filepath.Join(city, ".gc", "infra", ".beads", legacyCombinedDatabaseFilename)
			source := startSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildSource, databasePath, true,
			)
			source.kill(t)
			sourceDir := filepath.Dir(databasePath)
			before := snapshotLegacyCombinedSource(t, sourceDir)
			privateRoot := t.TempDir()

			command := exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$")
			command.Env = append(
				os.Environ(),
				"TMPDIR="+privateRoot,
				sqliteFenceBoundaryEnv+"="+boundary,
			)
			holder := startSQLiteFenceChild(t, command, sqliteFenceChildBoundaryHolder, databasePath, true)
			holder.kill(t)
			after := snapshotLegacyCombinedSource(t, sourceDir)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("SIGKILL at %q changed legacy source:\n--- before ---\n%#v\n--- after ---\n%#v", boundary, before, after)
			}

			command = exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$")
			command.Env = append(os.Environ(), "TMPDIR="+privateRoot)
			outcome := runSQLiteFenceChild(t, command, sqliteFenceChildLegacySnapshot, databasePath)
			requireSQLiteFenceChildOK(t, outcome)
			entries, err := os.ReadDir(privateRoot)
			if err != nil {
				t.Fatalf("reading private legacy snapshot root: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("private legacy snapshot residue after recovery from SIGKILL at %q: %v", boundary, names)
			}
			outcome = runSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildWriter, databasePath,
			)
			requireSQLiteFenceChildOK(t, outcome)
			outcome = runSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildReader, databasePath,
			)
			requireSQLiteFenceChildOK(t, outcome)
		})
	}
}

func TestSQLiteGraphSnapshotSIGKILLAtBoundaries(t *testing.T) {
	for _, boundary := range []string{
		"graph-snapshot-root-created",
		"graph-snapshot-copy-before",
		"graph-snapshot-copy-after",
		"graph-private-recovery-before",
		"graph-private-recovery-after",
	} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
			source := startSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildSource, databasePath, true,
			)
			source.kill(t)
			before := snapshotGraphSource(t, filepath.Dir(databasePath))
			privateRoot := t.TempDir()

			command := exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$")
			command.Env = append(
				os.Environ(),
				"TMPDIR="+privateRoot,
				sqliteFenceBoundaryEnv+"="+boundary,
			)
			holder := startSQLiteFenceChild(t, command, sqliteFenceChildBoundaryHolder, databasePath, true)
			holder.kill(t)
			after := snapshotGraphSource(t, filepath.Dir(databasePath))
			if !equalGraphSourceSnapshots(before, after) {
				t.Fatalf("SIGKILL at %q changed source:\n--- before ---\n%#v\n--- after ---\n%#v", boundary, before, after)
			}

			command = exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$")
			command.Env = append(os.Environ(), "TMPDIR="+privateRoot)
			outcome := runSQLiteFenceChild(t, command, sqliteFenceChildGraphSnapshot, databasePath)
			requireSQLiteFenceChildOK(t, outcome)
			entries, err := os.ReadDir(privateRoot)
			if err != nil {
				t.Fatalf("reading private snapshot root: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("private snapshot residue after recovery from SIGKILL at %q: %v", boundary, names)
			}
			outcome = runSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildWriter, databasePath,
			)
			requireSQLiteFenceChildOK(t, outcome)
			outcome = runSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildReader, databasePath,
			)
			requireSQLiteFenceChildOK(t, outcome)
		})
	}
}

func TestSQLiteWriterFenceSIGKILLAtReservationBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		sourceKind string
		boundaries []string
	}{
		{
			name:       "rollback",
			sourceKind: "rollback",
			boundaries: []string{
				"reservation-open-directory",
				"reservation-open-database",
				"reservation-open-sequence-floor",
				"reservation-lock-reserved",
				"reservation-lock-shared",
				"reservation-release-sequence-floor-before",
				"reservation-release-sequence-floor-after",
				"reservation-release-database-before",
				"reservation-release-database-after",
				"reservation-release-directory-before",
				"reservation-release-directory-after",
				"graph-claim-release-before",
				"graph-claim-release-after",
			},
		},
		{
			name:       "WAL with SHM",
			sourceKind: "wal-shm",
			boundaries: []string{
				"reservation-open-wal",
				"reservation-open-shm",
				"reservation-lock-shm-dms",
				"reservation-lock-shm-writers",
				"reservation-release-shm-before",
				"reservation-release-shm-after",
				"reservation-release-wal-before",
				"reservation-release-wal-after",
			},
		},
		{
			name:       "WAL without SHM",
			sourceKind: "wal-no-shm",
			boundaries: []string{
				"reservation-open-wal",
				"reservation-open-pending",
				"reservation-lock-pending",
				"reservation-release-pending-before",
				"reservation-release-pending-after",
				"reservation-release-wal-before",
				"reservation-release-wal-after",
			},
		},
		{
			name:       "hot rollback journal",
			sourceKind: "hot-journal",
			boundaries: []string{
				"reservation-open-journal",
				"reservation-release-journal-before",
				"reservation-release-journal-after",
			},
		},
	}

	for _, test := range tests {
		for _, boundary := range test.boundaries {
			t.Run(test.name+"/"+boundary, func(t *testing.T) {
				root := t.TempDir()
				databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
				switch test.sourceKind {
				case "rollback":
					outcome := runSQLiteFenceChild(t,
						exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
						sqliteFenceChildRollbackSource, databasePath,
					)
					requireSQLiteFenceChildOK(t, outcome)
					if err := os.WriteFile(filepath.Join(filepath.Dir(databasePath), graphSequenceFloorFilename), []byte("7\n"), 0o600); err != nil {
						t.Fatalf("writing sequence-floor fixture: %v", err)
					}
				case "wal-shm", "wal-no-shm":
					source := startSQLiteFenceChild(t,
						exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
						sqliteFenceChildSource, databasePath, true,
					)
					source.kill(t)
					if test.sourceKind == "wal-no-shm" {
						if err := os.Remove(databasePath + "-shm"); err != nil {
							t.Fatalf("removing crash-retained SQLite SHM: %v", err)
						}
					}
				case "hot-journal":
					source := startSQLiteFenceChild(t,
						exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
						sqliteFenceChildRollbackCrash, databasePath, true,
					)
					source.kill(t)
				default:
					t.Fatalf("unknown boundary source kind %q", test.sourceKind)
				}

				before := snapshotGraphSource(t, filepath.Dir(databasePath))
				command := exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$")
				command.Env = append(os.Environ(), sqliteFenceBoundaryEnv+"="+boundary)
				holder := startSQLiteFenceChild(t, command, sqliteFenceChildBoundaryHolder, databasePath, true)
				holder.kill(t)
				after := snapshotGraphSource(t, filepath.Dir(databasePath))
				if !equalGraphSourceSnapshots(before, after) {
					t.Fatalf("SIGKILL at %q changed source:\n--- before ---\n%#v\n--- after ---\n%#v", boundary, before, after)
				}

				fence := acquireProcessGraphFence(t, root, 64)
				if err := fence.Release(context.Background()); err != nil {
					t.Fatalf("releasing post-SIGKILL fence at %q: %v", boundary, err)
				}
				outcome := runSQLiteFenceChild(t,
					exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
					sqliteFenceChildWriter, databasePath,
				)
				requireSQLiteFenceChildOK(t, outcome)
				outcome = runSQLiteFenceChild(t,
					exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
					sqliteFenceChildReader, databasePath,
				)
				requireSQLiteFenceChildOK(t, outcome)
			})
		}
	}
}

func TestSQLiteWriterFenceUsesExactKernelLockModes(t *testing.T) {
	t.Run("rollback journal", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildRollbackSource, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
		fence := acquireProcessGraphFence(t, root, 61)
		requireSQLiteFenceLockPlan(t, databasePath, "rollback")
		if err := fence.Release(context.Background()); err != nil {
			t.Fatalf("releasing rollback-journal fence: %v", err)
		}
	})

	t.Run("WAL with usable SHM", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		fence := acquireProcessGraphFence(t, root, 62)
		requireSQLiteFenceLockPlan(t, databasePath, "wal-shm")
		if err := fence.Release(context.Background()); err != nil {
			t.Fatalf("releasing live-WAL fence: %v", err)
		}
		source.close(t)
	})

	t.Run("WAL without SHM", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		source.kill(t)
		if err := os.Remove(databasePath + "-shm"); err != nil {
			t.Fatalf("removing crash-retained SQLite SHM: %v", err)
		}
		fence := acquireProcessGraphFence(t, root, 63)
		requireSQLiteFenceLockPlan(t, databasePath, "wal-no-shm")
		if err := fence.Release(context.Background()); err != nil {
			t.Fatalf("releasing WAL-without-SHM fence: %v", err)
		}
	})
}

func acquireProcessGraphFence(t *testing.T, root string, generation storebinding.Generation) storebinding.WriterFence {
	t.Helper()
	inspection := inspectProcessGraph(t, root)
	fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: generation,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring Graph writer fence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	return fence
}

func requireSQLiteFenceLockPlan(t *testing.T, databasePath, plan string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$")
	command.Env = append(os.Environ(), sqliteFenceChildPlanEnv+"="+plan)
	outcome := runSQLiteFenceChild(t, command, sqliteFenceChildLockCensus, databasePath)
	requireSQLiteFenceChildOK(t, outcome)
}

func TestSQLiteSourceCensusPermitsContinuousReaderMarkChurn(t *testing.T) {
	tests := []struct {
		name       string
		path       func(string) string
		capture    func(string) (any, error)
		equalFence func(any, any) bool
	}{
		{
			name: "Graph",
			path: func(root string) string {
				return filepath.Join(root, graphDirectoryName, graphFilename)
			},
			capture: func(databasePath string) (any, error) {
				return captureGraphSource(databasePath)
			},
			equalFence: func(left, right any) bool {
				return left.(graphSourceState).equalForFence(right.(graphSourceState))
			},
		},
		{
			name: "legacy combined",
			path: func(root string) string {
				return filepath.Join(root, ".gc", "infra", ".beads", legacyCombinedDatabaseFilename)
			},
			capture: func(databasePath string) (any, error) {
				return captureLegacyCombinedSource(filepath.Dir(databasePath))
			},
			equalFence: func(left, right any) bool {
				return left.(legacyCombinedState).equalForFence(right.(legacyCombinedState))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databasePath := test.path(t.TempDir())
			source := startSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildSource, databasePath, true,
			)
			if err := os.Truncate(databasePath+"-shm", 32<<20); err != nil {
				t.Fatalf("extending live WAL-index census fixture: %v", err)
			}
			churner := startSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildSHMReaderChurn, databasePath, true,
			)

			before, err := test.capture(databasePath)
			if err != nil {
				t.Fatalf("capturing source under continuous reader churn: %v", err)
			}
			for attempt := 0; attempt < 3; attempt++ {
				after, err := test.capture(databasePath)
				if err != nil {
					t.Fatalf("recapturing source under continuous reader churn: %v", err)
				}
				if !test.equalFence(before, after) {
					t.Fatal("reader-mark churn changed authoritative fenced census")
				}
			}
			churner.close(t)
			source.close(t)
		})
	}
}

// TestSQLiteWriterFenceProcessComposition owns the real process boundary:
// independent SQLite processes must observe writer exclusion and clean kernel
// release, while a legitimate WAL reader remains permitted.
func TestSQLiteWriterFenceProcessComposition(t *testing.T) {
	t.Run("live WAL fence blocks independent writer then releases", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		inspection := inspectProcessGraph(t, root)
		if inspection.Complete() {
			t.Fatal("live WAL source unexpectedly completed before fencing")
		}
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 31,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("AcquireWriterFence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })

		before, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing fenced Graph source: %v", err)
		}
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
		after, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing blocked-writer source: %v", err)
		}
		if !before.equalForFence(after) {
			t.Fatalf("blocked child writer changed fenced source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
		}

		if err := fence.Release(context.Background()); err != nil {
			t.Fatalf("releasing Graph fence: %v", err)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
		source.close(t)
	})

	t.Run("live WAL fence permits independent reader with only SHM churn", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		inspection := inspectProcessGraph(t, root)
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 32,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("AcquireWriterFence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })
		before, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing fenced Graph source: %v", err)
		}
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildReader, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
		after, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing reader source: %v", err)
		}
		if !before.equalForFence(after) {
			t.Fatalf("independent WAL reader changed authoritative fenced source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
		}
		source.close(t)
	})

	t.Run("rollback-journal fence permits independent reader while writer is busy then releases", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildRollbackSource, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
		inspection := inspectProcessGraph(t, root)
		if !inspection.Complete() {
			t.Fatal("clean rollback-journal source unexpectedly requires a fence before inspection")
		}
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 36,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("AcquireWriterFence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })
		before, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing fenced rollback-journal source: %v", err)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildReader, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
		afterRead, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing rollback-journal source after reader: %v", err)
		}
		if !before.equal(afterRead) {
			t.Fatalf("independent rollback-journal reader changed fenced source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterRead)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
		afterWriter, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing rollback-journal source after blocked writer: %v", err)
		}
		if !before.equal(afterWriter) {
			t.Fatalf("blocked rollback-journal writer changed fenced source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterWriter)
		}
		if err := fence.Release(context.Background()); err != nil {
			t.Fatalf("releasing Graph fence: %v", err)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
	})

	t.Run("last live WAL close and checkpoint cannot mutate a fenced source", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		inspection := inspectProcessGraph(t, root)
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 33,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("AcquireWriterFence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })
		before, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing live WAL source before close: %v", err)
		}
		source.close(t)
		afterClose, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing source after live close: %v", err)
		}
		if !before.equalForFence(afterClose) {
			t.Fatalf("last live SQLite close changed fenced authoritative source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterClose)
		}
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildCheckpoint, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
		afterCheckpoint, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing source after blocked checkpoint: %v", err)
		}
		if !before.equalForFence(afterCheckpoint) {
			t.Fatalf("blocked checkpoint changed fenced authoritative source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterCheckpoint)
		}
		if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
			Target:             inspection.Target,
			Fence:              fence,
			ExpectedGeneration: 33,
		}); err != nil {
			t.Fatalf("InspectGraphFenced after live close: %v", err)
		}
		afterSnapshot, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing source after private snapshot: %v", err)
		}
		if !before.equalForFence(afterSnapshot) {
			t.Fatalf("private inspection changed fenced authoritative source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterSnapshot)
		}
	})

	t.Run("live WAL from a non-writable source snapshots privately", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		graphDir := filepath.Dir(databasePath)
		if err := os.Chmod(graphDir, 0o500); err != nil {
			t.Fatalf("making live WAL source non-writable: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(graphDir, 0o700) })
		before := snapshotGraphSource(t, graphDir)
		inspection := inspectProcessGraph(t, root)
		if inspection.Complete() {
			t.Fatal("live WAL source unexpectedly completed before fencing")
		}
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 37,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("AcquireWriterFence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })
		if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
			Target:             inspection.Target,
			Fence:              fence,
			ExpectedGeneration: 37,
		}); err != nil {
			t.Fatalf("InspectGraphFenced: %v", err)
		}
		after := snapshotGraphSource(t, graphDir)
		if !equalGraphSourceSnapshots(before, after) {
			t.Fatalf("fenced private snapshot changed non-writable live WAL source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
		}
		source.close(t)
	})

	t.Run("legacy live WAL snapshot fences child writer and preserves source record", func(t *testing.T) {
		city := t.TempDir()
		databasePath := filepath.Join(city, ".gc", "infra", ".beads", legacyCombinedDatabaseFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		sourceDir := filepath.Dir(databasePath)
		before, err := captureLegacyCombinedSource(sourceDir)
		if err != nil {
			t.Fatalf("capturing legacy live-WAL source before snapshot: %v", err)
		}

		reader, err := openTestLegacyCombinedSource(t, city, 38, func() {
			outcome := runSQLiteFenceChild(t,
				exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
				sqliteFenceChildWriter, databasePath,
			)
			requireSQLiteFenceChildBusy(t, outcome)
		})
		if err != nil {
			t.Fatalf("opening fenced legacy live-WAL source: %v", err)
		}
		t.Cleanup(func() { _ = reader.Close() })
		records, err := reader.ReadAll()
		if err != nil {
			t.Fatalf("reading fenced legacy live-WAL source: %v", err)
		}
		found := false
		for _, record := range records {
			if record.Bead.ID == "gcg-fence-child" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("legacy snapshot records = %#v, want gcg-fence-child", records)
		}
		after, err := captureLegacyCombinedSource(sourceDir)
		if err != nil {
			t.Fatalf("capturing legacy live-WAL source after snapshot: %v", err)
		}
		if !before.equalForFence(after) {
			t.Fatalf("legacy live-WAL snapshot changed source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
		}
		source.close(t)
	})

	t.Run("persistent WAL without SHM blocks new reader until release", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		source := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildSource, databasePath, true,
		)
		source.kill(t)
		if _, err := os.Stat(databasePath + "-wal"); err != nil {
			t.Fatalf("stating crash-retained SQLite WAL: %v", err)
		}
		if err := os.Remove(databasePath + "-shm"); err != nil {
			t.Fatalf("removing crash-retained SQLite SHM: %v", err)
		}
		if _, err := os.Stat(databasePath + "-shm"); !os.IsNotExist(err) {
			t.Fatalf("crash fixture retained SQLite SHM, stat error = %v", err)
		}
		inspection := inspectProcessGraph(t, root)
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 34,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("AcquireWriterFence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })
		before, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing WAL-without-SHM source: %v", err)
		}
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildReader, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
		afterBlockedRead, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing source after blocked reader: %v", err)
		}
		if !before.equal(afterBlockedRead) {
			t.Fatalf("blocked reader changed WAL-without-SHM source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterBlockedRead)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
		afterBlockedWriter, err := captureGraphSource(databasePath)
		if err != nil {
			t.Fatalf("capturing source after blocked WAL-without-SHM writer: %v", err)
		}
		if !before.equal(afterBlockedWriter) {
			t.Fatalf("blocked writer changed WAL-without-SHM source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterBlockedWriter)
		}
		if _, err := os.Stat(databasePath + "-shm"); !os.IsNotExist(err) {
			t.Fatalf("blocked WAL-without-SHM writer created SHM, stat error = %v", err)
		}
		if err := fence.Release(context.Background()); err != nil {
			t.Fatalf("releasing Graph fence: %v", err)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
	})

	t.Run("crashed rollback journal recovers only in private snapshot", func(t *testing.T) {
		root := t.TempDir()
		databasePath := filepath.Join(root, graphDirectoryName, graphFilename)
		child := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildRollbackCrash, databasePath, true,
		)
		child.kill(t)
		if err := validateSQLiteRollbackJournalPathForTest(databasePath + "-journal"); err != nil {
			t.Fatalf("crash-produced rollback journal failed validation: %v", err)
		}
		before := snapshotGraphSource(t, filepath.Dir(databasePath))

		inspection := inspectProcessGraph(t, root)
		if inspection.Complete() {
			t.Fatal("hot rollback-journal source unexpectedly completed before fencing")
		}
		fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
			Target:             inspection.Target,
			ExpectedGeneration: 35,
			Components:         []storebinding.ComponentID{GraphComponentID},
			Role:               storebinding.FenceRoleSource,
		})
		if err != nil {
			t.Fatalf("acquiring hot rollback-journal Graph fence: %v", err)
		}
		t.Cleanup(func() { _ = fence.Release(context.Background()) })

		originalOpen := openGraphInspectionDatabase
		originalClose := closeGraphInspectionDatabase
		originalRemove := removeGraphInspectionTempRoot
		t.Cleanup(func() {
			openGraphInspectionDatabase = originalOpen
			closeGraphInspectionDatabase = originalClose
			removeGraphInspectionTempRoot = originalRemove
		})
		var (
			phases             []string
			databaseModes      = make(map[*sql.DB]string)
			failOpenMode       string
			failCloseMode      string
			hotSnapshotOpens   int
			recoveredSnapshots int
			privateStateErr    error
		)
		openFailure := errors.New("injected Graph read-only reopen failure")
		closeFailure := errors.New("injected Graph inspection close failure")
		openGraphInspectionDatabase = func(driverName, dataSourceName string) (*sql.DB, error) {
			parsed, parseErr := url.Parse(dataSourceName)
			if parseErr != nil {
				return nil, parseErr
			}
			mode := parsed.Query().Get("mode")
			phases = append(phases, "open-"+mode)
			if mode == "rw" {
				if journalErr := validateSQLiteRollbackJournalPathForTest(parsed.Path + "-journal"); journalErr != nil {
					return nil, fmt.Errorf("validating private hot rollback journal before recovery: %w", journalErr)
				}
				hotSnapshotOpens++
			}
			if mode == failOpenMode {
				return nil, openFailure
			}
			database, openErr := originalOpen(driverName, dataSourceName)
			if database != nil {
				databaseModes[database] = mode
			}
			return database, openErr
		}
		closeGraphInspectionDatabase = func(database *sql.DB) error {
			mode := databaseModes[database]
			phases = append(phases, "close-"+mode)
			closeErr := originalClose(database)
			if mode == failCloseMode {
				return errors.Join(closeErr, closeFailure)
			}
			return closeErr
		}
		removeGraphInspectionTempRoot = func(root string) error {
			privateJournal := filepath.Join(root, graphDirectoryName, graphFilename+"-journal")
			if _, statErr := os.Stat(privateJournal); os.IsNotExist(statErr) {
				recoveredSnapshots++
			} else if statErr != nil {
				privateStateErr = errors.Join(privateStateErr, statErr)
			} else {
				privateStateErr = errors.Join(privateStateErr, errors.New("private hot rollback journal remained after recovery"))
			}
			return originalRemove(root)
		}

		assertFailure := func(name, openMode, closeMode string, wantPhases []string, wantErr error) {
			t.Helper()
			phases = nil
			failOpenMode = openMode
			failCloseMode = closeMode
			descriptor, inspectErr := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
				Target:             inspection.Target,
				Fence:              fence,
				ExpectedGeneration: 35,
			})
			if !errors.Is(inspectErr, wantErr) {
				t.Fatalf("%s error = %v, want %v", name, inspectErr, wantErr)
			}
			if !reflect.DeepEqual(descriptor, storebinding.Descriptor{}) {
				t.Fatalf("%s descriptor = %#v, want zero descriptor", name, descriptor)
			}
			if !reflect.DeepEqual(phases, wantPhases) {
				t.Fatalf("%s phases = %#v, want %#v", name, phases, wantPhases)
			}
		}

		assertFailure(
			"writable recovery close failure",
			"",
			"rw",
			[]string{"open-rw", "close-rw"},
			closeFailure,
		)
		assertFailure(
			"read-only reopen failure",
			"ro",
			"",
			[]string{"open-rw", "close-rw", "open-ro"},
			openFailure,
		)
		assertFailure(
			"read-only census close failure",
			"",
			"ro",
			[]string{"open-rw", "close-rw", "open-ro", "close-ro"},
			closeFailure,
		)

		phases = nil
		failOpenMode = ""
		failCloseMode = ""
		descriptor, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
			Target:             inspection.Target,
			Fence:              fence,
			ExpectedGeneration: 35,
		})
		if err != nil {
			t.Fatalf("InspectGraphFenced hot rollback-journal source: %v", err)
		}
		if err := descriptor.Validate(); err != nil {
			t.Fatalf("validating recovered Graph descriptor: %v", err)
		}
		wantPhases := []string{"open-rw", "close-rw", "open-ro", "close-ro"}
		if !reflect.DeepEqual(phases, wantPhases) {
			t.Fatalf("successful Graph private inspection phases = %#v, want %#v", phases, wantPhases)
		}
		if privateStateErr != nil {
			t.Fatalf("checking private recovery artifacts: %v", privateStateErr)
		}
		if hotSnapshotOpens != 4 {
			t.Fatalf("private hot-journal opens = %d, want 4", hotSnapshotOpens)
		}
		if recoveredSnapshots != 4 {
			t.Fatalf("recovered private snapshots = %d, want 4", recoveredSnapshots)
		}

		after := snapshotGraphSource(t, filepath.Dir(databasePath))
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("private rollback-journal recovery changed source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
		}
	})

	t.Run("SIGKILL holder releases component locks without source mutation", func(t *testing.T) {
		root := t.TempDir()
		graphDir := filepath.Join(root, graphDirectoryName)
		writer := openGraphSource(t, graphDir)
		if _, err := writer.Create(beads.Bead{Title: "killed holder source"}); err != nil {
			t.Fatalf("creating Graph source: %v", err)
		}
		if err := writer.CloseStore(); err != nil {
			t.Fatalf("closing Graph source: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".gc"), 0o700); err != nil {
			t.Fatalf("creating city guard directory: %v", err)
		}
		databasePath := filepath.Join(graphDir, graphFilename)
		before := snapshotGraphSource(t, graphDir)
		holder := startSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildFenceHolder, databasePath, true,
		)
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
		holder.kill(t)
		afterKill := snapshotGraphSource(t, graphDir)
		if !equalGraphSourceSnapshots(before, afterKill) {
			t.Fatalf("SIGKILL holder changed source:\n--- before ---\n%#v\n--- after ---\n%#v", before, afterKill)
		}
		outcome = runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildOK(t, outcome)
	})
}

func inspectProcessGraph(t *testing.T, root string) storebinding.Inspection {
	t.Helper()
	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	return inspection
}

func equalGraphSourceSnapshots(left, right graphSourceSnapshot) bool {
	return left.Directory == right.Directory && strings.Join(left.Entries, "\x00") == strings.Join(right.Entries, "\x00") && reflect.DeepEqual(left.Files, right.Files)
}

type sqliteFenceChild struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Scanner
	stderr   bytes.Buffer
	finished bool
}

func startSQLiteFenceChild(t *testing.T, cmd *exec.Cmd, mode, path string, waitReady bool) *sqliteFenceChild {
	t.Helper()
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, sqliteFenceChildModeEnv+"="+mode, sqliteFenceChildPathEnv+"="+path, sqliteFenceChildGenEnv+"=34")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("creating SQLite child stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("creating SQLite child stdout: %v", err)
	}
	child := &sqliteFenceChild{cmd: cmd, stdin: stdin, stdout: bufio.NewScanner(stdout)}
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting SQLite child %q: %v", mode, err)
	}
	t.Cleanup(func() { child.kill(t) })
	if waitReady {
		if line := child.line(t); line != "ready" {
			t.Fatalf("SQLite child %q readiness = %q, want ready", mode, line)
		}
	}
	return child
}

func runSQLiteFenceChild(t *testing.T, cmd *exec.Cmd, mode, path string) string {
	t.Helper()
	child := startSQLiteFenceChild(t, cmd, mode, path, false)
	line := child.line(t)
	if err := child.stdin.Close(); err != nil {
		t.Fatalf("closing SQLite child stdin: %v", err)
	}
	if err := child.wait(t); err != nil {
		t.Fatalf("waiting for SQLite child %q after protocol line %q: %v\nstderr:\n%s", mode, line, err, child.stderr.String())
	}
	return line
}

func (c *sqliteFenceChild) line(t *testing.T) string {
	t.Helper()
	lines := make(chan struct {
		line string
		ok   bool
	}, 1)
	go func() {
		ok := c.stdout.Scan()
		lines <- struct {
			line string
			ok   bool
		}{line: c.stdout.Text(), ok: ok}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	select {
	case result := <-lines:
		if !result.ok {
			t.Fatalf("SQLite child exited without a protocol line: %v", c.stdout.Err())
		}
		return result.line
	case <-ctx.Done():
		t.Fatalf("timed out waiting for SQLite child protocol line after %s", testutil.ExecRaceTimeout)
		return ""
	}
}

func (c *sqliteFenceChild) close(t *testing.T) {
	t.Helper()
	if c.finished {
		return
	}
	if _, err := io.WriteString(c.stdin, "close\n"); err != nil {
		t.Fatalf("sending SQLite child close: %v", err)
	}
	if line := c.line(t); line != "closed" {
		t.Fatalf("SQLite child close = %q, want closed", line)
	}
	if err := c.stdin.Close(); err != nil {
		t.Fatalf("closing SQLite child stdin: %v", err)
	}
	if err := c.wait(t); err != nil {
		t.Fatalf("waiting for SQLite child close: %v\nstderr:\n%s", err, c.stderr.String())
	}
}

func (c *sqliteFenceChild) kill(t *testing.T) {
	t.Helper()
	if c.finished {
		return
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.wait(t)
}

func (c *sqliteFenceChild) wait(t *testing.T) error {
	t.Helper()
	if c.finished {
		return nil
	}
	waited := make(chan error, 1)
	go func() { waited <- c.cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	select {
	case err := <-waited:
		c.finished = true
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out after %s", testutil.ExecRaceTimeout)
	}
}

func requireSQLiteFenceChildBusy(t *testing.T, outcome string) {
	t.Helper()
	if outcome != "busy" {
		t.Fatalf("SQLite child outcome = %q, want busy", outcome)
	}
}

func requireSQLiteFenceChildOK(t *testing.T, outcome string) {
	t.Helper()
	if !strings.HasPrefix(outcome, "ok") {
		t.Fatalf("SQLite child outcome = %q, want ok", outcome)
	}
}

func runSQLiteFenceSourceChild(t *testing.T, databasePath string) {
	store := openGraphSource(t, filepath.Dir(databasePath))
	if _, err := store.Create(beads.Bead{ID: "gcg-fence-child", Title: "fence child source"}); err != nil {
		_ = store.CloseStore()
		t.Fatalf("creating SQLite child source: %v", err)
	}
	if _, err := os.Stat(databasePath + "-wal"); err != nil {
		_ = store.CloseStore()
		t.Fatalf("stating live SQLite child WAL: %v", err)
	}
	emitSQLiteFenceChildLine("ready")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() || scanner.Text() != "close" {
		_ = store.CloseStore()
		return
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("closing SQLite child source: %v", err)
	}
	emitSQLiteFenceChildLine("closed")
}

func runSQLiteFenceBoundaryHolderChild(t *testing.T, databasePath string) {
	target := os.Getenv(sqliteFenceBoundaryEnv)
	if target == "" {
		t.Fatal("missing SQLite boundary target")
	}
	hit := false
	observeSQLiteBoundary = func(boundary string) {
		if hit || boundary != target {
			return
		}
		hit = true
		emitSQLiteFenceChildLine("ready")
		bufio.NewScanner(os.Stdin).Scan()
	}
	root := filepath.Dir(filepath.Dir(databasePath))
	if strings.HasPrefix(target, "legacy-snapshot-") ||
		strings.HasPrefix(target, "legacy-private-recovery-") ||
		strings.HasPrefix(target, "legacy-claim-release-") {
		outer := acquireCapturedLegacyFence(t, databasePath, 34)
		source, err := openLegacyCombinedSnapshot(context.Background(), outer)
		if err != nil {
			t.Fatalf("opening boundary-holder legacy snapshot: %v", err)
		}
		if err := source.Close(); err != nil {
			t.Fatalf("closing boundary-holder legacy snapshot: %v", err)
		}
		if err := outer.Release(context.Background()); err != nil {
			t.Fatalf("releasing boundary-holder legacy snapshot fence: %v", err)
		}
		if !hit {
			t.Fatalf("SQLite boundary %q was not reached", target)
		}
		return
	}
	if strings.HasPrefix(target, "graph-snapshot-") || strings.HasPrefix(target, "graph-private-recovery-") {
		outer, inspection := acquireCapturedProcessGraphFence(t, root, 34)
		if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
			Target:             inspection.Target,
			Fence:              outer,
			ExpectedGeneration: 34,
		}); err != nil {
			t.Fatalf("inspecting boundary-holder Graph snapshot: %v", err)
		}
		if err := outer.Release(context.Background()); err != nil {
			t.Fatalf("releasing boundary-holder Graph snapshot fence: %v", err)
		}
		if !hit {
			t.Fatalf("SQLite boundary %q was not reached", target)
		}
		return
	}
	fence := acquireProcessGraphFence(t, root, 34)
	if err := fence.Release(context.Background()); err != nil {
		t.Fatalf("releasing boundary-holder fence: %v", err)
	}
	if !hit {
		t.Fatalf("SQLite boundary %q was not reached", target)
	}
}

func runSQLiteFenceGraphSnapshotChild(t *testing.T, databasePath string) {
	root := filepath.Dir(filepath.Dir(databasePath))
	outer, inspection := acquireCapturedProcessGraphFence(t, root, 34)
	if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              outer,
		ExpectedGeneration: 34,
	}); err != nil {
		t.Fatalf("inspecting Graph snapshot: %v", err)
	}
	if err := outer.Release(context.Background()); err != nil {
		t.Fatalf("releasing Graph snapshot fence: %v", err)
	}
	emitSQLiteFenceChildLine("ok")
}

func runSQLiteFenceLegacySnapshotChild(t *testing.T, databasePath string) {
	outer := acquireCapturedLegacyFence(t, databasePath, 34)
	source, err := openLegacyCombinedSnapshot(context.Background(), outer)
	if err != nil {
		t.Fatalf("opening legacy snapshot: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing legacy snapshot: %v", err)
	}
	if err := outer.Release(context.Background()); err != nil {
		t.Fatalf("releasing legacy snapshot fence: %v", err)
	}
	emitSQLiteFenceChildLine("ok")
}

func acquireCapturedProcessGraphFence(t *testing.T, root string, generation storebinding.Generation) (storebinding.WriterFence, storebinding.Inspection) {
	t.Helper()
	inspection := inspectProcessGraph(t, root)
	cityGCDir := filepath.Join(root, ".gc")
	if err := os.MkdirAll(cityGCDir, 0o700); err != nil {
		t.Fatalf("creating captured-fence guard directory: %v", err)
	}
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		t.Fatalf("creating captured-fence guard scope: %v", err)
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, generation)
	if err != nil {
		t.Fatalf("acquiring captured-fence migration guard: %v", err)
	}
	t.Cleanup(func() { _ = guard.Release() })
	inspector, err := NewGraphInspector(storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("creating captured-fence Graph inspector: %v", err)
	}
	capturer := &capturingSQLiteFenceAcquirer{delegate: inspector}
	outer, err := storebinding.AcquireWriterFence(context.Background(), guard, capturer, storebinding.FenceRequest{
		Target:             inspection.Target,
		GuardScope:         scope,
		ExpectedGeneration: generation,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring captured Graph writer fence: %v", err)
	}
	if capturer.inner == nil {
		t.Fatal("captured Graph writer fence is nil")
	}
	t.Cleanup(func() { _ = outer.Release(context.Background()) })
	return outer, inspection
}

func acquireCapturedLegacyFence(t *testing.T, databasePath string, generation storebinding.Generation) storebinding.WriterFence {
	t.Helper()
	sourceDir := filepath.Dir(databasePath)
	city := filepath.Dir(filepath.Dir(filepath.Dir(sourceDir)))
	request, err := newLegacyCombinedFenceRequest(context.Background(), city, generation)
	if err != nil {
		t.Fatalf("creating captured legacy fence request: %v", err)
	}
	cityGCDir := filepath.Join(city, ".gc")
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		t.Fatalf("creating captured legacy guard scope: %v", err)
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, generation)
	if err != nil {
		t.Fatalf("acquiring captured legacy migration guard: %v", err)
	}
	t.Cleanup(func() { _ = guard.Release() })
	request.GuardScope = scope
	capturer := &capturingSQLiteFenceAcquirer{delegate: legacyCombinedFenceAcquirer{}}
	outer, err := storebinding.AcquireWriterFence(context.Background(), guard, capturer, request)
	if err != nil {
		t.Fatalf("acquiring captured legacy writer fence: %v", err)
	}
	if inner, ok := capturer.inner.(*legacyCombinedFence); !ok || inner == nil {
		t.Fatalf("captured legacy fence type = %T, want *legacyCombinedFence", capturer.inner)
	}
	t.Cleanup(func() { _ = outer.Release(context.Background()) })
	return outer
}

func runSQLiteFenceWriterChild(t *testing.T, databasePath string) {
	db := openSQLiteFenceChildDB(t, databasePath, "rw")
	defer db.Close() //nolint:errcheck
	_, err := db.ExecContext(context.Background(), `INSERT INTO kv(key, value) VALUES (?, ?)`, "sqlite-fence-child", "write")
	writeSQLiteFenceOutcome(err)
}

func runSQLiteFenceReaderChild(t *testing.T, databasePath string) {
	db := openSQLiteFenceChildDB(t, databasePath, "ro")
	defer db.Close() //nolint:errcheck
	var count int
	err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM beads`).Scan(&count)
	if err != nil {
		writeSQLiteFenceOutcome(err)
		return
	}
	emitSQLiteFenceChildLine("ok:%d", count)
}

func runSQLiteFenceSHMReaderChurnChild(t *testing.T, databasePath string) {
	db := openSQLiteFenceChildDB(t, databasePath, "ro")
	defer db.Close() //nolint:errcheck
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM beads`).Scan(&count); err != nil {
		t.Fatalf("reading SQLite source before WAL-index reader churn: %v", err)
	}
	shm, err := os.OpenFile(databasePath+"-shm", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening SQLite WAL-index for reader-mark churn: %v", err)
	}
	defer shm.Close() //nolint:errcheck
	original := make([]byte, 20)
	if _, err := shm.ReadAt(original, 100); err != nil {
		t.Fatalf("reading SQLite WAL-index reader marks: %v", err)
	}
	marks := append([]byte(nil), original...)
	stop := make(chan struct{})
	go func() {
		bufio.NewScanner(os.Stdin).Scan()
		close(stop)
	}()
	emitSQLiteFenceChildLine("ready")
	for {
		select {
		case <-stop:
			if _, err := shm.WriteAt(original, 100); err != nil {
				t.Fatalf("restoring SQLite WAL-index reader marks: %v", err)
			}
			emitSQLiteFenceChildLine("closed")
			return
		default:
			marks[0] ^= 1
			if _, err := shm.WriteAt(marks, 100); err != nil {
				t.Fatalf("churning SQLite WAL-index reader marks: %v", err)
			}
			runtime.Gosched()
		}
	}
}

type sqliteExpectedKernelLock struct {
	path        string
	lockType    int16
	start       int64
	length      int64
	description string
}

func runSQLiteFenceLockCensusChild(t *testing.T, databasePath string) {
	var expected []sqliteExpectedKernelLock
	database := func(lockType int16, start, length int64, description string) sqliteExpectedKernelLock {
		return sqliteExpectedKernelLock{path: databasePath, lockType: lockType, start: start, length: length, description: description}
	}
	shm := func(lockType int16, start, length int64, description string) sqliteExpectedKernelLock {
		return sqliteExpectedKernelLock{path: databasePath + "-shm", lockType: lockType, start: start, length: length, description: description}
	}
	switch plan := os.Getenv(sqliteFenceChildPlanEnv); plan {
	case "rollback":
		expected = []sqliteExpectedKernelLock{
			database(unix.F_UNLCK, sqlitePendingLockByte, 1, "PENDING"),
			database(unix.F_WRLCK, sqliteReservedLockByte, 1, "RESERVED"),
			database(unix.F_RDLCK, sqliteSharedLockStart, sqliteSharedLockCount, "database shared range"),
		}
	case "wal-shm":
		expected = []sqliteExpectedKernelLock{
			database(unix.F_UNLCK, sqlitePendingLockByte, 1, "PENDING"),
			database(unix.F_WRLCK, sqliteReservedLockByte, 1, "RESERVED"),
			database(unix.F_RDLCK, sqliteSharedLockStart, sqliteSharedLockCount, "database shared range"),
			shm(unix.F_WRLCK, sqliteWALWriteLockByte, sqliteWALLockCount, "WAL WRITE/CHECKPOINT/RECOVERY"),
			shm(unix.F_RDLCK, sqliteWALDeadManSwitchByte, 1, "WAL dead-man switch"),
		}
	case "wal-no-shm":
		expected = []sqliteExpectedKernelLock{
			database(unix.F_WRLCK, sqlitePendingLockByte, 1, "PENDING"),
			database(unix.F_WRLCK, sqliteReservedLockByte, 1, "RESERVED"),
			database(unix.F_RDLCK, sqliteSharedLockStart, sqliteSharedLockCount, "database shared range"),
		}
	default:
		t.Fatalf("unknown SQLite lock-census plan %q", plan)
	}
	for _, lock := range expected {
		if err := checkSQLiteKernelLock(lock); err != nil {
			emitSQLiteFenceChildLine("error:%v", err)
			return
		}
	}
	emitSQLiteFenceChildLine("ok")
}

func checkSQLiteKernelLock(expected sqliteExpectedKernelLock) error {
	file, err := os.Open(expected.path)
	if err != nil {
		return fmt.Errorf("opening %s lock-census component: %w", expected.description, err)
	}
	defer file.Close() //nolint:errcheck
	if expected.lockType == unix.F_UNLCK {
		got, err := querySQLiteKernelLock(file, unix.F_WRLCK, expected)
		if err != nil {
			return err
		}
		if got.Type != unix.F_UNLCK {
			return fmt.Errorf("%s lock = type %d at %d+%d, want unlocked", expected.description, got.Type, got.Start, got.Len)
		}
		return nil
	}
	requestType := int16(unix.F_RDLCK)
	if expected.lockType == unix.F_RDLCK {
		readQuery, err := querySQLiteKernelLock(file, unix.F_RDLCK, expected)
		if err != nil {
			return err
		}
		if readQuery.Type != unix.F_UNLCK {
			return fmt.Errorf("%s conflicts with a read lock: type %d at %d+%d", expected.description, readQuery.Type, readQuery.Start, readQuery.Len)
		}
		requestType = unix.F_WRLCK
	}
	got, err := querySQLiteKernelLock(file, requestType, expected)
	if err != nil {
		return err
	}
	if got.Type != expected.lockType || got.Start != expected.start || got.Len != expected.length {
		return fmt.Errorf(
			"%s lock = type %d at %d+%d, want type %d at %d+%d",
			expected.description,
			got.Type,
			got.Start,
			got.Len,
			expected.lockType,
			expected.start,
			expected.length,
		)
	}
	return nil
}

func querySQLiteKernelLock(file *os.File, requestType int16, expected sqliteExpectedKernelLock) (unix.Flock_t, error) {
	lock := unix.Flock_t{
		Type:   requestType,
		Whence: int16(unix.SEEK_SET),
		Start:  expected.start,
		Len:    expected.length,
	}
	if err := unix.FcntlFlock(file.Fd(), unix.F_GETLK, &lock); err != nil {
		return unix.Flock_t{}, fmt.Errorf("querying %s lock: %w", expected.description, err)
	}
	return lock, nil
}

func runSQLiteFenceCheckpointChild(t *testing.T, databasePath string) {
	db := openSQLiteFenceChildDB(t, databasePath, "rw")
	defer db.Close() //nolint:errcheck
	var busy, logFrames, checkpointedFrames int
	err := db.QueryRowContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames)
	if err != nil {
		writeSQLiteFenceOutcome(err)
		return
	}
	if busy != 0 {
		emitSQLiteFenceChildLine("busy")
		return
	}
	emitSQLiteFenceChildLine("ok")
}

func runSQLiteFenceHolderChild(t *testing.T, databasePath string) {
	root := filepath.Dir(filepath.Dir(databasePath))
	cityGCDir := filepath.Join(root, ".gc")
	if err := os.MkdirAll(cityGCDir, 0o700); err != nil {
		t.Fatalf("creating child city guard directory: %v", err)
	}
	inspection := inspectProcessGraph(t, root)
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		t.Fatalf("creating child guard scope: %v", err)
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, 34)
	if err != nil {
		t.Fatalf("acquiring child migration guard: %v", err)
	}
	defer guard.Release() //nolint:errcheck
	inspector, err := NewGraphInspector(storebinding.BindingSpec{Name: storebinding.BindingName("infra"), Provider: ProviderID, Path: root})
	if err != nil {
		t.Fatalf("creating child Graph inspector: %v", err)
	}
	fence, err := storebinding.AcquireWriterFence(context.Background(), guard, inspector, storebinding.FenceRequest{
		Target:             inspection.Target,
		GuardScope:         scope,
		ExpectedGeneration: 34,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring child Graph fence: %v", err)
	}
	defer fence.Release(context.Background()) //nolint:errcheck
	emitSQLiteFenceChildLine("ready")
	bufio.NewScanner(os.Stdin).Scan()
}

func runSQLiteFenceRollbackSourceChild(t *testing.T, databasePath string) {
	graphDir := filepath.Dir(databasePath)
	store := openGraphSource(t, graphDir)
	if _, err := store.Create(beads.Bead{ID: "gcg-rollback-source", Title: "rollback source"}); err != nil {
		_ = store.CloseStore()
		t.Fatalf("creating rollback source: %v", err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("closing rollback source store: %v", err)
	}

	db := openSQLiteFenceChildDB(t, databasePath, "rw")
	var journalMode string
	if err := db.QueryRowContext(context.Background(), `PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		_ = db.Close()
		t.Fatalf("switching rollback source to DELETE journaling: %v", err)
	}
	if journalMode != "delete" {
		_ = db.Close()
		t.Fatalf("rollback source journal mode = %q, want delete", journalMode)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO kv(key, value) VALUES (?, ?)`, "rollback-source", "ready"); err != nil {
		_ = db.Close()
		t.Fatalf("writing rollback source: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing rollback source database: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(databasePath + suffix); !os.IsNotExist(err) {
			t.Fatalf("rollback source retained SQLite sidecar %q: %v", suffix, err)
		}
	}
	emitSQLiteFenceChildLine("ok")
}

func runSQLiteFenceRollbackCrashChild(t *testing.T, databasePath string) {
	graphDir := filepath.Dir(databasePath)
	source := openGraphSource(t, graphDir)
	if _, err := source.Create(beads.Bead{ID: "gcg-rollback-crash", Title: "rollback crash source"}); err != nil {
		_ = source.CloseStore()
		t.Fatalf("creating rollback-crash source: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing rollback-crash source: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(databasePath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing WAL sidecar %q: %v", suffix, err)
		}
	}

	db := openSQLiteFenceChildDB(t, databasePath, "rw")
	defer db.Close() //nolint:errcheck
	var journalMode string
	if err := db.QueryRowContext(context.Background(), `PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		t.Fatalf("switching child source to rollback journaling: %v", err)
	}
	if journalMode != "delete" {
		t.Fatalf("rollback crash child journal mode = %q, want delete", journalMode)
	}
	for _, pragma := range []string{`PRAGMA synchronous=FULL`, `PRAGMA cache_size=1`, `PRAGMA cache_spill=ON`} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			t.Fatalf("configuring rollback crash child with %q: %v", pragma, err)
		}
	}
	if _, err := db.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("beginning rollback crash transaction: %v", err)
	}
	var lastErr error
	for index := 0; index < 256; index++ {
		key := fmt.Sprintf("rollback-crash-%03d", index)
		value := strings.Repeat(fmt.Sprintf("%03d", index), 2048)
		if _, err := db.ExecContext(context.Background(), `INSERT INTO kv(key, value) VALUES (?, ?)`, key, value); err != nil {
			t.Fatalf("writing rollback crash transaction record %d: %v", index, err)
		}
		lastErr = validateSQLiteRollbackJournalPathForTest(databasePath + "-journal")
		if lastErr == nil {
			emitSQLiteFenceChildLine("ready")
			bufio.NewScanner(os.Stdin).Scan()
			return
		}
	}
	t.Fatalf("transaction did not produce a valid hot rollback journal: %v", lastErr)
}

func openSQLiteFenceChildDB(t *testing.T, databasePath, mode string) *sql.DB {
	t.Helper()
	query := url.Values{}
	query.Set("mode", mode)
	query.Add("_pragma", "busy_timeout(0)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: databasePath, RawQuery: query.Encode()}).String())
	if err != nil {
		t.Fatalf("opening SQLite child database: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func validateSQLiteRollbackJournalPathForTest(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // test fixture descriptor
	return validateSQLiteRollbackJournal(context.Background(), file)
}

// emitSQLiteFenceChildLine writes one line of the stdout protocol the parent
// test reads from a fence helper process. A dropped line desynchronizes the
// parent, so a failed write aborts the child instead of continuing silently.
func emitSQLiteFenceChildLine(format string, args ...any) {
	if _, err := fmt.Fprintf(os.Stdout, format+"\n", args...); err != nil {
		panic(fmt.Sprintf("writing SQLite fence child protocol line: %v", err))
	}
}

func writeSQLiteFenceOutcome(err error) {
	if err == nil {
		emitSQLiteFenceChildLine("ok")
		return
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "sqlite_busy") || strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") {
		emitSQLiteFenceChildLine("busy")
		return
	}
	emitSQLiteFenceChildLine("error:%s", strings.ReplaceAll(err.Error(), "\n", " "))
}
