package containerhost

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	default:
		return
	}
	os.Exit(code)
}

// InstallCLI links the emulated host's CLIs (docker) into binDir, served by
// self (the running test binary).
func InstallCLI(binDir, self string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("creating CLI dir: %w", err)
	}
	for _, name := range []string{"docker"} {
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
