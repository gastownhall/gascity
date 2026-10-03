package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rbe-west's fork pool (infra nativelink-cas/west README "rbe-fork"): Blacksmith
// workers for the fork scheduler, instance oss-fork, which executes untrusted
// fork and Dependabot PR actions. rbe-fork-pool.yml runs blacksmith-worker.sh
// with WORKER_TIER=fork: isolation always on, every action in its own network
// namespace with loopback only (NETNS=1), no action cache, and the fork CA's
// worker certificate (CN=rbe-fork-worker), which reaches only the fork
// scheduler and the CAS. These tests pin that, and the id-based allowlist the
// fork mint reads for the rw tier.

const (
	rbeForkPoolWorkflow   = ".github/workflows/rbe-fork-pool.yml"
	rbeForkAllowlist      = ".github/rbe-fork-allowlist.txt"
	rbeForkCredential     = "tools/rbe/fork-credential.sh"
	blacksmithAllowlist   = ".github/blacksmith-allowlist.txt"
	rbeForkPoolMaxMinutes = 120
)

// The tier gate sits right after RBE_ACTION_ISOLATION is validated and before
// the canary picks a mode, so a fork worker refuses 0 and canary alike, and
// before anything is installed. NETNS is set here and nowhere else.
func TestRBEWorkerScriptForkTier(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	const gate = "WORKER_TIER=${WORKER_TIER:-oss}\n" +
		"case \"$WORKER_TIER\" in\n" +
		"oss) NETNS=0 ;;\n" +
		"fork)\n" +
		"\t# Untrusted actions never run unisolated, and never with a network.\n" +
		"\t[ \"$ACTION_ISOLATION\" = 1 ] || { echo \"WORKER_TIER=fork needs RBE_ACTION_ISOLATION=1\" >&2; exit 2; }\n" +
		"\tNETNS=1\n" +
		"\t;;\n" +
		"*) echo \"WORKER_TIER must be oss or fork\" >&2; exit 2 ;;\n" +
		"esac\n"
	at := 0
	for _, want := range []string{
		"ACTION_ISOLATION=${RBE_ACTION_ISOLATION:-1}\n",
		`*) echo "RBE_ACTION_ISOLATION must be 0, 1 or canary" >&2; exit 2 ;;`,
		gate,
		"RBE_WEST_PORT=${RBE_WEST_PORT:-443}\n",
		"sudo DEBIAN_FRONTEND=noninteractive",
		`if [ "$ACTION_ISOLATION" = canary ]; then`,
		"\t\tNETNS=${NETNS:-0}\n",
	} {
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
	if n := len(regexp.MustCompile(`(?m)^\s*(?:oss\) )?NETNS=`).FindAllString(script, -1)); n != 3 {
		t.Errorf("%s assigns NETNS %d times, want 3 (oss, fork, rbe-action.env)", rbeWorkerScript, n)
	}
	// render runs without the preamble too (the canary harness): it defaults
	// the tier and port itself, to the OSS ones.
	for _, want := range []string{
		`--arg host "grpcs://${RBE_WEST_HOST}:${RBE_WEST_PORT:-443}"`,
		`--arg tier "${WORKER_TIER:-oss}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s render missing %q", rbeWorkerScript, want)
		}
	}
}

type rbeForkPoolWorkflowFile struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		If             string `yaml:"if"`
		RunsOn         string `yaml:"runs-on"`
		TimeoutMinutes int    `yaml:"timeout-minutes"`
		Steps          []struct {
			Uses string            `yaml:"uses"`
			With map[string]any    `yaml:"with"`
			Env  map[string]string `yaml:"env"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestRBEForkPoolWorkflow(t *testing.T) {
	text := readFile(t, repoRoot(t), rbeForkPoolWorkflow)
	var wf rbeForkPoolWorkflowFile
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		t.Fatalf("parse %s: %v", rbeForkPoolWorkflow, err)
	}

	// Started only by rbe-west's fork pool scaler (or an operator): a dispatch
	// with the scaler's two inputs, never a PR, push or schedule.
	if len(wf.On) != 1 || wf.On["workflow_dispatch"] == nil {
		t.Errorf("on: %v, want workflow_dispatch only", rbeSortedKeys(wf.On))
	}
	dispatch, _ := wf.On["workflow_dispatch"].(map[string]any)
	inputs, _ := dispatch["inputs"].(map[string]any)
	for name, def := range map[string]string{"idle_minutes": "10", "max_minutes": strconv.Itoa(rbeForkPoolMaxMinutes)} {
		in, _ := inputs[name].(map[string]any)
		if in == nil || in["default"] != def || in["type"] != "string" {
			t.Errorf("workflow_dispatch input %s = %v, want a string defaulting to %q", name, in, def)
		}
	}
	if len(inputs) != 2 {
		t.Errorf("workflow_dispatch inputs %v, want idle_minutes and max_minutes (what the scaler sends)", rbeSortedKeys(inputs))
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Errorf("permissions %v, want contents: read only", wf.Permissions)
	}

	job, ok := wf.Jobs["worker"]
	if !ok || len(wf.Jobs) != 1 {
		t.Fatalf("%s: jobs %v, want worker alone", rbeForkPoolWorkflow, rbeSortedKeys(wf.Jobs))
	}
	// Blacksmith donates this compute for our OSS repos' workflows; the
	// default branch only, as rbe-worker-pool.yml.
	if want := "github.ref == format('refs/heads/{0}', github.event.repository.default_branch)"; job.If != want {
		t.Errorf("worker if: %q, want %q", job.If, want)
	}
	if job.RunsOn != "blacksmith-32vcpu-ubuntu-2404" {
		t.Errorf("worker runs-on %q, want blacksmith-32vcpu-ubuntu-2404 (the OSS pool's image)", job.RunsOn)
	}
	// max_minutes, then up to 5 minutes of drain (blacksmith-worker.sh), then
	// setup: the job outlives its worker; a compromised VM lives no longer.
	if job.TimeoutMinutes < rbeForkPoolMaxMinutes+10 || job.TimeoutMinutes > rbeForkPoolMaxMinutes+30 {
		t.Errorf("worker timeout-minutes %d, want %d-%d", job.TimeoutMinutes, rbeForkPoolMaxMinutes+10, rbeForkPoolMaxMinutes+30)
	}

	var checkout, worker bool
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout = true
			if v, _ := step.With["persist-credentials"].(bool); v || step.With["persist-credentials"] == nil {
				t.Errorf("checkout must set persist-credentials: false, got %v", step.With["persist-credentials"])
			}
		}
		if strings.TrimSpace(step.Run) != rbeWorkerScript {
			continue
		}
		worker = true
		// Exactly these: isolation is not the repository variable (no
		// rollback to 0, no canary), the fork CA's worker certificate and the
		// fork endpoint, nothing more (no CACHE_DIR: no sticky disk shared
		// across fork workers).
		want := map[string]string{
			"RBE_WORKER_TLS_CERT":  "${{ secrets.RBE_FORK_WORKER_TLS_CERT }}",
			"RBE_WORKER_TLS_KEY":   "${{ secrets.RBE_FORK_WORKER_TLS_KEY }}",
			"RBE_WEST_HOST":        "rbe-fork.ops.gascity.com",
			"RBE_WEST_PORT":        "8444",
			"WORKER_TIER":          "fork",
			"WORKER_MODE":          "pool",
			"POOL_IDLE_MINUTES":    "${{ inputs.idle_minutes }}",
			"POOL_MAX_MINUTES":     "${{ inputs.max_minutes }}",
			"WORKER_NAME":          "gha-fork-${{ github.event.repository.name }}-${{ github.run_id }}-${{ github.run_attempt }}",
			"RBE_ACTION_ISOLATION": "1",
		}
		for k, v := range want {
			if step.Env[k] != v {
				t.Errorf("worker step %s = %q, want %q", k, step.Env[k], v)
			}
		}
		for k, v := range step.Env {
			if _, ok := want[k]; !ok {
				t.Errorf("worker step sets %s=%q; the fork worker takes nothing else", k, v)
			}
		}
	}
	if !checkout || !worker {
		t.Fatalf("%s: checkout step found %v, worker step found %v", rbeForkPoolWorkflow, checkout, worker)
	}

	// No secret but the fork worker certificate (never the OSS worker's, which
	// writes AC_OSS), and no repository variable that could turn isolation
	// down.
	secrets := map[string]bool{}
	for _, m := range regexp.MustCompile(`secrets\.([A-Za-z0-9_]+)`).FindAllStringSubmatch(text, -1) {
		secrets[m[1]] = true
	}
	if got := rbeSortedKeys(secrets); strings.Join(got, ",") != "RBE_FORK_WORKER_TLS_CERT,RBE_FORK_WORKER_TLS_KEY" {
		t.Errorf("%s uses secrets %v, want RBE_FORK_WORKER_TLS_CERT and RBE_FORK_WORKER_TLS_KEY only", rbeForkPoolWorkflow, got)
	}
	if strings.Contains(text, "vars.") {
		t.Errorf("%s must not read repository variables", rbeForkPoolWorkflow)
	}
}

func rbeSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The mint gives a fork PR the rw tier (trusted OSS pool, results cached in
// AC_OSS) only if its author and the run's triggering actor are listed here.
// Logins can be re-registered after a rename; numeric ids never change hands
// (decision D9). One id per line with its login as a comment, the same people
// as the Blacksmith allowlist.
func TestRBEForkAllowlistIsIDBased(t *testing.T) {
	root := repoRoot(t)
	line := regexp.MustCompile(`^([1-9][0-9]{0,11}) # ([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))$`)
	ids := map[string]string{}
	var logins []string
	for i, l := range strings.Split(readFile(t, root, rbeForkAllowlist), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := line.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("%s:%d: %q is not \"<numeric id> # <login>\"", rbeForkAllowlist, i+1, l)
			continue
		}
		if prev, dup := ids[m[1]]; dup {
			t.Errorf("%s:%d: id %s listed twice (%s, %s)", rbeForkAllowlist, i+1, m[1], prev, m[2])
		}
		ids[m[1]] = m[2]
		logins = append(logins, strings.ToLower(m[2]))
	}
	var blacksmith []string
	for _, l := range strings.Split(readFile(t, root, blacksmithAllowlist), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			blacksmith = append(blacksmith, strings.ToLower(l))
		}
	}
	sort.Strings(logins)
	sort.Strings(blacksmith)
	if len(logins) == 0 || strings.Join(logins, ",") != strings.Join(blacksmith, ",") {
		t.Errorf("%s lists %v, %s lists %v: keep the same people (look up ids with gh api users/<login> --jq .id)",
			rbeForkAllowlist, logins, blacksmithAllowlist, blacksmith)
	}
}

