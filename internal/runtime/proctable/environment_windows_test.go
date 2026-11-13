//go:build windows

package proctable

import (
	"encoding/binary"
	"fmt"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// guardedUnits writes units into this process's own memory so they end exactly
// where readable memory ends, and returns the address of the first unit.
//
// The data fills the tail of freshly committed pages, and the page after them
// is reserved but never committed, so any read that reaches past the last unit
// fails the way a read past the end of a region in another process does.
func guardedUnits(t *testing.T, units []uint16) uintptr {
	t.Helper()
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	dataPages := (len(data) + environmentPage - 1) / environmentPage
	base, err := windows.VirtualAlloc(0, uintptr((dataPages+1)*environmentPage), windows.MEM_RESERVE, windows.PAGE_NOACCESS)
	if err != nil {
		t.Fatalf("reserving pages: %v", err)
	}
	t.Cleanup(func() { _ = windows.VirtualFree(base, 0, windows.MEM_RELEASE) })
	if _, err := windows.VirtualAlloc(base, uintptr(dataPages*environmentPage), windows.MEM_COMMIT, windows.PAGE_READWRITE); err != nil {
		t.Fatalf("committing pages: %v", err)
	}
	start := base + uintptr(dataPages*environmentPage-len(data))
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), start, &data[0], uintptr(len(data)), nil); err != nil {
		t.Fatalf("writing the test block: %v", err)
	}
	return start
}

// unterminatedEnvironment returns exactly n units of well-formed entries with no
// terminating empty entry, ending with GC_SESSION_ID so a reader that accepts a
// prefix visibly loses it.
func unterminatedEnvironment(t *testing.T, n int) []uint16 {
	t.Helper()
	tail := utf16.Encode([]rune("GC_SESSION_ID=lost-in-the-tail"))
	var units []uint16
	for i := 0; len(units) < n-len(tail); i++ {
		units = append(units, utf16.Encode([]rune(fmt.Sprintf("FILLER_%05d=%s", i, "value")))...)
		units = append(units, 0)
	}
	units = append(units[:n-len(tail)], tail...)
	if units[len(units)-1] == 0 || indexDoubleNUL(units) >= 0 {
		t.Fatal("the test block is not the unterminated block it claims to be")
	}
	return units
}

// TestScanRemoteEnvironmentRefusesABlockThatRunsIntoUnreadableMemory is the
// regression test for a truncated environment passed off as a complete one.
// The block spans three pages with no terminator and then meets unreadable
// memory. A reader that stops there and parses what it has returns every
// variable except the ones in the tail — here, the session ID.
func TestScanRemoteEnvironmentRefusesABlockThatRunsIntoUnreadableMemory(t *testing.T) {
	address := guardedUnits(t, unterminatedEnvironment(t, 3*environmentPage/2))

	units, err := scanRemoteEnvironment(windows.CurrentProcess(), address)
	if err == nil {
		t.Fatalf("scanRemoteEnvironment returned %d units and no error for a block with no terminator", len(units))
	}
}

// TestReadRemoteEnvironmentRefusesATruncatedDeclaredBlock covers the path that
// trusts EnvironmentSize: a declared length that ends before the terminator
// must not be parsed as the whole block.
func TestReadRemoteEnvironmentRefusesATruncatedDeclaredBlock(t *testing.T) {
	units := unterminatedEnvironment(t, 2*environmentPage/2)
	address := guardedUnits(t, units)
	header := make([]byte, paramsHeaderLength)
	binary.LittleEndian.PutUint64(header[paramsEnvironmentOffset:], uint64(address))
	binary.LittleEndian.PutUint64(header[paramsEnvironmentSizeOffset:], uint64(len(units)*2))

	env, err := readRemoteEnvironment(windows.CurrentProcess(), header)
	if err == nil {
		t.Fatalf("readRemoteEnvironment accepted a declared block with no terminator: %d variables, GC_SESSION_ID=%q", len(env), env["GC_SESSION_ID"])
	}
}

// TestScanRemoteEnvironmentReadsABlockEndingAtTheEdgeOfReadableMemory pins the
// case the scan's page granularity exists for: a complete block that starts
// near the end of its memory region. A fixed-size read from its start would
// run into the unreadable page beyond it and fail a block that was entirely
// readable.
func TestScanRemoteEnvironmentReadsABlockEndingAtTheEdgeOfReadableMemory(t *testing.T) {
	address := guardedUnits(t, encodeEnvironmentBlock("GC_SESSION_ID=at-the-edge", "GC_CITY_PATH=C:\\city"))

	units, err := scanRemoteEnvironment(windows.CurrentProcess(), address)
	if err != nil {
		t.Fatalf("scanRemoteEnvironment on a complete block at the edge of readable memory: %v", err)
	}
	env, complete := parseCompleteEnvironmentBlock(units)
	if !complete || env["GC_SESSION_ID"] != "at-the-edge" || env["GC_CITY_PATH"] != "C:\\city" {
		t.Fatalf("parsed %v (complete %v), want both variables", env, complete)
	}
}

// TestScanRemoteEnvironmentRefusesAnOddAddress pins the alignment rule. From an
// odd address the first page ends mid-unit, every later page decodes a byte
// out of phase, and the zeros past a block still read as its terminator, so the
// scan returned garbage rather than an error.
func TestScanRemoteEnvironmentRefusesAnOddAddress(t *testing.T) {
	block := encodeEnvironmentBlock("GC_SESSION_ID=straddles-a-page", "GC_CITY_PATH=C:\\city")
	data := make([]byte, len(block)*2)
	for i, unit := range block {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	base, err := windows.VirtualAlloc(0, 2*environmentPage, windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE)
	if err != nil {
		t.Fatalf("committing pages: %v", err)
	}
	t.Cleanup(func() { _ = windows.VirtualFree(base, 0, windows.MEM_RELEASE) })
	// Odd, and close enough to the page boundary that the block straddles it.
	start := base + environmentPage - 11
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), start, &data[0], uintptr(len(data)), nil); err != nil {
		t.Fatalf("writing the test block: %v", err)
	}

	if units, err := scanRemoteEnvironment(windows.CurrentProcess(), start); err == nil {
		env, _ := parseCompleteEnvironmentBlock(units)
		t.Fatalf("scanRemoteEnvironment accepted odd address %#x and returned %d units parsing to %v", start, len(units), env)
	}
}

func TestParseCompleteEnvironmentBlockRequiresItsTerminator(t *testing.T) {
	block := encodeEnvironmentBlock("A=1", "GC_SESSION_ID=s")
	if env, complete := parseCompleteEnvironmentBlock(block); !complete || env["GC_SESSION_ID"] != "s" {
		t.Fatalf("parseCompleteEnvironmentBlock(complete block) = %v, %v", env, complete)
	}
	if env, complete := parseCompleteEnvironmentBlock(block[:len(block)-1]); complete {
		t.Fatalf("parseCompleteEnvironmentBlock accepted a block without its terminator: %v", env)
	}
}
