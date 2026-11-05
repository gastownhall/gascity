package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// Exec admission (PAR-EXEC PR-4, owner decisions D1, D3, D4). v2 runs an exec
// session runtime iff the script's live handshake declares the v2 profile
// (sessionexec.V2ProfileGaps: observe.v1 and report-attachment; observe.v1
// obliges observe, inventory and kill-object). The handshake is the one
// thing the admission decision reads that is not config: execReadiness runs
// `<script> protocol` once per version of the script file and city, under
// execReadinessProbeTimeout, and remembers the answer: a handshake until
// the file changes (or C0.8 finds the built provider disagrees), an error
// for sessionexec.HandshakeRetryAfter. Everything else the latch reads is
// the composed config, the GC_SESSION override and the builtin runtime
// registry.

// execReadinessProbeTimeout bounds the admission probe's `protocol` op. A
// variable so a test can bound a hung probe.
var execReadinessProbeTimeout = 10 * time.Second

// execReadinessNow is the admission memo's clock.
var execReadinessNow = time.Now

// execHandshake is an admission probe's answer: the handshake and its v2
// gaps, or the error that kept the probe from reading it.
type execHandshake struct {
	info    runtime.ProtocolInfo
	missing []string
	err     error
}

// execReadinessKey is one probe's subject: a version of the script file (its
// resolved path, size, modification time and inode; rewriting or replacing
// the file is a new version) and the city it runs for, whose name and path
// the probe carries in its env, as the provider the registry builds does.
type execReadinessKey struct {
	path     string
	size     int64
	mtime    time.Time
	inode    uint64
	cityName string
	cityPath string
}

// execReadinessEntry is a memo entry: done closes once the probe answered,
// so callers that miss while it runs wait for its one answer (no second
// fork); at is when it answered, for an error's retry window.
type execReadinessEntry struct {
	done chan struct{}
	h    execHandshake
	at   time.Time
}

var execHandshakeMemo = struct {
	sync.Mutex
	m map[execReadinessKey]*execReadinessEntry
}{m: map[execReadinessKey]*execReadinessEntry{}}

// probeExecHandshake runs `script protocol` (sessionexec.ProbeV2Readiness)
// under execReadinessProbeTimeout with the city's env. It is a variable so
// tests can count and stall the probes.
var probeExecHandshake = func(path, cityName, cityPath string) (runtime.ProtocolInfo, []string, error) {
	return sessionexec.ProbeV2Readiness(context.Background(), path, cityName, cityPath, execReadinessProbeTimeout)
}

// execReadiness is script's admission handshake for the city: the
// remembered answer for this version of the file, else one probe, which
// every concurrent caller for the same version waits for. A handshake is
// remembered until the file changes or forgetExecReadiness; an error for
// sessionexec.HandshakeRetryAfter, so a hung or failing `protocol` costs
// one probe per window, not one per latch. A script that cannot be resolved
// or read is an error of its own, not remembered.
func execReadiness(script, cityName, cityPath string) execHandshake {
	path, err := exec.LookPath(script)
	if err != nil {
		return execHandshake{err: err}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return execHandshake{err: err}
	}
	k := execReadinessKey{path: path, size: fi.Size(), mtime: fi.ModTime(), inode: fileInode(fi), cityName: cityName, cityPath: cityPath}
	execHandshakeMemo.Lock()
	e, ok := execHandshakeMemo.m[k]
	if ok {
		select {
		case <-e.done:
			if e.h.err != nil && execReadinessNow().Sub(e.at) >= sessionexec.HandshakeRetryAfter {
				ok = false // the error's window ran out: probe again
			}
		default: // a probe is running: wait for its answer below
		}
	}
	if !ok {
		e = &execReadinessEntry{done: make(chan struct{})}
		execHandshakeMemo.m[k] = e
		execHandshakeMemo.Unlock()
		probeInto(e, path, cityName, cityPath)
		return e.h
	}
	execHandshakeMemo.Unlock()
	<-e.done
	return e.h
}

