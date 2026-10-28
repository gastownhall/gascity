//go:build windows

package herdr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/gastownhall/gascity/internal/runtime"
)

// goroutineCount counts the goroutines a leak check should see: all of them,
// less the ones go-winio creates for itself. Its pipe listener parks a
// goroutine on every pending connect, and its first use starts a process-wide
// completion-port goroutine, where a unix listener waits in the netpoller with
// none. Those belong to the transport. A goroutine this package starts is never
// created by go-winio, even while it blocks inside a go-winio read, so excluding
// them hides no leak of ours. Both counts come from one snapshot, so they agree
// on which goroutines exist.
func goroutineCount() int {
	buf := make([]byte, 64<<10)
	for {
		n := goruntime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	dump := "\n" + string(buf)
	return strings.Count(dump, "\ngoroutine ") - strings.Count(dump, "\ncreated by github.com/Microsoft/go-winio.")
}

// listenHerdrEndpoint serves sock the way herdr does on Windows: on the named
// pipe the path names (herdrPipeName), not at the path itself.
func listenHerdrEndpoint(sock string) (net.Listener, error) {
	name, err := herdrPipeName(sock)
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		return nil, err
	}
	return pipeListener{ln}, nil
}

// pipeListener is a go-winio pipe listener whose Close returns. go-winio
// v0.6.2's Close hands the listener a single stop signal, and a connect that
// completes at that moment can take it and fail with an error the listener does
// not treat as a stop; the listener then waits for another client while Close
// waits for the listener (microsoft/go-winio#388, fixed after v0.6.2). So Close
// signals again until the listener stops. v0.6.2 lets any number of Close calls
// wait together, and all of them return once it has stopped.
type pipeListener struct{ net.Listener }

func (l pipeListener) Close() error {
	stopped := make(chan struct{})
	go func() {
		_ = l.Listener.Close()
		close(stopped)
	}()
	for {
		select {
		case <-stopped:
			return nil
		case <-time.After(100 * time.Millisecond):
			go func() { _ = l.Listener.Close() }()
		}
	}
}

