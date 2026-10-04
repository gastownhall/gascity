package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Fork PRs (no secrets) read rbe-west's anonymous read-only cache through
// .bazelrc's fork-cache config, which bazel-test.yml selects by writing
// `build --config=fork-cache` to .bazelrc.local. Fork actions hit only if
// they hash like the trusted run's, so the fork .bazelrc.local must carry
// exactly the trusted run's lines minus build:remote-exec, and fork-cache
// itself must upload nothing, carry no credentials, degrade to local
// execution when the endpoint is closed or slow, and never be overridden by
// remote-exec's 3600s timeout.

const (
	bazelTestWorkflow     = ".github/workflows/bazel-test.yml"
	bazelRCConfigStep     = "Configure remote execution or the read-only fork cache"
	bazelRCConfigStepIf   = "env.BAZEL_REMOTE_EXECUTOR != '' || env.BAZEL_FORK_CACHE == 'true'"
	bazelForkCacheLine    = "build --config=fork-cache"
	bazelRCExecAssignment = "RCEXEC=(--config=remote-exec)"
	bazelRCExecGuard      = `if [ -n "$BAZEL_REMOTE_EXECUTOR" ]; then`
	// rbe-fork (fork and Dependabot PRs with a certificate from rbe-west's
	// mint) passes --config=remote-exec too: its .bazelrc.local carries the
	// mint's endpoint, instance and certificate under build:remote-exec and
	// no fork-cache. RBE_FORK_CERT must then be the fork-cert step's output.
	bazelRCExecForkGuard = `if [ -n "$BAZEL_REMOTE_EXECUTOR" ] || [ -n "$RBE_FORK_CERT" ]; then`
	bazelRCForkCertEnv   = "${{ steps.fork-cert.outputs.cert }}"
	// The test step also reads the minted instance: the fork pool
	// (oss-fork, no network) skips the acceptance tier (ga-73eoo).
	bazelRCForkInstanceEnv = "${{ steps.fork-cert.outputs.instance }}"
	bazelRCExecSteps       = 3
	forkCacheMaxTimeoutSec = 15
	// The farm admits 16 connections per source IP and Blacksmith runners
	// share egress IPs.
	forkCacheMaxConnections = 4
)

type bazelTestWorkflowFile struct {
	Jobs map[string]struct {
		Steps []bazelTestWorkflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

type bazelTestWorkflowStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
	// continue-on-error: the rbe-fork steps fall back to the fork cache.
	ContinueOnError bool `yaml:"continue-on-error"`
}

func bazelTestWorkflowSteps(t *testing.T, root string) []bazelTestWorkflowStep {
	t.Helper()
	var wf bazelTestWorkflowFile
	if err := yaml.Unmarshal([]byte(readFile(t, root, bazelTestWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelTestWorkflow, err)
	}
	job, ok := wf.Jobs["bazel"]
	if !ok {
		t.Fatalf("%s has no bazel job", bazelTestWorkflow)
	}
	return job.Steps
}

// runBazelRCConfigStep runs the step's script as Actions does (bash -eo
// pipefail) in a scratch directory with env, its /tmp/ paths redirected
// there, and returns the .bazelrc.local lines it writes with those paths
// mapped back.
func runBazelRCConfigStep(t *testing.T, script string, env map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(script, "/tmp/", dir+"/")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", path)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("step script with %v: %v\n%s", env, err, out)
	}
	rc, err := os.ReadFile(filepath.Join(dir, ".bazelrc.local"))
	if err != nil {
		t.Fatalf("step script with %v wrote no .bazelrc.local: %v", env, err)
	}
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(rc), dir+"/", "/tmp/"), "\n"), "\n")
}

