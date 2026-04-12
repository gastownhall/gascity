//go:build integration

package integration

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// e2eAgent describes an agent for E2E tests with full config control.
type e2eAgent struct {
	Name              string
	Dir               string // working directory (supports {{.Agent}} templates)
	StartCommand      string // overrides workspace default
	IdleTimeout       string // overrides default singleton keep-alive window
	OverlayDir        string // relative to cityDir
	PromptTemplate    string // path to prompt template
	Env               map[string]string
	InstallAgentHooks []string
	PreStart          []string
	SessionSetup      []string
	Pool              *e2ePool
	Suspended         bool
	WorkQuery         string
	Nudge             string
}

// e2ePool describes pool configuration for an e2eAgent.
type e2ePool struct {
	Min   int
	Max   int
	Check string
}

// e2eWorkspace describes workspace-level config for E2E tests.
type e2eWorkspace struct {
	Name              string
	StartCommand      string // workspace default
	SessionTemplate   string
	InstallAgentHooks []string
	Suspended         bool
}

// e2eProvider describes a custom provider in [providers.xxx].
type e2eProvider struct {
	Command string
	Env     map[string]string
}

// e2eCity is the top-level config for E2E test cities.
type e2eCity struct {
	Workspace e2eWorkspace
	Agents    []e2eAgent
	Providers map[string]e2eProvider
}

// e2eReport holds parsed report data from e2e-report.sh.
// Keys map to value slices since some keys repeat (FILE_PRESENT, HOOK_PRESENT).
type e2eReport struct {
	Values map[string][]string
}

// get returns the last value for the given key, or empty string.
// Uses last value because some keys appear multiple times (e.g., STATUS=started
// at top, STATUS=complete at bottom).
func (r *e2eReport) get(key string) string {
	if vs, ok := r.Values[key]; ok && len(vs) > 0 {
		return vs[len(vs)-1]
	}
	return ""
}

// getAll returns all values for the given key.
func (r *e2eReport) getAll(key string) []string {
	return r.Values[key]
}

// has returns true if any value matches for the given key.
func (r *e2eReport) has(key, value string) bool {
	for _, v := range r.Values[key] {
		if v == value {
			return true
		}
	}
	return false
}

// hasKey returns true if the key is present in the report.
func (r *e2eReport) hasKey(key string) bool {
	_, ok := r.Values[key]
	return ok
}

