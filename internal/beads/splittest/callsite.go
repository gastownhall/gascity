package splittest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// Site is the production call site a split violation is charged to: the first
// frame on the caller's stack that is neither test code nor store plumbing.
type Site struct {
	// File is the repo-relative, slash-separated source file.
	File string
	// Line is the line within File.
	Line int
	// Func is the short function name (package-qualified, import path
	// dropped), e.g. "api.(*Server).resolveBeadOwner".
	Func string
}

// String renders the site as file:line.
func (s Site) String() string {
	if s.File == "" {
		return "?"
	}
	return fmt.Sprintf("%s:%d", s.File, s.Line)
}

// stackFacts is what one stack walk learns about a store call.
type stackFacts struct {
	site Site
	// via is the next production frame outside the site's own file: who
	// called the site. A domain front door (a session or mail store) built over
	// the wrong store is charged to the front door; via names who built it.
	via Site
	// test is the top-level test function the call ran under, or "" for a
	// goroutine whose stack does not reach one.
	test string
	// marker is the read marker (beads.FederatedRead, beads.RelicRead) the
	// call runs under, if any.
	marker beads.ReadMarker
	// pkg is the nearest frame, other than the site, in the package under
	// test (SetPackageUnderTest): which of that package's functions led to
	// the site. Empty when no such frame is on the stack.
	pkg Site
}

var (
	// sourceRoot is the source-path prefix every frame of this build shares,
	// derived from this file's own path. go test records absolute paths and
	// Bazel's rules_go records execroot-relative ones; either way stripping
	// the prefix this file carries yields a repo-relative path.
	sourceRoot = func() string {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			return ""
		}
		const self = "internal/beads/splittest/callsite.go"
		file = filepath.ToSlash(file)
		if strings.HasSuffix(file, self) {
			return strings.TrimSuffix(file, self)
		}
		return ""
	}()

	transparentMu sync.RWMutex
	// transparentFiles are repo-relative production files whose frames are
	// pass-through store plumbing (a policy wrapper, a gate seam) rather than
	// a call site: a violation is charged to whoever called INTO them.
	transparentFiles = map[string]bool{}
)

// TreatFilesAsPlumbing registers repo-relative production files whose frames a
// split violation skips over when it looks for the responsible call site, the
// way it already skips internal/beads and this package. A package registers its
// own pass-through wrappers (cmd/gc's bead policy store, its gate seam) before
// any violation is recorded.
func TreatFilesAsPlumbing(files ...string) {
	transparentMu.Lock()
	defer transparentMu.Unlock()
	for _, f := range files {
		transparentFiles[filepath.ToSlash(f)] = true
	}
}

// frameInfo is the part of a stack frame the site search reads.
type frameInfo struct {
	file     string // repo-relative when the frame is this repository's
	line     int
	function string
}

// captureStack walks the stack of its caller and reports the call site, the
// test, and whether the read was marked federated. The walk starts at the
// caller; store and gate frames are skipped by kind, not by count.
func captureStack() stackFacts {
	pcs := make([]uintptr, 96)
	n := runtime.Callers(2, pcs)
	pcs = pcs[:n]
	var stack []frameInfo
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		stack = append(stack, frameInfo{file: relativeSourcePath(frame.File), line: frame.Line, function: frame.Function})
		if !more {
			break
		}
	}
	facts := factsFromFrames(stack)
	facts.marker = beads.ReadMarkerOnStack(pcs)
	return facts
}

// factsFromFrames picks the responsible call site (the innermost frame that is
// neither test code nor plumbing) and the outermost top-level test from a
// stack, innermost frame first.
func factsFromFrames(stack []frameInfo) stackFacts {
	var facts stackFacts
	for _, frame := range stack {
		if strings.HasSuffix(frame.file, "_test.go") {
			if name := topLevelTestName(frame.function); name != "" {
				facts.test = name
			}
			continue
		}
		if isPlumbingFrame(frame.file, frame.function) {
			continue
		}
		here := Site{File: frame.file, Line: frame.line, Func: normalizeFunc(shortFuncName(frame.function))}
		switch {
		case facts.site.File == "":
			facts.site = here
			continue
		case facts.via.File == "" && frame.file != facts.site.File:
			facts.via = here
		}
		if facts.pkg.File == "" && inPackageUnderTest(frame.file) && !isGenericGlue(frame.function) {
			facts.pkg = here
		}
	}
	return facts
}

var (
	packageUnderTestMu sync.RWMutex
	packageUnderTest   string
)

// SetPackageUnderTest names the repo-relative directory of the package whose
// tests are running ("cmd/gc", "internal/api"). A violation then records the
// nearest frame in that package that led to the site, and the ratchet keys on
// it: a known site reached from a NEW caller in the package is a new site.
func SetPackageUnderTest(dir string) {
	packageUnderTestMu.Lock()
	defer packageUnderTestMu.Unlock()
	packageUnderTest = strings.TrimSuffix(filepath.ToSlash(dir), "/")
}