// TestBazelForkCacheRCLocal runs the one step that writes .bazelrc.local in
// trusted and fork mode: the fork lines must be the trusted lines minus
// build:remote-exec, plus build --config=fork-cache.
func TestBazelForkCacheRCLocal(t *testing.T) {
	steps := bazelTestWorkflowSteps(t, repoRoot(t))
	var config *bazelTestWorkflowStep
	for i := range steps {
		if steps[i].Name == bazelRCConfigStep {
			if config != nil {
				t.Fatalf("%s has two %q steps", bazelTestWorkflow, bazelRCConfigStep)
			}
			config = &steps[i]
		}
		if steps[i].Name != bazelRCConfigStep && (strings.Contains(steps[i].Run, "> .bazelrc.local") ||
			strings.Contains(steps[i].Run, "tee .bazelrc.local") || strings.Contains(steps[i].Run, "tee -a .bazelrc.local")) {
			t.Errorf("step %q writes .bazelrc.local; only %q may", steps[i].Name, bazelRCConfigStep)
		}
	}
	if config == nil {
		t.Fatalf("%s has no %q step", bazelTestWorkflow, bazelRCConfigStep)
	}
	if config.If != bazelRCConfigStepIf {
		t.Errorf("%q runs if %q; want %q (with an executor, or for the fork cache)", config.Name, config.If, bazelRCConfigStepIf)
	}

	pem := "eA==" // base64 "x"
	trusted := runBazelRCConfigStep(t, config.Run, map[string]string{
		"BAZEL_REMOTE_EXECUTOR": "grpcs://executor.invalid:443",
		"BAZEL_FORK_CACHE":      "true",
		"RBE_INSTANCE":          "oss",
		"RBE_TLS_CERT":          pem,
		"RBE_TLS_KEY":           pem,
		"RBE_TLS_CA":            pem,
	})
	var want []string
	remoteExec := 0
	for _, line := range trusted {
		if strings.HasPrefix(line, "build:remote-exec ") {
			remoteExec++
			continue
		}
		want = append(want, line)
	}
	for _, line := range []string{
		"build:remote-exec --remote_executor=grpcs://executor.invalid:443",
		"build:remote-exec --tls_client_certificate=/tmp/rbe-cert.pem",
		"test --test_env=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
	} {
		if !strings.Contains("\n"+strings.Join(trusted, "\n")+"\n", "\n"+line+"\n") {
			t.Errorf("trusted .bazelrc.local lacks %q:\n%s", line, strings.Join(trusted, "\n"))
		}
	}
	if remoteExec == 0 || len(want) == 0 {
		t.Fatalf("trusted .bazelrc.local has %d remote-exec and %d shared lines:\n%s", remoteExec, len(want), strings.Join(trusted, "\n"))
	}
	for _, line := range want {
		if strings.Contains(line, "fork-cache") {
			t.Errorf("trusted .bazelrc.local selects the fork cache: %q", line)
		}
	}
	want = append(want, bazelForkCacheLine)

	// A fork (no secrets) and an rbe=cache dispatch (secrets present, executor
	// emptied) must write the same lines; so must a fork whose rbe-fork steps
	// produced no certificate (the mint closed, refused or unreachable: every
	// RBE_FORK_* empty, or a partial set).
	for name, env := range map[string]map[string]string{
		"fork":     {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true"},
		"dispatch": {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true", "RBE_INSTANCE": "oss", "RBE_TLS_CERT": pem, "RBE_TLS_KEY": pem, "RBE_TLS_CA": pem},
		"fork, rbe-fork closed": {
			"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true",
			"RBE_FORK_ENDPOINT": "", "RBE_FORK_INSTANCE": "", "RBE_FORK_CERT_FILE": "", "RBE_FORK_KEY_FILE": "",
		},
	} {
		got := runBazelRCConfigStep(t, config.Run, env)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s .bazelrc.local:\n%s\nwant the trusted lines minus build:remote-exec, plus %q:\n%s",
				name, strings.Join(got, "\n"), bazelForkCacheLine, strings.Join(want, "\n"))
		}
	}

	// rbe-fork (a fork with a minted certificate): the trusted shared lines,
	// so actions hash alike, plus build:remote-exec for the mint's endpoint,
	// instance and certificate; no fork cache, and never the CI secrets even
	// if a run could read them.
	shared := want[:len(want)-1]
	for _, instance := range []string{"oss-fork", "oss"} {
		fork := map[string]string{
			"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true",
			"RBE_FORK_ENDPOINT": rbeForkEndpoint, "RBE_FORK_INSTANCE": instance,
			"RBE_FORK_CERT_FILE": "/runner/rbe-fork/fork.crt", "RBE_FORK_KEY_FILE": "/runner/rbe-fork/fork.key",
			"RBE_TLS_CERT": pem, "RBE_TLS_KEY": pem, "RBE_INSTANCE": "oss",
		}
		got := runBazelRCConfigStep(t, config.Run, fork)
		var gotShared, gotRemote []string
		for _, line := range got {
			if strings.HasPrefix(line, "build:remote-exec ") {
				gotRemote = append(gotRemote, line)
			} else {
				gotShared = append(gotShared, line)
			}
		}
		wantRemote := []string{
			"build:remote-exec --remote_executor=" + rbeForkEndpoint,
			"build:remote-exec --remote_instance_name=" + instance,
			"build:remote-exec --tls_client_certificate=/runner/rbe-fork/fork.crt",
			"build:remote-exec --tls_client_key=/runner/rbe-fork/fork.key",
			"build:remote-exec --remote_max_connections=8",
		}
		if strings.Join(gotShared, "\n") != strings.Join(shared, "\n") || strings.Join(gotRemote, "\n") != strings.Join(wantRemote, "\n") {
			t.Errorf("rbe-fork %s .bazelrc.local:\n%s\nwant the trusted shared lines:\n%s\nand:\n%s",
				instance, strings.Join(got, "\n"), strings.Join(shared, "\n"), strings.Join(wantRemote, "\n"))
		}
	}
}