// writeE2EToml generates a full city.toml from the e2eCity config.
func renderE2EToml(city e2eCity) string {
	var b strings.Builder

	// [workspace]
	fmt.Fprintf(&b, "[workspace]\nname = %s\n", quote(city.Workspace.Name))
	if city.Workspace.StartCommand != "" {
		fmt.Fprintf(&b, "start_command = %s\n", quote(city.Workspace.StartCommand))
	}
	if city.Workspace.SessionTemplate != "" {
		fmt.Fprintf(&b, "session_template = %s\n", quote(city.Workspace.SessionTemplate))
	}
	if len(city.Workspace.InstallAgentHooks) > 0 {
		fmt.Fprintf(&b, "install_agent_hooks = [%s]\n", quoteSlice(city.Workspace.InstallAgentHooks))
	}
	if city.Workspace.Suspended {
		b.WriteString("suspended = true\n")
	}

	// [beads] — use file store so tests don't need bd init or dolt.
	b.WriteString("\n[beads]\nprovider = \"file\"\n")

	// [providers.xxx]
	for name, prov := range city.Providers {
		fmt.Fprintf(&b, "\n[providers.%s]\n", name)
		if prov.Command != "" {
			fmt.Fprintf(&b, "command = %s\n", quote(prov.Command))
		}
		if len(prov.Env) > 0 {
			b.WriteString("[providers." + name + ".env]\n")
			for k, v := range prov.Env {
				fmt.Fprintf(&b, "%s = %s\n", k, quote(v))
			}
		}
	}

	// [[agent]]
	for _, a := range city.Agents {
		fmt.Fprintf(&b, "\n[[agent]]\nname = %s\n", quote(a.Name))
		if a.StartCommand != "" {
			fmt.Fprintf(&b, "start_command = %s\n", quote(a.StartCommand))
		}
		if a.IdleTimeout != "" {
			fmt.Fprintf(&b, "idle_timeout = %s\n", quote(a.IdleTimeout))
		}
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
		if a.OverlayDir != "" {
			fmt.Fprintf(&b, "overlay_dir = %s\n", quote(a.OverlayDir))
		}
		if a.PromptTemplate != "" {
			fmt.Fprintf(&b, "prompt_template = %s\n", quote(a.PromptTemplate))
		}
		if len(a.InstallAgentHooks) > 0 {
			fmt.Fprintf(&b, "install_agent_hooks = [%s]\n", quoteSlice(a.InstallAgentHooks))
		}
		if len(a.PreStart) > 0 {
			fmt.Fprintf(&b, "pre_start = [%s]\n", quoteSlice(a.PreStart))
		}
		if len(a.SessionSetup) > 0 {
			fmt.Fprintf(&b, "session_setup = [%s]\n", quoteSlice(a.SessionSetup))
		}
		if a.Suspended {
			b.WriteString("suspended = true\n")
		}
		if a.WorkQuery != "" {
			fmt.Fprintf(&b, "work_query = %s\n", quote(a.WorkQuery))
		}
		if a.Nudge != "" {
			fmt.Fprintf(&b, "nudge = %s\n", quote(a.Nudge))
		}
		if a.Pool == nil {
			// E2E helpers expect a plain test agent to behave like a singleton
			// session unless the test explicitly opts into pool semantics.
			fmt.Fprintf(&b, "max_active_sessions = 1\n")
			if strings.TrimSpace(a.IdleTimeout) == "" {
				fmt.Fprintf(&b, "idle_timeout = %s\n", quote("1h"))
			}
		} else {
			fmt.Fprintf(&b, "min_active_sessions = %d\n", a.Pool.Min)
			fmt.Fprintf(&b, "max_active_sessions = %d\n", a.Pool.Max)
			if a.Pool.Check != "" {
				fmt.Fprintf(&b, "scale_check = %s\n", quote(a.Pool.Check))
			}
		}
		if len(a.Env) > 0 {
			b.WriteString("\n[agent.env]\n")
			for k, v := range a.Env {
				fmt.Fprintf(&b, "%s = %s\n", k, quote(v))
			}
		}
	}

	// [[named_session]]
	// Plain singleton agents are no longer controller-managed by template
	// alone. Materialize a canonical named session for the common E2E helper
	// case so tests that target the bare agent name keep exercising a stable,
	// managed runtime.
	for _, a := range city.Agents {
		if a.Pool != nil {
			continue
		}
		fmt.Fprintf(&b, "\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n", quote(a.Name))
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
	}

	return b.String()
}

// writeE2EToml generates a full city.toml from the e2eCity config.
func writeE2EToml(t *testing.T, cityDir string, city e2eCity) {
	t.Helper()

	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(renderE2EToml(city)), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// setupE2ECity initializes a city, writes config, starts agents, and
// registers cleanup. Returns the city directory path.
func setupE2ECity(t *testing.T, guard *tmuxtest.Guard, city e2eCity) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	if city.Workspace.Name == "" {
		if guard != nil {
			city.Workspace.Name = guard.CityName()
		} else {
			city.Workspace.Name = uniqueCityName()
		}
	}

	cityDir := filepath.Join(t.TempDir(), city.Workspace.Name)
	configPath := filepath.Join(t.TempDir(), city.Workspace.Name+".toml")
	if err := os.WriteFile(configPath, []byte(renderE2EToml(city)), 0o644); err != nil {
		t.Fatalf("writing init config: %v", err)
	}

	// Stage scripts before the first controller launch so CopyFiles hashing is
	// stable. If scripts appear only after init's startup, the second gc start
	// sees a different runtime fingerprint and drains the session for
	// config-drift.
	copyE2EScripts(t, cityDir)

	// gc init — seed the city directly from the intended E2E config rather than
	// the default tutorial scaffold, which brings along unrelated hooks/packs.
	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	for _, agentCfg := range city.Agents {
		if agentCfg.Pool != nil || agentCfg.Suspended {
			continue
		}
		qualifiedName := qualifiedE2EAgentName(agentCfg)
		if strings.Contains(agentCfg.StartCommand, "e2e-report.sh") {
			waitForReport(t, cityDir, qualifiedName, e2eDefaultTimeout())
			continue
		}
		waitForAgentRunning(t, cityDir, qualifiedName, 15*time.Second)
	}

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCWithEnv(env, "", "stop", cityDir)      //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop") //nolint:errcheck // best-effort cleanup
		fixRootOwnedFiles(cityDir)
	})

	return cityDir
}