// acceptAndClose answers every connect on ln and drops it, which is all a
// liveness probe needs. A pipe completes a connect only while an Accept is
// pending, where a unix listener's backlog completes one with no Accept at all.
func acceptAndClose(ln net.Listener) {
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

// unsetEnv removes name for the rest of the test; t.Setenv restores it after.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

// herdrConfigDirProbe is one environment herdr 0.9.1 was probed under, and the
// config directory it reported there.
type herdrConfigDirProbe struct {
	name string
	env  map[string]string // a variable absent from the map is unset
	want string
}

// herdrConfigDirVars are every variable a probe row sets or unsets, so the
// machine's own environment cannot decide a row.
var herdrConfigDirVars = []string{"XDG_CONFIG_HOME", "APPDATA", "USERPROFILE", "HOME", "TMP", "TEMP"}

func herdrConfigDirProbes(root string) []herdrConfigDirProbe {
	probes := []herdrConfigDirProbe{
		{
			name: "XDG_CONFIG_HOME wins over APPDATA",
			env:  map[string]string{"XDG_CONFIG_HOME": root + `\xdg`, "APPDATA": root + `\appdata`, "USERPROFILE": root + `\up`},
			want: root + `\xdg`,
		},
		{
			name: "an empty XDG_CONFIG_HOME is still used",
			env:  map[string]string{"XDG_CONFIG_HOME": "", "APPDATA": root + `\appdata`},
			want: "",
		},
		{
			name: "a relative XDG_CONFIG_HOME is used as given",
			env:  map[string]string{"XDG_CONFIG_HOME": "relcfg", "APPDATA": root + `\appdata`},
			want: "relcfg",
		},
		{
			name: "APPDATA when XDG_CONFIG_HOME is unset",
			env:  map[string]string{"APPDATA": root + `\appdata`, "USERPROFILE": root + `\up`, "HOME": root + `\home`},
			want: root + `\appdata`,
		},
		{
			name: "an empty APPDATA is still used",
			env:  map[string]string{"APPDATA": "", "USERPROFILE": root + `\up`},
			want: "",
		},
		{
			name: "USERPROFILE when APPDATA is unset",
			env:  map[string]string{"USERPROFILE": root + `\up`, "HOME": root + `\home`},
			want: root + `\up\AppData\Roaming`,
		},
		{
			name: "HOME when USERPROFILE is unset",
			env:  map[string]string{"HOME": root + `\home`, "TMP": root + `\tmp`},
			want: root + `\home\.config`,
		},
		{
			name: "the temp directory when none is set",
			env:  map[string]string{"TMP": root + `\tmp`, "TEMP": root + `\tmp`},
			want: root + `\tmp`,
		},
		{
			name: "a trailing separator on USERPROFILE is not doubled",
			env:  map[string]string{"USERPROFILE": root + `\up\`, "HOME": root + `\home`},
			want: root + `\up\AppData\Roaming`,
		},
		{
			name: "a trailing separator on HOME is not doubled",
			env:  map[string]string{"HOME": root + `\home\`, "TMP": root + `\tmp`},
			want: root + `\home\.config`,
		},
	}
	if parent, ok := driveRootParent(root); ok {
		probes = append(probes,
			herdrConfigDirProbe{
				name: "a parent of the drive root in XDG_CONFIG_HOME is kept",
				env:  map[string]string{"XDG_CONFIG_HOME": parent + `\xdg`, "APPDATA": root + `\appdata`},
				want: parent + `\xdg`,
			},
			herdrConfigDirProbe{
				name: "USERPROFILE is joined without cleaning",
				env:  map[string]string{"USERPROFILE": parent + `\up`, "HOME": root + `\home`},
				want: parent + `\up\AppData\Roaming`,
			},
		)
	}
	return probes
}

// driveRootParent spells root with a ".." at its drive's root (C:\x as
// C:\..\x): the same directory to the file system, but in a pipe name the ".."
// removes the drive instead. ok is false when root is not on a drive letter.
func driveRootParent(root string) (string, bool) {
	if len(filepath.VolumeName(root)) != 2 {
		return "", false
	}
	return root[:2] + `\..` + root[2:], true
}

// TestSocketPathJoinsLikeHerdr: socketPath is, character for character, the
// socket path herdr 0.9.1 reported for the same configuration (`herdr session
// list --json`): nothing is cleaned, a separator is added only where one is
// missing, and a bare drive takes none.
func TestSocketPathJoinsLikeHerdr(t *testing.T) {
	root := t.TempDir()
	slashed := strings.ReplaceAll(root, `\`, "/")
	cases := []struct{ xdg, session, want string }{
		{root + `\cfg\\`, "default", root + `\cfg\\herdr\herdr.sock`},
		{slashed + "/cfg/sub", "default", slashed + `/cfg/sub\herdr\herdr.sock`},
		{root + `\cfg\.\sub`, "default", root + `\cfg\.\sub\herdr\herdr.sock`},
		{root + `\cfg\x\..\sub`, "default", root + `\cfg\x\..\sub\herdr\herdr.sock`},
		{root + `\cfg\`, "probe", root + `\cfg\herdr\sessions\probe\herdr.sock`},
		{"C:", "default", `C:herdr\herdr.sock`},
	}
	if parent, ok := driveRootParent(root); ok {
		cases = append(cases, struct{ xdg, session, want string }{parent + `\cfg`, "default", parent + `\cfg\herdr\herdr.sock`})
	}
	for _, tc := range cases {
		t.Setenv("XDG_CONFIG_HOME", tc.xdg)
		if got := newClient(tc.session, "").socketPath(); got != tc.want {
			t.Errorf("XDG_CONFIG_HOME=%s, session %s: socketPath() = %q, want %q", tc.xdg, tc.session, got, tc.want)
		}
	}
}

// applyProbeEnv sets or unsets every herdrConfigDirVars entry as probe says,
// for the rest of the test.
func applyProbeEnv(t *testing.T, probe herdrConfigDirProbe) {
	t.Helper()
	for _, name := range herdrConfigDirVars {
		if v, ok := probe.env[name]; ok {
			t.Setenv(name, v)
		} else {
			unsetEnv(t, name)
		}
	}
}

// TestHerdrConfigDirReproducesHerdrsResolution pins the resolution herdr 0.9.1
// was measured to use on Windows: each row is one probe of the binary, the
// environment it ran under and the config directory it reported.
// TestSocketPathMatchesHerdrLive asks a real herdr the same questions, so a
// herdr that changes its answers fails there.
func TestHerdrConfigDirReproducesHerdrsResolution(t *testing.T) {
	for _, probe := range herdrConfigDirProbes(t.TempDir()) {
		t.Run(probe.name, func(t *testing.T) {
			applyProbeEnv(t, probe)
			if got := herdrConfigDir(); got != probe.want {
				t.Errorf("herdrConfigDir() = %q, want %q", got, probe.want)
			}
		})
	}
}

// TestHerdrPipeNameMatchesWindowsNormalization pins the name the client dials
// to the one Windows gives herdr's pipe. The trailing-dot row is why the name
// goes through GetFullPathName rather than filepath.Clean, which keeps the dot
// and would dial a pipe herdr never created. The refused rows would otherwise
// open a device outside the pipe namespace.
func TestHerdrPipeNameMatchesWindowsNormalization(t *testing.T) {
	for _, tc := range []struct{ sock, want string }{
		{`C:\Users\u\AppData\Roaming\herdr\sessions\s\herdr.sock`, `\\.\pipe\C:\Users\u\AppData\Roaming\herdr\sessions\s\herdr.sock`},
		{`C:/Users/u/xdg/herdr/sessions/s/herdr.sock`, `\\.\pipe\C:\Users\u\xdg\herdr\sessions\s\herdr.sock`},
		{`C:\Users\\u\herdr\herdr.sock`, `\\.\pipe\C:\Users\u\herdr\herdr.sock`},
		{`relcfg\herdr\sessions\s\herdr.sock`, `\\.\pipe\relcfg\herdr\sessions\s\herdr.sock`},
		{`C:\Users\u.\herdr\herdr.sock`, `\\.\pipe\C:\Users\u\herdr\herdr.sock`},
	} {
		got, err := herdrPipeName(tc.sock)
		if err != nil || got != tc.want {
			t.Errorf("herdrPipeName(%q) = %q, %v; want %q", tc.sock, got, err, tc.want)
		}
	}
	for _, sock := range []string{`..\x\herdr.sock`, `a\..\..\x\herdr.sock`, ``} {
		if got, err := herdrPipeName(sock); err == nil {
			t.Errorf("herdrPipeName(%q) = %q, want a refusal: the name leaves the pipe namespace", sock, got)
		} else if !errors.Is(err, errEndpointRefused) {
			t.Errorf("herdrPipeName(%q) = %v, want it marked errEndpointRefused", sock, err)
		}
	}
}

// echoOnce serves one connection on ln: it reports what the client sent before
// hanging up, echoing a line back when there is one.
func echoOnce(t *testing.T, ln net.Listener) <-chan string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- "accept: " + err.Error()
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil {
			_, _ = conn.Write([]byte(line))
		} else if !errors.Is(err, io.EOF) {
			line += "read: " + err.Error()
		}
		got <- line
	}()
	return got
}

func pipeTestSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "herdr", "sessions", "s", "herdr.sock")
}

// TestDialReachesTheSocketPathsPipe: the production connect, handed a socket
// path, reaches the pipe herdr serves that path on, and carries bytes both ways.
func TestDialReachesTheSocketPathsPipe(t *testing.T) {
	sock := pipeTestSocket(t)
	ln, err := listenHerdrEndpoint(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	served := echoOnce(t, ln)

	c := newClient("s", "")
	c.sockPath = sock
	conn, err := c.dialSocket(context.Background())
	if err != nil {
		t.Fatalf("dialSocket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	back, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || back != "ping\n" {
		t.Fatalf("echo = %q, %v; want %q", back, err, "ping\n")
	}
	select {
	case got := <-served:
		if got != "ping\n" {
			t.Fatalf("server received %q, want %q", got, "ping\n")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server never reported what it received")
	}
}

// recordReceived accepts every connection on ln and records what each client
// sends before hanging up. The returned stop closes ln and returns everything
// received. A client that hangs up before the pipe answers its connect never
// reaches Accept (go-winio drops the instance on ERROR_NO_DATA and waits for
// another client), so it sent nothing the server could read; a test that
// waited on a single Accept for it would wait forever.
func recordReceived(ln net.Listener) (stop func() string) {
	var (
		mu    sync.Mutex
		got   strings.Builder
		conns sync.WaitGroup
	)
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				b, _ := io.ReadAll(conn)
				mu.Lock()
				got.Write(b)
				mu.Unlock()
			}()
		}
	}()
	return func() string {
		_ = ln.Close()
		<-accepting
		conns.Wait()
		mu.Lock()
		defer mu.Unlock()
		return got.String()
	}
}

// TestDialRefusesAPipeAnotherAccountOwns: the owner check refuses a pipe whose
// owner is not an accepted one, names that owner, and runs before anything is
// written. No other account can create a pipe in a unit test, so the test
// narrows the accepted owners to the NULL SID, which owns nothing: the pipe is
// then foreign whatever token created it (this user's, an elevated
// administrator's, SYSTEM's). A pipe created here carries this token's default
// owner, so a check that named that owner without reading the pipe would pass
// too; only a pipe another account creates tells the two apart.
func TestDialRefusesAPipeAnotherAccountOwns(t *testing.T) {
	sock := pipeTestSocket(t)
	ln, err := listenHerdrEndpoint(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	received := recordReceived(ln)

	nobody, err := windows.CreateWellKnownSid(windows.WinNullSid)
	if err != nil {
		t.Fatal(err)
	}
	owner := tokenDefaultOwner(t, windows.GetCurrentProcessToken())
	owners := pipeOwners
	t.Cleanup(func() { pipeOwners = owners })
	pipeOwners = func() ([]*windows.SID, error) { return []*windows.SID{nobody}, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialUnix(ctx, sock)
	if err == nil {
		_ = conn.Close()
		t.Fatal("dialUnix accepted a pipe whose owner is not an accepted owner")
	}
	if !strings.Contains(err.Error(), "not by this user") || !strings.Contains(err.Error(), owner.String()) {
		t.Fatalf("dialUnix error = %v; want a refusal naming the pipe's owner %s", err, owner)
	}
	if !errors.Is(err, errEndpointRefused) {
		t.Fatalf("dialUnix error = %v; want it marked errEndpointRefused", err)
	}

	// The same pipe, with the real accepted owners, is this user's own.
	pipeOwners = owners
	conn, err = dialUnix(ctx, sock)
	if err != nil {
		t.Fatalf("dialUnix with this user's owners: %v", err)
	}
	_ = conn.Close()
	if got := received(); got != "" {
		t.Fatalf("the pipe received %q; the owner check must run before anything is written", got)
	}
}

// TestDialClosesThePipeWhenTheServerHangsUp: a server that hangs up on every
// client as soon as it connects costs the dial nothing. Whichever step the
// hang-up fails, the handle the dial opened is closed; go-winio's own dial
// leaks it when GetNamedPipeInfo is the step that fails.
func TestDialClosesThePipeWhenTheServerHangsUp(t *testing.T) {
	sock := pipeTestSocket(t)
	name, err := herdrPipeName(sock)
	if err != nil {
		t.Fatal(err)
	}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	const warm, connects = 100, 1000
	var servedCount atomic.Int32
	served := make(chan error, 1)
	go func() {
		for i := 0; i < connects; i++ {
			h, err := windows.CreateNamedPipe(name16, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 0, 0, 0, nil)
			if err != nil {
				served <- err
				return
			}
			_ = windows.ConnectNamedPipe(h, nil)
			_ = windows.DisconnectNamedPipe(h)
			_ = windows.CloseHandle(h)
			servedCount.Add(1)
		}
		served <- nil
	}()

	// The count is taken once the runtime has made the threads a blocking
	// server needs, so the rest of the growth is the dial's.
	var before uint32
	deadline := time.Now().Add(time.Minute)
	for dials := 0; ; dials++ {
		if before == 0 && servedCount.Load() >= warm {
			before = processHandleCount(t)
		}
		select {
		case err := <-served:
			if err != nil {
				t.Fatalf("serve: %v", err)
			}
			after := processHandleCount(t)
			t.Logf("%d dials, %d connects served: handles %d -> %d over the last %d", dials, connects, before, after, connects-warm)
			if grew := int(after) - int(before); grew > (connects-warm)/20 {
				t.Errorf("%d dials against a server that hung up on each left %d more handles open", dials, grew)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server saw fewer than %d connects in a minute", connects)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		if conn, err := dialUnix(ctx, sock); err == nil {
			_ = conn.Close()
		}
		cancel()
	}
}

var procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

// processHandleCount is the number of handles this process has open.
func processHandleCount(t *testing.T) uint32 {
	t.Helper()
	var n uint32
	if r, _, err := procGetProcessHandleCount.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n))); r == 0 {
		t.Fatalf("GetProcessHandleCount: %v", err)
	}
	return n
}

// pipeSquatterEnv, when set, makes this test binary a stand-in for a process
// that takes herdr's pipe name first. Instead of running tests it serves the
// pipe the variable names, holding each connection open until the client hangs
// up. It reports "ready" once the pipe exists and exits when its stdin closes.
const pipeSquatterEnv = "GC_HERDR_TEST_PIPE_SQUATTER"

func init() {
	name := os.Getenv(pipeSquatterEnv)
	if name == "" {
		return
	}
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		fmt.Printf("listen: %v\n", err)
		os.Exit(1)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// startPipeSquatter runs this test binary as a pipe squatter (pipeSquatterEnv)
// serving the pipe name, under token, or under this process's own token when
// token is 0, and returns once the pipe exists.
func startPipeSquatter(t *testing.T, name string, token windows.Token) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), pipeSquatterEnv+"="+name)
	if token != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(token)}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the pipe squatter: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		exited := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "ready" {
			t.Fatalf("the pipe squatter did not start serving: %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pipe squatter did not report ready within 30s")
	}
}

// integrityToken is a primary token for this user at the integrity level label
// names. A process may always lower its own level, so this needs no privilege.
func integrityToken(t *testing.T, label windows.WELL_KNOWN_SID_TYPE) windows.Token {
	t.Helper()
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT, &self); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = self.Close() }()
	var token windows.Token
	if err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = token.Close() })
	sid, err := windows.CreateWellKnownSid(label)
	if err != nil {
		t.Fatal(err)
	}
	tml := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(token, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&tml)), tml.Size()); err != nil {
		t.Fatal(err)
	}
	return token
}

// TestDialRefusesAPipeALowerIntegrityProcessCreated: a process of this user
// running below Medium integrity, a sandboxed one, can create herdr's pipe name
// before herdr does. The pipe is then owned by this user, but it carries its
// creator's Low label, and the dial refuses it. The same squatter at this
// process's own level is accepted, so the refusal is the label's.
func TestDialRefusesAPipeALowerIntegrityProcessCreated(t *testing.T) {
	low := pipeTestSocket(t)
	lowName, err := herdrPipeName(low)
	if err != nil {
		t.Fatal(err)
	}
	startPipeSquatter(t, lowName, integrityToken(t, windows.WinLowLabelSid))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := dialUnix(ctx, low); err == nil {
		_ = conn.Close()
		t.Fatal("dialUnix accepted a pipe a Low-integrity process created")
	} else if !strings.Contains(err.Error(), "labeled Low integrity") || !errors.Is(err, errEndpointRefused) {
		t.Fatalf("dialUnix error = %v; want a refusal (errEndpointRefused) naming the pipe's Low label", err)
	}

	same := pipeTestSocket(t)
	sameName, err := herdrPipeName(same)
	if err != nil {
		t.Fatal(err)
	}
	startPipeSquatter(t, sameName, 0)
	conn, err := dialUnix(ctx, same)
	if err != nil {
		t.Fatalf("dialUnix refused the same squatter at this process's own integrity: %v", err)
	}
	_ = conn.Close()
}

// TestMandatoryIntegrityReadsTheLabel: the level is the mandatory label ACE's,
// whatever else the SACL holds, and a descriptor with no label is Medium.
func TestMandatoryIntegrityReadsTheLabel(t *testing.T) {
	for _, tc := range []struct {
		sddl string
		want uint32
	}{
		{"O:BAD:(A;;FA;;;WD)", mandatoryMediumRID},
		{"O:BAD:(A;;FA;;;WD)S:", mandatoryMediumRID},
		{"O:BAD:(A;;FA;;;WD)S:(AU;SA;FA;;;WD)", mandatoryMediumRID},
		{"O:BAD:(A;;FA;;;WD)S:(ML;;NW;;;LW)", 0x1000},
		{"O:BAD:(A;;FA;;;WD)S:(AU;SA;FA;;;WD)(ML;;NW;;;LW)", 0x1000},
		{"O:BAD:(A;;FA;;;WD)S:(ML;;NW;;;S-1-16-0)", 0},
		{"O:BAD:(A;;FA;;;WD)S:(ML;;NW;;;ME)", mandatoryMediumRID},
		{"O:BAD:(A;;FA;;;WD)S:(ML;;NW;;;HI)", 0x3000},
	} {
		sd, err := windows.SecurityDescriptorFromString(tc.sddl)
		if err != nil {
			t.Fatalf("%s: %v", tc.sddl, err)
		}
		if got, err := mandatoryIntegrity(sd); err != nil || got != tc.want {
			t.Errorf("mandatoryIntegrity(%s) = %#x, %v; want %#x", tc.sddl, got, err, tc.want)
		}
	}
}

// TestServerAliveFollowsThePipeNotThePointerFile is the Windows form of the
// stale-server regressions in staleserver_test.go. What herdr leaves at the
// socket path after an unclean exit is a pointer file, not a socket: liveness
// must come from the pipe, and the file must be left for the next server,
// which overwrites it.
func TestServerAliveFollowsThePipeNotThePointerFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := newClient("staletest", "")
	sock := c.socketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	const pointer = "31976:1790669851751981300" // as herdr 0.9.1 writes it: <pid>:<nanoseconds>
	if err := os.WriteFile(sock, []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}

	if c.serverAlive() {
		t.Fatal("serverAlive() = true with only herdr's pointer file and no pipe; want false")
	}
	c.removeStaleSocket()
	if b, err := os.ReadFile(sock); err != nil || string(b) != pointer {
		t.Fatalf("removeStaleSocket changed herdr's pointer file: %q, %v", b, err)
	}

	ln, err := listenHerdrEndpoint(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	acceptAndClose(ln)
	if !c.serverAlive() {
		t.Fatal("serverAlive() = false while herdr's pipe is accepting; want true")
	}
}

// TestProviderFakesLeaveTheHerdrConfigDirAlone: the provider fakes keep to the
// test's own directories. With XDG_CONFIG_HOME unset, herdr's config directory
// is APPDATA, which outside this test is the real profile's.
func TestProviderFakesLeaveTheHerdrConfigDirAlone(t *testing.T) {
	appData := t.TempDir()
	unsetEnv(t, "XDG_CONFIG_HOME")
	t.Setenv("APPDATA", appData)
	requireEmpty := func(when string) {
		t.Helper()
		entries, err := os.ReadDir(appData)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("%s: %s appeared under the herdr config directory", when, e.Name())
		}
	}
	// Registered before the fakes' own cleanups, so it runs after them.
	t.Cleanup(func() { requireEmpty("after cleanup") })

	p, _ := newFakeHerdrProvider(t)
	if sock := p.c.socketPath(); strings.HasPrefix(strings.ToLower(sock), strings.ToLower(appData)) {
		t.Errorf("the provider fake's socket %s is under the herdr config directory: it would name a real session's pipe", sock)
	}
	listenHerdrSocket(t, p)
	ctx := context.Background()
	if err := p.Start(ctx, "hermetic", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop("hermetic"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	requireEmpty("after a provider journey")

	// The helper itself, for a provider whose socket does resolve there.
	bare := New(fmt.Sprintf("gctest-hermetic-%d", os.Getpid()), t.TempDir(), t.TempDir(), 0, 0)
	listenHerdrSocket(t, bare)
	requireEmpty("after serving a session under the config directory")
}

// TestTokenPipeOwnersOutliveTheQuery: a token whose default owner is not its
// user, as an elevated administrator's is (the Administrators group), adds that
// owner, and the SID must stay intact once the query has returned and the stack
// it ran on is reused. A split-token administrator's linked token is such a
// token, and reading it needs no elevation; an elevated process's own is one.
func TestTokenPipeOwnersOutliveTheQuery(t *testing.T) {
	self := windows.GetCurrentProcessToken()
	tokens := []windows.Token{self}
	if linked, err := self.GetLinkedToken(); err == nil {
		t.Cleanup(func() { _ = linked.Close() })
		tokens = append(tokens, linked)
	}
	// A collection could shrink the stack between the query and the clobber,
	// moving the frames instead of overwriting them.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	tested := false
	for _, token := range tokens {
		user, err := token.GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}
		want := tokenDefaultOwner(t, token)
		if want.Equals(user.User.Sid) {
			continue
		}
		tested = true
		clobberStack(64) // grow the stack first, so the clobber below overwrites the query's frame in place
		owners, err := tokenPipeOwners(token)
		if err != nil {
			t.Fatal(err)
		}
		clobberStack(64)
		if len(owners) != 2 {
			t.Fatalf("tokenPipeOwners returned %d owners; want the user and the default owner %v", len(owners), want)
		}
		if !owners[1].IsValid() || !owners[1].Equals(want) {
			t.Errorf("default owner once the query returned = %q (valid: %v); want %v", owners[1].String(), owners[1].IsValid(), want)
		}
	}
	if !tested {
		t.Skip("no token here has a default owner other than its user: not an administrator, or the default-owner policy names the object's creator")
	}
}

// tokenDefaultOwner is the oracle for tokenPipeOwners: token's default owner,
// copied out of the query buffer while that buffer is still live.
func tokenDefaultOwner(t *testing.T, token windows.Token) *windows.SID {
	t.Helper()
	var n uint32
	_ = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &n)
	if n == 0 {
		t.Fatal("TOKEN_OWNER reported no size")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], n, &n); err != nil {
		t.Fatal(err)
	}
	sid, err := (*(**windows.SID)(unsafe.Pointer(&buf[0]))).Copy()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// clobberStack overwrites the stack below its caller's frame, where the locals
// of a function the caller already returned from lived.
//
//go:noinline
func clobberStack(depth int) byte {
	var frame [512]byte
	for i := range frame {
		frame[i] = 0xa5
	}
	if depth == 0 {
		return frame[0]
	}
	return clobberStack(depth-1) ^ frame[depth%len(frame)]
}

// TestConfigureServerReportsARefusedPipe: when herdr's pipe name is held by a
// pipe the dial refuses, ConfigureServer fails at once with the refusal's
// reason and launches no herdr. The binary does not exist, so a launch would
// fail with a different error.
func TestConfigureServerReportsARefusedPipe(t *testing.T) {
	p := New("gctest-refused", t.TempDir(), t.TempDir(), 0, 0)
	p.c.sockPath = pipeTestSocket(t)
	p.c.bin = filepath.Join(t.TempDir(), "herdr.exe")
	ln, err := listenHerdrEndpoint(p.c.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	received := recordReceived(ln)
	t.Cleanup(func() { _ = received() })

	nobody, err := windows.CreateWellKnownSid(windows.WinNullSid)
	if err != nil {
		t.Fatal(err)
	}
	owners := pipeOwners
	t.Cleanup(func() { pipeOwners = owners })
	pipeOwners = func() ([]*windows.SID, error) { return []*windows.SID{nobody}, nil }

	err = p.ConfigureServer()
	if !errors.Is(err, errEndpointRefused) || !strings.Contains(err.Error(), "not by this user") {
		t.Fatalf("ConfigureServer = %v; want the refusal of the pipe holding herdr's name", err)
	}
}
