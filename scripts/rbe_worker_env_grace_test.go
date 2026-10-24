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
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = g.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// commit records name: the worker host's files (manifest, its pin unless
// pin is given, go.mod's Go) on top of main.
func (g *graceRepo) commit(name, manifest, pin, goVersion string) {
	g.t.Helper()
	if pin == "" {
		pin = sha256Pin(manifest)
	}
	writeTestFile(g.t, filepath.Join(g.dir, rbeWorkerEnvManifest), manifest)
	writeTestFile(g.t, filepath.Join(g.dir, rbeWorkerPlatformBuild), buildWithPin(pin))
	writeTestFile(g.t, filepath.Join(g.dir, "go.mod"), "module github.com/gastownhall/gascity\n\ngo "+goVersion+"\n")
	writeTestFile(g.t, filepath.Join(g.dir, "commit.txt"), name+"\n")
	g.git(g.dir, "add", "-A")
	g.git(g.dir, "commit", "-q", "-m", name)
	g.sha[name] = g.git(g.dir, "rev-parse", "HEAD")
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
// gets "previous none" with the reason. A push that did not move the pin is
// a usage error: worker-env-notify-beads.yml calls it only on a move.
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

	if _, out, err := record("c5", "same"); exitCode(err) != 2 || !strings.Contains(out, "did not move") {
		t.Errorf("record for a push that did not move the pin: exit %d, want 2 (usage)\n%s", exitCode(err), out)
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
// and succeeds; a record for another main, a forged previous pin or a
// malformed record fails. max-minutes ends the worker by serve-until.
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
		for _, k := range []string{"serve", "root", "previous", "go", "max-minutes"} {
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
	wantOut := map[string]string{"serve": "true", "root": root, "previous": p0, "go": "1.26.9", "max-minutes": "179"}
	if !reflect.DeepEqual(outputs, wantOut) {
		t.Errorf("outputs %v, want %v", outputs, wantOut)
	}

	// max-minutes: the dispatch's, unless serve-until comes first.
	for _, tc := range []struct {
		now     int
		max     string
		want    string
		refused bool
	}{
		{now: rbeGraceNow, max: "300", want: "180"},
		{now: rbeGraceNow, max: "60", want: "60"},
		{now: rbeGraceNow + rbeGraceAcceptS + rbeGraceDrainS - 30, max: "300", want: "1"},
		{now: rbeGraceNow, max: "", want: "180"}, // MAX_MINUTES unset: 300
		{now: rbeGraceNow, max: "0", refused: true},
		{now: rbeGraceNow, max: "5m", refused: true},
	} {
		env := []string{}
		if tc.max != "" {
			env = append(env, "MAX_MINUTES="+tc.max)
		}
		_, outputs, out, err := host(open, tc.now, env...)
		if tc.refused {
			if err == nil || outputs["serve"] == "true" {
				t.Errorf("MAX_MINUTES=%q: %v, outputs %v; want a refusal\n%s", tc.max, err, outputs, out)
			}
			continue
		}
		if err != nil || outputs["max-minutes"] != tc.want {
			t.Errorf("now %d MAX_MINUTES=%q: %v, max-minutes %q, want %s\n%s", tc.now, tc.max, err, outputs["max-minutes"], tc.want, out)
		}
	}

	// No window: serve=false, success, nothing written.
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
			if err != nil || !reflect.DeepEqual(outputs, map[string]string{"serve": "false"}) {
				t.Errorf("%v, outputs %v; want success and serve=false alone\n%s", err, outputs, out)
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
		"duplicate key": graceRecordText(open...) + "go 1.26.9\n",
		"bare key":      graceRecordText(open...) + "go\n",
		"CRLF":          strings.ReplaceAll(graceRecordText(open...), "\n", "\r\n"),
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
// (the branch is the history of windows), and refuses a malformed record;
// fetch prints the branch's record, nothing (and success) when there is no
// branch, and fails when the repository cannot be reached.
func TestWorkerEnvGracePublishAndFetch(t *testing.T) {
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	m1 := graceManifest("1.26.10", "2.1.8", "2.39")
	first := graceRecordText(openRecord(sha256Pin(m1), strings.Repeat("a", 40), sha256Pin(m0), "1.26.9", rbeGraceNow)...)
	second := graceRecordText("version", "1", "pin", sha256Pin("y\n"), "sha", strings.Repeat("b", 40), "previous", "none", "reason", "an image change")

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
		out, stderr, err := g.run(g.dir, []string{"GH_TOKEN=ghs_never-sent-to-file-urls"}, "publish", rec)
		return out + stderr, err
	}
	if out, err := publish(first); err != nil {
		t.Fatalf("publish the first record: %v\n%s", err, out)
	}
	tip1 := g.git(g.github, "rev-parse", "refs/heads/"+rbeGraceBranch)
	if got := g.git(g.github, "show", tip1+":"+rbeGraceFile); got+"\n" != first {
		t.Errorf("published record:\n%s\nwant\n%s", got, first)
	}
	if files := g.git(g.github, "ls-tree", "--name-only", tip1); files != rbeGraceFile {
		t.Errorf("the record commit holds %q, want %s alone", files, rbeGraceFile)
	}
	if out, err := fetch(); err != nil || out != first {
		t.Errorf("fetch: %v\n%s\nwant\n%s", err, out, first)
	}

	if out, err := publish(second); err != nil {
		t.Fatalf("publish the second record: %v\n%s", err, out)
	}
	tip2 := g.git(g.github, "rev-parse", "refs/heads/"+rbeGraceBranch)
	if parent := g.git(g.github, "rev-parse", tip2+"^"); parent != tip1 {
		t.Errorf("second record's parent %s, want the first record's commit %s", parent, tip1)
	}
	if out, err := fetch(); err != nil || out != second {
		t.Errorf("fetch after the second: %v\n%s\nwant\n%s", err, out, second)
	}
	if subject := g.git(g.github, "log", "-1", "--format=%s", tip2); !strings.Contains(subject, "no grace window") {
		t.Errorf("commit subject %q does not say what the record is", subject)
	}

	if out, err := publish("version 1\npin nope\n"); err == nil {
		t.Errorf("publish of a malformed record succeeded:\n%s", out)
	}
	if tip := g.git(g.github, "rev-parse", "refs/heads/"+rbeGraceBranch); tip != tip2 {
		t.Errorf("a refused record moved the branch to %s", tip)
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
		!reflect.DeepEqual(grace.Env, map[string]string{"MAX_MINUTES": "${{ inputs.max_minutes }}"}) || grace.If != "" || grace.ContinueOnError != "" {
		t.Errorf("grace step %+v, want fetch then host with MAX_MINUTES the dispatch's max_minutes, unconditional", grace)
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
	want["POOL_MAX_MINUTES"] = "${{ steps.grace.outputs.max-minutes }}"
	want["WORKER_NAME"] = "gha-legacy-${{ github.event.repository.name }}-${{ github.run_id }}-${{ github.run_attempt }}"
	if strings.TrimSpace(serve.Run) != `"$RBE_WORKER_DIR/blacksmith-worker.sh"` || serve.If != served || serve.ContinueOnError != "" || !reflect.DeepEqual(serve.Env, want) {
		t.Errorf("worker step run %q if %q continue-on-error %q env\n%v\nwant if %s, the pool worker's env but RBE_PRODUCT_ROOT, POOL_MAX_MINUTES and WORKER_NAME:\n%v",
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
	runner["MAX_MINUTES"] = interpolateGH(t, step.Env["MAX_MINUTES"], map[string]string{"inputs.max_minutes": "300"})
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
}

// TestWorkerEnvNotifyBeadsGraceJob pins worker-env-notify-beads.yml's grace
// job: on the push that moves main's pin (after repin, like dispatch), it
// writes the record for main before the push (repin's before output) and
// main now, and publishes it with GITHUB_TOKEN's contents: write, its only
// grant. It reads no secret and names no environment; the token reaches the
// publish step alone, after the checkout (which keeps no credential); one
// record at a time, never canceling one that is publishing.
func TestWorkerEnvNotifyBeadsGraceJob(t *testing.T) {
	wf := loadNotifyBeads(t, notifyBeadsWorkflow)
	grace, ok := wf.Jobs["grace"]
	if !ok {
		t.Fatalf("%s has no grace job", notifyBeadsWorkflow)
	}
	if grace.Needs != "repin" || grace.If != "needs.repin.outputs.moved == 'true'" || grace.RunsOn != "ubuntu-latest" || grace.Environment != nil {
		t.Errorf("grace needs %v if %q runs-on %q environment %v; want repin, the moved pin, ubuntu-latest, none", grace.Needs, grace.If, grace.RunsOn, grace.Environment)
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

	// Run the record and publish steps: a Go re-pin publishes a window, an
	// image change and a push with no main before it publish none.
	g := newGraceRepo(t)
	m0 := graceManifest("1.26.9", "2.1.8", "2.39")
	g.commit("c0", m0, "", "1.26.9")
	g.commit("c1", graceManifest("1.26.10", "2.1.8", "2.39"), "", "1.26.10")
	g.commit("c2", graceManifest("1.26.10", "2.1.8", "2.41"), "", "1.26.10")
	for _, tc := range []struct {
		name, before, head string
		previous           string
	}{
		{name: "Go re-pin", before: g.sha["c0"], head: g.sha["c1"], previous: sha256Pin(m0)},
		{name: "image change", before: g.sha["c1"], head: g.sha["c2"], previous: "none"},
		{name: "no main before the push", before: "", head: g.sha["c1"], previous: "none"},
		{name: "main before the push not fetchable", before: strings.Repeat("e", 40), head: g.sha["c1"], previous: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			co := filepath.Join(dir, "checkout")
			runNotifyBeadsFixture(t, dir, notifyBeadsCheckout, g.dir, co, tc.head)
			runner := map[string]string{}
			for _, kv := range g.env {
				k, v, _ := strings.Cut(kv, "=")
				runner[k] = v
			}
			runner["RUNNER_TEMP"] = dir
			runner["GITHUB_STEP_SUMMARY"] = filepath.Join(dir, "summary")
			ctx := map[string]string{"needs.repin.outputs.before": tc.before, "github.token": "ghs_never-sent-to-file-urls"}
			for _, s := range []ghStep{record, publish} {
				env := notifyBeadsEnv(t, wf, s, ctx, runner)
				if out, err := runWorkflowStepScript(t, co, s.Run, env); err != nil {
					t.Fatalf("step %s: %v\n%s", s.ID, err, out)
				}
			}
			got := parseGraceRecord(t, g.git(g.github, "show", "refs/heads/"+rbeGraceBranch+":"+rbeGraceFile)+"\n")
			if got.fields["previous"] != tc.previous || got.fields["sha"] != tc.head {
				t.Errorf("published record %v, want previous %s for %s", got.fields, tc.previous, tc.head)
			}
			if summary := readFile(t, dir, "summary"); !strings.Contains(summary, "previous "+tc.previous) {
				t.Errorf("step summary does not show the record:\n%s", summary)
			}
		})
	}
}
