package scripts_test

import (
	"bytes"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	bazelRCExecSteps     = 3
	// zstd cache transfers: only the anonymous fork cache (rbe-cache :8443)
	// can advertise a compressor, so only fork-cache may ask for one, and
	// only while cache-zstd-probe.sh finds it advertised (a probe, not a
	// repository variable: fork pull_request runs see no vars). Trusted
	// remote-exec and rbe-fork never: their schedulers advertise none, and
	// Bazel then refuses the remote.
	bazelCacheZstdLine     = "build:fork-cache --remote_cache_compression"
	bazelCacheZstdProbe    = "tools/rbe/cache-zstd-probe.sh"
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

// runWorkflowStepScript runs a workflow step's script as Actions does (bash
// --noprofile --norc -eo pipefail) in dir, with this process's PATH and env
// (env's PATH, if set, replaces it), and returns its combined output.
func runWorkflowStepScript(t *testing.T, dir, script string, env map[string]string) (string, error) {
	t.Helper()
	path := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", path)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runBazelRCConfigStep runs the step's script as Actions does (bash -eo
// pipefail) in a scratch directory with env, its /tmp/ paths redirected
// there, and returns the .bazelrc.local lines it writes with those paths
// mapped back.
func runBazelRCConfigStep(t *testing.T, script string, env map[string]string) []string {
	t.Helper()
	lines, _ := runBazelRCConfigStepProbed(t, script, env)
	return lines
}

// bazelCacheZstdProbeStub stands in for cache-zstd-probe.sh in the rc step:
// it counts its runs and passes only for BAZEL_TEST_PROBE=zstd (rbe-cache
// advertising zstd). TestCacheZstdProbe runs the real probe.
const bazelCacheZstdProbeStub = `#!/usr/bin/env bash
echo probe >>probe.log
[ "${BAZEL_TEST_PROBE:-}" = zstd ]
`

// runBazelRCConfigStepProbed is runBazelRCConfigStep that also returns how
// often the step ran the zstd probe (a stub; no test reaches rbe-cache).
func runBazelRCConfigStepProbed(t *testing.T, script string, env map[string]string) ([]string, int) {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, bazelCacheZstdProbe)
	if err := os.MkdirAll(filepath.Dir(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte(bazelCacheZstdProbeStub), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkflowStepScript(t, dir, strings.ReplaceAll(script, "/tmp/", dir+"/"), env)
	if err != nil {
		t.Fatalf("step script with %v: %v\n%s", env, err, out)
	}
	rc, err := os.ReadFile(filepath.Join(dir, ".bazelrc.local"))
	if err != nil {
		t.Fatalf("step script with %v wrote no .bazelrc.local: %v", env, err)
	}
	probes, _ := os.ReadFile(filepath.Join(dir, "probe.log"))
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(rc), dir+"/", "/tmp/"), "\n"), "\n"), strings.Count(string(probes), "probe\n")
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

	// Fork pull_request runs see no repository variables, so nothing here
	// may hang on one (the zstd switch is the probe).
	for k, v := range config.Env {
		if strings.Contains(v, "vars.") {
			t.Errorf("%q env %s = %q reads a repository variable; fork runs see none", config.Name, k, v)
		}
	}

	if want := "if bash " + bazelCacheZstdProbe + "; then"; strings.Count(config.Run, want) != 1 {
		t.Errorf("%q does not gate the fork cache's zstd line on %q once", config.Name, want)
	}

	// rbe-cache advertising zstd (the stub probe passes) in every mode
	// below: only fork-cache may ask it, and trusted and rbe-fork runs must
	// not even probe.
	pem := "eA==" // base64 "x"
	trusted, probes := runBazelRCConfigStepProbed(t, config.Run, map[string]string{
		"BAZEL_REMOTE_EXECUTOR": "grpcs://executor.invalid:443",
		"BAZEL_FORK_CACHE":      "true",
		"RBE_INSTANCE":          "oss",
		"RBE_TLS_CERT":          pem,
		"RBE_TLS_KEY":           pem,
		"RBE_TLS_CA":            pem,
		"BAZEL_TEST_PROBE":      "zstd",
	})
	for _, line := range trusted {
		if strings.Contains(line, "remote_cache_compression") {
			t.Errorf("trusted .bazelrc.local asks for compression (%q); rbe-west's trusted schedulers advertise none", line)
		}
	}
	if probes != 0 {
		t.Errorf("the trusted run probed rbe-cache %d times; only fork-cache runs may", probes)
	}
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
	forkCacheModes := map[string]map[string]string{
		"fork":     {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true"},
		"dispatch": {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true", "RBE_INSTANCE": "oss", "RBE_TLS_CERT": pem, "RBE_TLS_KEY": pem, "RBE_TLS_CA": pem},
		"fork, rbe-fork closed": {
			"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true",
			"RBE_FORK_ENDPOINT": "", "RBE_FORK_INSTANCE": "", "RBE_FORK_CERT_FILE": "", "RBE_FORK_KEY_FILE": "",
		},
	}
	for name, env := range forkCacheModes {
		got, probes := runBazelRCConfigStepProbed(t, config.Run, env)
		if probes != 1 {
			t.Errorf("%s probed rbe-cache %d times, want once", name, probes)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s .bazelrc.local:\n%s\nwant the trusted lines minus build:remote-exec, plus %q:\n%s",
				name, strings.Join(got, "\n"), bazelForkCacheLine, strings.Join(want, "\n"))
		}
		// A passing probe (rbe-cache advertises zstd) adds the fork cache's
		// zstd line, and only a passing one (above: it fails).
		zstd := map[string]string{"BAZEL_TEST_PROBE": "zstd"}
		for k, v := range env {
			zstd[k] = v
		}
		got = runBazelRCConfigStep(t, config.Run, zstd)
		if w := append(append([]string(nil), want...), bazelCacheZstdLine); strings.Join(got, "\n") != strings.Join(w, "\n") {
			t.Errorf("%s, rbe-cache advertising zstd .bazelrc.local:\n%s\nwant:\n%s", name, strings.Join(got, "\n"), strings.Join(w, "\n"))
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
		fork["BAZEL_TEST_PROBE"] = "zstd"
		got, probes := runBazelRCConfigStepProbed(t, config.Run, fork)
		if probes != 0 {
			t.Errorf("rbe-fork %s probed rbe-cache %d times; only fork-cache runs may", instance, probes)
		}
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
			if strings.Contains(v, "steps.fork-") && s.ID != "fork-key" && s.ID != "fork-cert" && s.Name != bazelRCConfigStep && v != bazelRCForkCertEnv {
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
		if strings.Contains(name, "remote_cache_compression") {
			errs = append(errs, errors.New("fork-cache sets "+flag+"; bazel-test.yml adds it while rbe-cache advertises zstd, so rollback needs no revert"))
		}
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
		"zstd in .bazelrc":    good + "build:fork-cache --remote_cache_compression\n",
		"plain build expands": good + "build --config=fork-cache\n",
		"common expands":      good + "common --config fork-cache\n",
		"config expands":      good + "build:ci --config=fork-cache\n",
	} {
		if len(checkBazelForkCacheConfig(rc)) == 0 {
			t.Errorf("%s: expected an error for .bazelrc fixture:\n%s", name, rc)
		}
	}
}

// TestBazelRemoteCacheCompressionOnlyForkCache: --remote_cache_compression
// appears in one place, the rc step's fork-cache line behind the zstd probe.
// No .bazelrc config and no other workflow or action may set it, since every
// other remote (rbe-west's trusted schedulers on :443, rbe-fork on :8444)
// advertises no compressor and Bazel then refuses the remote.
func TestBazelRemoteCacheCompressionOnlyForkCache(t *testing.T) {
	root := repoRoot(t)
	files := []string{".bazelrc"}
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml", ".github/actions/*/action.yml", ".github/actions/*/action.yaml"} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m {
			rel, err := filepath.Rel(root, f)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, rel)
		}
	}
	var found []string
	for _, f := range files {
		for i, line := range strings.Split(readFile(t, root, f), "\n") {
			if strings.Contains(line, "remote_cache_compression") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				found = append(found, f+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(found) != 1 || !strings.HasPrefix(found[0], bazelTestWorkflow+":") || !strings.HasSuffix(found[0], "echo '"+bazelCacheZstdLine+"'") {
		t.Errorf("--remote_cache_compression set at %q; want only bazel-test.yml's echo '%s'", found, bazelCacheZstdLine)
	}
	for _, s := range bazelTestWorkflowSteps(t, root) {
		if strings.Contains(s.Run, "remote_cache_compression") && s.Name != bazelRCConfigStep {
			t.Errorf("step %q sets --remote_cache_compression; only %q may, for fork-cache", s.Name, bazelRCConfigStep)
		}
	}
}

// refusedProbeURL: a loopback port nothing listens on, the probe's default
// in tests (a refused connection: no flag).
const refusedProbeURL = "https://127.0.0.1:1"

// GetCapabilities bodies for a stand-in rbe-cache. capsLive is what rbe-cache
// answered on 2026-10-04, before it advertised zstd (cache_capabilities:
// SHA256 and BLAKE3, action cache read-only, 64 MiB batches, symlinks
// allowed; API 2.0 to 2.3); capsZstd is the same with supported_compressors
// [ZSTD].
var (
	capsLiveCache       = []byte{0x0a, 0x02, 0x01, 0x09, 0x12, 0x02, 0x08, 0x01, 0x20, 0x80, 0x80, 0x04, 0x28, 0x01}
	capsLiveAPIVersions = []byte{0x22, 0x02, 0x08, 0x02, 0x2a, 0x04, 0x08, 0x02, 0x10, 0x03}
	capsLive            = capsWithCache(capsLiveCache)
	capsZstd            = capsWithCache(pbBytes(6, []byte{1}), capsLiveCache)
	// GetCapabilitiesRequest{instance_name: "oss"}, fork-cache's instance.
	capsRequest = grpcMessage(pbBytes(1, []byte("oss")))
)

// capsWithCache: a ServerCapabilities with the live API versions and a
// cache_capabilities of the given fields.
func capsWithCache(cache ...[]byte) []byte {
	return append(pbBytes(1, bytes.Join(cache, nil)), capsLiveAPIVersions...)
}

func pbBytes(field int, b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field)<<3|2)
	return append(binary.AppendUvarint(out, uint64(len(b))), b...)
}

func pbVarint(field int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(field)<<3), v)
}

