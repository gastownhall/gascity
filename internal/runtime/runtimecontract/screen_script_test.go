package runtimecontract

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file drives gc-session-screen against a fake screen and ps, so its
// decisions over screen's statuses, an unreadable listing and the process
// table are tested deterministically, without a terminal or real agents.

// fakeBackend is a directory of fake screen and ps executables. screen -ls
// prints the file ls (or fails when ls.fail exists), first running the
// script ls.then once if it exists (an op that runs after the listing is
// read, before its reader acts on it), and logs every other invocation to
// calls; ps prints ps.procs as its snapshot (or fails it when
// ps.fail exists), cut to COLUMNS without -ww as procps cuts it even into a
// pipe, and for -o lstart= a fixed start time (lstart.<pid> when it exists,
// none for a pid in gone); kill logs to calls and adds the pid to gone unless
// unkillable exists; uname -n prints host when it exists. The script run is
// a copy whose /proc is proc (see procFile).
type fakeBackend struct {
	t                  *testing.T
	dir, script, state string
	env                []string
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	for _, tool := range []string{"flock", "bash", "cksum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	uname, err := exec.LookPath("uname")
	if err != nil {
		t.Skip("uname not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nF="+fmt.Sprintf("%q", dir)+"\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Sessions a test sets live in $F/ls; sessions the script starts live in
	// its SCREENDIR, as real screen keeps them, so a script resolving
	// another socket directory does not see them.
	write("screen", `L="$SCREENDIR/fake-ls"
if [ "$1" = -ls ]; then
  if [ -f "$F/ls.fail" ]; then echo "Cannot open your terminal"; exit 1; fi
  rows=$(cat "$F/ls" "$L" 2>/dev/null)
  if [ -f "$F/ls.then" ]; then mv -f "$F/ls.then" "$F/ls.then.ran"; sh "$F/ls.then.ran"; fi
  if [ -n "$rows" ]; then printf 'There are screens on:\n%s\n%s Sockets in /x.\n' "$rows" 1; exit 1; fi
  echo "No Sockets found in /x."; exit 1
fi
printf '%s\n' "$*" >> "$F/calls"
# -dmS creates a session; -wipe and -X quit remove one, unless sticky.
case "$1" in
  -dmS) [ -f "$F/nobox" ] || printf '\t4194999.%s\t(11/02/2026 01:00:00 PM)\t(Detached)\n' "$2" >> "$L" ;;
  -wipe | -S)
    # hardcopy, as real screen: -X exits 0 once the command is sent, and
    # SCREEN writes the capture (a blank screen's empty lines included)
    # unless it cannot (hardcopy.fail: nothing written, still exit 0).
    # hardcopy.slow: SCREEN writes a long capture in two parts; the fake
    # sleep appends the second during the reader's second pause, after it
    # first measured the capture.
    if [ "$4" = hardcopy ] && [ -f "$F/hardcopy.slow" ]; then
      printf 'head of %s\n' "$2" > "$6"; printf '%s' "$6" > "$F/hardcopy.path"; : > "$F/hardcopy.pauses"; exit 0
    fi
    if [ "$4" = hardcopy ]; then [ -f "$F/hardcopy.fail" ] || printf 'screen of %s\n\n' "$2" > "$6"; exit 0; fi
    if [ "$1" = -wipe ] || [ "$4" = quit ]; then
      if [ ! -f "$F/sticky" ]; then
        for f in "$F/ls" "$L"; do
          [ -f "$f" ] || continue
          grep -v -F "$(printf '\t%s\t' "$2")" "$f" > "$f.new" || true; mv "$f.new" "$f"
        done
      fi
    fi ;;
esac
`)
	write("getent", `[ "$1" = passwd ] && printf 'u:x:%s:%s::%s:/bin/sh\n' "$2" "$2" "$(cat "$F/home")"
`)
	// The snapshot is ps.procs, one "<pid> <ppid> <comm> <args...>" per
	// line, printed in the fixed columns the script asks for.
	write("ps", `printf 'ps %s\n' "$*" >> "$F/ps.calls"
for last; do :; done
case "$*" in
  *lstart*)
    grep -qx "$last" "$F/gone" 2>/dev/null && exit 1
    if [ -f "$F/lstart.$last" ]; then cat "$F/lstart.$last"; else echo "Mon Nov  2 00:00:00 2026"; fi ;;
  *)
    [ -f "$F/ps.fail" ] && exit 1
    [ -f "$F/ps.procs" ] || exit 0
    cut=cat
    case " $* " in *" -ww "*) ;; *) [ -n "${COLUMNS:-}" ] && cut="cut -c1-$COLUMNS" ;; esac
    while read -r pid ppid comm args; do
      case "$*" in
        *comm*) printf '%10s %10s %-16.16s %s\n' "$pid" "$ppid" "$(printf %s "$comm" | tr '+' ' ')" "$args" ;;
        *args=*) printf '%10s %10s %s\n' "$pid" "$ppid" "$args" ;;
        *) printf '%s %s\n' "$pid" "$ppid" ;;
      esac
    done < "$F/ps.procs" | $cut ;;
esac
`)
	write("kill", `printf 'kill %s\n' "$*" >> "$F/calls"
for last; do :; done
[ -f "$F/unkillable" ] || echo "$last" >> "$F/gone"
`)
	// sleep sleeps as sleep does; under hardcopy.slow, the second pause
	// after a hardcopy appends the capture's second part (SCREEN still
	// writing it).
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not installed")
	}
	write("sleep", `if [ -f "$F/hardcopy.slow" ] && [ -f "$F/hardcopy.pauses" ]; then
  echo x >> "$F/hardcopy.pauses"
  if [ "$(wc -l < "$F/hardcopy.pauses")" -eq 2 ]; then printf 'tail of the capture\n' >> "$(cat "$F/hardcopy.path")"; fi
fi
exec `+fmt.Sprintf("%q", sleepBin)+` "$@"
`)
	write("uname", `if [ "$1" = -n ] && [ -f "$F/host" ]; then cat "$F/host"; exit 0; fi
exec `+fmt.Sprintf("%q", uname)+` "$@"
`)
	// A short state directory, so the socket-path guard admits ordinary
	// names.
	state, err := os.MkdirTemp("", "gcf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	// A copy of the script reading the fixture's proc as its /proc.
	src, err := os.ReadFile(screenReferencePath(t))
	if err != nil {
		t.Fatal(err)
	}
	const procLine = "\nPROC=/proc\n"
	if strings.Count(string(src), procLine) != 1 {
		t.Fatalf("the script has no single %q line", procLine)
	}
	script := filepath.Join(dir, "gc-session-screen")
	patched := strings.Replace(string(src), procLine, "\nPROC="+fmt.Sprintf("%q", filepath.Join(dir, "proc"))+"\n", 1)
	if err := os.WriteFile(script, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeBackend{t: t, dir: dir, script: script, state: state, env: []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"GC_EXEC_STATE_DIR=" + state,
		"GC_EXEC_CITY_PATH=/fake-city",
	}}
}

// procFile writes the fake /proc's file rel (a pid's cmdline, the boot id).
func (f *fakeBackend) procFile(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.dir, "proc", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.file(path, content)
}

// cmdline sets pid's argv in the fake /proc.
func (f *fakeBackend) cmdline(pid string, argv ...string) {
	f.procFile(pid+"/cmdline", strings.Join(argv, "\x00")+"\x00")
}

// procStat sets pid's stat in the fake /proc: its command name, its state
// (S, Z) and its start time, the 22nd field. With it the script reads pid
// from /proc, not ps.
func (f *fakeBackend) procStat(pid, comm, state, start string) {
	f.procFile(pid+"/stat", fmt.Sprintf("%s (%s) %s 1 %s %s 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 %s 0 0\n", pid, comm, state, pid, pid, start))
}

// rows sets screen -ls to exactly the given "<pid>.<name>\t<status>" rows,
// replacing the sessions the script started.
func (f *fakeBackend) rows(rows ...string) {
	started, _ := filepath.Glob(filepath.Join(f.state, "*", "fake-ls"))
	for _, p := range started {
		f.file(p, "")
	}
	var b strings.Builder
	for _, r := range rows {
		pidName, status, _ := strings.Cut(r, "\t")
		fmt.Fprintf(&b, "\t%s\t(11/02/2026 01:00:00 PM)\t(%s)\n", pidName, status)
	}
	f.file("ls", b.String())
}

func (f *fakeBackend) file(name, content string) {
	f.t.Helper()
	if !filepath.IsAbs(name) {
		name = filepath.Join(f.dir, name)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// run runs the script with op and args and stdin, through the kit's own op
// invocation, and returns its stdout and exit code.
func (f *fakeBackend) run(stdin string, args ...string) (string, int) {
	f.t.Helper()
	r := &runner{path: f.script, opts: Options{OpTimeout: 30 * time.Second}}
	res := r.opEnv(context.Background(), r.opts.OpTimeout, f.env, []byte(stdin), args...)
	switch {
	case res.unsupported:
		return "", 2
	case res.err != nil && res.exitCode == 0:
		f.t.Fatalf("%v: %v", args, res.err)
	case res.err != nil:
		return "", res.exitCode
	}
	return strings.TrimSpace(res.stdout), 0
}

// calls are the script's screen invocations other than -ls.
func (f *fakeBackend) calls() string {
	data, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	return string(data)
}

// Kills (MUST 1) an attachment that fails open: Attached and the multiuser
// "Multi, attached" are attached, the detached forms are not, and a status
// screen cannot read ("Private", "Remote or dead") or an unreadable listing
// is no answer (non-zero exit); a zombie kill of an attached box is
// "changed" and quits nothing.
func TestScreenScriptAttachmentNeverFailsOpen(t *testing.T) {
	f := newFakeBackend(t)
	for status, want := range map[string]string{
		"Attached": "true", "Multi, attached": "true",
		"Detached": "false", "Multi, detached": "false",
		"Private": "", "Remote or dead": "",
	} {
		f.rows("4194001.mc-a\t" + status)
		out, code := f.run("", "is-attached", "mc-a")
		if want == "" {
			if code == 0 {
				t.Errorf("%s: is-attached = %q, exit 0; want a failure (cannot tell)", status, out)
			}
			continue
		}
		if code != 0 || out != want {
			t.Errorf("%s: is-attached = %q, exit %d; want %s", status, out, code, want)
		}
	}
	f.file("ls.fail", "")
	if out, code := f.run("", "is-attached", "mc-a"); code == 0 {
		t.Errorf("an unreadable listing: is-attached = %q, exit 0; want a failure", out)
	}
	if err := os.Remove(filepath.Join(f.dir, "ls.fail")); err != nil {
		t.Fatal(err)
	}

	// A readable process table in which the agent is not running: only the
	// attachment keeps the box.
	f.file("ps.procs", "4194101 4194001 sh /bin/sh /x/wrapper.sh\n")
	for _, status := range []string{"Attached", "Multi, attached"} {
		f.rows("4194001.mc-z\t" + status)
		f.run("nosuch", "set-meta", "mc-z", "GT_PROCESS_NAMES")
		req := `{"object":{"id":"4194001","created":"Mon Nov 2 00:00:00 2026"},"mode":"zombie"}`
		if out, code := f.run(req, "kill-object", "mc-z"); code != 0 || out != "changed" {
			t.Errorf("%s: zombie kill = %q, exit %d; want changed", status, out, code)
		}
		if strings.Contains(f.calls(), "quit") {
			t.Fatalf("%s: a zombie kill quit an attached box: %q", status, f.calls())
		}
	}
	f.rows("4194001.mc-p\tPrivate")
	if out, _ := f.run("", "observe", "mc-p"); !strings.Contains(out, `"unknown"`) {
		t.Errorf("observe of an unreadable session = %q, want unknown", out)
	}
}

// Kills (MUST 3) a fresh_only start that reads an unreadable listing as
// vacant: it fails, creates no box and leaves the name's identity alone.
func TestScreenScriptStartRefusesAnUnreadableListing(t *testing.T) {
	f := newFakeBackend(t)
	f.run("tok-old", "set-meta", "mc-a", "GC_INSTANCE_TOKEN")
	f.file("ls.fail", "")
	if _, code := f.run(`{"command":"sleep 300","fresh_only":true,"env":{"GC_INSTANCE_TOKEN":"tok-new"}}`, "start", "mc-a"); code == 0 {
		t.Fatal("start over an unreadable listing succeeded")
	}
	if strings.Contains(f.calls(), "-dmS") {
		t.Fatalf("start created a box over an unreadable listing: %q", f.calls())
	}
	if out, _ := f.run("", "get-meta", "mc-a", "GC_INSTANCE_TOKEN"); out != "tok-old" {
		t.Fatalf("identity after the refused start = %q, want it untouched", out)
	}
}

// Kills (SHOULD 7) a driving op that targets a session by a name prefix:
// with mc-a gone and mc-ab running, no op for mc-a reaches mc-ab, and mc-ab's
// ops target its exact "<pid>.<name>".
func TestScreenScriptTargetsTheExactSession(t *testing.T) {
	f := newFakeBackend(t)
	f.rows("4194001.mc-ab\tDetached")
	for _, op := range []string{"interrupt", "nudge", "peek"} {
		f.run("hi", op, "mc-a")
	}
	if _, code := f.run("", "attach", "mc-a"); code == 0 {
		t.Error("attach to a name no session holds succeeded")
	}
	if calls := f.calls(); calls != "" {
		t.Fatalf("ops for mc-a reached screen: %q", calls)
	}
	for _, op := range []string{"interrupt", "nudge", "attach"} {
		f.run("hi", op, "mc-ab")
	}
	targets := 0
	for _, line := range strings.Split(f.calls(), "\n") {
		if !strings.HasPrefix(line, "-S ") && !strings.HasPrefix(line, "-r ") {
			continue // a nudge's payload line
		}
		targets++
		if !strings.Contains(line, "4194001.mc-ab") {
			t.Errorf("screen call %q does not target 4194001.mc-ab", line)
		}
	}
	if targets != 3 {
		t.Errorf("%d targeted screen calls for interrupt, nudge and attach, want 3: %q", targets, f.calls())
	}
}

// Kills (MUST 2) an agent read dead because it runs under another name or
// below the box's direct child: the snapshot is walked to every descendant
// and matched as gc's process table matches it; an unreadable snapshot
// leaves the agent out (unknown), never dead.
func TestScreenScriptAgentMatching(t *testing.T) {
	f := newFakeBackend(t)
	f.rows("4194001.mc-a\tDetached")
	// Each process is "<pid> <ppid> <comm> <args...>"; "+" in comm is a space.
	for _, tc := range []struct {
		name, proc, names, want string
	}{
		{"node 25 comm", "4194102 4194101 node-MainThread node /usr/local/bin/gemini --yolo", "gemini", "alive"},
		{"interpreter script with an extension", "4194102 4194101 node node /opt/copilot/index.js", "index", "alive"},
		{"runner subcommand", "4194102 4194101 npx npx run codex", "codex", "alive"},
		{"long name truncated in comm", "4194102 4194101 very-long-agent /opt/bin/very-long-agent-name --x", "very-long-agent-name", "alive"},
		{"comm with a space", "4194102 4194101 tmux:+server /usr/bin/claude", "claude", "alive"},
		{"matched by comm alone", "4194102 4194101 claude /opt/runner --x", "claude", "alive"},
		{"grandchild", "4194102 4194101 claude /usr/bin/claude", "claude", "alive"},
		{"not running", "4194102 4194101 vim /usr/bin/vim", "claude", "dead"},
		{"another box's agent", "4194102 1 claude /usr/bin/claude", "claude", "dead"},
	} {
		f.file("ps.procs", "4194101 4194001 sh /bin/sh /x/wrapper.sh\n"+tc.proc+"\n")
		out, code := f.run(tc.names+"\n", "observe", "mc-a")
		var o struct {
			State, Agent string
		}
		if err := json.Unmarshal([]byte(out), &o); err != nil || code != 0 {
			t.Fatalf("%s: observe = %q (%d), %v", tc.name, out, code, err)
		}
		if o.Agent != tc.want {
			t.Errorf("%s: agent = %q, want %s", tc.name, o.Agent, tc.want)
		}
	}
	f.file("ps.fail", "")
	if out, _ := f.run("claude\n", "observe", "mc-a"); !strings.Contains(out, `"running"`) || strings.Contains(out, `"agent"`) {
		t.Errorf("an unreadable process table: observe = %q, want the running box with no agent (unknown)", out)
	}
}

// Kills (SHOULD 5) an inventory that answers despite a name two sessions
// hold, and a row lookup that interprets escapes in a name.
func TestScreenScriptInventoryRefusesARepeatedName(t *testing.T) {
	f := newFakeBackend(t)
	f.rows("4194001.mc-a\tDetached", "4194002.mc-a\tDetached")
	if out, code := f.run("", "inventory"); code == 0 {
		t.Errorf("inventory with a repeated name = %q, exit 0; want a failure", out)
	}
	f.rows(`4194003.x\ty` + "\tDetached")
	if out, _ := f.run("", "observe", `x\ty`); !strings.Contains(out, `"running"`) {
		t.Errorf("observe of a name with a backslash = %q, want running", out)
	}
}

// Kills (SHOULD 6) a dead box without its object, a corpse kill that does
// not wipe it, a "killed" that was not verified, and a stop that drops a
// box's identity while the box is still listed.
func TestScreenScriptDeadBoxes(t *testing.T) {
	f := newFakeBackend(t)
	start := `{"command":"sleep 300","env":{"GC_SESSION_ID":"sid-d"}}`
	if _, code := f.run(start, "start", "mc-d"); code != 0 {
		t.Fatalf("start exited %d", code)
	}
	f.rows("4194999.mc-d\tDead ???")
	var o struct {
		State  string
		Object struct{ ID, Created string }
	}
	out, _ := f.run("", "observe", "mc-d")
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.State != "dead" || o.Object.ID != "4194999" || o.Object.Created == "" {
		t.Fatalf("observe of a dead box = %q (%v), want dead with its recorded object", out, err)
	}
	req := fmt.Sprintf(`{"object":{"id":%q,"created":%q},"mode":"corpse"}`, o.Object.ID, o.Object.Created)
	if out, code := f.run(req, "kill-object", "mc-d"); code != 0 || out != "killed" || !strings.Contains(f.calls(), "-wipe 4194999.mc-d") {
		t.Fatalf("corpse kill = %q (%d), calls %q; want the dead box wiped and killed", out, code, f.calls())
	}
	if out, _ := f.run("", "observe", "mc-d"); !strings.Contains(out, `"absent"`) {
		t.Fatalf("observe after the corpse kill = %q, want absent", out)
	}

	if _, code := f.run(strings.Replace(start, "sid-d", "sid-e", 1), "start", "mc-e"); code != 0 {
		t.Fatal("start mc-e failed")
	}
	f.rows("4194999.mc-e\tDead ???")
	f.file("sticky", "")
	if _, code := f.run("", "stop", "mc-e"); code == 0 {
		t.Fatal("stop of a dead box that stays listed succeeded")
	}
	if out, _ := f.run("", "get-meta", "mc-e", "GC_SESSION_ID"); out != "sid-e" {
		t.Fatalf("identity after a stop that left the box = %q, want it kept", out)
	}
	if out, code := f.run(strings.Replace(req, "mc-d", "mc-e", 1), "kill-object", "mc-e"); code == 0 && out == "killed" {
		t.Fatal("a corpse kill that left the box listed answered killed")
	}
	if err := os.Remove(filepath.Join(f.dir, "sticky")); err != nil {
		t.Fatal(err)
	}
	if _, code := f.run("", "stop", "mc-e"); code != 0 {
		t.Fatal("stop of a dead box failed")
	}
	if out, _ := f.run("", "get-meta", "mc-e", "GC_SESSION_ID"); out != "" {
		t.Fatalf("identity after the stop = %q, want it removed", out)
	}
}

// Kills a start that reports success for a session screen never started,
// or tries one whose socket path cannot fit.
func TestScreenScriptStartNeedsTheBox(t *testing.T) {
	f := newFakeBackend(t)
	if _, code := f.run(`{"command":"sleep 300"}`, "start", strings.Repeat("n", 120)); code == 0 {
		t.Fatal("start with a socket path over 107 bytes succeeded")
	}
	if strings.Contains(f.calls(), "-dmS") {
		t.Fatalf("start ran screen for a socket path that cannot fit: %q", f.calls())
	}
	// A socket path is "<state>/<8 hex key>/<pid>.<name>", the pid counted
	// as 7 digits. Two bytes per character: a name of half the budget and
	// one more character fits in characters, not in bytes.
	budget := 107 - len(f.state) - len("/01234567/1234567.")
	if _, code := f.run(`{"command":"sleep 300"}`, "start", strings.Repeat("é", budget/2+1)); code == 0 {
		t.Fatal("start with a socket path over 107 bytes (but not characters) succeeded")
	}
	if strings.Contains(f.calls(), "-dmS") {
		t.Fatalf("start ran screen for a socket path whose bytes cannot fit: %q", f.calls())
	}
	f.file("nobox", "")
	startN := `{"command":"sleep 300","env":{"GC_SESSION_ID":"sid-n"}}`
	// An unreadable process table cannot show this start's SCREEN gone: the
	// session may yet appear, so its identity stays.
	f.file("ps.fail", "")
	if _, code := f.run(startN, "start", "mc-n"); code == 0 {
		t.Fatal("start succeeded although screen started no session")
	}
	if out, _ := f.run("", "get-meta", "mc-n", "GC_SESSION_ID"); out != "sid-n" {
		t.Fatalf("identity of a start whose screen could not be checked = %q, want it kept", out)
	}
	if err := os.Remove(filepath.Join(f.dir, "ps.fail")); err != nil {
		t.Fatal(err)
	}
	// This start's own SCREEN (and its window) is ended under the lock, and
	// only then is the identity removed. It is found by its exact argv, here
	// screen run by path; another name's SCREEN, a SCREEN whose argv differs
	// only in the name, and another city's SCREEN of the same name are
	// untouched.
	wrapper := filepath.Join(f.metaDir(), "mc-n.wrapper.sh")
	f.file("ps.procs", "4194990 1 screen x\n4194991 4194990 sh x\n4194992 1 screen x\n4194995 1 screen x\n4194996 1 screen x\n")
	f.cmdline("4194990", "/usr/bin/SCREEN", "-dmS", "mc-n", wrapper)
	f.cmdline("4194991", "/bin/sh", wrapper)
	f.cmdline("4194992", "SCREEN", "-dmS", "mc-nn", filepath.Join(f.metaDir(), "mc-nn.wrapper.sh"))
	f.cmdline("4194995", "SCREEN", "-dmS", "mc-nx", wrapper)
	f.cmdline("4194996", "SCREEN", "-dmS", "mc-n", "/other/city-1/meta/mc-n.wrapper.sh")
	f.file("unkillable", "")
	if _, code := f.run(startN, "start", "mc-n"); code == 0 {
		t.Fatal("start succeeded although screen started no session")
	}
	if out, _ := f.run("", "get-meta", "mc-n", "GC_SESSION_ID"); out != "sid-n" {
		t.Fatalf("identity of a start whose screen survived its kill = %q, want it kept", out)
	}
	if err := os.Remove(filepath.Join(f.dir, "unkillable")); err != nil {
		t.Fatal(err)
	}
	f.file("calls", "")
	if _, code := f.run(startN, "start", "mc-n"); code == 0 {
		t.Fatal("start succeeded although screen started no session")
	}
	if calls := f.calls(); !strings.Contains(calls, "-- 4194990") || !strings.Contains(calls, "-- 4194991") || strings.Contains(calls, "4194992") || strings.Contains(calls, "4194995") || strings.Contains(calls, "4194996") {
		t.Fatalf("kills after a start timeout: %q, want mc-n's SCREEN and window only", calls)
	}
	if out, _ := f.run("", "get-meta", "mc-n", "GC_SESSION_ID"); out != "" {
		t.Fatalf("identity of a start whose screen was ended = %q, want it removed", out)
	}
	// A name procps cannot render in the C locale is matched by its bytes,
	// and a wrapped screen binary by its arguments, whatever its argv[0].
	f.file("calls", "")
	f.file("ps.procs", "4194994 1 screen SCREEN -dmS mc-?? x\n")
	f.cmdline("4194994", "/nix/store/x-screen/bin/.screen-wrapped", "-dmS", "mc-é", filepath.Join(f.metaDir(), "mc-é.wrapper.sh"))
	f.env = append(f.env, "LC_ALL=C")
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-é"); code == 0 || !strings.Contains(f.calls(), "-- 4194994") {
		t.Fatalf("start timeout of a non-ASCII name: exit %d, calls %q; want its SCREEN ended", code, f.calls())
	}
	f.env = f.env[:len(f.env)-1]
	if err := os.Remove(filepath.Join(f.dir, "nobox")); err != nil {
		t.Fatal(err)
	}
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-ok"); code != 0 || !strings.Contains(f.calls(), "-dmS mc-ok") {
		t.Fatalf("a start with an ordinary name did not reach screen: %q", f.calls())
	}
}

// metaDir is the fake city's metadata directory.
func (f *fakeBackend) metaDir() string {
	f.t.Helper()
	dirs, err := filepath.Glob(filepath.Join(f.state, "*.meta"))
	if err != nil || len(dirs) != 1 {
		f.t.Fatalf("metadata directory: %v, %v", dirs, err)
	}
	return dirs[0]
}

// Kills (SHOULD 13) state another user could read or redirect: every
// directory is 0700 and every file 0600, and a state directory that is a
// symlink is refused.
func TestScreenScriptPrivateState(t *testing.T) {
	f := newFakeBackend(t)
	if _, code := f.run(`{"command":"sleep 300","env":{"GC_INSTANCE_TOKEN":"tok-secret"}}`, "start", "mc-a"); code != 0 {
		t.Fatal("start failed")
	}
	err := filepath.Walk(f.state, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %v, want no group or other access", path, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.dir, "link")
	if err := os.Symlink(f.state, link); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "GC_EXEC_STATE_DIR="+link)
	if _, code := f.run("", "observe", "mc-a"); code == 0 {
		t.Error("a symlinked state directory was used")
	}
}

// Kills (SHOULD 8) a box whose identity sidecar is gone reading null for
// good: its wrapper's exports, written before the box, are its identity;
// and GC_CITY_PATH is kept with the identity for get-meta. Kills (adoption
// review SHOULD) a recovered identity taken from the caller's environment:
// a gc CLI run inside an agent session carries that session's identity, and
// none of it may name the box.
func TestScreenScriptIdentityFromTheWrapper(t *testing.T) {
	f := newFakeBackend(t)
	if _, code := f.run(`{"command":"sleep 300","env":{"GC_SESSION_ID":"sid-w","GC_INSTANCE_TOKEN":"tok w'q","GC_CITY_PATH":"/cities/w"}}`, "start", "mc-w"); code != 0 {
		t.Fatal("start failed")
	}
	if out, _ := f.run("", "get-meta", "mc-w", "GC_CITY_PATH"); out != "/cities/w" {
		t.Errorf("get-meta GC_CITY_PATH = %q, want /cities/w", out)
	}
	// A wrapper carrying no identity proves none: identity null, never {}.
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-v"); code != 0 {
		t.Fatal("start mc-v failed")
	}
	meta := f.metaDir()
	sidecars, _ := filepath.Glob(filepath.Join(meta, "mc-w.meta.*"))
	for _, p := range append(sidecars, filepath.Join(meta, "mc-w.identity"), filepath.Join(meta, "mc-v.identity")) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	f.env = append(f.env, "GC_SESSION_ID=caller-sid", "GC_RUNTIME_EPOCH=99", "GC_INSTANCE_TOKEN=caller-tok", "GT_PROCESS_NAMES=caller")
	out, _ := f.run("", "observe", "mc-w")
	var o struct{ Identity map[string]string }
	want := map[string]string{"GC_SESSION_ID": "sid-w", "GC_INSTANCE_TOKEN": "tok w'q"}
	if err := json.Unmarshal([]byte(out), &o); err != nil || !reflect.DeepEqual(o.Identity, want) {
		t.Fatalf("observe without the sidecar = %q (%v), want exactly the wrapper's identity %v", out, err, want)
	}
	if out, _ := f.run("", "observe", "mc-v"); !strings.Contains(out, `"identity":null`) {
		t.Fatalf("observe of a box whose wrapper carries no identity = %q, want identity null", out)
	}
}

// Kills (MUST A) a state directory that follows the caller's environment:
// the supervisor, a cron job and a sudo shell differ in XDG_RUNTIME_DIR,
// TMPDIR and HOME, and each must find the box the other started; a box read
// absent there would be started twice.
func TestScreenScriptStateIgnoresTheCallersEnvironment(t *testing.T) {
	f := newFakeBackend(t)
	home, err := os.MkdirTemp("", "gch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	f.file("home", home)
	base := []string{f.env[0], "GC_EXEC_CITY_PATH=/fake-city"} // PATH, no GC_EXEC_STATE_DIR
	supervisor := append(append([]string{}, base...), "XDG_RUNTIME_DIR="+f.dir+"/run-a", "TMPDIR="+f.dir+"/tmp-a", "HOME="+f.dir+"/home-a")
	cron := append(append([]string{}, base...), "TMPDIR="+f.dir+"/tmp-b", "HOME="+f.dir+"/home-b")
	f.env = supervisor
	if _, code := f.run(`{"command":"sleep 300","env":{"GC_SESSION_ID":"sid-m"}}`, "start", "mc-mayor"); code != 0 {
		t.Fatal("start failed")
	}
	f.env = cron
	if out, _ := f.run("", "observe", "mc-mayor"); !strings.Contains(out, `"running"`) || !strings.Contains(out, "sid-m") {
		t.Fatalf("observe from another environment = %q, want the running box with its identity", out)
	}
	if _, code := f.run(`{"command":"sleep 300","fresh_only":true}`, "start", "mc-mayor"); code != 73 {
		t.Fatalf("a fresh_only start from another environment exited %d, want 73 (no twin)", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".gcss")); err != nil {
		t.Fatalf("state is not under the password database's home: %v", err)
	}
}

// Kills (MUST B) proctable's name aliases missing from the script: "kimi"
// accepts kimi-code, and "gc" accepts the base name of the gc binary the
// box runs (its GC_BIN), so neither live agent reads dead and is killed.
func TestScreenScriptAgentAliases(t *testing.T) {
	f := newFakeBackend(t)
	f.rows("4194001.mc-k\tDetached", "4194002.mc-g\tDetached")
	f.run("/nix/store/abc-gc/bin/.gc-wrapped", "set-meta", "mc-g", "GC_BIN")
	f.file("ps.procs", "4194101 4194001 kimi-code kimi-code --yolo\n4194201 4194002 .gc-wrapped /nix/store/abc-gc/bin/.gc-wrapped start\n")
	for name, names := range map[string]string{"mc-k": "kimi", "mc-g": "gc"} {
		out, _ := f.run(names+"\n", "observe", name)
		if !strings.Contains(out, `"agent":"alive"`) {
			t.Errorf("%s with process name %s: observe = %q, want agent alive", name, names, out)
		}
	}

	// A box started with GC_BIN in its env keeps it for the alias.
	if _, code := f.run(`{"command":"sleep 300","env":{"GC_BIN":"/opt/gc/.gc-wrapped","GT_PROCESS_NAMES":"gc"}}`, "start", "mc-s"); code != 0 {
		t.Fatal("start mc-s failed")
	}
	f.file("ps.procs", "4194301 4194999 .gc-wrapped /opt/gc/.gc-wrapped start\n")
	inv, _ := f.run("", "inventory")
	if !strings.Contains(inv, `"name":"mc-s","state":"running"`) || !strings.Contains(inv, `"agent":"alive"`) {
		t.Errorf("inventory of a box running its own GC_BIN = %q, want its agent alive", inv)
	}
}

// Kills (round 3 MUST) a process snapshot cut to the caller's COLUMNS:
// procps cuts each line to COLUMNS even into a pipe, so without -ww an agent
// matched by a long command line reads dead and a zombie kill quits its box.
func TestScreenScriptReadsTheWholeCommandLine(t *testing.T) {
	f := newFakeBackend(t)
	f.env = append(f.env, "COLUMNS=80")
	f.rows("4194001.mc-c\tDetached")
	f.run("copilot", "set-meta", "mc-c", "GT_PROCESS_NAMES")
	f.file("ps.procs", "4194101 4194001 sh /bin/sh /x/wrapper.sh\n"+
		"4194102 4194101 node node /home/agent/.local/share/nvm/versions/node/v25.1.0/lib/node_modules/@github/copilot/dist/copilot.js\n")
	if out, _ := f.run("copilot\n", "observe", "mc-c"); !strings.Contains(out, `"agent":"alive"`) {
		t.Errorf("observe under COLUMNS=80 = %q, want the agent alive", out)
	}
	req := `{"object":{"id":"4194001","created":"Mon Nov 2 00:00:00 2026"},"mode":"zombie"}`
	if out, code := f.run(req, "kill-object", "mc-c"); code != 0 || out != "changed" || strings.Contains(f.calls(), "quit") {
		t.Errorf("zombie kill under COLUMNS=80 = %q (%d), calls %q; want changed and no quit", out, code, f.calls())
	}
}

// Kills (round 3 SHOULD) a city key without the host: two hosts sharing a
// home and a city path would list each other's sockets, which screen reads
// as dead and a stop or start wipes.
func TestScreenScriptKeysStateByHost(t *testing.T) {
	f := newFakeBackend(t)
	f.file("host", "host-a")
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-a"); code != 0 {
		t.Fatal("start on host-a failed")
	}
	f.file("host", "host-b")
	if out, _ := f.run("", "observe", "mc-a"); !strings.Contains(out, `"absent"`) {
		t.Errorf("observe on host-b of host-a's box = %q, want absent", out)
	}
	if dirs, _ := filepath.Glob(filepath.Join(f.state, "*.meta")); len(dirs) != 2 {
		t.Errorf("city metadata directories for two hosts = %v, want two", dirs)
	}
}

// Kills (round 3 NIT) a legacy start refused by the dead boxes a reboot
// leaves (state now persists): it wipes a name only dead boxes hold and
// starts once they are gone; a live box still refuses it, and fresh_only
// still answers 73.
func TestScreenScriptStartsOverADeadBox(t *testing.T) {
	f := newFakeBackend(t)
	f.rows("4194001.mc-r\tDead ???")
	if _, code := f.run(`{"command":"sleep 300","fresh_only":true}`, "start", "mc-r"); code != 73 {
		t.Fatalf("fresh_only start over a dead box exited %d, want 73", code)
	}
	if strings.Contains(f.calls(), "-wipe") {
		t.Fatalf("a refused fresh_only start wiped: %q", f.calls())
	}
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-r"); code != 0 {
		t.Fatal("legacy start over a dead box failed")
	}
	if calls := f.calls(); !strings.Contains(calls, "-wipe 4194001.mc-r") || !strings.Contains(calls, "-dmS mc-r") {
		t.Fatalf("legacy start over a dead box: calls %q, want the wipe then the start", calls)
	}
	// A dead box that stays listed after the wipe still holds the name.
	f.rows("4194001.mc-s\tDead ???")
	f.file("sticky", "")
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-s"); code == 0 || strings.Contains(f.calls(), "-dmS mc-s") {
		t.Fatalf("legacy start over a dead box the wipe left succeeded: %q", f.calls())
	}
	if err := os.Remove(filepath.Join(f.dir, "sticky")); err != nil {
		t.Fatal(err)
	}
	f.rows("4194001.mc-l\tDead ???", "4194002.mc-l\tDetached")
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-l"); code == 0 {
		t.Fatal("legacy start over a live box succeeded")
	}
	if strings.Contains(f.calls(), "mc-l") {
		t.Fatalf("a refused start touched mc-l: %q", f.calls())
	}
}

