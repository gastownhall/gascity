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

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// canonicalTempDir returns a per-test temp directory with its path
// symlink-resolved, so agent-reported CWDs (which Go's os.Getwd already
// canonicalizes) compare equal on macOS. Without this, every E2E
// string-equality check between cityDir and an agent-reported path
// fails because macOS's /var is a symlink to /private/var.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

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

// renderE2EToml generates a full single-file template for gc init --file.
func renderE2EToml(city e2eCity) string {
	var b strings.Builder
	writeE2EWorkspaceSection(&b, city.Workspace)
	b.WriteString("\n[beads]\nprovider = \"file\"\n")
	writeE2EProviderSections(&b, city.Providers)
	writeE2EAgentSections(&b, city.Agents)
	writeE2ENamedSessionSections(&b, city.Agents)
	return b.String()
}

func renderE2ECityRuntimeToml(city e2eCity) string {
	var b strings.Builder
	writeE2EWorkspaceSection(&b, city.Workspace)
	b.WriteString("\n[beads]\nprovider = \"file\"\n")
	return b.String()
}

func renderE2EPackToml(city e2eCity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[pack]\nname = %s\nschema = 2\n", quote(city.Workspace.Name))
	writeE2EProviderSections(&b, city.Providers)
	writeE2EAgentSections(&b, city.Agents)
	writeE2ENamedSessionSections(&b, city.Agents)
	return b.String()
}

func writeE2EWorkspaceSection(b *strings.Builder, workspace e2eWorkspace) {
	fmt.Fprintf(b, "[workspace]\nname = %s\n", quote(workspace.Name))
	if workspace.StartCommand != "" {
		fmt.Fprintf(b, "start_command = %s\n", quote(workspace.StartCommand))
	}
	if workspace.SessionTemplate != "" {
		fmt.Fprintf(b, "session_template = %s\n", quote(workspace.SessionTemplate))
	}
	if len(workspace.InstallAgentHooks) > 0 {
		fmt.Fprintf(b, "install_agent_hooks = [%s]\n", quoteSlice(workspace.InstallAgentHooks))
	}
	if workspace.Suspended {
		b.WriteString("suspended = true\n")
	}
}

func writeE2EProviderSections(b *strings.Builder, providers map[string]e2eProvider) {
	for name, prov := range providers {
		fmt.Fprintf(b, "\n[providers.%s]\n", name)
		if prov.Command != "" {
			fmt.Fprintf(b, "command = %s\n", quote(prov.Command))
		}
		if len(prov.Env) > 0 {
			b.WriteString("[providers." + name + ".env]\n")
			for k, v := range prov.Env {
				fmt.Fprintf(b, "%s = %s\n", k, quote(v))
			}
		}
	}
}

func writeE2EAgentSections(b *strings.Builder, agents []e2eAgent) {
	for _, a := range agents {
		fmt.Fprintf(b, "\n[[agent]]\nname = %s\n", quote(a.Name))
		if a.StartCommand != "" {
			fmt.Fprintf(b, "start_command = %s\n", quote(a.StartCommand))
		}
		if a.IdleTimeout != "" {
			fmt.Fprintf(b, "idle_timeout = %s\n", quote(a.IdleTimeout))
		}
		if a.Dir != "" {
			fmt.Fprintf(b, "dir = %s\n", quote(a.Dir))
		}
		if a.OverlayDir != "" {
			fmt.Fprintf(b, "overlay_dir = %s\n", quote(a.OverlayDir))
		}
		if a.PromptTemplate != "" {
			fmt.Fprintf(b, "prompt_template = %s\n", quote(a.PromptTemplate))
		}
		if len(a.InstallAgentHooks) > 0 {
			fmt.Fprintf(b, "install_agent_hooks = [%s]\n", quoteSlice(a.InstallAgentHooks))
		}
		if len(a.PreStart) > 0 {
			fmt.Fprintf(b, "pre_start = [%s]\n", quoteSlice(a.PreStart))
		}
		if len(a.SessionSetup) > 0 {
			fmt.Fprintf(b, "session_setup = [%s]\n", quoteSlice(a.SessionSetup))
		}
		if a.Suspended {
			b.WriteString("suspended = true\n")
		}
		if a.WorkQuery != "" {
			fmt.Fprintf(b, "work_query = %s\n", quote(a.WorkQuery))
		}
		if a.Nudge != "" {
			fmt.Fprintf(b, "nudge = %s\n", quote(a.Nudge))
		}
		if a.Pool == nil {
			// Plain E2E agents are kept resident by the named session below.
			// Leave generic template capacity unbounded so config rewrites do
			// not also carry legacy singleton-pool semantics.
			if strings.TrimSpace(a.IdleTimeout) == "" {
				fmt.Fprintf(b, "idle_timeout = %s\n", quote("1h"))
			}
		} else {
			fmt.Fprintf(b, "min_active_sessions = %d\n", a.Pool.Min)
			fmt.Fprintf(b, "max_active_sessions = %d\n", a.Pool.Max)
			if a.Pool.Check != "" {
				fmt.Fprintf(b, "scale_check = %s\n", quote(a.Pool.Check))
			}
		}
		if len(a.Env) > 0 {
			b.WriteString("\n[agent.env]\n")
			for k, v := range a.Env {
				fmt.Fprintf(b, "%s = %s\n", k, quote(v))
			}
		}
	}
}

