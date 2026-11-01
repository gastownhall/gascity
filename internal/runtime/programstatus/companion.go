package programstatus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CompanionName is the file name of the capture companion binary, which
// ships next to gc.
const CompanionName = "gc-osc7501"

// RelayVersion is the relay protocol gc launches a relay-declared program
// under (`gc-osc7501 relay -- <argv>`). gc never launches a program under a
// companion that reports another relay version, 0 included.
const RelayVersion = 1

var (
	// ErrCompanionNotFound is returned when the companion is neither next to
	// the running executable nor on PATH.
	ErrCompanionNotFound = errors.New("programstatus: " + CompanionName + " not found")
	// ErrCompanionCodec is returned for a companion that writes a codec
	// version this package does not read.
	ErrCompanionCodec = errors.New("programstatus: " + CompanionName + " codec mismatch")
)

// CompanionVersion is what `gc-osc7501 version` reports.
type CompanionVersion struct {
	Version string // the build's version label, never empty or spaced
	Codec   int    // the pane-option codec version the tap writes
	Relay   int    // the relay protocol version; 0 when the build has no relay
}

// String formats v as the version line: "gc-osc7501 <version> codec=N relay=N".
func (v CompanionVersion) String() string {
	return fmt.Sprintf("%s %s codec=%d relay=%d", CompanionName, v.Version, v.Codec, v.Relay)
}

// ParseCompanionVersion parses the line [CompanionVersion.String] writes,
// with at most a trailing newline.
func ParseCompanionVersion(line string) (CompanionVersion, error) {
	bad := func(why string) (CompanionVersion, error) {
		return CompanionVersion{}, fmt.Errorf("parsing %s version line %q: %s", CompanionName, line, why)
	}
	f := strings.Split(strings.TrimSuffix(line, "\n"), " ")
	if len(f) != 4 || f[0] != CompanionName {
		return bad("want 4 space-separated fields starting with " + CompanionName)
	}
	if f[1] == "" || strings.ContainsAny(f[1], "\t\r\n") {
		return bad("empty or unprintable version")
	}
	codec, ok := versionField(f[2], "codec=")
	if !ok {
		return bad("malformed codec field")
	}
	relay, ok := versionField(f[3], "relay=")
	if !ok {
		return bad("malformed relay field")
	}
	return CompanionVersion{Version: f[1], Codec: codec, Relay: relay}, nil
}

// versionField parses "<prefix><canonical non-negative decimal>".
func versionField(f, prefix string) (int, bool) {
	digits, ok := strings.CutPrefix(f, prefix)
	if !ok {
		return 0, false
	}
	n, err := parseCanonical(digits, 31)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// CheckCompanionVersion reports whether a companion reporting v writes the
// codec this package reads.
func CheckCompanionVersion(v CompanionVersion) error {
	if v.Codec != CodecVersion {
		return fmt.Errorf("%w: %s writes codec %d, gc reads codec %d", ErrCompanionCodec, v.Version, v.Codec, CodecVersion)
	}
	return nil
}

// LookupCompanion resolves the companion binary: next to [os.Executable] as
// returned (on darwin that keeps Homebrew's stable bin/ symlink), else on
// PATH. It is resolved afresh on every call because an upgrade can remove a
// previously resolved path.
func LookupCompanion() (string, error) {
	exe, err := os.Executable()
	return lookupCompanion(exe, err, exec.LookPath)
}

func lookupCompanion(exe string, exeErr error, lookPath func(string) (string, error)) (string, error) {
	if exeErr == nil && exe != "" {
		sibling := filepath.Join(filepath.Dir(exe), CompanionName)
		if isExecutableFile(sibling) {
			return sibling, nil
		}
	}
	p, err := lookPath(CompanionName)
	if err != nil {
		return "", fmt.Errorf("%w next to %q or on PATH: %w", ErrCompanionNotFound, exe, err)
	}
	return p, nil
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// ProbeCompanion runs `<path> version` and returns its report, failing with
// [ErrCompanionCodec] when the companion writes another codec.
func ProbeCompanion(ctx context.Context, path string) (CompanionVersion, error) {
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return CompanionVersion{}, fmt.Errorf("running %s version: %w", path, err)
	}
	v, err := ParseCompanionVersion(string(out))
	if err != nil {
		return CompanionVersion{}, err
	}
	if err := CheckCompanionVersion(v); err != nil {
		return v, err
	}
	return v, nil
}
