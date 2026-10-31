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
	envPort := strings.TrimSpace(os.Getenv("GC_DOLT_PORT"))

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
		if hint, found, hintErr := readPublishedDoltRuntimeStateHint(cityPath); hintErr == nil && found {
			if repaired, ok := repairedManagedDoltRuntimeState(cityPath, layout, hint); ok {
				if err := writeDoltRuntimeStateFile(stateFile, repaired); err != nil {
					return "", fmt.Errorf("repair provider runtime state from published hint: %w", err)
				}
				if samePath(stateFile, canonicalStateFile) {
					if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
						return "", fmt.Errorf("publish repaired managed dolt runtime state: %w", err)
					}
				}
				return strconv.Itoa(repaired.Port), nil
			}
		}
		if liveOwnedDoltRuntimeState(state, cityPath) {
			return strconv.Itoa(state.Port), nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read provider runtime state: %w", err)
	} else if hint, found, hintErr := readPublishedDoltRuntimeStateHint(cityPath); hintErr == nil && found {
		if repaired, ok := repairedManagedDoltRuntimeState(cityPath, layout, hint); ok {
			if err := writeDoltRuntimeStateFile(stateFile, repaired); err != nil {
				return "", fmt.Errorf("repair missing provider runtime state: %w", err)
			}
			if samePath(stateFile, canonicalStateFile) {
				if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
					return "", fmt.Errorf("publish repaired managed dolt runtime state: %w", err)
				}
			}
			return strconv.Itoa(repaired.Port), nil
		}
		if liveOwnedDoltRuntimeState(hint, cityPath) {
			return strconv.Itoa(hint.Port), nil
		}
	}
	if envPort != "" {
		return envPort, nil
	}
	if port, ok := freeRecordedManagedDoltPort(cityPath, stateFile, layout); ok {
		return strconv.Itoa(port), nil
	}
	seed := deterministicManagedDoltPortSeed(cityPath)
	return strconv.Itoa(nextAvailableManagedDoltPort(seed)), nil
}

// freeRecordedManagedDoltPort returns the port the city last recorded for its
// managed Dolt when no live server claimed it above and nothing else listens
// on it now. Running sessions pin the port they were spawned with, so a
// restart after the server died has to come back on it rather than re-walk
// from the seed. The record is the provider state, or the published hint when
// there is no provider state file; unlike validDoltRuntimeState it does not
// require Running or a live pid.
func freeRecordedManagedDoltPort(cityPath, stateFile string, layout managedDoltRuntimeLayout) (int, bool) {
	state, err := readDoltRuntimeStateFile(stateFile)
	if os.IsNotExist(err) {
		hint, found, hintErr := readPublishedDoltRuntimeStateHint(cityPath)
		if hintErr != nil || !found {
			return 0, false
		}
		state = hint
	} else if err != nil {
		return 0, false
	}
	if state.Port <= 0 || !samePath(strings.TrimSpace(state.DataDir), layout.DataDir) {
		return 0, false
	}
	return state.Port, managedDoltPortAvailable(state.Port)
}

// liveOwnedDoltRuntimeState reports whether state still names the city's own
// managed Dolt: the recorded process is alive, is this city's server, and no
// other process holds the port. Unlike validDoltRuntimeState it does not dial
// the port. A restart stops that server before it binds, so its port is the
// one to reuse, and a dial can fail while the server is healthy, for example
// when the host has run out of ephemeral ports.
func liveOwnedDoltRuntimeState(state doltRuntimeState, cityPath string) bool {
	layout, ok := validDoltRuntimeStateIdentity(state, cityPath)
	if !ok || !pidAlive(state.PID) {
		return false
	}
	return managedDoltRuntimeProcessOwned(state, layout)
}

func repairedManagedDoltRuntimeState(_ string, layout managedDoltRuntimeLayout, state doltRuntimeState) (doltRuntimeState, bool) {
	if state.Port <= 0 {
		return doltRuntimeState{}, false
	}
	if state.DataDir != "" && !samePath(state.DataDir, layout.DataDir) {
		return doltRuntimeState{}, false
	}
	port := strconv.Itoa(state.Port)
	holderPID := findPortHolderPID(port, state.PID)
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
	if !managedDoltPortReachableOrHeldBy(port, holderPID) {
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

// nextAvailableManagedDoltPortForHost is the host-aware variant used by
// startManagedDoltProcessWithOptions after a host-aware wait on the original
// port has failed. Using the same host as the eventual bind avoids picking a
// port that probes free on 127.0.0.1 but is actually busy on the bind host
// (e.g. another process holds 192.168.1.5:X while leaving 127.0.0.1:X free,
// and dolt is binding 0.0.0.0:X, which would fail). Blank host normalizes
// to the loopback bind default inside managedDoltPortAvailableFn (the
// indirection over managedDoltPortAvailableForHost) to match the bind
// default in startManagedDoltProcessWithOptions.
func nextAvailableManagedDoltPortForHost(host string, seed int) int {
	port := seed
	for attempts := 0; attempts < 100; attempts++ {
		if port > 60000 {
			port = 10000
		}
		if managedDoltPortAvailableFn(host, port) {
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