// A fork VM runs untrusted actions: user namespaces (the usual first step of a
// kernel escape from an unprivileged process) are off on it before any action
// runs, set inside isolate() after the render and before the selftest, so the
// selftest and the probe run with the limit in place, and the probe action
// checks it cannot make one. Nothing on the worker needs them: the launcher's
// unshare runs as root with pid/mount/ipc/net namespaces only.
func TestRBEWorkerScriptForkNoUserNamespaces(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerScript)
	at := 0
	for _, want := range []string{
		"isolate() {\n",
		"\tphase render\n\trender\n",
		"\tif [ \"$WORKER_TIER\" = fork ]; then\n\t\tphase userns\n" +
			"\t\tsudo sysctl -q -w user.max_user_namespaces=0\n" +
			"\t\t[ \"$(cat /proc/sys/user/max_user_namespaces)\" = 0 ] || fail \"user.max_user_namespaces is not 0\"\n\tfi\n",
		"\tphase selftest\n",
		"\t\t[ \"$2\" = fork ] && unshare --user true >/dev/null 2>&1 && echo \"LEAK userns\"\n",
		"' probe \"$ROOT/pki/worker.key\" \"$WORKER_TIER\" 2>&1) || true\n",
	} {
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
	for _, f := range []string{"tools/rbe/rbe-action-launch", "tools/rbe/rbe-action-entry.c", "tools/rbe/rbe-action-selftest", "tools/rbe/rbe-action-sweep"} {
		if regexp.MustCompile(`unshare[^\n]*(--user|\s-U\b)|CLONE_NEWUSER`).MatchString(readFile(t, root, f)) {
			t.Errorf("%s creates a user namespace; fork VMs run with user.max_user_namespaces=0", f)
		}
	}
}

