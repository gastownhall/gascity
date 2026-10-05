package beads

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RecomputeBlockedMinBDVersion is the first bd release that ships
// `bd recompute-blocked`. An older bd has no way to repair the is_blocked
// column, so callers skip the repair rather than fail on an unknown command.
const RecomputeBlockedMinBDVersion = "1.1.0"

// BDVersion reports the version token of the bd binary this store's runner
// drives (e.g. "1.3.2-rc.1"). It goes through the store's own runner, so a
// workspace-pinned BD_BIN is the binary asked, not whatever bd PATH resolves.
func (s *BdStore) BDVersion() (string, error) {
	out, err := s.runner(s.dir, "bd", "version")
	if err != nil {
		return "", fmt.Errorf("bd version: %w", err)
	}
	version, err := parseBDVersion(string(out))
	if err != nil {
		return "", fmt.Errorf("bd version: %w", err)
	}
	return version, nil
}

// ConfigGet reads a bd config key via `bd config get --json`. An unset key
// reads as "" with no error, which is how bd itself reports it.
func (s *BdStore) ConfigGet(key string) (string, error) {
	out, err := s.runner(s.dir, "bd", "config", "get", "--json", key)
	if err != nil {
		return "", fmt.Errorf("bd config get %s: %w", key, err)
	}
	var result struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(extractJSON(out), &result); err != nil || result.Value == nil {
		return "", fmt.Errorf("bd config get %s: unexpected output: %s", key, truncateRawOutput(out, 256))
	}
	return *result.Value, nil
}

// RecomputeBlocked runs `bd recompute-blocked`, bd's idempotent full rebuild of
// the denormalized is_blocked column, and returns how many rows it corrected.
// On a consistent store it changes nothing and reports 0.
func (s *BdStore) RecomputeBlocked() (int, error) {
	out, err := s.runner(s.dir, "bd", "recompute-blocked", "--json")
	if err != nil {
		return 0, fmt.Errorf("bd recompute-blocked: %w", err)
	}
	var result struct {
		RowsCorrected *int `json:"rows_corrected"`
	}
	if err := json.Unmarshal(extractJSON(out), &result); err != nil || result.RowsCorrected == nil {
		return 0, fmt.Errorf("bd recompute-blocked: unexpected output: %s", strings.TrimSpace(truncateRawOutput(out, 256)))
	}
	return *result.RowsCorrected, nil
}
