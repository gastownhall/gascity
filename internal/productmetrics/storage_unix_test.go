//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

func inspectStorageTestHome(t *testing.T, createRoot bool) gchome.ProductUsageHome {
	t.Helper()
	// The shared workspace lives below a deliberately group-writable /data.
	// Put this trust-boundary fixture below the supported root-owned sticky
	// ancestor instead.
	trustedTempRoot := "/tmp"
	if runtime.GOOS == "darwin" {
		trustedTempRoot = "/private/tmp"
	}
	// Go 1.26's testing.T.TempDir prefers GOTMPDIR over TMPDIR. Set both so
	// repository test runners may keep their build scratch space below /data
	// without moving this trust-boundary fixture below that unsafe ancestor.
	t.Setenv("GOTMPDIR", trustedTempRoot)
	t.Setenv("TMPDIR", trustedTempRoot)
	privateAncestor := t.TempDir()
	if err := os.Chmod(privateAncestor, 0o700); err != nil {
		t.Fatalf("Chmod private ancestor: %v", err)
	}
	homePath := filepath.Join(privateAncestor, ".gc")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatalf("Mkdir home: %v", err)
	}
	if createRoot {
		if err := os.Mkdir(filepath.Join(homePath, "product-usage"), 0o700); err != nil {
			t.Fatalf("Mkdir product root: %v", err)
		}
	}
	t.Setenv("GC_HOME", homePath)
	inspection, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
	if err != nil {
		t.Fatalf("InspectProductUsageHome: %v", err)
	}
	return inspection
}

func TestStorageAtomicWriteUsesPrivateModesAndDurabilitySteps(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var steps []storageStep
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		steps = append(steps, step)
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatalf("openStorageRootMutableWithHooks: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	rootInfo, err := os.Stat(inspection.Root())
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("root mode = %04o, want 0700", got)
	}
	steps = nil // Ignore directory-creation durability; inspect the file write.
	if err := root.writeFileAtomic("config.toml", []byte("preference = 'disabled'\n")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	fileInfo, err := os.Stat(filepath.Join(inspection.Root(), "config.toml"))
	if err != nil {
		t.Fatalf("Stat config: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", got)
	}
	wantSteps := []storageStep{
		storageStepDirectorySync, storageStepDirectorySync,
		storageStepMarkerCreate, storageStepFileSync, storageStepDirectorySync,
		storageStepDirectorySync, storageStepMarkerBind, storageStepFileSync, storageStepDirectorySync,
		storageStepWrite, storageStepFileSync, storageStepRename, storageStepDirectorySync,
		storageStepDelete, storageStepDirectorySync, storageStepDirectorySync, storageStepDirectorySync,
	}
	if fmt.Sprint(steps) != fmt.Sprint(wantSteps) {
		t.Fatalf("durability steps = %v, want %v", steps, wantSteps)
	}
	got, err := root.readFile("config.toml", 1024)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if string(got) != "preference = 'disabled'\n" {
		t.Fatalf("read bytes = %q", got)
	}
	if _, err := root.readFile("config.toml", 1); err == nil {
		t.Fatal("readFile accepted a file above the caller's byte limit")
	}
}

func TestStorageRejectsRelativeAndUnboundedNamesBeforeSyscalls(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"", ".", "..", "../outside", "/outside", "queue/event", `queue\event`, "space name", "café", strings.Repeat("x", 129)} {
		if err := root.writeFileAtomic(name, []byte("x")); err == nil {
			t.Errorf("writeFileAtomic accepted unsafe name %q", name)
		}
	}
	entries, err := os.ReadDir(inspection.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid names created entries: %v", entries)
	}
}

func TestStorageAllowsTrustedHomeComponentsOutsideMetadataAlphabet(t *testing.T) {
	base := inspectStorageTestHome(t, false)
	homePath := filepath.Join(filepath.Dir(base.Home().Path()), "gc home-é")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", homePath)
	inspection, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatalf("open trusted home with ordinary Unix path bytes: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStorageOpenDirRejectsCrossDeviceDescendantsBeforeDescending(t *testing.T) {
	for _, test := range []struct {
		name          string
		crossOnLookup int
	}{
		{name: "pre-open", crossOnLookup: 1},
		{name: "post-open-revalidation", crossOnLookup: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			boundary := filepath.Join(inspection.Root(), "queue", "generation")
			below := filepath.Join(boundary, "child")
			if err := os.MkdirAll(below, 0o700); err != nil {
				t.Fatal(err)
			}
			boundaryMetadata := 0
			boundaryOpens := 0
			belowOpens := 0
			root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
				metadata: func(path string, metadata storageMetadata) storageMetadata {
					if path == boundary {
						boundaryMetadata++
						if boundaryMetadata >= test.crossOnLookup {
							metadata.dev ^= 1 << 63
						}
					}
					return metadata
				},
				beforeDirectoryOpen: func(path string) error {
					switch path {
					case boundary:
						boundaryOpens++
					case below:
						belowOpens++
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			directory, openErr := root.openDir([]string{"queue", "generation", "child"}, false)
			if directory != nil {
				_ = directory.Close()
			}
			if !errors.Is(openErr, unix.EXDEV) || belowOpens != 0 {
				t.Fatalf("cross-device open = boundaryMetadata:%d boundaryOpens:%d belowOpens:%d err:%v",
					boundaryMetadata, boundaryOpens, belowOpens, openErr)
			}
			if test.crossOnLookup == 1 && boundaryOpens != 0 {
				t.Fatalf("pre-open cross-device boundary was opened %d times", boundaryOpens)
			}
		})
	}
}

func TestStorageReadOnlyOpenNeverCreatesOrRepairs(t *testing.T) {
	t.Run("missing root", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, false)
		if _, err := openStorageRootReadOnly(inspection); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("openStorageRootReadOnly error = %v, want not-exist", err)
		}
		if _, err := os.Lstat(inspection.Root()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read-only open created root: %v", err)
		}
	})

	t.Run("lax root", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, false)
		if err := os.Mkdir(inspection.Root(), 0o755); err != nil {
			t.Fatalf("Mkdir lax root: %v", err)
		}
		if _, err := openStorageRootReadOnly(inspection); err == nil {
			t.Fatal("read-only open accepted lax root")
		}
		info, err := os.Stat(inspection.Root())
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Fatalf("read-only open repaired mode to %04o, want unchanged 0755", got)
		}
	})

	t.Run("nested directory and mutations", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootReadOnly(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := root.Close(); err != nil {
				t.Errorf("Close read-only root: %v", err)
			}
		}()
		if _, err := root.openDir([]string{"queue"}, false); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read-only nested open error = %v, want not-exist", err)
		}
		if _, err := root.openDir([]string{"queue"}, true); err == nil {
			t.Fatal("read-only nested open accepted create=true")
		}
		if err := root.writeFileAtomic("config.toml", []byte("x")); err == nil {
			t.Fatal("read-only root accepted an atomic write")
		}
		if _, err := os.Lstat(filepath.Join(inspection.Root(), "queue")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read-only nested open created queue: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(inspection.Root(), "config.toml")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read-only write created config: %v", err)
		}
	})

	t.Run("owner-only modes are trusted without repair", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		path := filepath.Join(inspection.Root(), "config.toml")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(inspection.Root(), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(inspection.Root(), 0o700); err != nil {
				t.Errorf("restore root mode for cleanup: %v", err)
			}
		})
		root, err := openStorageRootReadOnly(inspection)
		if err != nil {
			t.Fatalf("open owner-only root: %v", err)
		}
		defer func() { _ = root.Close() }()
		if got, err := root.readFile("config.toml", 1); err != nil || string(got) != "x" {
			t.Fatalf("read owner-only file = %q, %v", got, err)
		}
		rootInfo, err := os.Stat(inspection.Root())
		if err != nil {
			t.Fatal(err)
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if rootInfo.Mode().Perm() != 0o500 || fileInfo.Mode().Perm() != 0o400 {
			t.Fatalf("read-only open repaired modes to root=%04o file=%04o", rootInfo.Mode().Perm(), fileInfo.Mode().Perm())
		}
	})
}

func TestStorageRejectsUnsafeFilesWithoutFollowingThem(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatalf("openStorageRootMutable: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(inspection.Root(), "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(inspection.Root(), "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(inspection.Root(), "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inspection.Root(), "lax"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(inspection.Root(), "lax"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inspection.Root(), "linked"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(inspection.Root(), "linked"), filepath.Join(inspection.Root(), "linked-again")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"symlink", "directory", "fifo", "lax", "linked", "linked-again"} {
		t.Run(name, func(t *testing.T) {
			if _, err := root.readFile(name, 1024); err == nil {
				t.Fatalf("readFile(%q) accepted unsafe entry", name)
			}
		})
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "secret" {
		t.Fatalf("symlink target changed: bytes=%q err=%v", got, err)
	}
}

func TestStorageMutationsRejectUnsafeEntriesWithoutChangingTargets(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("Close root: %v", err)
		}
	}()

	outRoot := t.TempDir()
	outside := filepath.Join(outRoot, "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(inspection.Root(), "config.toml")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic("config.toml", []byte("replacement")); err == nil {
		t.Fatal("atomic write accepted a symlink target")
	}
	if err := root.removeFile("config.toml"); err == nil {
		t.Fatal("delete accepted a symlink target")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "secret" {
		t.Fatalf("symlink target changed to %q, err=%v", got, err)
	}
	if info, err := os.Lstat(symlinkPath); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("rejected symlink was changed: info=%v err=%v", info, err)
	}

	linked := filepath.Join(inspection.Root(), "linked")
	if err := os.WriteFile(linked, []byte("event"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(linked, filepath.Join(inspection.Root(), "linked-again")); err != nil {
		t.Fatal(err)
	}
	destination, err := root.openDir([]string{"inflight"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := destination.Close(); err != nil {
			t.Errorf("Close destination: %v", err)
		}
	}()
	result, err := root.renameFile("linked", destination, "linked")
	if err == nil {
		t.Fatal("rename accepted a hard-linked source")
	}
	if result.state != storageRenameNotApplied {
		t.Fatalf("rejected hard-link rename state = %v, want not-applied", result.state)
	}
	if got, err := os.ReadFile(linked); err != nil || string(got) != "event" {
		t.Fatalf("rejected hard-link source changed to %q, err=%v", got, err)
	}
}

func TestStorageRejectsOwnerModeAndComponentIdentityDrift(t *testing.T) {
	t.Run("injected owner drift", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		hooks := storageTestHooks{metadata: func(path string, metadata storageMetadata) storageMetadata {
			if path == inspection.Root() {
				metadata.uid++
			}
			return metadata
		}}
		if _, err := openStorageRootMutableWithHooks(inspection, hooks); err == nil {
			t.Fatal("mutable open accepted wrong-owner root")
		}
	})

	t.Run("mode drift after inspection", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		if err := os.Chmod(inspection.Root(), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := openStorageRootMutable(inspection); err == nil {
			t.Fatal("mutable open accepted mode-drifted root")
		}
	})

	t.Run("component swapped after descriptor open", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		displaced := inspection.Root() + "-displaced"
		attacker := filepath.Join(t.TempDir(), "attacker")
		if err := os.Mkdir(attacker, 0o700); err != nil {
			t.Fatal(err)
		}
		swapped := false
		hooks := storageTestHooks{afterComponentOpen: func(path string) {
			if swapped || path != inspection.Root() {
				return
			}
			swapped = true
			if err := os.Rename(inspection.Root(), displaced); err != nil {
				t.Fatalf("swap rename: %v", err)
			}
			if err := os.Symlink(attacker, inspection.Root()); err != nil {
				t.Fatalf("swap symlink: %v", err)
			}
		}}
		if _, err := openStorageRootMutableWithHooks(inspection, hooks); err == nil {
			t.Fatal("open succeeded after the root name changed inode")
		}
		entries, err := os.ReadDir(attacker)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("component swap redirected storage into attacker directory: %v", entries)
		}
	})
}

func TestStorageRetainedDescriptorsPreventPostOpenComponentRedirection(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("Close root: %v", err)
		}
	}()
	queue, err := root.openDir([]string{"queue"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := queue.Close(); err != nil {
			t.Errorf("Close queue: %v", err)
		}
	}()
	inflight, err := root.openDir([]string{"inflight"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := inflight.Close(); err != nil {
			t.Errorf("Close inflight: %v", err)
		}
	}()

	displaced := inspection.Root() + "-displaced"
	attacker := filepath.Join(t.TempDir(), "attacker")
	if err := os.Mkdir(attacker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(inspection.Root(), displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, inspection.Root()); err != nil {
		t.Fatal(err)
	}

	if err := queue.writeFileAtomic("event.json", []byte("event")); err != nil {
		t.Fatalf("descriptor-relative write after swap: %v", err)
	}
	if result, err := queue.renameFile("event.json", inflight, "event.json"); err != nil || result.state != storageRenameAppliedDurable {
		t.Fatalf("descriptor-relative claim after swap: %v", err)
	}
	if result, err := inflight.renameFile("event.json", queue, "event.json"); err != nil || result.state != storageRenameAppliedDurable {
		t.Fatalf("descriptor-relative restore after swap: %v", err)
	}
	if err := queue.removeFile("event.json"); err != nil {
		t.Fatalf("descriptor-relative purge after swap: %v", err)
	}
	entries, err := os.ReadDir(attacker)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("post-open component swap redirected an operation: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(displaced, "queue", "event.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("purged file remains in retained tree: %v", err)
	}
}

func TestStorageOperationsRevalidateRetainedDirectoryTrust(t *testing.T) {
	t.Run("root mode drift after open", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := root.writeFileAtomic("config.toml", []byte("safe")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(inspection.Root(), 0o770); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(inspection.Root(), 0o700) })
		if _, err := root.readFile("config.toml", 1024); err == nil {
			t.Fatal("read continued after the retained root became group-writable")
		}
		if err := root.writeFileAtomic("second.toml", []byte("unsafe")); err == nil {
			t.Fatal("write continued after the retained root became group-writable")
		}
	})

	t.Run("nested mode drift after open", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		queue, err := root.openDir([]string{"queue"}, true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = queue.Close() }()
		queuePath := filepath.Join(inspection.Root(), "queue")
		if err := os.Chmod(queuePath, 0o707); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(queuePath, 0o700) })
		if err := queue.writeFileAtomic("event.json", []byte("unsafe")); err == nil {
			t.Fatal("nested write continued after retained-directory mode drift")
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(storageMetadata) storageMetadata
	}{
		{name: "owner", mutate: func(metadata storageMetadata) storageMetadata {
			metadata.uid++
			return metadata
		}},
		{name: "type", mutate: func(metadata storageMetadata) storageMetadata {
			metadata.mode = metadata.mode&^unix.S_IFMT | unix.S_IFREG
			return metadata
		}},
		{name: "link count", mutate: func(metadata storageMetadata) storageMetadata {
			metadata.nlink = 0
			return metadata
		}},
	} {
		t.Run("injected "+test.name+" drift", func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			hooks := storageTestHooks{metadata: func(path string, metadata storageMetadata) storageMetadata {
				if armed && path == inspection.Root() {
					return test.mutate(metadata)
				}
				return metadata
			}}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			armed = true
			if err := root.writeFileAtomic("config.toml", []byte("unsafe")); err == nil {
				t.Fatalf("write continued after injected retained-directory %s drift", test.name)
			}
		})
	}
}