// grpcMessage frames m as one uncompressed gRPC message.
func grpcMessage(m []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{0}, uint32(len(m))), m...)
}

// capsAnswer is how TestCacheZstdProbe's stand-in rbe-cache answers GetCapabilities: HTTP
// status (0: 200), body, and grpc-status ("": none; with no body, in the
// headers, as gRPC's trailers-only errors are), after delay.
type capsAnswer struct {
	status int
	grpc   string
	body   []byte
	delay  time.Duration
}

// requireCacheZstdProbeTools skips where the probe cannot run at all (it
// then leaves the flag off, which the rc-step tests above already cover).
func requireCacheZstdProbeTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

// TestCacheZstdProbe runs cache-zstd-probe.sh against a stand-in rbe-cache.
// Only an answer listing ZSTD in cache_capabilities.supported_compressors
// (the field Bazel checks) passes; every other answer, error, refusal or
// timeout fails, and the rc step then writes no flag. The probe never writes
// stdout (the rc step's stdout is .bazelrc.local) and always says why on
// stderr.
func TestCacheZstdProbe(t *testing.T) {
	requireCacheZstdProbeTools(t)
	root := repoRoot(t)
	script := filepath.Join(root, bazelCacheZstdProbe)

	// The probe asks what fork-cache uses: rbe-cache, instance oss.
	probeText := readFile(t, root, bazelCacheZstdProbe)
	for _, want := range []string{
		"url=${RBE_CACHE_PROBE_URL:-https://rbe-cache.ops.gascity.com:8443}\n",
		`printf '\000\000\000\000\005\012\003oss'`,
		"--connect-timeout 3 --max-time \"$max_time\"",
		"max_time=${RBE_CACHE_PROBE_MAX_TIME:-5}\n",
	} {
		if !strings.Contains(probeText, want) {
			t.Errorf("%s lacks %q", bazelCacheZstdProbe, want)
		}
	}
	if !bytes.Equal(capsRequest, []byte("\x00\x00\x00\x00\x05\x0a\x03oss")) {
		t.Fatalf("capsRequest = %x", capsRequest)
	}
	bazelrc := readFile(t, root, ".bazelrc")
	for _, want := range []string{"\nbuild:fork-cache --remote_cache=grpcs://rbe-cache.ops.gascity.com:8443\n", "\nbuild:fork-cache --remote_instance_name=oss\n"} {
		if !strings.Contains(bazelrc, want) {
			t.Errorf(".bazelrc lacks %q, which the probe asks", strings.TrimSpace(want))
		}
	}

	// serve answers like rbe-cache's Caddy, over TLS and HTTP/2, after
	// checking the request is the probe's GetCapabilities for instance oss.
	// It returns the probe's RBE_CACHE_PROBE_URL, a CA file for curl's
	// CURL_CA_BUNDLE, and the request count.
	serve := func(t *testing.T, a capsAnswer) (url, caFile string, hits *atomic.Int32) {
		t.Helper()
		hits = new(atomic.Int32)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			body, _ := io.ReadAll(r.Body)
			const path = "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"
			if r.ProtoMajor != 2 || r.Method != http.MethodPost || r.URL.Path != path ||
				r.Header.Get("Content-Type") != "application/grpc" || !bytes.Equal(body, capsRequest) {
				t.Errorf("probe sent %s %s %s, content-type %q, body %x; want HTTP/2 POST %s, application/grpc, %x",
					r.Proto, r.Method, r.URL.Path, r.Header.Get("Content-Type"), body, path, capsRequest)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			select {
			case <-time.After(a.delay):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/grpc")
			trailer := a.grpc != "" && a.body != nil
			if trailer {
				w.Header().Set("Trailer", "Grpc-Status")
			} else if a.grpc != "" {
				w.Header().Set("Grpc-Status", a.grpc)
			}
			status := a.status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write(a.body)
			// Streamed, as gRPC servers answer: Go drops the trailer of a
			// response it can give a content-length.
			w.(http.Flusher).Flush()
			if trailer {
				w.Header().Set("Grpc-Status", a.grpc)
			}
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		caFile = filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
			t.Fatal(err)
		}
		return srv.URL, caFile, hits
	}

	zstd := grpcMessage(capsZstd)
	cases := []struct {
		name   string
		answer capsAnswer
		want   bool
	}{
		{"rbe-cache before zstd", capsAnswer{grpc: "0", body: grpcMessage(capsLive)}, false},
		{"zstd advertised", capsAnswer{grpc: "0", body: zstd}, true},
		{"zstd among others, unpacked", capsAnswer{grpc: "0", body: grpcMessage(capsWithCache(capsLiveCache, pbVarint(6, 2), pbVarint(6, 1)))}, true},
		{"deflate only", capsAnswer{grpc: "0", body: grpcMessage(capsWithCache(capsLiveCache, pbBytes(6, []byte{2})))}, false},
		{"zstd for batch updates only", capsAnswer{grpc: "0", body: grpcMessage(capsWithCache(capsLiveCache, pbBytes(7, []byte{1})))}, false},
		{"zstd outside cache_capabilities", capsAnswer{grpc: "0", body: grpcMessage(append(capsLive, pbBytes(2, pbBytes(6, []byte{1}))...))}, false},
		{"gRPC error after the answer", capsAnswer{grpc: "13", body: zstd}, false},
		{"no grpc-status", capsAnswer{body: zstd}, false},
		{"trailers-only UNIMPLEMENTED", capsAnswer{grpc: "12"}, false},
		{"HTTP 502", capsAnswer{status: http.StatusBadGateway, grpc: "0", body: zstd}, false},
		{"compressed message", capsAnswer{grpc: "0", body: append([]byte{1}, zstd[1:]...)}, false},
		{"truncated message", capsAnswer{grpc: "0", body: zstd[:len(zstd)-1]}, false},
		{"truncated field", capsAnswer{grpc: "0", body: grpcMessage(capsZstd[:4])}, false},
		{"not gRPC", capsAnswer{grpc: "0", body: []byte("<html>bad gateway</html>")}, false},
		{"timeout", capsAnswer{grpc: "0", body: zstd, delay: 10 * time.Second}, false},
	}
	run := func(t *testing.T, env ...string) (bool, string, string) {
		t.Helper()
		dir := t.TempDir()
		envMap := map[string]string{"RBE_CACHE_PROBE_MAX_TIME": "1"}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			envMap[k] = v
		}
		// Run through the shared step runner, with the probe's own stdout
		// and stderr split to files: it must write nothing to stdout, so
		// runWorkflowStepScript's combined output cannot tell the two apart.
		wrapper := "bash " + strconv.Quote(script) + " >stdout.out 2>stderr.out\n"
		out, err := runWorkflowStepScript(t, dir, wrapper, envMap)
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("run %s: %v\n%s", bazelCacheZstdProbe, err, out)
		}
		if out != "" {
			t.Errorf("wrapper for %s wrote %q", bazelCacheZstdProbe, out)
		}
		stdout, _ := os.ReadFile(filepath.Join(dir, "stdout.out"))
		stderr, _ := os.ReadFile(filepath.Join(dir, "stderr.out"))
		return err == nil, string(stdout), string(stderr)
	}
	check := func(t *testing.T, ok bool, stdout, stderr string, want bool) {
		t.Helper()
		if ok != want {
			t.Errorf("probe passed: %v, want %v; stderr:\n%s", ok, want, stderr)
		}
		if stdout != "" {
			t.Errorf("probe wrote stdout %q; the rc step would write it to .bazelrc.local", stdout)
		}
		verdict := "; the fork cache stays identity\n"
		if want {
			verdict = "; the fork cache uses zstd\n"
		}
		if !strings.HasPrefix(stderr, "rbe-cache zstd probe: ") || !strings.HasSuffix(stderr, verdict) {
			t.Errorf("probe stderr %q; want one verdict line ending %q", stderr, verdict)
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, ca, hits := serve(t, c.answer)
			start := time.Now()
			ok, stdout, stderr := run(t, "RBE_CACHE_PROBE_URL="+url, "CURL_CA_BUNDLE="+ca)
			check(t, ok, stdout, stderr, c.want)
			if hits.Load() != 1 {
				t.Errorf("probe asked %d times, want 1", hits.Load())
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("probe took %v; RBE_CACHE_PROBE_MAX_TIME=1 bounds it", d)
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		ok, stdout, stderr := run(t, "RBE_CACHE_PROBE_URL="+refusedProbeURL)
		check(t, ok, stdout, stderr, false)
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		url, _, hits := serve(t, capsAnswer{grpc: "0", body: zstd})
		ok, stdout, stderr := run(t, "RBE_CACHE_PROBE_URL="+url)
		check(t, ok, stdout, stderr, false)
		if hits.Load() != 0 {
			t.Errorf("probe completed a request through an untrusted certificate")
		}
	})
}
