package orders

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ExecOutcomeFileEnv names the env var through which the controller hands an
// exec order a per-run file for its skipped/partial declaration.
const ExecOutcomeFileEnv = "GC_ORDER_OUTCOME_FILE"

// ExecOutcomeKind is what an exec order that exited 0 declares about the work
// it did not do.
type ExecOutcomeKind string

const (
	// ExecOutcomeSkipped means nothing the order exists to do ran.
	ExecOutcomeSkipped ExecOutcomeKind = "skipped"
	// ExecOutcomePartial means some of the order's work ran and some did not.
	ExecOutcomePartial ExecOutcomeKind = "partial"
)

// ExecOutcomeScope names one scope or step that did not run, and why.
type ExecOutcomeScope struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// ExecOutcome is an exec order's skipped/partial declaration.
type ExecOutcome struct {
	Outcome ExecOutcomeKind    `json:"outcome"`
	Reason  string             `json:"reason"`
	Scopes  []ExecOutcomeScope `json:"scopes"`
}

// DecodeExecOutcome parses the contents of an exec order's outcome file. An
// empty file is no declaration (ok=false, err=nil). Anything else must be
// exactly one declaration object with a known outcome and no unknown fields.
func DecodeExecOutcome(data []byte) (ExecOutcome, bool, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return ExecOutcome{}, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var out ExecOutcome
	if err := dec.Decode(&out); err != nil {
		return ExecOutcome{}, false, fmt.Errorf("decoding exec order outcome: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ExecOutcome{}, false, errors.New("decoding exec order outcome: trailing data after the declaration")
	}
	switch out.Outcome {
	case ExecOutcomeSkipped, ExecOutcomePartial:
	default:
		return ExecOutcome{}, false, fmt.Errorf("decoding exec order outcome: unknown outcome %q", out.Outcome)
	}
	for i, scope := range out.Scopes {
		if scope.Scope == "" {
			return ExecOutcome{}, false, fmt.Errorf("decoding exec order outcome: scope %d has no name", i)
		}
	}
	return out, true, nil
}
