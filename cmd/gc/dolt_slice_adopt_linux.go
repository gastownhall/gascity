//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const managedDoltAdoptInitialRetry = 5 * time.Millisecond

type managedDoltAdopter struct {
	ensureSlice     func(context.Context, string) error
	startScope      func(context.Context, string, string, string, []int) error
	inspect         func(string, string) (managedDoltProcessInspection, error)
	readState       func(string, int) (doltRuntimeState, error)
	readPPID        func(int) (int, error)
	readCmdline     func(int) ([]string, error)
	readEnviron     func(int) (map[string]string, error)
	readChildren    func(int) ([]int, error)
	readCgroup      func(int) (string, error)
	snapshot        func(int) (uint64, string)
	identityMatches func(int, uint64, string) bool
	executable      func() (string, error)
	configFile      func(string) (string, error)
	logFile         func(string) (string, error)
}

type managedDoltAdoptIdentity struct {
	pid      int
	ticks    uint64
	fallback string
}

type managedDoltAdoptTarget struct {
	state            doltRuntimeState
	watchdogIdentity managedDoltAdoptIdentity
	serverIdentity   managedDoltAdoptIdentity
}

func (target managedDoltAdoptTarget) identities() []managedDoltAdoptIdentity {
	if target.watchdogIdentity.pid > 1 {
		return []managedDoltAdoptIdentity{target.watchdogIdentity, target.serverIdentity}
	}
	return []managedDoltAdoptIdentity{target.serverIdentity}
}

func (target managedDoltAdoptTarget) pids() []int {
	identities := target.identities()
	pids := make([]int, 0, len(identities))
	for _, identity := range identities {
		pids = append(pids, identity.pid)
	}
	return pids
}

func (a managedDoltAdopter) adopt(ctx context.Context, cityPath, port, slice string) error {
	portNumber, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || portNumber <= 0 {
		return fmt.Errorf("adopt managed dolt: invalid port %q", port)
	}
	target, err := a.resolveTarget(cityPath, port, portNumber)
	if err != nil {
		return err
	}
	if err := a.ensureSlice(ctx, slice); err != nil {
		return fmt.Errorf("adopt managed dolt: %w", err)
	}
	if err := a.attachIfNeeded(ctx, slice, target); err != nil {
		return err
	}
	return a.verifyAfterAttach(cityPath, port, slice, target)
}

func (a managedDoltAdopter) attachIfNeeded(
	ctx context.Context,
	slice string,
	target managedDoltAdoptTarget,
) error {
	placed, err := a.alreadyPlaced(
		slice,
		target.identities()...,
	)
	if err != nil {
		return err
	}
	if placed {
		return nil
	}
	unit := fmt.Sprintf(
		"gcdolt-adopt-%d-%d.scope",
		target.serverIdentity.pid,
		target.serverIdentity.ticks,
	)
	if err := a.startScope(
		ctx,
		unit,
		slice,
		"Gas City managed Dolt adopted process tree",
		target.pids(),
	); err != nil {
		return fmt.Errorf("adopt managed dolt: attach watchdog/server: %w", err)
	}
	return a.waitForPlacement(
		ctx,
		slice,
		target.identities()...,
	)
}

func (a managedDoltAdopter) resolveTarget(
	cityPath, port string,
	portNumber int,
) (managedDoltAdoptTarget, error) {
	state, err := a.readState(cityPath, portNumber)
	if err != nil {
		return managedDoltAdoptTarget{}, fmt.Errorf("adopt managed dolt: read runtime state: %w", err)
	}
	if !state.Running || state.PID <= 1 || state.Port != portNumber {
		return managedDoltAdoptTarget{}, fmt.Errorf(
			"adopt managed dolt: runtime state running=%t pid=%d port=%d, want live port %d",
			state.Running, state.PID, state.Port, portNumber,
		)
	}
	inspection, err := a.inspect(cityPath, port)
	if err != nil {
		return managedDoltAdoptTarget{}, fmt.Errorf("adopt managed dolt: inspect owned process: %w", err)
	}
	if err := validateManagedDoltAdoptInspection(inspection, state.PID); err != nil {
		return managedDoltAdoptTarget{}, err
	}
	return a.resolveProcessTreeTarget(cityPath, state)
}

