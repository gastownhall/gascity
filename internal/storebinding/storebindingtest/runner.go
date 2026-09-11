// Package storebindingtest is the reusable bare-class and lifecycle conformance
// harness for the storage classes. Every storage class front door — the
// canonical Beads adapters, the SQLite class stores, and any out-of-tree
// provider a downstream fork compiles in — runs the SAME suites from this
// package unchanged.
//
// The suites live in ordinary (non-test) files on purpose. A suite that lives
// in a package-local _test.go file is reachable only from that one package, so
// an out-of-tree provider or a production wrapper cannot bind to it. That is
// the defect this package exists to close: the exit evidence for the wrapper
// stack is "the unchanged suites pass through the wrappers", and there has to
// be something exported to pass.
//
// Suites take a [Runner] rather than a *testing.T so the harness's own tests
// can drive them with a recording runner and prove each assertion is
// load-bearing (see brokenfakes.go). Test callers pass Wrap(t).
package storebindingtest

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
)

// TB is the subset of [testing.TB] the conformance suites use. *testing.T
// satisfies it directly, so a store factory written against TB compiles
// unchanged in any test package.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	TempDir() string
	Name() string
	Failed() bool
}

// Runner is a TB that can also open a named subtest. It is the argument every
// suite takes.
type Runner interface {
	TB
	// Run executes fn as a named subtest and reports whether it passed.
	Run(name string, fn func(Runner)) bool
}

// Wrap adapts a *testing.T into a Runner.
func Wrap(t *testing.T) Runner { return goRunner{t} }

type goRunner struct{ *testing.T }

// Run executes fn as a subtest of the wrapped *testing.T.
func (g goRunner) Run(name string, fn func(Runner)) bool {
	return g.T.Run(name, func(t *testing.T) { fn(goRunner{t}) })
}

// Recorder is a Runner that records assertion failures instead of failing a
// test. The harness's own broken-fake tests drive a suite with one and assert
// that a deliberately non-conforming store fails exactly the assertions that
// are supposed to catch it.
type Recorder struct {
	root *recorderRoot
	name string
	// failed is this node's own verdict. Failures propagate upward so a
	// parent's Failed reflects its subtests.
	failed   bool
	cleanups []func()
}

type recorderRoot struct {
	mu       sync.Mutex
	messages map[string][]string
	tempRoot string
	tempSeq  int
}

// NewRecorder returns a root Recorder. tempRoot is the directory its TempDir
// allocates below; pass t.TempDir() from the driving test.
func NewRecorder(tempRoot string) *Recorder {
	return &Recorder{root: &recorderRoot{messages: map[string][]string{}, tempRoot: tempRoot}}
}

// Helper is a no-op: a Recorder reports subtest paths, not source positions.
func (r *Recorder) Helper() {}

// Name returns the slash-joined subtest path, empty at the root.
func (r *Recorder) Name() string { return r.name }

// Failed reports whether this node or any of its subtests failed.
func (r *Recorder) Failed() bool {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	return r.failed
}

// Logf discards output. A recording run is not a diagnostic run.
func (r *Recorder) Logf(string, ...any) {}

// Cleanup registers fn to run when the enclosing subtest returns.
func (r *Recorder) Cleanup(fn func()) {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}

// Errorf records a failure against this node's subtest path.
func (r *Recorder) Errorf(format string, args ...any) {
	r.record(fmt.Sprintf(format, args...))
}

// Fatalf records a failure and abandons the current subtest, exactly as
// [testing.T.Fatalf] does.
func (r *Recorder) Fatalf(format string, args ...any) {
	r.record(fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// TempDir returns a fresh directory below the recorder's temp root.
func (r *Recorder) TempDir() string {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	r.root.tempSeq++
	dir := filepath.Join(r.root.tempRoot, fmt.Sprintf("recorder-%d", r.root.tempSeq))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(fmt.Sprintf("storebindingtest: recorder temp dir: %v", err))
	}
	return dir
}

// Run executes fn as a named subtest, isolating its Fatalf and its cleanups.
func (r *Recorder) Run(name string, fn func(Runner)) bool {
	child := &Recorder{root: r.root, name: path.Join(r.name, name)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer child.runCleanups()
		defer func() {
			// Goexit from Fatalf leaves recover nil; only a real panic lands here.
			if value := recover(); value != nil {
				child.record(fmt.Sprintf("panic: %v", value))
			}
		}()
		fn(child)
	}()
	<-done
	if !child.Failed() {
		return true
	}
	r.markFailed()
	return false
}

func (r *Recorder) runCleanups() {
	r.root.mu.Lock()
	cleanups := r.cleanups
	r.cleanups = nil
	r.root.mu.Unlock()
	for index := len(cleanups) - 1; index >= 0; index-- {
		cleanups[index]()
	}
}

func (r *Recorder) record(message string) {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	r.root.messages[r.name] = append(r.root.messages[r.name], message)
	r.failed = true
}

func (r *Recorder) markFailed() {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	r.failed = true
}

// FailedAssertions returns the sorted subtest paths that recorded a failure.
func (r *Recorder) FailedAssertions() []string {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	paths := make([]string, 0, len(r.root.messages))
	for key := range r.root.messages {
		paths = append(paths, key)
	}
	sort.Strings(paths)
	return paths
}

// Messages returns the recorded failure messages for one subtest path.
func (r *Recorder) Messages(assertion string) []string {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	return append([]string(nil), r.root.messages[assertion]...)
}
