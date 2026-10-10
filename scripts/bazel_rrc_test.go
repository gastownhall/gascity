package scripts_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// The fork-cache reader (mode cache, and a fork lane that fell back to the
// read-only cache): the same lines as the trusted reader, from rbe-cache's
// anonymous AC and CAS reads, only while cache-rrc-probe.sh passes (rbe-cache
// answers and its committed kill switch is on). Fork pull_request runs see
// no repository variables, so neither the rbe job's rrc output nor
// bazelRRCVar gates it.
const (
	bazelForkRRCReadStep   = "Fork cache remote repo contents cache (read)"
	bazelForkRRCReadStepIf = bazelCacheZstdStepIf
	bazelCacheRRCProbe     = "tools/rbe/cache-rrc-probe.sh"
	bazelCacheRRCProbeGate = "if why=$(bash " + bazelCacheRRCProbe + " 2>&1); then"
	bazelCacheRRCKillLine  = "fork_rrc_read=on"
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

// TestBazelRRCReadStep: the trusted reader runs in mode remote only (fork
// modes and local never read through it; fork-cache lanes have their own,
// probe-gated reader, TestBazelForkRRCReadStep), on the unit lane under
// canary and every lane under on, and writes exactly bazelRRCReadLines. It
// never enables uploads.
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

// TestBazelForkRRCReadStep: the fork-cache reader runs exactly where the
// lane uses fork-cache (mode cache, or a fork lane that fell back to it),
// never with the trusted reader and never in mode remote, local or a fork
// mode that kept its rbe-fork certificate. It reads no variable or secret,
// probes rbe-cache once, writes bazelRRCReadLines only when the probe
// passes, and otherwise writes nothing and leaves a notice saying why. It
// never enables uploads, and comes before the lane's bazel command.
func TestBazelForkRRCReadStep(t *testing.T) {
	step := bazelRCLocalLaneStep(t, bazelForkRRCReadStep)
	trusted := bazelRCLocalLaneStep(t, bazelRRCReadStep)
	if step.If != bazelForkRRCReadStepIf {
		t.Errorf("%q if %q, want %q (the lanes setup-bazel attaches fork-cache to)", step.Name, step.If, bazelForkRRCReadStepIf)
	}
	for _, mode := range []string{"remote", "fork-ro", "fork-rw", "cache", "local"} {
		for _, fallback := range []string{"success", "failure", "skipped", ""} {
			if fallback == "success" && !strings.HasPrefix(mode, "fork-") {
				continue // only a fork mode's lane can fall back
			}
			for _, rrc := range []string{"off", "seed", "canary", "on"} {
				for lane := range multiLaneCommands {
					ctx := map[string]string{
						"needs.rbe.outputs.mode": mode, "needs.rbe.outputs.rrc": rrc, "matrix.lane": lane,
						"steps.bazel-fallback.outcome": fallback,
					}
					want := mode == "cache" || fallback == "success"
					got := evalGHIf(t, step.If, ctx)
					if got != want {
						t.Errorf("mode %s fallback %q rrc %s lane %s: fork reader runs %v, want %v", mode, fallback, rrc, lane, got, want)
					}
					if got && evalGHIf(t, trusted.If, ctx) {
						t.Errorf("mode %s fallback %q rrc %s lane %s: both readers run", mode, fallback, rrc, lane)
					}
				}
			}
		}
	}
	for k, v := range step.Env {
		if strings.Contains(v, "vars.") || strings.Contains(v, "secrets.") {
			t.Errorf("%q env %s = %q; fork runs see no vars or secrets", step.Name, k, v)
		}
	}
	for _, bad := range []string{"vars.", "secrets.", "upload", "${{"} {
		if strings.Contains(step.Run, bad) {
			t.Errorf("%q script contains %q; it reads nothing but the probe and writes only the reader's lines", step.Name, bad)
		}
	}
	if strings.Count(step.Run, bazelCacheRRCProbeGate) != 1 {
		t.Errorf("%q does not gate the reader's lines on %q once", step.Name, bazelCacheRRCProbeGate)
	}
	for probe, want := range map[string][]string{"": nil, "ok": bazelRRCReadLines} {
		run := runBazelRCLocalStepProbes(t, step.Run, map[string]string{"BAZEL_TEST_RRC_PROBE": probe})
		if run.rrcProbes != 1 || run.zstdProbes != 0 {
			t.Errorf("probe %q: the step ran the rrc probe %d and the zstd probe %d times, want once and never", probe, run.rrcProbes, run.zstdProbes)
		}
		if !slices.Equal(run.lines, want) {
			t.Errorf("probe %q: .bazelrc.local %q, want %q", probe, run.lines, want)
		}
		notice := "::notice title=No remote repo contents cache::rbe-cache rrc probe: stub refused"
		if hasNotice := strings.Contains(run.out, notice); hasNotice != (want == nil) {
			t.Errorf("probe %q: notice %v, want %v (the probe's reason, only when it fails):\n%s", probe, hasNotice, want == nil, run.out)
		}
		// The verdict, with the probe's reason (rbe-cache's connect time
		// when it has one), in the lane's step summary either way.
		summary := "Fork cache remote repo contents cache read: **off** (stub refused)\n"
		if want != nil {
			summary = "Fork cache remote repo contents cache read: **on** (stub answers)\n"
		}
		if run.summary != summary {
			t.Errorf("probe %q: step summary %q, want %q", probe, run.summary, summary)
		}
	}
	wf := readMultiLaneWorkflow(t)
	read, test := -1, -1
	for i, s := range wf.Jobs["lane"].Steps {
		switch {
		case s.Name == bazelForkRRCReadStep:
			read = i
		case s.ID == "test":
			test = i
		}
	}
	if read < 0 || test < 0 || read > test {
		t.Errorf("lane job: %q at %d, the bazel step at %d; the reader must come first", bazelForkRRCReadStep, read, test)
	}
}

// cacheRRCProbeCases runs cache-rrc-probe.sh against serve (TestCacheProbes):
// only a gRPC status 0 GetCapabilities answer with cache_capabilities, then
// NOT_FOUND for the probe's GetActionResult key and ByteStream Read blob,
// passes; every error, refusal, timeout or unreadable answer fails, and so
// does a copy with the kill switch off, without asking rbe-cache at all. The probe never writes
// stdout and always says why on stderr, with the TCP connect time (without
// the DNS lookup) once it has one.
func cacheRRCProbeCases(t *testing.T, serve capsServer) {
	root := repoRoot(t)
	probeText := readFile(t, root, bazelCacheRRCProbe)
	for _, want := range []string{
		"\n" + bazelCacheRRCKillLine + "\n",
		"url=${RBE_CACHE_PROBE_URL:-https://rbe-cache.ops.gascity.com:8443}\n",
		`printf '\000\000\000\000\005\012\003oss'`,
		"--connect-timeout 3 --max-time \"$max_time\"",
		"max_time=${RBE_CACHE_PROBE_MAX_TIME:-5}\n",
	} {
		if strings.Count(probeText, want) != 1 {
			t.Errorf("%s lacks %q (once)", bazelCacheRRCProbe, want)
		}
	}
	switchedOff := filepath.Join(t.TempDir(), "cache-rrc-probe.sh")
	if err := os.WriteFile(switchedOff, []byte(strings.Replace(probeText, "\n"+bazelCacheRRCKillLine+"\n", "\nfork_rrc_read=off\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, script string, env map[string]string) (bool, string, string) {
		t.Helper()
		dir := t.TempDir()
		env["RBE_CACHE_PROBE_MAX_TIME"] = "1"
		out, err := runWorkflowStepScript(t, dir, "bash "+strconv.Quote(script)+" >stdout.out 2>stderr.out\n", env)
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("run %s: %v\n%s", script, err, out)
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
			t.Errorf("probe wrote stdout %q; the rc step would mistake it for its reason", stdout)
		}
		verdict := "; this lane fetches external repositories itself\n"
		if want {
			verdict = "; this lane reads the remote repo contents cache\n"
		}
		if !strings.HasPrefix(stderr, "rbe-cache rrc probe: ") || !strings.HasSuffix(stderr, verdict) || strings.Count(stderr, "\n") != 1 {
			t.Errorf("probe stderr %q; want one verdict line ending %q", stderr, verdict)
		}
	}
	live := grpcMessage(capsZstd)
	acWith := func(a capsAnswer) capsAnswer { return capsAnswer{grpc: "0", body: live, ac: &a} }
	bsWith := func(a capsAnswer) capsAnswer { return capsAnswer{grpc: "0", body: live, bs: &a} }
	for _, c := range []struct {
		name   string
		answer capsAnswer
		want   bool
		asks   int32 // GetCapabilities, GetActionResult, ByteStream Read: each only after a good answer
	}{
		{"rbe-cache today", capsAnswer{grpc: "0", body: live}, true, 3},
		{"rbe-cache before zstd", capsAnswer{grpc: "0", body: grpcMessage(capsLive)}, true, 3},
		{"no cache_capabilities", capsAnswer{grpc: "0", body: grpcMessage(capsLiveAPIVersions)}, false, 1},
		{"gRPC error after the answer", capsAnswer{grpc: "13", body: live}, false, 1},
		{"no grpc-status", capsAnswer{body: live}, false, 1},
		{"trailers-only PERMISSION_DENIED", capsAnswer{grpc: "7"}, false, 1},
		{"HTTP 502", capsAnswer{status: http.StatusBadGateway, grpc: "0", body: live}, false, 1},
		{"compressed message", capsAnswer{grpc: "0", body: append([]byte{1}, live[1:]...)}, false, 1},
		{"truncated message", capsAnswer{grpc: "0", body: live[:len(live)-1]}, false, 1},
		{"not gRPC", capsAnswer{grpc: "0", body: []byte("<html>bad gateway</html>")}, false, 1},
		{"timeout", capsAnswer{grpc: "0", body: live, delay: 10 * time.Second}, false, 1},
		{"action cache answers the key", acWith(capsAnswer{grpc: "0", body: grpcMessage(nil)}), false, 2},
		{"action cache PERMISSION_DENIED", acWith(capsAnswer{grpc: "7"}), false, 2},
		{"action cache UNAVAILABLE", acWith(capsAnswer{grpc: "14"}), false, 2},
		{"action cache no grpc-status", acWith(capsAnswer{body: []byte{}}), false, 2},
		{"action cache HTTP 502", acWith(capsAnswer{status: http.StatusBadGateway, grpc: "5"}), false, 2},
		{"action cache timeout", acWith(capsAnswer{grpc: "5", delay: 10 * time.Second}), false, 2},
		{"CAS serves the blob", bsWith(capsAnswer{grpc: "0", body: grpcMessage(pbBytes(10, []byte{0}))}), false, 3},
		{"CAS UNAVAILABLE", bsWith(capsAnswer{grpc: "14"}), false, 3},
		{"CAS PERMISSION_DENIED", bsWith(capsAnswer{grpc: "7"}), false, 3},
		{"CAS HTTP 502", bsWith(capsAnswer{status: http.StatusBadGateway, grpc: "5"}), false, 3},
		{"CAS timeout", bsWith(capsAnswer{grpc: "5", delay: 10 * time.Second}), false, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, ca, hits := serve(t, c.answer)
			start := time.Now()
			ok, stdout, stderr := run(t, filepath.Join(root, bazelCacheRRCProbe), map[string]string{"RBE_CACHE_PROBE_URL": url, "CURL_CA_BUNDLE": ca})
			check(t, ok, stdout, stderr, c.want)
			if hits.Load() != c.asks {
				t.Errorf("probe asked %d times, want %d", hits.Load(), c.asks)
			}
			if c.asks > 1 && !regexp.MustCompile(` \(tcp connect [0-9]+\.[0-9] ms\); `).MatchString(stderr) {
				t.Errorf("probe stderr %q lacks the GetCapabilities TCP connect time", stderr)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("probe took %v; RBE_CACHE_PROBE_MAX_TIME=1 bounds each call", d)
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		ok, stdout, stderr := run(t, filepath.Join(root, bazelCacheRRCProbe), map[string]string{"RBE_CACHE_PROBE_URL": refusedProbeURL})
		check(t, ok, stdout, stderr, false)
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		url, _, hits := serve(t, capsAnswer{grpc: "0", body: live})
		ok, stdout, stderr := run(t, filepath.Join(root, bazelCacheRRCProbe), map[string]string{"RBE_CACHE_PROBE_URL": url})
		check(t, ok, stdout, stderr, false)
		if hits.Load() != 0 {
			t.Errorf("probe completed a request through an untrusted certificate")
		}
	})
	t.Run("kill switch off", func(t *testing.T) {
		url, ca, hits := serve(t, capsAnswer{grpc: "0", body: live})
		ok, stdout, stderr := run(t, switchedOff, map[string]string{"RBE_CACHE_PROBE_URL": url, "CURL_CA_BUNDLE": ca})
		check(t, ok, stdout, stderr, false)
		if !strings.Contains(stderr, "switched off") {
			t.Errorf("switched-off probe stderr %q does not say it is switched off", stderr)
		}
		if hits.Load() != 0 {
			t.Errorf("switched-off probe asked rbe-cache %d times, want 0", hits.Load())
		}
	})
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
	// The issue's containment: fork-cache lanes see no repository variables,
	// so the committed switches (both repositories) and rbe-cache-gate stop
	// them. The trusted readers read the same entries, so a mismatch sets
	// the variable to seed (no reader, rrc-seed and this check keep running);
	// off stops seeding and this check too.
	for _, want := range []string{
		"fork_rrc_read=off in both probe scripts", bazelCacheRRCProbe, ".github/actions/setup-bazel/cache-rrc-probe.sh",
		"rbe-cache-gate close", "on a mismatch, set RBE_REPO_CONTENTS_CACHE to seed", "set it to off only to stop seeding too",
		"could not check, leave the variable as it is",
	} {
		if !strings.Contains(alert.Run, want) {
			t.Errorf("alert issue body lacks %q", want)
		}
	}
	for _, bad := range []string{"Stop readers first: set the repository variable", "leave RBE_REPO_CONTENTS_CACHE on"} {
		if strings.Contains(alert.Run, bad) {
			t.Errorf("alert issue body still says %q; a mismatch sets the variable to seed", bad)
		}
	}

	nightly := readFile(t, repoRoot(t), ".github/workflows/bazel-nightly.yml")
	if !strings.Contains(nightly, "      issues: write\n    uses: ./.github/workflows/bazel.yml") {
		t.Errorf("bazel-nightly.yml's call must grant issues: write, or rrc-verify cannot alert")
	}
}