// fork-credential.sh is a byte copy of beads' setup-bazel one (keep in sync,
// like tools/rbe/rbe-action-*). Whatever the mint answers, a build only ever
// goes to the fork endpoint, with ro on oss-fork or rw on oss, and its key is
// PKCS#8 (Bazel's TLS refuses SEC1).
func TestRBEForkCredentialPins(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeForkCredential)
	for _, want := range []string{
		"set -euo pipefail\n",
		`ENDPOINT_RE=${RBE_FORK_ENDPOINT_RE:-'^grpcs://rbe-fork\.ops\.gascity\.com:8444$'}`,
		`[[ $endpoint =~ $ENDPOINT_RE ]] || { echo "::error::rbe-fork mint returned endpoint '$endpoint'" >&2; exit 1; }`,
		`case "$tier/$instance" in ro/oss-fork | rw/oss) ;; *) echo "::error::rbe-fork mint returned tier '$tier' instance '$instance'" >&2; exit 1 ;; esac`,
		`if [ "$tier" != "$RBE_FORK_TIER" ]; then`,
		`openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$dir/fork.key"`,
		`[ "${BAZEL_FORK_REMOTE:-}" = true ] || exit 0`,
		`--connect-timeout 5 --max-time 60`,
		`case "$code" in 429 | 502 | 000) ;; *) break ;; esac`,
		`[ "$attempt" -eq 4 ] || sleep $((attempt * 10))`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s missing %q", rbeForkCredential, want)
		}
	}
}

// rbeForkMintCertStub stands in for curl in fork-credential.sh cert: it logs
// each request's URL and timeouts to $RBE_TEST_MINT_LOG and answers as
// RBE_TEST_MINT says: ro (a certificate for tier ro), an HTTP status with an
// error body, 000 (connection refused), or "<first>-then-<rest>" (the first
// request answers <first>). The certificate is self-signed: the script only
// prints its subject; beads' scripts/bazel_fork_mode_test.go covers the rest.
const rbeForkMintCertStub = `#!/usr/bin/env bash
set -euo pipefail
out= url= connect= max=
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	--connect-timeout) connect=$2; shift 2 ;;
	--max-time) max=$2; shift 2 ;;
	-w | -H | --data) shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
echo "curl $url connect=$connect max=$max" >>"$RBE_TEST_MINT_LOG"
n=$(grep -c '^curl ' "$RBE_TEST_MINT_LOG")
answer=$RBE_TEST_MINT
case "$answer" in
*-then-*) if [ "$n" -eq 1 ]; then answer=${answer%%-then-*}; else answer=${answer#*-then-}; fi ;;
esac
case "$answer" in
ro)
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout /dev/null -days 1 \
		-subj /O=gascity/OU=rbe-fork/CN=ro.gascity.6969.4242.1.bazel -out mint-cert.pem 2>/dev/null
	jq -cn --rawfile pem mint-cert.pem \
		'{tier: "ro", instance: "oss-fork", endpoint: "grpcs://rbe-fork.ops.gascity.com:8444", cert_pem: $pem}' >"$out"
	printf 200
	;;
000) echo "curl: (7) Failed to connect to rbe-mint.ops.gascity.com port 8444" >&2; exit 7 ;;
*) printf '{"error": "stub answer %s"}' "$answer" >"$out"; printf '%s' "$answer" ;;
esac
`

