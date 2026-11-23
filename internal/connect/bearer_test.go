package connect

import (
	"strings"
	"testing"
)

// testKey is bytes 0x00..0x1f. The golden values below were computed with an
// independent HMAC-SHA256 implementation (Python's hmac module), not with
// this package.
func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func mustMint(t *testing.T, city, session string, generation int) string {
	t.Helper()
	bearer, err := MintBearer(testKey(), city, session, generation)
	if err != nil {
		t.Fatalf("MintBearer(%q, %q, %d): %v", city, session, generation, err)
	}
	return bearer
}

func TestMintBearerGoldenVectors(t *testing.T) {
	cases := []struct {
		city, session string
		generation    int
		want          string
	}{
		{"maintainer-city", "mc-abc12", 7, "gcb1.mc-abc12.7.SEK9t2fJKbuzrMRfG96gsilYcYlzG7whL-4QChJGrzE"},
		{"a", "bc", 1, "gcb1.bc.1.YAUmvMRQ10M9YR2rgM2jVQOkY-Xca5A8_18tpDJM4jU"},
		{"ab", "c", 1, "gcb1.c.1.YGK87jqp-irHtBFheJcasUtivKKXWrMqrnRhWCvx-Sw"},
		{"maintainer-city", "mc.abc.12", 130, "gcb1.mc.abc.12.130.sqwVbiqNWx66KFhVCwA00YFZhLTUuUhkTL2hD13Ra0Y"},
		// A city longer than 127 bytes exercises a two-byte uvarint prefix.
		{strings.Repeat("x", 200), "mc-abc12", 7, "gcb1.mc-abc12.7.GF3cBmMFhyUXRPHBIRTmTzbHJ217_1X5bcbbJ5j5vxQ"},
	}
	for _, tc := range cases {
		if got := mustMint(t, tc.city, tc.session, tc.generation); got != tc.want {
			t.Errorf("MintBearer(%q, %q, %d) = %q, want %q", tc.city, tc.session, tc.generation, got, tc.want)
		}
	}
}

func mustSentinel(t *testing.T, key []byte, city string) string {
	t.Helper()
	s, err := Sentinel(key, city)
	if err != nil {
		t.Fatalf("Sentinel(%q): %v", city, err)
	}
	return s
}

func TestSentinelGoldenVectors(t *testing.T) {
	cases := []struct{ city, want string }{
		{"maintainer-city", "gcs1.x5f4iebvW3fmymjWGE5g9uG-H6FDn7W7LdrsnfWCTGU"},
		{strings.Repeat("x", 200), "gcs1.ccqAi4oJqzACqcmnyq6MFI1lMAhxIR2hWcoOcxsiBZ8"},
	}
	for _, tc := range cases {
		if got := mustSentinel(t, testKey(), tc.city); got != tc.want {
			t.Errorf("Sentinel(%q) = %q, want %q", tc.city, got, tc.want)
		}
	}
}

func TestBearerSeparatorAmbiguityPairsDiffer(t *testing.T) {
	macOf := func(bearer string) string { return bearer[strings.LastIndexByte(bearer, '.')+1:] }
	pairs := []struct {
		name   string
		cityA  string
		sessA  string
		genA   int
		cityB  string
		sessB  string
		genB   int
		sameID bool
	}{
		{name: "city/session boundary", cityA: "a", sessA: "bc", genA: 1, cityB: "ab", sessB: "c", genB: 1},
		{name: "session/generation boundary", cityA: "c", sessA: "s1", genA: 2, cityB: "c", sessB: "s", genB: 12},
		{name: "city differs, same session", cityA: "city-a", sessA: "s", genA: 1, cityB: "city-b", sessB: "s", genB: 1, sameID: true},
	}
	for _, p := range pairs {
		a := mustMint(t, p.cityA, p.sessA, p.genA)
		b := mustMint(t, p.cityB, p.sessB, p.genB)
		if macOf(a) == macOf(b) {
			t.Errorf("%s: MACs collide: %q and %q", p.name, a, b)
		}
		if p.sameID && strings.TrimSuffix(a, macOf(a)) != strings.TrimSuffix(b, macOf(b)) {
			t.Errorf("%s: plaintext parts differ unexpectedly: %q vs %q", p.name, a, b)
		}
	}
}

func TestSentinelIsCityScopedAndDisjointFromBearers(t *testing.T) {
	if mustSentinel(t, testKey(), "a") == mustSentinel(t, testKey(), "b") {
		t.Fatal("sentinels for different cities collide")
	}
	other := testKey()
	other[0] ^= 1
	if mustSentinel(t, testKey(), "a") == mustSentinel(t, other, "a") {
		t.Fatal("sentinels under different keys collide")
	}
	s := mustSentinel(t, testKey(), "a")
	if !strings.HasPrefix(s, SentinelPrefix) || len(s) != len(SentinelPrefix)+macLen {
		t.Fatalf("sentinel %q has the wrong shape", s)
	}
	if _, _, ok := ParseBearer(s); ok {
		t.Fatalf("ParseBearer accepted a sentinel %q", s)
	}
}

func TestBearerAndSentinelHaveNoWhitespaceOrHash(t *testing.T) {
	values := []string{
		mustMint(t, "maintainer-city", "mc-abc12", 7),
		mustMint(t, "city with spaces # and hash", "A.b_c-9", 1<<40),
		mustSentinel(t, testKey(), "city with spaces # and hash"),
	}
	for _, v := range values {
		if strings.ContainsAny(v, " \t\r\n#") {
			t.Errorf("%q contains whitespace or '#'", v)
		}
	}
}

