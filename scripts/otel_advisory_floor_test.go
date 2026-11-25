package scripts_test

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// otelAdvisoryFloors are the lowest go.mod versions that carry the fix for
// GO-2026-6974 (otel/sdk, otel/sdk/log, otel/trace; reachable from
// internal/telemetry and internal/productmetrics, so govulncheck exits 3 and
// the Go Vulnerability Scan job goes red) and GO-2026-6975 (otel). Each is a
// tagged release; a pseudo-version never satisfies a floor.
var otelAdvisoryFloors = map[string]string{
	"go.opentelemetry.io/otel":         "v1.47.0",
	"go.opentelemetry.io/otel/sdk":     "v1.47.0",
	"go.opentelemetry.io/otel/sdk/log": "v1.47.0",
	"go.opentelemetry.io/otel/trace":   "v1.47.0",
}

var goModRequireRe = regexp.MustCompile(`^\s*(?:require\s+)?(\S+)\s+(v\S+?)(?:\s+//.*)?$`)

// releaseTriple parses a tagged MAJOR.MINOR.PATCH version. A prerelease or
// build suffix (which includes every pseudo-version) is an error, so the
// floors cannot be met by an untagged commit.
func releaseTriple(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not a tagged vMAJOR.MINOR.PATCH release", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, fmt.Errorf("version %q is not a tagged vMAJOR.MINOR.PATCH release: %w", v, err)
		}
		out[i] = n
	}
	return out, nil
}

// TestGoModOTelModulesCarryAdvisoryFixes keeps the OpenTelemetry modules at
// or above the releases that fix GO-2026-6974 and GO-2026-6975, so a revert or
// a merge that lowers one fails here instead of turning the Go Vulnerability
// Scan red on main. Each floor module must be required in go.mod at a tagged
// release at or above its floor.
func TestGoModOTelModulesCarryAdvisoryFixes(t *testing.T) {
	root := repoRoot(t)
	required := map[string]string{}
	for _, line := range strings.Split(readFile(t, root, "go.mod"), "\n") {
		if m := goModRequireRe.FindStringSubmatch(line); m != nil {
			required[m[1]] = m[2]
		}
	}
	for mod, floor := range otelAdvisoryFloors {
		got, ok := required[mod]
		if !ok {
			t.Errorf("go.mod does not require %s; expected %s or later", mod, floor)
			continue
		}
		have, err := releaseTriple(got)
		if err != nil {
			t.Errorf("go.mod %s: %v (the advisory floor is a tagged release, never a pseudo-version)", mod, err)
			continue
		}
		want, err := releaseTriple(floor)
		if err != nil {
			t.Fatalf("floor for %s: %v", mod, err)
		}
		if have[0] != want[0] || have[1] < want[1] || (have[1] == want[1] && have[2] < want[2]) {
			t.Errorf("go.mod requires %s %s; GO-2026-6974/6975 are fixed in %s", mod, got, floor)
		}
	}
}
