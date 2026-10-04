package scripts_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Test results computed at any phase (pre-push, PR, main) are reused by the
// others only if every phase hashes its actions identically. So every flag
// that can change an action key lives unconditionally in the committed
// .bazelrc, and the per-mode configs (remote-exec, fork-cache) and the lines
// CI and developers write to the gitignored .bazelrc.local carry transport
// only: endpoints, credentials, timeouts, download and parallelism policy.
//
// Classification fails closed: a flag not known to be transport-only counts
// as key-affecting.

// bazelPinnedTestPath is the PATH every test action sees: the remote
// workers' Go at /usr/local/go and the system directories. Forwarding the
// client's PATH (a bare --test_env=PATH) gives every machine its own keys.
const bazelPinnedTestPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

// bazelRemoteModeConfigs select how actions run and where results come from;
// none may change what an action is.
var bazelRemoteModeConfigs = []string{"remote-exec", "fork-cache"}

// bazelClientEnvAllowed may be forwarded from the client environment: it is a
// debug override (internal/bazeltest) that no phase sets, so an unset value
// keeps it out of every action.
var bazelClientEnvAllowed = map[string]bool{"GC_TEST_REPO_ROOT": true}

// bazelTransportFlag reports whether flag (--name, --noname or --name=value)
// cannot change an action key.
func bazelTransportFlag(flag string) bool {
	name, _, _ := strings.Cut(flag, "=")
	switch name {
	case "--remote_default_exec_properties", "--remote_default_platform_properties":
		return false // platform properties are part of the action
	case "--jobs", "--experimental_circuit_breaker_strategy", "--disk_cache", "--keep_going", "--nokeep_going":
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

// checkBazelKeyParity checks the committed .bazelrc: the remote-mode configs
// are transport-only, the unconditional test PATH is the pinned one, and no
// unconditional line forwards client environment into actions.
func checkBazelKeyParity(bazelrc string) []error {
	var errs []error
	modes := map[string]int{}
	for _, m := range bazelRemoteModeConfigs {
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
	for _, m := range bazelRemoteModeConfigs {
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
// build:remote-exec transport flags and the fork-cache selection.
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
			if o.command != "build" || o.config != "remote-exec" || !bazelTransportFlag(o.flag) {
				errs = append(errs, errors.New(".bazelrc.local line "+strconv.Quote(line)+" is not a build:remote-exec transport flag; everything else is shared and belongs in .bazelrc"))
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
	} {
		if len(checkBazelKeyParity(rc)) == 0 {
			t.Errorf("%s: expected an error for .bazelrc fixture:\n%s", name, rc)
		}
	}

	for name, lines := range map[string][]string{
		"executor":  {"build:remote-exec --remote_executor=" + ep, "build:remote-exec --remote_instance_name=oss"},
		"mtls":      {"build:remote-exec --tls_client_certificate=/x.crt", "build:remote-exec --tls_client_key=/x.key", "build:remote-exec --tls_certificate_authority=/x.pem"},
		"fork":      {bazelForkCacheLine},
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
// every mode: whatever it writes must be transport-only, so trusted,
// rbe-fork and fork-cache runs (and developers) hash actions alike.
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
		for _, err := range checkBazelRCLocalLines(runBazelRCConfigStep(t, script, env)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
