package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Bazel's remote repo contents cache on rbe-west (ga-vnycm2.25,
// engdocs/design/bazel-remote-repo-contents-cache.md). bazel.yml's rbe job
// turns vars.RBE_REPO_CONTENTS_CACHE into its rrc output (off, seed, canary,
// on); the lane job's reader step appends bazelRRCReadLines to
// .bazelrc.local; the rrc-seed job, on push to main only, runs every lane's
// command with --nobuild and uploads repository trees with a 30-minute
// rbe-rrc-writer certificate (.github/scripts/rrc-writer-credential.sh).

const (
	bazelRRCReadStep     = "Remote repo contents cache (read)"
	bazelRRCReadStepIf   = "needs.rbe.outputs.mode == 'remote' && (needs.rbe.outputs.rrc == 'on' || (needs.rbe.outputs.rrc == 'canary' && matrix.lane == 'unit'))"
	bazelRRCSeedJob      = "rrc-seed"
	bazelRRCSeedJobIf    = "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.rbe.outputs.mode == 'remote' && needs.rbe.outputs.rrc != 'off'"
	bazelRRCModeStepID   = "rrc"
	bazelRRCCredential   = ".github/scripts/rrc-writer-credential.sh"
	bazelRRCVar          = "vars.RBE_REPO_CONTENTS_CACHE"
	bazelRRCSeedStepName = "Seed the remote repo contents cache"
)

// bazelRRCReadLines: the reader's .bazelrc.local lines. Both are key
// neutral (checkBazelRCLocalLines).
var bazelRRCReadLines = []string{
	"startup --experimental_remote_repo_contents_cache",
	"common --loading_phase_threads=64",
}

func bazelRRCJobStep(t *testing.T, job multiLaneJob, pick func(multiLaneStep) bool, what string) multiLaneStep {
	t.Helper()
	var found []multiLaneStep
	for _, s := range job.Steps {
		if pick(s) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want one %s step, got %d", bazelMultiLaneWorkflow, what, len(found))
	}
	return found[0]
}

// TestBazelRRCModeStep runs the rbe job's rrc step for every variable value
// and execution mode: only mode remote enables anything, unset and off are
// off, and an unknown value is off with a warning (a typo never fails the
// required gate).
func TestBazelRRCModeStep(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	rbe := wf.Jobs["rbe"]
	if got := rbe.Outputs["rrc"]; got != "${{ steps.rrc.outputs.rrc }}" {
		t.Errorf("rbe job output rrc = %q", got)
	}
	step := bazelRRCJobStep(t, rbe, func(s multiLaneStep) bool { return s.ID == bazelRRCModeStepID }, "rrc")
	if want := map[string]string{"RRC": "${{ " + bazelRRCVar + " }}", "MODE": "${{ steps.decide.outputs.mode }}"}; !reflect.DeepEqual(step.Env, want) {
		t.Errorf("rrc step env %v, want %v", step.Env, want)
	}
	if step.If != "" {
		t.Errorf("rrc step if %q; it must always set the output", step.If)
	}
	for _, mode := range []string{"remote", "fork-ro", "fork-rw", "cache", "local"} {
		for value, want := range map[string]string{"": "off", "off": "off", "seed": "seed", "canary": "canary", "on": "on", "On": "off", "true": "off", "on;x": "off"} {
			if mode != "remote" {
				want = "off"
			}
			dir := t.TempDir()
			output := filepath.Join(dir, "output")
			out, err := runWorkflowStepScript(t, dir, step.Run, map[string]string{
				"RRC": value, "MODE": mode, "GITHUB_OUTPUT": output, "GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
			})
			if err != nil {
				t.Errorf("mode %s value %q: %v\n%s", mode, value, err, out)
				continue
			}
			got, _ := readStepOutput(t, output, "rrc")
			if got != want {
				t.Errorf("mode %s value %q: rrc=%q, want %q", mode, value, got, want)
			}
			unknown := !slices.Contains([]string{"", "off", "seed", "canary", "on"}, value)
			if warned := strings.Contains(out, "::warning"); warned != unknown {
				t.Errorf("mode %s value %q: warning %v, want %v:\n%s", mode, value, warned, unknown, out)
			}
		}
	}
}

