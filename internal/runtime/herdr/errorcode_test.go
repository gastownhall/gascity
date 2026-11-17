package herdr

import (
	"fmt"
	"testing"
)

// TestHerdrCodeAnyShapeRecoversBothErrorShapes locks in the fix for
// ga-iwanrj. herdr reports a failure in one of two shapes depending on how
// the CLI exits (see runWithSecrets in client.go): a zero exit with the
// error in the JSON envelope on stdout, wrapped as a typed *herdrError; or a
// non-zero exit, wrapped verbatim as an untyped *herdrStderr. herdrErrorCode
// only recovers a code from the first shape via errors.As, which is exactly
// what breaks the pane-busy retry guard at provider.go:198 — herdr rejects
// agent_pane_busy via a non-zero exit (the second shape). herdrCodeAnyShape
// must recover the code from either shape.
func TestHerdrCodeAnyShapeRecoversBothErrorShapes(t *testing.T) {
	// Shape 1 (non-zero exit): runWithSecrets wraps herdr's stderr verbatim.
	stderrEnvelope := `{"error":{"code":"agent_pane_busy","message":"agent target pane w1:p1 is not an available shell"},"id":"cli:agent:start"}`
	nonZeroExitErr := fmt.Errorf("herdr %v: %w", []string{"agent", "start"}, &herdrStderr{Text: stderrEnvelope})
	if got := herdrCodeAnyShape(nonZeroExitErr); got != "agent_pane_busy" {
		t.Errorf("herdrCodeAnyShape(non-zero-exit stderr) = %q; want %q", got, "agent_pane_busy")
	}

	// Shape 2 (zero exit, positive control): runWithSecrets wraps the
	// envelope's typed *herdrError — the shape herdrErrorCode already
	// handles. herdrCodeAnyShape must remain a superset, not a replacement.
	typedErr := fmt.Errorf("herdr %v: %w", []string{"agent", "start"}, &herdrError{Code: "agent_name_taken", Message: "agent name already registered"})
	if got := herdrCodeAnyShape(typedErr); got != "agent_name_taken" {
		t.Errorf("herdrCodeAnyShape(typed envelope) = %q; want %q", got, "agent_name_taken")
	}
}
