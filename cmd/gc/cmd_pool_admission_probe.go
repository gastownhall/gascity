package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

// pool-admission-probe checks the exact provider capability used by the
// start fence, without creating a session or touching the capacity ledger.
// It runs in a normal gc binary because go test deliberately forbids a live
// /proc scan.
func newPoolAdmissionProbeCmd(stdout, _ io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "pool-admission-probe",
		Short: "Read-only probe of the pool start live census",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			result := map[string]any{"city_path": cityPath, "ok": false}
			defer func() {
				_ = json.NewEncoder(stdout).Encode(result)
			}()
			// Use the already-materialized city configuration. loadCityConfig
			// can repair builtin pack caches, which would make this probe write.
			cfg, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), skipRevisionSnapshot)
			if err != nil {
				result["error"] = fmt.Sprintf("load city: %v", err)
				return errExit
			}
			if err := validatePackRuntimeRegistrations(cfg); err != nil {
				result["error"] = fmt.Sprintf("validate provider registry: %v", err)
				return errExit
			}
			sp, err := newSessionProviderForCity(cfg, cityPath)
			if err != nil {
				result["error"] = fmt.Sprintf("construct provider: %v", err)
				return errExit
			}
			result["provider_type"] = fmt.Sprintf("%T", sp)
			running, err := sp.ListRunning("")
			if err != nil {
				result["error"] = fmt.Sprintf("list running: %v", err)
				return errExit
			}
			result["running_count"] = len(running)
			scanner, ok := sp.(runtime.ProcessTableScanner)
			if !ok {
				result["error"] = "provider lacks ProcessTableScanner"
				return errExit
			}
			processes, err := scanner.FindRuntimesBySessionID("")
			if err != nil {
				result["error"] = fmt.Sprintf("process scan: %v", err)
				return errExit
			}
			result["process_root_count"] = len(processes)
			unknownCity := 0
			for _, process := range processes {
				if process.City == "" {
					unknownCity++
				}
			}
			result["unknown_city_roots"] = unknownCity
			if unknownCity > 0 {
				result["error"] = "process roots without city identity"
				return errExit
			}
			result["ok"] = true
			return nil
		},
	}
}
