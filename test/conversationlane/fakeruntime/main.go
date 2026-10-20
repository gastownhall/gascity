// Command fakeruntime is a file-backed exec session runtime for the
// conversation-lane evidence tests (test/conversationlane). It implements the
// exec runtime protocol (docs/reference/exec-session-provider.md) with no
// protocol handshake, so gc sees no activity capability, timed-only sleep,
// no input fence and no delivery-key deduplication.
//
//	fakeruntime --state <dir> <op> [<name> [args...]]
//
// Every invocation is appended to <dir>/ops.jsonl. Each nudge is appended,
// under a flock, to <dir>/deliveries.jsonl as one JSON line holding the
// exact text gc sent. The file <dir>/activity-mode selects the activity
// model: "none" (or no file) reports no activity, so the runtime is always at
// a boundary; "last-nudge" reports the time of the last nudge (or whatever
// the test wrote to activity.<name>), so it is busy for gc's quiescence
// window after each one.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Delivery is one line of deliveries.jsonl.
type Delivery struct {
	Seq     int    `json:"seq"`
	At      string `json:"at"`
	Session string `json:"session"`
	Text    string `json:"text"`
}

// Invocation is one line of ops.jsonl.
type Invocation struct {
	At   string   `json:"at"`
	PID  int      `json:"pid"`
	PPID int      `json:"ppid"`
	Op   string   `json:"op"`
	Args []string `json:"args,omitempty"`
}

const exitUnknownOp = 2

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 3 || args[0] != "--state" {
		fmt.Fprintln(stderr, "usage: fakeruntime --state <dir> <op> [<name> [args...]]") //nolint:errcheck // usage
		return 1
	}
	s := state{dir: args[1]}
	op, rest := args[2], args[3:]
	if err := s.record(op, rest); err != nil {
		fmt.Fprintf(stderr, "fakeruntime: recording %s: %v\n", op, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	name := ""
	if len(rest) > 0 {
		name = rest[0]
	}
	out, code, err := s.do(op, name, rest, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "fakeruntime: %s %s: %v\n", op, name, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if out != "" {
		fmt.Fprint(stdout, out) //nolint:errcheck // best-effort stdout
	}
	return code
}

type state struct{ dir string }

func (s state) path(parts ...string) string { return filepath.Join(s.dir, strings.Join(parts, ".")) }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// record appends this invocation to ops.jsonl.
func (s state) record(op string, args []string) error {
	return s.appendLocked("ops", Invocation{At: now(), PID: os.Getpid(), PPID: os.Getppid(), Op: op, Args: args}, nil)
}

// appendLocked appends v as one JSON line to <name>.jsonl under
// <name>.lock; before, when set, runs under the lock first and may fill v.
func (s state) appendLocked(name string, v any, before func(lines int) any) error {
	lock, err := os.OpenFile(s.path(name, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // lock file
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // released on close too
	file := s.path(name, "jsonl")
	if before != nil {
		data, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		v = before(strings.Count(string(data), "\n"))
	}
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func boolText(b bool) string {
	if b {
		return "true\n"
	}
	return "false\n"
}

// do carries out op and returns its stdout and exit code.
func (s state) do(op, name string, rest []string, stdin io.Reader) (string, int, error) {
	box := s.path("box", name)
	switch op {
	case "start":
		cfg, err := io.ReadAll(stdin)
		if err != nil {
			return "", 0, err
		}
		if exists(box) {
			return "", 0, fmt.Errorf("session %s already exists", name)
		}
		if err := os.WriteFile(s.path("config", name, "json"), cfg, 0o644); err != nil {
			return "", 0, err
		}
		return "", 0, os.WriteFile(box, nil, 0o644)
	case "stop":
		metas, _ := filepath.Glob(s.path("meta", name, "*"))
		for _, m := range append(metas, box) {
			if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
				return "", 0, err
			}
		}
		return "", 0, nil
	case "is-running":
		return boolText(exists(box)), 0, nil
	case "process-alive":
		_, _ = io.Copy(io.Discard, stdin)
		return boolText(exists(box)), 0, nil
	case "list-running":
		boxes, _ := filepath.Glob(s.path("box", "*"))
		var b strings.Builder
		prefix := s.path("box", name)
		for _, p := range boxes {
			if strings.HasPrefix(p, prefix) {
				b.WriteString(strings.TrimPrefix(filepath.Base(p), "box."))
				b.WriteString("\n")
			}
		}
		return b.String(), 0, nil
	case "set-meta", "get-meta", "remove-meta":
		if len(rest) < 2 {
			return "", 0, fmt.Errorf("%s needs a key", op)
		}
		meta := s.path("meta", name, rest[1])
		switch op {
		case "set-meta":
			v, err := io.ReadAll(stdin)
			if err != nil {
				return "", 0, err
			}
			return "", 0, os.WriteFile(meta, v, 0o644)
		case "get-meta":
			v, err := os.ReadFile(meta)
			if os.IsNotExist(err) {
				return "", 0, nil
			}
			return string(v), 0, err
		default:
			if err := os.Remove(meta); err != nil && !os.IsNotExist(err) {
				return "", 0, err
			}
			return "", 0, nil
		}
	case "get-last-activity":
		mode, _ := os.ReadFile(s.path("activity-mode"))
		if strings.TrimSpace(string(mode)) != "last-nudge" {
			return "", 0, nil
		}
		at, err := os.ReadFile(s.path("activity", name))
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return strings.TrimSpace(string(at)) + "\n", 0, err
	case "nudge":
		text, err := io.ReadAll(stdin)
		if err != nil {
			return "", 0, err
		}
		at := now()
		err = s.appendLocked("deliveries", nil, func(lines int) any {
			return Delivery{Seq: lines + 1, At: at, Session: name, Text: string(text)}
		})
		if err != nil {
			return "", 0, err
		}
		mode, _ := os.ReadFile(s.path("activity-mode"))
		if strings.TrimSpace(string(mode)) == "last-nudge" {
			return "", 0, os.WriteFile(s.path("activity", name), []byte(at), 0o644)
		}
		return "", 0, nil
	case "interrupt", "send-keys", "clear-scrollback", "peek":
		return "", 0, nil
	default:
		return "", exitUnknownOp, nil
	}
}
