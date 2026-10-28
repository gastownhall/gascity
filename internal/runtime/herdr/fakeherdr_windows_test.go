//go:build windows

package herdr

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// fakeHerdrSpecFile is the spec a fake herdr reads from beside itself.
const fakeHerdrSpecFile = "fakeherdr.json"

// Run as herdr.exe, this test binary is a fake herdr: it does what the spec
// beside it says with its arguments and exits, before any test runs. The test
// binary itself is never named herdr.exe, so the name alone decides. Run with
// argvEchoEnv set, it prints its arguments and exits instead.
//
// Run as the test binary, it routes pane-shell child listings and their
// command lines for a fake herdr's panes to the fake (see shellChildren and
// childCommandLine) and every other herdr's to the real process table.
func init() {
	if os.Getenv(argvEchoEnv) == "1" {
		_, _ = os.Stdout.WriteString(asciiJSON(os.Args[1:]) + "\n")
		os.Exit(0)
	}
	if len(os.Args) == 0 || !strings.EqualFold(filepath.Base(os.Args[0]), "herdr.exe") {
		realChildren := paneShellChildren
		paneShellChildren = func(bin string, shellPID int) ([]paneChild, error) {
			if spec, ok := fakeHerdrs.Load(bin); ok {
				return spec.(fakeHerdrSpec).shellChildren(), nil
			}
			return realChildren(bin, shellPID)
		}
		realCommandLine := paneChildCommandLine
		paneChildCommandLine = func(bin string, child proctable.ChildProcess) (string, error) {
			if spec, ok := fakeHerdrs.Load(bin); ok {
				return spec.(fakeHerdrSpec).childCommandLine(child.PID)
			}
			return realCommandLine(bin, child)
		}
		return
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(os.Args[0]), fakeHerdrSpecFile))
	var spec fakeHerdrSpec
	if err == nil {
		err = json.Unmarshal(data, &spec)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "fake herdr: reading its spec: %v\n", err)
		os.Exit(125)
	}
	os.Exit(runFakeHerdr(spec, os.Args[1:], os.Stdout, os.Stderr))
}

// fakeHerdrs maps each live fake herdr's path to its spec.
var fakeHerdrs sync.Map

// shellChildren is what the fake's pane shell is running. A fake pane's
// shell PID is invented, so the real process table knows nothing about it;
// the children follow the same state flags as the fake's process-info reply.
//
// The flag detached_child adds an interpreter the shell started for itself,
// beside the pane rather than in it, as a profile's Start-Job does; the flag's
// contents name its image (powershell.exe when empty). launch_pending hides
// the typed launch's interpreter, as while it is still starting.
func (s fakeHerdrSpec) shellChildren() []paneChild {
	if s.Mode != "provider" {
		return nil
	}
	has := func(flag string) bool { return fakeFileExists(filepath.Join(s.State, flag)) }
	if has("pane_gone") {
		return nil
	}
	var children []paneChild
	if has("detached_child") {
		image, _ := os.ReadFile(filepath.Join(s.State, "detached_child"))
		if len(image) == 0 {
			image = []byte("powershell.exe")
		}
		children = append(children, paneChild{ChildProcess: proctable.ChildProcess{PID: fakeDetachedChildPID, Name: string(image), Created: time.Now()}})
	}
	switch {
	case has("rawinterp") && !has("launch_pending"):
		interpreter, _ := os.ReadFile(filepath.Join(s.State, "rawinterp"))
		children = append(children, paneChild{ChildProcess: proctable.ChildProcess{PID: fakeLaunchChildPID, Name: filepath.Base(string(interpreter)), Created: time.Now()}, Attached: true})
	case has("busy") && !has("rawinterp"):
		children = append(children, paneChild{ChildProcess: proctable.ChildProcess{PID: fakeLaunchChildPID, Name: "claude.exe", Created: time.Now()}, Attached: true})
	}
	return children
}

// The PIDs the fake gives its pane shell's children.
const (
	fakeLaunchChildPID   = 4243
	fakeDetachedChildPID = 4250
)