func writeE2ENamedSessionSections(b *strings.Builder, agents []e2eAgent) {
	// [[named_session]]
	// Plain singleton agents are no longer controller-managed by template
	// alone. Materialize a canonical named session for the common E2E helper
	// case so tests that target the bare agent name keep exercising a stable,
	// managed runtime.
	for _, a := range agents {
		if a.Pool != nil {
			continue
		}
		fmt.Fprintf(b, "\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n", quote(a.Name))
		if a.Dir != "" {
			fmt.Fprintf(b, "dir = %s\n", quote(a.Dir))
		}
	}
}

// writeE2EToml updates a post-init v2 city. Definition-bearing sections go
// into pack.toml; city.toml is written last to trigger the controller watcher
// after the pack update is durable.
func writeE2EToml(t *testing.T, cityDir string, city e2eCity) {
	t.Helper()

	packPath := filepath.Join(cityDir, "pack.toml")
	tomlPath := filepath.Join(cityDir, "city.toml")
	if _, err := os.Stat(packPath); err == nil {
		writeFileAtomic(t, packPath, []byte(renderE2EPackToml(city)))
		writeFileAtomic(t, tomlPath, []byte(renderE2ECityRuntimeToml(city)))
		return
	}
	writeFileAtomic(t, tomlPath, []byte(renderE2EToml(city)))
}

func writeE2ETomlFile(t *testing.T, tomlPath string, city e2eCity) {
	t.Helper()
	writeFileAtomic(t, tomlPath, []byte(renderE2EToml(city)))
}

func rewriteE2ETomlPreservingNamedSessions(t *testing.T, cityDir string, city e2eCity) {
	t.Helper()
	tomlPath := filepath.Join(cityDir, "city.toml")
	packPath := filepath.Join(cityDir, "pack.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	current, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing city.toml: %v", err)
	}
	if strings.TrimSpace(city.Workspace.Name) == "" {
		city.Workspace.Name = current.Workspace.Name
	}
	if strings.TrimSpace(city.Workspace.Name) == "" {
		city.Workspace.Name = filepath.Base(cityDir)
	}
	desiredCfg, err := config.Parse([]byte(renderE2EToml(city)))
	if err != nil {
		t.Fatalf("parsing rendered city.toml: %v", err)
	}
	nextRuntimeCfg, err := config.Parse([]byte(renderE2ECityRuntimeToml(city)))
	if err != nil {
		t.Fatalf("parsing rendered runtime city.toml: %v", err)
	}
	nextRuntimeCfg.NamedSessions = preservedLocalNamedSessions(current.NamedSessions, desiredCfg.NamedSessions)
	nextRuntime, err := nextRuntimeCfg.Marshal()
	if err != nil {
		t.Fatalf("marshaling city.toml: %v", err)
	}
	writeFileAtomic(t, packPath, []byte(renderE2EPackToml(city)))
	writeFileAtomic(t, tomlPath, nextRuntime)
	if _, _, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath); err != nil {
		t.Fatalf("loading rewritten city.toml: %v\n%s", err, nextRuntime)
	}
}

func preservedLocalNamedSessions(existing, desired []config.NamedSession) []config.NamedSession {
	preserved := make([]config.NamedSession, 0, len(existing))
	seen := make(map[string]bool, len(desired))
	for _, ns := range desired {
		seen[namedSessionMergeKey(ns)] = true
	}
	for _, ns := range existing {
		key := namedSessionMergeKey(ns)
		if seen[key] {
			continue
		}
		seen[key] = true
		preserved = append(preserved, ns)
	}
	return preserved
}

func namedSessionMergeKey(ns config.NamedSession) string {
	return ns.Dir + "\x00" + ns.IdentityName()
}

func writeFileAtomic(t *testing.T, path string, data []byte) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		t.Fatalf("creating temp file for %s: %v", path, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		t.Fatalf("writing temp file for %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("closing temp file for %s: %v", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		t.Fatalf("replacing %s: %v", path, err)
	}
	cleanup = false
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

	cityDir := filepath.Join(canonicalTempDir(t), city.Workspace.Name)
	// configPath doesn't need canonicalization today (no test compares it
	// to agent output), but keep it symmetric so future assertions don't
	// regress on macOS's /var→/private/var symlink.
	configPath := filepath.Join(canonicalTempDir(t), city.Workspace.Name+".toml")
	writeE2ETomlFile(t, configPath, city)

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
		// Each E2E helper gets its own isolated GC_HOME, so stopping that
		// supervisor is enough to tear down the city and any lingering
		// controllers or sessions.
		if out, err := runGCWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
			t.Logf("cleanup: gc supervisor stop --wait: %v\n%s", err, out)
		}
		cleanupTestCityDir(cityDir)
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

	cityDir := filepath.Join(canonicalTempDir(t), city.Workspace.Name)
	configPath := filepath.Join(canonicalTempDir(t), city.Workspace.Name+".toml")
	writeE2ETomlFile(t, configPath, city)

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
	restartIsolatedSupervisor(t, env)

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		// Each E2E helper gets its own isolated GC_HOME, so stopping that
		// supervisor is enough to tear down the city and any lingering
		// controllers or sessions.
		if out, err := runGCWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
			t.Logf("cleanup: gc supervisor stop --wait: %v\n%s", err, out)
		}
		cleanupTestCityDir(cityDir)
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

func cleanupTestCityDir(cityDir string) {
	fixRootOwnedFiles(cityDir)
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.RemoveAll(cityDir); err == nil {
			return
		}
		fixRootOwnedFiles(cityDir)
		time.Sleep(200 * time.Millisecond)
	}
}