// probeInto runs the probe for e and always releases its waiters: a probe
// that panics leaves e an error from the zero time, so the next caller
// probes again, and the panic goes on to this caller.
func probeInto(e *execReadinessEntry, path, cityName, cityPath string) {
	e.h = execHandshake{err: errExecProbePanicked}
	defer close(e.done)
	info, missing, perr := probeExecHandshake(path, cityName, cityPath)
	e.h, e.at = execHandshake{info: info, missing: missing, err: perr}, execReadinessNow()
}

// errExecProbePanicked is the answer a panicked probe leaves its waiters.
var errExecProbePanicked = errors.New("the handshake probe panicked")

// forgetExecReadiness drops every remembered handshake: C0.8 found a built
// provider whose own handshake disagrees with an admitted one, so the next
// latch probes again (and a default city falls back to legacy).
func forgetExecReadiness() {
	execHandshakeMemo.Lock()
	defer execHandshakeMemo.Unlock()
	for k, e := range execHandshakeMemo.m {
		select {
		case <-e.done:
			delete(execHandshakeMemo.m, k)
		default: // a probe still running keeps its entry for its waiters
		}
	}
}

// execRuntimeScript is the script an exec session runtime runs: the path an
// `exec:` name carries, or a pack runtime's declared command. ok is false
// for every other runtime, the t3bridge exec spelling included.
func execRuntimeScript(cfg *config.City, name string) (script string, ok bool) {
	if s, isExec := strings.CutPrefix(name, "exec:"); isExec {
		return s, !isLegacyT3BridgeExecScript(s)
	}
	if rt, isPack := cfg.Runtimes[name]; isPack {
		return rt.Command, true
	}
	return "", false
}

// fix is the admission refusal's Fix: the handshake error, or the
// capabilities it lacks.
func (h execHandshake) fix(script string) string {
	if h.err != nil {
		return fmt.Sprintf("its handshake (%s protocol) could not be read: %v; v2 needs the handshake to declare %s", script, h.err, strings.Join(sessionexec.V2ProfileCapabilities(), " and "))
	}
	return fmt.Sprintf("its handshake does not declare %s; see `gc runtime conformance %s`", strings.Join(h.missing, " or "), script)
}

// ready reports a handshake with no v2 gaps.
func (h execHandshake) ready() bool { return h.err == nil && len(h.missing) == 0 }

// fencesInput reports a handshake that fences input to one incarnation
// (proc.exec and input-fence; sessionexec.Provider.InputFencing).
func (h execHandshake) fencesInput() bool {
	return h.info.Has(runtime.ProtocolCapabilityConnectionExec) && h.info.Has(runtime.ProtocolCapabilityInputFence)
}

// execRuntimeDoctorDetail is the session reconciler check's line about the
// exec session runtime the city runs: v2-ready; "" for any other runtime,
// and for one that is not v2-ready, which the check's "v2 would refuse"
// line already names with its fix; and warn, when v2 runs on it without fenced input (owner
// decision D2: not required, so v2 runs, but input gc fences to one
// incarnation, a fenced turn stop or nudge, is refused on it rather than
// sent unfenced).
func execRuntimeDoctorDetail(cfg *config.City, cityPath string, lookupEnv func(string) (string, bool), mode reconcilerMode) (detail, warn string) {
	name := v2SessionRuntimeName(cfg, lookupEnv)
	script, ok := execRuntimeScript(cfg, name)
	if !ok {
		return "", ""
	}
	h := execReadiness(script, cfg.EffectiveCityName(), cityPath)
	if !h.ready() {
		return "", ""
	}
	detail = fmt.Sprintf("exec session runtime %q: v2-ready (%s)", name, strings.Join(sessionexec.V2ProfileCapabilities(), ", "))
	if mode == reconcilerV2 && !h.fencesInput() {
		warn = fmt.Sprintf("v2 runs on the exec session runtime %q, whose handshake does not declare %s with %s: input gc fences to one incarnation (a fenced turn stop or nudge) is refused on it", name, runtime.ProtocolCapabilityInputFence, runtime.ProtocolCapabilityConnectionExec)
	}
	return detail, warn
}

// cityPathOf is a doctor context's city path, "" without one.
func cityPathOf(ctx *doctor.CheckContext) string {
	if ctx == nil {
		return ""
	}
	return ctx.CityPath
}
