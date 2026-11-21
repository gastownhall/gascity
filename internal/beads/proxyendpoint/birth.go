package proxyendpoint

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// BirthTokenPrefix is the scheme bd stamps on a Linux process-birth token
// (beads internal/procid/procid_linux.go). A token with any other prefix was
// produced by a different platform's recipe and gc cannot recompute it.
const BirthTokenPrefix = "linux-v1:"

// ErrBirthUnavailable reports that this host cannot recompute a birth token —
// no /proc, or no readable boot id. It is a distinct outcome from "the token
// did not match": an unavailable recompute leaves the generation question open,
// and the caller falls back to argv evidence rather than declaring the record
// stale.
var ErrBirthUnavailable = errors.New("proxyendpoint: process birth token unavailable on this host")

// BirthToken assembles bd's token from its two parts: the host's boot id and
// the process's start time in clock ticks since boot.
//
// Both halves matter. The start time alone repeats across reboots, and the boot
// id alone says nothing about a process, so a token that matches proves the PID
// still names the process that published the record rather than a recycled
// number.
func BirthToken(bootID, startTime string) string {
	return BirthTokenPrefix + strings.TrimSpace(bootID) + ":" + strings.TrimSpace(startTime)
}

// ParseProcStatStartTime extracts field 22 (starttime) from a /proc/<pid>/stat
// row, mirroring bd's parse so the token gc computes is byte-identical to the
// one bd wrote.
//
// The comm field is parenthesised and may itself contain spaces and
// parentheses, so parsing anchors on the LAST ')' and counts from there: the
// remainder starts at field 3 (state), which puts starttime at index 19.
//
// A process reported as a zombie or dead is an error rather than a token: bd
// refuses those states too, and a zombie's start time would otherwise validate
// a record whose process can no longer serve anything.
func ParseProcStatStartTime(stat string) (string, error) {
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 {
		return "", errors.New("proxyendpoint: malformed proc stat: no comm terminator")
	}
	fields := strings.Fields(stat[endComm+1:])
	const startTimeIndex = 19 // field 22, counting from state (field 3) at index 0
	if len(fields) <= startTimeIndex {
		return "", fmt.Errorf("proxyendpoint: malformed proc stat: %d post-comm fields, want > %d", len(fields), startTimeIndex)
	}
	switch fields[0] {
	case "Z", "X", "x":
		return "", fmt.Errorf("proxyendpoint: process is in state %s and no longer running", fields[0])
	}
	if _, err := strconv.ParseUint(fields[startTimeIndex], 10, 64); err != nil {
		return "", fmt.Errorf("proxyendpoint: malformed proc stat starttime %q: %w", fields[startTimeIndex], err)
	}
	return fields[startTimeIndex], nil
}
