// Package exec implements [events.Provider] by delegating each operation to
// a user-supplied script via fork/exec. This follows the same pattern as
// the mail and session exec providers: a single script receives the operation
// name as its first argument and communicates via JSON on stdin/stdout.
//
// Record is fire-and-forget (fork per event, errors to stderr). List and
// LatestSeq fork once per call. Watch starts a long-running subprocess
// that streams NDJSON events on stdout.
package exec //nolint:revive // internal package, always imported with alias

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// Provider implements [events.Provider] by delegating to a user-supplied script.
type Provider struct {
	script  string
	timeout time.Duration
	ready   sync.Once // ensure-running called once
	stderr  io.Writer
}

type listScriptFilter struct {
	Type     string
	Actor    string
	Since    time.Time
	AfterSeq uint64
}

// NewProvider returns an exec events provider that delegates to the given script.
// Errors from best-effort operations (Record) are logged to stderr.
func NewProvider(script string, stderr io.Writer) *Provider {
	return &Provider{
		script:  script,
		timeout: 30 * time.Second,
		stderr:  stderr,
	}
}

// Record delegates to: script record with JSON event on stdin.
// Best-effort — errors are printed to stderr, never returned.
func (p *Provider) Record(e events.Event) {
	p.ensureRunning()
	data, err := json.Marshal(e)
	if err != nil {
		p.logErr("record: marshal: %v", err)
		return
	}
	if _, err := p.run(data, "record"); err != nil {
		p.logErr("record: %v", err)
	}
}

// List delegates to: script list with JSON filter on stdin, then applies the
// SDK filter locally so optional script filtering cannot weaken the contract.
func (p *Provider) List(filter events.Filter) ([]events.Event, error) {
	p.ensureRunning()
	scriptFilter := listScriptFilter{
		Type:     filter.Type,
		Actor:    filter.Actor,
		Since:    filter.Since,
		AfterSeq: filter.AfterSeq,
	}
	data, err := json.Marshal(scriptFilter)
	if err != nil {
		return nil, fmt.Errorf("exec events provider: marshal filter: %w", err)
	}
	out, err := p.run(data, "list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	evts, err := unmarshalEvents(out)
	if err != nil {
		return nil, err
	}
	return events.ApplyFilter(evts, filter), nil
}

// LatestSeq delegates to: script latest-seq
func (p *Provider) LatestSeq() (uint64, error) {
	p.ensureRunning()
	out, err := p.run(nil, "latest-seq")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	var seq uint64
	if _, err := fmt.Sscanf(out, "%d", &seq); err != nil {
		return 0, fmt.Errorf("exec events provider: parse seq %q: %w", out, err)
	}
	return seq, nil
}

// Watch starts a long-running subprocess: script watch <afterSeq>
// The subprocess streams NDJSON events on stdout. Each line is a
// complete JSON event.
func (p *Provider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	p.ensureRunning()
	cmd := exec.CommandContext(ctx, p.script, "watch", fmt.Sprintf("%d", afterSeq))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("exec events provider: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec events provider: start watch: %w", err)
	}
	return &execWatcher{
		cmd:     cmd,
		scanner: bufio.NewScanner(stdout),
		ctx:     ctx,
	}, nil
}

// Close is a no-op for the exec provider.
func (p *Provider) Close() error {
	return nil
}

// ensureRunning calls "ensure-running" on the script once per provider
// lifetime. Exit 2 (unknown op) is treated as success.
func (p *Provider) ensureRunning() {
	p.ready.Do(func() {
		_, _ = p.run(nil, "ensure-running")
	})
}

// run executes the script with the given args, optionally piping stdinData
// to its stdin. Returns the trimmed stdout on success.
func (p *Provider) run(stdinData []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.script, args...)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 2 {
				return "", nil
			}
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("exec events provider %s %s: %s", p.script, strings.Join(args, " "), errMsg)
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// logErr logs an error to stderr (best-effort).
func (p *Provider) logErr(format string, args ...any) {
	if p.stderr != nil {
		fmt.Fprintf(p.stderr, "events exec: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
	}
}

// unmarshalEvents decodes a JSON array of Events.
func unmarshalEvents(data string) ([]events.Event, error) {
	var evts []events.Event
	if err := json.Unmarshal([]byte(data), &evts); err != nil {
		return nil, fmt.Errorf("exec events provider: unmarshal events: %w", err)
	}
	return evts, nil
}

// execWatcher reads NDJSON events from a long-running subprocess.
type execWatcher struct {
	cmd     *exec.Cmd
	scanner *bufio.Scanner
	ctx     context.Context
}

// Next reads the next event from the subprocess stdout.
func (w *execWatcher) Next() (events.Event, error) {
	if !w.scanner.Scan() {
		if err := w.scanner.Err(); err != nil {
			return events.Event{}, err
		}
		// EOF — subprocess exited.
		if err := w.ctx.Err(); err != nil {
			return events.Event{}, err
		}
		return events.Event{}, fmt.Errorf("exec events watcher: subprocess exited")
	}
	var e events.Event
	if err := json.Unmarshal(w.scanner.Bytes(), &e); err != nil {
		return events.Event{}, fmt.Errorf("exec events watcher: unmarshal: %w", err)
	}
	return e, nil
}

// Close kills the subprocess and waits for it to exit.
func (w *execWatcher) Close() error {
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	return w.cmd.Wait()
}

// Compile-time interface check.
var _ events.Provider = (*Provider)(nil)
