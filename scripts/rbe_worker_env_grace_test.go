package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rbe-west's grace window after a Go re-pin (tools/rbe/worker-env-grace,
// TESTING.md "Grace window for Go re-pins"). gastownhall/beads carries a
// byte-identical copy of the worker-env manifest and pin, and rbe-west's
// pool workers serve main's pin alone, so the push that moves main's pin
// leaves every beads remote action without a worker until beads' own sync
// PR lands (about an hour: its CI runs uncached). A re-pin that moves only
// the manifest's go line can be undone on today's Blacksmith image: the
// pinned rbe-worker installs go.mod's Go, so a worker given this main's
// product files with the previous Go measures the previous host exactly.
//
//   - On such a push, worker-env-notify-beads.yml's grace job writes a
//     record of the previous pin and a window (record) and commits it to
//     refs/heads/rbe/worker-env-grace (publish). Any other re-pin publishes
//     "previous none".
//   - rbe-west's OSS pool scaler reads the record and, inside the window,
//     dispatches rbe-worker-legacy.yml: a worker that rebuilds the previous
//     host's product files from the record (fetch, host), checks that its
//     host measures the previous pin and serves it.
//   - beads' preflight accepts the previous pin until the window's until.
//
// These tests run the script against fixture repositories and pin the
// legacy workflow's and the grace job's shape.

const (
	rbeWorkerEnvGrace       = "tools/rbe/worker-env-grace"
	rbeWorkerLegacyWorkflow = ".github/workflows/rbe-worker-legacy.yml"
	rbeGraceBranch          = "rbe/worker-env-grace"
	rbeGraceFile            = "worker-env-grace.txt"
	rbeGraceLegacyJob       = "rbe legacy worker (Blacksmith, OSS scheduler)"
	rbeGraceAcceptS         = 7200
	rbeGraceDrainS          = 3600
	// 2026-10-21T01:10:30Z: about when gascity#8165's record would have gone up.
	rbeGraceNow = 1792545030
)

// graceManifest is a worker-env manifest in the real one's shape: sorted,
// with the given Go, dolt and libc6.
func graceManifest(goVersion, dolt, libc string) string {
	return "arch x86_64\n" +
		"dolt dolt version " + dolt + "\n" +
		"go go version go" + goVersion + " linux/amd64\n" +
		"os ubuntu 24.04\n" +
		"pkg bash 5.2\n" +
		"pkg libc6 " + libc + "\n" +
		"tool yq 4\n"
}

// graceRepo is a fixture gascity: a main history with the worker host's
// files and the real grace and drift scripts, and a bare "GitHub" the
// record is published to and fetched from.
type graceRepo struct {
	t      *testing.T
	root   string // this repository: the scripts under test
	dir    string // the fixture's working repository
	github string // a bare repository standing in for gastownhall/gascity on GitHub
	sha    map[string]string
	env    []string
}

