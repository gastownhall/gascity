package config

import "testing"

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
				// case/space-tolerant valid spellings must still decode.
				if _, err := Parse([]byte("[beads]\nnative_transport = \"" + raw + "\"\n")); err != nil {
					t.Fatalf("Parse(%q): unexpected error: %v", raw, err)
				}
				return
			}
			if _, err := Parse([]byte("[beads]\nnative_transport = \"" + raw + "\"\n")); err == nil {
				t.Fatalf("expected an error for an out-of-enum native_transport value %q", raw)
			}
		})
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
