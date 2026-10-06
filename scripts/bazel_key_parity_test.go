package scripts_test

import (
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Test results computed at any phase (pre-push, PR, main) are reused by the
// others only if every phase hashes its actions identically. So every flag
// that can change an action key lives unconditionally in the committed
// .bazelrc, and the per-mode configs (remote-exec, fork-cache), the CI policy
// config bazel.yml's lanes pass (ci), the lines CI and developers write to
// the gitignored .bazelrc.local and the rc setup-bazel generates for
// bazel.yml carry transport and result policy only: endpoints, credentials,
// timeouts, download and parallelism policy, retries and test-result reuse.
//
// Classification fails closed: a flag not known to be transport-only counts
// as key-affecting.

// bazelPinnedTestPath is the PATH every test action sees: the remote
// workers' Go at /usr/local/go and the system directories. Forwarding the
// client's PATH (a bare --test_env=PATH) gives every machine its own keys.
const bazelPinnedTestPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

// bazelNonKeyConfigs select how actions run, where results come from and
// whether a result is reused or retried; none may change what an action is.
// remote-exec and fork-cache are the remote modes (CI and pre-push); ci is
// bazel.yml's lane policy.
var bazelNonKeyConfigs = []string{"remote-exec", "fork-cache", "ci"}

// bazelClientEnvAllowed may be forwarded from the client environment: it is a
// debug override (internal/bazeltest) that no phase sets, so an unset value
// keeps it out of every action.
var bazelClientEnvAllowed = map[string]bool{"GC_TEST_REPO_ROOT": true}

// bazelTransportFlag reports whether flag (--name, --noname or --name=value)
// cannot change an action key.
//
// The result-policy flags were checked against Bazel 9.2.0 on
// //scripts/cipolicy:cipolicy_test: --flaky_test_attempts (any value) and
// --[no]cache_test_results leave the aquery action keys and the executed
// TestRunner spawn (arguments, environment, input digests, outputs,
// platform, timeout: what the remote action cache hashes) unchanged, while a
// different --test_env=PATH changes both. --profile only writes a local file.
func bazelTransportFlag(flag string) bool {
	name, _, _ := strings.Cut(flag, "=")
	switch name {
	case "--remote_default_exec_properties", "--remote_default_platform_properties":
		return false // platform properties are part of the action
	case "--jobs", "--experimental_circuit_breaker_strategy", "--disk_cache", "--keep_going", "--nokeep_going",
		"--flaky_test_attempts", "--cache_test_results", "--nocache_test_results", "--profile":
		return true
	}
	for _, prefix := range []string{"--remote_", "--experimental_remote_", "--incompatible_remote_", "--tls_", "--credential_helper", "--google_", "--bes_", "--build_event_", "--grpc_keepalive_"} {
		if strings.HasPrefix(name, prefix) || strings.HasPrefix(name, "--no"+strings.TrimPrefix(prefix, "--")) {
			return true
		}
	}
	return false
}

// bazelRCOption is one flag of one .bazelrc line: its command and config
// ("build:remote-exec" is build, remote-exec; "test" is test, "").
type bazelRCOption struct {
	command, config, flag string
}

func parseBazelRC(rc string) []bazelRCOption {
	var opts []bazelRCOption
	for _, line := range strings.Split(rc, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || fields[0] == "import" || fields[0] == "try-import" {
			continue
		}
		command, config, _ := strings.Cut(fields[0], ":")
		for i := 1; i < len(fields); i++ {
			if strings.HasPrefix(fields[i], "#") {
				break
			}
			flag := fields[i]
			if !strings.Contains(flag, "=") && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") && !strings.HasPrefix(fields[i+1], "#") {
				i++
				flag += "=" + fields[i]
			}
			opts = append(opts, bazelRCOption{command, config, flag})
		}
	}
	return opts
}