func TestStorageAtomicWriteCleansTempsAtEveryFailure(t *testing.T) {
	tests := []struct {
		name            string
		step            storageStep
		wantFinalExists bool
	}{
		{name: "file sync", step: storageStepFileSync},
		{name: "rename", step: storageStepRename},
		{name: "parent sync", step: storageStepDirectorySync, wantFinalExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			failed := false
			renameStarted := false
			hooks := storageTestHooks{
				beforeMutation: func(step storageStep, _ string) {
					if step == storageStepRename {
						renameStarted = true
					}
				},
				beforeStep: func(step storageStep) error {
					if armed && !failed && step == test.step && (step != storageStepDirectorySync || renameStarted) {
						failed = true
						return errors.New("injected " + string(step))
					}
					return nil
				},
			}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatalf("open root: %v", err)
			}
			t.Cleanup(func() { _ = root.Close() })
			armed = true
			if err := root.writeFileAtomic("config.toml", []byte("new")); err == nil {
				t.Fatal("writeFileAtomic succeeded despite injected failure")
			}
			if !failed {
				t.Fatalf("failure hook for %s was not reached", test.step)
			}
			entries, err := os.ReadDir(inspection.Root())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".pm-tmp-") {
					t.Fatalf("temporary file survived failure: %s", entry.Name())
				}
			}
			_, err = os.Stat(filepath.Join(inspection.Root(), "config.toml"))
			if got := err == nil; got != test.wantFinalExists {
				t.Fatalf("final exists = %v, want %v (stat error %v)", got, test.wantFinalExists, err)
			}
		})
	}

	t.Run("rejected temporary metadata", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		hooks := storageTestHooks{metadata: func(path string, metadata storageMetadata) storageMetadata {
			if strings.HasPrefix(filepath.Base(path), ".pm-tmp-") {
				metadata.nlink = 2
			}
			return metadata
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := root.writeFileAtomic("config.toml", []byte("new")); err == nil {
			t.Fatal("atomic write accepted injected unsafe temp metadata")
		}
		requireOnlyPersistentRootTempJournal(t, inspection.Root())
	})
}

func TestStorageAtomicNoReplaceAndWriteFailureSeams(t *testing.T) {
	t.Run("no-replace creation preserves existing event", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		var steps []storageStep
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			steps = append(steps, step)
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		steps = nil
		if err := root.writeFileAtomicNoReplace("event.json", []byte("first")); err != nil {
			t.Fatalf("first no-replace write: %v", err)
		}
		if countStep(steps, storageStepFileSync) != 3 || countStep(steps, storageStepRename) != 1 ||
			countStep(steps, storageStepDirectorySync) != 9 || countStep(steps, storageStepMarkerCreate) != 1 ||
			countStep(steps, storageStepMarkerBind) != 1 {
			t.Fatalf("no-replace durability steps = %v", steps)
		}
		if err := root.writeFileAtomicNoReplace("event.json", []byte("second")); !errors.Is(err, errStorageEntryExists) {
			t.Fatalf("second no-replace write error = %v, want entry-exists", err)
		}
		if got, err := root.readFile("event.json", 1024); err != nil || string(got) != "first" {
			t.Fatalf("existing no-replace event = %q, %v", got, err)
		}
	})

	t.Run("rejects staged source replacement at mutation boundary", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		var tempPath string
		var displacedPath string
		var injectedErr error
		armed := false
		swapped := false
		hooks := storageTestHooks{
			beforeTempFileCreate: func(path string) { tempPath = path },
			beforeMutation: func(step storageStep, _ string) {
				if !armed || swapped || step != storageStepRename {
					return
				}
				swapped = true
				displacedPath = tempPath + ".displaced"
				injectedErr = os.Rename(tempPath, displacedPath)
				if injectedErr == nil {
					injectedErr = os.WriteFile(tempPath, []byte("attacker"), 0o600)
				}
			},
		}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		armed = true
		writeErr := root.writeFileAtomicNoReplace("event.json", []byte("source"))
		_, targetErr := os.Lstat(filepath.Join(inspection.Root(), "event.json"))
		displacedData, displacedErr := os.ReadFile(displacedPath)
		if injectedErr != nil || !swapped || !errors.Is(writeErr, errStorageEntryChanged) ||
			!errors.Is(targetErr, fs.ErrNotExist) || displacedErr != nil || string(displacedData) != "source" {
			t.Fatalf("staged source replacement = swapped:%v injected:%v write:%v target:%v displaced:%q displacedErr:%v",
				swapped, injectedErr, writeErr, targetErr, displacedData, displacedErr)
		}
	})

	for _, test := range []struct {
		name string
		step storageStep
	}{
		{name: "write", step: storageStepWrite},
		{name: "no-replace install", step: storageStepRename},
	} {
		t.Run(test.name+" failure", func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			failed := false
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if armed && !failed && step == test.step {
					failed = true
					return errors.New("injected " + test.name + " failure")
				}
				return nil
			}}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			armed = true
			if err := root.writeFileAtomicNoReplace("event.json", []byte("event")); err == nil {
				t.Fatalf("no-replace write succeeded despite injected %s failure", test.name)
			}
			if !failed {
				t.Fatalf("%s failure seam was not reached", test.name)
			}
			journalEntries := requireOnlyPersistentRootTempJournal(t, inspection.Root())
			if len(journalEntries) != 0 {
				t.Fatalf("failed no-replace write left journal markers: %v", journalEntries)
			}
		})
	}
}

func TestStorageDeleteRetriesDirectorySyncAfterUncertainFailure(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	failNextDirectorySync := false
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if failNextDirectorySync && step == storageStepDirectorySync {
			failNextDirectorySync = false
			return errors.New("injected parent sync failure")
		}
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("Close root: %v", err)
		}
	}()
	if err := root.writeFileAtomic("event.json", []byte("event")); err != nil {
		t.Fatal(err)
	}
	failNextDirectorySync = true
	if err := root.removeFile("event.json"); err == nil {
		t.Fatal("delete succeeded despite failed parent-directory sync")
	}
	if _, err := os.Lstat(filepath.Join(inspection.Root(), "event.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("delete failure left the unlinked file visible: %v", err)
	}
	// A missing-file retry still syncs the retained parent descriptor, making
	// the prior uncertain unlink durable before reporting success.
	if err := root.removeFile("event.json"); err != nil {
		t.Fatalf("retry durable delete: %v", err)
	}
}

func TestStorageRemoveFileRejectsIdentityReplacementAtMutationBoundary(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	victimPath := filepath.Join(inspection.Root(), "event.json")
	displacedPath := victimPath + ".displaced"
	var injectedErr error
	armed := false
	swapped := false
	hooks := storageTestHooks{beforeMutation: func(step storageStep, _ string) {
		if !armed || swapped || step != storageStepDelete {
			return
		}
		swapped = true
		injectedErr = os.Rename(victimPath, displacedPath)
		if injectedErr == nil {
			injectedErr = os.WriteFile(victimPath, []byte("replacement"), 0o600)
		}
	}}
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.writeFileAtomic("event.json", []byte("original")); err != nil {
		t.Fatal(err)
	}
	armed = true
	removeErr := root.removeFile("event.json")
	victimData, victimErr := os.ReadFile(victimPath)
	displacedData, displacedErr := os.ReadFile(displacedPath)
	if injectedErr != nil || !swapped || !errors.Is(removeErr, errStorageEntryChanged) ||
		victimErr != nil || string(victimData) != "replacement" ||
		displacedErr != nil || string(displacedData) != "original" {
		t.Fatalf("remove identity replacement = swapped:%v injected:%v remove:%v victim:%q victimErr:%v displaced:%q displacedErr:%v",
			swapped, injectedErr, removeErr, victimData, victimErr, displacedData, displacedErr)
	}
}

func TestStorageDirectoryCreationRetryRecoversFailedParentSync(t *testing.T) {
	for _, test := range []struct {
		name       string
		failOnSync int
	}{
		{name: "new directory sync", failOnSync: 1},
		{name: "parent sync", failOnSync: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			syncCalls := 0
			failed := false
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if !armed || step != storageStepDirectorySync {
					return nil
				}
				syncCalls++
				if !failed && syncCalls == test.failOnSync {
					failed = true
					return errors.New("injected " + test.name + " failure")
				}
				return nil
			}}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			armed = true
			if directory, err := root.openDir([]string{"queue"}, true); err == nil {
				_ = directory.Close()
				t.Fatalf("directory creation succeeded despite failed %s", test.name)
			}
			if info, err := os.Stat(filepath.Join(inspection.Root(), "queue")); err != nil || !info.IsDir() {
				t.Fatalf("failed sync should leave a safe visible directory: info=%v err=%v", info, err)
			}
			syncCalls = 0
			directory, err := root.openDir([]string{"queue"}, true)
			if err != nil {
				t.Fatalf("retry directory creation: %v", err)
			}
			defer func() { _ = directory.Close() }()
			if syncCalls < 2 {
				t.Fatalf("retry sync calls = %d, want existing directory and parent resynced", syncCalls)
			}
		})
	}
}

func TestS2bFinalMutableOpenExistingRecoversUncertainDirectoryCreation(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	armed := false
	failed := false
	syncCalls := 0
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if !armed || step != storageStepDirectorySync {
			return nil
		}
		syncCalls++
		if !failed && syncCalls == 2 {
			failed = true
			return errors.New("injected parent sync failure")
		}
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	armed = true
	if directory, err := root.openDir([]string{"queue"}, true); err == nil {
		_ = directory.Close()
		t.Fatal("initial directory creation unexpectedly succeeded")
	}
	if !failed {
		t.Fatal("parent-sync failure seam was not reached")
	}

	syncCalls = 0
	queue, err := root.openDir([]string{"queue"}, false)
	if err != nil {
		t.Fatalf("open existing mutable directory: %v", err)
	}
	defer func() { _ = queue.Close() }()
	if syncCalls < 2 {
		t.Fatalf("mutable existing-directory open reported success after %d syncs, want child and parent recovery", syncCalls)
	}
	if err := queue.writeFileAtomic("event.json", []byte("event")); err != nil {
		t.Fatalf("write after recovered open: %v", err)
	}
	_ = filepath.Join(inspection.Root(), "queue", "event.json")
}

