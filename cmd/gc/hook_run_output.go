package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
)

// hookRunOutputSealEnv names the environment variable through which gc hook
// run hands its child a per-run seal. A wrapped command that commits a side
// effect after printing (nudge drain acks the nudges it just injected) frames
// that output in seals: it writes a seal, the output, and a seal, and only
// then commits. A seal marks the output before it as complete; the two seals
// of a frame mark the output between them as all or nothing.
//
// The wrapper drops a frame that never closed: a write failed part-way
// through it, or the child was killed before finishing it. When the wrapper's
// deadline fires, output before the last seal is complete and may already be
// committed, so the wrapper forwards it instead of discarding it. Output
// after the last seal may be partial and is still discarded.
const hookRunOutputSealEnv = "GC_HOOK_RUN_OUTPUT_SEAL"

// newHookRunOutputSeal returns a fresh random seal. Being unguessable, it
// cannot appear in relayed content by accident or by forgery.
func newHookRunOutputSeal() string {
	return "gc-hook-run-seal-" + rand.Text()
}

// hookRunSealMark returns seal as a child writes it: after a NUL byte. Hook
// output is text or JSON and holds no NUL, so output ending in a NUL and the
// start of the seal ends in a seal write that failed part-way.
func hookRunSealMark(seal string) []byte {
	return []byte("\x00" + seal)
}

// sealHookRunOutput writes a gc hook run seal to w, marking everything this
// process has written to w so far as complete. Called before and after a
// block of output, it frames that block. Outside gc hook run it writes
// nothing.
func sealHookRunOutput(w io.Writer) error {
	seal := os.Getenv(hookRunOutputSealEnv)
	if seal == "" {
		return nil
	}
	_, err := w.Write(hookRunSealMark(seal))
	return err
}

// hookRunForwardableOutput returns the part of a child's output gc hook run
// may forward, without seals. A frame that never closed is dropped, and so
// is a seal that was only partly written. Unless the output is complete,
// everything after the last seal may be partial and is dropped too.
func hookRunForwardableOutput(out []byte, seal string, complete bool) []byte {
	mark := hookRunSealMark(seal)
	if torn := bytes.LastIndexByte(out, 0); torn >= 0 && len(out)-torn < len(mark) && bytes.HasPrefix(mark, out[torn:]) {
		out = out[:torn]
	}
	if bytes.Count(out, mark)%2 == 1 || !complete {
		end := bytes.LastIndex(out, mark)
		if end < 0 {
			return nil
		}
		out = out[:end]
	}
	return bytes.ReplaceAll(out, mark, nil)
}

// hookRunResult returns what gc hook run forwards for a finished child and
// the code it exits with. Output is complete only when the child exited on
// its own (not timedOut) and its output was read in full (captured): then all
// of it is forwarded, less any frame left open, and the child's own status,
// childCode, is returned. Otherwise only the part the child sealed is known
// to be complete, and it may already be committed, so that part is forwarded
// and the configured fail-open timeoutExitCode is returned.
func hookRunResult(childCode int, timedOut, captured bool, out []byte, seal string, timeoutExitCode int) ([]byte, int) {
	if timedOut || !captured {
		return hookRunForwardableOutput(out, seal, false), timeoutExitCode
	}
	return hookRunForwardableOutput(out, seal, true), childCode
}

// hookRunSpool is the temporary file gc hook run captures child stdout in.
type hookRunSpool struct {
	// out is the child's stdout.
	out *os.File
	// in reads what the child wrote. createHookRunSpool decides whether it
	// can be the same open file as out.
	in *os.File
}

// read returns what the child wrote: the file's contents from offset 0 up to
// its size when read.
func (s *hookRunSpool) read() ([]byte, error) {
	info, err := s.in.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting output spool: %w", err)
	}
	return io.ReadAll(io.NewSectionReader(s.in, 0, info.Size()))
}

// close releases the spool. createHookRunSpool arranges for the file to
// disappear once its last handle closes, including handles that the child's
// descendants inherited, so there is nothing to delete by name.
func (s *hookRunSpool) close(stderr io.Writer) {
	files := []*os.File{s.out}
	if s.in != s.out {
		files = append(files, s.in)
	}
	for _, f := range files {
		if err := f.Close(); err != nil {
			fmt.Fprintf(stderr, "gc hook run: closing output spool: %v\n", err) //nolint:errcheck
		}
	}
}