const (
	rbeForkEndpoint  = "grpcs://rbe-fork.ops.gascity.com:8444"
	rbeForkStatusURL = "https://rbe-mint.ops.gascity.com:8444/v1/status?repo="
	// bazel-test.yml's BAZEL_FORK_REMOTE: fork and Dependabot pull_request
	// runs (no secrets) ask rbe-fork.
	bazelForkRemoteEnv = "${{ github.event_name == 'pull_request' && (github.event.pull_request.head.repo.full_name != github.repository || github.actor == 'dependabot[bot]') && 'true' || '' }}"
)

// bazelTestCurlStub stands in for curl in the rbe-fork status step: it
// records the URL and prints what rbe-fork-mint's /v1/status would for
// BAZEL_TEST_MINT (ro, rw: open; closed, rw-closed: open false, which
// today's mint answers as ro instead while rw is off; canary: a
// 403's body; garbage; evil: open with a tier that is neither); anything
// else is a refused connection (the gate closed, or no DNS yet).
const bazelTestCurlStub = `#!/usr/bin/env bash
echo "$*" >>"$BAZEL_TEST_CURL_LOG"
case "${BAZEL_TEST_MINT:-}" in
ro) echo '{"open": true, "tier": "ro", "instance": "oss-fork", "endpoint": "grpcs://rbe-fork.ops.gascity.com:8444", "reason": "eligible"}' ;;
rw) echo '{"open": true, "tier": "rw", "instance": "oss", "endpoint": "grpcs://rbe-fork.ops.gascity.com:8444", "reason": "eligible"}' ;;
closed) echo '{"open": false, "tier": "ro", "instance": "oss-fork", "endpoint": "grpcs://rbe-fork.ops.gascity.com:8444", "reason": "rbe-fork is closed"}' ;;
rw-closed) echo '{"open": false, "tier": "rw", "instance": "oss", "endpoint": "grpcs://rbe-fork.ops.gascity.com:8444", "reason": "the rw tier is closed"}' ;;
canary) echo '{"error": "rbe-fork canary: this PR is not enabled yet"}' ;;
garbage) echo '<html>bad gateway</html>' ;;
evil) echo '{"open": true, "tier": "admin", "instance": "", "endpoint": "grpcs://elsewhere:1"}' ;;
*) echo "curl: (7) Failed to connect to rbe-mint.ops.gascity.com port 8444" >&2; exit 7 ;;
esac
`

