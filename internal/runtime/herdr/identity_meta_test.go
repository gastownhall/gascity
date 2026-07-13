package herdr

import "testing"

// TestStampIdentityMeta pins the start-time identity stamp: the reconciler
// binds a live runtime to its session bead by probing GC_SESSION_ID /
// GC_INSTANCE_TOKEN / GC_RUNTIME_EPOCH via GetMeta, so Start must make them
// readable the moment the agent exists — not after the (up to a minute)
// startup delivery completes. An empty or missing env key stays unstamped.
func TestStampIdentityMeta(t *testing.T) {
	p := New("gctest-identity-meta", t.TempDir(), t.TempDir(), 0)
	p.stampIdentityMeta("canary", map[string]string{
		"GC_SESSION_ID":     "gm-abc123",
		"GC_INSTANCE_TOKEN": "tok-1",
		"GC_RUNTIME_EPOCH":  "3",
		"GC_CITY":           "not-an-identity-key",
	})
	for key, want := range map[string]string{
		"GC_SESSION_ID":     "gm-abc123",
		"GC_INSTANCE_TOKEN": "tok-1",
		"GC_RUNTIME_EPOCH":  "3",
		"GC_CITY":           "",
	} {
		got, err := p.GetMeta("canary", key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("GetMeta(%s) = %q, want %q", key, got, want)
		}
	}

	p.stampIdentityMeta("empty", map[string]string{"GC_SESSION_ID": ""})
	if got, _ := p.GetMeta("empty", "GC_SESSION_ID"); got != "" {
		t.Errorf("empty env value stamped as %q, want unstamped", got)
	}
}