func TestStorageReadOnlyExistingDescendantOpenDoesNotRepair(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	if err := os.Mkdir(filepath.Join(inspection.Root(), "queue"), 0o700); err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepDirectorySync {
			syncCalls++
		}
		return nil
	}}
	root, err := openStorageRoot(inspection, false, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	queue, err := root.openDir([]string{"queue"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	if syncCalls != 0 {
		t.Fatalf("read-only existing descendant open performed %d repair syncs, want zero", syncCalls)
	}
}

func TestStorageRootCreationRetryRecoversMissingIntermediateParentSync(t *testing.T) {
	trustedTempRoot := "/tmp"
	if runtime.GOOS == "darwin" {
		trustedTempRoot = "/private/tmp"
	}
	t.Setenv("GOTMPDIR", trustedTempRoot)
	t.Setenv("TMPDIR", trustedTempRoot)
	privateAncestor := t.TempDir()
	if err := os.Chmod(privateAncestor, 0o700); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(privateAncestor, "created-intermediate")
	homePath := filepath.Join(intermediate, ".gc")
	t.Setenv("GC_HOME", homePath)
	inspection, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.NeedsCreation() {
		t.Fatal("missing intermediate was not reported as needing creation")
	}

	attempt := 1
	targetSyncs := 0
	failed := false
	lastMetadataPath := ""
	hooks := storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			lastMetadataPath = path
			return metadata
		},
		beforeStep: func(step storageStep) error {
			if step != storageStepDirectorySync || lastMetadataPath != intermediate {
				return nil
			}
			targetSyncs++
			if attempt == 1 && targetSyncs == 2 && !failed {
				failed = true
				return errors.New("injected intermediate parent sync failure")
			}
			return nil
		},
	}
	if root, err := openStorageRootMutableWithHooks(inspection, hooks); err == nil {
		_ = root.Close()
		t.Fatal("root creation succeeded despite intermediate parent-sync failure")
	}
	if !failed {
		t.Fatal("intermediate parent-sync failure seam was not reached")
	}
	if info, err := os.Stat(intermediate); err != nil || !info.IsDir() {
		t.Fatalf("failed sync should leave the safe intermediate visible: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(homePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("first failed component creation unexpectedly continued to home: %v", err)
	}

	attempt = 2
	targetSyncs = 0
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatalf("retry root creation: %v", err)
	}
	defer func() { _ = root.Close() }()
	if targetSyncs < 2 {
		t.Fatalf("retry intermediate sync calls = %d, want existing intermediate and parent resynced", targetSyncs)
	}
}

func TestStorageRenameDeleteAndNestedDirectoriesAreDescriptorRelativeAndDurable(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	var steps []storageStep
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		steps = append(steps, step)
		return nil
	}}
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	queue, err := root.openDir([]string{"queue", "generation"}, true)
	if err != nil {
		t.Fatalf("openDir: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	for _, path := range []string{
		filepath.Join(inspection.Root(), "queue"),
		filepath.Join(inspection.Root(), "queue", "generation"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %04o, want 0700", path, got)
		}
	}
	if err := root.writeFileAtomic("event.json", []byte("event")); err != nil {
		t.Fatal(err)
	}
	steps = nil
	result, err := root.renameFile("event.json", queue, "event.json")
	if err != nil {
		t.Fatalf("renameFile: %v", err)
	}
	if result.state != storageRenameAppliedDurable {
		t.Fatalf("rename state = %v, want applied-durable", result.state)
	}
	if countStep(steps, storageStepRename) != 1 || countStep(steps, storageStepDirectorySync) != 2 {
		t.Fatalf("rename durability steps = %v, want rename and both directory syncs", steps)
	}
	if _, err := root.readFile("event.json", 1024); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source after rename error = %v, want not-exist", err)
	}
	if got, err := queue.readFile("event.json", 1024); err != nil || string(got) != "event" {
		t.Fatalf("destination bytes=%q err=%v", got, err)
	}
	steps = nil
	if err := queue.removeFile("event.json"); err != nil {
		t.Fatalf("removeFile: %v", err)
	}
	if fmt.Sprint(steps) != fmt.Sprint([]storageStep{storageStepDelete, storageStepDirectorySync}) {
		t.Fatalf("delete durability steps = %v", steps)
	}
	if err := queue.removeFile("event.json"); err != nil {
		t.Fatalf("idempotent removeFile: %v", err)
	}
}

type oneWayRenameSyncFixture struct {
	source     *storageDir
	target     *storageDir
	run        func() (storageRenameResult, error)
	sourcePath string
	targetPath string
	wantTarget string
}

func setStorageDirectoryStepHook(t *testing.T, directory *storageDir, hook func(storageStep) error) {
	t.Helper()
	if directory == nil || directory.backend == nil {
		t.Fatal("cannot install a step hook on a closed storage directory")
	}
	backend, ok := directory.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatalf("storage directory backend = %T, want *unixStorageDirectory", directory.backend)
	}
	backend.mu.Lock()
	backend.hooks.beforeStep = hook
	backend.mu.Unlock()
}

func TestStorageCrossDirectoryOneWayRenamesSyncDestinationBeforeSource(t *testing.T) {
	openRoot := func(t *testing.T) (*storageRoot, gchome.ProductUsageHome) {
		t.Helper()
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		return root, inspection
	}
	openChild := func(t *testing.T, root *storageRoot, name string) *storageDir {
		t.Helper()
		directory, err := root.openDir([]string{name}, true)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = directory.Close() })
		return directory
	}
	ordinaryFixture := func(t *testing.T, sourceName, targetName string) oneWayRenameSyncFixture {
		t.Helper()
		root, inspection := openRoot(t)
		source := openChild(t, root, sourceName)
		target := openChild(t, root, targetName)
		if err := source.writeFileAtomic("event.json", []byte("event")); err != nil {
			t.Fatal(err)
		}
		return oneWayRenameSyncFixture{
			source: source,
			target: target,
			run: func() (storageRenameResult, error) {
				return source.renameFile("event.json", target, "event.json")
			},
			sourcePath: filepath.Join(inspection.Root(), sourceName, "event.json"),
			targetPath: filepath.Join(inspection.Root(), targetName, "event.json"),
			wantTarget: "event",
		}
	}
	operations := []struct {
		name  string
		setup func(*testing.T) oneWayRenameSyncFixture
	}{
		{
			name: "claim queue to inflight",
			setup: func(t *testing.T) oneWayRenameSyncFixture {
				return ordinaryFixture(t, queueDirectoryName, inflightDirectoryName)
			},
		},
		{
			name: "restore inflight to queue",
			setup: func(t *testing.T) oneWayRenameSyncFixture {
				return ordinaryFixture(t, inflightDirectoryName, queueDirectoryName)
			},
		},
		{
			name: "cross-parent replacement",
			setup: func(t *testing.T) oneWayRenameSyncFixture {
				root, inspection := openRoot(t)
				control := openChild(t, root, "control")
				if err := control.writeFileAtomic("staged", []byte("new-quota")); err != nil {
					t.Fatal(err)
				}
				if err := root.writeFileAtomic("quota", []byte("old-quota")); err != nil {
					t.Fatal(err)
				}
				return oneWayRenameSyncFixture{
					source: control,
					target: root.storageDir,
					run: func() (storageRenameResult, error) {
						return control.replaceFile("staged", root.storageDir, "quota")
					},
					sourcePath: filepath.Join(inspection.Root(), "control", "staged"),
					targetPath: filepath.Join(inspection.Root(), "quota"),
					wantTarget: "new-quota",
				}
			},
		},
		{
			name: "enumerated entry",
			setup: func(t *testing.T) oneWayRenameSyncFixture {
				root, inspection := openRoot(t)
				target := openChild(t, root, "target")
				if err := root.writeFileAtomic("source", []byte("enumerated")); err != nil {
					t.Fatal(err)
				}
				entry, err := root.lookupEntry("source")
				if err != nil {
					t.Fatal(err)
				}
				return oneWayRenameSyncFixture{
					source: root.storageDir,
					target: target,
					run: func() (storageRenameResult, error) {
						return root.renameEnumeratedEntry(entry, target, "parked")
					},
					sourcePath: filepath.Join(inspection.Root(), "source"),
					targetPath: filepath.Join(inspection.Root(), "target", "parked"),
					wantTarget: "enumerated",
				}
			},
		},
	}
	cuts := []struct {
		name            string
		failDestination bool
		failSource      bool
		wantOrder       string
	}{
		{name: "durable", wantOrder: "destination,source"},
		{name: "destination sync crash", failDestination: true, wantOrder: "destination"},
		{name: "source sync crash", failSource: true, wantOrder: "destination,source"},
	}
	for _, operation := range operations {
		for _, cut := range cuts {
			t.Run(operation.name+"/"+cut.name, func(t *testing.T) {
				fixture := operation.setup(t)
				destinationFailure := errors.New("injected destination parent sync failure")
				sourceFailure := errors.New("injected source parent sync failure")
				var order []string
				setStorageDirectoryStepHook(t, fixture.target, func(step storageStep) error {
					if step != storageStepDirectorySync {
						return nil
					}
					order = append(order, "destination")
					if cut.failDestination {
						return destinationFailure
					}
					return nil
				})
				setStorageDirectoryStepHook(t, fixture.source, func(step storageStep) error {
					if step != storageStepDirectorySync {
						return nil
					}
					order = append(order, "source")
					if cut.failSource {
						return sourceFailure
					}
					return nil
				})

				result, renameErr := fixture.run()
				wantState := storageRenameAppliedDurable
				var wantErr error
				switch {
				case cut.failDestination:
					wantState = storageRenameAppliedSyncPending
					wantErr = destinationFailure
				case cut.failSource:
					wantState = storageRenameAppliedSyncPending
					wantErr = sourceFailure
				}
				targetData, targetErr := os.ReadFile(fixture.targetPath)
				_, sourceErr := os.Lstat(fixture.sourcePath)
				if result.state != wantState || (wantErr == nil && renameErr != nil) ||
					(wantErr != nil && !errors.Is(renameErr, wantErr)) || strings.Join(order, ",") != cut.wantOrder ||
					targetErr != nil || string(targetData) != fixture.wantTarget || !errors.Is(sourceErr, fs.ErrNotExist) {
					t.Fatalf("one-way rename = state:%v err:%v order:%v target:%q/%v source:%v, want state:%v err:%v order:%s target:%q",
						result.state, renameErr, order, targetData, targetErr, sourceErr,
						wantState, wantErr, cut.wantOrder, fixture.wantTarget)
				}
			})
		}
	}
}

func TestStorageSameDirectoryOneWayRenamesSyncOnce(t *testing.T) {
	operations := []struct {
		name  string
		setup func(*testing.T, *storageRoot) (func() (storageRenameResult, error), string, string)
	}{
		{
			name: "ordinary rename",
			setup: func(t *testing.T, root *storageRoot) (func() (storageRenameResult, error), string, string) {
				if err := root.writeFileAtomic("source", []byte("ordinary")); err != nil {
					t.Fatal(err)
				}
				return func() (storageRenameResult, error) {
					return root.renameFile("source", root.storageDir, "target")
				}, "source", "target"
			},
		},
		{
			name: "replacement",
			setup: func(t *testing.T, root *storageRoot) (func() (storageRenameResult, error), string, string) {
				if err := root.writeFileAtomic("source", []byte("replacement")); err != nil {
					t.Fatal(err)
				}
				if err := root.writeFileAtomic("target", []byte("old")); err != nil {
					t.Fatal(err)
				}
				return func() (storageRenameResult, error) {
					return root.replaceFile("source", root.storageDir, "target")
				}, "source", "target"
			},
		},
		{
			name: "enumerated entry",
			setup: func(t *testing.T, root *storageRoot) (func() (storageRenameResult, error), string, string) {
				if err := root.writeFileAtomic("source", []byte("enumerated")); err != nil {
					t.Fatal(err)
				}
				entry, err := root.lookupEntry("source")
				if err != nil {
					t.Fatal(err)
				}
				return func() (storageRenameResult, error) {
					return root.renameEnumeratedEntry(entry, root.storageDir, "target")
				}, "source", "target"
			},
		},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			root, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			run, sourceName, targetName := operation.setup(t, root)
			syncs := 0
			setStorageDirectoryStepHook(t, root.storageDir, func(step storageStep) error {
				if step == storageStepDirectorySync {
					syncs++
				}
				return nil
			})
			result, renameErr := run()
			data, targetErr := os.ReadFile(filepath.Join(inspection.Root(), targetName))
			_, sourceErr := os.Lstat(filepath.Join(inspection.Root(), sourceName))
			if renameErr != nil || result.state != storageRenameAppliedDurable || syncs != 1 ||
				targetErr != nil || len(data) == 0 || !errors.Is(sourceErr, fs.ErrNotExist) {
				t.Fatalf("same-directory rename = state:%v err:%v syncs:%d target:%q/%v source:%v",
					result.state, renameErr, syncs, data, targetErr, sourceErr)
			}
		})
	}
}

