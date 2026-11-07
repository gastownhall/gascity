// Command openapi-breaking fails when internal/api/openapi.json breaks
// clients built against the base revision's spec.
//
// It runs the pinned oasdiff binary (`make install-oasdiff`) over the base
// and revision specs with the severity overrides from the checked-in policy
// (internal/api/openapi-breaking.toml), adds gate-native checks oasdiff
// cannot see (problem-type URNs live in a vendor extension), and fails on
// any ERR-level change the policy does not waive.
//
// The base spec comes from -base-file, or from `git show <ref>:<revision>`
// where <ref> is -base (default $OPENAPI_BREAKING_BASE, else
// `git merge-base HEAD origin/main`).
//
// Run from the repository root:
//
//	go run ./cmd/openapi-breaking
//	go run ./cmd/openapi-breaking -base origin/main
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultRevision = "internal/api/openapi.json"
	defaultPolicy   = "internal/api/openapi-breaking.toml"
	defaultUpstream = "origin/main"
	// oasdiffTimeout bounds one oasdiff run; the full spec diffs in ~5s.
	oasdiffTimeout = 5 * time.Minute
)

type options struct {
	Oasdiff  string // oasdiff binary
	BaseRef  string // git ref holding the base spec (ignored when BaseFile is set)
	BaseFile string // base spec path
	Revision string // revision spec path
	Policy   string // policy TOML path
}

func main() {
	var opts options
	flag.StringVar(&opts.Oasdiff, "oasdiff", "", "oasdiff binary (default $OASDIFF, else oasdiff on PATH)")
	flag.StringVar(&opts.BaseRef, "base", os.Getenv("OPENAPI_BREAKING_BASE"), "git ref of the base spec (default $OPENAPI_BREAKING_BASE, else merge-base of HEAD and "+defaultUpstream+")")
	flag.StringVar(&opts.BaseFile, "base-file", "", "read the base spec from this file instead of git")
	flag.StringVar(&opts.Revision, "revision", defaultRevision, "revision spec path")
	flag.StringVar(&opts.Policy, "policy", defaultPolicy, "breaking-change policy TOML")
	flag.Parse()

	ok, err := run(context.Background(), opts, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-breaking:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

// run evaluates the gate and writes the report to out. It returns false when
// unwaived breaking changes exist and an error when the gate cannot run.
func run(ctx context.Context, opts options, out io.Writer) (bool, error) {
	v, err := check(ctx, opts)
	if err != nil {
		return false, err
	}
	return report(out, v, opts.Policy)
}

// check computes the gate verdict without rendering it.
func check(ctx context.Context, opts options) (verdict, error) {
	pol, err := loadPolicy(opts.Policy)
	if err != nil {
		return verdict{}, err
	}
	bin, err := resolveOasdiff(opts.Oasdiff)
	if err != nil {
		return verdict{}, err
	}
	revision, err := os.ReadFile(opts.Revision)
	if err != nil {
		return verdict{}, fmt.Errorf("reading revision spec: %w", err)
	}
	base, err := loadBase(ctx, opts)
	if err != nil {
		return verdict{}, err
	}

	dir, err := os.MkdirTemp("", "openapi-breaking-")
	if err != nil {
		return verdict{}, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort temp cleanup
	basePath := filepath.Join(dir, "base.json")
	levelsPath := filepath.Join(dir, "severity-levels.txt")
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		return verdict{}, fmt.Errorf("writing base spec: %w", err)
	}
	if err := os.WriteFile(levelsPath, []byte(pol.severityLevelsFile()), 0o600); err != nil {
		return verdict{}, fmt.Errorf("writing severity levels: %w", err)
	}

	findings, err := runOasdiff(ctx, bin, basePath, opts.Revision, levelsPath)
	if err != nil {
		return verdict{}, err
	}
	removed, err := problemTypeRemovals(base, revision)
	if err != nil {
		return verdict{}, err
	}
	return evaluate(append(findings, removed...), pol.Waivers)
}

func resolveOasdiff(flagValue string) (string, error) {
	for _, candidate := range []string{flagValue, os.Getenv("OASDIFF")} {
		if candidate != "" {
			return candidate, nil
		}
	}
	bin, err := exec.LookPath("oasdiff")
	if err != nil {
		return "", fmt.Errorf("oasdiff not found (run `make install-oasdiff` or set OASDIFF): %w", err)
	}
	return bin, nil
}

func loadBase(ctx context.Context, opts options) ([]byte, error) {
	if opts.BaseFile != "" {
		data, err := os.ReadFile(opts.BaseFile)
		if err != nil {
			return nil, fmt.Errorf("reading base spec: %w", err)
		}
		return data, nil
	}
	ref := opts.BaseRef
	if ref == "" {
		mb, err := gitOutput(ctx, "merge-base", "HEAD", defaultUpstream)
		if err != nil {
			return nil, fmt.Errorf("resolving base: no -base/OPENAPI_BREAKING_BASE and merge-base with %s failed (fetch it or pass -base): %w", defaultUpstream, err)
		}
		ref = strings.TrimSpace(string(mb))
	}
	data, err := gitOutput(ctx, "show", ref+":"+filepath.ToSlash(opts.Revision))
	if err != nil {
		return nil, fmt.Errorf("reading base spec at %s: %w", ref, err)
	}
	return data, nil
}

func gitOutput(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// runOasdiff returns the ERR-level findings of `oasdiff changelog`.
func runOasdiff(ctx context.Context, bin, base, revision, levels string) ([]finding, error) {
	ctx, cancel := context.WithTimeout(ctx, oasdiffTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "changelog", base, revision,
		"--format", "json",
		"--severity-levels", levels)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("oasdiff timed out after %s", oasdiffTimeout)
		}
		return nil, fmt.Errorf("running %s changelog: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return parseOasdiffChangelog(stdout.Bytes())
}