// Kills (round 4 SHOULD) a dead box read free while its agent runs: an agent
// that ignores the hangup outlives its SCREEN. The window process recorded
// at start (pid and start time) keeps the box unknown to observe and
// inventory, a corpse kill answers live, a legacy start refuses, and stop
// ends the agent; a reused pid with another start time does not count.
func TestScreenScriptDeadBoxWithALiveAgent(t *testing.T) {
	f := newFakeBackend(t)
	f.procFile("sys/kernel/random/boot_id", "boot-a\n")
	f.file("ps.procs", "4194998 4194999 claude /usr/bin/claude\n")
	if _, code := f.run(`{"command":"claude"}`, "start", "mc-o"); code != 0 {
		t.Fatal("start failed")
	}
	f.rows("4194999.mc-o\tDead ???")
	if out, _ := f.run("", "observe", "mc-o"); !strings.Contains(out, `"unknown"`) {
		t.Errorf("observe of a dead box whose agent runs = %q, want unknown", out)
	}
	if out, _ := f.run("", "inventory"); !strings.Contains(out, `{"name":"mc-o","state":"unknown"}`) {
		t.Errorf("inventory with a dead box whose agent runs = %q, want it unknown", out)
	}
	req := `{"object":{"id":"4194999","created":"Mon Nov 2 00:00:00 2026"},"mode":"corpse"}`
	if out, code := f.run(req, "kill-object", "mc-o"); code != 0 || out != "live" {
		t.Errorf("corpse kill of a dead box whose agent runs = %q (%d), want live", out, code)
	}
	if _, code := f.run(`{"command":"claude"}`, "start", "mc-o"); code == 0 {
		t.Error("legacy start over a dead box whose agent runs succeeded")
	}
	if calls := f.calls(); strings.Contains(calls, "-wipe") || strings.Count(calls, "-dmS") != 1 {
		t.Fatalf("a dead box whose agent runs was wiped or started over: %q", calls)
	}

	// The same pid started at another time is another process.
	f.file("lstart.4194998", "Tue Nov  3 00:00:00 2026\n")
	if out, _ := f.run("", "observe", "mc-o"); !strings.Contains(out, `"dead"`) {
		t.Errorf("observe of a dead box whose pid was reused = %q, want dead", out)
	}
	if err := os.Remove(filepath.Join(f.dir, "lstart.4194998")); err != nil {
		t.Fatal(err)
	}

	if _, code := f.run("", "stop", "mc-o"); code != 0 || !strings.Contains(f.calls(), "kill -s TERM -- 4194998") {
		t.Fatalf("stop of a dead box whose agent runs: exit %d, calls %q; want the agent ended", code, f.calls())
	}

	// After a reboot the record names no process: a pid with the same
	// boot-relative start time is a bystander, neither alive nor killed.
	f.file("ps.procs", "4194997 4194999 claude /usr/bin/claude\n")
	if _, code := f.run(`{"command":"claude"}`, "start", "mc-p"); code != 0 {
		t.Fatal("start mc-p failed")
	}
	f.rows("4194999.mc-p\tDead ???")
	f.procFile("sys/kernel/random/boot_id", "boot-b\n")
	f.file("calls", "")
	if out, _ := f.run("", "observe", "mc-p"); !strings.Contains(out, `"dead"`) {
		t.Errorf("observe after a reboot = %q, want dead", out)
	}
	if _, code := f.run("", "stop", "mc-p"); code != 0 || strings.Contains(f.calls(), "kill") || !strings.Contains(f.calls(), "-wipe 4194999.mc-p") {
		t.Fatalf("stop after a reboot: exit %d, calls %q; want the dead box wiped and no kill", code, f.calls())
	}
}