func newGraceRepo(t *testing.T) *graceRepo {
	t.Helper()
	g := &graceRepo{t: t, root: repoRoot(t), sha: map[string]string{}}
	base := t.TempDir()
	g.dir = filepath.Join(base, "gascity")
	g.github = filepath.Join(base, "github.git")
	g.env = append(notifyBeadsGitEnv(base), "GITHUB_REPOSITORY=gastownhall/gascity", "GITHUB_SERVER_URL=https://github.com",
		"WORKER_ENV_GRACE_REMOTE=file://"+g.github)
	g.git(base, "init", "-q", "-b", "main", g.dir)
	g.git(base, "init", "-q", "--bare", g.github)
	// Serve blob-less fetches and later blob requests as GitHub does, so
	// publish's main check reads main's pin lazily here too.
	g.git(g.github, "config", "uploadpack.allowFilter", "true")
	g.git(g.github, "config", "uploadpack.allowAnySHA1InWant", "true")
	for _, script := range []string{rbeWorkerEnvGrace, rbeWorkerEnvDrift} {
		writeTestFile(t, filepath.Join(g.dir, script), readFile(t, g.root, script))
		if err := os.Chmod(filepath.Join(g.dir, script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// git runs git in dir with the fixture's environment (no system or global
// config) and returns its trimmed stdout.
func (g *graceRepo) git(dir string, args ...string) string {
	g.t.Helper()
	return g.gitEnv(dir, nil, args...)
}

// gitEnv is git with env added to the fixture's environment.
func (g *graceRepo) gitEnv(dir string, env []string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append([]string{}, g.env...), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// commit records name: the worker host's files (manifest, its pin unless
// pin is given, go.mod's Go) on top of main, committed now.
func (g *graceRepo) commit(name, manifest, pin, goVersion string) {
	g.t.Helper()
	g.commitAt(name, manifest, pin, goVersion, 0)
}

// commitAt is commit with its author and committer time at unix time at
// (0: now).
func (g *graceRepo) commitAt(name, manifest, pin, goVersion string, at int) {
	g.t.Helper()
	var env []string
	if at != 0 {
		env = []string{"GIT_AUTHOR_DATE=@" + strconv.Itoa(at) + " +0000", "GIT_COMMITTER_DATE=@" + strconv.Itoa(at) + " +0000"}
	}
	if pin == "" {
		pin = sha256Pin(manifest)
	}
	writeTestFile(g.t, filepath.Join(g.dir, rbeWorkerEnvManifest), manifest)
	writeTestFile(g.t, filepath.Join(g.dir, rbeWorkerPlatformBuild), buildWithPin(pin))
	writeTestFile(g.t, filepath.Join(g.dir, "go.mod"), "module github.com/gastownhall/gascity\n\ngo "+goVersion+"\n")
	writeTestFile(g.t, filepath.Join(g.dir, "commit.txt"), name+"\n")
	g.git(g.dir, "add", "-A")
	g.gitEnv(g.dir, env, "commit", "-q", "-m", name)
	g.sha[name] = g.git(g.dir, "rev-parse", "HEAD")
}

// pushMain makes the "GitHub" repository's main the fixture's main: publish
// checks that main still pins the record's pin.
func (g *graceRepo) pushMain() {
	g.t.Helper()
	g.git(g.dir, "push", "-q", "--force", g.github, "HEAD:refs/heads/main")
}

// run runs the grace script in dir with the fixture's environment plus env.
func (g *graceRepo) run(dir string, env []string, args ...string) (string, string, error) {
	g.t.Helper()
	return runRBEScript(dir, append(append([]string{}, g.env...), env...), filepath.Join(g.dir, rbeWorkerEnvGrace), args...)
}

// graceRecord is a parsed record: key -> value, and the order keys came in.
type graceRecord struct {
	fields map[string]string
	keys   []string
}

func parseGraceRecord(t *testing.T, text string) graceRecord {
	t.Helper()
	r := graceRecord{fields: map[string]string{}}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("record line %q is not key value:\n%s", line, text)
		}
		if _, dup := r.fields[k]; dup {
			t.Fatalf("record repeats %s:\n%s", k, text)
		}
		r.fields[k] = v
		r.keys = append(r.keys, k)
	}
	return r
}

// graceRecordText writes a record from key, value pairs (in that order).
func graceRecordText(kv ...string) string {
	var b strings.Builder
	b.WriteString("# a fixture record\n")
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(kv[i] + " " + kv[i+1] + "\n")
	}
	return b.String()
}

// openRecord is the record gascity publishes for a Go re-pin from previous
// (Go goVersion) to pin, sha, at since.
func openRecord(pin, sha, previous, goVersion string, since int) []string {
	return []string{
		"version", "1", "pin", pin, "sha", sha, "previous", previous, "go", goVersion,
		"since", strconv.Itoa(since), "until", strconv.Itoa(since + rbeGraceAcceptS),
		"serve-until", strconv.Itoa(since + rbeGraceAcceptS + rbeGraceDrainS),
	}
}

func setRecordField(kv []string, key, value string) []string {
	out := append([]string{}, kv...)
	for i := 0; i+1 < len(out); i += 2 {
		if out[i] == key {
			out[i+1] = value
			return out
		}
	}
	return append(out, key, value)
}

func dropRecordField(kv []string, key string) []string {
	var out []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != key {
			out = append(out, kv[i], kv[i+1])
		}
	}
	return out
}

// TestWorkerEnvGraceRecord: the record for a push that moved main's pin.
// Only a re-pin whose manifest moved in its go line alone gets a window, of
// exactly 2 h for beads and 1 h more of service; an image change, a dolt
// change, a pin that does not hash its manifest, or no main before the push
// gets "previous none" with the reason. A push that did not move the pin
// gets no record and exit 2: the grace job, which also runs when repin
// could not fetch main before the push, then publishes nothing.
func TestWorkerEnvGraceRecord(t *testing.T) {
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	g.commit("c0", m0, "", "1.26.9")
	g.commit("c1", m1, "", "1.26.10")                                          // Go re-pin
	g.commit("c2", graceManifest("1.26.10", "2.1.8", "2.41"), "", "1.26.10")   // image: libc6
	g.commit("c3", graceManifest("1.26.11", "2.1.9", "2.41"), "", "1.26.11")   // Go and dolt
	g.commit("c4", graceManifest("1.26.12", "2.1.9", "2.41"), "", "1.26.12")   // Go again
	g.commit("bad", m0, sha256Pin("not the manifest\n"), "1.26.9")             // pin does not hash its manifest
	g.commit("c5", graceManifest("1.26.13", "2.1.9", "2.41"), "", "1.26.13")   // Go re-pin from bad
	g.commit("same", graceManifest("1.26.13", "2.1.9", "2.41"), "", "1.26.13") // the pin did not move
	var many strings.Builder
	for i := 0; i < 40; i++ {
		many.WriteString("pkg lib" + strconv.Itoa(100+i) + " 1.0\n")
	}
	g.commit("c6", graceManifest("1.26.14", "2.1.9", "2.41")+many.String(), "", "1.26.14") // a new image: 40 new packages
	pin := func(name string) string {
		return regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(g.git(g.dir, "show", g.sha[name]+":"+rbeWorkerPlatformBuild))
	}

	record := func(before, after string) (graceRecord, string, error) {
		t.Helper()
		b := ""
		if before != "" {
			b = g.sha[before]
		}
		out, stderr, err := g.run(g.dir, nil, "record", b, g.sha[after], strconv.Itoa(rbeGraceNow))
		if err != nil {
			return graceRecord{}, out + stderr, err
		}
		return parseGraceRecord(t, out), out + stderr, nil
	}

	r, out, err := record("c0", "c1")
	if err != nil {
		t.Fatalf("record c0 c1: %v\n%s", err, out)
	}
	want := map[string]string{
		"version": "1", "pin": pin("c1"), "sha": g.sha["c1"], "previous": pin("c0"), "go": "1.26.9",
		"since":       strconv.Itoa(rbeGraceNow),
		"until":       strconv.Itoa(rbeGraceNow + rbeGraceAcceptS),
		"serve-until": strconv.Itoa(rbeGraceNow + rbeGraceAcceptS + rbeGraceDrainS),
	}
	if !reflect.DeepEqual(r.fields, want) {
		t.Errorf("record for a Go re-pin:\n%v\nwant\n%v\n%s", r.fields, want, out)
	}
	if wantKeys := []string{"version", "pin", "sha", "previous", "go", "since", "until", "serve-until"}; !reflect.DeepEqual(r.keys, wantKeys) {
		t.Errorf("record keys %v, want %v", r.keys, wantKeys)
	}
	if !strings.Contains(out, "# beads may pin "+pin("c0")+" until 2026-10-21T03:10:30Z; rbe-west serves it until 2026-10-21T04:10:30Z.") {
		t.Errorf("record has no human line for the window:\n%s", out)
	}

	for _, tc := range []struct {
		name, before, after string
		reason              string
	}{
		{name: "image change", before: "c1", after: "c2", reason: "the re-pin moves more than the go line: -pkg libc6 2.39, +pkg libc6 2.41"},
		{name: "Go and dolt", before: "c2", after: "c3", reason: "the re-pin moves more than the go line: -dolt dolt version 2.1.8, +dolt dolt version 2.1.9"},
		{name: "no main before the push", before: "", after: "c4", reason: "no main commit before the push to compare with"},
		{name: "before's pin does not hash its manifest", before: "bad", after: "c5", reason: "main's manifest before the push does not hash to its pin " + pin("bad")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, out, err := record(tc.before, tc.after)
			if err != nil {
				t.Fatalf("record: %v\n%s", err, out)
			}
			want := map[string]string{"version": "1", "pin": pin(tc.after), "sha": g.sha[tc.after], "previous": "none", "reason": tc.reason}
			if !reflect.DeepEqual(r.fields, want) {
				t.Errorf("record:\n%v\nwant\n%v", r.fields, want)
			}
			if !strings.Contains(out, "# no grace window: "+tc.reason) {
				t.Errorf("record has no human line saying why there is no window:\n%s", out)
			}
		})
	}

	// A long list of moved lines is cut at a line, about 300 characters in.
	if r, out, err := record("same", "c6"); err != nil || r.fields["previous"] != "none" ||
		!strings.HasPrefix(r.fields["reason"], "the re-pin moves more than the go line: +pkg lib100 1.0, +pkg lib101 1.0, ") ||
		!strings.HasSuffix(r.fields["reason"], " 1.0, ...") || len(r.fields["reason"]) > 360 {
		t.Errorf("record for an image with many new packages: %v %v\n%s", err, r.fields, out)
	}

	if _, out, err := record("c5", "same"); exitCode(err) != 2 || !strings.Contains(out, "did not move") || strings.Contains(out, "version 1") {
		t.Errorf("record for a push that did not move the pin: exit %d, want 2 and no record\n%s", exitCode(err), out)
	}
}

// TestWorkerEnvGraceRecordStartsAtTheCommit: the window starts at the
// re-pin commit's time (at most NOW), not when the record is written, so a
// late or re-run job writes the very same record (publish then skips it)
// and cannot open a fresh window for a re-pin hours old.
func TestWorkerEnvGraceRecordStartsAtTheCommit(t *testing.T) {
	g := newGraceRepo(t)
	merged := rbeGraceNow - 60
	g.commitAt("c0", graceManifest("1.26.9", "2.1.8", "2.39"), "", "1.26.9", merged-86400)
	g.commitAt("c1", graceManifest("1.26.10", "2.1.8", "2.39"), "", "1.26.10", merged)
	var records []string
	for _, now := range []int{rbeGraceNow, rbeGraceNow + 3*3600} {
		out, stderr, err := g.run(g.dir, nil, "record", g.sha["c0"], g.sha["c1"], strconv.Itoa(now))
		if err != nil {
			t.Fatalf("record at %d: %v\n%s", now, err, stderr)
		}
		r := parseGraceRecord(t, out)
		if r.fields["since"] != strconv.Itoa(merged) || r.fields["until"] != strconv.Itoa(merged+rbeGraceAcceptS) ||
			r.fields["serve-until"] != strconv.Itoa(merged+rbeGraceAcceptS+rbeGraceDrainS) {
			t.Errorf("record written at %d: since %s until %s serve-until %s, want the commit's %d plus 2 h and 3 h", now,
				r.fields["since"], r.fields["until"], r.fields["serve-until"], merged)
		}
		records = append(records, out)
	}
	if records[0] != records[1] {
		t.Errorf("a later run wrote another record:\n%s\nvs\n%s", records[0], records[1])
	}
	// A commit time ahead of NOW (a skewed clock) starts at NOW.
	out, _, err := g.run(g.dir, nil, "record", g.sha["c0"], g.sha["c1"], strconv.Itoa(merged-30))
	if r := parseGraceRecord(t, out); err != nil || r.fields["since"] != strconv.Itoa(merged-30) {
		t.Errorf("commit ahead of NOW: %v since %s, want NOW %d", err, r.fields["since"], merged-30)
	}
}