// checkBazelKeyParity checks the committed .bazelrc: the non-key configs
// are transport and policy only, the unconditional test PATH is the pinned one, and no
// unconditional line forwards client environment into actions.
func checkBazelKeyParity(bazelrc string) []error {
	var errs []error
	modes := map[string]int{}
	for _, m := range bazelNonKeyConfigs {
		modes[m] = 0
	}
	path := ""
	for _, o := range parseBazelRC(bazelrc) {
		if _, ok := modes[o.config]; ok {
			modes[o.config]++
			if !bazelTransportFlag(o.flag) {
				errs = append(errs, errors.New(o.command+":"+o.config+" sets "+o.flag+", which can change action keys; put it unconditionally in .bazelrc so every phase hashes alike"))
			}
			continue
		}
		if o.config != "" {
			continue
		}
		name, value, hasValue := strings.Cut(o.flag, "=")
		switch name {
		case "--test_env", "--action_env", "--host_action_env", "--repo_env":
			envName, envValue, set := strings.Cut(value, "=")
			if !hasValue || envName == "" {
				errs = append(errs, errors.New(o.command+" "+o.flag+" names no variable"))
				continue
			}
			if !set && !bazelClientEnvAllowed[envName] {
				errs = append(errs, errors.New(o.command+" "+o.flag+" forwards the client's "+envName+" into action keys; pin a value"))
			}
			if name == "--test_env" && envName == "PATH" {
				path = envValue
			}
		}
	}
	for _, m := range bazelNonKeyConfigs {
		if modes[m] == 0 {
			errs = append(errs, errors.New(".bazelrc has no "+m+" config"))
		}
	}
	if path != bazelPinnedTestPath {
		errs = append(errs, errors.New(".bazelrc must end with an unconditional --test_env=PATH="+bazelPinnedTestPath+"; got "+strconv.Quote(path)))
	}
	return errs
}

// checkBazelRCLocalLines checks lines destined for .bazelrc.local: only
// build:remote-exec and build:fork-cache transport flags and the fork-cache
// selection. bazel-test.yml's probe-gated `build:fork-cache
// --remote_cache_compression` (zstd while rbe-cache advertises it) is such a
// transport flag: compression changes the bytes on the wire, never a digest.
func checkBazelRCLocalLines(lines []string) []error {
	var errs []error
	for _, line := range lines {
		if strings.TrimSpace(line) == bazelForkCacheLine {
			continue
		}
		opts := parseBazelRC(line)
		if len(opts) == 0 {
			errs = append(errs, errors.New(".bazelrc.local line "+strconv.Quote(line)+" sets nothing"))
		}
		for _, o := range opts {
			if o.command != "build" || (o.config != "remote-exec" && o.config != "fork-cache") || !bazelTransportFlag(o.flag) {
				errs = append(errs, errors.New(".bazelrc.local line "+strconv.Quote(line)+" is not a build:remote-exec or build:fork-cache transport flag; everything else is shared and belongs in .bazelrc"))
			}
		}
	}
	return errs
}

