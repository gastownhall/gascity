package bdflags

import "testing"

// A command substitution embeds a NEW command; its flags must not be
// attributed to the bd invocation that carries it. Measured false positive
// 2026-08-24: `gc bd create ... --metadata "$(jq -cn \` reported -cn as a
// bd create flag.
func TestScanIgnoresFlagsInsideCommandSubstitution(t *testing.T) {
	src := []byte(`gc bd create "<title>" -d "<the order>" --metadata "$(jq -cn \`)
	if fs := ScanUnknownFlags(src); len(fs) != 0 {
		t.Fatalf("expected no findings, got %+v", fs)
	}
}

func TestScanStillSeesOuterFlagsAfterClosedSubstitution(t *testing.T) {
	src := []byte(`gc bd create t --metadata "$(jq -cn --arg a b '{}')" --bogus-flag x`)
	fs := ScanUnknownFlags(src)
	if len(fs) != 1 || fs[0].Flag != "--bogus-flag" {
		t.Fatalf("expected exactly --bogus-flag, got %+v", fs)
	}
}

func TestScanSingleQuotesSuppressSubstitution(t *testing.T) {
	// inside single quotes $( is literal text, not a substitution
	src := []byte(`bd create t -d 'literal $(not a command)' --bogus-flag`)
	fs := ScanUnknownFlags(src)
	if len(fs) != 1 || fs[0].Flag != "--bogus-flag" {
		t.Fatalf("expected exactly --bogus-flag, got %+v", fs)
	}
}
