package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ApplyGraphPlan creates a bead graph via a single hidden bd command so the
// full graph becomes visible only after the underlying transaction commits.
func (s *BdStore) ApplyGraphPlan(_ context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("graph apply plan is nil")
	}

	data, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("marshaling graph apply plan: %w", err)
	}

	tmpDir := filepath.Join(s.dir, ".gc", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating graph apply temp dir: %w", err)
	}

	f, err := os.CreateTemp(tmpDir, "graph-apply-*.json")
	if err != nil {
		return nil, fmt.Errorf("creating graph apply temp file: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("writing graph apply temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("closing graph apply temp file: %w", err)
	}

	out, err := s.runner(s.dir, "bd", "create", "--graph", tmpPath, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
	}

	var result GraphApplyResult
	if err := json.Unmarshal(extractJSON(out), &result); err != nil {
		return nil, fmt.Errorf("bd create --graph: parsing JSON: %w", err)
	}
	if len(result.IDs) == 0 {
		return nil, fmt.Errorf("bd create --graph: empty result")
	}
	return &result, nil
}