func (a managedDoltAdopter) resolveProcessTreeTarget(
	cityPath string,
	state doltRuntimeState,
) (managedDoltAdoptTarget, error) {
	serverPID := state.PID
	serverIdentity, err := a.captureIdentity(serverPID)
	if err != nil {
		return managedDoltAdoptTarget{}, err
	}

	watchdogPID, parentErr := a.readPPID(serverPID)
	watchdogErr := fmt.Errorf("invalid watchdog pid %d for server %d", watchdogPID, serverPID)
	if parentErr == nil && watchdogPID > 1 && watchdogPID != serverPID {
		watchdogErr = a.validateWatchdog(cityPath, watchdogPID)
		if watchdogErr == nil {
			if err := a.validateProcessTree(watchdogPID, serverPID); err != nil {
				return managedDoltAdoptTarget{}, err
			}
			watchdogIdentity, err := a.captureIdentity(watchdogPID)
			if err != nil {
				return managedDoltAdoptTarget{}, err
			}
			return managedDoltAdoptTarget{
				state:            state,
				watchdogIdentity: watchdogIdentity,
				serverIdentity:   serverIdentity,
			}, nil
		}
	} else if parentErr != nil {
		watchdogErr = fmt.Errorf("read server parent: %w", parentErr)
	}

	if err := a.validateDirectServer(cityPath, serverPID); err != nil {
		return managedDoltAdoptTarget{}, fmt.Errorf(
			"adopt managed dolt: process is neither canonical watchdog child (%w) nor direct managed server (%w)",
			watchdogErr,
			err,
		)
	}
	return managedDoltAdoptTarget{
		state:          state,
		serverIdentity: serverIdentity,
	}, nil
}

func (a managedDoltAdopter) waitForPlacement(
	ctx context.Context,
	slice string,
	identities ...managedDoltAdoptIdentity,
) error {
	delay := managedDoltAdoptInitialRetry
	var last []string
	for {
		placed, status, err := a.placementStatusWhileWaiting(slice, identities...)
		if err != nil {
			return err
		}
		last = status
		if placed {
			return nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf(
				"adopt managed dolt: wait for %s placement: %w (%s)",
				slice,
				ctx.Err(),
				strings.Join(last, ", "),
			)
		case <-timer.C:
		}
		if delay < 100*time.Millisecond {
			delay *= 2
			if delay > 100*time.Millisecond {
				delay = 100 * time.Millisecond
			}
		}
	}
}

func (a managedDoltAdopter) alreadyPlaced(
	slice string,
	identities ...managedDoltAdoptIdentity,
) (bool, error) {
	placed := true
	for _, identity := range identities {
		if !a.identityMatches(identity.pid, identity.ticks, identity.fallback) {
			return false, fmt.Errorf(
				"adopt managed dolt: pid %d identity changed while checking placement",
				identity.pid,
			)
		}
		cgroup, err := a.readCgroup(identity.pid)
		if err != nil {
			return false, fmt.Errorf("adopt managed dolt: read pid %d cgroup: %w", identity.pid, err)
		}
		if !managedDoltCgroupContainsSlice(cgroup, slice) {
			placed = false
		}
	}
	return placed, nil
}

func (a managedDoltAdopter) placementStatusWhileWaiting(
	slice string,
	identities ...managedDoltAdoptIdentity,
) (bool, []string, error) {
	placed := true
	status := make([]string, 0, len(identities))
	for _, identity := range identities {
		if !a.identityMatches(identity.pid, identity.ticks, identity.fallback) {
			return false, nil, fmt.Errorf(
				"adopt managed dolt: pid %d identity changed while checking placement",
				identity.pid,
			)
		}
		cgroup, err := a.readCgroup(identity.pid)
		if err != nil {
			placed = false
			status = append(status, fmt.Sprintf("pid=%d error=%v", identity.pid, err))
			continue
		}
		status = append(status, fmt.Sprintf("pid=%d cgroup=%q", identity.pid, cgroup))
		if !managedDoltCgroupContainsSlice(cgroup, slice) {
			placed = false
		}
	}
	return placed, status, nil
}

func validateManagedDoltAdoptInspection(info managedDoltProcessInspection, serverPID int) error {
	if info.ManagedPID != serverPID || !info.ManagedOwned || info.ManagedDeletedInodes {
		return fmt.Errorf(
			"adopt managed dolt: managed pid ownership mismatch pid=%d owned=%t deleted=%t, want pid=%d",
			info.ManagedPID, info.ManagedOwned, info.ManagedDeletedInodes, serverPID,
		)
	}
	if info.PortHolderPID != serverPID || !info.PortHolderOwned || info.PortHolderDeletedInodes {
		return fmt.Errorf(
			"adopt managed dolt: port-holder ownership mismatch pid=%d owned=%t deleted=%t, want pid=%d",
			info.PortHolderPID, info.PortHolderOwned, info.PortHolderDeletedInodes, serverPID,
		)
	}
	return nil
}

