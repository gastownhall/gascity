//go:build acceptance_c

// Tier C acceptance tests — real inference agents.
//
// These start cities with real AI models (haiku) and verify end-to-end
// outcomes: work dispatched → agent picks up → implements → result appears.
// Assertions are loose (eventual consistency) because model behavior is
// non-deterministic.
//
// Requires: gc binary, bd binary, tmux, dolt, Synthetic/Anthropic env
// credentials (ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN), or Claude OAuth.
// Expected duration: ~5 min per scenario.
// Trigger: manual (make test-acceptance-c), then nightly.
package tierc_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var testEnvC *helpers.Env

const tierCAcceptanceConfig = `
[session]
startup_timeout = "3m"
`

func TestMain(m *testing.M) {
	// Tier C needs real inference. Accept either:
	// 1. ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN env var (CI mode)
	// 2. GC_TIERC_FORCE=1 env var (local OAuth mode — user asserts Claude is authed)
	// 3. Detect OAuth: check if ~/.claude/ exists with credentials
	apiKey := firstNonEmpty(
		strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")),
	)
	forceRun := os.Getenv("GC_TIERC_FORCE") == "1"
	hasOAuth := oauthCredentialsExist()

	if apiKey == "" && !forceRun && !hasOAuth {
		// No credentials available, skip silently.
		os.Exit(0)
	}

	tmpRoot, err := acceptanceTempRoot()
	if err != nil {
		panic("acceptance-c: preparing temp root: " + err.Error())
	}
	if err := os.Setenv("TMPDIR", tmpRoot); err != nil {
		panic("acceptance-c: setting TMPDIR: " + err.Error())
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "gcac-*")
	if err != nil {
		panic("acceptance-c: creating temp dir: " + err.Error())
	}
	if os.Getenv("GC_ACCEPTANCE_KEEP") != "1" {
		defer os.RemoveAll(tmpDir)
	}

	gcBinary := helpers.BuildGC(tmpDir)

	gcHome := filepath.Join(tmpDir, "gc-home")
	runtimeDir := filepath.Join(tmpDir, "runtime")
	for _, d := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			panic("acceptance-c: " + err.Error())
		}
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		panic("acceptance-c: " + err.Error())
	}

	// Configure dolt identity in the isolated home (dolt requires user.name).
	doltCfgDir := filepath.Join(gcHome, ".dolt")
	if err := os.MkdirAll(doltCfgDir, 0o755); err != nil {
		panic("acceptance-c: " + err.Error())
	}
	doltCfg := `{"user.name":"gc-test","user.email":"gc-test@test.local"}`
	if err := os.WriteFile(filepath.Join(doltCfgDir, "config_global.json"), []byte(doltCfg), 0o644); err != nil {
		panic("acceptance-c: " + err.Error())
	}

	// Force a token refresh before staging credentials. Claude Code
	// refreshes tokens in-memory but may not persist to .credentials.json,
	// leaving the on-disk token expired. A quick --print call forces the
	// refresh and (in newer versions) persists it.
	if refreshOut, err := exec.Command("claude", "--print", "ok").CombinedOutput(); err != nil {
		if apiKey != "" {
			panic(fmt.Sprintf("acceptance-c: provider preflight failed: %v\n%s", err, refreshOut))
		}
		fmt.Fprintf(os.Stderr, "acceptance-c: OAuth preflight refresh failed: %v\n%s\n", err, refreshOut)
	}

	// Symlink the host's .claude dir so the test always sees fresh OAuth
	// tokens (including tokens refreshed by aimux during the test run).
	// Copying credentials leads to stale token failures on long test suites.
	realHome, _ := os.UserHomeDir()
	srcClaudeDir := filepath.Join(realHome, ".claude")
	dstClaudeDir := filepath.Join(gcHome, ".claude")
	if _, err := os.Stat(srcClaudeDir); err == nil {
		if err := os.Symlink(srcClaudeDir, dstClaudeDir); err != nil {
			// Fall back to copy if symlink fails (e.g., cross-device).
			if err2 := stageClaudeOAuth(realHome, gcHome); err2 != nil {
				panic("acceptance-c: staging Claude oauth: " + err2.Error())
			}
		}
	} else if err := stageClaudeOAuth(realHome, gcHome); err != nil {
		panic("acceptance-c: staging Claude oauth: " + err.Error())
	}
	// Keep onboarding state isolated from the host, then force the minimal
	// accepted/trusted flags so workers do not stall on first-run UI.
	if err := copyFileIfExists(filepath.Join(realHome, ".claude.json"), filepath.Join(gcHome, ".claude.json"), 0o600); err != nil {
		panic("acceptance-c: staging Claude state: " + err.Error())
	}
	if err := helpers.EnsureClaudeStateFile(gcHome); err != nil {
		panic("acceptance-c: ensuring Claude state: " + err.Error())
	}

	testEnvC = helpers.NewEnv(gcBinary, gcHome, runtimeDir).
		Without("GC_SESSION"). // use real tmux, not subprocess
		Without("GC_BEADS").   // use real bd (dolt-backed) provider
		Without("GC_DOLT")     // let gc manage dolt (don't skip it)

	if apiKey != "" {
		testEnvC = testEnvC.With("ANTHROPIC_API_KEY", apiKey)
	}
	for _, key := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			testEnvC = testEnvC.With(key, v)
		}
	}
	testEnvC = testEnvC.With("DOLT_ROOT_PATH", gcHome) // dolt reads config from $DOLT_ROOT_PATH/.dolt/

	// Ensure tmux is available.
	if _, err := exec.LookPath("tmux"); err != nil {
		panic("acceptance-c: tmux not found")
	}

	code := m.Run()

	helpers.RunGC(testEnvC, "", "supervisor", "stop") //nolint:errcheck
	os.Exit(code)
}