// graceCheckout lays out a checkout of main whose pin is manifest's: the two
// files host reads.
func graceCheckout(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, rbeWorkerEnvManifest), manifest)
	writeTestFile(t, filepath.Join(dir, rbeWorkerPlatformBuild), buildWithPin(sha256Pin(manifest)))
	return dir
}

// TestWorkerEnvGraceHost: the legacy worker's half. Inside the window, host
// writes this main's manifest with the record's Go as the previous host's
// product files, the three rbe-worker reads (go.mod, platforms/BUILD.bazel,
// tools/rbe/worker-env.txt), and refuses unless they hash to the record's
// previous pin: the record cannot name a host this main's manifest does not
// roll back to. Outside the window, or with no window, it serves nothing
// and exits 3 (the scaler dispatched a run gascity has no window for:
// failing feeds the scaler's breaker for these runs); a record for another
// main, a forged previous pin or a malformed record fails. The dispatch's
// minutes reach the worker's shell arithmetic, so anything but whole minutes
// fails; max-minutes stops the worker about serve-until, its setup and
// drain included.
func TestWorkerEnvGraceHost(t *testing.T) {
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	p0, p1 := sha256Pin(m0), sha256Pin(m1)
	sha := strings.Repeat("c", 40)
	open := openRecord(p1, sha, p0, "1.26.9", rbeGraceNow)
	co := graceCheckout(t, m1)

	host := func(kv []string, now int, env ...string) (string, map[string]string, string, error) {
		t.Helper()
		dir := t.TempDir()
		rec := filepath.Join(dir, "record.txt")
		writeTestFile(t, rec, graceRecordText(kv...))
		if kv == nil {
			writeTestFile(t, rec, "")
		}
		output := filepath.Join(dir, "output")
		root := filepath.Join(dir, "previous")
		out, stderr, err := g.run(co, append([]string{"GITHUB_OUTPUT=" + output}, env...), "host", rec, root, strconv.Itoa(now))
		outputs := map[string]string{}
		for _, k := range []string{"serve", "root", "previous", "go", "idle-minutes", "max-minutes"} {
			if v, ok := readStepOutput(t, output, k); ok {
				outputs[k] = v
			}
		}
		return root, outputs, out + stderr, err
	}

	// Inside the window: the previous host's files.
	root, outputs, out, err := host(open, rbeGraceNow+60)
	if err != nil {
		t.Fatalf("host inside the window: %v\n%s", err, out)
	}
	if got := readFile(t, root, rbeWorkerEnvManifest); got != m0 {
		t.Errorf("previous manifest:\n%s\nwant\n%s", got, m0)
	}
	if got := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(readFile(t, root, "go.mod")); got == nil || got[1] != "1.26.9" {
		t.Errorf("previous go.mod's go line %v, want 1.26.9:\n%s", got, readFile(t, root, "go.mod"))
	}
	if got := regexp.MustCompile(`(?m)^ *"worker-env": "(sha256:[0-9a-f]{64})",$`).FindAllStringSubmatch(readFile(t, root, rbeWorkerPlatformBuild), -1); len(got) != 1 || got[0][1] != p0 {
		t.Errorf("previous platforms/BUILD.bazel pins %v, want exactly %s (in the shape worker-env-drift's pin_of reads)", got, p0)
	}
	wantOut := map[string]string{"serve": "true", "root": root, "previous": p0, "go": "1.26.9", "idle-minutes": "15", "max-minutes": "171"}
	if !reflect.DeepEqual(outputs, wantOut) {
		t.Errorf("outputs %v, want %v", outputs, wantOut)
	}

	// max-minutes: the dispatch's, unless serve-until (less 8 minutes of
	// setup and drain) comes first; idle-minutes: the dispatch's.
	for _, tc := range []struct {
		now       int
		max, idle string
		want      string
		wantIdle  string
		refused   bool
	}{
		{now: rbeGraceNow, max: "300", want: "172", wantIdle: "15"},
		{now: rbeGraceNow, max: "60", idle: "8", want: "60", wantIdle: "8"},
		{now: rbeGraceNow + rbeGraceAcceptS + rbeGraceDrainS - 30, max: "300", want: "1", wantIdle: "15"},
		{now: rbeGraceNow, want: "172", wantIdle: "15"}, // MAX_MINUTES and IDLE_MINUTES unset: 300 and 15
		{now: rbeGraceNow, max: "0", refused: true},
		{now: rbeGraceNow, max: "5m", refused: true},
		{now: rbeGraceNow, idle: "0", refused: true},
		{now: rbeGraceNow, idle: "8 ", refused: true},
		// bash arithmetic would run the subscript: never let it reach the worker.
		{now: rbeGraceNow, idle: "BASH_VERSINFO[$(touch pwned)0]", refused: true},
	} {
		env := []string{}
		if tc.max != "" {
			env = append(env, "MAX_MINUTES="+tc.max)
		}
		if tc.idle != "" {
			env = append(env, "IDLE_MINUTES="+tc.idle)
		}
		_, outputs, out, err := host(open, tc.now, env...)
		if tc.refused {
			if err == nil || outputs["serve"] == "true" || !strings.Contains(out, "is not whole minutes") {
				t.Errorf("MAX_MINUTES=%q IDLE_MINUTES=%q: %v, outputs %v; want a refusal\n%s", tc.max, tc.idle, err, outputs, out)
			}
			continue
		}
		if err != nil || outputs["max-minutes"] != tc.want || outputs["idle-minutes"] != tc.wantIdle {
			t.Errorf("now %d MAX_MINUTES=%q IDLE_MINUTES=%q: %v, max-minutes %q idle-minutes %q, want %s %s\n%s",
				tc.now, tc.max, tc.idle, err, outputs["max-minutes"], outputs["idle-minutes"], tc.want, tc.wantIdle, out)
		}
	}
	if _, err := os.Stat(filepath.Join(co, "pwned")); !os.IsNotExist(err) {
		t.Errorf("an IDLE_MINUTES subscript ran a command: %v", err)
	}

	// No window: serve=false, exit 3, nothing written.
	for _, tc := range []struct {
		name string
		kv   []string
		now  int
	}{
		{name: "no record", kv: nil, now: rbeGraceNow},
		{name: "previous none", kv: []string{"version", "1", "pin", p1, "sha", sha, "previous", "none", "reason", "the re-pin moves more than the go line: -pkg libc6 2.39, +pkg libc6 2.41"}, now: rbeGraceNow},
		{name: "served until passed", kv: open, now: rbeGraceNow + rbeGraceAcceptS + rbeGraceDrainS},
		{name: "not open yet", kv: open, now: rbeGraceNow - 301},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outputs, out, err := host(tc.kv, tc.now)
			if exitCode(err) != 3 || !reflect.DeepEqual(outputs, map[string]string{"serve": "false"}) || !strings.Contains(out, "serves nothing") {
				t.Errorf("exit %d, outputs %v; want exit 3, serve=false alone, and why\n%s", exitCode(err), outputs, out)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("%s was written: %v", root, err)
			}
		})
	}
	// Clock skew: a record a few minutes ahead of this host is open.
	if _, outputs, out, err := host(open, rbeGraceNow-299); err != nil || outputs["serve"] != "true" {
		t.Errorf("record 299 s ahead: %v, outputs %v; want serve=true\n%s", err, outputs, out)
	}

	// Refused: the record does not describe this main, or names a previous
	// host its manifest does not roll back to, or is malformed.
	refused := map[string]struct {
		kv  []string
		why string
	}{
		"another main's record":    {setRecordField(open, "pin", sha256Pin("x\n")), "it describes another main"},
		"forged previous pin":      {setRecordField(open, "previous", sha256Pin("x\n")), "not the record's previous pin"},
		"Go that is not previous":  {setRecordField(open, "go", "1.26.8"), "not the record's previous pin"},
		"version 2":                {setRecordField(open, "version", "2"), "malformed record: version '2'"},
		"unknown key":              {append(append([]string{}, open...), "extra", "1"), "unknown key 'extra'"},
		"missing go":               {dropRecordField(open, "go"), "malformed record: go ''"},
		"missing serve-until":      {dropRecordField(open, "serve-until"), "malformed record: since"},
		"window over two hours":    {setRecordField(setRecordField(open, "until", strconv.Itoa(rbeGraceNow+rbeGraceAcceptS+1)), "serve-until", strconv.Itoa(rbeGraceNow+rbeGraceAcceptS+1+rbeGraceDrainS)), "is not within 7200s after since"},
		"drain over an hour":       {setRecordField(open, "serve-until", strconv.Itoa(rbeGraceNow+rbeGraceAcceptS+rbeGraceDrainS+1)), "is not within 3600s after until"},
		"until before since":       {setRecordField(open, "until", strconv.Itoa(rbeGraceNow)), "is not within 7200s after since"},
		"non-numeric since":        {setRecordField(open, "since", "soon"), "malformed record: since 'soon'"},
		"short sha":                {setRecordField(open, "sha", "abc"), "malformed record: sha 'abc'"},
		"uppercase pin":            {setRecordField(open, "previous", strings.ToUpper(p0)), "malformed record: previous"},
		"previous is the pin":      {setRecordField(open, "previous", p1), "previous is the pin"},
		"go with a shell char":     {setRecordField(open, "go", "1.26.9;x"), "malformed record: go '1.26.9;x'"},
		"reason with a window":     {append(append([]string{}, open...), "reason", "x"), "a reason with a window"},
		"none with a window field": {[]string{"version", "1", "pin", p1, "sha", sha, "previous", "none", "reason", "x", "since", "1"}, "previous none with a window"},
		"none without a reason":    {[]string{"version", "1", "pin", p1, "sha", sha, "previous", "none"}, "previous none without a reason"},
		"control character reason": {[]string{"version", "1", "pin", p1, "sha", sha, "previous", "none", "reason", "x\x1b[31m"}, "not printable ASCII"},
		"octal-looking since":      {setRecordField(open, "since", "0"+strconv.Itoa(rbeGraceNow)), "malformed record: since '0"},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			root, outputs, out, err := host(tc.kv, rbeGraceNow+60)
			if err == nil || outputs["serve"] == "true" || !strings.Contains(out, tc.why) {
				t.Errorf("%v, outputs %v; want a failure saying %q and no serve=true\n%s", err, outputs, tc.why, out)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("%s was written: %v", root, err)
			}
		})
	}
	// A duplicated key, and a line that is not "key value".
	for name, text := range map[string]string{
		"duplicate key":  graceRecordText(open...) + "go 1.26.9\n",
		"bare key":       graceRecordText(open...) + "go\n",
		"CRLF":           strings.ReplaceAll(graceRecordText(open...), "\n", "\r\n"),
		"lone CR":        graceRecordText(open...) + "# a comment\r::warning title=injected::x\n",
		"NUL":            graceRecordText(open...) + "# a\x00b\n",
		"over 64 KiB":    graceRecordText(open...) + strings.Repeat("# padding\n", 6600),
		"NUL in a value": strings.Replace(graceRecordText(open...), "version 1\n", "version 1\x00\n", 1),
		"blank line":     graceRecordText(open...) + "   \n",
	} {
		dir := t.TempDir()
		rec := filepath.Join(dir, "record.txt")
		writeTestFile(t, rec, text)
		out, stderr, err := g.run(co, []string{"GITHUB_OUTPUT=" + filepath.Join(dir, "output")}, "host", rec, filepath.Join(dir, "previous"), strconv.Itoa(rbeGraceNow+60))
		if v, _ := readStepOutput(t, filepath.Join(dir, "output"), "serve"); err == nil || v == "true" {
			t.Errorf("%s: %v, serve %q; want a failure\n%s%s", name, err, v, out, stderr)
		}
	}
}