// TestRBEForkCredentialRetries runs fork-credential.sh cert against a
// stubbed mint: it retries only what may pass on its own (429, 502 and
// connection failures), backs off 10, 20 and 30 s between its four
// attempts and never after the last, and gives up connecting after 5 s.
func TestRBEForkCredentialRetries(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	bin := t.TempDir()
	for name, body := range map[string]string{
		"curl":  rbeForkMintCertStub,
		"sleep": "#!/bin/sh\necho \"sleep $*\" >>\"$RBE_TEST_MINT_LOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The step's $GITHUB_OUTPUT is the helper's .bazelrc.local; result and
	// error are this wrapper's.
	step := `if bash "$RBE_TEST_SCRIPT" cert >out.txt 2>&1; then r=ok; else r=failed; fi
echo "result=$r" >>.bazelrc.local
echo "error=$(grep -o 'mint refused (HTTP [0-9]*)' out.txt || true)" >>.bazelrc.local
`
	for _, c := range []struct {
		answer, result, err string
		calls               int
		sleeps              string
	}{
		{"ro", "ok", "", 1, ""},
		{"429-then-ro", "ok", "", 2, "10"},
		{"502-then-ro", "ok", "", 2, "10"},
		{"000-then-ro", "ok", "", 2, "10"},
		{"429", "failed", "mint refused (HTTP 429)", 4, "10 20 30"},
		{"502", "failed", "mint refused (HTTP 502)", 4, "10 20 30"},
		{"000", "failed", "mint refused (HTTP 000)", 4, "10 20 30"},
		{"403", "failed", "mint refused (HTTP 403)", 1, ""},
		{"503", "failed", "mint refused (HTTP 503)", 1, ""},
	} {
		secret := filepath.Join(t.TempDir(), "secret")
		if err := os.MkdirAll(secret, 0o700); err != nil {
			t.Fatal(err)
		}
		mintLog := filepath.Join(t.TempDir(), "mint.log")
		got := runBazelRCConfigStep(t, step, map[string]string{
			"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GITHUB_OUTPUT":       ".bazelrc.local",
			"RBE_TEST_SCRIPT":     filepath.Join(repoRoot(t), rbeForkCredential),
			"RBE_TEST_MINT":       c.answer,
			"RBE_TEST_MINT_LOG":   mintLog,
			"BAZEL_CI_SECRET_DIR": secret,
			"ARTIFACT_ID":         "99",
			"RBE_FORK_PR":         "6969",
			"RBE_FORK_TIER":       "ro",
			"GITHUB_REPOSITORY":   "gastownhall/gascity",
			"GITHUB_RUN_ID":       "4242",
			"GITHUB_RUN_ATTEMPT":  "1",
			"GITHUB_JOB":          "bazel",
		})
		want := []string{"result=" + c.result, "error=" + c.err}
		if c.result == "ok" {
			want = append([]string{
				"cert=" + secret + "/fork.crt", "key=" + secret + "/fork.key",
				"endpoint=grpcs://rbe-fork.ops.gascity.com:8444", "instance=oss-fork", "tier=ro",
			}, want...)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("mint %s: outputs %q, want %q", c.answer, got, want)
		}
		b, _ := os.ReadFile(mintLog)
		var calls int
		var sleeps []string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if n, ok := strings.CutPrefix(line, "sleep "); ok {
				sleeps = append(sleeps, n)
			} else if line != "curl https://rbe-mint.ops.gascity.com:8444/v1/cert connect=5 max=60" {
				t.Errorf("mint %s: request %q, want POST /v1/cert with --connect-timeout 5 --max-time 60", c.answer, line)
			} else {
				calls++
			}
		}
		if calls != c.calls || strings.Join(sleeps, " ") != c.sleeps {
			t.Errorf("mint %s: %d requests, slept %q; want %d, %q\n%s", c.answer, calls, sleeps, c.calls, c.sleeps, b)
		}
	}
}