// TestSwarm_SlingWorkCoderCommits verifies the swarm end-to-end:
// sling a task → coder picks up → creates a file → committer commits.
//
// This is a loose assertion test: we don't verify intermediate steps,
// only that a commit eventually appears with the expected content.
func TestSwarm_SlingWorkCoderCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("Tier C: skipping in short mode")
	}

	// Create a throwaway git repo as the rig.
	rigDir := setupThrowawayRepo(t)
	rigName := filepath.Base(rigDir)

	// Init a swarm city.
	c := helpers.NewCity(t, testEnvC)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "swarm"))
	applyTierCAcceptanceConfig(c)

	// Add the rig via gc rig add (initializes beads, hooks, routes).
	c.RigAdd(rigDir, "packs/swarm")

	// Limit pool sizes to reduce cost.
	c.AppendToConfig("\n[[rigs.overrides]]\nagent = \"coder\"\n[rigs.overrides.pool]\nmin = 1\nmax = 1\n")

	c.StartForeground()

	// Wait for supervisor + dolt + agents to initialize.
	time.Sleep(15 * time.Second)

	// Sling work to the coder pool.
	out, err := c.GC("sling", rigName+"/coder", "Create a file called hello.txt with the text 'hello world'")
	if err != nil {
		t.Fatalf("gc sling: %v\n%s", err, out)
	}
	t.Logf("Slung work: %s", strings.TrimSpace(out))

	// Poll for outcome: a commit should eventually appear that creates hello.txt.
	deadline := 8 * time.Minute
	found := pollForCondition(t, deadline, 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(rigDir, "hello.txt"))
		return err == nil
	})

	if !found {
		gitLog := gitCmd(t, rigDir, "log", "--oneline", "-10")
		status, _ := c.GC("status")
		rigBeads, _ := bdCmd(testEnvC, rigDir, "list", "--json", "--limit=50")
		sessionDiag := gatherSessionDiagnostics(t, c, c.Dir, "repo/coder", "repo/committer")
		t.Fatalf("hello.txt not created within %s\ngit log:\n%s\nstatus:\n%s\nrig beads:\n%s\n%s", deadline, gitLog, status, rigBeads, sessionDiag)
	}

	t.Logf("hello.txt created successfully")
	gitLog := gitCmd(t, rigDir, "log", "--oneline", "-5")
	t.Logf("Recent commits:\n%s", gitLog)
}