func TestStorageRenameOutcomesAreTypedAndRecoverable(t *testing.T) {
	t.Run("rename syscall failure is not applied", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		armed := false
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && step == storageStepRename {
				return errors.New("injected rename failure")
			}
			return nil
		}}
		root, target := openRenameTestDirectories(t, inspection, hooks)
		if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
			t.Fatal(err)
		}
		armed = true
		result, err := root.renameFile("event.json", target, "event.json")
		if err == nil || result.state != storageRenameNotApplied {
			t.Fatalf("rename result = (%v, %v), want not-applied failure", result.state, err)
		}
		if got, err := root.readFile("event.json", 1024); err != nil || string(got) != "source" {
			t.Fatalf("not-applied source = %q, %v", got, err)
		}
		if _, err := target.readFile("event.json", 1024); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("not-applied destination error = %v, want not-exist", err)
		}
	})

	for _, test := range []struct {
		name       string
		failOnSync int
	}{
		{name: "target sync failure", failOnSync: 1},
		{name: "source sync failure", failOnSync: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			syncCalls := 0
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if armed && step == storageStepDirectorySync {
					syncCalls++
					if syncCalls == test.failOnSync {
						return errors.New("injected " + test.name)
					}
				}
				return nil
			}}
			root, target := openRenameTestDirectories(t, inspection, hooks)
			if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
				t.Fatal(err)
			}
			armed = true
			result, err := root.renameFile("event.json", target, "event.json")
			if err == nil || result.state != storageRenameAppliedSyncPending {
				t.Fatalf("rename result = (%v, %v), want applied-sync-pending failure", result.state, err)
			}
			if _, err := root.readFile("event.json", 1024); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("applied source error = %v, want not-exist", err)
			}
			if got, err := target.readFile("event.json", 1024); err != nil || string(got) != "source" {
				t.Fatalf("visible applied destination = %q, %v", got, err)
			}
			if err := root.syncDirectory(); err != nil {
				t.Fatalf("resync source directory: %v", err)
			}
			if err := target.syncDirectory(); err != nil {
				t.Fatalf("resync target directory: %v", err)
			}
		})
	}

	t.Run("same-directory sync failure", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		armed := false
		failed := false
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && !failed && step == storageStepDirectorySync {
				failed = true
				return errors.New("injected same-directory sync failure")
			}
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := root.writeFileAtomic("source.json", []byte("source")); err != nil {
			t.Fatal(err)
		}
		armed = true
		result, err := root.renameFile("source.json", root.storageDir, "target.json")
		if err == nil || result.state != storageRenameAppliedSyncPending {
			t.Fatalf("same-directory result = (%v, %v), want applied-sync-pending", result.state, err)
		}
		if got, err := root.readFile("target.json", 1024); err != nil || string(got) != "source" {
			t.Fatalf("same-directory visible destination = %q, %v", got, err)
		}
		if err := root.syncDirectory(); err != nil {
			t.Fatalf("same-directory explicit resync: %v", err)
		}
	})

	t.Run("destination conflict never overwrites", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, target := openRenameTestDirectories(t, inspection, storageTestHooks{})
		if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
			t.Fatal(err)
		}
		if err := target.writeFileAtomic("event.json", []byte("destination")); err != nil {
			t.Fatal(err)
		}
		result, err := root.renameFile("event.json", target, "event.json")
		if !errors.Is(err, errStorageDestinationExists) || result.state != storageRenameNotApplied {
			t.Fatalf("conflict result = (%v, %v), want typed not-applied conflict", result.state, err)
		}
		if got, err := root.readFile("event.json", 1024); err != nil || string(got) != "source" {
			t.Fatalf("conflict source = %q, %v", got, err)
		}
		if got, err := target.readFile("event.json", 1024); err != nil || string(got) != "destination" {
			t.Fatalf("conflict destination = %q, %v", got, err)
		}
	})

	t.Run("directory sync retries interrupted syscall", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		armed := false
		calls := 0
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && step == storageStepDirectorySync {
				calls++
				if calls == 1 {
					return unix.EINTR
				}
			}
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		armed = true
		if err := root.syncDirectory(); err != nil {
			t.Fatalf("syncDirectory: %v", err)
		}
		if calls != 2 {
			t.Fatalf("directory sync hook calls = %d, want EINTR retry", calls)
		}
	})
}

func TestStorageNoReplaceRenameUnsupportedErrorsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "syscall unavailable", err: unix.ENOSYS},
		{name: "filesystem unsupported", err: unix.EOPNOTSUPP},
		{name: "flag unsupported", err: unix.EINVAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if armed && step == storageStepRename {
					return test.err
				}
				return nil
			}}
			root, target := openRenameTestDirectories(t, inspection, hooks)
			if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
				t.Fatal(err)
			}
			armed = true
			result, err := root.renameFile("event.json", target, "event.json")
			if err == nil || result.state != storageRenameNotApplied {
				t.Fatalf("unsupported no-replace rename = (%v, %v), want not-applied failure", result.state, err)
			}
			if got, err := root.readFile("event.json", 1024); err != nil || string(got) != "source" {
				t.Fatalf("unsupported rename changed source to %q: %v", got, err)
			}
			if _, err := target.readFile("event.json", 1024); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("unsupported rename created destination: %v", err)
			}
		})
	}
}

func openRenameTestDirectories(t *testing.T, inspection gchome.ProductUsageHome, hooks storageTestHooks) (*storageRoot, *storageDir) {
	t.Helper()
	root, err := openStorageRootMutableWithHooks(inspection, hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	target, err := root.openDir([]string{"inflight"}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	return root, target
}

func TestS2bRedTeamRenameCollisionIsAtomicNoReplace(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	armed := false
	var collisionErr error
	targetPath := filepath.Join(inspection.Root(), "inflight", "event.json")
	hooks := storageTestHooks{beforeStep: func(step storageStep) error {
		if armed && step == storageStepRename {
			armed = false
			collisionErr = os.WriteFile(targetPath, []byte("racer"), 0o600)
		}
		return nil
	}}
	root, target := openRenameTestDirectories(t, inspection, hooks)
	if err := root.writeFileAtomic("event.json", []byte("source")); err != nil {
		t.Fatal(err)
	}
	armed = true
	result, err := root.renameFile("event.json", target, "event.json")
	if collisionErr != nil {
		t.Fatalf("create racing destination: %v", collisionErr)
	}
	if !errors.Is(err, errStorageDestinationExists) || result.state != storageRenameNotApplied {
		got, readErr := target.readFile("event.json", 1024)
		t.Fatalf("racing destination was not preserved: result=(%v, %v), destination=%q readErr=%v", result.state, err, got, readErr)
	}
}

func TestS2bRedTeamEnumeratedCleanupCannotSplitStableLockInode(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	firstRoot, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstRoot.Close() }()
	secondRoot, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondRoot.Close() }()
	firstLock, err := firstRoot.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstLock.Release() }()

	entry := enumerateAllStorageEntries(t, firstRoot.storageDir)["state.lock"]
	if err := firstRoot.unlinkEnumeratedEntry(entry); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	secondLock, err := secondRoot.acquireLock(ctx, "state.lock")
	if err == nil {
		_ = secondLock.Release()
		t.Fatal("enumerated cleanup unlinked the held stable lock and allowed a second simultaneous lock owner")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire after enumerated lock unlink = %v, want contention", err)
	}
}

func TestStorageEnumeratedCleanupProtectsBothHeldRootLocks(t *testing.T) {
	for _, name := range []string{"state.lock", "uploader.lock"} {
		t.Run(name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			firstRoot, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = firstRoot.Close() }()
			secondRoot, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = secondRoot.Close() }()
			firstLock, err := firstRoot.acquireLock(context.Background(), name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = firstLock.Release() }()

			lockPath := filepath.Join(inspection.Root(), name)
			before, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			entry := enumerateAllStorageEntries(t, firstRoot.storageDir)[name]
			if err := firstRoot.unlinkEnumeratedEntry(entry); err == nil {
				t.Fatal("enumerated cleanup unlinked a stable root lock")
			}
			after, err := os.Stat(lockPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("protected lock inode changed: before=%v after=%v err=%v", before, after, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			defer cancel()
			if secondLock, err := secondRoot.acquireLock(ctx, name); err == nil {
				_ = secondLock.Release()
				t.Fatal("second root acquired a protected held lock")
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("second acquire = %v, want deadline exceeded", err)
			}
		})
	}
}

func TestStorageEnumeratedDirectoryCleanupProtectsRootLockNames(t *testing.T) {
	for _, name := range []string{"state.lock", "uploader.lock"} {
		t.Run(name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			path := filepath.Join(inspection.Root(), name)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			root, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			entry := enumerateAllStorageEntries(t, root.storageDir)[name]
			if err := root.removeEnumeratedDirectory(entry); err == nil {
				t.Fatal("enumerated directory cleanup removed a root stable-lock name")
			}
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				t.Fatalf("protected root lock-name directory changed: info=%v err=%v", info, err)
			}
		})
	}
}

func TestStorageRootLockProtectionSurvivesEmptyComponentAlias(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := root.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	rootAlias, err := root.openDir(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rootAlias.Close() }()
	entry := enumerateAllStorageEntries(t, rootAlias)["state.lock"]
	if err := rootAlias.unlinkEnumeratedEntry(entry); err == nil {
		t.Fatal("empty-component root alias bypassed stable-lock protection")
	}
}

func TestStorageNestedLockNamedPoisonRemainsCleanable(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	nested, err := root.openDir([]string{"queue"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nested.Close() }()
	if _, err := nested.acquireLock(context.Background(), "state.lock"); err == nil {
		t.Fatal("nested directory acquired a root-global stable lock")
	}

	for _, name := range []string{"state.lock", "uploader.lock"} {
		path := filepath.Join(inspection.Root(), "queue", name)
		if err := os.WriteFile(path, []byte("poison"), 0o600); err != nil {
			t.Fatal(err)
		}
		entry := enumerateAllStorageEntries(t, nested)[name]
		if err := nested.unlinkEnumeratedEntry(entry); err != nil {
			t.Fatalf("unlink nested %s poison: %v", name, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("nested %s file remains: %v", name, err)
		}

		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		entry = enumerateAllStorageEntries(t, nested)[name]
		if err := nested.removeEnumeratedDirectory(entry); err != nil {
			t.Fatalf("remove nested %s directory poison: %v", name, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("nested %s directory remains: %v", name, err)
		}
	}
}

func TestStorageIteratorYieldsBoundedNoFollowMetadata(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.writeFileAtomic("regular", []byte("data")); err != nil {
		t.Fatal(err)
	}
	longName := strings.Repeat("n", 129)
	sparsePath := filepath.Join(inspection.Root(), longName)
	sparse, err := os.OpenFile(sparsePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(1 << 30); err != nil {
		_ = sparse.Close()
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(inspection.Root(), "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(inspection.Root(), "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inspection.Root(), "linked"), []byte("linked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(inspection.Root(), "linked"), filepath.Join(inspection.Root(), "linked-again")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(inspection.Root(), "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inspection.Root(), "bad name"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	entries := enumerateAllStorageEntries(t, root.storageDir)
	if len(entries) != 9 {
		t.Fatalf("enumerated %d entries, want 9: %v", len(entries), entries)
	}
	if got := entries[rootTempJournalDirectoryName]; got.metadata.kind != storageEntryDirectory || !got.metadata.ownerOnly {
		t.Fatalf("persistent root-temp journal metadata = %#v", got)
	}
	if got := entries[longName]; got.nameBytes != 129 || got.metadata.size != 1<<30 || got.metadata.mode&unix.S_IFMT != unix.S_IFREG {
		t.Fatalf("sparse overlong entry = %#v", got)
	}
	if got := entries["symlink"]; got.metadata.mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("symlink metadata followed target: %#v", got)
	}
	if got := entries["fifo"]; got.metadata.mode&unix.S_IFMT != unix.S_IFIFO {
		t.Fatalf("FIFO metadata = %#v", got)
	}
	if got := entries["linked"]; got.metadata.nlink != 2 {
		t.Fatalf("hard-link metadata = %#v", got)
	}
	if got := entries["empty-dir"]; got.metadata.mode&unix.S_IFMT != unix.S_IFDIR {
		t.Fatalf("directory metadata = %#v", got)
	}
	if got := entries["regular"]; got.metadata.uid != uint32(os.Geteuid()) || got.metadata.mtimeSeconds == 0 || got.metadata.dev == 0 || got.metadata.ino == 0 {
		t.Fatalf("regular metadata incomplete: %#v", got)
	}
}

func TestStorageIteratorCloseAndFailureSeams(t *testing.T) {
	t.Run("fresh cursor", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.WriteFile(filepath.Join(inspection.Root(), "event"), []byte("event"), 0o600); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			iterator, err := root.iterateEntries()
			if err != nil {
				t.Fatal(err)
			}
			entry, nextErr := iterator.Next()
			if closeErr := iterator.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if nextErr != nil || entry.name != "event" {
				t.Fatalf("fresh iterator = (%#v, %v), want event", entry, nextErr)
			}
		}
	})

	t.Run("close", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		iterator, err := root.iterateEntries()
		if err != nil {
			t.Fatal(err)
		}
		if err := iterator.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := iterator.Next(); !errors.Is(err, errStorageClosed) {
			t.Fatalf("Next after Close = %v, want closed", err)
		}
		if err := iterator.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})

	t.Run("concurrent Next and Close", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.WriteFile(filepath.Join(inspection.Root(), "event"), []byte("event"), 0o600); err != nil {
			t.Fatal(err)
		}

		for range 64 {
			iterator, err := root.iterateEntries()
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var entry storageEntry
			var nextErr, closeErr error
			var wait sync.WaitGroup
			wait.Add(2)
			go func() {
				defer wait.Done()
				<-start
				entry, nextErr = iterator.Next()
			}()
			go func() {
				defer wait.Done()
				<-start
				closeErr = iterator.Close()
			}()
			close(start)
			wait.Wait()
			if closeErr != nil {
				t.Fatalf("concurrent Close: %v", closeErr)
			}
			if nextErr == nil {
				if entry.name != "event" {
					t.Fatalf("concurrent Next returned %#v, want event", entry)
				}
			} else if !errors.Is(nextErr, errStorageClosed) {
				t.Fatalf("concurrent Next = %v, want event or typed closed", nextErr)
			}
			if _, err := iterator.Next(); !errors.Is(err, errStorageClosed) {
				t.Fatalf("Next after concurrent Close = %v, want typed closed", err)
			}
		}
	})

	for _, test := range []struct {
		name string
		step storageStep
	}{
		{name: "enumerate", step: storageStepEnumerate},
		{name: "entry stat", step: storageStepEntryStat},
	} {
		t.Run(test.name+" failure", func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if armed && step == test.step {
					return errors.New("injected " + test.name + " failure")
				}
				return nil
			}}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if err := os.WriteFile(filepath.Join(inspection.Root(), "event"), []byte("event"), 0o600); err != nil {
				t.Fatal(err)
			}
			iterator, err := root.iterateEntries()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = iterator.Close() }()
			armed = true
			if _, err := iterator.Next(); err == nil {
				t.Fatalf("iterator ignored injected %s failure", test.name)
			}
			armed = false
			entry, err := iterator.Next()
			if err != nil || entry.name != "event" {
				t.Fatalf("iterator skipped entry after %s failure: entry=%#v err=%v", test.name, entry, err)
			}
		})
	}

	t.Run("directory mode drift during iteration", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.WriteFile(filepath.Join(inspection.Root(), "event"), []byte("event"), 0o600); err != nil {
			t.Fatal(err)
		}
		iterator, err := root.iterateEntries()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = iterator.Close() }()
		if err := os.Chmod(inspection.Root(), 0o770); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(inspection.Root(), 0o700) })
		if _, err := iterator.Next(); err == nil {
			t.Fatal("iterator continued after retained-directory mode drift")
		}
	})

	t.Run("component swap does not redirect iteration", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.WriteFile(filepath.Join(inspection.Root(), "retained"), []byte("event"), 0o600); err != nil {
			t.Fatal(err)
		}
		iterator, err := root.iterateEntries()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = iterator.Close() }()
		displaced := inspection.Root() + "-displaced"
		attacker := filepath.Join(t.TempDir(), "attacker")
		if err := os.Mkdir(attacker, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(attacker, "redirected"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(inspection.Root(), displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, inspection.Root()); err != nil {
			t.Fatal(err)
		}
		entry, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if entry.name != "retained" {
			t.Fatalf("iterator redirected to %q, want retained directory entry", entry.name)
		}
	})
}