// childCommandLine is the command line of one of shellChildren's children.
// The typed launch's interpreter carries the statement's encoded command, as a
// real one does; the detached child's is a background job's.
func (s fakeHerdrSpec) childCommandLine(pid int) (string, error) {
	read := func(flag string) string {
		data, _ := os.ReadFile(filepath.Join(s.State, flag))
		return string(data)
	}
	switch pid {
	case fakeLaunchChildPID:
		if interpreter := read("rawinterp"); interpreter != "" {
			return `"` + interpreter + `" -NoProfile -EncodedCommand ` + read("rawtoken"), nil
		}
		return "claude.exe", nil
	case fakeDetachedChildPID:
		return `"C:\WINDOWS\System32\WindowsPowerShell\v1.0\powershell.exe" -Version 5.1 -s -NoLogo -NoProfile`, nil
	}
	return "", proctable.ErrProcessGone
}

// newGoFakeHerdr links this test binary into a temp dir as herdr.exe, with spec
// beside it, and returns the path to run it by. A copy is the fallback where
// the temp dir is on another volume.
//
// The link names the running test binary's own image, which the temp dir's
// RemoveAll cannot unlink (it reports access denied); os.Remove, through
// DeleteFile, can. So the link is removed first, by a cleanup registered after
// the temp dir's and therefore run before it.
func newGoFakeHerdr(t *testing.T, spec fakeHerdrSpec) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "herdr.exe")
	if linkErr := os.Link(self, bin); linkErr != nil {
		if err := copyFakeHerdrBinary(self, bin); err != nil {
			t.Fatalf("placing the fake herdr: linking: %v; copying: %v", linkErr, err)
		}
	}
	t.Cleanup(func() { _ = os.Remove(bin) })
	writeFakeHerdrSpec(t, bin, spec)
	fakeHerdrs.Store(bin, spec)
	t.Cleanup(func() { fakeHerdrs.Delete(bin) })
	return bin
}

func copyFakeHerdrBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeFakeHerdrSpec(t *testing.T, bin string, spec fakeHerdrSpec) {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encoding the fake herdr's spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), fakeHerdrSpecFile), data, 0o644); err != nil {
		t.Fatalf("writing the fake herdr's spec: %v", err)
	}
}

func readFakeHerdrSpec(t *testing.T, bin string) fakeHerdrSpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(bin), fakeHerdrSpecFile))
	if err != nil {
		t.Fatalf("reading the fake herdr's spec: %v", err)
	}
	var spec fakeHerdrSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decoding the fake herdr's spec: %v", err)
	}
	return spec
}

// goFakeHerdrFromScript is writeFakeHerdr on Windows: the fake that gives the
// one-shot script's reply. A script of a shape replySpecForScript does not know
// fails the test rather than running as something else.
func goFakeHerdrFromScript(t *testing.T, script string) string {
	t.Helper()
	spec, ok := replySpecForScript(script)
	if !ok {
		t.Fatalf("this fake herdr script has no Windows equivalent; add its shape to replyScriptShapes:\n%s", script)
	}
	return newGoFakeHerdr(t, spec)
}

// rewriteGoFake is rewriteFake on Windows. The Go fake cannot be patched as
// text, so a rewrite a test makes is a flag in its spec; the only one is the
// workspace list that reports an existing workspace.
func rewriteGoFake(t *testing.T, p *Provider, old, replacement string) {
	t.Helper()
	if !strings.HasPrefix(old, "workspace_list)") || !strings.Contains(replacement, `{"result":{"workspaces":[{"workspace_id":"w1","label":"gastown"}]}}`) {
		t.Fatalf("the Go fake herdr has no equivalent of this rewrite:\n%s\n=>\n%s", old, replacement)
	}
	spec := readFakeHerdrSpec(t, p.c.bin)
	spec.WorkspaceExists = true
	writeFakeHerdrSpec(t, p.c.bin, spec)
	fakeHerdrs.Store(p.c.bin, spec)
}