// TestWorkerEnvGracePublishAndFetch: publish commits the record to
// refs/heads/rbe/worker-env-grace, one commit per record on top of the last
// (the branch is the history of windows), only while main still pins the
// record's pin (a later re-pin's job, or a re-run of an older one, must not
// put an older record back), not again when the branch already has it, and
// never a malformed one; a refused push, or a read GitHub fails, is
// retried. fetch prints the
// branch's record, nothing (and success) when there is no branch, and fails
// when the repository cannot be reached.
func TestWorkerEnvGracePublishAndFetch(t *testing.T) {
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	m2 := graceManifest("1.26.11", "2.1.8", "2.39")
	g.commit("c0", m0, "", "1.26.9")
	g.commit("c1", m1, "", "1.26.10")
	g.pushMain()
	first := graceRecordText(openRecord(sha256Pin(m1), g.sha["c1"], sha256Pin(m0), "1.26.9", rbeGraceNow)...)
	second := graceRecordText("version", "1", "pin", sha256Pin(m1), "sha", g.sha["c1"], "previous", "none", "reason", "an image change")
	notMains := graceRecordText(openRecord(sha256Pin("a host main never pinned\n"), strings.Repeat("d", 40), sha256Pin(m1), "1.26.10", rbeGraceNow+600)...)

	// sleep, stubbed: publish pauses between attempts.
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "sleep"), notifyBeadsSleepStub)
	sleeps := filepath.Join(t.TempDir(), "sleeps")
	stubbed := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "SLEEP_LOG=" + sleeps}

	fetch := func(env ...string) (string, error) {
		t.Helper()
		out, stderr, err := g.run(g.dir, env, "fetch")
		if err != nil {
			t.Logf("fetch stderr:\n%s", stderr)
		}
		return out, err
	}
	if out, err := fetch(); err != nil || out != "" {
		t.Errorf("fetch with no branch: %v, %q; want success and nothing", err, out)
	}
	if out, err := fetch("WORKER_ENV_GRACE_REMOTE=file:///nonexistent/github.git"); err == nil {
		t.Errorf("fetch from an unreachable repository: success %q, want a failure", out)
	}

	publish := func(text string) (string, error) {
		t.Helper()
		rec := filepath.Join(t.TempDir(), "record.txt")
		writeTestFile(t, rec, text)
		out, stderr, err := g.run(g.dir, append([]string{"GH_TOKEN=ghs_never-sent-to-file-urls"}, stubbed...), "publish", rec)
		return out + stderr, err
	}
	tip := func() string { return g.git(g.github, "rev-parse", "refs/heads/"+rbeGraceBranch) }

	if out, err := publish(first); err != nil {
		t.Fatalf("publish the first record: %v\n%s", err, out)
	}
	tip1 := tip()
	if got := g.git(g.github, "show", tip1+":"+rbeGraceFile); got+"\n" != first {
		t.Errorf("published record:\n%s\nwant\n%s", got, first)
	}
	if files := g.git(g.github, "ls-tree", "--name-only", tip1); files != rbeGraceFile {
		t.Errorf("the record commit holds %q, want %s alone", files, rbeGraceFile)
	}
	if out, err := fetch(); err != nil || out != first {
		t.Errorf("fetch: %v\n%s\nwant\n%s", err, out, first)
	}

	// The same record again (a re-run): nothing new.
	if out, err := publish(first); err != nil || tip() != tip1 || !strings.Contains(out, "already has this record") {
		t.Errorf("republishing the same record: %v, tip %s (was %s)\n%s", err, tip(), tip1, out)
	}

	if out, err := publish(second); err != nil {
		t.Fatalf("publish the second record: %v\n%s", err, out)
	}
	tip2 := tip()
	if parent := g.git(g.github, "rev-parse", tip2+"^"); parent != tip1 {
		t.Errorf("second record's parent %s, want the first record's commit %s", parent, tip1)
	}
	if out, err := fetch(); err != nil || out != second {
		t.Errorf("fetch after the second: %v\n%s\nwant\n%s", err, out, second)
	}
	if subject := g.git(g.github, "log", "-1", "--format=%s", tip2); !strings.Contains(subject, "no grace window") {
		t.Errorf("commit subject %q does not say what the record is", subject)
	}

	// A record for a pin main no longer has (a newer push re-pinned): main
	// moves on to c2, so first (for c1's pin) is not published again, and
	// neither is a record for a pin main never had.
	g.commit("c2", m2, "", "1.26.11")
	g.pushMain()
	for name, text := range map[string]string{"superseded": first, "not main's": notMains} {
		if out, err := publish(text); err != nil || tip() != tip2 || !strings.Contains(out, "Not publishing") {
			t.Errorf("%s record: %v, tip %s (was %s); want success, no commit and why\n%s", name, err, tip(), tip2, out)
		}
	}

	if out, err := publish("version 1\npin nope\n"); err == nil {
		t.Errorf("publish of a malformed record succeeded:\n%s", out)
	}
	if tip() != tip2 {
		t.Errorf("a refused record moved the branch to %s", tip())
	}

	// A push the remote refuses once is retried (after a pause).
	hook := filepath.Join(g.github, "hooks", "pre-receive")
	writeExecutable(t, hook, "#!/bin/sh\n[ -e \"$0.refused\" ] && exit 0\ntouch \"$0.refused\"\necho refused once >&2\nexit 1\n")
	third := graceRecordText(openRecord(sha256Pin(m2), g.sha["c2"], sha256Pin(m1), "1.26.10", rbeGraceNow+900)...)
	if out, err := publish(third); err != nil || g.git(g.github, "show", tip()+":"+rbeGraceFile)+"\n" != third {
		t.Errorf("publish after one refused push: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(sleeps); strings.TrimSpace(string(b)) != "5" {
		t.Errorf("pauses between attempts %q, want one of 5 s", b)
	}

	// A read GitHub fails once (the fetch of main, a 502) is retried as a
	// refused push is, after a pause: one hiccup does not cost the window.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	failing := t.TempDir()
	writeExecutable(t, filepath.Join(failing, "git"), graceFailingGit)
	once := filepath.Join(t.TempDir(), "failed-once")
	hiccupSleeps := filepath.Join(t.TempDir(), "sleeps")
	fourth := graceRecordText("version", "1", "pin", sha256Pin(m2), "sha", g.sha["c2"], "previous", "none", "reason", "an image change, published after a hiccup")
	rec := filepath.Join(t.TempDir(), "record.txt")
	writeTestFile(t, rec, fourth)
	out, stderr, err := g.run(g.dir, []string{
		"PATH=" + failing + string(os.PathListSeparator) + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SLEEP_LOG=" + hiccupSleeps, "REAL_GIT=" + realGit, "GIT_FAIL_ARG=+refs/heads/main:refs/main", "GIT_FAIL_ONCE=" + once,
	}, "publish", rec)
	if _, injected := os.Stat(once); err != nil || injected != nil || g.git(g.github, "show", tip()+":"+rbeGraceFile)+"\n" != fourth {
		t.Errorf("publish after one failed fetch of main: %v (failure injected: %v)\n%s%s", err, injected == nil, out, stderr)
	}
	if b, _ := os.ReadFile(hiccupSleeps); strings.TrimSpace(string(b)) != "5" {
		t.Errorf("pauses after a failed read %q, want one of 5 s", b)
	}
}