// Kills (adoption review MUST) a name read free while the agent of its last
// box runs with no box left: an agent that ignores the hangup outlives a
// SCREEN that quit or was killed, whose socket is gone. The window process
// recorded at start keeps the name unknown to observe and listed unknown in
// a complete inventory; a fresh_only start answers 73 and a legacy start
// refuses, neither touching the record or the identity; and stop ends the
// agent. Once that process is gone, or its pid is another process's, the
// name is absent and free.
func TestScreenScriptAgentOutlivesItsBox(t *testing.T) {
	f := newFakeBackend(t)
	f.procFile("sys/kernel/random/boot_id", "boot-a\n")
	f.file("ps.procs", "4194998 4194999 claude /usr/bin/claude\n")
	start := `{"command":"claude","env":{"GC_SESSION_ID":"sid-g","GC_INSTANCE_TOKEN":"tok-1"}}`
	if _, code := f.run(start, "start", "mc-g"); code != 0 {
		t.Fatal("start failed")
	}
	f.rows() // the box is gone; its agent runs on
	if out, _ := f.run("claude\n", "observe", "mc-g"); !strings.Contains(out, `"state":"unknown"`) {
		t.Errorf("observe of a name whose agent outlived its box = %q, want unknown", out)
	}
	if out, _ := f.run("", "inventory"); out != `{"complete":true,"sessions":[{"name":"mc-g","state":"unknown"}]}` {
		t.Errorf("inventory with an agent that outlived its box = %q, want the name listed unknown", out)
	}
	fresh := `{"command":"claude","fresh_only":true,"env":{"GC_SESSION_ID":"sid-g","GC_INSTANCE_TOKEN":"tok-2"}}`
	if _, code := f.run(fresh, "start", "mc-g"); code != 73 {
		t.Errorf("fresh_only start over an agent that outlived its box exited %d, want 73", code)
	}
	if _, code := f.run(strings.Replace(start, "tok-1", "tok-3", 1), "start", "mc-g"); code == 0 {
		t.Error("legacy start over an agent that outlived its box succeeded")
	}
	if calls := f.calls(); strings.Count(calls, "-dmS") != 1 || strings.Contains(calls, "kill") {
		t.Fatalf("a refused start started a box or killed: %q", calls)
	}
	if out, _ := f.run("", "get-meta", "mc-g", "GC_INSTANCE_TOKEN"); out != "tok-1" {
		t.Errorf("token after the refused starts = %q, want the running agent's tok-1", out)
	}
	if out, _ := f.run("claude\n", "observe", "mc-g"); !strings.Contains(out, `"state":"unknown"`) {
		t.Fatalf("observe after the refused starts = %q, want the record kept (unknown)", out)
	}
	if _, code := f.run("", "stop", "mc-g"); code != 0 || !strings.Contains(f.calls(), "kill -s TERM -- 4194998") {
		t.Fatalf("stop of an agent that outlived its box: exit %d, calls %q; want the agent ended", code, f.calls())
	}
	if out, _ := f.run("", "observe", "mc-g"); out != `{"state":"absent"}` {
		t.Errorf("observe after the stop = %q, want absent", out)
	}

	// The record of a window whose pid now runs another process (another
	// start time) holds nothing.
	f.file("ps.procs", "4194997 4194999 claude /usr/bin/claude\n")
	if _, code := f.run(start, "start", "mc-r"); code != 0 {
		t.Fatal("start mc-r failed")
	}
	f.rows()
	f.file("lstart.4194997", "Tue Nov  3 00:00:00 2026\n")
	if out, _ := f.run("", "observe", "mc-r"); out != `{"state":"absent"}` {
		t.Errorf("observe of a name whose window pid was reused = %q, want absent", out)
	}
	if out, _ := f.run("", "inventory"); out != `{"complete":true,"sessions":[]}` {
		t.Errorf("inventory with a reused window pid = %q, want complete and empty", out)
	}
	if _, code := f.run(`{"command":"claude","fresh_only":true}`, "start", "mc-r"); code != 0 {
		t.Errorf("fresh_only start over a reused window pid exited %d, want 0", code)
	}
}