// TestBazelRRCReadStep: the reader runs in mode remote only (fork runs and
// local modes never read: only rbe-west's trusted edge serves the entries),
// on the unit lane under canary and every lane under on, and writes exactly
// bazelRRCReadLines. It never enables uploads.
func TestBazelRRCReadStep(t *testing.T) {
	step := bazelRCLocalLaneStep(t, bazelRRCReadStep)
	if step.If != bazelRRCReadStepIf {
		t.Errorf("%q if %q, want %q", step.Name, step.If, bazelRRCReadStepIf)
	}
	for _, mode := range []string{"remote", "fork-ro", "fork-rw", "cache", "local"} {
		for _, rrc := range []string{"off", "seed", "canary", "on"} {
			for lane := range multiLaneCommands {
				want := mode == "remote" && (rrc == "on" || (rrc == "canary" && lane == "unit"))
				got := evalGHIf(t, step.If, map[string]string{
					"needs.rbe.outputs.mode": mode, "needs.rbe.outputs.rrc": rrc, "matrix.lane": lane,
				})
				if got != want {
					t.Errorf("mode %s rrc %s lane %s: reader runs %v, want %v", mode, rrc, lane, got, want)
				}
			}
		}
	}
	lines, _ := runBazelRCLocalStep(t, step.Run, nil)
	if !slices.Equal(lines, bazelRRCReadLines) {
		t.Errorf("%q writes %q, want %q", step.Name, lines, bazelRRCReadLines)
	}
	if strings.Contains(step.Run, "upload") {
		t.Errorf("%q mentions uploads; lanes only read", step.Name)
	}
	// Before the lane's bazel command, so the server starts with it.
	wf := readMultiLaneWorkflow(t)
	read, test := -1, -1
	for i, s := range wf.Jobs["lane"].Steps {
		switch {
		case s.Name == bazelRRCReadStep:
			read = i
		case s.ID == "test":
			test = i
		}
	}
	if read < 0 || test < 0 || read > test {
		t.Errorf("lane job: %q at %d, the bazel step at %d; the reader must come first", bazelRRCReadStep, read, test)
	}
}

// TestBazelRRCSeedJob: the only writer runs on push to main in mode remote
// with rrc not off, holds the OIDC permission no other bazel.yml job has,
// gates nothing, reads no secret (its credential is the OIDC-minted
// certificate) and sets Bazel up in local mode.
func TestBazelRRCSeedJob(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	job, ok := wf.Jobs[bazelRRCSeedJob]
	if !ok {
		t.Fatalf("%s has no %s job", bazelMultiLaneWorkflow, bazelRRCSeedJob)
	}
	if job.If != bazelRRCSeedJobIf {
		t.Errorf("%s if %q, want %q", bazelRRCSeedJob, job.If, bazelRRCSeedJobIf)
	}
	for _, c := range []struct {
		event, ref, mode, rrc string
		want                  bool
	}{
		{"push", "refs/heads/main", "remote", "seed", true},
		{"push", "refs/heads/main", "remote", "canary", true},
		{"push", "refs/heads/main", "remote", "on", true},
		{"push", "refs/heads/main", "remote", "off", false},
		{"push", "refs/heads/main", "cache", "on", false},
		{"push", "refs/heads/other", "remote", "on", false},
		{"pull_request", "refs/pull/1/merge", "remote", "on", false},
		{"merge_group", mergeQueueRef, "remote", "on", false},
		{"workflow_dispatch", "refs/heads/main", "remote", "on", false},
		{"schedule", "refs/heads/main", "remote", "on", false},
	} {
		got := evalGHIf(t, job.If, map[string]string{
			"github.event_name": c.event, "github.ref": c.ref, "needs.rbe.outputs.mode": c.mode, "needs.rbe.outputs.rrc": c.rrc,
		})
		if got != c.want {
			t.Errorf("%+v: seed runs %v, want %v", c, got, c.want)
		}
	}
	for id, other := range wf.Jobs {
		if id != bazelRRCSeedJob {
			if _, has := other.Permissions["id-token"]; has {
				t.Errorf("job %s asks for id-token; only %s may", id, bazelRRCSeedJob)
			}
		}
	}
	var gateNeeds []string
	switch n := wf.Jobs["gate"].Needs.(type) {
	case string:
		gateNeeds = []string{n}
	case []any:
		for _, v := range n {
			gateNeeds = append(gateNeeds, v.(string))
		}
	}
	if len(gateNeeds) == 0 || slices.Contains(gateNeeds, bazelRRCSeedJob) {
		t.Errorf("the gate needs %s; a failed seed must not fail the required check", bazelRRCSeedJob)
	}
	raw := readFile(t, repoRoot(t), bazelMultiLaneWorkflow)
	start := strings.Index(raw, "\n  "+bazelRRCSeedJob+":\n")
	if start < 0 {
		t.Fatalf("cannot find the %s job's text", bazelRRCSeedJob)
	}
	text := raw[start+1:]
	if next := regexp.MustCompile(`\n  [a-z0-9-]+:\n`).FindStringIndex(text); next != nil {
		text = text[:next[0]]
	}
	if strings.Contains(text, "secrets.") {
		t.Errorf("%s reads a secret; its only credential is the OIDC-minted writer certificate", bazelRRCSeedJob)
	}
	setups := 0
	for _, s := range job.Steps {
		if s.Uses == setupBazelUses {
			setups++
			if len(s.Env) != 0 {
				t.Errorf("%s setup-bazel env %v; local mode (no executor, no certificate) only", bazelRRCSeedJob, s.Env)
			}
		}
	}
	if setups != 1 {
		t.Errorf("%s: %d setup-bazel steps, want 1", bazelRRCSeedJob, setups)
	}
	cred := bazelRRCJobStep(t, job, func(s multiLaneStep) bool { return s.ID == "writer" }, "writer certificate")
	if cred.Run != "bash "+bazelRRCCredential {
		t.Errorf("writer step runs %q, want bash %s", cred.Run, bazelRRCCredential)
	}
	cleanup := bazelRRCJobStep(t, job, func(s multiLaneStep) bool { return strings.Contains(s.Run, "rrc-writer.key") }, "key removal")
	if !strings.HasPrefix(cleanup.If, "always()") {
		t.Errorf("the writer key removal runs if %q; it must always run", cleanup.If)
	}
}

