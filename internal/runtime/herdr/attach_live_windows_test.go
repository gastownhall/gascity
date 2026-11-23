//go:build windows

package herdr

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestAttachShowsTheSessionLive pins Windows attach against a real herdr. The
// command attach runs, hosted in a pseudo-console the test owns, must draw the
// session's own screen and no other session's, and closing it must end no
// session. Two sessions share a workspace in tabs of their own and a third has
// a workspace to itself, and attach visits them so that focus moves every time,
// within a workspace and across one.
// Opt-in live tier: see requireLiveHerdr.
func TestAttachShowsTheSessionLive(t *testing.T) {
	requireLiveHerdr(t)

	// Each marker is computed, so neither a typed line nor a label supplies it.
	sessions := []struct{ name, command, marker string }{
		{name: "town__alpha", command: `"attach-alpha-$(6*7)"; & ping.exe -n 120 127.0.0.1 | Out-Null`, marker: "attach-alpha-42"},
		{name: "town__beta", command: `"attach-beta-$(7*8)"; & ping.exe -n 120 127.0.0.1 | Out-Null`, marker: "attach-beta-56"},
		{name: "rig1--town__gamma", command: `"attach-gamma-$(8*9)"; & ping.exe -n 120 127.0.0.1 | Out-Null`, marker: "attach-gamma-72"},
	}
	p := New("gctest-attach", t.TempDir(), t.TempDir(), 0, 0)
	for _, s := range sessions {
		_ = p.Stop(s.name) // clear any leftover from a crashed prior run
	}
	t.Cleanup(func() {
		for _, s := range sessions {
			_ = p.Stop(s.name)
		}
		_ = p.TeardownServer()
	})

	ctx := context.Background()
	for _, s := range sessions {
		if err := p.Start(ctx, s.name, runtime.Config{WorkDir: t.TempDir(), Command: s.command}); err != nil {
			t.Fatalf("Start %s: %v", s.name, err)
		}
		var screen string
		for i := 0; i < 40 && !strings.Contains(screen, s.marker); i++ {
			time.Sleep(250 * time.Millisecond)
			screen, _ = p.Peek(s.name, 30)
		}
		if !strings.Contains(screen, s.marker) {
			t.Fatalf("%s never showed %s; screen %q", s.name, s.marker, screen)
		}
	}

	for _, i := range []int{0, 1, 2, 0} {
		target := sessions[i]
		cmd, err := p.attachCommand(ctx, target.name)
		if err != nil {
			t.Fatalf("attachCommand %s: %v", target.name, err)
		}
		screen := pseudoConsoleScreen(t, cmd, target.marker, 20*time.Second)
		if !strings.Contains(screen, target.marker) {
			t.Errorf("attaching to %s drew no %s; the client showed %q", target.name, target.marker, lastRunes(screen, 600))
		}
		for _, other := range sessions {
			if other.name != target.name && strings.Contains(screen, other.marker) {
				t.Errorf("attaching to %s drew %s's %s", target.name, other.name, other.marker)
			}
		}
	}
	for _, s := range sessions {
		if !p.IsRunning(s.name) {
			t.Errorf("%s stopped running once attach's clients were closed", s.name)
		}
	}
}

// procUpdateProcThreadAttribute is called directly because the pseudo-console
// attribute's value is the console handle itself rather than its address, and
// x/sys takes that value as an unsafe.Pointer: a handle held as a pointer is an
// invalid pointer to the Go runtime.
var procUpdateProcThreadAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

// pseudoConsoleScreen runs cmd in a 160x48 pseudo-console the test owns and
// returns what it drew, escape sequences removed, once want appears or within
// has passed. The command has ended by the time it returns.
func pseudoConsoleScreen(t *testing.T, cmd *exec.Cmd, want string, within time.Duration) string {
	t.Helper()
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		t.Fatalf("CreatePipe: %v", err)
	}
	defer windows.CloseHandle(inWrite) //nolint:errcheck
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead) //nolint:errcheck
		t.Fatalf("CreatePipe: %v", err)
	}
	var console windows.Handle
	err := windows.CreatePseudoConsole(windows.Coord{X: 160, Y: 48}, inRead, outWrite, 0, &console)
	windows.CloseHandle(inRead)   //nolint:errcheck
	windows.CloseHandle(outWrite) //nolint:errcheck
	if err != nil {
		windows.CloseHandle(outRead) //nolint:errcheck
		t.Fatalf("CreatePseudoConsole: %v", err)
	}

	var mu sync.Mutex
	var drawn strings.Builder
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			var n uint32
			if err := windows.ReadFile(outRead, buf, &n, nil); err != nil || n == 0 {
				return
			}
			mu.Lock()
			drawn.Write(buf[:n])
			mu.Unlock()
		}
	}()
	screen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return escapeSequence.ReplaceAllString(drawn.String(), "")
	}

	process := startInPseudoConsole(t, cmd, console)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) && !strings.Contains(screen(), want) {
		time.Sleep(250 * time.Millisecond)
	}
	got := screen()

	// Closing the console ends the client; the reader drains until it does.
	windows.ClosePseudoConsole(console)
	if event, _ := windows.WaitForSingleObject(process, 5000); event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(process, 1)
		_, _ = windows.WaitForSingleObject(process, 5000)
	}
	windows.CloseHandle(process) //nolint:errcheck
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Error("the pseudo-console's output never closed after its client ended")
	}
	windows.CloseHandle(outRead) //nolint:errcheck
	return got
}

// startInPseudoConsole starts cmd attached to console and returns its process
// handle.
func startInPseudoConsole(t *testing.T, cmd *exec.Cmd, console windows.Handle) windows.Handle {
	t.Helper()
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatalf("NewProcThreadAttributeList: %v", err)
	}
	defer attrs.Delete()
	if r, _, err := procUpdateProcThreadAttribute.Call(uintptr(unsafe.Pointer(attrs.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0); r == 0 {
		t.Fatalf("UpdateProcThreadAttribute: %v", err)
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Null standard handles, flagged as given, make the client use the
	// pseudo-console's. Without the flag it wrote to this process's redirected
	// output instead (measured: its screen landed in the test's own output and
	// the pseudo-console drew nothing).
	si.Flags |= windows.STARTF_USESTDHANDLES
	app, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		t.Fatal(err)
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		t.Fatal(err)
	}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(app, line, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &si.StartupInfo, &pi); err != nil {
		t.Fatalf("starting %q in a pseudo-console: %v", cmd.Args, err)
	}
	windows.CloseHandle(pi.Thread) //nolint:errcheck
	return pi.Process
}

// escapeSequence matches the terminal escape sequences a client draws with:
// CSI, OSC, character-set selection and the single-character escapes.
var escapeSequence = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[()][0-9A-Za-z]|[=>78])`)

// lastRunes is the end of s, at most n runes of it.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