func (a managedDoltAdopter) validateWatchdog(cityPath string, watchdogPID int) error {
	argv, err := a.readCmdline(watchdogPID)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: read watchdog argv: %w", err)
	}
	executable, err := a.executable()
	if err != nil {
		return fmt.Errorf("adopt managed dolt: resolve gc executable: %w", err)
	}
	configFile, err := a.configFile(cityPath)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: resolve config: %w", err)
	}
	logFile, err := a.logFile(cityPath)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: resolve log: %w", err)
	}
	if len(argv) != 5 ||
		!samePath(argv[0], executable) ||
		argv[1] != managedDoltScopeWatchdogArg ||
		!samePath(argv[2], configFile) ||
		!samePath(argv[3], logFile) ||
		!samePath(argv[4], cityPath) {
		return fmt.Errorf("adopt managed dolt: watchdog argv does not match canonical managed process: %q", argv)
	}
	return nil
}

func (a managedDoltAdopter) validateProcessTree(watchdogPID, serverPID int) error {
	watchdogChildren, err := a.readChildren(watchdogPID)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: read watchdog children: %w", err)
	}
	if len(watchdogChildren) != 1 || watchdogChildren[0] != serverPID {
		return fmt.Errorf(
			"adopt managed dolt: watchdog children %v, want only server pid %d",
			watchdogChildren, serverPID,
		)
	}
	serverChildren, err := a.readChildren(serverPID)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: read server children: %w", err)
	}
	if len(serverChildren) != 0 {
		return fmt.Errorf("adopt managed dolt: unverified server descendants %v", serverChildren)
	}
	return nil
}

func (a managedDoltAdopter) validateDirectServer(cityPath string, serverPID int) error {
	argv, err := a.readCmdline(serverPID)
	if err != nil {
		return fmt.Errorf("read direct server argv: %w", err)
	}
	configFile, err := a.configFile(cityPath)
	if err != nil {
		return fmt.Errorf("resolve direct server config: %w", err)
	}
	if len(argv) != 4 ||
		filepath.Base(argv[0]) != "dolt" ||
		argv[1] != "sql-server" ||
		argv[2] != "--config" ||
		!samePath(argv[3], configFile) {
		return fmt.Errorf("direct server argv does not match canonical managed process: %q", argv)
	}
	environ, err := a.readEnviron(serverPID)
	if err != nil {
		return fmt.Errorf("read direct server environment: %w", err)
	}
	if environ[managedDoltProcessSentinelEnv] != managedDoltProcessSentinelValue {
		return fmt.Errorf("direct server is missing managed-process sentinel")
	}
	for _, key := range managedDoltSessionAttributionEnvKeys {
		if _, present := environ[key]; present {
			return fmt.Errorf("direct server retains session attribution %s", key)
		}
	}
	children, err := a.readChildren(serverPID)
	if err != nil {
		return fmt.Errorf("read direct server children: %w", err)
	}
	if len(children) != 0 {
		return fmt.Errorf("direct server has unverified descendants %v", children)
	}
	return nil
}

func (a managedDoltAdopter) captureIdentity(pid int) (managedDoltAdoptIdentity, error) {
	ticks, fallback := a.snapshot(pid)
	if ticks == 0 && strings.TrimSpace(fallback) == "" {
		return managedDoltAdoptIdentity{}, fmt.Errorf("adopt managed dolt: cannot snapshot pid %d identity", pid)
	}
	return managedDoltAdoptIdentity{pid: pid, ticks: ticks, fallback: fallback}, nil
}

func (a managedDoltAdopter) verifyAfterAttach(
	cityPath, port, slice string,
	target managedDoltAdoptTarget,
) error {
	for _, identity := range target.identities() {
		if !a.identityMatches(identity.pid, identity.ticks, identity.fallback) {
			return fmt.Errorf("adopt managed dolt: pid %d identity changed during attach", identity.pid)
		}
		cgroup, err := a.readCgroup(identity.pid)
		if err != nil {
			return fmt.Errorf("adopt managed dolt: read pid %d cgroup after attach: %w", identity.pid, err)
		}
		if !managedDoltCgroupContainsSlice(cgroup, slice) {
			return fmt.Errorf("adopt managed dolt: pid %d cgroup %q is outside %s", identity.pid, cgroup, slice)
		}
	}
	afterState, err := a.readState(cityPath, target.state.Port)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: re-read runtime state: %w", err)
	}
	if afterState.Running != target.state.Running ||
		afterState.PID != target.state.PID ||
		afterState.Port != target.state.Port {
		return fmt.Errorf(
			"adopt managed dolt: runtime state changed during attach: before=%+v after=%+v",
			target.state, afterState,
		)
	}
	afterInspection, err := a.inspect(cityPath, port)
	if err != nil {
		return fmt.Errorf("adopt managed dolt: re-inspect owned process: %w", err)
	}
	if err := validateManagedDoltAdoptInspection(afterInspection, target.state.PID); err != nil {
		return err
	}
	if target.watchdogIdentity.pid > 1 {
		return a.validateProcessTree(target.watchdogIdentity.pid, target.serverIdentity.pid)
	}
	return a.validateDirectServer(cityPath, target.serverIdentity.pid)
}

