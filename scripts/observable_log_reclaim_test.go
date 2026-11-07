package scripts_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reclaimFixture is an isolated world for scripts/observable-log-reclaim: a
// scratch root, a fake /proc, and fake gc, bd and git on PATH. The script never
// sees the host's real bead stores, process table or repositories.
type reclaimFixture struct {
	t       *testing.T
	dir     string
	scratch string
	proc    string
	fakeBin string
	fixDir  string
	cityDir string
	rigDir  string
	bdLog   string
}

const (
	reclaimOld   = 100 * time.Hour // comfortably past the 72 h floor
	reclaimYoung = 10 * time.Hour  // comfortably inside it
)

func newReclaimFixture(t *testing.T) *reclaimFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	f := &reclaimFixture{
		t:       t,
		dir:     dir,
		scratch: filepath.Join(dir, "scratch"),
		proc:    filepath.Join(dir, "proc"),
		fakeBin: filepath.Join(dir, "bin"),
		fixDir:  filepath.Join(dir, "fixtures"),
		cityDir: filepath.Join(dir, "city"),
		rigDir:  filepath.Join(dir, "rig"),
		bdLog:   filepath.Join(dir, "bd.log"),
	}
	for _, d := range []string{f.scratch, f.proc, f.fakeBin, f.fixDir, f.cityDir, f.rigDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// One live process that holds nothing relevant, so an empty /proc is never
	// mistaken for "nothing is open".
	f.addProc("1")
	f.writeFakeTools()
	f.setRigs(true)
	f.setBeads(f.cityDir, `[{"id":"gm-1","title":"unrelated"}]`)
	f.setBeads(f.rigDir, `[{"id":"ga-1","title":"unrelated"}]`)
	return f
}

// key turns a store root into the fixture file name the fake tools look up.
func (f *reclaimFixture) key(root string) string {
	return filepath.Join(f.fixDir, strings.ReplaceAll(root, string(os.PathSeparator), "_"))
}

func (f *reclaimFixture) writeFakeTools() {
	f.t.Helper()
	tools := map[string]string{
		"gc": `#!/bin/sh
if [ "$1" = "rig" ] && [ "$2" = "list" ] && [ "$3" = "--json" ]; then
  [ -e "` + f.fixDir + `/gc.fail" ] && { echo "gc: no city" >&2; exit 1; }
  cat "` + f.fixDir + `/rigs.json"
  exit 0
fi
echo "fake gc: unexpected args: $*" >&2
exit 2
`,
		"bd": `#!/bin/sh
printf 'bd %s BEADS_DIR=%s\n' "$*" "${BEADS_DIR-unset}" >> "` + f.bdLog + `"
if [ "$1" != "-C" ]; then echo "fake bd: store not addressed with -C: $*" >&2; exit 2; fi
root=$2
shift 2
if [ "$1 $2 $3 $4 $5 $6" != "list --all --json --limit 0 " ] && [ "$*" != "list --all --json --limit 0" ]; then
  echo "fake bd: unexpected args: $*" >&2; exit 2
fi
key=$(printf '%s' "$root" | tr / _)
[ -e "` + f.fixDir + `/$key.bdfail" ] && { echo "bd: store unreachable" >&2; exit 1; }
cat "` + f.fixDir + `/$key"
`,
		"git": `#!/bin/sh
if [ "$1" != "-C" ] || [ "$3" != "ls-files" ]; then echo "fake git: unexpected args: $*" >&2; exit 2; fi
root=$2
key=$(printf '%s' "$root" | tr / _)
[ -e "` + f.fixDir + `/$key.gitfail" ] && { echo "fatal: not a git repository" >&2; exit 128; }
[ -e "` + f.fixDir + `/$key.tracked" ] && cat "` + f.fixDir + `/$key.tracked"
exit 0
`,
	}
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(f.fakeBin, name), []byte(body), 0o755); err != nil {
			f.t.Fatalf("write fake %s: %v", name, err)
		}
	}
}

