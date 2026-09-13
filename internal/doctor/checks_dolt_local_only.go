package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"gopkg.in/yaml.v3"
)

// DoltLocalOnlyRemoteCheck verifies that local-only managed Dolt rigs do not
// retain off-box Dolt remotes reintroduced from the CLI repo state.
type DoltLocalOnlyRemoteCheck struct {
	cityPath     string
	rig          config.Rig
	doltDataDir  string
	removeRemote func(rigPath, remoteName string) error
}

// NewDoltLocalOnlyRemoteCheck creates a per-rig local-only Dolt remote check.
func NewDoltLocalOnlyRemoteCheck(cityPath string, rig config.Rig, doltDataDir string) *DoltLocalOnlyRemoteCheck {
	if strings.TrimSpace(doltDataDir) == "" {
		doltDataDir = filepath.Join(cityPath, ".beads", "dolt")
	}
	return &DoltLocalOnlyRemoteCheck{
		cityPath:     cityPath,
		rig:          rig,
		doltDataDir:  doltDataDir,
		removeRemote: removeDoltRemote,
	}
}

// Name returns the check identifier ("rig:<name>:dolt-local-only-remote").
func (c *DoltLocalOnlyRemoteCheck) Name() string {
	return "rig:" + c.rig.Name + ":dolt-local-only-remote"
}

// Run executes the check.
func (c *DoltLocalOnlyRemoteCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	rigPath := normalizedRigPath(c.cityPath, c.rig)

	localOnly, configured, err := rigDoltLocalOnly(rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: cannot read dolt local-only config", c.rig.Name)
		r.Details = []string{err.Error()}
		return r
	}
	if !configured {
		r.Status = StatusOK
		r.Message = "dolt local-only not configured"
		return r
	}
	if !localOnly {
		r.Status = StatusOK
		r.Message = "dolt local-only disabled"
		return r
	}

	offBox, details, err := c.offBoxRemotes(rigPath)
	r.Details = append(r.Details, details...)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: cannot inspect Dolt repo_state.json", c.rig.Name)
		r.Details = append(r.Details, err.Error())
		return r
	}
	if len(offBox) == 0 {
		r.Status = StatusOK
		r.Message = "dolt local-only remote state OK"
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("rig %q: dolt.local-only is true but off-box Dolt remote(s) are registered: %s",
		c.rig.Name, localOnlyRemoteNames(offBox))
	r.FixHint = localOnlyRemoteFixHint(offBox)
	return r
}

// CanFix returns true only when local-only mode is explicitly enabled.
func (c *DoltLocalOnlyRemoteCheck) CanFix() bool {
	rigPath := normalizedRigPath(c.cityPath, c.rig)
	localOnly, _, err := rigDoltLocalOnly(rigPath)
	return err == nil && localOnly
}

// Fix removes only off-box Dolt remotes from a local-only rig.
func (c *DoltLocalOnlyRemoteCheck) Fix(_ *CheckContext) error {
	rigPath := normalizedRigPath(c.cityPath, c.rig)
	localOnly, _, err := rigDoltLocalOnly(rigPath)
	if err != nil {
		return err
	}
	if !localOnly {
		return nil
	}
	offBox, _, err := c.offBoxRemotes(rigPath)
	if err != nil {
		return err
	}
	for _, remote := range offBox {
		if err := c.removeRemote(rigPath, remote.Name); err != nil {
			return fmt.Errorf("removing dolt remote %q: %w", remote.Name, err)
		}
	}
	return nil
}

func (c *DoltLocalOnlyRemoteCheck) offBoxRemotes(rigPath string) ([]doltRemoteState, []string, error) {
	dbName, details := resolveDoltDBName(c.rig, rigPath)
	remotes, err := readDoltRepoStateRemotes(c.doltDataDir, dbName)
	if err != nil {
		return nil, details, err
	}
	var offBox []doltRemoteState
	for _, remote := range remotes {
		if isOffBoxDoltRemoteURL(remote.URL) {
			offBox = append(offBox, remote)
		}
	}
	sortDoltRemotes(offBox)
	return offBox, details, nil
}

func rigDoltLocalOnly(rigPath string) (bool, bool, error) {
	configPath := filepath.Join(rigPath, ".beads", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read %s: %w", configPath, err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return false, true, fmt.Errorf("parse %s: %w", configPath, err)
	}
	value, ok := lookupConfigValue(cfg, "dolt.local-only")
	if !ok {
		return false, false, nil
	}
	enabled, ok := boolish(value)
	if !ok {
		return false, true, fmt.Errorf("parse %s dolt.local-only: expected boolean, got %T", configPath, value)
	}
	return enabled, true, nil
}

func lookupConfigValue(cfg map[string]any, key string) (any, bool) {
	if value, ok := cfg[key]; ok {
		return value, true
	}
	parts := strings.Split(key, ".")
	var current any = cfg
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func boolish(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

type doltRemoteState struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func readDoltRepoStateRemotes(doltDataDir, dbName string) ([]doltRemoteState, error) {
	statePath := filepath.Join(doltDataDir, dbName, ".dolt", "repo_state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var state struct {
		Remotes map[string]doltRemoteState `json:"remotes"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", statePath, err)
	}
	remotes := make([]doltRemoteState, 0, len(state.Remotes))
	for name, remote := range state.Remotes {
		if strings.TrimSpace(remote.Name) == "" {
			remote.Name = name
		}
		remotes = append(remotes, remote)
	}
	sortDoltRemotes(remotes)
	return remotes, nil
}

func isOffBoxDoltRemoteURL(rawURL string) bool {
	url := strings.ToLower(strings.TrimSpace(rawURL))
	if url == "" || strings.HasPrefix(url, "file://") {
		return false
	}
	if strings.HasPrefix(url, "/") || strings.HasPrefix(url, "./") || strings.HasPrefix(url, "../") {
		return false
	}
	switch {
	case strings.HasPrefix(url, "git+https://"):
		return true
	case strings.HasPrefix(url, "git+http://"):
		return true
	case strings.HasPrefix(url, "git+ssh://"):
		return true
	case strings.HasPrefix(url, "https://"):
		return true
	case strings.HasPrefix(url, "http://"):
		return true
	case strings.HasPrefix(url, "ssh://"):
		return true
	case strings.HasPrefix(url, "dolthub://"):
		return true
	case strings.HasPrefix(url, "s3://"):
		return true
	case strings.HasPrefix(url, "gs://"):
		return true
	case strings.HasPrefix(url, "az://"):
		return true
	case strings.Contains(url, "@") && strings.Contains(url, ":"):
		return true
	default:
		return false
	}
}

func sortDoltRemotes(remotes []doltRemoteState) {
	sort.Slice(remotes, func(i, j int) bool {
		return remotes[i].Name < remotes[j].Name
	})
}

func localOnlyRemoteNames(remotes []doltRemoteState) string {
	names := make([]string, 0, len(remotes))
	for _, remote := range remotes {
		names = append(names, remote.Name)
	}
	return strings.Join(names, ", ")
}

func localOnlyRemoteFixHint(remotes []doltRemoteState) string {
	lines := make([]string, 0, len(remotes)+1)
	lines = append(lines, "remove off-box Dolt remote(s):")
	for _, remote := range remotes {
		lines = append(lines, "  bd dolt remote remove "+remote.Name)
	}
	return strings.Join(lines, "\n")
}

func removeDoltRemote(rigPath, remoteName string) error {
	cmd := exec.Command("bd", "--sandbox", "dolt", "remote", "remove", remoteName)
	cmd.Dir = rigPath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd --sandbox dolt remote remove %s: %w: %s", remoteName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