// TestBazelRBEForkSteps: fork and Dependabot PRs try rbe-fork before the
// read-only fork cache, and every failure on the way falls back to it. The
// four rbe-fork steps (status, key and CSR, CSR artifact, certificate) are
// continue-on-error and each runs only on the previous one's output; the
// rc step and the test steps read only the certificate step's outputs, so
// no certificate means exactly today's fork-cache run. The status step is
// run against a stubbed mint: only an open ro or rw answer yields a tier.
func TestBazelRBEForkSteps(t *testing.T) {
	root := repoRoot(t)
	var wf struct {
		Jobs map[string]struct {
			Env   map[string]string       `yaml:"env"`
			Steps []bazelTestWorkflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, bazelTestWorkflow)), &wf); err != nil {
		t.Fatal(err)
	}
	job := wf.Jobs["bazel"]
	if job.Env["BAZEL_FORK_REMOTE"] != bazelForkRemoteEnv {
		t.Errorf("BAZEL_FORK_REMOTE = %q, want %q", job.Env["BAZEL_FORK_REMOTE"], bazelForkRemoteEnv)
	}
	byID := map[string]bazelTestWorkflowStep{}
	order := map[string]int{}
	for i, s := range job.Steps {
		if s.ID != "" {
			byID[s.ID] = s
			order[s.ID] = i
		}
		if s.Name == bazelRCConfigStep {
			order["config"] = i
		}
	}
	type want struct {
		ifExpr string
		env    map[string]string
	}
	for id, w := range map[string]want{
		"fork-status": {
			"env.BAZEL_REMOTE_EXECUTOR == '' && env.BAZEL_FORK_REMOTE == 'true'",
			map[string]string{"PR_NUMBER": "${{ github.event.pull_request.number }}"},
		},
		"fork-key": {
			"steps.fork-status.outputs.tier != ''",
			map[string]string{"BAZEL_CI_SECRET_DIR": "${{ runner.temp }}/rbe-fork"},
		},
		"fork-csr": {"steps.fork-key.outputs.csr != ''", nil},
		"fork-cert": {"steps.fork-csr.outputs.artifact-id != ''", map[string]string{
			"BAZEL_CI_SECRET_DIR": "${{ runner.temp }}/rbe-fork",
			"ARTIFACT_ID":         "${{ steps.fork-csr.outputs.artifact-id }}",
			"RBE_FORK_PR":         "${{ github.event.pull_request.number }}",
			"RBE_FORK_TIER":       "${{ steps.fork-status.outputs.tier }}",
		}},
	} {
		s, ok := byID[id]
		if !ok {
			t.Errorf("%s has no step %s", bazelTestWorkflow, id)
			continue
		}
		if s.If != w.ifExpr || !s.ContinueOnError || len(s.Env) != len(w.env) {
			t.Errorf("step %s: if %q, continue-on-error %v, env %v; want if %q, continue-on-error, env %v", id, s.If, s.ContinueOnError, s.Env, w.ifExpr, w.env)
		}
		for k, v := range w.env {
			if s.Env[k] != v {
				t.Errorf("step %s env %s = %q, want %q", id, k, s.Env[k], v)
			}
		}
		if order[id] > order["config"] {
			t.Errorf("step %s runs after %q", id, bazelRCConfigStep)
		}
	}
	if order["fork-status"] > order["fork-key"] || order["fork-key"] > order["fork-csr"] || order["fork-csr"] > order["fork-cert"] {
		t.Errorf("rbe-fork steps out of order: %v", order)
	}
	if got := byID["fork-key"].Run; strings.TrimSpace(got) != "bash tools/rbe/fork-credential.sh key" {
		t.Errorf("fork-key runs %q", got)
	}
	if got := byID["fork-cert"].Run; strings.TrimSpace(got) != "bash tools/rbe/fork-credential.sh cert" {
		t.Errorf("fork-cert runs %q", got)
	}
	csr := byID["fork-csr"]
	if csr.Uses != "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02 # v4.6.2" && csr.Uses != "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02" ||
		csr.With["name"] != "${{ steps.fork-key.outputs.artifact-name }}" || csr.With["path"] != "${{ steps.fork-key.outputs.csr }}" ||
		csr.With["compression-level"] != "0" || csr.With["if-no-files-found"] != "error" {
		t.Errorf("fork-csr: uses %q with %v; want the pinned upload-artifact of fork-key's csr under its artifact name, stored", csr.Uses, csr.With)
	}
	// The rc step and every --config=remote-exec step read the certificate
	// step's outputs, nothing else of rbe-fork's.
	var config bazelTestWorkflowStep
	for _, s := range job.Steps {
		if s.Name == bazelRCConfigStep {
			config = s
		}
	}
	for k, v := range map[string]string{
		"RBE_FORK_ENDPOINT":  "${{ steps.fork-cert.outputs.endpoint }}",
		"RBE_FORK_INSTANCE":  "${{ steps.fork-cert.outputs.instance }}",
		"RBE_FORK_CERT_FILE": "${{ steps.fork-cert.outputs.cert }}",
		"RBE_FORK_KEY_FILE":  "${{ steps.fork-cert.outputs.key }}",
	} {
		if config.Env[k] != v {
			t.Errorf("%q env %s = %q, want %q", bazelRCConfigStep, k, config.Env[k], v)
		}
	}
	for _, s := range job.Steps {
		for k, v := range s.Env {
			if strings.Contains(v, "steps.fork-") && s.ID != "fork-key" && s.ID != "fork-cert" && s.Name != bazelRCConfigStep && v != bazelRCForkCertEnv && v != bazelRCForkInstanceEnv {
				t.Errorf("step %q env %s reads %q; only the rc step and the remote-exec guards read rbe-fork's outputs", s.Name, k, v)
			}
		}
		if strings.Contains(s.If, "fork-cert") && s.If != "env.BAZEL_REMOTE_EXECUTOR == '' && env.BAZEL_FORK_CACHE == 'true' && steps.fork-cert.outputs.cert == ''" {
			t.Errorf("step %q if %q", s.Name, s.If)
		}
	}

	// The status step, against the stub, in a scratch dir (its
	// $GITHUB_OUTPUT is the helper's .bazelrc.local).
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(bazelTestCurlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	status := byID["fork-status"].Run
	for answer, wantTier := range map[string]string{
		"ro": "ro", "rw": "rw", "closed": "", "rw-closed": "", "canary": "", "garbage": "", "evil": "", "unreachable": "",
	} {
		curlLog := filepath.Join(dir, answer+".log")
		got := runBazelRCConfigStep(t, status, map[string]string{
			"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GITHUB_OUTPUT":       ".bazelrc.local",
			"GITHUB_REPOSITORY":   "gastownhall/gascity",
			"GITHUB_RUN_ID":       "4242",
			"GITHUB_RUN_ATTEMPT":  "1",
			"PR_NUMBER":           "6969",
			"BAZEL_TEST_MINT":     answer,
			"BAZEL_TEST_CURL_LOG": curlLog,
		})
		if strings.Join(got, "\n") != "tier="+wantTier {
			t.Errorf("mint %s: status step outputs %q, want tier=%s", answer, got, wantTier)
		}
		if b, _ := os.ReadFile(curlLog); !strings.Contains(string(b), rbeForkStatusURL+"gascity&run=4242&attempt=1&pr=6969") {
			t.Errorf("mint %s: status step asked %q", answer, b)
		}
		// A closed gate drops the connection: each try gives up in 5 s.
		if b, _ := os.ReadFile(curlLog); !strings.HasPrefix(string(b), "-sS --connect-timeout 5 --max-time 30 ") {
			t.Errorf("mint %s: status step ran curl %q, want --connect-timeout 5 --max-time 30", answer, b)
		}
	}
}