// Kills (adoption review MUST) a zombie kill that drops the record of a
// window that ignored the hangup, leaving it running: the box quits, the
// window is ended as stop ends it, and only then is the record dropped. A
// window that survives fails the kill and keeps the name unknown.
func TestScreenScriptZombieKillEndsTheWindow(t *testing.T) {
	f := newFakeBackend(t)
	f.file("ps.procs", "4194998 4194999 sh /bin/sh -c x\n")
	zombie := `{"object":{"id":"4194999","created":"Mon Nov 2 00:00:00 2026"},"mode":"zombie"}`
	if _, code := f.run(`{"command":"sh","env":{"GT_PROCESS_NAMES":"claude"}}`, "start", "mc-z"); code != 0 {
		t.Fatal("start failed")
	}
	if out, code := f.run(zombie, "kill-object", "mc-z"); code != 0 || out != "killed" || !strings.Contains(f.calls(), "kill -s TERM -- 4194998") {
		t.Fatalf("zombie kill = %q (%d), calls %q; want killed, its window ended", out, code, f.calls())
	}
	if out, _ := f.run("", "observe", "mc-z"); out != `{"state":"absent"}` {
		t.Errorf("observe after the zombie kill = %q, want absent", out)
	}

	if err := os.Remove(filepath.Join(f.dir, "gone")); err != nil {
		t.Fatal(err)
	}
	if _, code := f.run(`{"command":"sh","env":{"GT_PROCESS_NAMES":"claude"}}`, "start", "mc-y"); code != 0 {
		t.Fatal("start mc-y failed")
	}
	f.file("unkillable", "")
	if out, code := f.run(zombie, "kill-object", "mc-y"); code == 0 {
		t.Fatalf("zombie kill whose window survived = %q, exit 0; want a failure", out)
	}
	if out, _ := f.run("", "observe", "mc-y"); !strings.Contains(out, `"state":"unknown"`) {
		t.Errorf("observe after a zombie kill whose window survived = %q, want unknown (its record kept)", out)
	}
}