// TestGastown_PolecatImplementsRefineryMerges verifies the gastown flow:
// dispatch work to polecat pool → polecat creates branch + commits →
// reassigns to refinery → refinery merges to default branch.
func TestGastown_PolecatImplementsRefineryMerges(t *testing.T) {
	if testing.Short() {
		t.Skip("Tier C: skipping in short mode")
	}

	rigDir := setupThrowawayRepo(t)
	rigName := filepath.Base(rigDir)

	c := newGastownAcceptanceCity(t)
	unregisterOut, unregisterErr := c.GC("unregister", c.Dir)
	require.NoError(t, unregisterErr, "gc unregister after init: %s", unregisterOut)

	// Add the rig via gc rig add (initializes beads, hooks, routes).
	c.RigAdd(rigDir, "packs/gastown")

	// Start with polecat suspended so we can verify the attached-formula
	// queue invariants before any worker claims the work.
	c.AppendToConfig("\n[[rigs.overrides]]\nagent = \"polecat\"\nsuspended = true\n[rigs.overrides.pool]\nmin = 0\nmax = 1\n")

	c.StartForeground()

	require.Eventually(t, func() bool {
		_, err := bdCmd(testEnvC, rigDir, "list", "--json", "--limit=1")
		return err == nil
	}, 2*time.Minute, 2*time.Second, "rig bead store did not become ready")

	// Sling attached formula work while the pool is suspended.
	out, err := c.GC("sling", rigName+"/polecat", "Create a file called feature.txt containing 'new feature'", "--on", "mol-polecat-work")
	if err != nil {
		t.Fatalf("gc sling: %v\n%s", err, out)
	}
	t.Logf("Slung work to polecat: %s", strings.TrimSpace(out))

	routeKey := rigName + "/polecat"
	readyOut, err := bdCmd(testEnvC, rigDir, "ready", "--metadata-field", "gc.routed_to="+routeKey, "--unassigned", "--json", "--limit=20")
	require.NoError(t, err, "bd ready")
	var ready []beadJSON
	require.NoError(t, json.Unmarshal([]byte(readyOut), &ready), "unmarshal ready queue")
	require.Len(t, ready, 1, "expected only the outer bead in the routed ready queue")
	require.NotContains(t, ready[0].ID, ".", "expected outer bead id, not a step id")

	outerID := ready[0].ID
	outerOut, err := bdCmd(testEnvC, rigDir, "show", outerID, "--json")
	require.NoError(t, err, "bd show outer")
	var outer []beadJSON
	require.NoError(t, json.Unmarshal([]byte(outerOut), &outer), "unmarshal outer bead")
	require.Len(t, outer, 1, "expected one outer bead")
	moleculeID := metaString(outer[0].Metadata, "molecule_id")
	require.NotEmpty(t, moleculeID, "outer bead should carry molecule_id metadata")

	rootOut, err := bdCmd(testEnvC, rigDir, "show", moleculeID, "--json")
	require.NoError(t, err, "bd show molecule root")
	var root []beadJSON
	require.NoError(t, json.Unmarshal([]byte(rootOut), &root), "unmarshal molecule root")
	require.Len(t, root, 1, "expected one molecule root")
	require.Empty(t, strings.TrimSpace(root[0].ParentID), "attached molecule root should not have a parent")

	// Enable polecat and restart the city so execution can begin.
	c.WriteConfig(strings.Replace(c.ReadFile("city.toml"),
		"\n[[rigs.overrides]]\nagent = \"polecat\"\nsuspended = true\n[rigs.overrides.pool]\nmin = 0\nmax = 1\n",
		"\n[[rigs.overrides]]\nagent = \"polecat\"\n[rigs.overrides.pool]\nmin = 1\nmax = 1\n",
		1,
	))
	c.StartForeground()

	// Poll for outcome: refinery must eventually merge the work to origin/main.
	// 18 minutes: Synthetic-backed workers can take longer to start and
	// complete the polecat -> witness -> refinery chain than the original
	// Anthropic-backed budget this test was written around.
	deadline := 18 * time.Minute
	merged := pollForCondition(t, deadline, 15*time.Second, func() bool {
		_ = gitCmd(t, rigDir, "fetch", "origin")
		content := gitCmd(t, rigDir, "show", "origin/main:feature.txt")
		return strings.TrimSpace(content) == "new feature"
	})

	if !merged {
		_ = gitCmd(t, rigDir, "fetch", "origin")
		gitLog := gitCmd(t, rigDir, "log", "--all", "--oneline", "-10")
		branches := gitCmd(t, rigDir, "branch", "-a")
		originMain := gitCmd(t, rigDir, "log", "--oneline", "-5", "origin/main")
		status, _ := c.GC("status")
		outerFinal, _ := bdCmd(testEnvC, rigDir, "show", outerID, "--json")
		refineryAssigned, _ := bdCmd(testEnvC, rigDir, "list", "--assignee=repo/refinery", "--json", "--limit=20")
		refineryInProgress, _ := bdCmd(testEnvC, rigDir, "list", "--status=in_progress", "--assignee=repo/refinery", "--json", "--limit=20")
		sessionDiag := gatherSessionDiagnostics(t, c, c.Dir, "mayor", "repo/witness", "repo/refinery", "repo/polecat")
		t.Fatalf("feature.txt was not merged to origin/main within %s\nbranches:\n%s\ngit log:\n%s\norigin/main:\n%s\nstatus:\n%s",
			deadline, branches, gitLog, originMain, status+
				"\nouter bead:\n"+outerFinal+
				"\nrefinery assigned:\n"+refineryAssigned+
				"\nrefinery in_progress:\n"+refineryInProgress+
				"\n"+sessionDiag)
	}

	t.Log("Refinery merged feature.txt to origin/main")
	mainLog := gitCmd(t, rigDir, "log", "--oneline", "-5", "origin/main")
	t.Logf("origin/main commits:\n%s", mainLog)
}

