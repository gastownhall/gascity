package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file drives gc-session-screen against a fake screen and ps, so its
// decisions over screen's statuses, an unreadable listing and the process
// table are tested deterministically, without a terminal or real agents.

// fakeBackend is a directory of fake screen and ps executables. screen -ls
// prints the file ls (or fails when ls.fail exists) and logs every other
// invocation to calls; ps prints ps.args / ps.comm for the snapshots (or
// fails them when ps.fail exists) and a fixed start time for -o lstart=.
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
	write("screen", `if [ "$1" = -ls ]; then
  if [ -f "$F/ls.fail" ]; then echo "Cannot open your terminal"; exit 1; fi
  if [ -s "$F/ls" ]; then printf 'There are screens on:\n'; cat "$F/ls"; printf '%s Sockets in /x.\n' 1; exit 1; fi
  echo "No Sockets found in /x."; exit 1
fi
printf '%s\n' "$*" >> "$F/calls"
# -dmS creates a session; -wipe and -X quit remove one, unless sticky.
case "$1" in
  -dmS) [ -f "$F/nobox" ] || printf '\t4194999.%s\t(11/02/2026 01:00:00 PM)\t(Detached)\n' "$2" >> "$F/ls" ;;
  -wipe | -S)
    if [ "$1" = -wipe ] || [ "$4" = quit ]; then
      [ -f "$F/sticky" ] || { grep -v -F "$(printf '\t%s\t' "$2")" "$F/ls" > "$F/ls.new" || true; mv "$F/ls.new" "$F/ls"; }
    fi ;;
esac
`)
	write("ps", `case "$*" in
  *lstart*) echo "Mon Nov  2 00:00:00 2026" ;;
  *args=*) [ -f "$F/ps.fail" ] && exit 1; cat "$F/ps.args" 2>/dev/null ;;
  *comm=*) [ -f "$F/ps.fail" ] && exit 1; cat "$F/ps.comm" 2>/dev/null ;;
esac
`)
	// A short state directory, so the socket-path guard admits ordinary
	// names.
	state, err := os.MkdirTemp("", "gcf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	return &fakeBackend{t: t, dir: dir, script: screenReferencePath(t), state: state, env: []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"GC_EXEC_STATE_DIR=" + state,
		"GC_EXEC_CITY_PATH=/fake-city",
	}}
}

// rows sets screen -ls to the given "<pid>.<name>\t<status>" rows.
func (f *fakeBackend) rows(rows ...string) {
	var b strings.Builder
	for _, r := range rows {
		pidName, status, _ := strings.Cut(r, "\t")
		fmt.Fprintf(&b, "\t%s\t(11/02/2026 01:00:00 PM)\t(%s)\n", pidName, status)
	}
	f.file("ls", b.String())
}

func (f *fakeBackend) file(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
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
	f.file("ps.args", "4194101 4194001 /bin/sh /x/wrapper.sh\n")
	f.file("ps.comm", "4194101 sh\n")
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
	for _, tc := range []struct {
		name, args, comm, names, want string
	}{
		{"node 25 comm", "4194102 4194101 node /usr/local/bin/gemini --yolo", "4194102 node-MainThread", "gemini", "alive"},
		{"interpreter script with an extension", "4194102 4194101 node /opt/copilot/index.js", "4194102 node", "index", "alive"},
		{"runner subcommand", "4194102 4194101 npx run codex", "4194102 npx", "codex", "alive"},
		{"long name truncated in comm", "4194102 4194101 /opt/bin/very-long-agent-name --x", "4194102 very-long-agent", "very-long-agent-name", "alive"},
		{"grandchild", "4194102 4194101 /usr/bin/claude", "4194102 claude", "claude", "alive"},
		{"not running", "4194102 4194101 /usr/bin/vim", "4194102 vim", "claude", "dead"},
		{"another box's agent", "4194102 1 /usr/bin/claude", "4194102 claude", "claude", "dead"},
	} {
		f.file("ps.args", "4194101 4194001 /bin/sh /x/wrapper.sh\n"+tc.args+"\n")
		f.file("ps.comm", "4194101 sh\n"+tc.comm+"\n")
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
	f.file("nobox", "")
	if _, code := f.run(`{"command":"sleep 300","env":{"GC_SESSION_ID":"sid-n"}}`, "start", "mc-n"); code == 0 {
		t.Fatal("start succeeded although screen started no session")
	}
	if out, _ := f.run("", "get-meta", "mc-n", "GC_SESSION_ID"); out != "" {
		t.Fatalf("identity of a start that created no box = %q, want none", out)
	}
	if _, code := f.run(`{"command":"sleep 300"}`, "start", "mc-ok"); code == 0 || !strings.Contains(f.calls(), "-dmS mc-ok") {
		t.Fatalf("a start with an ordinary name did not reach screen: %q", f.calls())
	}
}

// metaDir is the fake city's metadata directory.
func (f *fakeBackend) metaDir() string {
	f.t.Helper()
	dirs, err := filepath.Glob(filepath.Join(f.state, "city-*", "meta"))
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
// and GC_CITY_PATH is kept with the identity for get-meta.
func TestScreenScriptIdentityFromTheWrapper(t *testing.T) {
	f := newFakeBackend(t)
	if _, code := f.run(`{"command":"sleep 300","env":{"GC_SESSION_ID":"sid-w","GC_INSTANCE_TOKEN":"tok w'q","GC_CITY_PATH":"/cities/w"}}`, "start", "mc-w"); code != 0 {
		t.Fatal("start failed")
	}
	if out, _ := f.run("", "get-meta", "mc-w", "GC_CITY_PATH"); out != "/cities/w" {
		t.Errorf("get-meta GC_CITY_PATH = %q, want /cities/w", out)
	}
	meta := f.metaDir()
	sidecars, _ := filepath.Glob(filepath.Join(meta, "mc-w.meta.*"))
	for _, p := range append(sidecars, filepath.Join(meta, "mc-w.identity")) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := f.run("", "observe", "mc-w")
	var o struct{ Identity map[string]string }
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Identity["GC_SESSION_ID"] != "sid-w" || o.Identity["GC_INSTANCE_TOKEN"] != "tok w'q" {
		t.Fatalf("observe without the sidecar = %q (%v), want the wrapper's identity", out, err)
	}
}