// Kills (adoption review SHOULD) a zombie read alive: a window /proc shows
// in state Z has exited, only its reaping pending, which on a host without
// a reaper never comes. A dead box holding it reads dead, and stop ends the
// box without a kill of the corpse. A command name holding ") " does not
// shift the parse.
func TestScreenScriptZombieWindowIsGone(t *testing.T) {
	f := newFakeBackend(t)
	f.procFile("sys/kernel/random/boot_id", "boot-a\n")
	f.file("ps.procs", "4194998 4194999 claude /usr/bin/claude\n")
	f.procStat("4194998", "a) b", "S", "4242")
	if _, code := f.run(`{"command":"claude"}`, "start", "mc-q"); code != 0 {
		t.Fatal("start failed")
	}
	f.rows("4194999.mc-q\tDead ???")
	if out, _ := f.run("", "observe", "mc-q"); !strings.Contains(out, `"state":"unknown"`) {
		t.Errorf("observe of a dead box whose window runs (/proc) = %q, want unknown", out)
	}
	f.procStat("4194998", "a) b", "S", "4343")
	if out, _ := f.run("", "observe", "mc-q"); !strings.Contains(out, `"state":"dead"`) {
		t.Errorf("observe of a dead box whose window pid runs another process (/proc) = %q, want dead", out)
	}
	f.procStat("4194998", "a) b", "Z", "4242")
	if out, _ := f.run("", "observe", "mc-q"); !strings.Contains(out, `"state":"dead"`) {
		t.Errorf("observe of a dead box whose window is a zombie = %q, want dead", out)
	}
	if _, code := f.run("", "stop", "mc-q"); code != 0 || strings.Contains(f.calls(), "kill") {
		t.Fatalf("stop of a dead box whose window is a zombie: exit %d, calls %q; want it stopped, no kill", code, f.calls())
	}
	if out, _ := f.run("", "observe", "mc-q"); out != `{"state":"absent"}` {
		t.Errorf("observe after the stop = %q, want absent", out)
	}
}

