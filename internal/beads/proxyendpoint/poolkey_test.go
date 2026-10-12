package proxyendpoint

import (
	"strings"
	"testing"
)

func TestNewPoolKeyCarriesTheWholeIdentity(t *testing.T) {
	rec := Record{PID: 4242, Port: 45123, Birth: BirthToken("boot-1", "99887766"), RootID: "aabbccddeeff00112233"}
	key := NewPoolKey(rec, "beads")
	want := PoolKey{RootID: rec.RootID, PID: rec.PID, Birth: rec.Birth, Port: rec.Port, Database: "beads"}
	if key != want {
		t.Fatalf("NewPoolKey = %+v, want %+v", key, want)
	}
}

// TestPoolKeyDistinguishesWhatThePortCannot is the reason the key exists. Each
// case is a real shape: a respawn onto a pinned port, a root recreated at the
// same path, and a rig sharing its city's proxy with its own database.
func TestPoolKeyDistinguishesWhatThePortCannot(t *testing.T) {
	base := PoolKey{RootID: "root-a", PID: 100, Birth: "linux-v1:boot:111", Port: 45123, Database: "beads"}

	cases := []struct {
		name           string
		other          PoolKey
		wantEqual      bool
		wantGeneration bool
	}{
		{
			name:           "itself",
			other:          base,
			wantEqual:      true,
			wantGeneration: true,
		},
		{
			// A sidecar that pins --proxied-server-port gets the same port back
			// for a different process. The port alone would call this the same
			// pool.
			name:  "a respawn onto the same pinned port",
			other: PoolKey{RootID: "root-a", PID: 200, Birth: "linux-v1:boot:222", Port: 45123, Database: "beads"},
		},
		{
			// The same PID number after a reboot, which resets start times.
			name:  "the same pid born at another time",
			other: PoolKey{RootID: "root-a", PID: 100, Birth: "linux-v1:boot:999", Port: 45123, Database: "beads"},
		},
		{
			// `mv rigs/a rigs/a.old` and a fresh bd command in a recreated
			// rigs/a: the path identity repeats, the generation does not.
			name:  "a root recreated at the same path",
			other: PoolKey{RootID: "root-a", PID: 300, Birth: "linux-v1:boot:333", Port: 45999, Database: "beads"},
		},
		{
			// The migrate-proxied shape: one proxy, two scopes, two databases.
			// Same generation, different pool.
			name:           "a rig sharing its city's proxy",
			other:          PoolKey{RootID: "root-a", PID: 100, Birth: "linux-v1:boot:111", Port: 45123, Database: "rig"},
			wantGeneration: true,
		},
		{
			name:  "another root entirely",
			other: PoolKey{RootID: "root-b", PID: 100, Birth: "linux-v1:boot:111", Port: 45123, Database: "beads"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := base == tc.other; got != tc.wantEqual {
				t.Errorf("key equality = %v, want %v", got, tc.wantEqual)
			}
			if got := base.SameGeneration(tc.other); got != tc.wantGeneration {
				t.Errorf("SameGeneration = %v, want %v", got, tc.wantGeneration)
			}
		})
	}
}

// TestPoolKeyRenderingHidesTheBootID pins that no rendering leaks the boot id
// the birth token embeds, and that the fingerprint still separates two
// generations — the two properties that make the rendering safe AND useful.
func TestPoolKeyRenderingHidesTheBootID(t *testing.T) {
	const bootID = "5f0b9c1e-aaaa-bbbb-cccc-0123456789ab"
	birth := BirthToken(bootID, "99887766")
	key := PoolKey{RootID: "0123456789abcdef0123", PID: 4242, Birth: birth, Port: 45123, Database: "beads"}

	for _, rendered := range []string{key.String(), key.Generation()} {
		if strings.Contains(rendered, bootID) {
			t.Fatalf("%q leaks the host's boot id", rendered)
		}
		if strings.Contains(rendered, BirthTokenPrefix) {
			t.Fatalf("%q spends its budget on the token's constant prefix rather than the part that differs", rendered)
		}
	}

	// Same host, later process: only the start time differs, which is exactly
	// the case a truncated token would have rendered identically.
	sibling := key
	sibling.Birth = BirthToken(bootID, "99887799")
	if key.Generation() == sibling.Generation() {
		t.Fatal("two generations on one host rendered the same fingerprint")
	}

	want := "root=0123456789ab gen=" + key.Generation() + " port=45123 db=beads"
	if got := key.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestShortDigest(t *testing.T) {
	if got := ShortDigest(""); got != "" {
		t.Errorf("ShortDigest(%q) = %q, want empty", "", got)
	}
	if got := ShortDigest("   "); got != "" {
		t.Errorf("ShortDigest of blank = %q, want empty", got)
	}
	digest := ShortDigest("linux-v1:boot:1")
	if len(digest) != 12 {
		t.Fatalf("ShortDigest = %q, want 12 hex characters", digest)
	}
	if ShortDigest("linux-v1:boot:1") != digest {
		t.Fatal("ShortDigest is not stable across calls")
	}
	if ShortDigest("linux-v1:boot:2") == digest {
		t.Fatal("two tokens produced one digest")
	}
}

func TestShortID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"0123456789ab", "0123456789ab"},
		{"0123456789abcdef", "0123456789ab"},
	}
	for _, tc := range cases {
		if got := ShortID(tc.in); got != tc.want {
			t.Errorf("ShortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
