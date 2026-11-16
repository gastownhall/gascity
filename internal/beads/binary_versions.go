package beads

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/doltversion"
	"golang.org/x/sync/singleflight"
)

// binaryVersionProbeTimeout bounds each `<binary> version` subprocess so a
// hung binary cannot stall the caller that resolves versions.
const binaryVersionProbeTimeout = 5 * time.Second

// ProbeBDVersion runs `bd version` and returns the semantic version token the
// bd CLI reports (e.g. "1.0.4"). bd subprocess execution is confined to this
// package by architectural rule (see boundary_test.go), so version probing
// for operator-facing surfaces lives here rather than in the API layer.
//
// preferredBin, when non-empty, is resolved instead of the bare "bd" name on
// PATH — a city's pinned BD_BIN (city.toml workspace.env), so the reported
// version is the binary that actually drives the city's bd work, not
// whichever "bd" happens to be first on the caller's PATH.
//
// bd runs with its telemetry switched off (bdVersionProbeEnv), so the probe
// never exports or waits on a collector.
func ProbeBDVersion(preferredBin string) (string, error) {
	out, err := probeBinaryVersion("bd", preferredBin, bdVersionProbeEnv())
	if err != nil {
		return "", err
	}
	return parseBDVersion(out)
}

// ProbeDoltVersion runs `dolt version` and returns the parsed version the dolt
// engine reports (e.g. "2.0.7"). Dolt is the store engine bd drives, so its
// version probe is colocated with bd's. Dolt has no per-city pin equivalent to
// BD_BIN, so it is always resolved on PATH.
func ProbeDoltVersion() (string, error) {
	out, err := probeBinaryVersion("dolt", "", nil)
	if err != nil {
		return "", err
	}
	info, err := doltversion.Parse(out)
	if err != nil {
		return "", err
	}
	return info.Raw, nil
}

// probeBinaryVersion locates the binary to probe and runs `<name> version`
// under a bounded timeout, returning combined output. preferredBin, when
// non-empty, is resolved (via exec.LookPath, so a relative path still
// resolves and permission/existence are checked the same way) instead of
// name on PATH — the same resolution used when the binary is driven for real
// work. env, when non-nil, is the probe's whole environment; nil inherits
// this process's environment.
func probeBinaryVersion(name, preferredBin string, env []string) (string, error) {
	lookup := name
	if preferredBin != "" {
		lookup = preferredBin
	}
	path, err := exec.LookPath(lookup)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), binaryVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("%s version timed out after %s", name, binaryVersionProbeTimeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s version: %w", name, err)
	}
	return string(out), nil
}

// bdVersionProbeEnv returns this process's environment without the BD_OTEL_*
// names that switch on bd's telemetry. With all four unset, no bd release
// starts an exporter, so `bd version` cannot sit in an exit-time flush to a
// slow collector until binaryVersionProbeTimeout. OTEL_SDK_DISABLED would not
// do: bd v1.0.4 and v1.2.2 ignore it.
func bdVersionProbeEnv() []string {
	env := os.Environ()
	for _, key := range []string{
		bdLegacyOTELMetricsURLKey,
		bdLegacyOTELLogsURLKey,
		bdLegacyOTELStdoutKey,
		bdOTELEnabledEnvKey,
	} {
		env = envWithout(env, key)
	}
	return env
}

// parseBDVersion extracts the semantic version token from `bd version` output
// (e.g. "bd version 1.0.4 (ce242a879)" -> "1.0.4"). The token is read from the
// first line that starts with "bd version ", so a warning bd prints ahead of
// it does not hide the version; output without such a line is read from its
// first line. Mirrors doltversion.Parse's leniency: the "bd version " prefix,
// a leading "v", and any trailing build/commit descriptor are stripped.
func parseBDVersion(out string) (string, error) {
	const prefix = "bd version "
	lines := strings.Split(strings.TrimSpace(out), "\n")
	line := strings.TrimSpace(lines[0])
	for _, candidate := range lines {
		candidate = strings.TrimSpace(candidate)
		if strings.HasPrefix(strings.ToLower(candidate), prefix) {
			line = candidate
			break
		}
	}
	if strings.HasPrefix(strings.ToLower(line), prefix) {
		line = line[len(prefix):]
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "v"))
	if line == "" || line[0] < '0' || line[0] > '9' {
		return "", fmt.Errorf("no version token in bd version output")
	}
	return line, nil
}

// bdVersionReadsStandardOTELEnv reports whether a bd of the given version reads
// BD_OTEL_ENABLED plus the standard OTEL_EXPORTER_OTLP_*_ENDPOINT names (beads
// commit 1cf7490c71, #3859). That change shipped in v1.2.0, v1.2.1 and every
// release from v1.3.0 on, but not in v1.2.2, which was cut from the v1.1 line,
// so no single minimum version describes it. Any other bd reads only the legacy
// BD_OTEL_* names. Prerelease suffixes are ignored: v1.3.0-rc.1 has the change,
// v1.2.2-rc.1 does not.
func bdVersionReadsStandardOTELEnv(version string) bool {
	v := deps.ParseVersion(version)
	switch {
	case v[0] != 1:
		return v[0] > 1
	case v[1] == 2:
		// v1.2.2 was cut from the v1.1 line without 1cf7490c71. Re-check this
		// arm against the beads tags whenever a new v1.2.x is released.
		return v[2] <= 1
	default:
		return v[1] > 2
	}
}

// bdStandardOTELEnvByBinary caches bdVersionReadsStandardOTELEnv per bd
// binary, keyed by resolved path, size and modification time so a binary
// replaced in place is probed again. bdStandardOTELEnvProbes collapses
// concurrent probes of one binary into a single `bd version`.
var (
	bdStandardOTELEnvByBinary sync.Map // binary key -> bool
	bdStandardOTELEnvProbes   singleflight.Group
)

// bdReadsStandardOTELEnv reports whether the bd binary execName resolves to
// reads the standard OTEL names, probing `bd version` once per binary. When
// the answer is unknown it reports false, which keeps the legacy names every
// bd still honors: the binary cannot be resolved, its version cannot be read
// (logged once per binary), or ctx ends first. A probe abandoned because ctx
// ended keeps running and caches its answer for the next command.
//
// The probe costs one extra serial `bd version` exec per bd binary per
// process, about 0.1-0.15s, paid only by processes whose bd env carries a
// legacy BD_OTEL_*_URL without BD_OTEL_STDOUT=true. A bd command issued before
// the answer is cached waits for the probe, at most until ctx ends or
// binaryVersionProbeTimeout elapses.
func bdReadsStandardOTELEnv(ctx context.Context, execName string) bool {
	path, err := exec.LookPath(execName)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	key := fmt.Sprintf("%s\x00%d\x00%d", path, info.Size(), info.ModTime().UnixNano())
	if reads, ok := bdStandardOTELEnvByBinary.Load(key); ok {
		return reads.(bool)
	}
	probe := bdStandardOTELEnvProbes.DoChan(key, func() (any, error) {
		version, err := ProbeBDVersion(path)
		if err != nil {
			log.Printf("beads: passing legacy BD_OTEL_* names to %s unchanged: %v", path, err)
		}
		reads := err == nil && bdVersionReadsStandardOTELEnv(version)
		bdStandardOTELEnvByBinary.Store(key, reads)
		return reads, nil
	})
	select {
	case result := <-probe:
		return result.Val.(bool)
	case <-ctx.Done():
		return false
	}
}
