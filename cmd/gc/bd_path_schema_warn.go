package main

import (
	"debug/buildinfo"
	"fmt"
	"io"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

var defaultPATHBdSchemaSkewCheck = pathBdSchemaSkewCheck{
	lookPath: exec.LookPath,
	gcPin:    linkedBeadsModuleVersion,
	binPin:   beadsModuleVersionFromBinary,
}

// warnPATHBdSchemaSkew writes a loud, once-per-process warning when the
// standalone bd on PATH is not built from the same beads commit this gc
// binary links. That skew is how a rebuilt gc auto-migrates a city store to
// a schema PATH bd cannot read.
//
// The warning never fails the caller: headless controllers must not wedge
// on a cosmetic pin skew. A missing or non-Go PATH bd is a different
// problem and is silent here.
var warnPATHBdSchemaSkew = defaultPATHBdSchemaSkewCheck.warn

type pathBdSchemaSkewCheck struct {
	once     sync.Once
	lookPath func(string) (string, error)
	gcPin    func() string
	binPin   func(string) (string, error)
}

func (c *pathBdSchemaSkewCheck) warn(w io.Writer) {
	if c == nil || w == nil {
		return
	}
	c.once.Do(func() {
		if msg := c.warning(); msg != "" {
			fmt.Fprintln(w, msg) //nolint:errcheck // best-effort stderr
		}
	})
}

func (c *pathBdSchemaSkewCheck) warning() string {
	if c == nil || c.lookPath == nil || c.gcPin == nil || c.binPin == nil {
		return ""
	}
	gcPin := strings.TrimSpace(c.gcPin())
	if gcPin == "" {
		return ""
	}
	bdPath, err := c.lookPath("bd")
	if err != nil || strings.TrimSpace(bdPath) == "" {
		return ""
	}
	bdPin, err := c.binPin(bdPath)
	if err != nil {
		return ""
	}
	bdPin = strings.TrimSpace(bdPin)
	if bdPin == "" || bdPin == gcPin {
		return ""
	}
	return formatPATHBdSchemaSkewWarning(bdPath, bdPin, gcPin)
}

func formatPATHBdSchemaSkewWarning(bdPath, bdPin, gcPin string) string {
	return strings.Join([]string{
		"gc: WARNING: standalone bd on PATH cannot keep up with this gc's beads schema",
		"  PATH bd:  " + bdPath + " (beads " + bdPin + ")",
		"  this gc:  beads " + gcPin,
		"  If this gc migrates the city store, every bare `bd` call will fail with",
		`  "schema version mismatch". Rebuild the matching bd: make install-bd`,
	}, "\n")
}

func linkedBeadsModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return contract.BeadsModuleVersion(info)
}

func beadsModuleVersionFromBinary(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	pin := contract.BeadsModuleVersion(info)
	if pin == "" {
		return "", fmt.Errorf("beads module not found in %s build info", path)
	}
	return pin, nil
}
