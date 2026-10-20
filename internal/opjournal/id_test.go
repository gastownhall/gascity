package opjournal

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
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

// PinnedID is the scheme DeriveID uses: the same tag and fields give the same
// hash under another family.
func TestPinnedIDIsTheDeriveIDScheme(t *testing.T) {
	op, err := DeriveID("gc", "gc-1", "operator:alice", "k1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := PinnedID("gc", "x", opIDTag, "gc-1", "operator:alice", "k1")
	if err != nil {
		t.Fatalf("PinnedID: %v", err)
	}
	if strings.TrimPrefix(got, "gc-x-") != strings.TrimPrefix(op, "gc-op-") {
		t.Fatalf("PinnedID hash %s differs from DeriveID's %s", got, op)
	}
}

// Golden vectors, computed independently (Python hashlib + base64).
func TestPinnedIDGoldenVectors(t *testing.T) {
	const origin = "proc:0123456789abcdef0123456789abcdef"
	cases := []struct {
		prefix, family, tag string
		fields              []string
		want                string
	}{
		{"gc", "ev", "gascity/event-fact/v1", []string{"gc-a3f8e", "3", origin, "1"}, "gc-ev-e6o7rior5skvdx2d5w33iaddaq"},
		{"gc", "ev", "gascity/event-fact/v1", []string{"gc-a3f8e", "3", origin, "2"}, "gc-ev-snridtrzpplc2mrpfenmqu4iyy"},
		// Boundary pair: the same bytes split differently across fields.
		{"gc", "ev", "gascity/event-fact/v1", []string{"gc-a3f8e", "31", "proc:x", "1"}, "gc-ev-e5ce2jdzkx5x4awsgn66mbab3q"},
		{"gc", "ev", "gascity/event-fact/v1", []string{"gc-a3f8e3", "1", "proc:x", "1"}, "gc-ev-ox7e57t77drwu6iphld4bgguuq"},
		{"my-city", "evproj", "gascity/event-projector/v1", []string{"my-city"}, "my-city-evproj-zrmdzquvb6wyt462o74fvejjda"},
	}
	for _, c := range cases {
		got, err := PinnedID(c.prefix, c.family, c.tag, c.fields...)
		if err != nil {
			t.Fatalf("PinnedID(%q, %q, %q, %q): %v", c.prefix, c.family, c.tag, c.fields, err)
		}
		if got != c.want {
			t.Errorf("PinnedID(%q, %q, %q, %q) = %s, want %s", c.prefix, c.family, c.tag, c.fields, got, c.want)
		}
	}
}

// IsPinnedID matches every id the pinned schemes mint, and no other bead id
// shape: a store's sequence or hash id, a child id, or a pinned id with its
// hash cut, widened or uppercased.
func TestIsPinnedID(t *testing.T) {
	op, err := DeriveID("gc", "gc-1", "operator", "k1")
	if err != nil {
		t.Fatal(err)
	}
	fact, err := FactID("my-city", Fact{Kind: FactTurnStarted, Conversation: "gc-1", Generation: 1, Turn: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := RuntimeRowID("gc", "gc-1")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := PinnedID("gc", "ev", "gascity/event-fact/v1", "gc-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{op, fact, rt, ev} {
		if !IsPinnedID(id) {
			t.Errorf("IsPinnedID(%q) = false, want true", id)
		}
	}
	hash := strings.TrimPrefix(op, "gc-op-")
	for _, id := range []string{
		"", "gc-12", "ga-j0aune", "gc-a3f8e.1", "gc-op-" + hash[:25], "gc-op-" + hash + "a",
		"gc-op-" + strings.ToUpper(hash), "-op-" + hash, "gc-" + hash, "gc-1op-" + hash,
	} {
		if IsPinnedID(id) {
			t.Errorf("IsPinnedID(%q) = true, want false", id)
		}
	}
}

func TestPinnedIDRefuses(t *testing.T) {
	cases := []struct{ name, prefix, family, tag string }{
		{"empty prefix", "", "ev", "t"},
		{"prefix trailing dash", "gc-", "ev", "t"},
		{"prefix dot", "g.c", "ev", "t"},
		{"empty family", "gc", "", "t"},
		{"family with dash", "gc", "e-v", "t"},
		{"family upper", "gc", "Ev", "t"},
		{"family digit first", "gc", "1ev", "t"},
		{"family too long", "gc", strings.Repeat("e", 17), "t"},
		{"op family", "gc", "op", "t"},
		{"fact family", "gc", "fact", "t"},
		{"rt family", "gc", "rt", "t"},
		{"empty tag", "gc", "ev", ""},
	}
	for _, c := range cases {
		if _, err := PinnedID(c.prefix, c.family, c.tag, "f"); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: PinnedID = %v, want ErrInvalidRequest", c.name, err)
		}
	}
}

// Every id infix this package mints is one of ownIDFamilies, so PinnedID
// refuses it to every other package. A new <name>IDInfix constant fails here
// until it is listed below, and listing it requires its family to be reserved.
func TestOwnIDFamiliesCoverEveryInfix(t *testing.T) {
	infixes := map[string]string{
		"opIDInfix":      opIDInfix,
		"factIDInfix":    factIDInfix,
		"runtimeIDInfix": runtimeIDInfix,
	}
	for name, infix := range infixes {
		family := strings.TrimSuffix(strings.TrimPrefix(infix, "-"), "-")
		if "-"+family+"-" != infix || !slices.Contains(ownIDFamilies, family) {
			t.Errorf("%s = %q is not -<family>- for a family in ownIDFamilies %v", name, infix, ownIDFamilies)
		}
		if _, err := PinnedID("gc", family, "t", "f"); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("PinnedID mints in %s's family %q: %v", name, family, err)
		}
	}
	for _, name := range declaredIDInfixConstants(t) {
		if _, ok := infixes[name]; !ok {
			t.Errorf("constant %s is not listed in this test: reserve its family in ownIDFamilies and list it", name)
		}
	}
}

// declaredIDInfixConstants returns the name of every package-level constant
// in this package's non-test files that ends in IDInfix.
func declaredIDInfixConstants(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	parsed := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		parsed++
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if strings.HasSuffix(name.Name, "IDInfix") {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	if len(names) < 3 {
		t.Fatalf("found %d IDInfix constants in %d files; the scan is broken (are the package sources in the test's data?)", len(names), parsed)
	}
	return names
}

// TestValidateConversation pins the exported conversation check's verdicts:
// it accepts well-formed ids up to the length limit and refuses every
// malformed one as ErrInvalidRequest. It is validateConversation, the check
// Propose applies, so the two cannot drift.
func TestValidateConversation(t *testing.T) {
	for _, conv := range []string{"gc-1", "gc-abc.def", strings.Repeat("a", maxConversationLen)} {
		if err := ValidateConversation(conv); err != nil {
			t.Errorf("ValidateConversation(%q) = %v, want nil", conv, err)
		}
	}
	for _, conv := range []string{"", "gc 1", "gc@1", "gc#1", "gc,1", "gc\"1", "gc\x011", strings.Repeat("a", maxConversationLen+1), "\xff"} {
		if err := ValidateConversation(conv); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ValidateConversation(%q) = %v, want ErrInvalidRequest", conv, err)
		}
	}
}

// TestMaxRefusedWhyLen pins the exported refused-reason budget to the token
// limit a close applies: refused:<why> with a why of MaxRefusedWhyLen bytes
// is a valid outcome reason, and one byte more is refused.
func TestMaxRefusedWhyLen(t *testing.T) {
	fits := Outcome{Arm: ArmFailed, Reason: RefusedReasonPrefix + strings.Repeat("x", MaxRefusedWhyLen)}
	if err := validateOutcome(fits, Cancel{}); err != nil {
		t.Fatalf("validateOutcome(a %d-byte why) = %v, want nil", MaxRefusedWhyLen, err)
	}
	over := Outcome{Arm: ArmFailed, Reason: RefusedReasonPrefix + strings.Repeat("x", MaxRefusedWhyLen+1)}
	if err := validateOutcome(over, Cancel{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("validateOutcome(a %d-byte why) = %v, want ErrInvalidRequest", MaxRefusedWhyLen+1, err)
	}
}