// TestWorkerEnvGraceGitHubAuth: publish hands GH_TOKEN to git as an
// http.extraheader for github.com, through git's environment config alone
// (never a URL, a command line or a file), and only for a github.com URL.
func TestWorkerEnvGraceGitHubAuth(t *testing.T) {
	g := newGraceRepo(t)
	show := `. "$1"; github_auth "$2"; echo "count=${GIT_CONFIG_COUNT:-} key=${GIT_CONFIG_KEY_0:-} value=${GIT_CONFIG_VALUE_0:-}"`
	run := func(url, token string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c", show, "auth", filepath.Join(g.dir, rbeWorkerEnvGrace), url)
		cmd.Env = append(append([]string{}, g.env...), "GH_TOKEN="+token)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("github_auth %s: %v\n%s", url, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	want := "count=1 key=http.https://github.com/.extraheader value=AUTHORIZATION: basic eC1hY2Nlc3MtdG9rZW46Z2hzX3NlY3JldA=="
	if got := run("https://github.com/gastownhall/gascity.git", "ghs_secret"); got != want {
		t.Errorf("github.com with a token: %q, want %q (base64 of x-access-token:ghs_secret)", got, want)
	}
	for _, tc := range [][2]string{{"https://github.com/gastownhall/gascity.git", ""}, {"file:///tmp/github.git", "ghs_secret"}, {"https://evil.example/github.com/x.git", "ghs_secret"}} {
		if got := run(tc[0], tc[1]); got != "count= key= value=" {
			t.Errorf("%s with token %q: %q, want no header", tc[0], tc[1], got)
		}
	}
}

// TestWorkerEnvGraceRoundTrip: the record a Go re-pin publishes, fetched by
// a legacy worker on the re-pinned main, rebuilds exactly the previous
// commit's host files: its manifest byte for byte, its pin and its go.mod's
// Go.
func TestWorkerEnvGraceRoundTrip(t *testing.T) {
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	g.commit("c0", m0, "", "1.26.9")
	g.commit("c1", m1, "", "1.26.10")
	g.pushMain()
	rec := filepath.Join(t.TempDir(), "record.txt")
	out, stderr, err := g.run(g.dir, nil, "record", g.sha["c0"], g.sha["c1"], strconv.Itoa(rbeGraceNow))
	if err != nil {
		t.Fatalf("record: %v\n%s", err, stderr)
	}
	writeTestFile(t, rec, out)
	if _, stderr, err := g.run(g.dir, nil, "publish", rec); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	fetched, stderr, err := g.run(g.dir, nil, "fetch")
	if err != nil || fetched != out {
		t.Fatalf("fetch: %v %q\n%s", err, fetched, stderr)
	}
	writeTestFile(t, rec, fetched)
	root := filepath.Join(t.TempDir(), "previous")
	output := filepath.Join(t.TempDir(), "output")
	if _, stderr, err := g.run(g.dir, []string{"GITHUB_OUTPUT=" + output}, "host", rec, root, strconv.Itoa(rbeGraceNow+10)); err != nil {
		t.Fatalf("host: %v\n%s", err, stderr)
	}
	for _, f := range []string{rbeWorkerEnvManifest, rbeWorkerPlatformBuild} {
		want := g.git(g.dir, "show", g.sha["c0"]+":"+f)
		if f == rbeWorkerPlatformBuild {
			want = regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(want)
		}
		if got := readFile(t, root, f); !strings.Contains(got, want) {
			t.Errorf("%s:\n%s\nwant the previous commit's\n%s", f, got, want)
		}
	}
}

// TestRBEWorkerLegacyWorkflow pins rbe-worker-legacy.yml: the pool
// workflow's worker, serving the previous pin. Dispatched by the scaler
// alone (workflow_dispatch, main of gastownhall/gascity only, the pool's
// inputs); the worker VM gets no token beyond contents: read; the pinned
// rbe-worker (never another revision); the grace step before any
// measurement; a measurement that must pass (a host that does not measure
// the previous pin never registers); no drift jobs (a previous-pin host is
// not the pinned host's drift, and a drift issue would cap the pools); and
// a worker step whose environment is the pool worker's, but for the product
// root, the max age and the worker name.
func TestRBEWorkerLegacyWorkflow(t *testing.T) {
	const defaultBranch = "github.repository == 'gastownhall/gascity' && github.ref == format('refs/heads/{0}', github.event.repository.default_branch)"
	wf := parseWorkflow(t, rbeWorkerLegacyWorkflow)
	pool := parseWorkflow(t, rbeWorkerWorkflow)
	checkPinnedActions(t, rbeWorkerLegacyWorkflow, wf)
	if got := rbeSortedKeys(wf.On); strings.Join(got, ",") != "workflow_dispatch" {
		t.Errorf("on %v, want workflow_dispatch alone (the scaler dispatches it)", got)
	}
	// The scaler dispatches both workflows alike: the same inputs, types,
	// defaults and choices (the descriptions may differ).
	dispatchInputs := func(on map[string]any) map[string]string {
		d, _ := on["workflow_dispatch"].(map[string]any)
		in, _ := d["inputs"].(map[string]any)
		out := map[string]string{}
		for name, v := range in {
			m, _ := v.(map[string]any)
			options, _ := m["options"].([]any)
			out[name] = fmt.Sprintf("type=%v default=%v options=%v", m["type"], m["default"], options)
		}
		return out
	}
	if got, want := dispatchInputs(wf.On), dispatchInputs(pool.On); len(got) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("workflow_dispatch inputs %v, want the pool workflow's %v", got, want)
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Errorf("permissions %v, want contents: read alone", wf.Permissions)
	}
	if got := rbeSortedKeys(wf.Jobs); strings.Join(got, ",") != "worker" {
		t.Fatalf("jobs %v, want worker alone (no drift jobs)", got)
	}
	worker := wf.Jobs["worker"]
	poolWorker := pool.Jobs["worker"]
	if worker.Name != rbeGraceLegacyJob || worker.If != defaultBranch || worker.Needs != nil || len(worker.Permissions) != 0 {
		t.Errorf("worker job name %q if %q needs %v permissions %v; want %q, %q, nothing, nothing", worker.Name, worker.If, worker.Needs, worker.Permissions, rbeGraceLegacyJob, defaultBranch)
	}
	if worker.RunsOn != poolWorker.RunsOn {
		t.Errorf("worker runs-on %q, want the pool worker's %q", worker.RunsOn, poolWorker.RunsOn)
	}

	var ids []string
	for _, s := range worker.Steps {
		ids = append(ids, s.ID)
	}
	if want := []string{"", "rbe-worker", "grace", "measure", "worker"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("worker step ids %v, want %v (checkout, the pinned rbe-worker, the grace record, measure, serve)", ids, want)
	}
	checkout, fetch, grace, measure, serve := worker.Steps[0], worker.Steps[1], worker.Steps[2], worker.Steps[3], worker.Steps[4]
	if !strings.HasPrefix(checkout.Uses, "actions/checkout@") || checkout.With["persist-credentials"] != false {
		t.Errorf("first step %q with %v, want actions/checkout with persist-credentials: false (actions run on this VM)", checkout.Uses, checkout.With)
	}
	if fetch.Uses != "./"+rbeWorkerActionDir || len(fetch.With) != 0 || len(fetch.Env) != 0 {
		t.Errorf("fetch step %+v, want the pinned rbe-worker with no input or env", fetch)
	}
	if !strings.Contains(grace.Run, rbeWorkerEnvGrace+` fetch >"$RUNNER_TEMP/worker-env-grace.txt"`) ||
		!strings.Contains(grace.Run, rbeWorkerEnvGrace+` host "$RUNNER_TEMP/worker-env-grace.txt" "$RUNNER_TEMP/worker-env-previous"`) ||
		!reflect.DeepEqual(grace.Env, map[string]string{"IDLE_MINUTES": "${{ inputs.idle_minutes }}", "MAX_MINUTES": "${{ inputs.max_minutes }}"}) ||
		grace.If != "" || grace.ContinueOnError != "" {
		t.Errorf("grace step %+v, want fetch then host with the dispatch's idle and max minutes, unconditional, failing the job", grace)
	}
	const served = "steps.grace.outputs.serve == 'true'"
	wantMeasure := map[string]string{
		"WORKER_MODE":         "measure",
		"RBE_PRODUCT_ROOT":    "${{ steps.grace.outputs.root }}",
		"RBE_WORKER_DIR":      "${{ steps.rbe-worker.outputs.dir }}",
		"RBE_WORKER_REVISION": "${{ steps.rbe-worker.outputs.sha }}",
	}
	if !isMeasureStep(measure) || measure.If != served || measure.ContinueOnError != "" || !reflect.DeepEqual(measure.Env, wantMeasure) {
		t.Errorf("measure step if %q continue-on-error %q env %v; want if %s, failing the job on drift, env %v", measure.If, measure.ContinueOnError, measure.Env, served, wantMeasure)
	}

	_, poolServe := findStep(poolWorker, func(s ghStep) bool { return s.Env["WORKER_MODE"] == "pool" })
	if poolServe == nil {
		t.Fatalf("%s has no pool worker step", rbeWorkerWorkflow)
	}
	want := map[string]string{}
	for k, v := range poolServe.Env {
		want[k] = v
	}
	want["RBE_PRODUCT_ROOT"] = "${{ steps.grace.outputs.root }}"
	// The checked minutes: host refuses anything but whole minutes, which
	// reach the worker's shell arithmetic.
	want["POOL_IDLE_MINUTES"] = "${{ steps.grace.outputs.idle-minutes }}"
	want["POOL_MAX_MINUTES"] = "${{ steps.grace.outputs.max-minutes }}"
	want["WORKER_NAME"] = "gha-legacy-${{ github.event.repository.name }}-${{ github.run_id }}-${{ github.run_attempt }}"
	if strings.TrimSpace(serve.Run) != `"$RBE_WORKER_DIR/blacksmith-worker.sh"` || serve.If != served || serve.ContinueOnError != "" || !reflect.DeepEqual(serve.Env, want) {
		t.Errorf("worker step run %q if %q continue-on-error %q env\n%v\nwant if %s, the pool worker's env but RBE_PRODUCT_ROOT, POOL_IDLE_MINUTES, POOL_MAX_MINUTES and WORKER_NAME:\n%v",
			serve.Run, serve.If, serve.ContinueOnError, serve.Env, served, want)
	}
	if strings.Contains(readFile(t, repoRoot(t), rbeWorkerLegacyWorkflow), "upload-artifact") {
		t.Errorf("%s uploads an artifact; a previous-pin host reports no drift", rbeWorkerLegacyWorkflow)
	}
}

// TestRBEWorkerLegacyGraceStep runs the legacy workflow's grace step on a
// checkout of a re-pinned main, with the record its push published: it
// writes the previous host's files and the outputs the next steps read.
func TestRBEWorkerLegacyGraceStep(t *testing.T) {
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	g.commit("c0", m0, "", "1.26.9")
	g.commit("c1", m1, "", "1.26.10")
	g.pushMain()
	now := int(time.Now().Unix())
	rec := filepath.Join(t.TempDir(), "record.txt")
	out, stderr, err := g.run(g.dir, nil, "record", g.sha["c0"], g.sha["c1"], strconv.Itoa(now))
	if err != nil {
		t.Fatalf("record: %v\n%s", err, stderr)
	}
	writeTestFile(t, rec, out)
	if _, stderr, err := g.run(g.dir, nil, "publish", rec); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}

	wf := parseWorkflow(t, rbeWorkerLegacyWorkflow)
	_, step := findStep(wf.Jobs["worker"], func(s ghStep) bool { return s.ID == "grace" })
	if step == nil {
		t.Fatalf("no grace step")
	}
	runner := map[string]string{}
	for _, kv := range g.env {
		k, v, _ := strings.Cut(kv, "=")
		runner[k] = v
	}
	temp := t.TempDir()
	runner["RUNNER_TEMP"] = temp
	runner["GITHUB_OUTPUT"] = filepath.Join(temp, "output")
	inputs := map[string]string{"inputs.max_minutes": "300", "inputs.idle_minutes": "8"}
	for k, v := range step.Env {
		runner[k] = interpolateGH(t, v, inputs)
	}
	got, err := runWorkflowStepScript(t, g.dir, step.Run, runner)
	if err != nil {
		t.Fatalf("grace step: %v\n%s", err, got)
	}
	if serve, _ := readStepOutput(t, runner["GITHUB_OUTPUT"], "serve"); serve != "true" {
		t.Errorf("serve %q, want true\n%s", serve, got)
	}
	root, _ := readStepOutput(t, runner["GITHUB_OUTPUT"], "root")
	if root != filepath.Join(temp, "worker-env-previous") || readFile(t, root, rbeWorkerEnvManifest) != m0 {
		t.Errorf("root %q does not hold the previous manifest\n%s", root, got)
	}
	if idle, _ := readStepOutput(t, runner["GITHUB_OUTPUT"], "idle-minutes"); idle != "8" {
		t.Errorf("idle-minutes %q, want the dispatch's 8", idle)
	}

	// No grace branch at all: the step fails (the scaler's breaker counts it).
	empty := t.TempDir()
	g.git(empty, "init", "-q", "--bare", empty)
	runner["WORKER_ENV_GRACE_REMOTE"] = "file://" + empty
	runner["GITHUB_OUTPUT"] = filepath.Join(temp, "output-none")
	if got, err := runWorkflowStepScript(t, g.dir, step.Run, runner); exitCode(err) != 3 {
		t.Errorf("grace step with no record: exit %d, want 3\n%s", exitCode(err), got)
	}
	if serve, _ := readStepOutput(t, runner["GITHUB_OUTPUT"], "serve"); serve != "false" {
		t.Errorf("serve %q with no record, want false", serve)
	}
}