// Kills (adoption review MUST) a default layout that leaves no room for
// gc's pool session names, <template>-gcg-session-<32 hex>: with no
// GC_EXEC_STATE_DIR and a home as long as /home/ubuntu (12 bytes), names up
// to 71 bytes (83 less the home's 12) start, among them 67-byte
// adopt-pr-review-claude and 70-byte gc__implementation-worker pool names,
// and a name one byte longer is refused before screen runs.
func TestScreenScriptDefaultLayoutFitsPoolSessionNames(t *testing.T) {
	f := newFakeBackend(t)
	home := tmpDirOfLength(t, len("/home/ubuntu"))
	f.file("home", home)
	f.env = []string{f.env[0], "GC_EXEC_CITY_PATH=/fake-city"} // PATH, no GC_EXEC_STATE_DIR
	hex := strings.Repeat("0123456789abcdef", 2)
	for _, name := range []string{
		"claude-gcg-session-" + hex,
		"adopt-pr-review-claude-gcg-session-" + hex,
		"gc__implementation-worker-gcg-session-" + hex,
		strings.Repeat("n", 71),
	} {
		if _, code := f.run(`{"command":"sleep 300"}`, "start", name); code != 0 || !strings.Contains(f.calls(), "-dmS "+name+" ") {
			t.Errorf("start of a %d-byte name under a 12-byte home: exit %d, calls %q; want it started", len(name), code, f.calls())
		}
	}
	long := strings.Repeat("n", 72)
	if _, code := f.run(`{"command":"sleep 300"}`, "start", long); code == 0 || strings.Contains(f.calls(), "-dmS "+long+" ") {
		t.Errorf("start of a 72-byte name under a 12-byte home: exit %d, calls %q; want it refused before screen", code, f.calls())
	}
	if _, err := os.Stat(filepath.Join(home, ".gcss")); err != nil {
		t.Fatalf("state is not under the home's .gcss: %v", err)
	}
}

