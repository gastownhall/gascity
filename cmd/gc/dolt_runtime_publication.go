package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func providerManagedDoltStatePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
}

func readDoltRuntimeStateFile(path string) (doltRuntimeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return doltRuntimeState{}, err
	}
	var state doltRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return doltRuntimeState{}, err
	}
	return state, nil
}

func writeDoltRuntimeStateFile(path string, state doltRuntimeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

func removeDoltRuntimeStateFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func managedDoltLifecycleOwned(cityPath string) (bool, error) {
	if cityUsesBdStoreContract(cityPath) {
		_, _, ok, invalid := resolveConfiguredCityDoltTarget(cityPath)
		if invalid {
			return false, fmt.Errorf("invalid canonical city endpoint state")
		}
		return !ok, nil
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("load city config for managed dolt ownership: %w", err)
	}
	if cfg == nil {
		return false, nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	cityState, err := syncDesiredCityDoltConfigState(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg))
	if err != nil {
		return false, err
	}
	if cityState.EndpointOrigin != contract.EndpointOriginManagedCity {
		return false, nil
	}
	for _, rig := range cfg.Rigs {
		if !rigUsesManagedBdStoreContract(cityPath, rig) {
			continue
		}
		rigState, err := syncDesiredRigDoltConfigState(cityPath, rig, cityState)
		if err != nil {
			return false, err
		}
		if rigState.EndpointOrigin == contract.EndpointOriginInheritedCity {
			return true, nil
		}
	}
	return false, nil
}

func syncManagedDoltPortMirrors(cityPath string) error {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		removeDoltPortFile(cityPath)
		return nil
	}
	return syncConfiguredDoltPortFiles(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg), cfg.Rigs)
}

func publishManagedDoltRuntimeState(cityPath string) error {
	state, err := readDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath))
	if err != nil {
		return fmt.Errorf("read provider dolt runtime state: %w", err)
	}
	if !validDoltRuntimeState(state, cityPath) {
		return fmt.Errorf("invalid managed dolt runtime state")
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		return fmt.Errorf("write published dolt runtime state: %w", err)
	}
	if err := syncManagedDoltPortMirrors(cityPath); err != nil {
		return fmt.Errorf("sync managed dolt port mirrors: %w", err)
	}
	return nil
}

func clearManagedDoltRuntimeState(cityPath string) error {
	if err := removeDoltRuntimeStateFile(managedDoltStatePath(cityPath)); err != nil {
		return fmt.Errorf("remove published dolt runtime state: %w", err)
	}
	if err := syncManagedDoltPortMirrors(cityPath); err != nil {
		return fmt.Errorf("sync managed dolt port mirrors: %w", err)
	}
	return nil
}

func publishManagedDoltRuntimeStateIfOwned(cityPath string) error {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return publishManagedDoltRuntimeState(cityPath)
}

func clearManagedDoltRuntimeStateIfOwned(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) {
		return clearManagedDoltRuntimeState(cityPath)
	}
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return clearManagedDoltRuntimeState(cityPath)
}
