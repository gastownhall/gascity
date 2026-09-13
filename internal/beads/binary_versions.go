package beads

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doltversion"
)

// binaryVersionProbeTimeout bounds each `<binary> version` subprocess so a
// hung binary cannot stall the caller that resolves versions.
const binaryVersionProbeTimeout = 5 * time.Second

// ProbeBDVersion runs `bd version` and returns the semantic version token the
// bd CLI reports (e.g. "1.0.4"). bd subprocess execution is confined to this
// package by architectural rule (see boundary_test.go), so version probing
// for operator-facing surfaces lives here rather than in the API layer.
func ProbeBDVersion() (string, error) {
	out, err := probeBinaryVersion("bd")
	if err != nil {
		return "", err
	}
	return parseBDVersion(out)
}

// ProbeDoltVersion runs `dolt version` and returns the parsed version the dolt
// engine reports (e.g. "2.0.7"). Dolt is the store engine bd drives, so its
// version probe is colocated with bd's.
func ProbeDoltVersion() (string, error) {
	out, err := probeBinaryVersion("dolt")
	if err != nil {
		return "", err
	}
	info, err := doltversion.Parse(out)
	if err != nil {
		return "", err
	}
	return info.Raw, nil
}

// probeBinaryVersion locates name on PATH — the same resolution used when the
// binary is driven for real work — and runs `<name> version` under a bounded
// timeout, returning combined output.
func probeBinaryVersion(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), binaryVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("%s version timed out after %s", name, binaryVersionProbeTimeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s version: %w", name, err)
	}
	return string(out), nil
}

// parseBDVersion extracts the semantic version token from `bd version` output
// (e.g. "bd version 1.0.4 (ce242a879)" -> "1.0.4"). Mirrors doltversion.Parse's
// leniency: the "bd version " prefix, a leading "v", and any trailing
// build/commit descriptor are stripped.
func parseBDVersion(out string) (string, error) {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	const prefix = "bd version "
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