func managedDoltCgroupContainsSlice(cgroup, slice string) bool {
	slice = strings.TrimSpace(slice)
	for _, line := range strings.Split(cgroup, "\n") {
		path := line
		if index := strings.Index(line, "::"); index >= 0 {
			path = line[index+2:]
		}
		for _, segment := range strings.Split(path, "/") {
			if segment == slice {
				return true
			}
		}
	}
	return false
}

func defaultManagedDoltAdopter() managedDoltAdopter {
	return managedDoltAdopter{
		ensureSlice:     prepareManagedDoltPlacement,
		startScope:      managedDoltStartScopeWithProcesses,
		inspect:         inspectManagedDoltProcess,
		readState:       readManagedDoltAdoptState,
		readPPID:        readManagedDoltProcessPPID,
		readCmdline:     readManagedDoltProcessCmdline,
		readEnviron:     readManagedDoltProcessEnviron,
		readChildren:    readManagedDoltProcessChildren,
		readCgroup:      readManagedDoltProcessCgroup,
		snapshot:        snapshotManagedDoltStartIdentity,
		identityMatches: managedDoltPIDStartIdentityMatches,
		executable:      os.Executable,
		configFile: func(cityPath string) (string, error) {
			layout, err := resolveManagedDoltRuntimeLayout(cityPath)
			return layout.ConfigFile, err
		},
		logFile: func(cityPath string) (string, error) {
			layout, err := resolveManagedDoltRuntimeLayout(cityPath)
			return layout.LogFile, err
		},
	}
}

func adoptManagedDoltPlacement(cityPath, port string) error {
	slice := managedDoltSlice()
	if strings.TrimSpace(slice) == "" {
		return fmt.Errorf("adopt managed dolt: required slice is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltPlacementOperationTimeout)
	defer cancel()
	return defaultManagedDoltAdopter().adopt(ctx, cityPath, port, slice)
}

func readManagedDoltAdoptState(cityPath string, port int) (doltRuntimeState, error) {
	paths := []string{providerManagedDoltStatePath(cityPath), managedDoltStatePath(cityPath)}
	var failures []string
	for _, path := range paths {
		state, err := readDoltRuntimeStateFile(path)
		if err == nil && state.Running && state.PID > 1 && state.Port == port {
			return state, nil
		}
		if err == nil {
			failures = append(
				failures,
				fmt.Sprintf(
					"%s: running=%t pid=%d port=%d, want live port=%d",
					filepath.Base(path), state.Running, state.PID, state.Port, port,
				),
			)
			continue
		}
		failures = append(failures, filepath.Base(path)+": "+err.Error())
	}
	return doltRuntimeState{}, fmt.Errorf("%s", strings.Join(failures, "; "))
}

func readManagedDoltProcessPPID(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	closeParen := strings.LastIndex(string(data), ")")
	if closeParen < 0 {
		return 0, fmt.Errorf("malformed /proc stat")
	}
	fields := strings.Fields(string(data)[closeParen+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("short /proc stat")
	}
	return strconv.Atoi(fields[1])
}

func readManagedDoltProcessCmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	return splitCmdline(data), nil
}

func readManagedDoltProcessEnviron(pid int) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	environ := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
		if key, value, ok := strings.Cut(entry, "="); ok && key != "" {
			environ[key] = value
		}
	}
	return environ, nil
}

func readManagedDoltProcessChildren(pid int) ([]int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data))
	children := make([]int, 0, len(fields))
	for _, field := range fields {
		child, err := strconv.Atoi(field)
		if err != nil || child <= 1 {
			return nil, fmt.Errorf("invalid child pid %q", field)
		}
		children = append(children, child)
	}
	return children, nil
}

func readManagedDoltProcessCgroup(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