func TestMintBearerMACLength(t *testing.T) {
	b := mustMint(t, "maintainer-city", "mc-abc12", 7)
	mac := b[strings.LastIndexByte(b, '.')+1:]
	if len(mac) != 43 {
		t.Fatalf("mac %q has length %d, want 43", mac, len(mac))
	}
}

func TestMintBearerRefusals(t *testing.T) {
	cases := []struct {
		name    string
		key     []byte
		city    string
		session string
		gen     int
	}{
		{"empty key", nil, "c", "s", 1},
		{"one-byte key", []byte{1}, "c", "s", 1},
		{"31-byte key", testKey()[:MinKeyLen-1], "c", "s", 1},
		{"empty city", testKey(), "", "s", 1},
		{"empty session", testKey(), "c", "", 1},
		{"zero generation", testKey(), "c", "s", 0},
		{"negative generation", testKey(), "c", "s", -1},
		{"leading dot", testKey(), "c", ".s", 1},
		{"leading dash", testKey(), "c", "-s", 1},
		{"leading underscore", testKey(), "c", "_s", 1},
		{"space", testKey(), "c", "a b", 1},
		{"hash", testKey(), "c", "a#b", 1},
		{"slash", testKey(), "c", "a/b", 1},
		{"colon", testKey(), "c", "a:b", 1},
		{"newline", testKey(), "c", "a\nb", 1},
		{"non-ascii", testKey(), "c", "aé", 1},
		{"too long", testKey(), "c", "a" + strings.Repeat("b", 128), 1},
	}
	for _, tc := range cases {
		if got, err := MintBearer(tc.key, tc.city, tc.session, tc.gen); err == nil {
			t.Errorf("%s: MintBearer = %q, want an error", tc.name, got)
		}
	}
	if _, err := MintBearer(testKey(), "c", "a"+strings.Repeat("b", 127), 1); err != nil {
		t.Errorf("128-character session id refused: %v", err)
	}
}

func TestParseBearerRoundTripsDottedIDs(t *testing.T) {
	for _, tc := range []struct {
		session string
		gen     int
	}{
		{"mc-abc12", 7},
		{"mc.abc.12", 130},
		{"a.1", 2},
		{"a..b", 1},
		{"a.", 9},
		{"Z", 1 << 40},
	} {
		bearer := mustMint(t, "maintainer-city", tc.session, tc.gen)
		id, gen, ok := ParseBearer(bearer)
		if !ok || id != tc.session || gen != tc.gen {
			t.Errorf("ParseBearer(%q) = (%q, %d, %v), want (%q, %d, true)", bearer, id, gen, ok, tc.session, tc.gen)
		}
	}
}

func TestParseBearerRefusals(t *testing.T) {
	mac := "SEK9t2fJKbuzrMRfG96gsilYcYlzG7whL-4QChJGrzE"
	cases := map[string]string{
		"empty":               "",
		"no prefix":           "mc-abc12.7." + mac,
		"wrong prefix":        "gcb2.mc-abc12.7." + mac,
		"sentinel prefix":     "gcs1.mc-abc12.7." + mac,
		"no generation":       "gcb1.mc-abc12." + mac,
		"no session":          "gcb1.7." + mac,
		"empty session":       "gcb1..7." + mac,
		"zero generation":     "gcb1.mc-abc12.0." + mac,
		"leading-zero gen":    "gcb1.mc-abc12.07." + mac,
		"signed generation":   "gcb1.mc-abc12.+7." + mac,
		"negative generation": "gcb1.mc-abc12.-7." + mac,
		"overflow generation": "gcb1.mc-abc12.99999999999999999999." + mac,
		"short mac":           "gcb1.mc-abc12.7." + mac[:42],
		"long mac":            "gcb1.mc-abc12.7." + mac + "A",
		"padded mac":          "gcb1.mc-abc12.7." + mac[:42] + "=",
		"std alphabet mac":    "gcb1.mc-abc12.7." + strings.ReplaceAll(mac, "-", "+"),
		"non-canonical mac":   "gcb1.mc-abc12.7." + mac[:42] + "F",
		"space in session":    "gcb1.mc abc.7." + mac,
		"hash in session":     "gcb1.mc#abc.7." + mac,
		"leading dot session": "gcb1..mc.7." + mac,
		"trailing newline":    "gcb1.mc-abc12.7." + mac + "\n",
		"session too long":    "gcb1.a" + strings.Repeat("b", 128) + ".7." + mac,
	}
	for name, input := range cases {
		if id, gen, ok := ParseBearer(input); ok {
			t.Errorf("%s: ParseBearer(%q) = (%q, %d, true), want refusal", name, input, id, gen)
		}
	}
}

func TestSentinelRefusesWeakKeyAndEmptyCity(t *testing.T) {
	cases := []struct {
		name string
		key  []byte
		city string
	}{
		{"nil key", nil, "c"},
		{"empty key", []byte{}, "c"},
		{"31-byte key", testKey()[:MinKeyLen-1], "c"},
		{"empty city", testKey(), ""},
	}
	for _, tc := range cases {
		if got, err := Sentinel(tc.key, tc.city); err == nil {
			t.Errorf("%s: Sentinel = %q, want an error", tc.name, got)
		}
	}
}

func TestMinKeyLenIsD7Minimum(t *testing.T) {
	if MinKeyLen != 32 {
		t.Fatalf("MinKeyLen = %d, want D7's 32 bytes", MinKeyLen)
	}
	if _, err := MintBearer(testKey()[:MinKeyLen], "c", "s", 1); err != nil {
		t.Fatalf("MintBearer refused a %d-byte key: %v", MinKeyLen, err)
	}
}