// setRigs publishes `gc rig list --json` with the city (HQ) and one rig.
func (f *reclaimFixture) setRigs(includeRig bool) {
	f.t.Helper()
	rigs := []map[string]any{{"name": "city", "path": f.cityDir, "hq": true}}
	if includeRig {
		rigs = append(rigs, map[string]any{"name": "rig", "path": f.rigDir, "hq": false})
	}
	data, err := json.Marshal(map[string]any{"ok": true, "rigs": rigs})
	if err != nil {
		f.t.Fatalf("marshal rigs: %v", err)
	}
	f.write(filepath.Join(f.fixDir, "rigs.json"), string(data))
}

func (f *reclaimFixture) setBeads(root, jsonArray string) {
	f.t.Helper()
	f.write(f.key(root), jsonArray)
}

// track records a tracked file under root (its text is what the citation scan
// reads) the way `git ls-files -z` would print it.
func (f *reclaimFixture) track(root, rel, text string) {
	f.t.Helper()
	f.write(filepath.Join(root, rel), text)
	list := f.key(root) + ".tracked"
	prev, _ := os.ReadFile(list)
	if err := os.WriteFile(list, append(prev, []byte(rel+"\x00")...), 0o644); err != nil {
		f.t.Fatalf("write tracked list: %v", err)
	}
}

func (f *reclaimFixture) mark(root, suffix string) {
	f.t.Helper()
	f.write(f.key(root)+suffix, "")
}

func (f *reclaimFixture) write(path, text string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

// log creates a scratch log of the given age and returns its path.
func (f *reclaimFixture) log(name, content string, age time.Duration) string {
	f.t.Helper()
	path := filepath.Join(f.scratch, name)
	f.write(path, content)
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		f.t.Fatalf("age %s: %v", path, err)
	}
	return path
}

// addProc adds a fake process directory with an fd table and returns it.
func (f *reclaimFixture) addProc(pid string) string {
	f.t.Helper()
	fd := filepath.Join(f.proc, pid, "fd")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		f.t.Fatalf("mkdir fake proc %s: %v", pid, err)
	}
	return fd
}

type reclaimRun struct {
	stdout string
	stderr string
	code   int
	files  map[string]map[string]string // path -> fields of its `file` line
	stores map[string]string            // store root -> bead count
	sum    map[string]string            // fields of the `summary` line
}

var reclaimFieldRE = regexp.MustCompile(`^(\w[\w-]*)=(.*)$`)

// run executes the script. extra is placed after the fixture's own flags.
func (f *reclaimFixture) run(extra ...string) reclaimRun {
	f.t.Helper()
	args := append([]string{"--scratch-root", f.scratch, "--proc-root", f.proc}, extra...)
	cmd := scriptCommand(repoRoot(f.t), "observable-log-reclaim", args...)
	cmd.Dir = f.dir
	cmd.Env = []string{
		"PATH=" + f.fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.dir,
		"TMPDIR=" + f.dir,
		// The ambient shape in an agent session: it pins bd to one rig store.
		"BEADS_DIR=" + filepath.Join(f.dir, "pinned-rig-store"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(interface{ ExitCode() int })
		if !ok {
			f.t.Fatalf("run observable-log-reclaim: %v\nstderr:\n%s", err, stderr.String())
		}
		code = exitErr.ExitCode()
	}
	r := reclaimRun{
		stdout: stdout.String(), stderr: stderr.String(), code: code,
		files:  map[string]map[string]string{},
		stores: map[string]string{},
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		kind, rest, _ := strings.Cut(line, " ")
		switch kind {
		case "file":
			fields := parseReclaimFields(rest)
			r.files[fields["path"]] = fields
		case "store":
			fields := parseReclaimFields(rest)
			r.stores[fields["path"]] = fields["beads"]
		case "summary":
			r.sum = parseReclaimFields(rest)
		}
	}
	return r
}

// parseReclaimFields reads `k=v k=v ... path=<rest of line>`: path is last and
// may contain spaces, every other value is a single token.
func parseReclaimFields(rest string) map[string]string {
	fields := map[string]string{}
	if i := strings.Index(rest, "path="); i >= 0 {
		fields["path"] = rest[i+len("path="):]
		rest = rest[:i]
	}
	for _, tok := range strings.Fields(rest) {
		if m := reclaimFieldRE.FindStringSubmatch(tok); m != nil {
			fields[m[1]] = m[2]
		}
	}
	return fields
}

func (r reclaimRun) verdict(t *testing.T, path string) string {
	t.Helper()
	fields, ok := r.files[path]
	if !ok {
		t.Fatalf("no `file` line for %s\nstdout:\n%s\nstderr:\n%s", path, r.stdout, r.stderr)
	}
	return fields["verdict"]
}

func (r reclaimRun) mustVerdict(t *testing.T, path, want string) {
	t.Helper()
	if got := r.verdict(t, path); got != want {
		t.Errorf("%s: verdict = %q, want %q\nstdout:\n%s\nstderr:\n%s", filepath.Base(path), got, want, r.stdout, r.stderr)
	}
}

func readGzip(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatalf("gzip header %s: %v", path, err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip %s: %v", path, err)
	}
	return string(data)
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s should exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s should not exist", path)
	}
}