// TestBazelRRCSeedRunsEveryLaneAnalysisOnly runs the seed step with a stub
// bazel over the rbe job's real lane list: one --nobuild invocation per
// lane command, with the startup flag before the command, uploads on, the
// writer certificate, and the cache endpoint alone (never an executor).
func TestBazelRRCSeedRunsEveryLaneAnalysisOnly(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	step := bazelRRCJobStep(t, wf.Jobs[bazelRRCSeedJob], func(s multiLaneStep) bool { return s.Name == bazelRRCSeedStepName }, "seed")
	want := map[string]string{
		"LANES":    "${{ needs.rbe.outputs.lanes }}",
		"CERT":     "${{ steps.writer.outputs.cert }}",
		"KEY":      "${{ steps.writer.outputs.key }}",
		"ENDPOINT": "${{ steps.writer.outputs.endpoint }}",
		"INSTANCE": "${{ steps.writer.outputs.instance }}",
	}
	if !reflect.DeepEqual(step.Env, want) {
		t.Errorf("seed step env %v, want %v", step.Env, want)
	}
	var lanes []map[string]any
	for name, cmd := range multiLaneCommands {
		lanes = append(lanes, map[string]any{"lane": name, "cmd": cmd})
	}
	lanesJSON, err := json.Marshal(lanes)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	// Bazel's `test --nobuild`: exit 1 after a successful analysis, with
	// "Unable to run tests"; BAZEL_TEST_FAIL makes analysis fail instead.
	stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >>" + calls + "\n" +
		"if [ -n \"${BAZEL_TEST_FAIL:-}\" ]; then echo 'ERROR: no such package'; echo 'ERROR: Build did NOT complete successfully'; exit 1; fi\n" +
		"echo 'INFO: Build completed successfully, 0 total actions'\necho \"ERROR: Couldn't start the build. Unable to run tests\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "bazel"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkflowStepScript(t, dir, step.Run, map[string]string{
		"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"LANES":               string(lanesJSON),
		"CERT":                "/secret/rrc-writer.crt",
		"KEY":                 "/secret/rrc-writer.key",
		"ENDPOINT":            "grpcs://rbe-west.ops.gascity.com:443",
		"INSTANCE":            "oss",
		"RUNNER_TEMP":         dir,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
	})
	if err != nil {
		t.Fatalf("seed step: %v\n%s", err, out)
	}
	got := strings.Split(strings.TrimSpace(readFile(t, dir, "calls")), "\n")
	var wantCalls []string
	for _, l := range lanes {
		wantCalls = append(wantCalls, "--experimental_remote_repo_contents_cache "+l["cmd"].(string)+" --nobuild --loading_phase_threads=64"+
			" --remote_cache=grpcs://rbe-west.ops.gascity.com:443 --remote_instance_name=oss"+
			" --tls_client_certificate=/secret/rrc-writer.crt --tls_client_key=/secret/rrc-writer.key --remote_upload_local_results")
	}
	if !slices.Equal(got, wantCalls) {
		t.Errorf("seed bazel calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}
	for _, c := range got {
		if strings.Contains(c, "remote_executor") {
			t.Errorf("seed call %q names an executor; the writer certificate is cache only", c)
		}
	}
	if _, err := runWorkflowStepScript(t, dir, step.Run, map[string]string{
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "LANES": "[]", "RUNNER_TEMP": dir,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
	}); err == nil {
		t.Errorf("seed step with no lanes succeeded; it must fail rather than seed nothing")
	}
	if out, err := runWorkflowStepScript(t, dir, step.Run, map[string]string{
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "LANES": string(lanesJSON), "RUNNER_TEMP": dir,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"), "BAZEL_TEST_FAIL": "1",
	}); err == nil {
		t.Errorf("seed step passed a failed analysis:\n%s", out)
	}
	if step.If != "steps.writer.outputs.cert != ''" {
		t.Errorf("seed step if %q; it must skip when the mint switched seeding off (no certificate)", step.If)
	}
}

// rrcMintCurlStub stands in for curl in rrc-writer-credential.sh. The OIDC
// request (its URL carries audience=) logs the audience and authorization
// and answers a token. A mint request logs its Authorization header and
// answers as the space-separated RBE_TEST_MINT says, one word per request
// (the last repeats): ok (a certificate for the runner's key, CN
// rbe-rrc-writer O=gascity), cn, endpoint, instance, otherkey, nocert (each
// wrong in that one way), 000 (connection refused) or an HTTP status with
// an error body.
const rrcMintCurlStub = `#!/usr/bin/env bash
set -euo pipefail
out= url= headers=()
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	-H) headers+=("$2"); shift 2 ;;
	-w | --data | --connect-timeout | --max-time | --retry) shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
case "$url" in
*audience=*)
	echo "oidc audience=${url##*audience=} auth=${headers[0]}" >>"$RBE_TEST_MINT_LOG"
	echo '{"value": "oidc-jwt-value"}'
	exit 0
	;;
esac
echo "mint $url auth=${headers[0]}" >>"$RBE_TEST_MINT_LOG"
n=$(grep -c '^mint ' "$RBE_TEST_MINT_LOG")
read -r -a answers <<<"$RBE_TEST_MINT"
i=$((n - 1)); [ "$i" -lt "${#answers[@]}" ] || i=$((${#answers[@]} - 1))
answer=${answers[$i]}
key="$BAZEL_CI_SECRET_DIR/rrc-writer.key" cn=rbe-rrc-writer endpoint=grpcs://rbe-west.ops.gascity.com:443 instance=oss
case "$answer" in
ok | cn | endpoint | instance | otherkey | nocert) ;;
000) echo "curl: (7) Failed to connect" >&2; exit 7 ;;
*) printf '{"error": "stub %s"}' "$answer" >"$out"; printf '%s' "$answer"; exit 0 ;;
esac
case "$answer" in
cn) cn=rbe-ci ;;
endpoint) endpoint=grpcs://attacker.example:443 ;;
instance) instance=main ;;
otherkey) openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out other.key 2>/dev/null; key=other.key ;;
esac
pem=
if [ "$answer" != nocert ]; then
	pem=$(openssl req -new -x509 -key "$key" -subj "/O=gascity/CN=$cn" -days 1 2>/dev/null)
fi
jq -cn --arg pem "$pem" --arg e "$endpoint" --arg i "$instance" --arg cn "$cn" \
	'{cert_pem: $pem, endpoint: $e, instance: $i, cn: $cn}' >"$out"
printf 200
`

// TestRRCWriterCredential runs rrc-writer-credential.sh against a stubbed
// curl (rrcMintCurlStub): it asks GitHub for an OIDC token with audience
// rbe-rrc-writer, masks it, sends it to the mint, retries only 429, 502 and
// the network, treats 503 or an unreachable mint as seeding off (no
// outputs, exit 0), and accepts only a certificate for the key it generated,
// CN rbe-rrc-writer[-x] O=gascity, for rbe-west's endpoint and instance oss.
func TestRRCWriterCredential(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script := filepath.Join(repoRoot(t), bazelRRCCredential)
	for _, c := range []struct {
		answers string
		ok, off bool
		mints   int
	}{
		{"ok", true, false, 1},
		{"429 ok", true, false, 2},
		{"502 ok", true, false, 2},
		{"000 ok", true, false, 2},
		{"502", false, false, 4},
		{"403", false, false, 1},
		{"401", false, false, 1},
		{"409", false, false, 1},
		{"cn", false, false, 1},
		{"endpoint", false, false, 1},
		{"instance", false, false, 1},
		{"otherkey", false, false, 1},
		{"nocert", false, false, 1},
		{"503", true, true, 1},
		{"000", true, true, 4},
	} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"curl": rrcMintCurlStub, "sleep": "#!/bin/sh\nexit 0\n"} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		secret := filepath.Join(t.TempDir(), "secret")
		output := filepath.Join(dir, "output")
		mintLog := filepath.Join(dir, "mint.log")
		out, err := runWorkflowStepScript(t, dir, "bash "+script, map[string]string{
			"PATH":                           bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"BAZEL_CI_SECRET_DIR":            secret,
			"GITHUB_OUTPUT":                  output,
			"ACTIONS_ID_TOKEN_REQUEST_URL":   "https://token.invalid/oidc?api-version=2.0",
			"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
			"RBE_TEST_MINT":                  c.answers,
			"RBE_TEST_MINT_LOG":              mintLog,
		})
		if (err == nil) != c.ok {
			t.Errorf("%q: ok=%v, want %v\n%s", c.answers, err == nil, c.ok, out)
			continue
		}
		log := readFile(t, dir, "mint.log")
		if !strings.Contains(log, "oidc audience=rbe-rrc-writer auth=Authorization: bearer request-token\n") {
			t.Errorf("%q: OIDC request %q", c.answers, log)
		}
		if got := strings.Count(log, "mint https://rbe-mint.ops.gascity.com:8444/v1/rrc-writer/cert auth=Authorization: Bearer oidc-jwt-value\n"); got != c.mints {
			t.Errorf("%q: %d mint requests with the OIDC token, want %d:\n%s", c.answers, got, c.mints, log)
		}
		if !strings.Contains(out, "::add-mask::oidc-jwt-value") {
			t.Errorf("%q: the OIDC token is not masked:\n%s", c.answers, out)
		}
		if !c.ok {
			continue
		}
		if c.off {
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Errorf("%q: wrote outputs (%v); the seed must skip", c.answers, err)
			}
			if !strings.Contains(out, "seeding off (HTTP "+c.answers[len(c.answers)-3:]+")") {
				t.Errorf("%q: no seeding-off warning:\n%s", c.answers, out)
			}
			continue
		}
		for name, want := range map[string]string{
			"cert": filepath.Join(secret, "rrc-writer.crt"), "key": filepath.Join(secret, "rrc-writer.key"),
			"endpoint": "grpcs://rbe-west.ops.gascity.com:443", "instance": "oss",
		} {
			if got, _ := readStepOutput(t, output, name); got != want {
				t.Errorf("%q: output %s=%q, want %q", c.answers, name, got, want)
			}
		}
		if key := readFile(t, secret, "rrc-writer.key"); !strings.Contains(key, "-----BEGIN PRIVATE KEY-----") {
			t.Errorf("%q: key is not PKCS#8 (Bazel's TLS refuses SEC1)", c.answers)
		}
		if fi, err := os.Stat(filepath.Join(secret, "rrc-writer.key")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%q: key mode %v (%v), want 0600", c.answers, fi.Mode().Perm(), err)
		}
		if _, err := os.Stat(filepath.Join(secret, "rrc-mint.json")); !os.IsNotExist(err) {
			t.Errorf("%q: the mint reply stays on disk (%v)", c.answers, err)
		}
	}

	dir := t.TempDir()
	if out, err := runWorkflowStepScript(t, dir, "bash "+script, map[string]string{
		"BAZEL_CI_SECRET_DIR": filepath.Join(t.TempDir(), "secret"), "GITHUB_OUTPUT": filepath.Join(dir, "output"),
	}); err == nil || !strings.Contains(out, "id-token: write") {
		t.Errorf("without an OIDC request URL: err %v, want a failure naming id-token: write\n%s", err, out)
	}
}

