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
func (s *BdStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage creates a bead graph in a storage tier selected by
// policy middleware.
func (s *BdStore) ApplyGraphPlanWithStorage(_ context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("graph apply plan is nil")
	}
	ephemeral, noHistory, err := graphStorageFlags(storage)
	if err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
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

	args := []string{"create", "--graph", tmpPath, "--json"}
	if ephemeral {
		args = append(args, "--ephemeral")
	}
	if noHistory {
		args = append(args, "--no-history")
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
	}

	var result GraphApplyResult
	if err := json.Unmarshal(extractJSON(out), &result); err != nil {
		return nil, fmt.Errorf("bd create --graph: parsing JSON: %w", err)
	}
	if err := ValidateGraphApplyResult(plan, &result); err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
	}
	return &result, nil
}

func graphStorageFlags(storage StorageClass) (ephemeral bool, noHistory bool, err error) {
	switch storage {
	case StorageDefault, StorageHistory:
		return false, false, nil
	case StorageNoHistory:
		return false, true, nil
	case StorageEphemeral:
		return true, false, nil
	default:
		return false, false, fmt.Errorf("unknown storage class %q", storage)
	}
}

// SupportsEphemeralGraphApply reports whether this store can apply a whole
// graph directly into ephemeral storage.
func (s *BdStore) SupportsEphemeralGraphApply() bool {
	return true
}
