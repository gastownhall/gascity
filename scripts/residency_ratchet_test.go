package scripts_test

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The one baseline all four rules ratchet against.
//
// Every rule in this contract counts sites per (file, enclosing function,
// pattern) and compares them to scripts/residency-boundary-baseline.txt. Shared
// storage is what keeps the halves from disagreeing about what is pinned — and
// it is why the proof of a pure refactor here is that the baseline does not
// move.

// readResidencyBaseline reads the
// `path <TAB> function <TAB> pattern <TAB> count` rows whose pattern the
// predicate accepts, keyed "path\tfunction\tpattern".
func readResidencyBaseline(path string, want func(pattern string) bool) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	out := map[string]int{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("baseline row %q is not path<TAB>function<TAB>pattern<TAB>count", line)
		}
		if !want(parts[2]) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[3]))
		if err != nil {
			return nil, fmt.Errorf("baseline row %q: bad count %q", line, parts[3])
		}
		out[parts[0]+"\t"+parts[1]+"\t"+parts[2]] = n
	}
	return out, scanner.Err()
}

// ratchetViolations reports growth and un-shrunk staleness, in that order.
func ratchetViolations(found, baseline map[string]int) []string {
	var out []string
	for _, key := range sortedKeys(found) {
		if found[key] > baseline[key] {
			out = append(out, fmt.Sprintf("%s: %d sites, baseline %d — a NEW store-enumeration site. Consume internal/storeref (Plan/ResolveOwner/Union), or annotate it `%s <reason>`.",
				strings.ReplaceAll(key, "\t", " "), found[key], baseline[key], residencyAllowMarker))
		}
	}
	for _, key := range sortedKeys(baseline) {
		if found[key] < baseline[key] {
			out = append(out, fmt.Sprintf("%s: %d sites, baseline %d — shrink the baseline in the same commit that retires a site. Follow the regeneration procedure in the header of scripts/residency-boundary-baseline.txt: three emitters feed that one file, so running any of them alone over it drops the other two halves' rows.",
				strings.ReplaceAll(key, "\t", " "), found[key], baseline[key]))
		}
	}
	return out
}

func assertRatchet(t *testing.T, found, baseline map[string]int) {
	t.Helper()
	for _, v := range ratchetViolations(found, baseline) {
		t.Errorf("RESIDENCY-BOUNDARY: %s", v)
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
