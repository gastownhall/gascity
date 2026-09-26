package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// gcTestSrcsBlockPattern isolates the srcs = [...] list that belongs to the
// gc_test go_test target specifically — cmd/gc/BUILD.bazel also declares
// gc_lib and gc targets, whose srcs must not be conflated with this one.
var gcTestSrcsBlockPattern = regexp.MustCompile(`(?s)go_test\(\s*name\s*=\s*"gc_test",.*?srcs\s*=\s*\[(.*?)\]`)

// TestBazelGCTestSrcsIncludeEveryUntaggedTestFile guards against the drift
// behind ga-j5vst2 / #6579: an ordinary cmd/gc test file with no build
// constraint can land without gazelle ever registering it in
// cmd/gc/BUILD.bazel's gc_test srcs list. `go test ./...` stays green
// regardless (Go itself never reads BUILD.bazel), so the gap is invisible
// until `bazel test //cmd/gc:gc_test` — or CI's bazel-test.yml — silently
// never compiles the file at all.
//
// This intentionally does not require every on-disk _test.go file to appear
// in srcs: files gated behind a //go:build constraint (integration,
// productmetrics_testhook, liveprobe, ...) are correctly excluded by gazelle
// from this untagged default target and must stay excluded.
func TestBazelGCTestSrcsIncludeEveryUntaggedTestFile(t *testing.T) {
	dir, err := gcTestSrcsCensusDir()
	if err != nil {
		t.Fatal(err)
	}

	srcs, err := parseGCTestSrcs(filepath.Join(dir, "BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	inSrcs := make(map[string]bool, len(srcs))
	for _, name := range srcs {
		inSrcs[name] = true
	}

	untagged, err := untaggedTestFileNames(dir)
	if err != nil {
		t.Fatal(err)
	}

	var missing []string
	for _, name := range untagged {
		if !inSrcs[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("cmd/gc/BUILD.bazel gc_test srcs is missing %d untagged test file(s); run `make bazel-sync` and commit the result:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

// gcTestSrcsCensusDir locates the cmd/gc directory regardless of whether
// tests run with the package directory or the repo root as the working
// directory.
func gcTestSrcsCensusDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get gc_test srcs census working directory: %w", err)
	}
	for _, candidate := range []string{cwd, filepath.Join(cwd, "cmd", "gc")} {
		info, statErr := os.Stat(filepath.Join(candidate, "BUILD.bazel"))
		if statErr == nil && !info.IsDir() {
			return filepath.Clean(candidate), nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect gc_test srcs census directory %q: %w", candidate, statErr)
		}
	}
	return "", fmt.Errorf("locate cmd/gc/BUILD.bazel from working directory %q", cwd)
}

// parseGCTestSrcs extracts the quoted file list from the gc_test go_test
// target's srcs attribute in the given BUILD.bazel file. This is a plain
// text scan, not a Starlark parser: cmd/gc/BUILD.bazel is gazelle-generated
// and its srcs lists are consistently one quoted string literal per line, so
// a regexp is precise enough and avoids taking on a Starlark-parsing
// dependency for a single generated file.
func parseGCTestSrcs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	match := gcTestSrcsBlockPattern.FindSubmatch(data)
	if match == nil {
		return nil, fmt.Errorf("%q: gc_test srcs block not found", path)
	}
	var srcs []string
	for _, line := range strings.Split(string(match[1]), "\n") {
		trimmed := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if len(trimmed) >= 2 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
			srcs = append(srcs, trimmed[1:len(trimmed)-1])
		}
	}
	return srcs, nil
}

// untaggedTestFileNames lists the _test.go files directly in dir that carry
// no //go:build constraint — the files gazelle places in gc_test's default
// srcs list.
func untaggedTestFileNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		tagged, err := hasGoBuildConstraint(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if !tagged {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// hasGoBuildConstraint reports whether path has a //go:build line before its
// package clause, per the Go build-constraint syntax rules.
func hasGoBuildConstraint(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %q: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//go:build") {
			return true, nil
		}
		if strings.HasPrefix(trimmed, "package ") {
			break
		}
	}
	return false, nil
}