// TestGastown_PolecatLifecycle verifies the full polecat lifecycle:
// prime -> work -> gt done. This is the test that would have caught
// regressions in polecat session management and worktree creation.
func TestGastown_PolecatLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Tier C: skipping in short mode")
	}

	rigDir := setupThrowawayRepo(t)

	// Write a simple Go file with a TODO for the polecat to fix.
	mainGo := filepath.Join(rigDir, "main.go")
	os.WriteFile(mainGo, []byte("package main\n\n// TODO: add a hello function\nfunc main() {}\n"), 0o644)
	gitCmd(t, rigDir, "add", ".")
	gitCmd(t, rigDir, "commit", "-m", "add main.go with TODO")

	rigName := filepath.Base(rigDir)

	c := newGastownAcceptanceCity(t)
	c.RigAdd(rigDir, "packs/gastown")
	seedGastownClaudeProjects(t, c, rigName)

	// Limit pool to 1 polecat, cap cost.
	c.AppendToConfig("\n[[rigs.overrides]]\nagent = \"polecat\"\n[rigs.overrides.pool]\nmin = 1\nmax = 1\n")

	c.StartWithSupervisor()
	time.Sleep(15 * time.Second) // Wait for init.

	// Sling a small, verifiable task.
	out, err := c.GC("sling", rigName+"/polecat", "Add a function called Hello that prints 'hello world' to main.go")
	require.NoError(t, err, "gc sling: %s", out)
	t.Logf("Slung work: %s", strings.TrimSpace(out))

	// Poll: a new branch should appear (polecat creates a worktree branch).
	deadline := 5 * time.Minute
	branchCreated := pollForCondition(t, deadline, 10*time.Second, func() bool {
		branches := gitCmd(t, rigDir, "branch", "--list", "--no-color", "-a")
		for _, line := range strings.Split(branches, "\n") {
			branch := strings.TrimSpace(strings.TrimPrefix(line, "*"))
			if branch != "" && branch != "main" && branch != "master" {
				return true
			}
		}
		return false
	})

	if !branchCreated {
		status, _ := c.GC("status")
		t.Fatalf("polecat did not create a branch within %s\nstatus:\n%s", deadline, status)
	}

	t.Log("Polecat lifecycle test passed: branch created")
}

