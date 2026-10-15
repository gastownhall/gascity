package opjournal

import (
	"errors"
	"strings"
	"testing"
)

// Golden vectors, computed independently (Python hashlib + base64).
func TestDeriveIDGoldenVectors(t *testing.T) {
	cases := []struct{ prefix, conv, principal, key, want string }{
		{"gc", "gc-1", "operator:alice", "k1", "gc-op-kdpftlql5feaypldytje3nypwy"},
		// Boundary pair: the same bytes split differently across fields.
		{"gc", "gc-1", "ab", "c", "gc-op-rpbncs2z6pdl7okrqjcd6vgzz4"},
		{"gc", "gc-1a", "b", "c", "gc-op-ckefefduzvsuhqu4yp37e62oxe"},
		{"gc", "gc-1", "op:a", "bc", "gc-op-fj57uteqwiavk5fgivrtwfq4ci"},
		{"gc", "gc-1", "op:ab", "c", "gc-op-eiyxg4k3cxhwwvlfqssixwtmbi"},
		{"my-city", "my-city-a3f", "t3:env1/user:u1", "msg-123", "my-city-op-dtiaajrf6w4om4wkkivqjoonie"},
		// Migrated row: the prefix is the store's, not the conversation's.
		{"gcs", "gc-a3f8e", "order:ci-watch", "slot:2026-10-13T14:00:00Z", "gcs-op-iqifa2qdqp2icseywmk4xa572i"},
		{"gc", "gc-1", "operator:alice", "stop:7:run-9:2", "gc-op-ygkwx2lvlsdrq7bnvlmgvytouu"},
	}
	for _, c := range cases {
		got, err := DeriveID(c.prefix, c.conv, c.principal, c.key)
		if err != nil {
			t.Fatalf("DeriveID(%q,%q,%q,%q): %v", c.prefix, c.conv, c.principal, c.key, err)
		}
		if got != c.want {
			t.Errorf("DeriveID(%q,%q,%q,%q) = %s, want %s", c.prefix, c.conv, c.principal, c.key, got, c.want)
		}
		again, _ := DeriveID(c.prefix, c.conv, c.principal, c.key)
		if again != got {
			t.Errorf("DeriveID is not stable: %s then %s", got, again)
		}
		if !validOpID(got) {
			t.Errorf("%s does not match the op id shape", got)
		}
	}
}

// The separator-ambiguity pair the brief names: ('a','bc') vs ('ab','c').
func TestDeriveIDSeparatorAmbiguity(t *testing.T) {
	a, _ := DeriveID("gc", "gc-1", "a", "bc")
	b, _ := DeriveID("gc", "gc-1", "ab", "c")
	if a == b {
		t.Fatalf("('a','bc') and ('ab','c') collide: %s", a)
	}
}

func TestDeriveIDPrefixOnlyChangesThePrefix(t *testing.T) {
	a, _ := DeriveID("gc", "gc-1", "operator:alice", "k1")
	b, _ := DeriveID("other", "gc-1", "operator:alice", "k1")
	if strings.TrimPrefix(a, "gc") != strings.TrimPrefix(b, "other") {
		t.Fatalf("hash suffix moved with the prefix: %s vs %s", a, b)
	}
}

func TestDeriveIDRefusesNonCanonicalInput(t *testing.T) {
	long := strings.Repeat("x", 513)
	cases := []struct{ name, prefix, conv, principal, key string }{
		{"empty prefix", "", "gc-1", "p", "k"},
		{"prefix trailing dash", "gc-", "gc-1", "p", "k"},
		{"prefix space", "g c", "gc-1", "p", "k"},
		{"prefix hash", "g#c", "gc-1", "p", "k"},
		{"prefix dot", "g.c", "gc-1", "p", "k"},
		{"empty conversation", "gc", "", "p", "k"},
		{"conversation space", "gc", "gc 1", "p", "k"},
		{"conversation at", "gc", "gc@1", "p", "k"},
		{"conversation hash", "gc", "gc#1", "p", "k"},
		{"conversation comma", "gc", "gc,1", "p", "k"},
		{"conversation quote", "gc", "gc\"1", "p", "k"},
		{"conversation newline", "gc", "gc\n1", "p", "k"},
		{"conversation too long", "gc", strings.Repeat("c", 201), "p", "k"},
		{"empty principal", "gc", "gc-1", "", "k"},
		{"principal leading space", "gc", "gc-1", " p", "k"},
		{"principal control", "gc", "gc-1", "p\x01", "k"},
		{"principal bad utf8", "gc", "gc-1", "p\xff", "k"},
		{"principal too long", "gc", "gc-1", strings.Repeat("p", 257), "k"},
		{"empty key", "gc", "gc-1", "p", ""},
		{"key trailing space", "gc", "gc-1", "p", "k "},
		{"key too long", "gc", "gc-1", "p", long},
	}
	for _, c := range cases {
		if _, err := DeriveID(c.prefix, c.conv, c.principal, c.key); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: DeriveID = %v, want ErrInvalidRequest", c.name, err)
		}
	}
}

func TestTargetKeys(t *testing.T) {
	// A key at the limit is one DeriveID accepts.
	for _, fn := range []func() (string, error){
		func() (string, error) { return InterruptKey(1, strings.Repeat("t", maxClientKeyLen-len("stop:1:"))) },
		func() (string, error) { return RespondKey(1, strings.Repeat("r", maxClientKeyLen-len("respond:1:"))) },
	} {
		k, err := fn()
		if err != nil {
			t.Fatalf("key at the limit: %v", err)
		}
		if _, err := DeriveID("gc", "gc-1", "p", k); err != nil {
			t.Fatalf("DeriveID refused a %d-byte key the helper returned: %v", len(k), err)
		}
	}
	if k, err := InterruptKey(7, "run-9:2"); err != nil || k != "stop:7:run-9:2" {
		t.Errorf("InterruptKey(7, run-9:2) = %q, %v", k, err)
	}
	if k, err := RespondKey(7, "req-1"); err != nil || k != "respond:7:req-1" {
		t.Errorf("RespondKey(7, req-1) = %q, %v", k, err)
	}
	for _, c := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"interrupt gen 0", func() (string, error) { return InterruptKey(0, "t") }},
		{"interrupt no turn", func() (string, error) { return InterruptKey(1, "") }},
		{"respond gen -1", func() (string, error) { return RespondKey(-1, "r") }},
		{"respond no request", func() (string, error) { return RespondKey(1, "") }},
		{"interrupt key too long", func() (string, error) { return InterruptKey(1, strings.Repeat("t", maxClientKeyLen-len("stop:1:")+1)) }},
		{"respond key too long", func() (string, error) { return RespondKey(1, strings.Repeat("r", maxClientKeyLen-len("respond:1:")+1)) }},
	} {
		if _, err := c.fn(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", c.name, err)
		}
	}
}
