package exec_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestExecV2Conformance runs the in-tree provider contract against the
// exec provider over the runtimecontract golden reference (runtimecontract/reference), which declares
// observe.v1: the same executable passes the wire catalog
// (runtimecontract) and RunProviderTests, so the observe group's provider
// cases (absent after stop, a refused FreshOnly start, the attested
// listing) run through the production seam-backed provider in v2 mode.
func TestExecV2Conformance(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(filepath.Join(dir, "state"), reference.Bodies())), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := sessionexec.NewSeamBackedForCity(script, "exec-v2-conformance", "/exec-v2-conformance")
	if !runtime.FreshReadable(sp) || !runtime.ListRunningAttested(sp) {
		t.Fatal("the golden reference is not v2-ready through the exec provider")
	}
	var counter int64
	runtimetest.RunProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return sp, runtime.Config{WorkDir: caseT.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)},
			fmt.Sprintf("exec-v2-%06d", atomic.AddInt64(&counter, 1))
	})
}
