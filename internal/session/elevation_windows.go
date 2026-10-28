//go:build windows

package session

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// Seams for tests: the integrity level this process runs at, this user's
// processes above it, the running boot, this process's identity, and whether a
// recorded gc is still running.
var (
	currentIntegrity       = proctable.CurrentIntegrity
	elevatedProcessesSince = proctable.ElevatedProcessesSince
	currentBoot            = proctable.CurrentBoot
	currentProcessStart    = proctable.CurrentProcessStart
	processRunning         = proctable.ProcessRunning
)

// The mandatory integrity levels a record can carry: above medium, where a
// lower gc can no longer read what it started, and at most system.
const (
	integrityMedium = 0x2000
	integritySystem = 0x4000
)

// elevatedSessionsDir holds, in a city's runtime directory, a record for each
// gc that started sessions there from above medium integrity. Every record is a
// file of its own, written whole by rename and never rewritten, so one start
// cannot overwrite another's.
const elevatedSessionsDir = "elevated-sessions"

// elevatedStartSuffix marks a record. Nothing else in the directory is one,
// including the temporary file a record is written through.
const elevatedStartSuffix = ".rec"

// elevatedStartKeys are a record's fields, in the order they are written.
var elevatedStartKeys = [...]string{"since", "integrity", "pid", "created", "boot"}

// elevatedStart is one record: the gc identified by pid and created, running
// at integrity in the boot identified by boot, started sessions in the city at
// or after since. Times are Unix nanoseconds.
type elevatedStart struct {
	path      string
	since     int64
	integrity int64
	pid       int64
	created   int64
	boot      int64
}

func elevatedSessionsPath(cityPath string) string {
	return filepath.Join(citylayout.RuntimeDataDir(cityPath), elevatedSessionsDir)
}

func (r elevatedStart) fileName() string {
	return fmt.Sprintf("%d-%d-%d%s", r.pid, r.created, r.since, elevatedStartSuffix)
}

func (r elevatedStart) encode() []byte {
	values := [...]int64{r.since, r.integrity, r.pid, r.created, r.boot}
	fields := make([]string, len(values))
	for i, value := range values {
		fields[i] = elevatedStartKeys[i] + "=" + strconv.FormatInt(value, 10)
	}
	return []byte(strings.Join(fields, " ") + "\n")
}

func invalidElevatedStart(path, reason string) error {
	return fmt.Errorf("%s is not a record an elevated gc writes (%s); delete it once no session started from an elevated gc is running", path, reason)
}

// parseElevatedStart reads a record's fields. Their values are checked by
// validate, except the boot's, which decides whether the rest matter at all.
func parseElevatedStart(path string, data []byte) (elevatedStart, error) {
	fields := strings.Fields(string(data))
	if len(fields) != len(elevatedStartKeys) {
		return elevatedStart{}, invalidElevatedStart(path, fmt.Sprintf("%d fields, want %d", len(fields), len(elevatedStartKeys)))
	}
	var values [len(elevatedStartKeys)]int64
	for i, key := range elevatedStartKeys {
		text, ok := strings.CutPrefix(fields[i], key+"=")
		if !ok {
			return elevatedStart{}, invalidElevatedStart(path, fmt.Sprintf("field %d is not %s", i+1, key))
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return elevatedStart{}, invalidElevatedStart(path, fmt.Sprintf("%s: %v", key, err))
		}
		values[i] = value
	}
	record := elevatedStart{path: path, since: values[0], integrity: values[1], pid: values[2], created: values[3], boot: values[4]}
	if record.boot <= 0 {
		return elevatedStart{}, invalidElevatedStart(path, "it names no boot")
	}
	return record, nil
}

// validate refuses a record from this boot that no gc could have written.
func (r elevatedStart) validate(now time.Time) error {
	switch {
	case r.since <= 0 || r.since > now.UnixNano():
		return invalidElevatedStart(r.path, "its start time is not in the past")
	case r.integrity <= integrityMedium || r.integrity > integritySystem:
		return invalidElevatedStart(r.path, fmt.Sprintf("integrity %#x is not above medium and at most system", r.integrity))
	case r.pid <= 0 || r.pid > int64(^uint32(0)) || r.created <= 0:
		return invalidElevatedStart(r.path, "it does not identify the gc that wrote it")
	}
	return nil
}

// readElevatedStarts returns the records in dir. A directory or record that
// cannot be read refuses, rather than being read as none.
func readElevatedStarts(dir string) ([]elevatedStart, error) {
	// ReadDir on a file reports it as not existing on Windows, so what the
	// path names is settled first.
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the elevated-session records in %s: %w", dir, err)
	case !info.IsDir():
		return nil, fmt.Errorf("%s is not a directory of elevated-session records; delete it once no session started from an elevated gc is running", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the elevated-session records in %s: %w", dir, err)
	}
	var records []elevatedStart
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), elevatedStartSuffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			// Retired by another gc since the directory was listed.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		record, err := parseElevatedStart(path, data)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// retireElevatedStart removes a record that nothing it describes can have
// outlived. The start does not depend on the removal, so a failure does not
// refuse it; the record is checked again at every later start, so the failure
// is logged.
func retireElevatedStart(record elevatedStart) {
	if err := os.Remove(record.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("session: leaving the elevated-session record %s in place: %v", record.path, err)
	}
}