func TestBazelKeyParity(t *testing.T) {
	for _, err := range checkBazelKeyParity(readFile(t, repoRoot(t), ".bazelrc")) {
		t.Error(err)
	}

	ep := "grpc" + "s://cache.example:8443"
	good := "test --test_env=GC_TEST_REPO_ROOT\n" +
		"test --test_env=PATH=" + bazelPinnedTestPath + "\n" +
		"build:remote-exec --remote_timeout=3600 --noremote_upload_local_results\n" +
		"build:remote-exec --remote_download_minimal --jobs=64\n" +
		"build:fork-cache --remote_cache=" + ep + " --remote_instance_name oss\n" +
		"build:fork-cache --noremote_local_fallback --experimental_circuit_breaker_strategy=failure\n" +
		"test:ci --flaky_test_attempts=1\n" +
		"test:ci --experimental_remote_cache_eviction_retries=0\n" +
		"build:other --define=gotags=x\n" +
		"try-import %workspace%/.bazelrc.local\n"
	if errs := checkBazelKeyParity(good); len(errs) != 0 {
		t.Errorf("good fixture: %v", errs)
	}
	for name, rc := range map[string]string{
		"bare PATH":            strings.Replace(good, "PATH="+bazelPinnedTestPath, "PATH", 1),
		"other PATH":           strings.Replace(good, bazelPinnedTestPath, "/opt/go/bin:/usr/bin", 1),
		"no PATH":              strings.Replace(good, "test --test_env=PATH="+bazelPinnedTestPath+"\n", "", 1),
		"PATH re-forwarded":    good + "test --test_env=PATH\n",
		"PATH only in config":  strings.Replace(good, "test --test_env=PATH=", "test:ci --test_env=PATH=", 1),
		"client action env":    good + "build --action_env=HOME\n",
		"client test env":      good + "test --test_env=USER\n",
		"remote-exec test env": good + "test:remote-exec --test_env=PATH=" + bazelPinnedTestPath + "\n",
		"fork-cache define":    good + "build:fork-cache --define=gotags=x\n",
		"remote-exec platform": good + "build:remote-exec --extra_execution_platforms=//:rbe\n",
		"exec properties":      good + "build:remote-exec --remote_default_exec_properties=OSFamily=linux\n",
		"remote-exec config":   good + "build:remote-exec --config=other\n",
		"strict env off":       good + "build:fork-cache --noincompatible_strict_action_env\n",
		"no remote-exec":       strings.ReplaceAll(good, "build:remote-exec", "build:gone"),
		"no fork-cache":        strings.ReplaceAll(good, "build:fork-cache", "build:gone"),
		"no ci":                strings.ReplaceAll(good, "test:ci", "test:gone"),
		"ci PATH copy":         good + "test:ci --test_env=PATH=" + bazelPinnedTestPath + "\n",
		"ci define":            good + "test:ci --define=gotags=x\n",
		"ci test timeout":      good + "test:ci --test_timeout=1100\n",
		"ci action env":        good + "build:ci --action_env=GOFLAGS=-mod=mod\n",
	} {
		if len(checkBazelKeyParity(rc)) == 0 {
			t.Errorf("%s: expected an error for .bazelrc fixture:\n%s", name, rc)
		}
	}

	for name, lines := range map[string][]string{
		"executor":  {"build:remote-exec --remote_executor=" + ep, "build:remote-exec --remote_instance_name=oss"},
		"mtls":      {"build:remote-exec --tls_client_certificate=/x.crt", "build:remote-exec --tls_client_key=/x.key", "build:remote-exec --tls_certificate_authority=/x.pem"},
		"fork":      {bazelForkCacheLine},
		"fork zstd": {bazelForkCacheLine, bazelCacheZstdLine},
		"conns":     {"build:remote-exec --remote_max_connections=8"},
		"no lines":  nil,
		"two flags": {"build:remote-exec --remote_executor=" + ep + " --remote_instance_name=oss"},
	} {
		if errs := checkBazelRCLocalLines(lines); len(errs) != 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	for name, lines := range map[string][]string{
		"test PATH":      {"test --test_env=PATH=" + bazelPinnedTestPath},
		"plain build":    {"build --remote_download_minimal"},
		"plain jobs":     {"build --jobs=64"},
		"define":         {"build:remote-exec --define=gotags=x"},
		"fork define":    {"build:fork-cache --define=gotags=x"},
		"fork test env":  {"test:fork-cache --test_env=PATH=" + bazelPinnedTestPath},
		"platform":       {"build:remote-exec --extra_execution_platforms=//:rbe"},
		"exec props":     {"build:remote-exec --remote_default_exec_properties=a=b"},
		"other config":   {"build:trusted --remote_executor=" + ep},
		"test command":   {"test:remote-exec --remote_executor=" + ep},
		"other selector": {"build --config=remote-exec"},
		"comment only":   {"# nothing"},
	} {
		if len(checkBazelRCLocalLines(lines)) == 0 {
			t.Errorf("%s: expected an error for .bazelrc.local lines %q", name, lines)
		}
	}
}

// TestBazelCIRCLocalCarriesOnlyTransport runs bazel-test.yml's rc step in
// every mode, with rbe-cache advertising zstd and without: whatever it
// writes must be transport-only, so trusted, rbe-fork and fork-cache runs
// (and developers) hash actions alike.
func TestBazelCIRCLocalCarriesOnlyTransport(t *testing.T) {
	steps := bazelTestWorkflowSteps(t, repoRoot(t))
	var script string
	for _, s := range steps {
		if s.Name == bazelRCConfigStep {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatalf("%s has no %q step", bazelTestWorkflow, bazelRCConfigStep)
	}
	pem := "eA=="
	for name, env := range map[string]map[string]string{
		"trusted": {"BAZEL_REMOTE_EXECUTOR": "grpcs://executor.invalid:443", "BAZEL_FORK_CACHE": "true", "RBE_INSTANCE": "oss", "RBE_TLS_CERT": pem, "RBE_TLS_KEY": pem, "RBE_TLS_CA": pem},
		"fork":    {"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true"},
		"rbe-fork": {
			"BAZEL_REMOTE_EXECUTOR": "", "BAZEL_FORK_CACHE": "true",
			"RBE_FORK_ENDPOINT": rbeForkEndpoint, "RBE_FORK_INSTANCE": "oss-fork",
			"RBE_FORK_CERT_FILE": "/runner/fork.crt", "RBE_FORK_KEY_FILE": "/runner/fork.key",
		},
	} {
		for _, probe := range []string{"", "zstd"} {
			env["BAZEL_TEST_PROBE"] = probe
			lines := runBazelRCConfigStep(t, script, env)
			for _, err := range checkBazelRCLocalLines(lines) {
				t.Errorf("%s (probe %q): %v", name, probe, err)
			}
			if name == "fork" && probe == "zstd" && !slices.Contains(lines, bazelCacheZstdLine) {
				t.Errorf("fork with rbe-cache advertising zstd wrote no %q, so this test no longer classifies it:\n%s", bazelCacheZstdLine, strings.Join(lines, "\n"))
			}
		}
	}
}

// checkSetupBazelRCLines checks the rc .github/actions/setup-bazel writes for
// bazel.yml's lanes (outside the workspace, passed by its bazel wrapper):
// startup heap sizing, repository-fetch settings, build:remote-exec transport
// and the selection of a remote-mode config. Repository fetching feeds no
// action key except through fetched content, which go.sum and Bazel's sha256
// checks pin.
func checkSetupBazelRCLines(lines []string) []error {
	var errs []error
	for _, line := range lines {
		switch strings.TrimSpace(line) {
		case "build --config=remote-exec", bazelForkCacheLine:
			continue
		}
		for _, o := range parseBazelRC(line) {
			name, value, _ := strings.Cut(o.flag, "=")
			ok := false
			switch {
			case o.command == "startup" && o.config == "":
				ok = name == "--host_jvm_args"
			case o.command == "common" && o.config == "":
				switch name {
				case "--repository_cache", "--repo_contents_cache", "--http_timeout_scaling":
					ok = true
				case "--repo_env":
					// A pinned value for repository rules; never forwarded.
					_, _, ok = strings.Cut(value, "=")
				}
			case o.command == "build" && o.config == "remote-exec":
				ok = bazelTransportFlag(o.flag)
			}
			if !ok {
				errs = append(errs, errors.New("setup-bazel rc line "+strconv.Quote(line)+" can change action keys; only startup sizing, repository fetching, build:remote-exec transport and the remote-mode selection belong there"))
			}
		}
	}
	return errs
}

// TestSetupBazelRCCarriesOnlyTransport runs setup-bazel's write-bazelrc.sh in
// every mode bazel.yml uses (remote, fork-ro/fork-rw, cache, local): its rc
// must be transport-only, so bazel.yml's lanes hash like pre-push and
// bazel-test.yml.
func TestSetupBazelRCCarriesOnlyTransport(t *testing.T) {
	script := readFile(t, repoRoot(t), setupBazelDir+"/write-bazelrc.sh")
	pem := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	for name, env := range map[string]map[string]string{
		"local":  {},
		"cache":  {"BAZEL_FORK_CACHE": "true"},
		"remote": {"BAZEL_REMOTE_EXECUTOR": "grpcs://executor.invalid:443", "RBE_TLS_CERT": pem, "RBE_TLS_KEY": pem, "RBE_TLS_CA": pem, "RBE_INSTANCE": "oss"},
		"fork": {
			"RBE_FORK_ENDPOINT": rbeForkEndpoint, "RBE_FORK_INSTANCE": "oss-fork",
			"RBE_FORK_CERT_FILE": "/runner/fork.crt", "RBE_FORK_KEY_FILE": "/runner/fork.key",
		},
	} {
		dir := t.TempDir()
		output := filepath.Join(dir, "output")
		env["GITHUB_WORKSPACE"] = dir
		env["GITHUB_OUTPUT"] = output
		env["BAZEL_CI_CACHE_DIR"] = t.TempDir()
		env["BAZEL_CI_SECRET_DIR"] = t.TempDir()
		if out, err := runWorkflowStepScript(t, dir, script, env); err != nil {
			t.Errorf("%s: write-bazelrc.sh: %v\n%s", name, err, out)
			continue
		}
		rcPath, ok := readStepOutput(t, output, "rc")
		if !ok {
			t.Errorf("%s: write-bazelrc.sh wrote no rc output", name)
			continue
		}
		rc := readFile(t, filepath.Dir(rcPath), filepath.Base(rcPath))
		lines := strings.Split(strings.TrimSuffix(rc, "\n"), "\n")
		if len(lines) < 2 {
			t.Errorf("%s: rc has %d lines:\n%s", name, len(lines), rc)
		}
		for _, err := range checkSetupBazelRCLines(lines) {
			t.Errorf("%s: %v", name, err)
		}
	}

	for name, lines := range map[string][]string{
		"test PATH":     {"test --test_env=PATH=" + bazelPinnedTestPath},
		"common define": {"common --define=gotags=x"},
		"forwarded env": {"common --repo_env=HOME"},
		"action env":    {"common --action_env=GOPROXY=https://proxy.golang.org"},
		"startup other": {"startup --output_user_root=/x"},
		"platform":      {"build:remote-exec --extra_execution_platforms=//:rbe"},
		"ci in rc":      {"test:ci --test_env=PATH=/opt/go/bin"},
		"suite config":  {"build --config=acceptance"},
	} {
		if len(checkSetupBazelRCLines(lines)) == 0 {
			t.Errorf("%s: expected an error for setup-bazel rc lines %q", name, lines)
		}
	}
}

// bazelSuiteConfigs select a tagged suite, keyed apart on purpose (--define,
// --test_timeout); pre-push never runs them, and bazel-test.yml passes
// acceptance's flags (bazel_multilane_test.go).
var bazelSuiteConfigs = map[string]bool{"acceptance": true, "integration": true}

// TestBazelMultiLaneLanesHashLikePrePush: bazel.yml's unit lane runs what
// pre-push runs (`bazel test //...`) with only non-key configs and flags, so
// pre-push, PR and main share results; the suite lanes add only their suite
// config. The lane step's own flags (profile, BEP file) are non-key too.
func TestBazelMultiLaneLanesHashLikePrePush(t *testing.T) {
	prePush := readFile(t, repoRoot(t), ".githooks/lib/push-suite.sh")
	if !strings.Contains(prePush, `exec bazel test //... "--config=$config" --keep_going`) {
		t.Fatalf(".githooks/lib/push-suite.sh no longer runs bazel test //... with one remote-mode config; update this test")
	}
	for lane, cmd := range multiLaneCommands {
		fields := strings.Fields(cmd)
		suites := 0
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "--") {
				continue
			}
			if c, ok := strings.CutPrefix(f, "--config="); ok {
				switch {
				case slices.Contains(bazelNonKeyConfigs, c):
				case bazelSuiteConfigs[c]:
					suites++
				default:
					t.Errorf("lane %s passes --config=%s, neither a non-key config %v nor a suite config", lane, c, bazelNonKeyConfigs)
				}
				continue
			}
			if !bazelTransportFlag(f) {
				t.Errorf("lane %s passes %s, which can change action keys; commit it unconditionally in .bazelrc", lane, f)
			}
		}
		if lane == "unit" && (suites != 0 || fields[len(fields)-1] != "//...") {
			t.Errorf("unit lane %q must run //... with no suite config, as pre-push does", cmd)
		}
	}

	wf := readMultiLaneWorkflow(t)
	run := ""
	for _, s := range wf.Jobs["lane"].Steps {
		if s.ID == "test" {
			run = s.Run
		}
	}
	if run == "" {
		t.Fatalf("%s: the lane job has no step with id test", bazelMultiLaneWorkflow)
	}
	flags := regexp.MustCompile(`(?m)^\s+(--[a-z_]+)=`).FindAllStringSubmatch(run, -1)
	if len(flags) == 0 {
		t.Fatalf("%s: the lane step passes no flags after the command; update this test", bazelMultiLaneWorkflow)
	}
	for _, m := range flags {
		if !bazelTransportFlag(m[1]) {
			t.Errorf("%s lane step passes %s, which can change action keys", bazelMultiLaneWorkflow, m[1])
		}
	}
}