// checkBazelRCExecGuards requires every step that passes
// --config=remote-exec to do so only behind a $BAZEL_REMOTE_EXECUTOR guard
// (or that guard or $RBE_FORK_CERT, the minted rbe-fork certificate, which
// the step must take from the fork-cert step): .bazelrc.local exists in
// fork-cache mode too, and remote-exec's --remote_timeout=3600 would
// override fork-cache's.
func checkBazelRCExecGuards(steps []bazelTestWorkflowStep) []error {
	var errs []error
	guarded := 0
	for _, s := range steps {
		s.Run = stripShellComments(s.Run)
		if strings.Count(s.Run, "--config=remote-exec") != strings.Count(s.Run, bazelRCExecAssignment) {
			errs = append(errs, errors.New("step "+strconv.Quote(s.Name)+" passes --config=remote-exec outside "+bazelRCExecAssignment))
		}
		if !strings.Contains(s.Run, bazelRCExecAssignment) {
			continue
		}
		guarded++
		if strings.Contains(s.Run, "-f .bazelrc.local") || strings.Contains(s.Run, "-e .bazelrc.local") || strings.Contains(s.Run, "-s .bazelrc.local") {
			errs = append(errs, errors.New("step "+strconv.Quote(s.Name)+" keys --config=remote-exec on .bazelrc.local, which fork-cache runs write too"))
		}
		prev := ""
		for _, line := range strings.Split(s.Run, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.Contains(line, bazelRCExecAssignment) {
				switch {
				case strings.HasPrefix(line, bazelRCExecGuard) || prev == bazelRCExecGuard:
				case strings.HasPrefix(line, bazelRCExecForkGuard) || prev == bazelRCExecForkGuard:
					if s.Env["RBE_FORK_CERT"] != bazelRCForkCertEnv {
						errs = append(errs, errors.New("step "+strconv.Quote(s.Name)+" guards on $RBE_FORK_CERT, which is "+
							strconv.Quote(s.Env["RBE_FORK_CERT"])+", not "+bazelRCForkCertEnv))
					}
				default:
					errs = append(errs, errors.New("step "+strconv.Quote(s.Name)+" sets "+bazelRCExecAssignment+" without "+bazelRCExecGuard+" or "+bazelRCExecForkGuard))
				}
			}
			prev = line
		}
	}
	if guarded != bazelRCExecSteps {
		errs = append(errs, errors.New(strconv.Itoa(guarded)+" steps set "+bazelRCExecAssignment+"; want "+strconv.Itoa(bazelRCExecSteps)+" (update bazelRCExecSteps if a bazel step was added or removed)"))
	}
	return errs
}

