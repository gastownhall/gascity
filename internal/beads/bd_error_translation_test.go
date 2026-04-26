package beads

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTranslateBdError_Nil(t *testing.T) {
	if got := translateBdError(nil); got != nil {
		t.Errorf("translateBdError(nil) = %v, want nil", got)
	}
}

func TestTranslateBdError_UnrelatedPassthrough(t *testing.T) {
	original := errors.New("bd: some other failure")
	got := translateBdError(original)
	if !errors.Is(got, original) {
		t.Errorf("unrelated error must be returned unchanged; got %v", got)
	}
	if errors.Is(got, ErrIssuePrefixMissing) {
		t.Error("unrelated error must not match ErrIssuePrefixMissing")
	}
}

// TestTranslateBdError_DetectsBdRuntimeError verifies the translation on
// the exact stderr text bd 1.0.3 emits when the config row is missing —
// the bug from #1232.
func TestTranslateBdError_DetectsBdRuntimeError(t *testing.T) {
	bdErr := errors.New(`exit status 1: {"error": "database not initialized: issue_prefix config is missing (run 'bd init --prefix <prefix>' for a new project, or 'bd bootstrap' to clone an existing remote)", "schema_version": 1}`)
	got := translateBdError(bdErr)
	if !errors.Is(got, ErrIssuePrefixMissing) {
		t.Errorf("expected ErrIssuePrefixMissing, got %v", got)
	}
	if !errors.Is(got, bdErr) {
		t.Errorf("translated error must still wrap the underlying bd error; got %v", got)
	}
	if !strings.Contains(got.Error(), "gc doctor --fix") {
		t.Errorf("translated error should mention gc doctor --fix, got %q", got.Error())
	}
	if !strings.Contains(got.Error(), "#1232") {
		t.Errorf("translated error should reference the issue, got %q", got.Error())
	}
}

// TestTranslateBdError_AlreadyTranslated avoids double-wrapping when
// the helper is called twice in a chain.
func TestTranslateBdError_AlreadyTranslated(t *testing.T) {
	once := translateBdError(errors.New("issue_prefix config is missing"))
	twice := translateBdError(once)
	if twice.Error() != once.Error() {
		t.Errorf("double translation should not change the message:\n once:  %s\n twice: %s", once, twice)
	}
}

// TestTranslateBdError_TolerantOfFormatVariations confirms the matcher
// catches the same error class even if bd reformats its output.
func TestTranslateBdError_TolerantOfFormatVariations(t *testing.T) {
	cases := []string{
		"issue_prefix config is missing",
		"bd create: issue_prefix config is missing",
		`exit 1: {"error": "issue_prefix config is missing"}`,
		"warning: ...\n issue_prefix config is missing\nfix: ...",
	}
	for _, msg := range cases {
		t.Run(msg[:min(len(msg), 40)], func(t *testing.T) {
			err := translateBdError(errors.New(msg))
			if !errors.Is(err, ErrIssuePrefixMissing) {
				t.Errorf("did not detect issue_prefix marker in %q", msg)
			}
		})
	}
}

// TestTranslateBdError_DoesNotMatchSimilarStrings guards against false
// positives that would mis-report unrelated failures as repairable.
func TestTranslateBdError_DoesNotMatchSimilarStrings(t *testing.T) {
	cases := []string{
		"bd: issue_prefix value reserved", // similar but not the marker
		"prefix mismatch",
		"bd init failed",
		"connection refused",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			err := translateBdError(errors.New(msg))
			if errors.Is(err, ErrIssuePrefixMissing) {
				t.Errorf("false positive on %q", msg)
			}
		})
	}
}

func TestTranslateBdError_PreservesWrappedChain(t *testing.T) {
	root := errors.New("connection refused")
	wrapped := fmt.Errorf("dolt: %w", root)
	bdErr := fmt.Errorf("bd create: issue_prefix config is missing: %w", wrapped)

	got := translateBdError(bdErr)
	if !errors.Is(got, ErrIssuePrefixMissing) {
		t.Error("translated error must satisfy errors.Is(ErrIssuePrefixMissing)")
	}
	if !errors.Is(got, root) {
		t.Error("translated error must still preserve deeper wrapped chain")
	}
}

// min is here so the test file works on Go versions without builtin min.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
