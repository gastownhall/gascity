package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAPINonTestFilesCloseSessionsViaWorkerBoundary guards the session close
// path in internal/api against worker-boundary bypasses (ga-frfj2d), mirroring
// cmd/gc's TestGCNonTestFilesStayOnWorkerBoundary. Close must go through
// worker.Handle.CloseDetailed, not a directly constructed session.Manager.
//
// internal/api still constructs session.Manager values for other handlers
// (see "Active migrations / Worker boundary" in the root AGENTS.md), so this
// test only forbids direct manager *close* calls. The single pinned exception
// is the session-create rollback in handler_session_create.go, which closes a
// half-created session before any worker handle exists for it.
func TestAPINonTestFilesCloseSessionsViaWorkerBoundary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	allowed := map[string][]string{
		// Pre-existing rollback path: closes the session the manager itself
		// just created when post-create wiring fails. Pinned, not expandable.
		"handler_session_create.go": {"sessionManager(store).Close("},
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range []string{
			"mgr.CloseDetailed(",
			"mgr.Close(",
			"sessionManager(store).CloseDetailed(",
			"sessionManager(store).Close(",
		} {
			if !strings.Contains(content, needle) {
				continue
			}
			if closeBoundaryExempt(allowed[name], needle) {
				continue
			}
			t.Errorf("%s contains forbidden direct session.Manager close %q; route through worker.Handle.CloseDetailed instead", path, needle)
		}
	}
}

func closeBoundaryExempt(needles []string, needle string) bool {
	for _, n := range needles {
		if n == needle {
			return true
		}
	}
	return false
}
