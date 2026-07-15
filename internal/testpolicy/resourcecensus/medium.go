package resourcecensus

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RunnableOwner is the canonical package plus top-level Go test identity.
type RunnableOwner struct {
	PackageDir  string
	PackageName string
	Owner       string
}

// MediumOwner declares the exact runnable owner of a set of Medium resources.
type MediumOwner struct {
	PackageDir      string     `toml:"package_dir"`
	PackageName     string     `toml:"package_name"`
	Owner           string     `toml:"owner"`
	Resources       []Resource `toml:"resources"`
	OwnerBead       string     `toml:"owner_bead"`
	Invariant       string     `toml:"invariant"`
	ResourceOwner   string     `toml:"resource_owner"`
	MigrationTarget string     `toml:"migration_target"`
	Expires         string     `toml:"expires"`
}

type runnableKey struct {
	packageDir  string
	packageName string
	owner       string
}

// SmallCount returns resource calls not owned by an exact Medium declaration.
func (c Census) SmallCount(scope Scope, resource Resource, medium []MediumOwner) Count {
	owned := make(map[runnableKey]map[Resource]struct{}, len(medium))
	for _, row := range medium {
		key := runnableKey{packageDir: row.PackageDir, packageName: row.PackageName, owner: row.Owner}
		resources := owned[key]
		if resources == nil {
			resources = make(map[Resource]struct{})
			owned[key] = resources
		}
		for _, declared := range row.Resources {
			resources[declared] = struct{}{}
		}
	}

	files := map[string]struct{}{}
	count := Count{}
	for _, occurrence := range c.Occurrences {
		if occurrence.Resource != resource || !scopeContains(scope, occurrence) {
			continue
		}
		if occurrence.Runnable {
			key := runnableKey{packageDir: occurrence.PackageDir, packageName: occurrence.PackageName, owner: occurrence.Owner}
			if _, excluded := owned[key][resource]; excluded {
				continue
			}
		}
		count.Calls++
		files[occurrence.Path] = struct{}{}
	}
	count.Files = len(files)
	return count
}

func validateMediumOwners(rows []MediumOwner, census Census, now time.Time) error {
	runnables := make(map[runnableKey]struct{}, len(census.Runnables))
	for _, runnable := range census.Runnables {
		runnables[runnableKey{packageDir: runnable.PackageDir, packageName: runnable.PackageName, owner: runnable.Owner}] = struct{}{}
	}

	problems := validateMediumDefinitions(rows, now)
	for _, row := range rows {
		key := runnableKey{packageDir: row.PackageDir, packageName: row.PackageName, owner: row.Owner}
		prefix := fmt.Sprintf("medium owner package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner)
		if _, exists := runnables[key]; !exists {
			problems = append(problems, prefix+": runnable owner does not exist")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}

func validateMediumDefinitions(rows []MediumOwner, now time.Time) []string {
	seen := make(map[runnableKey]struct{}, len(rows))
	var problems []string
	for _, row := range rows {
		key := runnableKey{packageDir: row.PackageDir, packageName: row.PackageName, owner: row.Owner}
		prefix := fmt.Sprintf("medium owner package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner)
		if _, duplicate := seen[key]; duplicate {
			problems = append(problems, fmt.Sprintf("duplicate medium owner: package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner))
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(row.PackageDir) == "" {
			problems = append(problems, prefix+": package_dir is required")
		}
		if strings.TrimSpace(row.PackageName) == "" {
			problems = append(problems, prefix+": package_name is required")
		}
		if strings.TrimSpace(row.Owner) == "" {
			problems = append(problems, prefix+": owner is required")
		}
		if len(row.Resources) == 0 {
			problems = append(problems, prefix+": resources must not be empty")
		}
		declared := make(map[Resource]struct{}, len(row.Resources))
		for _, resource := range row.Resources {
			if _, duplicate := declared[resource]; duplicate {
				problems = append(problems, fmt.Sprintf("%s: duplicate resource %q", prefix, resource))
			}
			declared[resource] = struct{}{}
			if _, known := knownResources[resource]; !known {
				problems = append(problems, fmt.Sprintf("%s: unknown resource %q", prefix, resource))
			}
		}
		problems = append(problems, validateOwnershipFields(prefix, row.OwnerBead, row.Invariant, row.ResourceOwner, row.MigrationTarget, row.Expires, now)...)
	}
	return problems
}

func validateMediumRowsAgainstPolicy(policyRows, ledgerRows []MediumOwner, now time.Time) []string {
	problems := validateMediumDefinitions(policyRows, now)
	problems = append(problems, validateMediumDefinitions(ledgerRows, now)...)
	policyByKey := make(map[runnableKey]MediumOwner, len(policyRows))
	for _, row := range policyRows {
		policyByKey[runnableKey{packageDir: row.PackageDir, packageName: row.PackageName, owner: row.Owner}] = row
	}
	seen := make(map[runnableKey]struct{}, len(ledgerRows))
	for _, row := range ledgerRows {
		key := runnableKey{packageDir: row.PackageDir, packageName: row.PackageName, owner: row.Owner}
		seen[key] = struct{}{}
		prefix := fmt.Sprintf("medium owner package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner)
		want, exists := policyByKey[key]
		if !exists {
			problems = append(problems, fmt.Sprintf("unexpected medium owner: package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner))
			continue
		}
		if !equalResources(row.Resources, want.Resources) {
			problems = append(problems, fmt.Sprintf("%s: resources = %v, bootstrap policy requires %v", prefix, row.Resources, want.Resources))
		}
		for _, field := range []struct {
			name      string
			got, want string
		}{
			{"owner_bead", row.OwnerBead, want.OwnerBead},
			{"invariant", row.Invariant, want.Invariant},
			{"resource_owner", row.ResourceOwner, want.ResourceOwner},
			{"migration_target", row.MigrationTarget, want.MigrationTarget},
			{"expires", row.Expires, want.Expires},
		} {
			if field.got != field.want {
				problems = append(problems, fmt.Sprintf("%s: %s = %q, bootstrap policy requires %q", prefix, field.name, field.got, field.want))
			}
		}
	}
	for key := range policyByKey {
		if _, exists := seen[key]; !exists {
			problems = append(problems, fmt.Sprintf("missing required medium owner: package_dir=%s package_name=%s owner=%s", key.packageDir, key.packageName, key.owner))
		}
	}
	return problems
}

func equalResources(left, right []Resource) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateSmallBaseline(row Baseline, census Census, medium []MediumOwner) []string {
	actual := census.SmallCount(row.Scope, row.Resource, medium)
	switch {
	case actual.Calls > row.BaselineCalls || actual.Files > row.BaselineFiles:
		return []string{fmt.Sprintf("Small resource census grew: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d)", row.Scope, row.Resource, actual.Calls, row.BaselineCalls, actual.Files, row.BaselineFiles)}
	case actual.Calls < row.BaselineCalls || actual.Files < row.BaselineFiles:
		return []string{fmt.Sprintf("Small resource census baseline is stale: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d); lower the checked baseline to bank the improvement", row.Scope, row.Resource, actual.Calls, row.BaselineCalls, actual.Files, row.BaselineFiles)}
	default:
		return nil
	}
}
