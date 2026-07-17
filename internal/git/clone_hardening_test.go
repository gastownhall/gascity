package git

import (
	"errors"
	"testing"
)

// TestClassifyCloneSchemeFileAnySlashCount pins that the single-slash file:
// form is rejected as ErrSchemeFile (not misread as an scp/ssh remote), in every
// case, including when ssh is enabled.
func TestClassifyCloneSchemeFileAnySlashCount(t *testing.T) {
	for _, raw := range []string{"file:/etc/passwd", "FILE:/etc/x", "file:foo", "file:///etc/x", "file://host/x"} {
		if err := classifyCloneScheme(raw, false); !errors.Is(err, ErrSchemeFile) {
			t.Errorf("classifyCloneScheme(%q, false) = %v, want ErrSchemeFile", raw, err)
		}
		if err := classifyCloneScheme(raw, true); !errors.Is(err, ErrSchemeFile) {
			t.Errorf("classifyCloneScheme(%q, true) = %v, want ErrSchemeFile", raw, err)
		}
	}
}

// TestClassifyCloneSchemeRejectsLeadingDashHost pins the CVE-2017-1000117
// option-smuggling guard: an ssh/scp host beginning with "-" is refused even
// when ssh is enabled.
func TestClassifyCloneSchemeRejectsLeadingDashHost(t *testing.T) {
	scp := []string{"-oProxyCommand=x@host:repo", "user@-host:repo"}
	for _, raw := range scp {
		if err := classifyCloneScheme(raw, true); !errors.Is(err, ErrHostLeadingDash) {
			t.Errorf("classifyCloneScheme(%q, true) = %v, want ErrHostLeadingDash", raw, err)
		}
	}
	// The ssh:// leading-dash form must be rejected (ErrHostLeadingDash if it
	// parses, else ErrUnparseableURL) — never allowed.
	if err := classifyCloneScheme("ssh://-oProxyCommand=payload/repo", true); err == nil {
		t.Error("ssh://-oProxyCommand=... was allowed; want rejected")
	}
}