// setupE2ECityNoStart initializes a city and writes config but does NOT
// start agents. Useful for tests that need to verify pre-start state or
// test gc start behavior directly.
func setupE2ECityNoStart(t *testing.T, city e2eCity) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	if city.Workspace.Name == "" {
		city.Workspace.Name = uniqueCityName()
	}

	cityDir := filepath.Join(t.TempDir(), city.Workspace.Name)
	configPath := filepath.Join(t.TempDir(), city.Workspace.Name+".toml")
	if err := os.WriteFile(configPath, []byte(renderE2EToml(city)), 0o644); err != nil {
		t.Fatalf("writing init config: %v", err)
	}

	// Pre-stage scripts so init's first launch fingerprints the final staged
	// content instead of a missing scripts directory.
	copyE2EScripts(t, cityDir)

	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	// Reset the city to a quiescent state for tests that want to drive startup.
	out, err = runGCWithEnv(env, "", "stop", cityDir)
	if err != nil {
		t.Fatalf("gc stop after init failed: %v\noutput: %s", err, out)
	}

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCWithEnv(env, "", "stop", cityDir)      //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop") //nolint:errcheck // best-effort cleanup
		fixRootOwnedFiles(cityDir)
	})

	return cityDir
}

// e2eReportScript returns the start_command for e2e-report.sh.
// Uses $GC_CITY so the path resolves inside Docker/K8s containers
// where the city directory is mounted but the host source tree is not.
func e2eReportScript() string {
	return "bash $GC_CITY/.gc/scripts/e2e-report.sh"
}

// e2eSleepScript returns a start_command that sleeps forever.
// Uses $GC_CITY so the path resolves inside Docker/K8s containers.
// Uses stuck-agent.sh instead of bare "sleep 3600" because the
// subprocess provider appends a beacon argument to the command;
// sleep can't parse it, but bash scripts ignore extra arguments.
func e2eSleepScript() string {
	return "bash $GC_CITY/.gc/scripts/stuck-agent.sh"
}

// copyE2EScripts copies test agent scripts from the source tree into
// cityDir/.gc/scripts/ so they are accessible inside Docker/K8s containers
// (which mount cityDir but not the host source tree).
func copyE2EScripts(t *testing.T, cityDir string) {
	t.Helper()
	dstDir := filepath.Join(cityDir, ".gc", "scripts")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("creating scripts dir: %v", err)
	}
	for _, name := range []string{"e2e-report.sh", "stuck-agent.sh"} {
		src := agentScript(name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		dst := filepath.Join(dstDir, name)
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
}

// waitForReport polls for an agent's report file until STATUS=complete
// or the timeout expires. Returns the parsed report.
//
// For container providers (Docker, K8s), the report may only exist inside the
// container/pod, not on the host filesystem. When the local file is missing,
// this function tries to read it via the session provider's copy-from operation.
func waitForReport(t *testing.T, cityDir, agentName string, timeout time.Duration) *e2eReport {
	t.Helper()
	safeName := strings.ReplaceAll(agentName, "/", "__")
	reportPath := filepath.Join(cityDir, ".gc-reports", safeName+".report")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Try local filesystem first (works for subprocess, tmux, Docker bind-mount).
		data, err := os.ReadFile(reportPath)
		if err == nil && strings.Contains(string(data), "STATUS=complete") {
			return parseReport(t, data)
		}

		// For container providers, try reading from inside the session.
		if !usingSubprocess() {
			if data := readReportFromSession(cityDir, agentName, reportPath); data != nil &&
				strings.Contains(string(data), "STATUS=complete") {
				return parseReport(t, data)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Timeout: show what we have.
	data, err := os.ReadFile(reportPath)
	if err != nil {
		// Last attempt via session provider.
		if data := readReportFromSession(cityDir, agentName, reportPath); data != nil {
			t.Fatalf("timed out waiting for report from %s (STATUS=complete not found):\n%s", agentName, string(data))
		}
		t.Fatalf("timed out waiting for report from %s: file not found at %s", agentName, reportPath)
	}
	t.Fatalf("timed out waiting for report from %s (STATUS=complete not found):\n%s", agentName, string(data))
	return nil // unreachable
}

func waitForAgentRunning(t *testing.T, cityDir, agentName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := gc(cityDir, "session", "list", "--state", "all")
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) < 6 {
					continue
				}
				state := fields[2]
				target := fields[4]
				if target == agentName && state == "active" {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := gc(cityDir, "session", "list", "--state", "all")
	t.Fatalf("agent %q not active within %s\nsessions:\n%s", agentName, timeout, out)
}

func qualifiedE2EAgentName(a e2eAgent) string {
	if strings.TrimSpace(a.Dir) == "" {
		return a.Name
	}
	return a.Dir + "/" + a.Name
}

// readReportFromSession reads a file from inside the session via the session
// provider's copy-from operation. Returns nil if the read fails or the provider
// doesn't support copy-from (non-exec providers).
func readReportFromSession(cityDir, agentName, filePath string) []byte {
	sessionScript := sessionProviderScript()
	if sessionScript == "" {
		return nil // Not an exec provider.
	}
	sessName := buildSessionName(cityDir, agentName)
	if sessName == "" {
		return nil
	}
	cmd := exec.Command(sessionScript, "copy-from", sessName, filePath)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// buildSessionName computes the session name for an agent, honoring any
// session_template in city.toml. Mirrors agent.SessionNameFor logic.
func buildSessionName(cityDir, agentName string) string {
	cityName := findCityNameFromDir(cityDir)
	if cityName == "" {
		return ""
	}
	sanitized := strings.ReplaceAll(agentName, "/", "--")
	st := findSessionTemplate(cityDir)
	if st == "" {
		return "gc-" + cityName + "-" + sanitized
	}
	tmpl, err := template.New("session").Parse(st)
	if err != nil {
		return "gc-" + cityName + "-" + sanitized
	}
	data := struct{ City, Agent string }{City: cityName, Agent: sanitized}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "gc-" + cityName + "-" + sanitized
	}
	return buf.String()
}

// findSessionTemplate reads city.toml to extract the workspace session_template.
func findSessionTemplate(cityDir string) string {
	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "session_template") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
		}
	}
	return ""
}

