package herdr

import (
	"testing"
	"time"
)

// The reconciler's pending-create ownership check reads GC_SESSION_ID /
// GC_INSTANCE_TOKEN via GetMeta while Start is still delivering the startup
// nudge. tmux satisfies that read from the session environment (seeded from
// cfg.Env at creation); herdr's sidecar must be seeded explicitly or the
// fresh runtime is reaped as "live runtime belongs to another session".
func TestSeedMetaFromEnvMakesIdentityKeysReadable(t *testing.T) {
	p := New("gctest-seedmeta", t.TempDir(), t.TempDir(), time.Second)
	env := map[string]string{
		"GC_SESSION_ID":     "az-wisp-abc12",
		"GC_INSTANCE_TOKEN": "tok-1",
		"GC_RUNTIME_EPOCH":  "3",
	}
	if err := p.seedMetaFromEnv("polecat-az-wisp-abc12", env); err != nil {
		t.Fatalf("seedMetaFromEnv: %v", err)
	}
	for k, want := range env {
		got, err := p.GetMeta("polecat-az-wisp-abc12", k)
		if err != nil || got != want {
			t.Errorf("GetMeta(%q) = %q, %v; want %q", k, got, err, want)
		}
	}
	// Unset keys still read as absent, not an error.
	if got, err := p.GetMeta("polecat-az-wisp-abc12", "GC_DRAIN"); err != nil || got != "" {
		t.Errorf("GetMeta(unset) = %q, %v; want \"\", nil", got, err)
	}
}

// Later SetMeta calls override seeded values, matching tmux setenv semantics.
func TestSeedMetaFromEnvIsOverridableBySetMeta(t *testing.T) {
	p := New("gctest-seedmeta2", t.TempDir(), t.TempDir(), time.Second)
	if err := p.seedMetaFromEnv("s1", map[string]string{"GC_INSTANCE_TOKEN": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "new" {
		t.Errorf("GetMeta after override = %q, want %q", got, "new")
	}
}

// Empty env is a no-op.
func TestSeedMetaFromEnvEmptyIsNoOp(t *testing.T) {
	p := New("gctest-seedmeta3", t.TempDir(), t.TempDir(), time.Second)
	if err := p.seedMetaFromEnv("s1", nil); err != nil {
		t.Fatalf("seedMetaFromEnv(nil) = %v, want nil", err)
	}
}