// TestWorkerEnvNotifyBeadsGraceJob pins worker-env-notify-beads.yml's grace
// job: on the push that moves main's pin (after repin, like dispatch), or
// one whose main before the push repin could not fetch, it writes the
// record for main before the push (repin's before output, never the parent
// repin compared with instead) and main now, and publishes it with
// GITHUB_TOKEN's contents: write, its only grant. It reads no secret and
// names no environment; the token reaches the publish step alone, after the
// checkout (which keeps no credential); one record at a time, never
// canceling one that is publishing.
func TestWorkerEnvNotifyBeadsGraceJob(t *testing.T) {
	wf := loadNotifyBeads(t, notifyBeadsWorkflow)
	grace, ok := wf.Jobs["grace"]
	if !ok {
		t.Fatalf("%s has no grace job", notifyBeadsWorkflow)
	}
	if grace.Needs != "repin" || grace.If != "needs.repin.outputs.moved == 'true' || needs.repin.outputs.unfetched == 'true'" ||
		grace.RunsOn != "ubuntu-latest" || grace.Environment != nil {
		t.Errorf("grace needs %v if %q runs-on %q environment %v; want repin, a moved pin or an unfetched before, ubuntu-latest, none", grace.Needs, grace.If, grace.RunsOn, grace.Environment)
	}
	if want := map[string]string{"contents": "write"}; !reflect.DeepEqual(grace.Permissions, want) {
		t.Errorf("grace permissions %v, want %v alone", grace.Permissions, want)
	}
	if grace.Concurrency["group"] != "rbe-worker-env-grace" || grace.Concurrency["cancel-in-progress"] != false {
		t.Errorf("grace concurrency %v, want group rbe-worker-env-grace without cancel-in-progress", grace.Concurrency)
	}
	if repin := wf.Jobs["repin"]; repin.Outputs["before"] != "${{ steps.pin.outputs.before }}" {
		t.Errorf("repin outputs %v, want before from its pin step", repin.Outputs)
	}
	var ids []string
	for _, s := range grace.Steps {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"", "record", "publish"}) {
		t.Fatalf("grace steps %v, want a checkout, record and publish", ids)
	}
	checkout, record, publish := grace.Steps[0], grace.Steps[1], grace.Steps[2]
	if !strings.HasPrefix(checkout.Uses, "actions/checkout@") || !reflect.DeepEqual(checkout.With, map[string]any{"persist-credentials": false}) {
		t.Errorf("grace checkout %q with %v, want actions/checkout keeping no credential", checkout.Uses, checkout.With)
	}
	if !reflect.DeepEqual(record.Env, map[string]string{"BEFORE": "${{ needs.repin.outputs.before }}"}) || record.Uses != "" {
		t.Errorf("record step env %v uses %q, want BEFORE from repin alone, inline", record.Env, record.Uses)
	}
	if !reflect.DeepEqual(publish.Env, map[string]string{"GH_TOKEN": "${{ github.token }}"}) || publish.Uses != "" ||
		strings.TrimSpace(publish.Run) != rbeWorkerEnvGrace+` publish "$RUNNER_TEMP/worker-env-grace.txt"` {
		t.Errorf("publish step env %v run %q, want GH_TOKEN alone and the publish command", publish.Env, publish.Run)
	}
	if publish.If != "steps.record.outputs.publish == 'true'" {
		t.Errorf("publish step if %q, want it to run only when the record step wrote a record", publish.If)
	}

	// Run the job as GitHub does on a push from before to head: repin's step
	// in a checkout of head and its parent, grace's if on its outputs, the
	// record step in a checkout of head alone (it fetches main before the
	// push itself), then the publish step if the record step says so. Each
	// case first makes head the "GitHub" main: publish checks that main
	// still pins the record's pin.
	//
	// The record is for main before the push, never the parent repin
	// compares with when it cannot fetch that (hiccup: GitHub fails repin's
	// fetches of before alone; an unknown before fails every fetch). For a
	// merge-queue group of an image change (c2) and a Go re-pin (c3), the
	// parent would give c3 a window for c2's pin, which beads never had. For
	// a group whose last commit (c1-docs) follows its Go re-pin (c1), the
	// parent pins c1-docs' host, so repin finds no move, yet main before the
	// group (c0) gives a window. When main before the push cannot be read,
	// the job fails and publishes nothing (re-run it); when it pins head's
	// host, the job publishes nothing and passes.
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	g.commit("c0", m0, "", "1.26.9")
	g.commit("c1", m1, "", "1.26.10")                                        // Go re-pin
	g.commit("c1-docs", m1, "", "1.26.10")                                   // the pin did not move
	g.commit("c2", graceManifest("1.26.10", "2.1.8", "2.41"), "", "1.26.10") // image: libc6
	g.commit("c3", graceManifest("1.26.11", "2.1.8", "2.41"), "", "1.26.11") // Go re-pin
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	repin := notifyBeadsStep(t, wf, "repin", "pin")
	const zero = "0000000000000000000000000000000000000000"
	unknown := strings.Repeat("e", 40)
	for _, tc := range []struct {
		name, before, head string
		hiccup             bool
		runs               bool   // the grace job runs
		previous           string // the record it publishes ("": none)
		fails              bool   // it fails, publishing nothing
	}{
		{name: "Go re-pin", before: g.sha["c0"], head: g.sha["c1"], runs: true, previous: sha256Pin(m0)},
		{name: "image change", before: g.sha["c1-docs"], head: g.sha["c2"], runs: true, previous: "none"},
		{name: "no main before the push", before: zero, head: g.sha["c1"], runs: true, previous: "none"},
		{name: "pin did not move", before: g.sha["c1"], head: g.sha["c1-docs"]},
		{name: "image change then Go re-pin", before: g.sha["c1-docs"], head: g.sha["c3"], runs: true, previous: "none"},
		{name: "image change then Go re-pin, repin cannot fetch before", before: g.sha["c1-docs"], head: g.sha["c3"], hiccup: true, runs: true, previous: "none"},
		{name: "image change then Go re-pin, before not fetchable", before: unknown, head: g.sha["c3"], runs: true, fails: true},
		{name: "Go re-pin then docs", before: g.sha["c0"], head: g.sha["c1-docs"], runs: true, previous: sha256Pin(m0)},
		{name: "Go re-pin then docs, repin cannot fetch before", before: g.sha["c0"], head: g.sha["c1-docs"], hiccup: true, runs: true, previous: sha256Pin(m0)},
		{name: "Go re-pin then docs, before not fetchable", before: unknown, head: g.sha["c1-docs"], runs: true, fails: true},
		{name: "pin did not move, repin cannot fetch before", before: g.sha["c1"], head: g.sha["c1-docs"], hiccup: true, runs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.git(g.dir, "push", "-q", "--force", g.github, tc.head+":refs/heads/main")
			branch := func() string {
				return g.git(g.github, "for-each-ref", "--format=%(objectname)", "refs/heads/"+rbeGraceBranch)
			}
			tipBefore := branch()
			dir := t.TempDir()
			runner := map[string]string{}
			for _, kv := range g.env {
				k, v, _ := strings.Cut(kv, "=")
				runner[k] = v
			}
			bin := t.TempDir()
			writeExecutable(t, filepath.Join(bin, "sleep"), notifyBeadsSleepStub)
			runner["PATH"] = bin + string(os.PathListSeparator) + runner["PATH"]
			runner["SLEEP_LOG"] = filepath.Join(dir, "sleeps")
			runner["RUNNER_TEMP"] = dir
			runner["GITHUB_STEP_SUMMARY"] = filepath.Join(dir, "summary")

			repinCo := filepath.Join(dir, "repin")
			runNotifyBeadsFixture(t, dir, notifyBeadsCheckout, g.dir, repinCo, tc.head)
			repinRunner := map[string]string{"GITHUB_OUTPUT": filepath.Join(dir, "repin-output")}
			for k, v := range runner {
				repinRunner[k] = v
			}
			if tc.hiccup {
				hiccup := t.TempDir()
				writeExecutable(t, filepath.Join(hiccup, "git"), graceFailingGit)
				repinRunner["PATH"] = hiccup + string(os.PathListSeparator) + runner["PATH"]
				repinRunner["REAL_GIT"] = realGit
				repinRunner["GIT_FAIL_ARG"] = tc.before
			}
			out, err := runWorkflowStepScript(t, repinCo, repin.Run, notifyBeadsEnv(t, wf, repin, map[string]string{"github.event.before": tc.before}, repinRunner))
			if err != nil {
				t.Fatalf("repin step: %v\n%s", err, out)
			}
			ctx := map[string]string{"github.token": "ghs_never-sent-to-file-urls"}
			for _, k := range []string{"moved", "pin", "sha", "before", "unfetched"} {
				ctx["needs.repin.outputs."+k], _ = readStepOutput(t, repinRunner["GITHUB_OUTPUT"], k)
			}
			if runs := evalGHIf(t, grace.If, ctx); runs != tc.runs {
				t.Fatalf("grace runs %v after repin's outputs %v, want %v\n%s", runs, ctx, tc.runs, out)
			}
			if !tc.runs {
				return
			}

			co := filepath.Join(dir, "checkout")
			runNotifyBeadsFixture(t, dir, graceCheckoutDepth1, g.dir, co, tc.head)
			runner["GITHUB_OUTPUT"] = filepath.Join(dir, "record-output")
			out, err = runWorkflowStepScript(t, co, record.Run, notifyBeadsEnv(t, wf, record, ctx, runner))
			if tc.fails {
				if tipAfter := branch(); err == nil || !strings.Contains(out, "nothing published") || tipAfter != tipBefore {
					t.Errorf("record step: %v, branch %s (was %s); want a failure and nothing published\n%s", err, tipAfter, tipBefore, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("record step: %v\n%s", err, out)
			}
			ctx["steps.record.outputs.publish"], _ = readStepOutput(t, runner["GITHUB_OUTPUT"], "publish")
			if publishes := evalGHIf(t, publish.If, ctx); publishes != (tc.previous != "") {
				t.Fatalf("publish step runs %v (record step's publish %q), want %v\n%s", publishes, ctx["steps.record.outputs.publish"], tc.previous != "", out)
			}
			if tc.previous == "" {
				if tipAfter := branch(); tipAfter != tipBefore || !strings.Contains(out, "nothing to publish") {
					t.Errorf("record step for a pin main before the push already had: branch %s (was %s); want nothing published, and why\n%s", tipAfter, tipBefore, out)
				}
				return
			}
			if out, err := runWorkflowStepScript(t, co, publish.Run, notifyBeadsEnv(t, wf, publish, ctx, runner)); err != nil {
				t.Fatalf("publish step: %v\n%s", err, out)
			}
			published := g.git(g.github, "show", "refs/heads/"+rbeGraceBranch+":"+rbeGraceFile) + "\n"
			got := parseGraceRecord(t, published)
			if got.fields["previous"] != tc.previous || got.fields["sha"] != tc.head || published != readFile(t, dir, "worker-env-grace.txt") {
				t.Errorf("published record %v, want this run's record, previous %s for %s", got.fields, tc.previous, tc.head)
			}
			if summary := readFile(t, dir, "summary"); !strings.Contains(summary, "previous "+tc.previous) {
				t.Errorf("step summary does not show the record:\n%s", summary)
			}
		})
	}
}

