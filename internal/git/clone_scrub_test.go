package git

import (
	"errors"
	"strings"
	"testing"
)

// TestScrubCloneErrorMasksUnparseableCredential covers the fail-open gap: an
// invalid %-escape in the userinfo makes url.Parse reject the URL, but git may
// still echo the raw secret, so it must be masked from the returned error.
func TestScrubCloneErrorMasksUnparseableCredential(t *testing.T) {
	raw := "https://user:pa%zz@host/repo.git"
	gitOut := errors.New("git clone: fatal: unable to access 'https://user:pa%zz@host/repo.git/': the requested URL returned error: 403")
	got := scrubCloneError(gitOut, raw).Error()
	if strings.Contains(got, "pa%zz") {
		t.Fatalf("scrubbed error still leaks the raw credential: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("expected the userinfo masked with ***, got %q", got)
	}
}

func TestScrubCloneErrorMasksParseableCredential(t *testing.T) {
	raw := "https://user:s3cr3t@host/repo.git"
	gitOut := errors.New("git clone: fatal: unable to access 'https://user:s3cr3t@host/repo.git/': 403")
	got := scrubCloneError(gitOut, raw).Error()
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("scrubbed error still leaks the password: %q", got)
	}
}

// TestScrubCloneErrorMasksPercentEncodedCredential covers a PARSEABLE URL whose
// password is percent-encoded: url.Parse decodes it (User.Password() == "secret")
// and re-encodes it (User.String() == "user:secret"), so neither matches the RAW
// "user:se%63ret" git echoes verbatim. The unconditional raw-userinfo scrub must
// mask it — before the fix this leaked because the raw-substring scrub only ran
// when url.Parse failed.
func TestScrubCloneErrorMasksPercentEncodedCredential(t *testing.T) {
	raw := "https://user:se%63ret@host/repo.git"
	gitOut := errors.New("git clone: fatal: unable to access 'https://user:se%63ret@host/repo.git/': the requested URL returned error: 403")
	got := scrubCloneError(gitOut, raw).Error()
	if strings.Contains(got, "se%63ret") {
		t.Fatalf("scrubbed error still leaks the percent-encoded credential: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("expected the userinfo masked with ***, got %q", got)
	}
}