// TestGastown_MayorDispatchPipeline tests the full mayor -> polecat -> refinery
// pipeline: send mail to mayor, mayor dispatches work, bead appears.
func TestGastown_MayorDispatchPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("Tier C: skipping in short mode")
	}

	rigDir := setupThrowawayRepo(t)

	// Add a simple file for mayor to dispatch work about.
	os.WriteFile(filepath.Join(rigDir, "app.py"), []byte("# TODO: add a greet function\n"), 0o644)
	gitCmd(t, rigDir, "add", ".")
	gitCmd(t, rigDir, "commit", "-m", "add app.py")

	rigName := filepath.Base(rigDir)

	c := newGastownAcceptanceCity(t)
	c.RigAdd(rigDir, "packs/gastown")
	seedGastownClaudeProjects(t, c, rigName)

	// Limit pool sizes.
	c.AppendToConfig("\n[[rigs.overrides]]\nagent = \"polecat\"\n[rigs.overrides.pool]\nmin = 1\nmax = 1\n")

	c.StartWithSupervisor()
	time.Sleep(15 * time.Second)

	// Send mail to mayor asking to implement a feature.
	out, err := c.GC("mail", "send", "mayor", "Please add a greet() function to app.py that prints 'hello'")
	if err != nil {
		t.Fatalf("gc mail send: %v\n%s", err, out)
	}
	t.Logf("Sent mail to mayor: %s", strings.TrimSpace(out))

	// Poll: eventually a bead should be created (mayor dispatches work).
	deadline := 12 * time.Minute
	beadCreated := pollForCondition(t, deadline, 15*time.Second, func() bool {
		out, err := c.GC("bd", "list", "--rig", rigName)
		if err != nil {
			return false
		}
		// Look for any bead (mayor creates one from the mail).
		return strings.Contains(out, "open") || strings.Contains(out, "closed")
	})

	if !beadCreated {
		status, _ := c.GC("status")
		rigBeads, _ := bdCmd(testEnvC, rigDir, "list", "--json", "--limit=50")
		mayorInbox, mayorInboxErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "mail", "inbox", "mayor")
		if mayorInboxErr != nil {
			mayorInbox = strings.TrimSpace(mayorInbox + "\nERR: " + mayorInboxErr.Error())
		}
		sessionDiag := gatherSessionDiagnostics(t, c, c.Dir, "mayor", "repo/witness", "repo/refinery", "repo/polecat")
		t.Fatalf("mayor did not dispatch work within %s\nstatus:\n%s\nrig beads:\n%s\nmayor inbox:\n%s\n%s", deadline, status, rigBeads, mayorInbox, sessionDiag)
	}

	t.Log("Mayor dispatch pipeline test passed: work dispatched")
}

// --- helpers ---

func setupThrowawayRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	originDir := filepath.Join(root, "origin.git")
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "init", "--bare", "--initial-branch=main", originDir)
	gitCmd(t, root, "clone", originDir, repoDir)
	gitCmd(t, repoDir, "config", "user.email", "test@test.com")
	gitCmd(t, repoDir, "config", "user.name", "Test")
	readme := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(readme, []byte("# Test Repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "initial commit")
	gitCmd(t, repoDir, "push", "-u", "origin", "main")
	return repoDir
}

func newGastownAcceptanceCity(t *testing.T) *helpers.City {
	t.Helper()
	c := helpers.NewCity(t, testEnvC)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))
	applyTierCAcceptanceConfig(c)
	seedClaudeProjectState(t, c, filepath.Join(c.Dir, ".gc", "agents", "mayor"))
	seedClaudeProjectState(t, c, filepath.Join(c.Dir, ".gc", "agents", "deacon"))
	seedClaudeProjectState(t, c, filepath.Join(c.Dir, ".gc", "agents", "boot"))
	return c
}

func applyTierCAcceptanceConfig(c *helpers.City) {
	c.AppendToConfig(tierCAcceptanceConfig)
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out)
	}
	return strings.TrimSpace(string(out))
}

