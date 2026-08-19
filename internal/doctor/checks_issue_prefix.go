package doctor

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// IssuePrefixCheck verifies that the Dolt-backed issue_prefix config row is
// present and matches the canonical prefix in .beads/config.yaml.
//
// gastownhall/gascity#1436: bd rejects `bd config set issue_prefix`, so a city
// can look healthy (doctor green, schema present) while every bead create
// fails with "database not initialized: issue_prefix config is missing".
type IssuePrefixCheck struct {
	// Dir is the directory to check (city root or rig path).
	Dir string
	// Label identifies this check instance (e.g., "city" or rig name).
	Label string
	want  string
}

// NewIssuePrefixCheck creates a check for a specific store directory.
func NewIssuePrefixCheck(dir, label string) *IssuePrefixCheck {
	return &IssuePrefixCheck{Dir: dir, Label: label}
}

// Name returns the check identifier.
func (c *IssuePrefixCheck) Name() string {
	return "issue-prefix:" + c.Label
}

// Run checks that the runtime issue_prefix matches the canonical config.
func (c *IssuePrefixCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	beadsDir := filepath.Join(c.Dir, ".beads")
	if !dirExists(beadsDir) {
		r.Status = StatusOK
		r.Message = "no .beads directory, skipping"
		return r
	}

	want, ok, err := contract.ReadIssuePrefix(fsys.OSFS{}, filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read canonical issue_prefix: %v", err)
		return r
	}
	if !ok || strings.TrimSpace(want) == "" {
		r.Status = StatusWarning
		r.Message = "canonical .beads/config.yaml has no issue_prefix"
		r.FixHint = "re-run gc init / gc start so Gas City can seed the beads prefix"
		return r
	}
	c.want = strings.TrimSpace(want)

	got, err := getIssuePrefix(c.Dir)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("could not read runtime issue_prefix: %v", err)
		r.FixHint = "ensure the beads/Dolt server is up, then run: gc start (provider init reseeds issue_prefix via SQL)"
		return r
	}
	if got == "" {
		r.Status = StatusError
		r.Message = fmt.Sprintf("runtime issue_prefix is not set (want %q)", c.want)
		r.FixHint = "run: gc start (provider init reseeds issue_prefix via SQL); do not use bd init --force (creates a split store)"
		return r
	}
	if got != c.want {
		r.Status = StatusError
		r.Message = fmt.Sprintf("runtime issue_prefix = %q, want %q", got, c.want)
		r.FixHint = "run: gc start (provider init reseeds issue_prefix via SQL)"
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("issue_prefix = %q", got)
	return r
}

// CanFix returns false — reseeding requires the managed provider SQL path,
// not `bd config set issue_prefix` (which bd rejects).
func (c *IssuePrefixCheck) CanFix() bool { return false }

// Fix is a no-op; see CanFix.
func (c *IssuePrefixCheck) Fix(_ *CheckContext) error { return nil }

func getIssuePrefix(dir string) (string, error) {
	cmd := exec.Command("bd", "config", "get", "--json", "issue_prefix")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	var parsed struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		got := strings.TrimSpace(string(out))
		if got != "" && !strings.Contains(got, "{") {
			return got, nil
		}
		return "", fmt.Errorf("parsing bd config get output: %w", err)
	}
	return strings.TrimSpace(parsed.Value), nil
}