// tmpDirOfLength makes a new directory whose path is n bytes, n at least 7:
// /tmp and a random name.
func tmpDirOfLength(t *testing.T, n int) string {
	t.Helper()
	for range 10 {
		dir := "/tmp/" + strings.ToLower(rand.Text())[:n-len("/tmp/")]
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
		if !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	t.Fatalf("no free %d-byte directory under /tmp", n)
	return ""
}

// Kills (F-1, PR-3b) a peek that answers what it cannot read as an empty
// screen: no box and a dead box answer exit 66, no screen; a listing that
// cannot be read, a session screen cannot read and a failed hardcopy fail;
// a running box's screen is the exact session's hardcopy.
func TestScreenScriptPeekAnswersNoScreen(t *testing.T) {
	f := newFakeBackend(t)
	f.rows("4194001.mc-a\tDetached", "4194002.mc-d\tDead ???", "4194003.mc-p\tPrivate")
	for name, want := range map[string]int{"mc-none": 66, "mc-d": 66, "mc-p": 1, "mc-a": 0} {
		if _, code := f.run("", "peek", name, "10"); code != want {
			t.Errorf("peek %s: exit %d, want %d", name, code, want)
		}
	}
	if calls := f.calls(); strings.Count(calls, "hardcopy") != 1 || !strings.Contains(calls, "-S 4194001.mc-a -X hardcopy -h ") {
		t.Errorf("screen calls %q, want one hardcopy of 4194001.mc-a", calls)
	}
	if out, code := f.run("", "peek", "mc-a", "10"); code != 0 || !strings.Contains(out, "screen of 4194001.mc-a") {
		t.Errorf("peek of a running box = %q (%d), want its capture", out, code)
	}
	// A capture SCREEN is still writing is read once its size settles, never
	// as its partial head.
	f.file("hardcopy.slow", "")
	if out, code := f.run("", "peek", "mc-a", "10"); code != 0 || !strings.Contains(out, "head of") || !strings.Contains(out, "tail of the capture") {
		t.Errorf("peek of a capture written in two parts = %q (%d), want both parts", out, code)
	}
	if err := os.Remove(filepath.Join(f.dir, "hardcopy.slow")); err != nil {
		t.Fatal(err)
	}
	// screen exits 0 for a hardcopy SCREEN never writes: the empty capture
	// is a failure, never an empty screen.
	f.file("hardcopy.fail", "")
	if out, code := f.run("", "peek", "mc-a", "10"); code == 0 || code == 66 {
		t.Errorf("a hardcopy that wrote nothing: %q, exit %d; want a failure", out, code)
	}
	f.file("ls.fail", "")
	if _, code := f.run("", "peek", "mc-a", "10"); code == 0 || code == 66 {
		t.Errorf("an unreadable listing: exit %d, want a failure", code)
	}
}

// Kills (PR-3a audit MUST) an inventory that slows without bound: each
// name's last window record is read on every call, and a pool session name
// is never reused, so records pile up. With procfs mounted, a recorded pid
// with no /proc entry is gone with no fork (no ps per record), and a gone
// record whose name no box lists is forgotten, its lock kept: with 60 stale
// records the first inventory asks ps nothing and forgets them all, so the
// next reads none. A record whose window still runs stays listed unknown.
func TestScreenScriptInventoryForgetsStaleRecords(t *testing.T) {
	f := newFakeBackend(t)
	f.procFile("self/stat", "1 (self) S 0\n") // procfs is mounted
	f.procFile("sys/kernel/random/boot_id", "boot-a\n")
	f.run("", "inventory") // the state directories
	meta := f.metaDir()
	for i := range 60 {
		name := fmt.Sprintf("worker-gcg-session-%032d", i)
		f.file(filepath.Join(meta, name+".window"), fmt.Sprintf("boot-a %d 12345", 4100000+i))
		f.file(filepath.Join(meta, name+".wrapper.sh"), "export GC_INSTANCE_TOKEN=tok\n")
		f.file(filepath.Join(meta, name+".lock"), "")
	}
	f.procFile("4194990/stat", "4194990 (claude) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0 0\n")
	f.file(filepath.Join(meta, "mc-live.window"), "boot-a 4194990 777")
	// A stale name whose session gc acked a drain: gc reads the ack after
	// the box is gone, so the keys gc set outlive the start's record.
	f.file(filepath.Join(meta, "mc-acked.window"), "boot-a 4100100 12345")
	f.file(filepath.Join(meta, "mc-acked.meta.GC_INSTANCE_TOKEN"), "tok")
	f.file(filepath.Join(meta, "mc-acked.meta.GC_DRAIN_ACK"), "1")
	if out, code := f.run("", "inventory"); code != 0 || out != `{"complete":true,"sessions":[{"name":"mc-live","state":"unknown"}]}` {
		t.Fatalf("inventory = %q (%d), want only the live window's name, unknown", out, code)
	}
	if out, _ := f.run("", "get-meta", "mc-acked", "GC_DRAIN_ACK"); out != "1" {
		t.Fatalf("drain ack after the stale record was forgotten = %q, want 1", out)
	}
	for _, gone := range []string{"mc-acked.window", "mc-acked.meta.GC_INSTANCE_TOKEN"} {
		if _, err := os.Stat(filepath.Join(meta, gone)); !os.IsNotExist(err) {
			t.Fatalf("the stale record's %s was kept (%v)", gone, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.dir, "ps.calls")); strings.Contains(string(data), "lstart") {
		t.Fatalf("inventory asked ps for a start time with procfs mounted: %q", data)
	}
	left, _ := filepath.Glob(filepath.Join(meta, "worker-gcg-session-*"))
	for _, p := range left {
		if !strings.HasSuffix(p, ".lock") {
			t.Fatalf("a stale record's metadata %s was kept", p)
		}
	}
	if len(left) != 60 {
		t.Fatalf("%d lock files left, want the 60 locks kept", len(left))
	}
	if _, err := os.Stat(filepath.Join(meta, "mc-live.window")); err != nil {
		t.Fatalf("the live window's record was forgotten: %v", err)
	}

	// An empty record proves nothing (a start may have removed the record
	// the inventory read, and be seeding the name now): nothing is forgotten.
	f.file(filepath.Join(meta, "mc-new.window"), "")
	f.file(filepath.Join(meta, "mc-new.meta.GC_SESSION_ID"), "sid-new")
	f.run("", "inventory")
	if _, err := os.Stat(filepath.Join(meta, "mc-new.meta.GC_SESSION_ID")); err != nil {
		t.Fatalf("an empty record's name lost its metadata: %v", err)
	}
}

// Kills (PR-3b adoption review) a cleanup that acts on inventory's listing
// once it is stale: a start can run between the listing and the cleanup,
// and its box die with its window. The name's window record is then
// unchanged under the lock and names a process that is gone, but the dead
// box holds the name, and forgetting its box record would leave that box
// unknown for good. Under the lock the name is listed again: a row keeps
// the record, so the dead box keeps its object and identity, and the next
// inventory lists it dead.
func TestScreenScriptInventoryKeepsABoxStartedSinceItsListing(t *testing.T) {
	f := newFakeBackend(t)
	f.file("ps.procs", "4194998 4194999 claude /usr/bin/claude\n")
	// Once inventory has read its listing, mc-x starts, and then its box and
	// window die.
	f.file("start.json", `{"command":"claude","env":{"GC_SESSION_ID":"sid-x"}}`)
	f.file("ls.then", fmt.Sprintf(`%q start mc-x < %q || echo "exit $?" > %q
printf '\t4194999.mc-x\t(11/02/2026 01:00:00 PM)\t(Dead ???)\n' > %q
: > "$SCREENDIR/fake-ls"
echo 4194998 >> %q
`, f.script, filepath.Join(f.dir, "start.json"), filepath.Join(f.dir, "ls.then.err"), filepath.Join(f.dir, "ls"), filepath.Join(f.dir, "gone")))
	if out, code := f.run("", "inventory"); code != 0 || out != `{"complete":true,"sessions":[]}` {
		t.Fatalf("inventory over a listing read before mc-x started = %q (%d), want its listing's answer", out, code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "ls.then.ran")); err != nil {
		t.Fatalf("mc-x never started between the listing and the cleanup: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(f.dir, "ls.then.err")); err == nil {
		t.Fatalf("mc-x's start failed: %s", data)
	}
	var o struct {
		State  string
		Object struct{ ID, Created string }
	}
	if out, _ := f.run("", "observe", "mc-x"); json.Unmarshal([]byte(out), &o) != nil || o.State != "dead" || o.Object.ID != "4194999" || o.Object.Created == "" {
		t.Fatalf("observe of the dead box = %q, want dead with its recorded object", out)
	}
	if out, _ := f.run("", "get-meta", "mc-x", "GC_SESSION_ID"); out != "sid-x" {
		t.Fatalf("identity of the dead box = %q, want sid-x kept", out)
	}
	if out, _ := f.run("", "inventory"); !strings.Contains(out, `{"name":"mc-x","state":"dead","object":{"id":"4194999"`) {
		t.Fatalf("next inventory = %q, want mc-x listed dead with its object", out)
	}
}
