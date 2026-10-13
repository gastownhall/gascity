package connect

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCodesAreUniqueAndPrefixed(t *testing.T) {
	seen := make(map[Code]bool)
	for _, c := range Codes() {
		if !strings.HasPrefix(string(c), "connect_") {
			t.Errorf("code %q lacks the connect_ prefix", c)
		}
		if seen[c] {
			t.Errorf("code %q is listed twice", c)
		}
		seen[c] = true
	}
	if len(seen) != 25 {
		t.Errorf("Codes() has %d codes, want the 25 §3.8 publishes", len(seen))
	}
}

func TestRefusalErrorAndCodeOf(t *testing.T) {
	cause := errors.New("cause")
	r := &Refusal{Code: CodeLedgerUnreachable, Detail: "dial failed", Err: cause}
	if got, want := r.Error(), "connect_ledger_unreachable: dial failed"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := (&Refusal{Code: CodeNoCity}).Error(); got != "connect_no_city" {
		t.Errorf("Error() without detail = %q", got)
	}
	wrapped := fmt.Errorf("gc connect: %w", r)
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is does not reach the cause through the refusal")
	}
	if code, ok := CodeOf(wrapped); !ok || code != CodeLedgerUnreachable {
		t.Errorf("CodeOf(wrapped) = (%q, %v)", code, ok)
	}
	if _, ok := CodeOf(cause); ok {
		t.Error("CodeOf found a code on a plain error")
	}
}
