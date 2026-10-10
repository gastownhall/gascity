package main

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// Shared fixtures for the v2 tests, kept from the deleted per-key router and
// runtime tests (C3).

const routerTestLeg = "city:test"

func routerSessionBead(id string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

func routerWorkBead(id, status, assignee string) beads.Bead {
	return beads.Bead{ID: id, Type: "task", Status: status, Assignee: assignee}
}

func routedWorkBead(id, status, assignee, route string) beads.Bead {
	b := routerWorkBead(id, status, assignee)
	if route != "" {
		b.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: route}
	}
	return b
}

func beadEvent(t *testing.T, eventType string, b beads.Bead) events.Event {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("encoding %s: %v", b.ID, err)
	}
	return events.Event{Type: eventType, Subject: b.ID, Payload: payload}
}

// advance moves a synctest bubble's clock by d and lets it settle.
func advance(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

// Kills a reintroduction of the per-key workqueue (C3): no package cmd/gc
// builds from, nor cmd/gc's own tests, imports internal/workqueue.
func TestCmdGcDoesNotImportWorkqueue(t *testing.T) {
	const module = "github.com/gastownhall/gascity/"
	const banned = module + "internal/workqueue"
	root := bazeltest.RepoRoot(t)
	seen := map[string]bool{}
	var walk func(path string, tests bool, from string)
	walk = func(path string, tests bool, from string) {
		if path == banned {
			t.Errorf("%s imports %s", from, banned)
			return
		}
		if seen[path] || !strings.HasPrefix(path, module) {
			return
		}
		seen[path] = true
		pkg, err := build.Default.ImportDir(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, module))), 0)
		if err != nil {
			t.Fatalf("resolving %s: %v", path, err)
		}
		imports := pkg.Imports
		if tests {
			imports = append(append(imports, pkg.TestImports...), pkg.XTestImports...)
		}
		for _, imp := range imports {
			walk(imp, false, path)
		}
	}
	walk(module+"cmd/gc", true, "")
	if len(seen) < 10 {
		t.Fatalf("walked only %d packages, want cmd/gc's module dependencies", len(seen))
	}
}
