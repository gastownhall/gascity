package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestNativeTransportParseAndDefault covers decode and the accessor default
// for beads.native_transport, mirroring TestConditionalWritesParseAndDefault
// / TestGuardedReleaseParseAndValidate — except this switch's unset default
// is "auto" (today's eligibility-gated behavior), not "off".
func TestNativeTransportParseAndDefault(t *testing.T) {
	// zero value / omitted -> default "auto".
	if (BeadsConfig{}).NormalizedNativeTransport() != "auto" {
		t.Fatalf("zero-value accessor = %q, want auto", (BeadsConfig{}).NormalizedNativeTransport())
	}
	// an explicit "off" decodes.
	out, err := Parse([]byte("[beads]\nnative_transport = \"off\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.Beads.NativeTransport != "off" {
		t.Fatalf("decoded native_transport = %q, want off", out.Beads.NativeTransport)
	}
	if got := out.Beads.NormalizedNativeTransport(); got != "off" {
		t.Fatalf("NormalizedNativeTransport = %q, want off", got)
	}
	// an explicit "auto" decodes.
	out2, err := Parse([]byte("[beads]\nnative_transport = \"auto\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out2.Beads.NativeTransport != "auto" {
		t.Fatalf("decoded native_transport = %q, want auto", out2.Beads.NativeTransport)
	}
	// unset decodes to empty, normalizing to auto.
	out3, err := Parse([]byte("[workspace]\nname = \"t\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out3.Beads.NativeTransport != "" || out3.Beads.NormalizedNativeTransport() != "auto" {
		t.Fatalf("unset native_transport = %q (norm %q), want empty->auto",
			out3.Beads.NativeTransport, out3.Beads.NormalizedNativeTransport())
	}
}

// TestNativeTransportRejectsOutOfEnum proves a typo, and the third
// gate.ParseMode spelling "require" (which belongs to conditional_writes /
// guarded_release, not this two-valued switch), both fail config load rather
// than silently resolving to a mode.
func TestNativeTransportRejectsOutOfEnum(t *testing.T) {
	for _, raw := range []string{"requre", "bogus", "true", "1", "Off ", " AUTO"} {
		t.Run(raw, func(t *testing.T) {
			switch raw {
			case "Off ", " AUTO":
				// case/space-tolerant valid spellings must still decode, AND
				// NormalizedNativeTransport must fold them to the canonical
				// lowercase form — not just gate.ParseMode's validation at
				// load time. A consumer that compares the raw string (as
				// resolvedNativeTransportMode does) against the lowercase
				// beads.NativeTransportOff constant would otherwise silently
				// treat "Off " as native-eligible.
				out, err := Parse([]byte("[beads]\nnative_transport = \"" + raw + "\"\n"))
				if err != nil {
					t.Fatalf("Parse(%q): unexpected error: %v", raw, err)
				}
				want := strings.ToLower(strings.TrimSpace(raw))
				if got := out.Beads.NormalizedNativeTransport(); got != want {
					t.Fatalf("NormalizedNativeTransport() for raw %q = %q, want %q", raw, got, want)
				}
				return
			}
			if _, err := Parse([]byte("[beads]\nnative_transport = \"" + raw + "\"\n")); err == nil {
				t.Fatalf("expected an error for an out-of-enum native_transport value %q", raw)
			}
		})
	}
}

// TestNativeTransportNormalizesCaseAndWhitespace proves every mixed-case and
// whitespace-padded spelling gate.ParseMode accepts at load time also
// resolves, through NormalizedNativeTransport, to the exact lowercase
// constant every runtime consumer compares against
// (resolvedNativeTransportMode casts this string straight to
// beads.NativeTransportMode with no further parsing). Before this test
// (and the fix it pins), "OFF"/"Off"/" off " all passed validateNativeTransport
// but NormalizedNativeTransport returned them verbatim, so the city went
// native instead of honoring the kill switch.
func TestNativeTransportNormalizesCaseAndWhitespace(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		// viaTOML is false for a raw value TOML's own grammar cannot carry
		// through a quoted basic string (a literal tab/newline is a TOML
		// syntax error, not a native_transport concern) — those cases only
		// exercise the direct accessor, which a caller could still reach by
		// building a BeadsConfig without going through Parse.
		viaTOML bool
	}{
		{raw: "off", want: "off", viaTOML: true},
		{raw: "OFF", want: "off", viaTOML: true},
		{raw: "Off", want: "off", viaTOML: true},
		{raw: " off ", want: "off", viaTOML: true},
		{raw: "\toff\n", want: "off", viaTOML: false},
		{raw: "auto", want: "auto", viaTOML: true},
		{raw: "AUTO", want: "auto", viaTOML: true},
		{raw: "Auto", want: "auto", viaTOML: true},
		{raw: " auto ", want: "auto", viaTOML: true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if tc.viaTOML {
				out, err := Parse([]byte("[beads]\nnative_transport = \"" + tc.raw + "\"\n"))
				if err != nil {
					t.Fatalf("Parse(%q): unexpected error: %v", tc.raw, err)
				}
				if got := out.Beads.NormalizedNativeTransport(); got != tc.want {
					t.Fatalf("NormalizedNativeTransport() for raw %q = %q, want %q", tc.raw, got, tc.want)
				}
			}
			// The direct accessor (not Parse's decode path) must normalize
			// identically, since every caller — including one that builds a
			// BeadsConfig without going through Parse (e.g. a test fixture) —
			// uses this method, never the raw field.
			if got := (BeadsConfig{NativeTransport: tc.raw}).NormalizedNativeTransport(); got != tc.want {
				t.Fatalf("BeadsConfig{NativeTransport: %q}.NormalizedNativeTransport() = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNativeTransportRejectedOnTheRealComposedLoadPath proves the enum
// validation Parse applies also applies to LoadWithIncludesOptions — the
// composed-root path every real `gc` invocation actually uses (loadCityConfig
// -> loadCityConfigFS -> LoadWithIncludesOptions). Before this test (and the
// fix it pins), LoadWithIncludesOptions decoded the root layer with
// parseWithMeta directly and never called validateNativeTransport /
// validateConditionalWrites / validateGuardedRelease on the result: a real
// city.toml with an out-of-enum beads.native_transport value loaded with no
// error at all, so the kill switch's validation existed only for
// config.Parse's direct callers (mostly tests), not for cities loaded for
// real.
func TestNativeTransportRejectedOnTheRealComposedLoadPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "t"

[beads]
native_transport = "bogus"
`)
	cityPath := filepath.Join(dir, "city.toml")
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, cityPath, LoadOptions{}); err == nil {
		t.Fatal("LoadWithIncludesOptions: want an error for an out-of-enum native_transport, got nil")
	}
}

// TestNativeTransportRejectsFragmentSuppliedOutOfEnum proves the composed-root
// validation also covers a value an included fragment supplies: fragments are
// decoded without Parse, so an out-of-enum native_transport there must fail
// the load rather than silently override the root's valid "off".
func TestNativeTransportRejectsFragmentSuppliedOutOfEnum(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
native_transport = "off"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
native_transport = "bogus"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes: want an error for a fragment-supplied out-of-enum native_transport, got nil")
	}
	if !strings.Contains(err.Error(), "beads.native_transport") {
		t.Fatalf("LoadWithIncludes error = %v, want the beads.native_transport validation error", err)
	}
}

// TestNativeTransportRejectsRequire proves "require" specifically is refused:
// it is a valid gate.Mode spelling (shared with conditional_writes /
// guarded_release) but is NOT part of native_transport's off|auto grammar.
func TestNativeTransportRejectsRequire(t *testing.T) {
	if _, err := Parse([]byte("[beads]\nnative_transport = \"require\"\n")); err == nil {
		t.Fatalf("expected an error for native_transport = \"require\"")
	}
}

// TestNativeTransportSurvivesBeadsFragment is the load-bearing regression for
// the kill switch under config layering: an included fragment that defines
// ONLY an unrelated [beads] sibling key must NOT reset the root's explicit
// native_transport = "off". mergeFragment overwrites base.Beads wholesale when
// a fragment defines [beads]; without the per-field IsDefined preservation the
// switch composes to "" — which passes validation and normalizes to "auto" —
// so the city silently opens natively despite the operator's "off".
func TestNativeTransportSurvivesBeadsFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
native_transport = "off"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
bd_compatibility = "bd-1.0.5"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedNativeTransport(); got != "off" {
		t.Fatalf("NormalizedNativeTransport = %q, want root off to survive a [beads] fragment", got)
	}
	if got := cfg.Beads.NormalizedBDCompatibility(); got != "bd-1.0.5" {
		t.Fatalf("BDCompatibility = %q, want the fragment's bd-1.0.5", got)
	}
}

// TestNativeTransportFragmentOverridesRoot is the companion to the
// preservation test: a fragment that DOES set native_transport must win
// (LWW), so the preservation branch can't drift into "base value always wins."
func TestNativeTransportFragmentOverridesRoot(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
native_transport = "off"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
native_transport = "auto"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedNativeTransport(); got != "auto" {
		t.Fatalf("NormalizedNativeTransport = %q, want the fragment's auto to win", got)
	}
}