func TestObservableLogReclaimDryRunIsReadOnlyAndReportsEveryFile(t *testing.T) {
	f := newReclaimFixture(t)
	eligible := f.log("gascity-test.jsonl.AAAAAA", "0123456789", reclaimOld)
	young := f.log("gascity-test.jsonl.BBBBBB", "young", reclaimYoung)
	f.setBeads(f.cityDir, `[{"id":"gm-1","title":"a"},{"id":"gm-2","title":"b"}]`)
	f.setBeads(f.rigDir, `[{"id":"ga-1","title":"c"}]`)
	before, _ := os.Stat(eligible)

	r := f.run() // no flags: dry-run

	if r.code != 0 {
		t.Fatalf("dry-run exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	r.mustVerdict(t, eligible, "ELIGIBLE")
	r.mustVerdict(t, young, "HELD:young")
	if got := r.files[eligible]["bytes"]; got != "10" {
		t.Errorf("bytes = %q, want 10", got)
	}
	if got, _ := strconv.Atoi(r.files[eligible]["age_h"]); got < 99 || got > 101 {
		t.Errorf("age_h = %q, want about 100", r.files[eligible]["age_h"])
	}
	if got := r.files[eligible]["open"]; got != "no" {
		t.Errorf("open = %q, want no", got)
	}
	if got := r.files[eligible]["cited-by"]; got != "-" {
		t.Errorf("cited-by = %q, want -", got)
	}
	// R7: the stores scanned with their bead counts, so an empty result is
	// distinguishable from an unreachable store.
	if r.stores[f.cityDir] != "2" || r.stores[f.rigDir] != "1" {
		t.Errorf("stores = %v, want %s=2 and %s=1", r.stores, f.cityDir, f.rigDir)
	}
	if r.sum["eligible"] != "1" || r.sum["eligible_bytes"] != "10" || r.sum["mode"] != "dry-run" {
		t.Errorf("summary = %v, want eligible=1 eligible_bytes=10 mode=dry-run", r.sum)
	}
	// R1: read-only. Nothing compressed, nothing removed, mtime untouched.
	mustNotExist(t, eligible+".gz")
	after, err := os.Stat(eligible)
	if err != nil {
		t.Fatalf("eligible file gone after dry-run: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("dry-run changed mtime: %v -> %v", before.ModTime(), after.ModTime())
	}
	entries, _ := os.ReadDir(f.scratch)
	if len(entries) != 2 {
		t.Errorf("dry-run left %d entries in the scratch root, want 2", len(entries))
	}
}

func TestObservableLogReclaimAddressesEveryStoreExplicitly(t *testing.T) {
	f := newReclaimFixture(t)
	f.log("gascity-test.jsonl.AAAAAA", "x", reclaimOld)

	r := f.run()

	if r.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	log, err := os.ReadFile(f.bdLog)
	if err != nil {
		t.Fatalf("fake bd was never called: %v", err)
	}
	for _, root := range []string{f.cityDir, f.rigDir} {
		want := "bd -C " + root + " list --all --json --limit 0 BEADS_DIR=unset"
		if !strings.Contains(string(log), want) {
			t.Errorf("bd log lacks %q (G9: -C STORE_ROOT with the ambient BEADS_DIR removed)\n%s", want, log)
		}
	}
}

func TestObservableLogReclaimExclusionsHold(t *testing.T) {
	f := newReclaimFixture(t)
	eligible := f.log("gascity-test.jsonl.AAAAAA", "keep-eligible", reclaimOld)
	young := f.log("gascity-test.jsonl.YOUNG1", "young", reclaimYoung)
	open := f.log("gascity-test.jsonl.OPENNN", "open", reclaimOld)
	beadCited := f.log("gascity-test.jsonl.BEAD11", "bead", reclaimOld)
	gateCited := f.log("gascity-test.jsonl.GATE11", "gate", reclaimOld)
	docCited := f.log("gascity-test.jsonl.DOC111", "doc", reclaimOld)
	dup := f.log("gascity-test.jsonl.DUPGZ1", "dup", reclaimOld)
	f.write(dup+".gz", "someone else's gz")
	alreadyGz := f.log("gascity-test.jsonl.DONE11.gz", "already gz", reclaimOld)
	victim := f.log("victim.txt", "victim", reclaimOld)
	symlink := filepath.Join(f.scratch, "gascity-test.jsonl.LINK11")
	if err := os.Symlink(victim, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	subdir := filepath.Join(f.scratch, "sub")
	nested := filepath.Join(subdir, "gascity-test.jsonl.NEST11")
	f.write(nested, "nested")
	aged := time.Now().Add(-reclaimOld)
	if err := os.Chtimes(nested, aged, aged); err != nil {
		t.Fatalf("age nested: %v", err)
	}

	// open: a process holds it
	if err := os.Symlink(open, filepath.Join(f.addProc("321"), "5")); err != nil {
		t.Fatalf("symlink fd: %v", err)
	}
	// cited by basename in a bead (HQ store) and in a rig bead
	f.setBeads(f.cityDir, fmt.Sprintf(`[{"id":"gm-9","title":"evidence","notes":"log at /var/tmp/%s"}]`, filepath.Base(beadCited)))
	f.setBeads(f.rigDir, fmt.Sprintf(`[{"id":"ga-7","description":"see %s"}]`, filepath.Base(gateCited)))
	// cited in a tracked gate record and in tracked docs
	f.track(f.rigDir, "release-gates/ga-x-gate.md", "evidence: "+filepath.Base(gateCited)+"\n")
	f.track(f.cityDir, "docs/notes.md", "also "+filepath.Base(docCited)+"\n")

	r := f.run("--apply")

	if r.code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	r.mustVerdict(t, eligible, "COMPRESSED")
	r.mustVerdict(t, young, "HELD:young")
	r.mustVerdict(t, open, "HELD:open")
	r.mustVerdict(t, beadCited, "HELD:cited")
	r.mustVerdict(t, gateCited, "HELD:cited")
	r.mustVerdict(t, docCited, "HELD:cited")
	r.mustVerdict(t, dup, "HELD:gz-exists")
	if got := r.files[beadCited]["cited-by"]; got != "gm-9" {
		t.Errorf("bead-cited cited-by = %q, want gm-9", got)
	}
	if got := r.files[gateCited]["cited-by"]; !strings.Contains(got, "ga-7") || !strings.Contains(got, "release-gates/ga-x-gate.md") {
		t.Errorf("gate-cited cited-by = %q, want the bead id and the gate record path", got)
	}
	if got := r.files[open]["open"]; got != "yes" {
		t.Errorf("open = %q, want yes", got)
	}
	// Out of scope, so never listed and never touched: .gz files, symlinks, subdirectories.
	for _, p := range []string{alreadyGz, symlink, nested} {
		if _, listed := r.files[p]; listed {
			t.Errorf("%s is out of scope (R2) but was reported", p)
		}
	}
	mustNotExist(t, eligible)
	mustExist(t, eligible+".gz")
	for _, p := range []string{young, open, beadCited, gateCited, docCited, dup, alreadyGz, victim, nested} {
		mustExist(t, p)
	}
	for _, p := range []string{young, open, beadCited, gateCited, docCited, nested} {
		mustNotExist(t, p+".gz")
	}
	if got, err := os.Readlink(symlink); err != nil || got != victim {
		t.Errorf("symlink changed: target %q err %v, want %q", got, err, victim)
	}
	if data, _ := os.ReadFile(dup + ".gz"); string(data) != "someone else's gz" {
		t.Errorf("an existing .gz was overwritten: %q", data)
	}
	if data, _ := os.ReadFile(victim); string(data) != "victim" {
		t.Errorf("symlink victim was modified: %q", data)
	}
}

func TestObservableLogReclaimApplyCompressesVerifiesAndIsIdempotent(t *testing.T) {
	f := newReclaimFixture(t)
	content := strings.Repeat(`{"Action":"pass"}`+"\n", 400)
	path := f.log("gascity-test.jsonl.AAAAAA", content, reclaimOld)
	other := f.log("gascity-productmetrics-testhook.jsonl.ZZZZZZ", "second", reclaimOld+time.Hour)
	before, _ := os.Stat(path)

	r := f.run("--apply")

	if r.code != 0 {
		t.Fatalf("apply exit = %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	r.mustVerdict(t, path, "COMPRESSED")
	r.mustVerdict(t, other, "COMPRESSED")
	if got := r.files[path]["bytes"]; got != strconv.Itoa(len(content)) {
		t.Errorf("bytes before = %q, want %d", got, len(content))
	}
	gz, err := os.Stat(path + ".gz")
	if err != nil {
		t.Fatalf("no .gz written: %v", err)
	}
	if got := r.files[path]["after"]; got != strconv.FormatInt(gz.Size(), 10) {
		t.Errorf("bytes after = %q, want %d", got, gz.Size())
	}
	if gz.Size() >= int64(len(content)) {
		t.Errorf("gz is %d bytes for %d of repetitive input", gz.Size(), len(content))
	}
	if got := readGzip(t, path+".gz"); got != content {
		t.Errorf("gunzip(%s.gz) differs from the original", filepath.Base(path))
	}
	if !gz.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime not preserved: %v -> %v", before.ModTime(), gz.ModTime())
	}
	mustNotExist(t, path)
	mustNotExist(t, other)
	if r.sum["mode"] != "apply" {
		t.Errorf("summary mode = %q, want apply", r.sum["mode"])
	}
	// No temp file may be left behind in the shared scratch root.
	entries, _ := os.ReadDir(f.scratch)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".gz") {
			t.Errorf("stray entry %q left in the scratch root", e.Name())
		}
	}

	// R6: a second run is a no-op.
	again := f.run("--apply")
	if again.code != 0 {
		t.Fatalf("second apply exit = %d\nstdout:\n%s\nstderr:\n%s", again.code, again.stdout, again.stderr)
	}
	if len(again.files) != 0 || again.sum["eligible"] != "0" {
		t.Errorf("second run was not a no-op: files=%v summary=%v", again.files, again.sum)
	}
	if got := readGzip(t, path+".gz"); got != content {
		t.Errorf("second run changed the archive")
	}
}

func TestObservableLogReclaimApplyReevaluatesAtApplyTime(t *testing.T) {
	f := newReclaimFixture(t)
	path := f.log("gascity-test.jsonl.AAAAAA", "late-citation", reclaimOld)

	dry := f.run()
	dry.mustVerdict(t, path, "ELIGIBLE")

	// A citation lands between the dry-run and the apply.
	f.setBeads(f.rigDir, fmt.Sprintf(`[{"id":"ga-new","notes":"%s"}]`, filepath.Base(path)))
	applied := f.run("--apply")

	applied.mustVerdict(t, path, "HELD:cited")
	mustExist(t, path)
	mustNotExist(t, path+".gz")
}

func TestObservableLogReclaimProbeFailuresFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(f *reclaimFixture)
		reason  string
	}{
		{"no proc root", func(f *reclaimFixture) {
			if err := os.RemoveAll(f.proc); err != nil {
				t.Fatal(err)
			}
		}, "open"},
		{"empty proc root", func(f *reclaimFixture) {
			if err := os.RemoveAll(filepath.Join(f.proc, "1")); err != nil {
				t.Fatal(err)
			}
		}, "open"},
		{"unreadable same-uid fd table", func(f *reclaimFixture) {
			// A process directory of ours with no readable fd table.
			if err := os.MkdirAll(filepath.Join(f.proc, "777"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "open"},
		{"HQ store errors", func(f *reclaimFixture) { f.mark(f.cityDir, ".bdfail") }, "cited-by"},
		{"rig store errors", func(f *reclaimFixture) { f.mark(f.rigDir, ".bdfail") }, "cited-by"},
		{"store returns invalid json", func(f *reclaimFixture) { f.setBeads(f.rigDir, "not json") }, "cited-by"},
		{"store returns a non-array", func(f *reclaimFixture) { f.setBeads(f.rigDir, `{"error":"x"}`) }, "cited-by"},
		{"gc rig list fails", func(f *reclaimFixture) { f.write(filepath.Join(f.fixDir, "gc.fail"), "") }, "cited-by"},
		{"tracked-file listing fails", func(f *reclaimFixture) { f.mark(f.rigDir, ".gitfail") }, "cited-by"},
		{"empty index", func(f *reclaimFixture) {
			f.setBeads(f.cityDir, `[]`)
			f.setBeads(f.rigDir, `[]`)
		}, "cited-by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReclaimFixture(t)
			path := f.log("gascity-test.jsonl.AAAAAA", "must-survive", reclaimOld)
			tc.breakIt(f)

			for _, args := range [][]string{nil, {"--apply"}} {
				r := f.run(args...)
				if r.code == 0 {
					t.Errorf("%v: exit 0 despite a failed probe\nstdout:\n%s", args, r.stdout)
				}
				r.mustVerdict(t, path, "HELD:probe-failed")
				if !strings.Contains(r.stderr, "probe-failed") {
					t.Errorf("%v: stderr does not say why:\n%s", args, r.stderr)
				}
				if got := r.files[path][tc.reason]; got == "no" || got == "-" {
					t.Errorf("%v: field %s = %q; a failed probe must not read as a clean answer", args, tc.reason, got)
				}
				mustExist(t, path)
				mustNotExist(t, path+".gz")
			}
		})
	}
}

func TestObservableLogReclaimRejectsBadUsage(t *testing.T) {
	f := newReclaimFixture(t)
	path := f.log("gascity-test.jsonl.AAAAAA", "x", reclaimOld)

	for _, args := range [][]string{{"--delete"}, {"--scratch-root"}, {"stray-positional"}} {
		r := f.run(args...)
		if r.code != 2 {
			t.Errorf("%v: exit = %d, want 2\nstderr:\n%s", args, r.code, r.stderr)
		}
	}
	missing := scriptCommand(repoRoot(t), "observable-log-reclaim", "--scratch-root", filepath.Join(f.dir, "absent"), "--proc-root", f.proc)
	missing.Dir = f.dir
	missing.Env = []string{"PATH=" + f.fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + f.dir, "TMPDIR=" + f.dir}
	if err := missing.Run(); err == nil {
		t.Errorf("a scratch root that does not exist must not exit 0")
	}
	mustExist(t, path)
	mustNotExist(t, path+".gz")
}

// The tool must stay a foreground, operator-invoked procedure: no scheduler,
// no detached work, and it never deletes by pattern (G1, G3, G7).
func TestObservableLogReclaimStaysForegroundAndNarrow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "observable-log-reclaim"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	src := string(data)
	if !strings.HasPrefix(src, "#!") {
		t.Errorf("script has no shebang")
	}
	for _, banned := range []string{"nohup", "setsid", "disown", "crontab", "systemd-run", "vartmp-scratch-reaper"} {
		if strings.Contains(src, banned) {
			t.Errorf("script mentions %q: it must stay a foreground operator tool", banned)
		}
	}
	if regexp.MustCompile(`(?m)^[^#\n]*[^&]&\s*$`).MatchString(src) {
		t.Errorf("script backgrounds a command with a trailing &")
	}
	if regexp.MustCompile(`(?m)^\s*trap\b`).MatchString(src) && strings.Contains(src, "gascity-*") && regexp.MustCompile(`(?m)trap[^\n]*rm\b[^\n]*\$\{?(f|file|log)\b`).MatchString(src) {
		t.Errorf("a trap removes a log")
	}
	mode := fileMode(t, filepath.Join(repoRoot(t), "scripts", "observable-log-reclaim"))
	if mode&0o111 == 0 {
		t.Errorf("script is not executable (mode %v)", mode)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode()
}