const (
	bazelRRCVerifyJob   = "rrc-verify"
	bazelRRCVerifyJobIf = "(github.event_name == 'schedule' || github.event_name == 'workflow_dispatch') && needs.rbe.outputs.mode == 'remote' && needs.rbe.outputs.rrc != 'off'"
	bazelRRCVerifyTool  = "tools/bazel/rrc_verify.py"
)

// TestBazelRRCVerifyJob: the nightly check (ga-vnycm2.26) runs from
// bazel-nightly.yml (schedule) or a dispatch while the cache is in use,
// fetches every lane cold without the repo contents cache, compares with
// rrc_verify.py using the lanes' read-only certificate, gates nothing, and
// alerts through an issue only when the comparison fails.
func TestBazelRRCVerifyJob(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	job, ok := wf.Jobs[bazelRRCVerifyJob]
	if !ok {
		t.Fatalf("%s has no %s job", bazelMultiLaneWorkflow, bazelRRCVerifyJob)
	}
	if job.If != bazelRRCVerifyJobIf {
		t.Errorf("%s if %q, want %q", bazelRRCVerifyJob, job.If, bazelRRCVerifyJobIf)
	}
	for _, c := range []struct {
		event, mode, rrc string
		want             bool
	}{
		{"schedule", "remote", "on", true},
		{"schedule", "remote", "seed", true},
		{"workflow_dispatch", "remote", "canary", true},
		{"schedule", "remote", "off", false},
		{"schedule", "cache", "on", false},
		{"push", "remote", "on", false},
		{"pull_request", "remote", "on", false},
		{"merge_group", "remote", "on", false},
	} {
		got := evalGHIf(t, job.If, map[string]string{
			"github.event_name": c.event, "needs.rbe.outputs.mode": c.mode, "needs.rbe.outputs.rrc": c.rrc,
		})
		if got != c.want {
			t.Errorf("%+v: verify runs %v, want %v", c, got, c.want)
		}
	}
	if want := map[string]string{"contents": "read", "issues": "write"}; !reflect.DeepEqual(job.Permissions, want) {
		t.Errorf("%s permissions %v, want %v", bazelRRCVerifyJob, job.Permissions, want)
	}
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "experimental_remote_repo_contents_cache") || strings.Contains(s.Run, ".bazelrc.local") ||
			strings.Contains(s.Run, "remote_upload_local_results") {
			t.Errorf("%s step %q reads or writes the repo contents cache; the cold fetch must not", bazelRRCVerifyJob, s.Name)
		}
	}
	fetch := bazelRRCJobStep(t, job, func(s multiLaneStep) bool { return strings.HasPrefix(s.Name, "Cold fetch") }, "cold fetch")
	if fetch.Env["LANES"] != "${{ needs.rbe.outputs.lanes }}" || !strings.Contains(fetch.Run, "--nobuild") {
		t.Errorf("cold fetch env %v; it must run every lane's command (needs.rbe.outputs.lanes) with --nobuild", fetch.Env)
	}
	verify := bazelRRCJobStep(t, job, func(s multiLaneStep) bool { return s.ID == "verify" }, "verify")
	for _, want := range []string{"python3 " + bazelRRCVerifyTool, `--cert "$SECRET_DIR/client.crt"`, `--key "$SECRET_DIR/client.key"`, "--instance oss", ".bazelversion"} {
		if !strings.Contains(verify.Run, want) {
			t.Errorf("verify step lacks %q", want)
		}
	}
	alert := bazelRRCJobStep(t, job, func(s multiLaneStep) bool { return strings.Contains(s.Run, "gh issue") }, "alert")
	if alert.If != "failure() && steps.verify.outcome == 'failure'" {
		t.Errorf("alert step if %q; it must alert only on a failed comparison", alert.If)
	}
	if strings.Contains(alert.Run, "${{") {
		t.Errorf("alert step interpolates an expression into its script; pass it through env")
	}

	nightly := readFile(t, repoRoot(t), ".github/workflows/bazel-nightly.yml")
	if !strings.Contains(nightly, "      issues: write\n    uses: ./.github/workflows/bazel.yml") {
		t.Errorf("bazel-nightly.yml's call must grant issues: write, or rrc-verify cannot alert")
	}
}