// checkElevatedSessions refuses a session start from a gc that cannot see the
// sessions a more elevated gc started in this city.
//
// Windows keeps a process from reading the memory of one above its own
// integrity level, so the orphan scan here cannot read the environment of a
// session an elevated gc started, and cannot tell it from any other elevated
// process. A record from an earlier boot is retired whatever its level:
// nothing it describes survived the boot. For each record from this boot
// above this gc's level:
//
//   - While the gc that wrote it is still running, the start is refused. That
//     gc may be about to start a session, or have one starting that no process
//     snapshot shows yet.
//   - Once every such gc has exited, nothing can add to what they started, so
//     only the elevated processes created since the earliest of their starts
//     could be one of their sessions. While any is running, the start is
//     refused and they are named; once none is, those records are retired.
//
// The snapshot is taken only after every recorder is confirmed exited. Taken
// before, it could miss a session a recorder started between the two.
func checkElevatedSessions(cityPath string) error {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	dir := elevatedSessionsPath(cityPath)
	records, err := readElevatedStarts(dir)
	if err != nil || len(records) == 0 {
		return err
	}
	boot, err := currentBoot()
	if err != nil {
		return fmt.Errorf("identifying the current boot to check the elevated-session records in %s: %w", dir, err)
	}
	level, err := currentIntegrity()
	if err != nil {
		return fmt.Errorf("reading this process's integrity level: %w", err)
	}
	now := time.Now()
	var above []elevatedStart
	for _, record := range records {
		if record.boot != boot {
			retireElevatedStart(record)
			continue
		}
		if err := record.validate(now); err != nil {
			return err
		}
		if int64(level) < record.integrity {
			above = append(above, record)
		}
	}
	if len(above) == 0 {
		return nil
	}

	var recorders []string
	for _, record := range above {
		running, err := processRunning(int(record.pid), record.created)
		if err != nil {
			return fmt.Errorf("cannot tell whether the elevated gc that wrote %s (PID %d) is still running: %w", record.path, record.pid, err)
		}
		if running {
			recorders = append(recorders, fmt.Sprintf("PID %d, starting sessions since %s", record.pid, time.Unix(0, record.since).Format(time.RFC3339)))
		}
	}
	if len(recorders) > 0 {
		return fmt.Errorf("a gc running elevated has started sessions in this city and is still running: %s. "+
			"This gc is not elevated, so it cannot read the sessions that one starts; run this from an elevated terminal, or stop that gc and its sessions first. "+
			"Its records are in %s",
			strings.Join(recorders, "; "), dir)
	}

	since := above[0].since
	for _, record := range above[1:] {
		since = min(since, record.since)
	}
	survivors, err := elevatedProcessesSince(time.Unix(0, since))
	if err != nil {
		return fmt.Errorf("listing elevated processes: %w", err)
	}
	if len(survivors) > 0 {
		named := make([]string, 0, len(survivors))
		for _, p := range survivors {
			named = append(named, fmt.Sprintf("PID %d (%s)", p.PID, p.Name))
		}
		return fmt.Errorf("sessions in this city were started from an elevated gc (since %s), and %d elevated process(es) of this user started since then are still running: %s. "+
			"This gc is not elevated, so it cannot read them to rule out a surviving session; stop them from an elevated terminal, or restart Windows (Restart: a Fast Startup shutdown does not end the boot), then start again. "+
			"The records are in %s",
			time.Unix(0, since).Format(time.RFC3339), len(survivors), strings.Join(named, ", "), dir)
	}
	for _, record := range above {
		retireElevatedStart(record)
	}
	return nil
}

// ownElevatedStarts remembers the record this process last wrote in each
// records directory. A later start it makes there is covered by that record
// while the record is still in place, so a long-running elevated gc keeps one
// record per city rather than adding one per start.
var ownElevatedStarts struct {
	sync.Mutex
	byDir map[string]elevatedStart
}

// recordElevatedSessionStart notes, before a session starts, that a gc above
// medium integrity is starting it, so a later gc at a lower level knows its
// orphan scan cannot see that session.
func recordElevatedSessionStart(cityPath string) error {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	level, err := currentIntegrity()
	if err != nil {
		return fmt.Errorf("reading this process's integrity level: %w", err)
	}
	if level <= integrityMedium {
		return nil
	}
	pid, created, err := currentProcessStart()
	if err != nil {
		return fmt.Errorf("recording an elevated session start: identifying this process: %w", err)
	}
	// Back-dated a second, so no session this start creates can predate it.
	since := time.Now().Add(-time.Second).UnixNano()
	dir := elevatedSessionsPath(cityPath)

	ownElevatedStarts.Lock()
	defer ownElevatedStarts.Unlock()
	// This process's own record covers the start if it is still in place and
	// begins no later. Only this process's exit or a new boot retires it, so a
	// record from before a clock step backwards is the only reason to add one.
	if own, ok := ownElevatedStarts.byDir[dir]; ok && own.pid == int64(pid) && own.created == created && own.since <= since {
		if _, err := os.Stat(own.path); err == nil {
			return nil
		}
	}
	boot, err := currentBoot()
	if err != nil {
		return fmt.Errorf("recording an elevated session start: identifying the current boot: %w", err)
	}
	record := elevatedStart{since: since, integrity: int64(level), pid: int64(pid), created: created, boot: boot}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("recording an elevated session start: %w", err)
	}
	record.path = filepath.Join(dir, record.fileName())
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, record.path, record.encode(), 0o600); err != nil {
		return fmt.Errorf("recording an elevated session start: %w", err)
	}
	if ownElevatedStarts.byDir == nil {
		ownElevatedStarts.byDir = make(map[string]elevatedStart)
	}
	ownElevatedStarts.byDir[dir] = record
	return nil
}
