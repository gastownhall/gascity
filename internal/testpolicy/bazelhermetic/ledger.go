package bazelhermetic

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// LedgerPath is the repository-relative path of the checked ledger.
const LedgerPath = "test/bazel-hermeticity.toml"

// CacheExemptTags keep a test result out of the remote cache. A ledgered
// target must carry at least one.
var CacheExemptTags = []string{"external", "no-cache", "no-remote-cache"}

// ManagedTags are the tags tools/bazel/hermetic_tags.py owns on go_test
// rules: it sets them from the ledger and removes them everywhere else.
var ManagedTags = []string{"external", "no-cache", "no-remote-cache", "no-remote-exec", "requires-network"}

// Ledger is the decoded test/bazel-hermeticity.toml.
type Ledger struct {
	// Targets are go_test packages whose results must not be cached.
	Targets []LedgerTarget `toml:"target"`
	// Reviewed are signals a human confirmed do not make a package's
	// result environment-dependent.
	Reviewed []LedgerReview `toml:"reviewed"`
}

// LedgerTarget tags the go_test in Package so its result is never served
// from the remote cache.
type LedgerTarget struct {
	Package  string   `toml:"package"`
	Tags     []string `toml:"tags"`
	Reason   string   `toml:"reason"`
	Evidence []string `toml:"evidence"`
}

// LedgerReview accepts one signal in one package as hermetic.
type LedgerReview struct {
	Package string `toml:"package"`
	Signal  Signal `toml:"signal"`
	Reason  string `toml:"reason"`
}

// LoadLedger decodes and validates the ledger at LedgerPath in sourceFS.
func LoadLedger(sourceFS fs.FS) (Ledger, error) {
	data, err := fs.ReadFile(sourceFS, LedgerPath)
	if err != nil {
		return Ledger{}, fmt.Errorf("reading %s: %w", LedgerPath, err)
	}
	var ledger Ledger
	meta, err := toml.Decode(string(data), &ledger)
	if err != nil {
		return Ledger{}, fmt.Errorf("decoding %s: %w", LedgerPath, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Ledger{}, fmt.Errorf("decoding %s: unknown keys %v", LedgerPath, undecoded)
	}
	if err := ledger.Validate(); err != nil {
		return Ledger{}, fmt.Errorf("%s: %w", LedgerPath, err)
	}
	return ledger, nil
}

// Validate checks the ledger's own structure, independent of any scan.
func (l Ledger) Validate() error {
	var problems []string
	seen := map[string]bool{}
	for _, target := range l.Targets {
		if target.Package == "" {
			problems = append(problems, "a [[target]] has no package")
			continue
		}
		if seen[target.Package] {
			problems = append(problems, fmt.Sprintf("target %s is listed twice", target.Package))
		}
		seen[target.Package] = true
		if strings.TrimSpace(target.Reason) == "" {
			problems = append(problems, fmt.Sprintf("target %s has no reason", target.Package))
		}
		exempt := false
		for _, tag := range target.Tags {
			if !contains(ManagedTags, tag) {
				problems = append(problems, fmt.Sprintf("target %s: tag %q is not one of %v", target.Package, tag, ManagedTags))
			}
			if contains(CacheExemptTags, tag) {
				exempt = true
			}
		}
		if !exempt {
			problems = append(problems, fmt.Sprintf("target %s: tags %v keep the result cacheable; add one of %v", target.Package, target.Tags, CacheExemptTags))
		}
	}
	reviewed := map[string]bool{}
	for _, review := range l.Reviewed {
		key := review.Package + "|" + string(review.Signal)
		if review.Package == "" || !contains(AllSignals, review.Signal) {
			problems = append(problems, fmt.Sprintf("a [[reviewed]] entry needs a package and one of the signals %v (got %q, %q)", AllSignals, review.Package, review.Signal))
			continue
		}
		if reviewed[key] {
			problems = append(problems, fmt.Sprintf("reviewed %s %s is listed twice", review.Package, review.Signal))
		}
		reviewed[key] = true
		if strings.TrimSpace(review.Reason) == "" {
			problems = append(problems, fmt.Sprintf("reviewed %s %s has no reason", review.Package, review.Signal))
		}
		if seen[review.Package] {
			problems = append(problems, fmt.Sprintf("reviewed %s %s is redundant: the package is already a cache-exempt target", review.Package, review.Signal))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid ledger:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// Check compares scan findings with the ledger. Every finding must sit in a
// cache-exempt target package or match a reviewed (package, signal); every
// reviewed entry must still match a finding, so the ledger cannot outlive
// the code it excuses. Packages must exist in packages (the set of
// directories holding a go_test).
func Check(findings []Finding, ledger Ledger, packages map[string]bool) []string {
	exempt := map[string]bool{}
	for _, target := range ledger.Targets {
		exempt[target.Package] = true
	}
	reviewed := map[string]bool{}
	for _, review := range ledger.Reviewed {
		reviewed[review.Package+"|"+string(review.Signal)] = true
	}
	var problems []string
	matched := map[string]bool{}
	for _, finding := range findings {
		key := finding.Package + "|" + string(finding.Signal)
		switch {
		case exempt[finding.Package]:
		case reviewed[key]:
			matched[key] = true
		default:
			problems = append(problems, fmt.Sprintf("%s\n      undeclared test input in %s: make the test hermetic, or add the package to %s as a [[target]] with a cache-exempt tag (%s), or as a [[reviewed]] %q entry explaining why the result cannot change",
				finding, finding.Package, LedgerPath, strings.Join(CacheExemptTags, ", "), finding.Signal))
		}
	}
	for _, review := range ledger.Reviewed {
		key := review.Package + "|" + string(review.Signal)
		if !matched[key] {
			problems = append(problems, fmt.Sprintf("%s: reviewed %s %s matches no finding; remove the stale entry", LedgerPath, review.Package, review.Signal))
		}
	}
	for _, target := range ledger.Targets {
		if packages != nil && !packages[target.Package] {
			problems = append(problems, fmt.Sprintf("%s: target %s has no Go test package", LedgerPath, target.Package))
		}
	}
	sort.Strings(problems)
	return problems
}

// TestPackages returns the set of directories in sourceFS holding *_test.go
// files, using the same skip rules as Scan.
func TestPackages(sourceFS fs.FS) (map[string]bool, error) {
	packages := map[string]bool{}
	err := fs.WalkDir(sourceFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return skipDir(name, entry)
		}
		if strings.HasSuffix(name, "_test.go") {
			packages[path.Dir(name)] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing test packages: %w", err)
	}
	return packages, nil
}

func contains[T comparable](list []T, v T) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
