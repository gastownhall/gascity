package rollout

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// depsEnvValue returns the value bound to key in a dotenv file ("" + false when
// absent). It is the read-side of the graduation forcing function: when the beads
// CAS gate's VersionAnchor (BD_CONDITIONAL_WRITES_MIN_VERSION) lands in deps.env
// with a concrete value, the gate has graduated past "pending".
func depsEnvValue(path, key string) (value string, present bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true, nil
		}
	}
	return "", false, sc.Err()
}

func writeDotenv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "deps.env")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}
	return p
}

// TestConditionalWritesGraduation proves the graduation forcing function on
// SYNTHETIC deps.env fixtures: DORMANT when the version anchor is absent (today's
// real state, beads#4682 untagged — see TestBeadsVersionAnchorPending), ARMED with
// a concrete version when it lands. When armed, the gate has graduated and S4-T4
// must flip the Default Off->Auto (FlipDueBy); this test is the seam that arms
// that work — it exercises the reader on both states without depending on the real
// (still pending) deps.env.
func TestConditionalWritesGraduation(t *testing.T) {
	t.Parallel()
	anchor := beadsConditionalWritesSpec().VersionAnchor
	if anchor == "" {
		t.Fatal("beads CAS gate has no VersionAnchor")
	}

	dormant := writeDotenv(t, "BD_VERSION=v1.1.0\nDOLT_VERSION=2.1.7\n")
	if v, present, err := depsEnvValue(dormant, anchor); err != nil || present {
		t.Errorf("dormant fixture: value=%q present=%v err=%v, want absent (graduation dormant)", v, present, err)
	}

	armed := writeDotenv(t, "BD_VERSION=v1.2.0\n"+anchor+"=v1.2.0\n")
	v, present, err := depsEnvValue(armed, anchor)
	if err != nil {
		t.Fatalf("armed fixture: %v", err)
	}
	if !present {
		t.Fatal("armed fixture: anchor absent, want present (graduation armed)")
	}
	if v != "v1.2.0" {
		t.Errorf("armed anchor value = %q, want v1.2.0", v)
	}
	if !strings.HasPrefix(v, "v") {
		t.Errorf("armed anchor value %q is not a version tag; the graduation forcing function needs a concrete flip target", v)
	}
}

// TestConditionalWritesGraduationRealDepsEnv binds the forcing function to the
// REAL repo-root deps.env, not a fixture. Today the anchor is absent (dormant):
// bd's --if-revision support is untagged, so BD_VERSION cannot satisfy the gate
// and Default stays Off. The moment someone lands the anchor in deps.env this
// test starts validating it: it must be a well-formed version tag, and
// BD_PREV_VERSION (the minimum-supported bd) must also satisfy it — a fleet
// where the previous binary predates conditional writes cannot graduate,
// because auto would silently degrade on every un-upgraded executor.
func TestConditionalWritesGraduationRealDepsEnv(t *testing.T) {
	t.Parallel()
	anchor := beadsConditionalWritesSpec().VersionAnchor
	realDepsEnv := filepath.Join("..", "..", "deps.env")

	if _, err := os.Stat(realDepsEnv); err != nil {
		t.Fatalf("repo-root deps.env unreadable: %v", err)
	}
	if v, present, err := depsEnvValue(realDepsEnv, "BD_VERSION"); err != nil || !present || !strings.HasPrefix(v, "v") {
		t.Fatalf("real deps.env BD_VERSION = %q present=%v err=%v, want a v-prefixed tag", v, present, err)
	}

	floor, present, err := depsEnvValue(realDepsEnv, anchor)
	if err != nil {
		t.Fatalf("reading %s from real deps.env: %v", anchor, err)
	}
	if !present {
		// Dormant, today's real state. Nothing more to check: graduation has
		// not been declared, so Default Off is correct and S4-T4 stays parked.
		return
	}
	// Armed for real. The declared floor must be a concrete version tag, and
	// the minimum-supported bd must be at or above it.
	if !strings.HasPrefix(floor, "v") {
		t.Fatalf("real %s = %q is not a version tag; graduation needs a concrete flip floor", anchor, floor)
	}
	prev, prevPresent, err := depsEnvValue(realDepsEnv, "BD_PREV_VERSION")
	if err != nil || !prevPresent {
		t.Fatalf("real deps.env BD_PREV_VERSION present=%v err=%v, want present alongside an armed anchor", prevPresent, err)
	}
	if semverLess(prev, floor) {
		t.Fatalf("graduation armed at %s=%s but BD_PREV_VERSION=%s predates it; "+
			"the minimum-supported bd must satisfy the CAS floor before the gate can graduate", anchor, floor, prev)
	}
}

// semverLess reports a < b for simple vX.Y.Z tags (numeric fields, no
// pre-release handling — deps.env only carries plain release tags).
func semverLess(a, b string) bool {
	pa, pb := parseSimpleSemver(a), parseSimpleSemver(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseSimpleSemver(v string) [3]int {
	var out [3]int
	for i, f := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		n := 0
		for _, r := range f {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

// TestTerminalSpecsCarryExpiryShape is the merge-CI half of the expiry teeth:
// every rollout/migration gate must carry a well-formed Expires (shape only — no
// time.Now(), which would make merge CI a flaky clock). Wall-clock staleness is a
// doctor WARN (runtime), not a merge gate. ValidateSpecs enforces this too; this
// test pins the intent so a future gate edit can't quietly drop the date.
func TestTerminalSpecsCarryExpiryShape(t *testing.T) {
	t.Parallel()
	for _, s := range Specs() {
		if s.Category != InfraRollout && s.Category != InfraMigration {
			continue
		}
		if !isYYYYMMDD(s.Expires) {
			t.Errorf("gate %s (%s): Expires %q is not a well-formed YYYY-MM-DD date", s.Key, s.Category, s.Expires)
		}
	}
}
