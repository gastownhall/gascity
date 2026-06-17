package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// ExecSink records each fact by invoking an external script with the fact's
// JSON on stdin (one JSON object, newline-terminated). It is the out-of-process
// injection seam — the script is the integration point for an external
// aggregator, exchanged over a JSON wire contract rather than a linked Go API
// (mirroring the events exec provider).
//
// Record runs the script synchronously; callers on a latency-sensitive path
// should prefer the durable LocalSink (which an external drainer can tail) and
// reserve ExecSink for low-frequency facts.
type ExecSink struct {
	script string
}

// NewExecSink returns an ExecSink that invokes script for each fact.
func NewExecSink(script string) *ExecSink { return &ExecSink{script: script} }

// Record marshals f to JSON and feeds it to the script on stdin.
func (s *ExecSink) Record(ctx context.Context, f Fact) error {
	line, err := json.Marshal(f)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.script)
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("usage exec sink %q: %w: %s", s.script, err, stderr.String())
	}
	return nil
}