func pollForCondition(t *testing.T, timeout, interval time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

type beadJSON struct {
	ID       string         `json:"id"`
	ParentID string         `json:"parent_id"`
	Status   string         `json:"status"`
	Assignee string         `json:"assignee"`
	Title    string         `json:"title"`
	Labels   []string       `json:"labels"`
	Metadata map[string]any `json:"metadata"`
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	v, ok := meta[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func bdCmd(env *helpers.Env, dir string, args ...string) (string, error) {
	bdPath := "bd"
	if path, err := exec.LookPath("bd"); err == nil {
		bdPath = path
	}
	cmd := exec.Command(bdPath, args...)
	cmd.Dir = dir
	cmd.Env = env.List()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gatherSessionDiagnostics(t *testing.T, c *helpers.City, beadDir string, templates ...string) string {
	t.Helper()

	var b strings.Builder

	sessionOut, sessionErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "list")
	if sessionErr != nil {
		sessionOut = strings.TrimSpace(sessionOut + "\nERR: " + sessionErr.Error())
	}
	b.WriteString("sessions:\n")
	b.WriteString(sessionOut)
	b.WriteString("\n")

	sessionBeadsOut, sessionBeadsErr := bdCmd(testEnvC, beadDir, "list", "--include-infra", "--label", "gc:session", "--json", "--limit=50")
	if sessionBeadsErr != nil {
		sessionBeadsOut = strings.TrimSpace(sessionBeadsOut + "\nERR: " + sessionBeadsErr.Error())
	}
	b.WriteString("\nsession beads:\n")
	b.WriteString(sessionBeadsOut)
	b.WriteString("\n")

	templateSet := make(map[string]struct{}, len(templates))
	for _, template := range templates {
		templateSet[template] = struct{}{}
	}

	matched := 0
	if sessionBeadsErr == nil {
		for _, bead := range parseBeadListJSON(t, sessionBeadsOut) {
			template := metaString(bead.Metadata, "template")
			if _, ok := templateSet[template]; !ok {
				continue
			}
			matched++
			fmt.Fprintf(&b, "\nmatched session bead (%s):\n%+v\n", template, bead)
			sessionName := metaString(bead.Metadata, "session_name")
			if sessionName == "" {
				b.WriteString("session_name metadata: <empty>\n")
				continue
			}
			logsOut, logsErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "logs", sessionName, "--tail", "0")
			if logsErr != nil {
				logsOut = strings.TrimSpace(logsOut + "\nERR: " + logsErr.Error())
			}
			fmt.Fprintf(&b, "session logs (%s):\n%s\n", sessionName, logsOut)

			peekOut, peekErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "peek", sessionName, "--lines", "200")
			if peekErr != nil {
				peekOut = strings.TrimSpace(peekOut + "\nERR: " + peekErr.Error())
			}
			fmt.Fprintf(&b, "session peek (%s):\n%s\n", sessionName, peekOut)
		}
	}
	if matched == 0 {
		b.WriteString("\nmatched session beads: none\n")
	}

	supervisorOut, supervisorErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "supervisor", "logs")
	if supervisorErr != nil {
		supervisorOut = strings.TrimSpace(supervisorOut + "\nERR: " + supervisorErr.Error())
	}
	b.WriteString("\nsupervisor logs:\n")
	b.WriteString(supervisorOut)
	b.WriteString("\n")

	controllerLog := tailFile(filepath.Join(c.Dir, ".gc", "acceptance-controller.log"), 200)
	b.WriteString("\ncontroller log tail:\n")
	b.WriteString(controllerLog)
	b.WriteString("\n")

	return b.String()
}

func seedGastownClaudeProjects(t *testing.T, c *helpers.City, rigName string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(c.Dir, ".gc", "agents", rigName, "witness"),
		filepath.Join(c.Dir, ".gc", "worktrees", rigName, "refinery"),
		filepath.Join(c.Dir, ".gc", "worktrees", rigName, "polecats", "polecat"),
	} {
		seedClaudeProjectState(t, c, path)
	}
}

func seedClaudeProjectState(t *testing.T, c *helpers.City, projectPath string) {
	t.Helper()
	require.NoError(t, helpers.EnsureClaudeProjectState(c.Env, projectPath), "seed Claude project state for %s", projectPath)
}

// oauthCredentialsExist checks if Claude CLI OAuth credentials are
// available at ~/.claude/.credentials.json. When running locally with
// Claude Max, ANTHROPIC_API_KEY is not set, but the CLI authenticates
// via these OAuth tokens.
func oauthCredentialsExist() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, candidate := range []string{
		filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".claude", "credentials.json"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func stageClaudeOAuth(realHome, gcHome string) error {
	srcClaudeDir := filepath.Join(realHome, ".claude")
	dstClaudeDir := filepath.Join(gcHome, ".claude")
	if err := os.MkdirAll(dstClaudeDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{".credentials.json", "settings.json"} {
		if err := copyFileIfExists(filepath.Join(srcClaudeDir, name), filepath.Join(dstClaudeDir, name), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func copyFileIfExists(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(dst, data, perm)
}

func acceptanceTempRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_TMPDIR"))
	if root == "" {
		root = filepath.Join("/tmp", "gcac")
		if err := os.MkdirAll(root, 0o755); err != nil {
			root = filepath.Join(os.TempDir(), "gcac")
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

func tailFile(path string, maxLines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) <= maxLines {
		return string(data)
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n")
}