// graceFailingGit stands in for git on PATH and fails as GitHub does when it
// hiccups: a run with the argument $GIT_FAIL_ARG fails as a 502 would, every
// time, or with GIT_FAIL_ONCE only the first time (it creates that file);
// every other run is $REAL_GIT's.
const graceFailingGit = `#!/bin/sh
for arg in "$@"; do
	[ "$arg" = "$GIT_FAIL_ARG" ] || continue
	if [ -n "${GIT_FAIL_ONCE:-}" ]; then
		[ ! -e "$GIT_FAIL_ONCE" ] || break
		: >"$GIT_FAIL_ONCE"
	fi
	echo "fatal: unable to access 'https://github.com/gastownhall/gascity.git/': The requested URL returned error: 502" >&2
	exit 128
done
exec "$REAL_GIT" "$@"
`

// graceCheckoutDepth1 checks HEAD out of ORIGIN into DIR as the grace job's
// actions/checkout does: that commit alone (fetch-depth 1), from a remote
// named origin.
const graceCheckoutDepth1 = `#!/usr/bin/env bash
set -euo pipefail
origin=$1 dir=$2 head=$3
git init -q "$dir"
git -C "$dir" remote add origin "$origin"
git -C "$dir" fetch -q --no-tags --depth=1 origin "$head"
git -C "$dir" checkout -q --detach "$head"
`