// sessionProviderScript returns the script path from GC_SESSION=exec:<path>,
// or empty string if not an exec provider.
func sessionProviderScript() string {
	s := os.Getenv("GC_SESSION")
	if strings.HasPrefix(s, "exec:") {
		return s[5:]
	}
	return ""
}

// findCityNameFromDir reads city.toml to extract the workspace name.
// Returns empty string on failure (caller handles gracefully).
func findCityNameFromDir(cityDir string) string {
	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
		}
	}
	return ""
}

// parseReport parses KEY=VALUE lines from report data.
func parseReport(t *testing.T, data []byte) *e2eReport {
	t.Helper()
	report := &e2eReport{Values: make(map[string][]string)}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := line[:idx]
			value := line[idx+1:]
			report.Values[key] = append(report.Values[key], value)
		}
	}
	return report
}

// createOverlayDir creates an overlay directory with test marker files
// inside the city directory. Returns the relative path from cityDir.
func createOverlayDir(t *testing.T, cityDir string) string {
	t.Helper()
	overlayDir := filepath.Join(cityDir, "overlays", "test")
	if err := os.MkdirAll(filepath.Join(overlayDir, "overlay-subdir"), 0o755); err != nil {
		t.Fatalf("creating overlay dir: %v", err)
	}
	// Marker file at root.
	if err := os.WriteFile(filepath.Join(overlayDir, ".overlay-marker"), []byte("e2e-test"), 0o644); err != nil {
		t.Fatalf("creating overlay marker: %v", err)
	}
	// Nested file.
	if err := os.WriteFile(filepath.Join(overlayDir, "overlay-subdir", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatalf("creating nested overlay file: %v", err)
	}
	return "overlays/test"
}

// quoteSlice returns TOML-formatted quoted strings joined by commas.
func quoteSlice(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = quote(s)
	}
	return strings.Join(quoted, ", ")
}

// e2eDefaultTimeout returns the polling timeout for E2E tests.
// Container providers (Docker, K8s) need longer than subprocess because
// container startup, tmux initialization, and docker exec add latency.
func e2eDefaultTimeout() time.Duration {
	if usingSubprocess() {
		return 15 * time.Second
	}
	return 90 * time.Second
}

// fixRootOwnedFiles fixes permission-denied errors during t.TempDir()
// cleanup when Docker containers create root-owned files in mounted
// volumes. Agent scripts use umask 000, but this is a safety net.
func fixRootOwnedFiles(cityDir string) {
	if usingSubprocess() {
		return
	}
	filepath.Walk(cityDir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if info.Mode().Perm()&0o200 == 0 {
			os.Chmod(path, info.Mode()|0o666) //nolint:errcheck
		}
		return nil
	})
}
