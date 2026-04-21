package main

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func chooseManagedDoltPort(cityPath, stateFile string) (string, error) {
	cityPath = normalizePathForCompare(cityPath)
	if port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT")); port != "" {
		return port, nil
	}

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return "", err
	}
	canonicalStateFile := layout.StateFile
	if strings.TrimSpace(stateFile) == "" {
		stateFile = layout.StateFile
	} else {
		layout.StateFile = stateFile
	}

	if state, err := readDoltRuntimeStateFile(stateFile); err == nil {
		if validDoltRuntimeState(state, cityPath) {
			return strconv.Itoa(state.Port), nil
		}
		if repaired, ok := repairedManagedDoltRuntimeState(cityPath, layout, state); ok {
			if repaired != state {
				if err := writeDoltRuntimeStateFile(stateFile, repaired); err != nil {
					return "", fmt.Errorf("repair provider runtime state: %w", err)
				}
				if samePath(stateFile, canonicalStateFile) {
					if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
						return "", fmt.Errorf("publish repaired managed dolt runtime state: %w", err)
					}
				}
			}
			return strconv.Itoa(repaired.Port), nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read provider runtime state: %w", err)
	}
	seed := deterministicManagedDoltPortSeed(cityPath)
	return strconv.Itoa(nextAvailableManagedDoltPort(seed)), nil
}

func repairedManagedDoltRuntimeState(_ string, layout managedDoltRuntimeLayout, state doltRuntimeState) (doltRuntimeState, bool) {
	if state.Port <= 0 {
		return doltRuntimeState{}, false
	}
	if state.DataDir != "" && !samePath(state.DataDir, layout.DataDir) {
		return doltRuntimeState{}, false
	}
	port := strconv.Itoa(state.Port)
	holderPID := findPortHolderPID(port)
	if holderPID <= 0 {
		return doltRuntimeState{}, false
	}
	stateDir := strings.TrimSpace(state.DataDir)
	if stateDir == "" {
		stateDir = layout.DataDir
	}
	if !managedDoltProcessOwnedWithStateDir(holderPID, layout, stateDir) {
		return doltRuntimeState{}, false
	}
	if processHasDeletedDataInodes(holderPID, layout.DataDir) {
		return doltRuntimeState{}, false
	}
	managedPID, _ := findManagedDoltPID(layout, port)
	if managedPID <= 0 || managedPID != holderPID {
		return doltRuntimeState{}, false
	}
	if !managedDoltTCPReachable("127.0.0.1", port) {
		return doltRuntimeState{}, false
	}
	repaired := state
	repaired.Running = true
	repaired.PID = holderPID
	repaired.DataDir = layout.DataDir
	if strings.TrimSpace(repaired.StartedAt) == "" {
		repaired.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return repaired, true
}

func deterministicManagedDoltPortSeed(cityPath string) int {
	cityPath = normalizePathForCompare(cityPath)
	if seed, err := cksumManagedDoltPortSeed(cityPath); err == nil {
		return seed
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(cityPath))
	return int(hasher.Sum32()%50000) + 10000
}

func cksumManagedDoltPortSeed(cityPath string) (int, error) {
	cmd := exec.Command("cksum")
	cmd.Stdin = strings.NewReader(cityPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty cksum output")
	}
	value, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("parse cksum output %q: %w", fields[0], err)
	}
	return value%50000 + 10000, nil
}

func nextAvailableManagedDoltPort(seed int) int {
	port := seed
	for attempts := 0; attempts < 100; attempts++ {
		if port > 60000 {
			port = 10000
		}
		if managedDoltPortAvailable(port) {
			return port
		}
		port++
	}
	return seed
}

func managedDoltPortAvailable(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	defer listener.Close() //nolint:errcheck // best-effort cleanup
	return true
}