// stripShellComments drops the whole-line comments of a run script.
func stripShellComments(run string) string {
	var kept []string
	for _, line := range strings.Split(run, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestBazelForkCacheRCExecGuards(t *testing.T) {
	for _, err := range checkBazelRCExecGuards(bazelTestWorkflowSteps(t, repoRoot(t))) {
		t.Error(err)
	}

	block := "if [ -n \"$BAZEL_REMOTE_EXECUTOR\" ]; then\n  RCEXEC=(--config=remote-exec)\nelse\n  RCEXEC=()\nfi\nbazel test //... \"${RCEXEC[@]}\"\n"
	inline := "if [ -n \"$BAZEL_REMOTE_EXECUTOR\" ]; then RCEXEC=(--config=remote-exec); else RCEXEC=(); fi\n"
	good := []bazelTestWorkflowStep{{Name: "a", Run: block}, {Name: "b", Run: inline}, {Name: "c", Run: inline}, {Name: "d", Run: "echo hi\n"}}
	if errs := checkBazelRCExecGuards(good); len(errs) != 0 {
		t.Errorf("good fixture: %v", errs)
	}
	forkEnv := map[string]string{"RBE_FORK_CERT": bazelRCForkCertEnv}
	forkInline := strings.Replace(inline, bazelRCExecGuard, bazelRCExecForkGuard, 1)
	forkBlock := strings.Replace(block, bazelRCExecGuard, bazelRCExecForkGuard, 1)
	goodFork := append([]bazelTestWorkflowStep(nil), good...)
	goodFork[0] = bazelTestWorkflowStep{Name: "a", Run: forkBlock, Env: forkEnv}
	goodFork[1] = bazelTestWorkflowStep{Name: "b", Run: forkInline, Env: forkEnv}
	if errs := checkBazelRCExecGuards(goodFork); len(errs) != 0 {
		t.Errorf("good rbe-fork fixture: %v", errs)
	}
	for name, steps := range map[string][]bazelTestWorkflowStep{
		"fork guard, no env":         {goodFork[0], {Name: "b", Run: forkInline}, good[2], good[3]},
		"fork guard, other env":      {goodFork[0], {Name: "b", Run: forkInline, Env: map[string]string{"RBE_FORK_CERT": "${{ secrets.RBE_TLS_CERT }}"}}, good[2], good[3]},
		"fork guard, other variable": {goodFork[0], {Name: "b", Run: strings.Replace(forkInline, "$RBE_FORK_CERT", "$RBE_FORK_KEY", 1), Env: forkEnv}, good[2], good[3]},
		"fork guard alone":           {goodFork[0], {Name: "b", Run: strings.Replace(inline, "$BAZEL_REMOTE_EXECUTOR", "$RBE_FORK_CERT", 1), Env: forkEnv}, good[2], good[3]},
	} {
		if len(checkBazelRCExecGuards(steps)) == 0 {
			t.Errorf("%s: expected an error", name)
		}
	}
	with := func(i int, run string) []bazelTestWorkflowStep {
		out := append([]bazelTestWorkflowStep(nil), good...)
		out[i].Run = run
		return out
	}
	for name, steps := range map[string][]bazelTestWorkflowStep{
		"file guard":    with(1, "if [ -f .bazelrc.local ]; then RCEXEC=(--config=remote-exec); else RCEXEC=(); fi\n"),
		"unguarded":     with(1, "RCEXEC=(--config=remote-exec)\n"),
		"other guard":   with(0, strings.Replace(block, "$BAZEL_REMOTE_EXECUTOR", "$RBE_TLS_CERT", 1)),
		"literal flag":  with(3, "bazel test //... --config=remote-exec\n"),
		"missing step":  good[:2],
		"extra step":    append(append([]bazelTestWorkflowStep(nil), good...), bazelTestWorkflowStep{Name: "e", Run: inline}),
		"negated guard": with(2, strings.Replace(inline, "-n", "-z", 1)),
		"commented out": with(0, "# "+block),
	} {
		if len(checkBazelRCExecGuards(steps)) == 0 {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// checkBazelForkCacheConfig checks .bazelrc's fork-cache config: a remote
// cache with no local-result uploads, both local fallbacks (without them a
// closed endpoint fails every action in GetCapabilities), the failure
// circuit breaker and a short --remote_timeout (a slow endpoint), few
// connections, and no executor or credentials. Only bazel-test.yml's
// .bazelrc.local may select it.
func checkBazelForkCacheConfig(bazelrc string) []error {
	var errs []error
	var opts []string
	for _, line := range strings.Split(bazelrc, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || fields[0] == "import" || fields[0] == "try-import" {
			continue
		}
		_, config, _ := strings.Cut(fields[0], ":")
		for i := 1; i < len(fields); i++ {
			flag := fields[i]
			if flag == "--config" && i+1 < len(fields) {
				i++
				flag += "=" + fields[i]
			}
			if flag == "--config=fork-cache" {
				errs = append(errs, errors.New(fields[0]+" expands --config=fork-cache; only bazel-test.yml's .bazelrc.local may"))
			}
			if config == "fork-cache" {
				opts = append(opts, flag)
			}
		}
	}
	if len(opts) == 0 {
		return append(errs, errors.New(".bazelrc has no fork-cache config"))
	}
	for _, flag := range opts {
		name, _, _ := strings.Cut(flag, "=")
		if name == "--remote_executor" || strings.HasPrefix(name, "--tls_") || strings.HasSuffix(name, "_header") ||
			strings.HasPrefix(name, "--credential_helper") || strings.HasPrefix(name, "--google_") || strings.HasPrefix(name, "--bes_") {
			errs = append(errs, errors.New("fork-cache sets "+flag+"; the fork cache is anonymous and executes nothing remotely"))
		}
	}
	if forkCacheLastValue(opts, "--remote_cache") == "" {
		errs = append(errs, errors.New("fork-cache sets no --remote_cache"))
	}
	for name, want := range map[string]bool{
		"remote_upload_local_results":                         false,
		"remote_local_fallback":                               true,
		"incompatible_remote_local_fallback_for_remote_cache": true,
	} {
		if got, set := forkCacheBoolFinal(opts, name); !set || got != want {
			form := "--" + name
			if !want {
				form = "--no" + name
			}
			errs = append(errs, errors.New("fork-cache must end with "+form))
		}
	}
	if got := forkCacheLastValue(opts, "--experimental_circuit_breaker_strategy"); got != "failure" {
		errs = append(errs, errors.New("fork-cache must end with --experimental_circuit_breaker_strategy=failure"))
	}
	for flag, max := range map[string]int{
		"--remote_timeout":         forkCacheMaxTimeoutSec,
		"--remote_max_connections": forkCacheMaxConnections,
	} {
		if n, err := strconv.Atoi(forkCacheLastValue(opts, flag)); err != nil || n < 1 || n > max {
			errs = append(errs, errors.New("fork-cache must end with "+flag+" of 1-"+strconv.Itoa(max)))
		}
	}
	return errs
}

// forkCacheBoolFinal returns the last setting of the boolean flag name
// (without dashes) in opts, and whether any sets it.
func forkCacheBoolFinal(opts []string, name string) (value, set bool) {
	for _, flag := range opts {
		switch flag {
		case "--" + name, "--" + name + "=true", "--" + name + "=1", "--" + name + "=yes":
			value, set = true, true
		case "--no" + name, "--" + name + "=false", "--" + name + "=0", "--" + name + "=no":
			value, set = false, true
		}
	}
	return value, set
}

// forkCacheLastValue returns the value of the last flag=value in opts, or "".
func forkCacheLastValue(opts []string, flag string) string {
	value := ""
	for _, o := range opts {
		if v, ok := strings.CutPrefix(o, flag+"="); ok {
			value = v
		}
	}
	return value
}

func TestBazelForkCacheConfig(t *testing.T) {
	for _, err := range checkBazelForkCacheConfig(readFile(t, repoRoot(t), ".bazelrc")) {
		t.Error(err)
	}

	ep := "grpc" + "s://cache.example:8443"
	good := "build:fork-cache --remote_cache=" + ep + "\n" +
		"build:fork-cache --noremote_upload_local_results\n" +
		"build:fork-cache --remote_local_fallback\n" +
		"build:fork-cache --incompatible_remote_local_fallback_for_remote_cache\n" +
		"build:fork-cache --remote_timeout=15 --remote_retries=2\n" +
		"build:fork-cache --experimental_circuit_breaker_strategy=failure\n" +
		"build:fork-cache --remote_max_connections=4\n" +
		"build:remote-exec --remote_timeout=3600\n" +
		"try-import %workspace%/.bazelrc.local\n"
	if errs := checkBazelForkCacheConfig(good); len(errs) != 0 {
		t.Errorf("good fixture: %v", errs)
	}
	drop := func(line string) string { return strings.Replace(good, line+"\n", "", 1) }
	for name, rc := range map[string]string{
		"missing":             "build:remote-exec --remote_timeout=3600\n",
		"no endpoint":         drop("build:fork-cache --remote_cache=" + ep),
		"no upload switch":    drop("build:fork-cache --noremote_upload_local_results"),
		"uploads again":       good + "build:fork-cache --remote_upload_local_results\n",
		"no local fallback":   drop("build:fork-cache --remote_local_fallback"),
		"no cache fallback":   drop("build:fork-cache --incompatible_remote_local_fallback_for_remote_cache"),
		"fallback off":        good + "build:fork-cache --noremote_local_fallback\n",
		"no breaker":          drop("build:fork-cache --experimental_circuit_breaker_strategy=failure"),
		"no timeout":          strings.Replace(good, "--remote_timeout=15 ", "", 1),
		"slow timeout":        good + "build:fork-cache --remote_timeout=60\n",
		"no connection cap":   drop("build:fork-cache --remote_max_connections=4"),
		"too many conns":      good + "build:fork-cache --remote_max_connections=8\n",
		"executor":            good + "build:fork-cache --remote_executor=" + ep + "\n",
		"client cert":         good + "build:fork-cache --tls_client_certificate=/x.crt\n",
		"client key":          good + "build:fork-cache --tls_client_key=/x.key\n",
		"ca":                  good + "build:fork-cache --tls_certificate_authority=/x.pem\n",
		"header":              good + "build:fork-cache --remote_header=x-api-key=abc\n",
		"cache header":        good + "build:fork-cache --remote_cache_header=x-api-key=abc\n",
		"credential helper":   good + "build:fork-cache --credential_helper=/x\n",
		"plain build expands": good + "build --config=fork-cache\n",
		"common expands":      good + "common --config fork-cache\n",
		"config expands":      good + "build:ci --config=fork-cache\n",
	} {
		if len(checkBazelForkCacheConfig(rc)) == 0 {
			t.Errorf("%s: expected an error for .bazelrc fixture:\n%s", name, rc)
		}
	}
}