func TestStorageEnumeratedEntryRenameMovesExactAnyKindAndSyncsParents(t *testing.T) {
	for _, shape := range []struct {
		name  string
		setup func(*testing.T, string, string)
		check func(*testing.T, string, string)
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, source, _ string) {
				if err := os.WriteFile(source, []byte("file-payload"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, target, _ string) {
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "file-payload" {
					t.Fatalf("moved file = %q, %v", data, err)
				}
			},
		},
		{
			name: "nonempty directory",
			setup: func(t *testing.T, source, _ string) {
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "payload"), []byte("directory-payload"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, target, _ string) {
				data, err := os.ReadFile(filepath.Join(target, "payload"))
				if err != nil || string(data) != "directory-payload" {
					t.Fatalf("moved directory = %q, %v", data, err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, source, sentinel string) {
				if err := os.Symlink(sentinel, source); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, target, sentinel string) {
				link, err := os.Readlink(target)
				data, readErr := os.ReadFile(sentinel)
				if err != nil || link != sentinel || readErr != nil || string(data) != "outside" {
					t.Fatalf("moved symlink = %q err:%v sentinel=%q readErr:%v", link, err, data, readErr)
				}
			},
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			syncs := 0
			armed := false
			root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{beforeStep: func(step storageStep) error {
				if armed && step == storageStepDirectorySync {
					syncs++
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			targetDirectory, err := root.openDir([]string{"target"}, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = targetDirectory.Close() }()
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			sourcePath := filepath.Join(inspection.Root(), "source")
			targetPath := filepath.Join(inspection.Root(), "target", "parked")
			shape.setup(t, sourcePath, sentinel)
			source, err := root.lookupEntry("source")
			if err != nil {
				t.Fatal(err)
			}
			armed = true
			result, renameErr := root.renameEnumeratedEntry(source, targetDirectory, "parked")
			if renameErr != nil || result.state != storageRenameAppliedDurable || syncs != 2 {
				t.Fatalf("enumerated %s rename = state:%v syncs:%d err:%v", shape.name, result.state, syncs, renameErr)
			}
			if _, err := os.Lstat(sourcePath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("enumerated %s source remains: %v", shape.name, err)
			}
			parked, err := targetDirectory.lookupEntry("parked")
			if err != nil || parked.metadata.dev != source.metadata.dev || parked.metadata.ino != source.metadata.ino ||
				parked.metadata.kind != source.metadata.kind {
				t.Fatalf("enumerated %s target = %+v source=%+v err:%v", shape.name, parked, source, err)
			}
			shape.check(t, targetPath, sentinel)
		})
	}
}

func TestStorageEnumeratedEntryRenameCollisionPreservesBothEntries(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	target, err := root.openDir([]string{"target"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	sourcePath := filepath.Join(inspection.Root(), "source")
	targetPath := filepath.Join(inspection.Root(), "target", "occupied")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := root.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	var sourceBefore, targetBefore unix.Stat_t
	if err := unix.Lstat(sourcePath, &sourceBefore); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(targetPath, &targetBefore); err != nil {
		t.Fatal(err)
	}
	result, renameErr := root.renameEnumeratedEntry(source, target, "occupied")
	var sourceAfter, targetAfter unix.Stat_t
	if err := unix.Lstat(sourcePath, &sourceAfter); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(targetPath, &targetAfter); err != nil {
		t.Fatal(err)
	}
	if result.state != storageRenameNotApplied || !errors.Is(renameErr, errStorageDestinationExists) ||
		sourceBefore.Dev != sourceAfter.Dev || sourceBefore.Ino != sourceAfter.Ino ||
		targetBefore.Dev != targetAfter.Dev || targetBefore.Ino != targetAfter.Ino {
		t.Fatalf("enumerated collision = state:%v err:%v source:%+v/%+v target:%+v/%+v",
			result.state, renameErr, sourceBefore, sourceAfter, targetBefore, targetAfter)
	}
}

func TestStorageEnumeratedEntryRenameRejectsSourceReplacementAtMutationBoundary(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	armed := false
	replaced := false
	sourcePath := filepath.Join(inspection.Root(), "source")
	displacedPath := filepath.Join(inspection.Root(), "enumerated-source")
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{beforeMutation: func(step storageStep, name string) {
		if !armed || replaced || step != storageStepRename || name != "source" {
			return
		}
		replaced = true
		if err := os.Rename(sourcePath, displacedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourcePath, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	target, err := root.openDir([]string{"target"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	if err := os.WriteFile(sourcePath, []byte("enumerated"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := root.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	armed = true
	result, renameErr := root.renameEnumeratedEntry(source, target, "parked")
	replacement, replacementErr := os.ReadFile(sourcePath)
	displaced, displacedErr := os.ReadFile(displacedPath)
	_, targetErr := os.Lstat(filepath.Join(inspection.Root(), "target", "parked"))
	if !replaced || result.state != storageRenameNotApplied || !errors.Is(renameErr, errStorageEntryChanged) ||
		replacementErr != nil || string(replacement) != "replacement" || displacedErr != nil || string(displaced) != "enumerated" ||
		!errors.Is(targetErr, fs.ErrNotExist) {
		t.Fatalf("enumerated replacement = replaced:%v state:%v err:%v replacement:%q/%v displaced:%q/%v target:%v",
			replaced, result.state, renameErr, replacement, replacementErr, displaced, displacedErr, targetErr)
	}
}

func TestStorageEnumeratedEntryRenameSyncFailureIsAppliedPending(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	armed := false
	failed := false
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{beforeStep: func(step storageStep) error {
		if armed && !failed && step == storageStepDirectorySync {
			failed = true
			return errors.New("injected enumerated rename parent sync failure")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	target, err := root.openDir([]string{"target"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	sourcePath := filepath.Join(inspection.Root(), "source")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := root.lookupEntry("source")
	if err != nil {
		t.Fatal(err)
	}
	armed = true
	result, renameErr := root.renameEnumeratedEntry(source, target, "parked")
	parked, parkedErr := target.lookupEntry("parked")
	_, sourceErr := os.Lstat(sourcePath)
	if !failed || renameErr == nil || result.state != storageRenameAppliedSyncPending ||
		parkedErr != nil || parked.metadata.dev != source.metadata.dev || parked.metadata.ino != source.metadata.ino ||
		!errors.Is(sourceErr, fs.ErrNotExist) {
		t.Fatalf("enumerated sync failure = failed:%v state:%v err:%v parked:%+v/%v source:%v",
			failed, result.state, renameErr, parked, parkedErr, sourceErr)
	}
}

func TestStorageEnumeratedEntryRenameProtectsStableRootLockNames(t *testing.T) {
	for _, lockName := range []string{stateLockName, "uploader.lock"} {
		t.Run(lockName+" source", func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			root, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			target, err := root.openDir([]string{"target"}, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = target.Close() }()
			lockPath := filepath.Join(inspection.Root(), lockName)
			if err := os.WriteFile(lockPath, []byte("lock"), 0o600); err != nil {
				t.Fatal(err)
			}
			entry, err := root.lookupEntry(lockName)
			if err != nil {
				t.Fatal(err)
			}
			result, renameErr := root.renameEnumeratedEntry(entry, target, "parked")
			data, readErr := os.ReadFile(lockPath)
			if renameErr == nil || result.state != storageRenameNotApplied || readErr != nil || string(data) != "lock" {
				t.Fatalf("stable source lock rename = state:%v err:%v data:%q readErr:%v", result.state, renameErr, data, readErr)
			}
		})
		t.Run(lockName+" target", func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			root, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			sourcePath := filepath.Join(inspection.Root(), "source")
			if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			entry, err := root.lookupEntry("source")
			if err != nil {
				t.Fatal(err)
			}
			result, renameErr := root.renameEnumeratedEntry(entry, root.storageDir, lockName)
			data, readErr := os.ReadFile(sourcePath)
			_, targetErr := os.Lstat(filepath.Join(inspection.Root(), lockName))
			if renameErr == nil || result.state != storageRenameNotApplied || readErr != nil || string(data) != "source" ||
				!errors.Is(targetErr, fs.ErrNotExist) {
				t.Fatalf("stable target lock rename = state:%v err:%v data:%q readErr:%v target:%v", result.state, renameErr, data, readErr, targetErr)
			}
		})
	}
}

func TestStorageEnumeratedUnlinkRejectsReplacementAtMutationBoundary(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	victimPath := filepath.Join(inspection.Root(), "victim")
	displacedPath := victimPath + "-enumerated"
	replaced := false
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{beforeMutation: func(step storageStep, path string) {
		if replaced || step != storageStepUnlink || path != victimPath {
			return
		}
		replaced = true
		if renameErr := os.Rename(victimPath, displacedPath); renameErr != nil {
			t.Fatal(renameErr)
		}
		if writeErr := os.WriteFile(victimPath, []byte("replacement"), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.WriteFile(victimPath, []byte("enumerated"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := root.lookupEntry("victim")
	if err != nil {
		t.Fatal(err)
	}
	unlinkErr := root.unlinkEnumeratedEntry(entry)
	replacement, replacementErr := os.ReadFile(victimPath)
	displaced, displacedErr := os.ReadFile(displacedPath)
	if !replaced || !errors.Is(unlinkErr, errStorageEntryChanged) || replacementErr != nil || string(replacement) != "replacement" ||
		displacedErr != nil || string(displaced) != "enumerated" {
		t.Fatalf("enumerated unlink replacement = replaced:%v err:%v replacement:%q/%v displaced:%q/%v",
			replaced, unlinkErr, replacement, replacementErr, displaced, displacedErr)
	}
}

func TestStorageEnumeratedRmdirRejectsReplacementAtMutationBoundary(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	victimPath := filepath.Join(inspection.Root(), "victim")
	displacedPath := victimPath + "-enumerated"
	replaced := false
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{beforeMutation: func(step storageStep, path string) {
		if replaced || step != storageStepRmdir || path != victimPath {
			return
		}
		replaced = true
		if renameErr := os.Rename(victimPath, displacedPath); renameErr != nil {
			t.Fatal(renameErr)
		}
		if mkdirErr := os.Mkdir(victimPath, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Mkdir(victimPath, 0o700); err != nil {
		t.Fatal(err)
	}
	entry, err := root.lookupEntry("victim")
	if err != nil {
		t.Fatal(err)
	}
	removeErr := root.removeEnumeratedCleanupDirectory(entry)
	replacement, replacementErr := os.Stat(victimPath)
	displaced, displacedErr := os.Stat(displacedPath)
	if !replaced || !errors.Is(removeErr, errStorageEntryChanged) || replacementErr != nil || !replacement.IsDir() ||
		displacedErr != nil || !displaced.IsDir() {
		t.Fatalf("enumerated rmdir replacement = replaced:%v err:%v replacement:%v/%v displaced:%v/%v",
			replaced, removeErr, replacement, replacementErr, displaced, displacedErr)
	}
}

func TestStorageEnumeratedExchangeProtectsBothStableRootLockEndpoints(t *testing.T) {
	for _, lockName := range []string{stateLockName, "uploader.lock"} {
		for _, lockEndpoint := range []string{"source", "target"} {
			t.Run(lockName+" "+lockEndpoint, func(t *testing.T) {
				inspection := inspectStorageTestHome(t, true)
				root, err := openStorageRootMutable(inspection)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				lockPath := filepath.Join(inspection.Root(), lockName)
				otherPath := filepath.Join(inspection.Root(), "other")
				if err := os.WriteFile(lockPath, []byte("stable-lock"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(otherPath, []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
				lockEntry, err := root.lookupEntry(lockName)
				if err != nil {
					t.Fatal(err)
				}
				otherEntry, err := root.lookupEntry("other")
				if err != nil {
					t.Fatal(err)
				}
				source, target := lockEntry, otherEntry
				if lockEndpoint == "target" {
					source, target = otherEntry, lockEntry
				}
				result, exchangeErr := root.exchangeEnumeratedEntries(source, root.storageDir, target)
				lockData, lockErr := os.ReadFile(lockPath)
				otherData, otherErr := os.ReadFile(otherPath)
				if exchangeErr == nil || result.state != storageRenameNotApplied || lockErr != nil || string(lockData) != "stable-lock" ||
					otherErr != nil || string(otherData) != "other" {
					t.Fatalf("stable-lock exchange = state:%v err:%v lock:%q/%v other:%q/%v",
						result.state, exchangeErr, lockData, lockErr, otherData, otherErr)
				}
			})
		}
	}
}

func TestStorageIdentityCheckedUnsafeCleanup(t *testing.T) {
	t.Run("symlink FIFO sparse and hard-link poison", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(inspection.Root(), "symlink")); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(inspection.Root(), "fifo"), 0o600); err != nil {
			t.Fatal(err)
		}
		sparseName := strings.Repeat("s", 129)
		sparse, err := os.OpenFile(filepath.Join(inspection.Root(), sparseName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := sparse.Truncate(1 << 30); err != nil {
			_ = sparse.Close()
			t.Fatal(err)
		}
		if err := sparse.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inspection.Root(), "linked"), []byte("linked"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(inspection.Root(), "linked"), filepath.Join(inspection.Root(), "linked-again")); err != nil {
			t.Fatal(err)
		}
		entries := enumerateAllStorageEntries(t, root.storageDir)
		for _, name := range []string{"symlink", "fifo", sparseName, "linked"} {
			if err := root.unlinkEnumeratedEntry(entries[name]); err != nil {
				t.Fatalf("unlink %s: %v", name, err)
			}
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
			t.Fatalf("symlink cleanup touched target: %q, %v", got, err)
		}
		if got, err := os.ReadFile(filepath.Join(inspection.Root(), "linked-again")); err != nil || string(got) != "linked" {
			t.Fatalf("hard-link cleanup touched other link: %q, %v", got, err)
		}
	})

	t.Run("entry identity change", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := root.writeFileAtomic("victim", []byte("old")); err != nil {
			t.Fatal(err)
		}
		entry := enumerateAllStorageEntries(t, root.storageDir)["victim"]
		if err := os.Rename(filepath.Join(inspection.Root(), "victim"), filepath.Join(inspection.Root(), "old-victim")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inspection.Root(), "victim"), []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := root.unlinkEnumeratedEntry(entry); !errors.Is(err, errStorageEntryChanged) {
			t.Fatalf("changed-entry unlink = %v, want identity-changed", err)
		}
		if got, err := os.ReadFile(filepath.Join(inspection.Root(), "victim")); err != nil || string(got) != "new" {
			t.Fatalf("changed entry was unlinked: %q, %v", got, err)
		}
	})

	t.Run("unlink sync failure is recoverable", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		armed := false
		failed := false
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && !failed && step == storageStepDirectorySync {
				failed = true
				return errors.New("injected unlink parent sync failure")
			}
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := root.writeFileAtomic("poison", []byte("poison")); err != nil {
			t.Fatal(err)
		}
		entry := enumerateAllStorageEntries(t, root.storageDir)["poison"]
		armed = true
		if err := root.unlinkEnumeratedEntry(entry); err == nil {
			t.Fatal("unlink succeeded despite parent-sync failure")
		}
		if err := root.unlinkEnumeratedEntry(entry); err != nil {
			t.Fatalf("missing-entry unlink retry: %v", err)
		}
	})
}

func TestStorageIdentityCheckedEmptyDirectoryRemoval(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Mkdir(filepath.Join(inspection.Root(), "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := enumerateAllStorageEntries(t, root.storageDir)["empty"]
	if err := root.removeEnumeratedDirectory(entry); err != nil {
		t.Fatalf("remove empty directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(inspection.Root(), "empty")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed directory remains: %v", err)
	}
}

func TestStorageCleanupDirectoryRejectsCrossDeviceDescentBeforeOpen(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	childPath := filepath.Join(inspection.Root(), "child")
	sentinelPath := filepath.Join(childPath, "keep")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinelPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	childOpens := 0
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if path == childPath {
				metadata.dev ^= 1 << 63
			}
			return metadata
		},
		beforeDirectoryOpen: func(path string) error {
			if path == childPath {
				childOpens++
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	entry, err := root.lookupEntry("child")
	if err != nil {
		t.Fatal(err)
	}
	child, openErr := root.openEnumeratedCleanupDirectory(entry)
	if child != nil {
		_ = child.Close()
	}
	if openErr == nil || childOpens != 0 {
		t.Fatalf("cross-device cleanup descent = child:%v opens:%d err:%v", child != nil, childOpens, openErr)
	}
	if data, err := os.ReadFile(sentinelPath); err != nil || string(data) != "keep" {
		t.Fatalf("cross-device rejection changed sentinel: data=%q err=%v", data, err)
	}
}

func TestStorageCleanupDirectoryAllowsSameDeviceDescent(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	childPath := filepath.Join(inspection.Root(), "child")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	entry, err := root.lookupEntry("child")
	if err != nil {
		t.Fatal(err)
	}
	child, err := root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		t.Fatalf("same-device cleanup descent: %v", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStorageCleanupBoundaryBeginsAtRetainedMetricsRoot(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	childPath := filepath.Join(inspection.Root(), "child")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	var actual unix.Stat_t
	if err := unix.Stat(inspection.Root(), &actual); err != nil {
		t.Fatal(err)
	}
	syntheticRootDevice := unixStatDevice(actual) ^ (1 << 63)
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if path == inspection.Root() || strings.HasPrefix(path, inspection.Root()+string(os.PathSeparator)) {
				metadata.dev = syntheticRootDevice
			}
			return metadata
		},
	})
	if err != nil {
		t.Fatalf("open separately mounted metrics root simulation: %v", err)
	}
	defer func() { _ = root.Close() }()
	entry, err := root.lookupEntry("child")
	if err != nil {
		t.Fatal(err)
	}
	child, err := root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		t.Fatalf("retained-root-local cleanup descent: %v", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRootAtomicWritesJournalMarkerBeforeCreatingTemp(t *testing.T) {
	for _, operation := range []string{"replace", "outcome", "no-replace"} {
		t.Run(operation, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, false)
			journalName := ".pm-root-temp-journal"
			markerObserved := false
			root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
				beforeTempFileCreate: func(path string) {
					if filepath.Dir(path) != inspection.Root() {
						return
					}
					marker := filepath.Join(inspection.Root(), journalName, filepath.Base(path))
					info, markerErr := os.Lstat(marker)
					if markerErr == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 {
						markerObserved = true
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			switch operation {
			case "replace":
				err = root.writeFileAtomic("target", []byte("value"))
			case "outcome":
				_, err = root.writeFileAtomicOutcome("target", []byte("value"))
			case "no-replace":
				err = root.writeFileAtomicNoReplace("target", []byte("value"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if !markerObserved {
				t.Fatal("root temporary file was created before its durable journal marker")
			}
			entries, err := os.ReadDir(filepath.Join(inspection.Root(), journalName))
			if err != nil || len(entries) != 0 {
				t.Fatalf("successful root write journal is not empty: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestRootTempJournalMarkerCodecIsStrictAndExact(t *testing.T) {
	name := ".pm-tmp-1-2"
	temp := recordIncarnation{dev: 3, ino: 4}
	bound, err := encodeBoundRootTempJournalMarker(name, temp)
	if err != nil {
		t.Fatal(err)
	}
	wantBound := []byte{
		'G', 'C', 'P', 'M', 'R', 'T', 'J', '1', 0x02, byte(len(name)), 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 3,
		0, 0, 0, 0, 0, 0, 0, 4,
	}
	wantBound = append(wantBound, []byte(name)...)
	if !bytes.Equal(bound, wantBound) {
		t.Fatalf("bound marker wire form = %x, want %x", bound, wantBound)
	}
	decoded, err := decodeRootTempJournalMarker(name, bound)
	if err != nil || decoded.state != rootTempJournalMarkerBound || decoded.name != name || decoded.temp != temp {
		t.Fatalf("bound marker round trip = %+v err=%v", decoded, err)
	}
	intent, err := decodeRootTempJournalMarker(name, nil)
	if err != nil || intent.state != rootTempJournalMarkerIntent || intent.name != name {
		t.Fatalf("intent marker = %+v err=%v", intent, err)
	}

	clone := func() []byte { return append([]byte(nil), bound...) }
	badMagic := clone()
	badMagic[0] = 'X'
	reservedState := clone()
	reservedState[8] = 0x01
	badReserved := clone()
	badReserved[10] = 1
	zeroDevice := clone()
	for index := 16; index < 24; index++ {
		zeroDevice[index] = 0
	}
	zeroInode := clone()
	for index := 24; index < 32; index++ {
		zeroInode[index] = 0
	}
	for _, test := range []struct {
		name       string
		markerName string
		data       []byte
	}{
		{name: "empty marker name intent", markerName: "", data: nil},
		{name: "noncanonical intent name", markerName: "intent", data: nil},
		{name: "wrong magic", markerName: name, data: badMagic},
		{name: "reserved state", markerName: name, data: reservedState},
		{name: "reserved bytes", markerName: name, data: badReserved},
		{name: "truncated header", markerName: name, data: clone()[:rootTempJournalMarkerHeaderBytes-1]},
		{name: "truncated name", markerName: name, data: clone()[:len(bound)-1]},
		{name: "trailing byte", markerName: name, data: append(clone(), 0)},
		{name: "wrong enumerated name", markerName: ".pm-tmp-1-3", data: clone()},
		{name: "zero device", markerName: name, data: zeroDevice},
		{name: "zero inode", markerName: name, data: zeroInode},
	} {
		t.Run(test.name, func(t *testing.T) {
			if decoded, err := decodeRootTempJournalMarker(test.markerName, test.data); err == nil ||
				decoded.state != rootTempJournalMarkerInvalid {
				t.Fatalf("invalid marker decoded as %+v err=%v", decoded, err)
			}
		})
	}
}

func TestRootAtomicWritePreservesPreexistingTempCollision(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	setup, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	sequence := storageTempSequence.Load() + 1
	if sequence == 0 {
		sequence++
	}
	name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), sequence)
	collisionPath := filepath.Join(inspection.Root(), name)
	if err := os.WriteFile(collisionPath, []byte("preexisting root entry"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.writeFileAtomic("target", []byte("value")); err != nil {
		t.Fatalf("root write did not continue after a safely retained collision: %v", err)
	}
	if data, err := os.ReadFile(collisionPath); err != nil || string(data) != "preexisting root entry" {
		t.Fatalf("root write changed preexisting collision: data=%q err=%v", data, err)
	}
	markerPath := filepath.Join(inspection.Root(), rootTempJournalDirectoryName, name)
	if _, err := os.Lstat(markerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("root collision left its exact intent marker unsettled: %v", err)
	}
}

func TestRootAtomicWriteCounterWrapDoesNotConsumeCollisionAttempt(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	setup, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence < maximumStorageTempAttempts; sequence++ {
		name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), sequence)
		if err := os.WriteFile(filepath.Join(inspection.Root(), name), []byte("preexisting collision"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	priorSequence := storageTempSequence.Load()
	storageTempSequence.Store(^uint64(0))
	t.Cleanup(func() { storageTempSequence.Store(priorSequence) })
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.writeFileAtomic("target", []byte("value")); err != nil {
		t.Fatalf("counter wrap consumed one of %d real attempts: %v", maximumStorageTempAttempts, err)
	}
	if data, err := os.ReadFile(filepath.Join(inspection.Root(), "target")); err != nil || string(data) != "value" {
		t.Fatalf("counter-wrap write target = %q err=%v", data, err)
	}
	for sequence := uint64(1); sequence < maximumStorageTempAttempts; sequence++ {
		name := fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), sequence)
		if data, err := os.ReadFile(filepath.Join(inspection.Root(), name)); err != nil || string(data) != "preexisting collision" {
			t.Fatalf("counter-wrap collision %q changed: data=%q err=%v", name, data, err)
		}
	}
}

func TestRootAtomicWriteRejectsRetainedMarkerReplacementBeforeTempCreation(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var markerPath, displacedPath string
	replaced := false
	var replaceErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if replaced || filepath.Dir(path) != inspection.Root() {
				return
			}
			replaced = true
			markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
			displacedPath = markerPath + ".displaced"
			if err := os.Rename(markerPath, displacedPath); err != nil {
				replaceErr = err
				return
			}
			replaceErr = os.WriteFile(markerPath, []byte("replacement marker"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if replaceErr != nil {
		t.Fatalf("replace retained marker fixture: %v", replaceErr)
	}
	if !replaced {
		t.Fatal("retained marker replacement was not injected")
	}
	if writeErr == nil {
		t.Fatal("root atomic write continued after its exact marker was replaced")
	}
	if _, err := os.Lstat(filepath.Join(inspection.Root(), "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker replacement installed sensitive target: %v", err)
	}
	if data, err := os.ReadFile(markerPath); err != nil || string(data) != "replacement marker" {
		t.Fatalf("marker replacement was not retained: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(displacedPath); err != nil {
		t.Fatalf("displaced exact marker was unexpectedly removed: %v", err)
	}
	entries, err := os.ReadDir(inspection.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if canonicalStorageTempName(entry.Name()) {
			t.Fatalf("marker replacement left a root temp %q", entry.Name())
		}
	}
}

func TestRootAtomicWriteRevalidatesTempAfterBindHookBeforeBoundBytes(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, displacedTemp, markerPath string
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) != inspection.Root() {
				return
			}
			tempPath = path
			markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
		},
		beforeStep: func(step storageStep) error {
			if swapped || step != storageStepMarkerBind || tempPath == "" {
				return nil
			}
			swapped = true
			displacedTemp = tempPath + ".displaced"
			if err := os.Rename(tempPath, displacedTemp); err != nil {
				swapErr = err
				return nil
			}
			swapErr = os.WriteFile(tempPath, nil, 0o600)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if swapErr != nil || !swapped {
		t.Fatalf("swap root temp at marker bind: swapped=%v err=%v", swapped, swapErr)
	}
	if writeErr == nil {
		t.Fatal("root write accepted a temp replacement at marker bind")
	}
	if data, err := os.ReadFile(markerPath); err != nil || len(data) != 0 {
		t.Fatalf("failed bind made marker authoritative: bytes=%x err=%v", data, err)
	}
	for _, path := range []string{tempPath, displacedTemp} {
		if info, err := os.Lstat(path); err != nil || info.Size() != 0 {
			t.Fatalf("failed bind changed temp %q: info=%v err=%v", path, info, err)
		}
	}
}

func TestRootAtomicWriteRevalidatesIntentMarkerAfterTempMetadataBeforeBoundBytes(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, markerPath, displacedMarker string
	armed := false
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) != inspection.Root() {
				return
			}
			tempPath = path
			markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
		},
		beforeStep: func(step storageStep) error {
			if step == storageStepMarkerBind {
				armed = true
			}
			return nil
		},
		beforeMetadataAttempt: func(path string) error {
			if !armed || swapped || path != tempPath {
				return nil
			}
			swapped = true
			displacedMarker = markerPath + ".displaced"
			if err := os.Rename(markerPath, displacedMarker); err != nil {
				swapErr = err
				return nil
			}
			swapErr = os.WriteFile(markerPath, nil, 0o600)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if swapErr != nil || !swapped {
		t.Fatalf("swap marker during intent temp validation: swapped=%v err=%v", swapped, swapErr)
	}
	if writeErr == nil {
		t.Fatal("root write accepted a marker replacement during intent temp validation")
	}
	if _, err := os.Lstat(filepath.Join(inspection.Root(), "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker replacement installed target: %v", err)
	}
	for _, path := range []string{markerPath, displacedMarker} {
		if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
			t.Fatalf("failed intent guard wrote BOUND bytes through %q: data=%x err=%v", path, data, err)
		}
	}
}

func TestRootAtomicWritePreBindCrashLeavesOnlyIntentAndEmptyTemp(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, markerPath string
	injected := false
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) == inspection.Root() {
				tempPath = path
				markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
			}
		},
		beforeStep: func(step storageStep) error {
			if !injected && step == storageStepMarkerBind {
				injected = true
				return errors.New("injected pre-bind crash")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if closeErr := root.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if writeErr == nil || !injected {
		t.Fatalf("pre-bind crash = injected:%v err:%v", injected, writeErr)
	}
	for _, path := range []string{tempPath, markerPath} {
		if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
			t.Fatalf("pre-bind crash artifact %q = %x err=%v", path, data, err)
		}
	}
	cleanRoot, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanRoot.Close() }()
	state := &spoolSweepState{
		root:     cleanRoot,
		purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
		seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
	}
	state.cleanupRootTempJournal()
	if !errors.Is(state.operation, errUnsettledRootTempJournal) || state.mutated {
		t.Fatalf("pre-bind intent cleanup = mutated:%v err:%v", state.mutated, state.operation)
	}
	if data, err := os.ReadFile(tempPath); err != nil || len(data) != 0 {
		t.Fatalf("pre-bind cleanup changed empty temp: data=%x err=%v", data, err)
	}
}

func TestRootAtomicWriteRevalidatesMarkerAfterPayloadHookBeforeFirstByte(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, markerPath, displacedMarker string
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) != inspection.Root() {
				return
			}
			tempPath = path
			markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
		},
		beforeStep: func(step storageStep) error {
			if swapped || step != storageStepWrite || markerPath == "" {
				return nil
			}
			swapped = true
			displacedMarker = markerPath + ".displaced"
			if err := os.Rename(markerPath, displacedMarker); err != nil {
				swapErr = err
				return nil
			}
			swapErr = os.WriteFile(markerPath, nil, 0o600)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if swapErr != nil || !swapped {
		t.Fatalf("swap marker before payload: swapped=%v err=%v", swapped, swapErr)
	}
	if writeErr == nil {
		t.Fatal("root write accepted a marker replacement before payload")
	}
	if data, err := os.ReadFile(tempPath); err != nil || len(data) != 0 {
		t.Fatalf("marker replacement allowed sensitive temp bytes: data=%q err=%v", data, err)
	}
	for _, path := range []string{markerPath, displacedMarker} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("marker replacement evidence %q was removed: %v", path, err)
		}
	}
}

func TestRootAtomicWriteFailureCleanupRevalidatesMarkerAfterDeleteHook(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, markerPath, displacedMarker string
	payloadStarted := false
	failedSync := false
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) == inspection.Root() {
				tempPath = path
				markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
			}
		},
		beforeStep: func(step storageStep) error {
			if step == storageStepWrite {
				payloadStarted = true
			}
			if payloadStarted && !failedSync && step == storageStepFileSync {
				failedSync = true
				return errors.New("injected payload sync failure")
			}
			return nil
		},
		beforeMutation: func(step storageStep, path string) {
			if swapped || !failedSync || step != storageStepDelete || path != tempPath {
				return
			}
			swapped = true
			displacedMarker = markerPath + ".displaced"
			if err := os.Rename(markerPath, displacedMarker); err != nil {
				swapErr = err
				return
			}
			swapErr = os.WriteFile(markerPath, nil, 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if writeErr == nil || !failedSync || !swapped || swapErr != nil {
		t.Fatalf("failure cleanup marker swap = failed:%v swapped:%v swapErr:%v writeErr:%v",
			failedSync, swapped, swapErr, writeErr)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "sensitive" {
		t.Fatalf("failure cleanup mutated temp without marker authority: data=%q err=%v", data, err)
	}
	for _, path := range []string{markerPath, displacedMarker} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("failure cleanup marker evidence %q missing: %v", path, err)
		}
	}
}

func TestRootAtomicWriteMarkerRetirementRequiresTempAbsenceAfterDeleteHook(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, markerPath string
	created := false
	var createErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) == inspection.Root() {
				tempPath = path
				markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
			}
		},
		beforeMutation: func(step storageStep, path string) {
			if created || step != storageStepDelete || path != markerPath {
				return
			}
			created = true
			createErr = os.WriteFile(tempPath, []byte("late temp"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.writeFileAtomic("target", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if createErr != nil || !created {
		t.Fatalf("create temp at writer marker retirement: created=%v err=%v", created, createErr)
	}
	if data, err := os.ReadFile(filepath.Join(inspection.Root(), "target")); err != nil || string(data) != "value" {
		t.Fatalf("durable target changed: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(tempPath); err != nil || string(data) != "late temp" {
		t.Fatalf("late writer temp changed: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("writer marker retired over late temp: %v", err)
	}
}

func TestRootAtomicWritePreservesTempNameReplacementDuringValidationFailure(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var tempPath, displacedTemp, markerPath string
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) == inspection.Root() {
				tempPath = path
				markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
			}
		},
		metadata: func(path string, metadata storageMetadata) storageMetadata {
			if swapped || path != tempPath {
				return metadata
			}
			swapped = true
			displacedTemp = tempPath + ".displaced"
			if err := os.Rename(tempPath, displacedTemp); err != nil {
				swapErr = err
				return metadata
			}
			swapErr = os.WriteFile(tempPath, nil, 0o600)
			return metadata
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if writeErr == nil || !swapped || swapErr != nil {
		t.Fatalf("validation temp swap = swapped:%v swapErr:%v writeErr:%v", swapped, swapErr, writeErr)
	}
	for _, path := range []string{tempPath, displacedTemp, markerPath} {
		if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
			t.Fatalf("validation failure changed evidence %q: data=%x err=%v", path, data, err)
		}
	}
}

func TestRootAtomicWriteRevalidatesMarkerAfterRenameHookBeforeInstall(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	var markerPath, displacedMarker string
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if filepath.Dir(path) == inspection.Root() {
				markerPath = filepath.Join(inspection.Root(), rootTempJournalDirectoryName, filepath.Base(path))
			}
		},
		beforeStep: func(step storageStep) error {
			if swapped || step != storageStepRename || markerPath == "" {
				return nil
			}
			swapped = true
			displacedMarker = markerPath + ".displaced"
			if err := os.Rename(markerPath, displacedMarker); err != nil {
				swapErr = err
				return nil
			}
			swapErr = os.WriteFile(markerPath, nil, 0o600)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if swapErr != nil || !swapped {
		t.Fatalf("swap marker before rename: swapped=%v err=%v", swapped, swapErr)
	}
	if writeErr == nil {
		t.Fatal("root write installed target after marker replacement at rename")
	}
	if _, err := os.Lstat(filepath.Join(inspection.Root(), "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker replacement installed target: %v", err)
	}
	for _, path := range []string{markerPath, displacedMarker} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("rename marker evidence %q was removed: %v", path, err)
		}
	}
}

func TestRootAtomicWriteRejectsJournalPathReplacementBeforeTempCreation(t *testing.T) {
	inspection := inspectStorageTestHome(t, false)
	journalPath := filepath.Join(inspection.Root(), rootTempJournalDirectoryName)
	displacedPath := filepath.Join(inspection.Root(), ".displaced-root-temp-journal")
	swapped := false
	var swapErr error
	root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if swapped || filepath.Dir(path) != inspection.Root() {
				return
			}
			swapped = true
			if err := os.Rename(journalPath, displacedPath); err != nil {
				swapErr = err
				return
			}
			swapErr = os.Mkdir(journalPath, 0o700)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	writeErr := root.writeFileAtomic("target", []byte("sensitive"))
	if swapErr != nil {
		t.Fatalf("replace root-temp journal fixture: %v", swapErr)
	}
	if !swapped {
		t.Fatal("journal path replacement was not injected")
	}
	if writeErr == nil {
		t.Fatal("root atomic write continued after its journal became unreachable by name")
	}
	if _, err := os.Lstat(filepath.Join(inspection.Root(), "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal-path replacement installed target: %v", err)
	}
	entries, err := os.ReadDir(inspection.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if canonicalStorageTempName(entry.Name()) {
			t.Fatalf("journal-path replacement left unjournaled root temp %q", entry.Name())
		}
	}
}

func TestStorageEnumeratedCleanupMutationsRejectCrossDeviceBoundary(t *testing.T) {
	for _, operation := range []string{"unlink", "rmdir", "rename", "exchange"} {
		t.Run(operation, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			childPath := filepath.Join(inspection.Root(), "child")
			sentinelPath := filepath.Join(childPath, "keep")
			if err := os.Mkdir(childPath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinelPath, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			otherPath := filepath.Join(inspection.Root(), "other")
			if err := os.Mkdir(otherPath, 0o700); err != nil {
				t.Fatal(err)
			}
			mutationAttempts := 0
			root, err := openStorageRootMutableWithHooks(inspection, storageTestHooks{
				metadata: func(path string, metadata storageMetadata) storageMetadata {
					if path == childPath {
						metadata.dev ^= 1 << 63
					}
					return metadata
				},
				beforeMutation: func(_ storageStep, path string) {
					if path == childPath || strings.HasPrefix(path, childPath+string(os.PathSeparator)) {
						mutationAttempts++
					}
				},
				beforeExchange: func() error {
					mutationAttempts++
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			entry, err := root.lookupEntry("child")
			if err != nil {
				t.Fatal(err)
			}
			var mutationErr error
			switch operation {
			case "unlink":
				mutationErr = root.unlinkEnumeratedEntry(entry)
			case "rmdir":
				mutationErr = root.removeEnumeratedCleanupDirectory(entry)
			case "rename":
				_, mutationErr = root.renameEnumeratedDirectory(entry, root.storageDir, "parked")
			case "exchange":
				other, lookupErr := root.lookupEntry("other")
				if lookupErr != nil {
					t.Fatal(lookupErr)
				}
				_, mutationErr = root.exchangeEnumeratedEntries(entry, root.storageDir, other)
			}
			if !errors.Is(mutationErr, unix.EXDEV) || mutationAttempts != 0 {
				t.Fatalf("cross-device %s = attempts:%d err:%v", operation, mutationAttempts, mutationErr)
			}
			if data, err := os.ReadFile(sentinelPath); err != nil || string(data) != "keep" {
				t.Fatalf("cross-device %s changed sentinel: data=%q err=%v", operation, data, err)
			}
			if _, err := os.Lstat(filepath.Join(inspection.Root(), "parked")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("cross-device %s created rename target: %v", operation, err)
			}
		})
	}
}

func TestStorageDirectoryRemovalRejectsPostEnumerationTrustDrift(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	path := filepath.Join(inspection.Root(), "empty")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := enumerateAllStorageEntries(t, root.storageDir)["empty"]
	if err := os.Chmod(path, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := root.removeEnumeratedDirectory(entry); err == nil {
		t.Fatal("directory removal accepted post-enumeration mode drift")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rejected directory was removed: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestStorageCleanupFailureSeamsAndDirectorySyncRecovery(t *testing.T) {
	for _, test := range []struct {
		name      string
		step      storageStep
		directory bool
	}{
		{name: "unlink", step: storageStepUnlink},
		{name: "rmdir", step: storageStepRmdir, directory: true},
	} {
		t.Run(test.name+" failure", func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			armed := false
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				if armed && step == test.step {
					return errors.New("injected " + test.name + " failure")
				}
				return nil
			}}
			root, err := openStorageRootMutableWithHooks(inspection, hooks)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			path := filepath.Join(inspection.Root(), "entry")
			if test.directory {
				err = os.Mkdir(path, 0o700)
			} else {
				err = os.Symlink(filepath.Join(t.TempDir(), "target"), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			entry := enumerateAllStorageEntries(t, root.storageDir)["entry"]
			armed = true
			if test.directory {
				err = root.removeEnumeratedDirectory(entry)
			} else {
				err = root.unlinkEnumeratedEntry(entry)
			}
			if err == nil {
				t.Fatalf("cleanup ignored injected %s failure", test.name)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("failed %s removed entry: %v", test.name, err)
			}
		})
	}

	t.Run("rmdir parent sync retry", func(t *testing.T) {
		inspection := inspectStorageTestHome(t, true)
		armed := false
		failed := false
		hooks := storageTestHooks{beforeStep: func(step storageStep) error {
			if armed && !failed && step == storageStepDirectorySync {
				failed = true
				return errors.New("injected rmdir parent sync failure")
			}
			return nil
		}}
		root, err := openStorageRootMutableWithHooks(inspection, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		if err := os.Mkdir(filepath.Join(inspection.Root(), "empty"), 0o700); err != nil {
			t.Fatal(err)
		}
		entry := enumerateAllStorageEntries(t, root.storageDir)["empty"]
		armed = true
		if err := root.removeEnumeratedDirectory(entry); err == nil {
			t.Fatal("rmdir succeeded despite parent-sync failure")
		}
		if err := root.removeEnumeratedDirectory(entry); err != nil {
			t.Fatalf("missing-directory rmdir retry: %v", err)
		}
	})
}

func enumerateAllStorageEntries(t *testing.T, directory *storageDir) map[string]storageEntry {
	t.Helper()
	iterator, err := directory.iterateEntries()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := iterator.Close(); err != nil {
			t.Errorf("Close iterator: %v", err)
		}
	}()
	entries := make(map[string]storageEntry)
	for {
		entry, err := iterator.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[entry.name] = entry
	}
}

func countStep(steps []storageStep, want storageStep) int {
	count := 0
	for _, step := range steps {
		if step == want {
			count++
		}
	}
	return count
}

func requireOnlyPersistentRootTempJournal(t *testing.T, rootPath string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != rootTempJournalDirectoryName || !entries[0].IsDir() {
		t.Fatalf("storage root entries = %v, want only persistent root-temp journal", entries)
	}
	journalEntries, err := os.ReadDir(filepath.Join(rootPath, rootTempJournalDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	return journalEntries
}

func TestStorageAdvisoryLockUsesStableInodeAndHonorsContext(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	firstRoot, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := firstRoot.Close(); err != nil {
			t.Errorf("Close first root: %v", err)
		}
	}()
	secondRoot, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := secondRoot.Close(); err != nil {
			t.Errorf("Close second root: %v", err)
		}
	}()

	first, err := firstRoot.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	lockPath := filepath.Join(inspection.Root(), "state.lock")
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %04o, want 0600", got)
	}
	if err := firstRoot.writeFileAtomic("state.lock", []byte("replacement")); err == nil {
		t.Fatal("atomic writer was allowed to replace the stable lock inode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := secondRoot.acquireLock(ctx, "state.lock"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended acquire error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context-bounded lock took %v", elapsed)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := secondRoot.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	defer func() {
		if err := second.Release(); err != nil {
			t.Errorf("Release second lock: %v", err)
		}
	}()
	uploader, err := secondRoot.acquireLock(context.Background(), "uploader.lock")
	if err != nil {
		t.Fatalf("acquire uploader lock: %v", err)
	}
	if err := uploader.Release(); err != nil {
		t.Fatalf("release uploader lock: %v", err)
	}
	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock acquisition replaced the stable lock inode")
	}
}

func TestStorageCloseRacesOperationsWithTypedClosedResult(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	seed, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.writeFileAtomic("config.toml", []byte("config")); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.readFile("config.toml", 1024); !errors.Is(err, errStorageClosed) {
		t.Fatalf("operation after Close = %v, want typed closed error", err)
	}

	for range 64 {
		root, err := openStorageRootMutable(inspection)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var readErr, closeErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_, readErr = root.readFile("config.toml", 1024)
		}()
		go func() {
			defer wait.Done()
			<-start
			closeErr = root.Close()
		}()
		close(start)
		wait.Wait()
		if closeErr != nil {
			t.Fatalf("concurrent Close: %v", closeErr)
		}
		if readErr != nil && !errors.Is(readErr, errStorageClosed) {
			t.Fatalf("operation racing Close = %v, want completion or typed closed", readErr)
		}
	}
}

func TestStorageAdvisoryLockConcurrentReleaseIsIdempotent(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := root.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatal(err)
	}
	errorsByCaller := make(chan error, 64)
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByCaller <- lock.Release()
		}()
	}
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Errorf("concurrent Release: %v", err)
		}
	}
	reacquired, err := root.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatalf("reacquire after concurrent Release: %v", err)
	}
	if err := reacquired.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStorageAdvisoryLockRejectsHardlinkAndSymlink(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			inspection := inspectStorageTestHome(t, true)
			original := filepath.Join(inspection.Root(), "original")
			if err := os.WriteFile(original, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(inspection.Root(), "state.lock")
			var err error
			if kind == "hardlink" {
				err = os.Link(original, lockPath)
			} else {
				err = os.Symlink(original, lockPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			root, err := openStorageRootMutable(inspection)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := root.Close(); err != nil {
					t.Errorf("Close root: %v", err)
				}
			}()
			if _, err := root.acquireLock(context.Background(), "state.lock"); err == nil {
				t.Fatalf("acquireLock accepted %s", kind)
			}
		})
	}
}

func TestStorageAdvisoryLockIsReleasedWhenProcessDies(t *testing.T) {
	inspection := inspectStorageTestHome(t, true)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageLockHolderHelper$", "--", "--productmetrics-lock-holder", inspection.Home().Path())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && line != "locked\n" {
			readErr = fmt.Errorf("helper output %q", line)
		}
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("lock helper: %v", err)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("timed out waiting for lock helper")
	}
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("Close root: %v", err)
		}
	}()
	contendedContext, cancelContention := context.WithTimeout(context.Background(), 75*time.Millisecond)
	if _, err := root.acquireLock(contendedContext, "state.lock"); !errors.Is(err, context.DeadlineExceeded) {
		cancelContention()
		t.Fatalf("cross-process contended acquire = %v, want deadline exceeded", err)
	}
	cancelContention()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed lock helper exited successfully")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	lock, err := root.acquireLock(ctx, "state.lock")
	if err != nil {
		t.Fatalf("acquire after process death: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("Release lock: %v", err)
		}
	}()
}

func TestStorageLockHolderHelper(t *testing.T) {
	home, ok := parseStorageLockHolderArgs(os.Args)
	if !ok {
		return
	}
	if err := os.Setenv("GC_HOME", home); err != nil {
		t.Fatal(err)
	}
	inspection, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	root, err := openStorageRootMutable(inspection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := root.acquireLock(context.Background(), "state.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	fmt.Println("locked")
	for {
		time.Sleep(time.Hour)
	}
}

func parseStorageLockHolderArgs(args []string) (string, bool) {
	if len(args) < 4 {
		return "", false
	}
	suffix := args[len(args)-3:]
	if suffix[0] != "--" || suffix[1] != "--productmetrics-lock-holder" || suffix[2] == "" {
		return "", false
	}
	if !filepath.IsAbs(suffix[2]) || filepath.Clean(suffix[2]) != suffix[2] {
		return "", false
	}
	return suffix[2], true
}

func TestParseStorageLockHolderArgsRequiresExactSuffix(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantHome string
		wantOK   bool
	}{
		{name: "exact", args: []string{"test", "-test.run=helper", "--", "--productmetrics-lock-holder", "/safe/home"}, wantHome: "/safe/home", wantOK: true},
		{name: "normal invocation", args: []string{"test", "-test.run=helper"}},
		{name: "ambient sentinel", args: []string{"test", "--productmetrics-lock-holder", "/unsafe", "--", "other"}},
		{name: "missing separator", args: []string{"test", "--productmetrics-lock-holder", "/unsafe"}},
		{name: "tuple without argv zero", args: []string{"--", "--productmetrics-lock-holder", "/unsafe"}},
		{name: "extra trailing argument", args: []string{"test", "--", "--productmetrics-lock-holder", "/unsafe", "extra"}},
		{name: "empty home", args: []string{"test", "--", "--productmetrics-lock-holder", ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, ok := parseStorageLockHolderArgs(test.args)
			if home != test.wantHome || ok != test.wantOK {
				t.Fatalf("parse = (%q, %v), want (%q, %v)", home, ok, test.wantHome, test.wantOK)
			}
		})
	}
}
