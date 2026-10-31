package containerhost

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// RunAsTool runs this process as one of the emulated host's executables when
// it was started under one of their names (a test binary's TestMain calls it
// first), and reports whether it did. It does not return when it did.
func RunAsTool() {
	var code int
	switch filepath.Base(os.Args[0]) {
	case "docker":
		code = Docker(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "pgrep":
		code = Pgrep(os.Args[1:], os.Stdout, os.Stderr)
	case "kubectl":
		code = Kubectl(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	case initShimName:
		code = initShim(os.Args[1:])
	default:
		return
	}
	os.Exit(code)
}

// InstallCLI links the emulated host's CLIs (docker, kubectl) into binDir, served by
// self (the running test binary).
func InstallCLI(binDir, self string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("creating CLI dir: %w", err)
	}
	for _, name := range []string{"docker", "kubectl"} {
		if err := os.Symlink(self, filepath.Join(binDir, name)); err != nil {
			return fmt.Errorf("installing %s: %w", name, err)
		}
	}
	return nil
}

// Pgrep is procps pgrep restricted to the calling process's container: it
// sees only processes carrying the same MarkerEnv tag, as pgrep inside a
// container sees only that container's PID namespace. Supported: -x (exact
// name), -f (match the full command line), one pattern.
func Pgrep(args []string, stdout, stderr io.Writer) int {
	exact, full := false, false
	var pattern string
	havePattern := false
	for _, a := range args {
		switch {
		case a == "-x":
			exact = true
		case a == "-f":
			full = true
		case a == "-xf" || a == "-fx":
			exact, full = true, true
		case strings.HasPrefix(a, "-") && a != "-":
			_, _ = fmt.Fprintf(stderr, "pgrep: emulated pgrep does not support option %s\n", a)
			return 2
		default:
			if havePattern {
				_, _ = fmt.Fprintln(stderr, "pgrep: only one pattern can be provided")
				return 2
			}
			pattern, havePattern = a, true
		}
	}
	if !havePattern {
		_, _ = fmt.Fprintln(stderr, "pgrep: no matching criteria specified")
		return 2
	}
	expr := pattern
	if exact {
		expr = "^(?:" + pattern + ")$"
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgrep: invalid pattern %q: %v\n", pattern, err)
		return 2
	}
	id := os.Getenv(MarkerEnv)
	if id == "" {
		_, _ = fmt.Fprintln(stderr, "pgrep: not running inside an emulated container")
		return 3
	}
	found := false
	for _, pid := range containerPIDs(id) {
		subject := processName(pid)
		if full {
			subject = processCmdline(pid)
		}
		if re.MatchString(subject) {
			_, _ = fmt.Fprintln(stdout, strconv.Itoa(pid))
			found = true
		}
	}
	if !found {
		return 1
	}
	return 0
}

// processName is the kernel's comm (at most 15 bytes), which pgrep matches.
func processName(pid int) string {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(comm), "\n")
}

func processCmdline(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return string(bytes.TrimRight(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}), " "))
}

// initShimName is the argv[0] the init shim runs under.
const initShimName = "containerhost-init"

// initShim runs `containerhost-init <status file> <path> <argv...>`: it
// starts the command, forwards SIGTERM, SIGINT and SIGHUP to it, waits, and
// writes its exit status (128+signal when killed) to the status file before
// exiting with it. It is a container's PID 1 the way tini is.
func initShim(args []string) int {
	if len(args) < 3 {
		_, _ = fmt.Fprintln(os.Stderr, "containerhost-init: usage: containerhost-init <status> <path> <argv...>")
		return 125
	}
	status, path, argv := args[0], args[1], args[2:]
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	code := 0
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "containerhost-init: %v\n", err)
		code = 127
	} else {
		go func() {
			for sig := range signals {
				_ = cmd.Process.Signal(sig)
			}
		}()
		code = exitStatus(cmd.Wait())
	}
	tmp := status + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(code)+"\n"), 0o644); err == nil {
		_ = os.Rename(tmp, status)
	}
	return code
}

// exitStatus maps a Wait error to a shell-style exit status.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	}
	return 126
}
