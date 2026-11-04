package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// Exec admission (PAR-EXEC PR-4, owner decisions D1, D3, D4). v2 runs an exec
// session runtime iff the script's live handshake declares the v2 profile
// (sessionexec.V2ProfileGaps: observe.v1 and report-attachment; observe.v1
// obliges observe, inventory and kill-object). The handshake is the one
// thing the admission decision reads that is not config: execReadiness runs
// `<script> protocol` once per version of the script file, under
// execReadinessProbeTimeout, and remembers the answer (never an error).
// Everything else the latch reads is the composed config, the GC_SESSION
// override and the builtin runtime registry.

// execReadinessProbeTimeout bounds the admission probe's `protocol` op.
const execReadinessProbeTimeout = 10 * time.Second

// execHandshake is an admission probe's answer: the handshake and its v2
// gaps, or the error that kept the probe from reading it.
type execHandshake struct {
	info    runtime.ProtocolInfo
	missing []string
	err     error
}

// execScriptVersion is one version of a script file: its resolved path,
// size, modification time and inode. Rewriting the file in place, or
// replacing it, is a new version, so the probe runs again.
type execScriptVersion struct {
	path  string
	size  int64
	mtime time.Time
	inode uint64
}

var execHandshakeMemo = struct {
	sync.Mutex
	m map[execScriptVersion]execHandshake
}{m: map[execScriptVersion]execHandshake{}}

// probeExecHandshake runs `script protocol` (sessionexec.ProbeV2Readiness)
// under execReadinessProbeTimeout, with the city's name in its env and no
// city path: the latch reads config, which names no path. It is a variable
// so tests can count the probes.
var probeExecHandshake = func(path, cityName string) (runtime.ProtocolInfo, []string, error) {
	return sessionexec.ProbeV2Readiness(context.Background(), path, cityName, "", execReadinessProbeTimeout)
}

// execReadiness is script's admission handshake: the remembered answer for
// this version of the file, else a fresh probe, remembered when it read a
// handshake. A script that cannot be resolved or read is an error, never
// remembered either.
func execReadiness(script, cityName string) execHandshake {
	path, err := exec.LookPath(strings.TrimSpace(script))
	if err != nil {
		return execHandshake{err: err}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return execHandshake{err: err}
	}
	v := execScriptVersion{path: path, size: fi.Size(), mtime: fi.ModTime(), inode: fileInode(fi)}
	execHandshakeMemo.Lock()
	h, ok := execHandshakeMemo.m[v]
	execHandshakeMemo.Unlock()
	if ok {
		return h
	}
	info, missing, err := probeExecHandshake(path, cityName)
	h = execHandshake{info: info, missing: missing, err: err}
	if err == nil {
		execHandshakeMemo.Lock()
		execHandshakeMemo.m[v] = h
		execHandshakeMemo.Unlock()
	}
	return h
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

// execRuntimeDoctorDetail is the session reconciler check's line about the
// exec session runtime the city runs, "" for any other runtime: v2-ready or
// not, and why; and warn, when v2 runs on it without input-fence (owner
// decision D2: not required, so v2 runs, but input gc fences to one
// incarnation, a fenced turn stop or response, is refused on it rather than
// sent unfenced).
func execRuntimeDoctorDetail(cfg *config.City, lookupEnv func(string) (string, bool), mode reconcilerMode) (detail, warn string) {
	name := v2SessionRuntimeName(cfg, lookupEnv)
	script, ok := execRuntimeScript(cfg, name)
	if !ok {
		return "", ""
	}
	h := execReadiness(script, cfg.EffectiveCityName())
	if !h.ready() {
		return fmt.Sprintf("exec session runtime %q: not v2-ready: %s", name, h.fix(script)), ""
	}
	detail = fmt.Sprintf("exec session runtime %q: v2-ready (%s)", name, strings.Join(sessionexec.V2ProfileCapabilities(), ", "))
	if mode == reconcilerV2 && (!h.info.Has(runtime.ProtocolCapabilityInputFence) || !h.info.Has(runtime.ProtocolCapabilityConnectionExec)) {
		warn = fmt.Sprintf("v2 runs on the exec session runtime %q, whose handshake does not declare %s (with %s): input gc fences to one incarnation (a fenced turn stop or response) is refused on it", name, runtime.ProtocolCapabilityInputFence, runtime.ProtocolCapabilityConnectionExec)
	}
	return detail, warn
}
