package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/spf13/cobra"
)

// haltFileName is the base name of the flag file that pauses the
// per-city reconciliation tick. It lives under <city>/.gc/runtime/.
const haltFileName = "halt"

// haltFilePath returns the absolute path to the halt flag file for a city.
func haltFilePath(cityPath string) string {
	return filepath.Join(citylayout.RuntimeDataDir(cityPath), haltFileName)
}

// isCityHalted reports whether the halt flag file exists for a city.
// Any error other than "not exist" is treated as "not halted" so a
// permission glitch cannot accidentally pause the supervisor forever.
func isCityHalted(cityPath string) bool {
	_, err := os.Stat(haltFilePath(cityPath))
	return err == nil
}

// writeHaltFile creates the halt flag file for a city, ensuring the
// runtime directory exists. Idempotent: calling it when the file is
// already present is a no-op (the existing file is left in place).
func writeHaltFile(cityPath string) error {
	runtimeDir := citylayout.RuntimeDataDir(cityPath)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	path := haltFilePath(cityPath)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat halt file: %w", err)
	}
	// Write a short marker payload so operators who cat the file get
	// a hint about what it is.
	payload := []byte("supervisor halted; remove this file to resume\n")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write halt file: %w", err)
	}
	return nil
}

// removeHaltFile deletes the halt flag file for a city. Idempotent:
// returns nil if the file does not exist.
func removeHaltFile(cityPath string) error {
	err := os.Remove(haltFilePath(cityPath))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove halt file: %w", err)
}

// newHaltCmd creates the "gc halt [path]" command.
func newHaltCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "halt [path]",
		Short: "Pause the supervisor reconciliation tick",
		Long: `Halt the supervisor reconciliation tick for a city by creating
a flag file at <city>/.gc/runtime/halt. While the flag is present the
supervisor loop skips tick work (no session wakes, no convergence,
no order dispatch) but keeps the process alive, logs, and control
socket responsive.

This is a soft circuit breaker for emergencies: it stops disk thrash
from a runaway reconciler without requiring "systemctl stop". The
supervisor process itself is not killed.

Idempotent. Use "gc resume" to clear the flag.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHalt(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdHalt is the CLI entry point for "gc halt".
func cmdHalt(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc halt: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeHaltFile(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc halt: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "City halted (%s)\n", haltFilePath(cityPath)) //nolint:errcheck // best-effort stdout
	return 0
}

// haltGate tracks the most recently observed halt state for a single
// reconciliation loop so the transition log fires only once per
// running → halted transition. It is not safe for concurrent use;
// each CityRuntime owns one.
type haltGate struct {
	halted bool
}

// check returns true if the tick should be skipped because the halt
// flag file is present. On the transition from running → halted it
// writes a one-shot log line to the provided writer. On the transition
// back it writes a resume log line.
func (g *haltGate) check(cityPath string, stderr io.Writer) bool {
	halted := isCityHalted(cityPath)
	if halted && !g.halted {
		fmt.Fprintf(stderr, "supervisor: halted (remove %s to resume)\n", //nolint:errcheck // best-effort stderr
			filepath.Join(".gc", "runtime", haltFileName))
	} else if !halted && g.halted {
		fmt.Fprintln(stderr, "supervisor: resumed") //nolint:errcheck // best-effort stderr
	}
	g.halted = halted
	return halted
}
