package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxiedStoreNotRunningOnlyForAStoppedProxiedScope(t *testing.T) {
	proxied := t.TempDir()
	writeProxiedLocalScope(t, proxied, "hq")
	if !ProxiedStoreNotRunning(proxied) {
		t.Error("a proxied scope with no proxy record reads as running")
	}

	direct := t.TempDir()
	if err := os.MkdirAll(filepath.Join(direct, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(direct, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, scope := range map[string]string{"direct": direct, "unbound": t.TempDir()} {
		if ProxiedStoreNotRunning(scope) {
			t.Errorf("a %s scope reads as a stopped proxied store", name)
		}
	}
}

func TestStoreNotRunningCheckKeepsTheNameAndHasNoFix(t *testing.T) {
	c := StoreNotRunningCheck("custom-types:city", false, "city", "r1")
	if c.Name() != "custom-types:city" || c.CanFix() {
		t.Fatalf("name/canFix = %q/%v", c.Name(), c.CanFix())
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK || r.Message != StoreNotRunningMessage+" (city, r1)" {
		t.Fatalf("result = %v %q", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, " "), "gc start") {
		t.Errorf("details do not say how to get the check run: %q", r.Details)
	}
}

// Under a running city a stopped store is a fault, so the stand-in warns.
func TestStoreNotRunningCheckWarnsWhenTheCityIsRunning(t *testing.T) {
	r := StoreNotRunningCheck("beads-store", true, "city").Run(&CheckContext{})
	if r.Status != StatusWarning || !strings.Contains(r.Message, StoreNotRunningMessage) || r.FixHint == "" {
		t.Fatalf("result = %v %q (hint %q), want a warning with a fix hint", r.Status, r.Message, r.FixHint)
	}
}
