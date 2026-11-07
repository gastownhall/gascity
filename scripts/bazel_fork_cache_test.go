package scripts_test

import (
	"bytes"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Fork PRs (no secrets) whose rbe-fork certificate the mint does not grant
// read rbe-west's anonymous read-only cache through .bazelrc's fork-cache
// config, which bazel.yml's mode cache selects through setup-bazel's rc.
// Fork actions hit only if they hash like the trusted run's, so neither rc
// carries a key-affecting flag (those are committed in .bazelrc:
// bazel_key_parity_test.go), and fork-cache itself must upload nothing,
// carry no credentials, degrade to local execution when the endpoint is
// closed or slow, and never be overridden by remote-exec's 3600s timeout.

const (
	bazelForkCacheLine = "build --config=fork-cache"
	// zstd cache transfers: only the anonymous fork cache (rbe-cache :8443)
	// advertises a compressor, so only fork-cache asks for one, in .bazelrc
	// itself (contributors, pre-push and fork CI alike). Trusted remote-exec
	// and rbe-fork never: their schedulers advertise none, and Bazel then
	// refuses the remote. cache-zstd-probe.sh is the operator's check that
	// rbe-cache still advertises it (TestCacheProbes); nothing in CI runs it.
	bazelCacheZstdLine     = "build:fork-cache --remote_cache_compression"
	bazelCacheZstdProbe    = "tools/rbe/cache-zstd-probe.sh"
	forkCacheMaxTimeoutSec = 15
	// The farm admits 16 connections per source IP and Blacksmith runners
	// share egress IPs.
	forkCacheMaxConnections = 4
)

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

// bazelCacheZstdProbeStub stands in for cache-zstd-probe.sh: it counts its
// runs, says why on stderr as the real probe does, and passes only for
// BAZEL_TEST_PROBE=zstd (rbe-cache advertising zstd). TestCacheProbes runs
// the real probe.
const bazelCacheZstdProbeStub = `#!/usr/bin/env bash
echo probe >>probe.log
if [ "${BAZEL_TEST_PROBE:-}" = zstd ]; then
	echo "rbe-cache zstd probe: stub advertises zstd; fork-cache can use it" >&2
	exit 0
fi
echo "rbe-cache zstd probe: stub not advertised; fork-cache builds run locally" >&2
exit 1
`

// bazelCacheRRCProbeStub stands in for cache-rrc-probe.sh: it counts its
// runs in rrc-probe.log, says why on stderr as the real probe does, and
// passes only for BAZEL_TEST_RRC_PROBE=ok (rbe-cache answering, the kill
// switch on). TestCacheProbes runs the real probe.
const bazelCacheRRCProbeStub = `#!/usr/bin/env bash
echo probe >>rrc-probe.log
if [ "${BAZEL_TEST_RRC_PROBE:-}" = ok ]; then
	echo "rbe-cache rrc probe: stub answers" >&2
	exit 0
fi
echo "rbe-cache rrc probe: stub refused" >&2
exit 1
`

// bazelRCLocalStepRun is what runBazelRCLocalStepProbes saw: the
// .bazelrc.local lines a step wrote (none if it wrote no file), how often
// it ran each probe stub, its combined output and its step summary.
type bazelRCLocalStepRun struct {
	lines                 []string
	zstdProbes, rrcProbes int
	out, summary          string
}

// runBazelRCLocalStepProbes runs a step's script as Actions does (bash -eo
// pipefail) in a scratch directory with env and stub zstd and rrc probes
// (no test reaches rbe-cache).
func runBazelRCLocalStepProbes(t *testing.T, script string, env map[string]string) bazelRCLocalStepRun {
	t.Helper()
	dir := t.TempDir()
	for path, body := range map[string]string{bazelCacheZstdProbe: bazelCacheZstdProbeStub, bazelCacheRRCProbe: bazelCacheRRCProbeStub} {
		probe := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(probe), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(probe, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withSummary := map[string]string{"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary.md")}
	for k, v := range env {
		withSummary[k] = v
	}
	out, err := runWorkflowStepScript(t, dir, script, withSummary)
	if err != nil {
		t.Fatalf("step script with %v: %v\n%s", env, err, out)
	}
	summary, _ := os.ReadFile(filepath.Join(dir, "summary.md"))
	run := bazelRCLocalStepRun{out: out, summary: string(summary)}
	probes, _ := os.ReadFile(filepath.Join(dir, "probe.log"))
	run.zstdProbes = strings.Count(string(probes), "probe\n")
	probes, _ = os.ReadFile(filepath.Join(dir, "rrc-probe.log"))
	run.rrcProbes = strings.Count(string(probes), "probe\n")
	rc, err := os.ReadFile(filepath.Join(dir, ".bazelrc.local"))
	if os.IsNotExist(err) {
		return run
	}
	if err != nil {
		t.Fatal(err)
	}
	run.lines = strings.Split(strings.TrimSuffix(string(rc), "\n"), "\n")
	return run
}

// runBazelRCLocalStep is runBazelRCLocalStepProbes for the .bazelrc.local
// lines alone.
func runBazelRCLocalStep(t *testing.T, script string, env map[string]string) []string {
	t.Helper()
	return runBazelRCLocalStepProbes(t, script, env).lines
}

// bazelRCLocalLaneSteps are the only steps anywhere in bazel.yml that may
// write .bazelrc.local, all in the lane job: the remote repo contents cache
// readers, trusted and fork-cache (bazel_rrc_test.go).
var bazelRCLocalLaneSteps = []string{bazelRRCReadStep, bazelForkRRCReadStep}

// bazelRCLocalLaneStep returns bazel.yml's lane step name (one of
// bazelRCLocalLaneSteps), after checking that no step outside
// bazelRCLocalLaneSteps touches .bazelrc.local.
func bazelRCLocalLaneStep(t *testing.T, name string) multiLaneStep {
	t.Helper()
	wf := readMultiLaneWorkflow(t)
	var found *multiLaneStep
	for id, job := range wf.Jobs {
		for i, s := range job.Steps {
			if id == "lane" && slices.Contains(bazelRCLocalLaneSteps, s.Name) {
				if s.Name == name {
					if found != nil {
						t.Fatalf("%s: two %q steps", bazelMultiLaneWorkflow, name)
					}
					found = &job.Steps[i]
				}
				continue
			}
			if strings.Contains(s.Run, ".bazelrc.local") {
				t.Errorf("%s job %s step %q touches .bazelrc.local; only the lane's %q may", bazelMultiLaneWorkflow, id, s.Name, bazelRCLocalLaneSteps)
			}
		}
	}
	if found == nil {
		t.Fatalf("%s: the lane job has no %q step", bazelMultiLaneWorkflow, name)
	}
	return *found
}

const (
	rbeForkEndpoint  = "grpcs://rbe-fork.ops.gascity.com:8444"
	rbeForkStatusURL = "https://rbe-mint.ops.gascity.com:8444/v1/status?repo="
)

// bazelTestCurlStub stands in for curl in the rbe job's decide step: it
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

// checkBazelForkCacheConfig checks .bazelrc's fork-cache config: a remote
// cache with no local-result uploads, both local fallbacks (without them a
// closed endpoint fails every action in GetCapabilities), the failure
// circuit breaker and a short --remote_timeout (a slow endpoint), few
// connections, zstd transfers (rbe-cache serves zstd; without the flag
// contributors read identity), no credentials, and an executor reset to
// none: a machine whose own rc (~/.bazelrc, /etc/bazel.bazelrc) names an
// executor would otherwise execute remotely against the read-only cache,
// whose CAS refuses the input upload (FindMissingBlobs PERMISSION_DENIED).
// No .bazelrc line may select
// it: setup-bazel's rc (bazel.yml mode cache) and the pre-push suite's
// command line do.
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
				errs = append(errs, errors.New(fields[0]+" expands --config=fork-cache; only setup-bazel's rc and the pre-push command line may"))
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
		name, value, _ := strings.Cut(flag, "=")
		if (name == "--remote_executor" && value != "") || strings.HasPrefix(name, "--tls_") || strings.HasSuffix(name, "_header") ||
			strings.HasPrefix(name, "--credential_helper") || strings.HasPrefix(name, "--google_") || strings.HasPrefix(name, "--bes_") {
			errs = append(errs, errors.New("fork-cache sets "+flag+"; the fork cache is anonymous and executes nothing remotely"))
		}
	}
	if !slices.Contains(opts, "--remote_executor=") || forkCacheLastValue(opts, "--remote_executor") != "" {
		errs = append(errs, errors.New("fork-cache must end with --remote_executor= (no executor, whatever the machine's own rc sets)"))
	}
	if forkCacheLastValue(opts, "--remote_cache") == "" {
		errs = append(errs, errors.New("fork-cache sets no --remote_cache"))
	}
	for name, want := range map[string]bool{
		"remote_upload_local_results":                         false,
		"remote_local_fallback":                               true,
		"incompatible_remote_local_fallback_for_remote_cache": true,
		// A BEP file with path conversion uploads the files it references;
		// each refusal counts against the circuit breaker checked below.
		"build_event_json_file_path_conversion":   false,
		"build_event_binary_file_path_conversion": false,
		// rbe-cache serves zstd; without it contributors read identity.
		"remote_cache_compression": true,
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
		"build:fork-cache --remote_executor=\n" +
		"build:fork-cache --noremote_upload_local_results\n" +
		"build:fork-cache --remote_local_fallback\n" +
		"build:fork-cache --incompatible_remote_local_fallback_for_remote_cache\n" +
		"build:fork-cache --remote_timeout=15 --remote_retries=2\n" +
		"build:fork-cache --experimental_circuit_breaker_strategy=failure\n" +
		"build:fork-cache --nobuild_event_json_file_path_conversion\n" +
		"build:fork-cache --nobuild_event_binary_file_path_conversion\n" +
		"build:fork-cache --remote_max_connections=4\n" +
		"build:fork-cache --remote_cache_compression\n" +
		"build:remote-exec --remote_timeout=3600\n" +
		"try-import %workspace%/.bazelrc.local\n"
	if errs := checkBazelForkCacheConfig(good); len(errs) != 0 {
		t.Errorf("good fixture: %v", errs)
	}
	drop := func(line string) string { return strings.Replace(good, line+"\n", "", 1) }
	for name, rc := range map[string]string{
		"missing":             "build:remote-exec --remote_timeout=3600\n",
		"no endpoint":         drop("build:fork-cache --remote_cache=" + ep),
		"no executor reset":   drop("build:fork-cache --remote_executor="),
		"no upload switch":    drop("build:fork-cache --noremote_upload_local_results"),
		"uploads again":       good + "build:fork-cache --remote_upload_local_results\n",
		"no local fallback":   drop("build:fork-cache --remote_local_fallback"),
		"no cache fallback":   drop("build:fork-cache --incompatible_remote_local_fallback_for_remote_cache"),
		"fallback off":        good + "build:fork-cache --noremote_local_fallback\n",
		"no breaker":          drop("build:fork-cache --experimental_circuit_breaker_strategy=failure"),
		"BEP json uploads":    drop("build:fork-cache --nobuild_event_json_file_path_conversion"),
		"BEP binary uploads":  drop("build:fork-cache --nobuild_event_binary_file_path_conversion"),
		"BEP json again":      good + "build:fork-cache --build_event_json_file_path_conversion\n",
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
		"no zstd":             drop("build:fork-cache --remote_cache_compression"),
		"zstd off":            good + "build:fork-cache --noremote_cache_compression\n",
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
// appears in one place, .bazelrc's fork-cache line. No other .bazelrc config
// and no workflow, action, git hook (the pre-push suite is
// .githooks/lib/push-suite.sh), rbe tool or make target may set it, since
// every other remote (rbe-west's trusted schedulers on :443, rbe-fork on
// :8444) advertises no compressor and Bazel then refuses the remote.
func TestBazelRemoteCacheCompressionOnlyForkCache(t *testing.T) {
	root := repoRoot(t)
	files := []string{".bazelrc", "Makefile"}
	for _, dir := range []string{".githooks", "tools/rbe"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, path)
			files = append(files, rel)
			return err
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml", ".github/actions/*/*"} {
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
	if len(found) != 1 || !strings.HasPrefix(found[0], ".bazelrc:") || !strings.HasSuffix(found[0], ": "+bazelCacheZstdLine) {
		t.Errorf("--remote_cache_compression set at %q; want only .bazelrc's %s", found, bazelCacheZstdLine)
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

// capsAnswer is how TestCacheProbes' stand-in rbe-cache answers
// GetCapabilities: HTTP status (0: 200), body, and grpc-status ("": none;
// with no body, in the headers, as gRPC's trailers-only errors are), after
// delay. ac and bs answer cache-rrc-probe.sh's GetActionResult and
// ByteStream Read the same way; nil is rbe-cache's answer for a key no
// action has, or a blob it does not hold: a trailers-only NOT_FOUND.
type capsAnswer struct {
	status int
	grpc   string
	body   []byte
	delay  time.Duration
	ac, bs *capsAnswer
}

// acNotFound: rbe-cache's answer to a GetActionResult for a key no action
// has, or a ByteStream Read for a blob it does not hold (capsAnswer.ac's and
// capsAnswer.bs's default).
var acNotFound = capsAnswer{grpc: "5"}

// acRequest: cache-rrc-probe.sh's GetActionResultRequest{instance_name:
// "oss", action_digest: {sha256("cache-rrc-probe: no action has this key"),
// 1}}, a key no action has.
var acRequest = grpcMessage(append(pbBytes(1, []byte("oss")),
	pbBytes(2, append(pbBytes(1, []byte("ad12d7a2f1dec188fc1f2c4f5535a4a65eb630fa2f309eca423695e6ec7abee7")), pbVarint(2, 1)...))...))

// bsRequest: cache-rrc-probe.sh's ReadRequest{resource_name:
// "oss/blobs/<acRequest's hash>/1"}, a blob the CAS does not hold.
var bsRequest = grpcMessage(pbBytes(1, []byte("oss/blobs/ad12d7a2f1dec188fc1f2c4f5535a4a65eb630fa2f309eca423695e6ec7abee7/1")))

// capsServer is TestCacheProbes' stand-in rbe-cache: it answers a and
// returns the probe's RBE_CACHE_PROBE_URL, a CA file for curl's
// CURL_CA_BUNDLE, and the request count.
type capsServer func(t *testing.T, a capsAnswer) (url, caFile string, hits *atomic.Int32)

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

// TestCacheProbes runs rbe-cache's two probes against stand-in rbe-cache
// servers, one serve for both: cache-zstd-probe.sh (cacheZstdProbeCases)
// and cache-rrc-probe.sh (cacheRRCProbeCases).
func TestCacheProbes(t *testing.T) {
	requireCacheZstdProbeTools(t)

	// serve answers like rbe-cache's Caddy, over TLS and HTTP/2, after
	// checking the request is a probe's GetCapabilities, or
	// cache-rrc-probe.sh's GetActionResult or ByteStream Read, for instance
	// oss.
	serve := func(t *testing.T, a capsAnswer) (url, caFile string, hits *atomic.Int32) {
		t.Helper()
		hits = new(atomic.Int32)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			body, _ := io.ReadAll(r.Body)
			path, want, answer := "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities", capsRequest, a
			switch r.URL.Path {
			case "/build.bazel.remote.execution.v2.ActionCache/GetActionResult":
				path, want, answer = r.URL.Path, acRequest, acNotFound
				if a.ac != nil {
					answer = *a.ac
				}
			case "/google.bytestream.ByteStream/Read":
				path, want, answer = r.URL.Path, bsRequest, acNotFound
				if a.bs != nil {
					answer = *a.bs
				}
			}
			if r.ProtoMajor != 2 || r.Method != http.MethodPost || r.URL.Path != path ||
				r.Header.Get("Content-Type") != "application/grpc" || !bytes.Equal(body, want) {
				t.Errorf("probe sent %s %s %s, content-type %q, body %x; want HTTP/2 POST %s, application/grpc, %x",
					r.Proto, r.Method, r.URL.Path, r.Header.Get("Content-Type"), body, path, want)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			select {
			case <-time.After(answer.delay):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/grpc")
			trailer := answer.grpc != "" && answer.body != nil
			if trailer {
				w.Header().Set("Trailer", "Grpc-Status")
			} else if answer.grpc != "" {
				w.Header().Set("Grpc-Status", answer.grpc)
			}
			status := answer.status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write(answer.body)
			// Streamed, as gRPC servers answer: Go drops the trailer of a
			// response it can give a content-length.
			w.(http.Flusher).Flush()
			if trailer {
				w.Header().Set("Grpc-Status", answer.grpc)
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
	t.Run("zstd", func(t *testing.T) { cacheZstdProbeCases(t, serve) })
	t.Run("rrc", func(t *testing.T) { cacheRRCProbeCases(t, serve) })
}

// cacheZstdProbeCases runs cache-zstd-probe.sh against serve. Only an answer
// listing ZSTD in cache_capabilities.supported_compressors (the field Bazel
// checks) passes; every other answer, error, refusal or timeout fails, and
// the rc step then writes no flag. The probe never writes stdout (the rc
// step's stdout is .bazelrc.local) and always says why on stderr.
func cacheZstdProbeCases(t *testing.T, serve capsServer) {
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
		verdict := "; fork-cache builds run locally\n"
		if want {
			verdict = "; fork-cache can use it\n"
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
