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
	bazelTestWorkflow      = ".github/workflows/bazel-test.yml"
	bazelRCConfigStep      = "Configure remote execution or the read-only fork cache"
	bazelRCConfigStepIf    = "env.BAZEL_REMOTE_EXECUTOR != '' || env.BAZEL_FORK_CACHE == 'true'"
	bazelForkCacheLine     = "build --config=fork-cache"
	bazelRCExecAssignment  = "RCEXEC=(--config=remote-exec)"
	bazelRCExecGuard       = `if [ -n "$BAZEL_REMOTE_EXECUTOR" ]; then`
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
	Name string `yaml:"name"`
	If   string `yaml:"if"`
	Run  string `yaml:"run"`
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
	// emptied) must write the same lines.
	for name, env := range map[string]map[string]string{
		"fork":     {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true"},
		"dispatch": {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true", "RBE_INSTANCE": "oss", "RBE_TLS_CERT": pem, "RBE_TLS_KEY": pem, "RBE_TLS_CA": pem},
	} {
		got := runBazelRCConfigStep(t, config.Run, env)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s .bazelrc.local:\n%s\nwant the trusted lines minus build:remote-exec, plus %q:\n%s",
				name, strings.Join(got, "\n"), bazelForkCacheLine, strings.Join(want, "\n"))
		}
	}
}

// checkBazelRCExecGuards requires every step that passes
// --config=remote-exec to do so only behind a $BAZEL_REMOTE_EXECUTOR guard:
// .bazelrc.local exists in fork-cache mode too, and remote-exec's
// --remote_timeout=3600 would override fork-cache's.
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
			if strings.Contains(line, bazelRCExecAssignment) &&
				!strings.HasPrefix(line, bazelRCExecGuard) && prev != bazelRCExecGuard {
				errs = append(errs, errors.New("step "+strconv.Quote(s.Name)+" sets "+bazelRCExecAssignment+" without "+bazelRCExecGuard))
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
