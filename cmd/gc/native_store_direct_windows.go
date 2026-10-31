//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"golang.org/x/sys/windows"
)

// providerOwnedDirectProbeTarget returns the Dolt server bd runs for scope
// when the scope is provider-owned, ready, and bound to bd's direct transport
// on this machine: the scope resolves (contract.ResolveDoltConnectionTarget)
// to the local server-mode Dolt bd recorded in the scope's own
// .beads/dolt-server.pid and .port, while that process is alive, is dolt.exe,
// owns the recorded listener, and the port answers. ok is false for every
// other scope, including one that resolves anywhere but bd's own server, which
// keeps the canonical target as the only one probed.
//
// bd writes such a scope's config, so it never resolves as canonical, and the
// identity check would otherwise have no endpoint to read the database's
// project_id from. The check is not relaxed: the project_id is still read from
// this server and compared with the scope's metadata.
func providerOwnedDirectProbeTarget(cityPath, scope string) (contract.DoltConnectionTarget, bool) {
	entry, owned, err := providerOwnedScopeState(cityPath, scope)
	if err != nil || !owned || entry.State != providerScopeReady {
		return contract.DoltConnectionTarget{}, false
	}
	bdPort, ok := contract.ProviderOwnedServerPort(fsys.OSFS{}, scope)
	if !ok {
		return contract.DoltConnectionTarget{}, false
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scope)
	if err != nil || target.DoltMode != "server" || target.External || target.Port != bdPort {
		return contract.DoltConnectionTarget{}, false
	}
	if !contract.DoltHostIsLocal(target.Host) || !providerOwnedDirectServerRecordMatches(scope, target.Host, target.Port) {
		return contract.DoltConnectionTarget{}, false
	}
	return target, true
}

func providerOwnedDirectServerRecordMatches(scope, host, port string) bool {
	pid, recordedPort, ok := readProviderOwnedDirectServerRecord(scope)
	if !ok || recordedPort != port {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	waitResult, waitErr := windows.WaitForSingleObject(handle, 0)
	if waitErr != nil || waitResult != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	if !providerOwnedDirectProcessImageIsDolt(handle) {
		return false
	}
	return providerOwnedDirectListenerOwnedByPID(host, port, pid)
}

func readProviderOwnedDirectServerRecord(scope string) (int, string, bool) {
	pidRaw, err := os.ReadFile(filepath.Join(scope, ".beads", "dolt-server.pid"))
	if err != nil {
		return 0, "", false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	portRaw, err := os.ReadFile(filepath.Join(scope, ".beads", "dolt-server.port"))
	if err != nil {
		return 0, "", false
	}
	port := strings.TrimSpace(string(portRaw))
	if value, err := strconv.Atoi(port); err != nil || value <= 0 {
		return 0, "", false
	}
	return pid, port, true
}

func providerOwnedDirectProcessImageIsDolt(handle windows.Handle) bool {
	for size := uint32(windows.MAX_PATH); ; size *= 2 {
		buffer := make([]uint16, size)
		length := size
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer[:length])), "dolt.exe")
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size >= 32768 {
			return false
		}
	}
}

type tcpListenerFamily uint8

const (
	tcpListenerIPv4 tcpListenerFamily = iota
	tcpListenerIPv6
)

const (
	tcpStateListen              = 2
	tcpTableOwnerPIDListener    = 3
	mibTCPRowOwnerPIDSize       = 24
	mibTCP6RowOwnerPIDSize      = 56
	getExtendedTCPTableOrdering = 1
)

var getExtendedTCPTableProc = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func providerOwnedDirectListenerOwnedByPID(host, port string, pid int) bool {
	portValue, err := strconv.ParseUint(strings.TrimSpace(port), 10, 16)
	if err != nil || portValue == 0 || pid <= 0 {
		return false
	}
	families, ok := providerOwnedDirectTCPFamilies(host)
	if !ok {
		return false
	}
	found := false
	for _, family := range families {
		var pids []int
		var err error
		switch family {
		case tcpListenerIPv4:
			pids, err = providerOwnedDirectTCP4ListenerPIDs(uint16(portValue), host)
		case tcpListenerIPv6:
			pids, err = providerOwnedDirectTCP6ListenerPIDs(uint16(portValue), host)
		}
		if err != nil {
			return false
		}
		for _, owner := range pids {
			if owner != pid {
				return false
			}
			found = true
		}
	}
	return found
}