func inPackageUnderTest(file string) bool {
	packageUnderTestMu.RLock()
	dir := packageUnderTest
	packageUnderTestMu.RUnlock()
	if dir == "" || !strings.HasPrefix(file, dir+"/") {
		return false
	}
	return !strings.Contains(strings.TrimPrefix(file, dir+"/"), "/")
}

// closureSuffix matches the compiler's names for closures, deferred and
// go-statement wrappers ("f.func1", "f.func2.3", "f.gowrap1", "f.deferwrap1"),
// and generic instantiation markers ("f[...]").
var closureSuffix = regexp.MustCompile(`(\.(func|gowrap|deferwrap)\d+(\.\d+)*|\[[^\]]*\])`)

// methodValue matches a method value's wrapper name, which the compiler
// spells with its enclosing function in some builds and without it in others
// ("pkg.f.(*T).m-fm" or "pkg.(*T).m-fm"): it is named for the method.
var methodValue = regexp.MustCompile(`^([^.]+)\..*(\(\*?[A-Za-z0-9_]+\)\.[A-Za-z0-9_]+)(-fm)?$`)

// normalizeFunc drops closure, instantiation and method-value decorations
// from a function name, so a key names the function a reader can find in the
// source, does not shift when a closure is added above the site, and reads
// the same whatever the build inlined.
func normalizeFunc(name string) string {
	name = strings.TrimSuffix(name, "-fm")
	for {
		next := closureSuffix.ReplaceAllString(name, "")
		if next == name {
			break
		}
		name = next
	}
	if m := methodValue.FindStringSubmatch(name); m != nil && strings.Count(name, "(") > 1 {
		return m[1] + "." + m[2]
	}
	return name
}

// isGenericGlue reports a frame of a generic function's instantiation (or a
// closure inside one): request-routing glue such as internal/api's
// cityGet[...]/cityPost[...], whose frames builds name and keep differently.
// The ratchet's caller skips them for the next frame of the package under
// test.
func isGenericGlue(function string) bool {
	return strings.Contains(function, "[")
}

// relativeSourcePath strips the build's repo-root prefix from a frame's file.
func relativeSourcePath(file string) string {
	file = filepath.ToSlash(file)
	if sourceRoot != "" && strings.HasPrefix(file, sourceRoot) {
		return strings.TrimPrefix(file, sourceRoot)
	}
	return file
}

// isPlumbingFrame reports frames that are never the responsible call site: the
// Go runtime and testing harness, compiler-generated method wrappers (promoted
// methods of the typed class wrappers), internal/beads itself (stores and
// their wrappers), this package, and registered pass-through files.
func isPlumbingFrame(file, function string) bool {
	if file == "<autogenerated>" || file == "" {
		return true
	}
	for _, prefix := range []string{"runtime.", "testing.", "reflect.", "sync."} {
		if strings.HasPrefix(function, prefix) {
			return true
		}
	}
	if strings.HasPrefix(file, "internal/beads/") && !strings.Contains(strings.TrimPrefix(file, "internal/beads/"), "/") {
		return true
	}
	if strings.HasPrefix(file, "internal/beads/splittest/") {
		return true
	}
	if !isRepoSource(file) {
		// The standard library or a module dependency.
		return true
	}
	transparentMu.RLock()
	defer transparentMu.RUnlock()
	return transparentFiles[file]
}

// repoSourceDirs are the repository's top-level Go source trees. A frame
// outside them is the standard library or a dependency, whatever form the build
// recorded its path in.
var repoSourceDirs = []string{"cmd/", "internal/", "pkg/", "test/", "examples/", "contrib/", "tools/", "scripts/"}

// isRepoSource reports whether a repo-relative path names this repository's
// own source.
func isRepoSource(file string) bool {
	for _, dir := range repoSourceDirs {
		if strings.HasPrefix(file, dir) {
			return true
		}
	}
	return false
}

// topLevelTestName extracts "TestFoo" from a test-file frame's function name
// ("pkg.TestFoo", "pkg.TestFoo.func1.2"), or "" when the frame is a helper.
func topLevelTestName(function string) string {
	short := shortFuncName(function)
	dot := strings.Index(short, ".")
	if dot < 0 {
		return ""
	}
	rest := short[dot+1:]
	if !strings.HasPrefix(rest, "Test") {
		return ""
	}
	if end := strings.Index(rest, "."); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// shortFuncName drops the import path from a frame's function name.
func shortFuncName(function string) string {
	if slash := strings.LastIndex(function, "/"); slash >= 0 {
		return function[slash+1:]
	}
	return function
}
