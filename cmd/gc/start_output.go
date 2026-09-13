package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/logutil"
	"golang.org/x/term"
)

type startOutputProxyOptions struct {
	Verbose bool
	TTY     bool
}

type startOutputRecord struct {
	line      string
	warning   bool
	duplicate int
}

type startOutputProxy struct {
	mu             sync.Mutex
	dst            io.Writer
	opts           startOutputProxyOptions
	partial        strings.Builder
	records        []startOutputRecord
	warningRecords map[string]int
	warningDedup   *logutil.Dedup
	warnings       int
	fatal          string
	closed         bool
}

func newStartOutputProxy(dst io.Writer, opts startOutputProxyOptions) *startOutputProxy {
	return &startOutputProxy{
		dst:            dst,
		opts:           opts,
		warningRecords: make(map[string]int),
		warningDedup:   logutil.NewDedup(logutil.DefaultDedupCapacity),
	}
}

func (p *startOutputProxy) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	written := len(data)
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			p.partial.Write(data)
			return written, nil
		}
		p.partial.Write(data[:idx])
		p.addLineLocked(strings.TrimSuffix(p.partial.String(), "\r"))
		p.partial.Reset()
		data = data[idx+1:]
	}
	return written, nil
}

func (p *startOutputProxy) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	if p.partial.Len() > 0 {
		p.addLineLocked(strings.TrimSuffix(p.partial.String(), "\r"))
		p.partial.Reset()
	}
	if p.dst == nil {
		return nil
	}
	for _, record := range p.records {
		if !record.warning {
			continue
		}
		line := record.line
		if record.duplicate > 0 {
			line = fmt.Sprintf("%s (suppressed %d more)", line, record.duplicate)
		}
		if _, err := fmt.Fprintln(p.dst, line); err != nil {
			return err
		}
	}
	if p.fatal != "" {
		if _, err := fmt.Fprintln(p.dst, logutil.RenderFatalLine(p.fatal, p.opts.TTY)); err != nil {
			return err
		}
	}
	return nil
}

func (p *startOutputProxy) WarningCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.warnings
}

func (p *startOutputProxy) FatalCause() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return logutil.FatalCause(p.fatal)
}

func (p *startOutputProxy) SetFatal(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setFatalLocked(message)
}

func (p *startOutputProxy) addLineLocked(line string) {
	if message, ok := logutil.ParseFatalLine(line); ok {
		p.setFatalLocked(message)
		return
	}
	if isStartWarningLine(line) {
		p.warnings++
		if !p.opts.Verbose {
			if !p.warningDedup.First(line) {
				if idx, ok := p.warningRecords[line]; ok {
					p.records[idx].duplicate++
				}
				return
			}
		}
		p.warningRecords[line] = len(p.records)
		p.records = append(p.records, startOutputRecord{line: line, warning: true})
		return
	}
	// Stream non-warning, non-fatal lines directly so operators see
	// progress live instead of waiting until Flush().
	if p.dst != nil {
		fmt.Fprintln(p.dst, line) //nolint:errcheck // best-effort streaming
	}
}

func (p *startOutputProxy) setFatalLocked(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	p.fatal = message
}

func (p *startOutputProxy) deriveFatalFromRecords() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fatal != "" {
		return p.fatal
	}
	for _, record := range p.records {
		line := strings.TrimSpace(record.line)
		if line == "" || isStartWarningLine(line) {
			continue
		}
		if strings.HasPrefix(line, "hint: ") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "gc start:"))
	}
	return "gc start failed"
}

func isStartWarningLine(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "warning: ") || strings.Contains(line, ": warning: ")
}

type startSummary struct {
	PID      int
	Binary   string
	Build    string
	Drift    string
	Warnings int
	Fatal    string
}

func startSummaryLine(s startSummary) string {
	return fmt.Sprintf(
		"gc-start: pid=%d binary=%s build=%s drift=%s warnings=%d fatal=%s",
		s.PID,
		s.Binary,
		s.Build,
		s.Drift,
		s.Warnings,
		s.Fatal,
	)
}

func writeStartSummary(w io.Writer, s startSummary) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, startSummaryLine(s)) //nolint:errcheck // best-effort summary output
}

func startSummaryBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe
	}
	return abs
}

func shortBuildHash() string {
	if commit == "" || commit == "unknown" {
		return "unknown"
	}
	dirty := ""
	base := commit
	if strings.HasSuffix(base, "-dirty") {
		dirty = "-dirty"
		base = strings.TrimSuffix(base, "-dirty")
	}
	if len(base) > 12 {
		base = base[:12]
	}
	return base + dirty
}

func startOutputIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func currentSupervisorPID() int {
	if supervisorAliveHook == nil {
		return 0
	}
	return supervisorAliveHook()
}

func fatalSummaryCause(message string) string {
	cause := logutil.FatalCause(message)
	if cause == "" {
		return ""
	}
	return cause
}