func providerOwnedDirectTCPFamilies(host string) ([]tcpListenerFamily, bool) {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" || host == "localhost" {
		return []tcpListenerFamily{tcpListenerIPv4, tcpListenerIPv6}, true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || (!addr.IsLoopback() && !addr.IsUnspecified()) {
		return nil, false
	}
	if addr.Is4() {
		return []tcpListenerFamily{tcpListenerIPv4}, true
	}
	return []tcpListenerFamily{tcpListenerIPv6}, true
}

func providerOwnedDirectTCP4ListenerPIDs(port uint16, host string) ([]int, error) {
	table, err := providerOwnedDirectExtendedTCPTable(windows.AF_INET)
	if err != nil {
		return nil, err
	}
	if len(table) < 4 {
		return nil, errors.New("TCP4 owner table too small")
	}
	count := int(binary.LittleEndian.Uint32(table[:4]))
	if len(table) < 4+count*mibTCPRowOwnerPIDSize {
		return nil, errors.New("TCP4 owner table truncated")
	}
	var pids []int
	for i := 0; i < count; i++ {
		row := table[4+i*mibTCPRowOwnerPIDSize:]
		if binary.LittleEndian.Uint32(row[:4]) != tcpStateListen {
			continue
		}
		if windows.Ntohs(uint16(binary.LittleEndian.Uint32(row[8:12]))) != port {
			continue
		}
		if !providerOwnedDirectTCP4AddressMatchesHost(binary.LittleEndian.Uint32(row[4:8]), host) {
			continue
		}
		pids = append(pids, int(binary.LittleEndian.Uint32(row[20:24])))
	}
	return pids, nil
}

func providerOwnedDirectTCP6ListenerPIDs(port uint16, host string) ([]int, error) {
	table, err := providerOwnedDirectExtendedTCPTable(windows.AF_INET6)
	if err != nil {
		return nil, err
	}
	if len(table) < 4 {
		return nil, errors.New("TCP6 owner table too small")
	}
	count := int(binary.LittleEndian.Uint32(table[:4]))
	if len(table) < 4+count*mibTCP6RowOwnerPIDSize {
		return nil, errors.New("TCP6 owner table truncated")
	}
	var pids []int
	for i := 0; i < count; i++ {
		row := table[4+i*mibTCP6RowOwnerPIDSize:]
		if binary.LittleEndian.Uint32(row[48:52]) != tcpStateListen {
			continue
		}
		if windows.Ntohs(uint16(binary.LittleEndian.Uint32(row[20:24]))) != port {
			continue
		}
		var local [16]byte
		copy(local[:], row[:16])
		if !providerOwnedDirectTCP6AddressMatchesHost(local, host) {
			continue
		}
		pids = append(pids, int(binary.LittleEndian.Uint32(row[52:56])))
	}
	return pids, nil
}

func providerOwnedDirectExtendedTCPTable(family uint32) ([]byte, error) {
	var size uint32
	result, _, _ := getExtendedTCPTableProc.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		getExtendedTCPTableOrdering,
		uintptr(family),
		tcpTableOwnerPIDListener,
		0)
	if result != 0 && size == 0 {
		return nil, syscall.Errno(result)
	}
	table := make([]byte, size)
	result, _, _ = getExtendedTCPTableProc.Call(
		uintptr(unsafe.Pointer(&table[0])),
		uintptr(unsafe.Pointer(&size)),
		getExtendedTCPTableOrdering,
		uintptr(family),
		tcpTableOwnerPIDListener,
		0)
	if result != 0 {
		return nil, syscall.Errno(result)
	}
	return table[:size], nil
}

func providerOwnedDirectTCP4AddressMatchesHost(raw uint32, host string) bool {
	local := netip.AddrFrom4([4]byte{byte(raw), byte(raw >> 8), byte(raw >> 16), byte(raw >> 24)})
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" || host == "localhost" {
		return local.IsLoopback() || local.IsUnspecified()
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is4() {
		return false
	}
	return local.IsUnspecified() || local == addr
}

func providerOwnedDirectTCP6AddressMatchesHost(raw [16]byte, host string) bool {
	local := netip.AddrFrom16(raw)
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" || host == "localhost" {
		return local.IsLoopback() || local.IsUnspecified()
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is6() {
		return false
	}
	return local.IsUnspecified() || local == addr
}
