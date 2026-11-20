package main

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
)

// gatherSupervisorBinary collects the running supervisor's executable and
// build identity for the supervisor-binary doctor check and decides whether
// that build differs from this gc's. The remediation comes from the mismatch
// model gc init, gc start and gc status use, so it accounts for the binary
// the supervisor's service would relaunch. Probe failures are carried on the
// result so the check can report them.
func gatherSupervisorBinary(pid int) doctor.SupervisorBinary {
	local := localGCBinaryIdentity()
	var info doctor.SupervisorBinary
	info.LocalBuildID, info.LocalVersion = knownGCIdentity(local.BuildID, local.Version)
	info.ExePath, info.ExeDeleted, info.ExeErr = readSupervisorExeState(pid)
	if status, err := readSupervisorHealthStatus(); err != nil {
		info.StatusErr = err
	} else {
		info.BuildID, info.Version = knownGCIdentity(status.BuildID, status.Version)
		info.BuildDiffers = supervisorBuildDiffers(local, status)
	}
	m := newSupervisorBinaryMismatch(pid, gcBinaryIdentity{ExePath: info.ExePath, Version: info.Version, BuildID: info.BuildID})
	m.classify()
	if m.RelaunchPath != "" && !supervisorSameBinary(m.RelaunchPath, info.ExePath) {
		info.ServiceBinary = m.RelaunchPath
	}
	info.FixHint = supervisorBinaryFixHint(m)
	return info
}

// supervisorBinaryFixHint returns supervisorBinaryMismatchFix's remediation
// for the doctor. An invalid systemd delegation setting is named instead,
// because the supervisor lifecycle commands refuse to run until it is fixed.
func supervisorBinaryFixHint(m supervisorBinaryMismatch) string {
	if _, _, err := supervisorSystemdDelegation(); err != nil {
		return supervisorRestartGuidance()
	}
	return strings.Join(supervisorBinaryMismatchFix(m), "\n")
}

// supervisorBuildDiffers reports whether the supervisor serves a different
// gc build than local. Build identities decide when both sides report one,
// compared with sameGCBuildID so an abbreviated hash matches its full form.
// Otherwise known release versions decide, as in detectGCBinaryDrift: a
// homebrew-core build carries only a version. With neither known on both
// sides there is nothing to compare, so it reports false.
func supervisorBuildDiffers(local gcBinaryIdentity, sv SupervisorStatus) bool {
	if knownGCBuildID(local.BuildID) && knownGCBuildID(sv.BuildID) {
		return !sameGCBuildID(local.BuildID, sv.BuildID)
	}
	if knownGCVersion(local.Version) && knownGCVersion(sv.Version) {
		return normalizeGCVersion(local.Version) != normalizeGCVersion(sv.Version)
	}
	return false
}

// knownGCIdentity trims a build ID and version and blanks each one that
// identifies no build ("unknown", "dev", or empty).
func knownGCIdentity(buildID, ver string) (string, string) {
	buildID, ver = strings.TrimSpace(buildID), strings.TrimSpace(ver)
	if !knownGCBuildID(buildID) {
		buildID = ""
	}
	if !knownGCVersion(ver) {
		ver = ""
	}
	return buildID, ver
}

// readSupervisorExeState reports the executable process pid was started from
// and whether that file has since been removed or replaced on disk. Linux
// reads /proc/<pid>/exe, where the kernel marks an unlinked executable
// " (deleted)". macOS has no /proc, so it falls back to ps(1); see
// readSupervisorExeStateViaPS.
func readSupervisorExeState(pid int) (string, bool, error) {
	path, deleted, err := readSupervisorExeLink(pid)
	if err == nil {
		return path, deleted, nil
	}
	if supervisorRuntimeGOOS != "darwin" {
		return "", false, err
	}
	return readSupervisorExeStateViaPS(pid)
}

// readSupervisorExeStateViaPS accepts the ps(1) comm value only as
// verifiedPSExecutablePath does: an absolute path to an existing regular
// file. The one exception is a missing path that is the binary the managing
// service launches. That path is the supervisor's real launch path, so its
// absence means the build the supervisor runs was removed. Any other missing
// path may be a stale or truncated comm, so it is reported as unreadable,
// not as deleted.
func readSupervisorExeStateViaPS(pid int) (string, bool, error) {
	comm, err := readProcessCommViaPSHook(pid)
	if err != nil {
		return "", false, err
	}
	path, err := verifiedPSExecutablePath(pid, comm)
	if err == nil {
		return path, false, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		if serviceBinary, _ := supervisorServiceBinaryHook(); serviceBinary != "" && supervisorSameBinary(comm, serviceBinary) {
			return comm, true, nil
		}
	}
	return "", false, err
}
