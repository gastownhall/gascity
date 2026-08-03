package exec //nolint:revive // internal package, always imported with alias

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStoreIDPrefixFromEnv verifies the exec store exposes its scope prefix from
// the projected GC_BEADS_PREFIX, including whitespace trimming and empty/nil env.
func TestStoreIDPrefixFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "set", env: map[string]string{"GC_BEADS_PREFIX": "tr"}, want: "tr"},
		{name: "trims whitespace", env: map[string]string{"GC_BEADS_PREFIX": "  tr\n"}, want: "tr"},
		{name: "empty value", env: map[string]string{"GC_BEADS_PREFIX": ""}, want: ""},
		{name: "absent key", env: map[string]string{"GC_CITY": "x"}, want: ""},
		{name: "nil env", env: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore("beads-provider")
			s.SetEnv(tc.env)
			if got := s.IDPrefix(); got != tc.want {
				t.Fatalf("IDPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStoreIDPrefixNilReceiver guards the nil-receiver path.
func TestStoreIDPrefixNilReceiver(t *testing.T) {
	var s *Store
	if got := s.IDPrefix(); got != "" {
		t.Fatalf("nil Store IDPrefix() = %q, want empty", got)
	}
}

// TestCachingStoreDerivesPrefixFromExecStore is the regression this fix exists
// for: NewCachingStore must pick up an exec-backed store's scope prefix via the
// optional IDPrefix() capability, so a rig-scoped cache is keyed by prefix
// rather than "(no-prefix)".
func TestCachingStoreDerivesPrefixFromExecStore(t *testing.T) {
	s := NewStore("beads-provider")
	s.SetEnv(map[string]string{"GC_BEADS_PREFIX": "tr"})

	cache := beads.NewCachingStore(s, nil)
	if got := cache.IDPrefix(); got != "tr" {
		t.Fatalf("NewCachingStore(execStore).IDPrefix() = %q, want %q", got, "tr")
	}
}
