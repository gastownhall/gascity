//go:build windows

package proctable

// Windows process-table primitives shared by the scanner, the host-wide
// snapshot, the identity reader and the kill path.
//
// Three kernel facilities carry the whole port:
//
//   - NtQuerySystemInformation(SystemProcessInformation) enumerates every PID,
//     PPID, image name and creation time on the host in one kernel pass,
//     without opening a single process, so the table stays complete even where
//     this user may not touch the processes in it — and every row carries the
//     identity that binds it to one process incarnation.
//   - GetProcessTimes reads the same creation time from a handle, which is how a
//     handle opened later is proven to be the process the snapshot saw. Windows
//     recycles PIDs aggressively and never clears a parent's PID out of a
//     child's record, so a bare PID identifies nothing on its own.
//   - NtQueryInformationProcess plus ReadProcessMemory walk a target's PEB to
//     its RTL_USER_PROCESS_PARAMETERS, the only place a process's environment
//     block and command line can be read from outside it. Windows has no
//     /proc/<pid>/environ equivalent and no documented Win32 call for either.
//
// Failing to read one process is ordinary: another user's processes, protected
// processes and most services refuse OpenProcess, and a process can exit between
// the enumeration and the read. Those rows are reported without the fields that
// needed a handle. Two other outcomes are not ordinary. Failing to read EVERY
// process means the mechanism itself is unavailable, and readProcessTable
// refuses rather than hand back a table that looks like an idle host. And a
// process this user could open but still not read might be exactly the session
// a caller is looking for, so a scan that meets one reports itself incomplete.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pointerSize is the reader's own pointer width.
const pointerSize = int(unsafe.Sizeof(uintptr(0)))

// Offsets into PEB and RTL_USER_PROCESS_PARAMETERS on 64-bit Windows. The
// structures are undocumented, but this part of their layout has been stable
// since Windows 7 and is the same for x64 and arm64. Every field is read out of
// a bounds-checked buffer, so a layout change surfaces as an unreadable process
// rather than as a wild pointer.
const (
	pebProcessParametersOffset  = 0x20
	paramsCommandLineOffset     = 0x70  // UNICODE_STRING
	paramsEnvironmentOffset     = 0x80  // PVOID
	paramsEnvironmentSizeOffset = 0x3F0 // SIZE_T
	paramsHeaderLength          = 0x3F8

	// unicodeStringBufferOffset is the offset of UNICODE_STRING.Buffer, after
	// Length, MaximumLength and four bytes of alignment padding.
	unicodeStringBufferOffset = 8
)

// maxEnvironmentBlock bounds both the declared EnvironmentSize this package is
// willing to trust and the scan that replaces it when that field is
// implausible. A real Windows environment block is tens of kilobytes; 4 MiB
// never truncates one and keeps a garbage length from becoming a huge
// allocation.
//
// environmentPage is the granularity of that scan. Reads never cross a page
// boundary, so a block that ends near the end of its memory region is read up
// to its terminator instead of failing on the unmapped page beyond it.
const (
	maxEnvironmentBlock = 4 << 20
	environmentPage     = 4 << 10
)

// inspection records how reading one row's environment ended.
type inspection int

const (
	inspectionNotAttempted inspection = iota
	// inspected: the environment block was read in full.
	inspected
	// inspectionGone: the process exited, or its PID now names a process the
	// snapshot never saw.
	inspectionGone
	// inspectionDenied: the process refused the walk, and it could not be tied
	// to this user: it hides its token, or it refuses even a query-limited open
	// and its security descriptor names another owner or cannot be read.
	// Treated as out of scope. A process this user starts gets a DACL
	// granting this user full access, so refusing that least-privileged query
	// takes a deliberate narrowing of its own DACL against its owner; every
	// such row on a measured host was a system or service process. Counting
	// these as incomplete would refuse every scan.
	inspectionDenied
	// inspectionFailed: opened, still running, still the same process, and
	// still unreadable — or refused by this user's own process at no higher an
	// integrity than this one, which can only be its DACL narrowed against this
	// user. Either way it could be anything, including the session a caller is
	// looking for.
	inspectionFailed
	// inspectionForeign: refused the walk, and its token names another account.
	// It cannot be one of this user's sessions.
	inspectionForeign
	// inspectionElevated: refused the walk, and its token is this user's at a
	// higher integrity level than this process — Windows' no-read-up policy
	// keeps a lower integrity from reading its memory. Treated as out of scope
	// for now: it can be a session only if a supervisor ran elevated, and
	// counting these as incomplete would refuse every scan on a host that
	// always has some.
	inspectionElevated
)

// errProcessInspectionUnsupported reports that this build cannot walk a PEB at
// all. The offsets above are 64-bit; a 32-bit reader applying them to a 64-bit
// target would read silently misaligned fields, which is worse than no answer.
var errProcessInspectionUnsupported = errors.New("proctable: reading another process's parameters requires a 64-bit Windows build")

// processBasicInformation mirrors PROCESS_BASIC_INFORMATION.
//
// It is declared here rather than taken from x/sys/windows because that
// package types PebBaseAddress as *PEB. The value is an address in ANOTHER
// process, so storing it in a Go pointer field would hand the garbage collector
// a foreign address to scan. uintptr keeps it opaque, which is the only safe
// representation for a pointer the kernel owns.
type processBasicInformation struct {
	ExitStatus                   int32
	_                            int32 // alignment for the pointer that follows
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 int32
	_                            int32
	UniqueProcessId              uintptr
	InheritedFromUniqueProcessId uintptr
}

// windowsProcess is one row of the host process table.
//
// creationOK and envOK are separate from their values because "not readable"
// and "empty" are different facts here: a protected process yields neither a
// creation time nor an environment, and a row that silently reported both as
// zero would let a caller fence on an identity nobody ever observed.
type windowsProcess struct {
	pid        int
	ppid       int
	image      string
	creation   int64 // Unix nanoseconds, from the snapshot
	creationOK bool
	env        map[string]string
	envOK      bool

	inspection    inspection
	inspectionErr error
}

// startIdentity returns the process-start token used everywhere this package
// fences a PID against reuse. It is the same decimal Unix-nanosecond encoding
// pidutil.StartTime produces on Windows, so a token from a snapshot row and one
// from ProcessIdentity or pidutil compare directly.
func (p windowsProcess) startIdentity() string {
	if !p.creationOK {
		return ""
	}
	return strconv.FormatInt(p.creation, 10)
}

func (p windowsProcess) sessionID() string {
	return p.env["GC_SESSION_ID"]
}

// uninspected reports whether this row describes a process that might be one
// of this user's and whose environment could not be read, which is what makes
// a table read incomplete.
func (p windowsProcess) uninspected() bool {
	return p.inspection == inspectionFailed
}

// processTable is one host-wide read of the Windows process table.
type processTable struct {
	rows  []windowsProcess
	byPID map[int]windowsProcess

	// uninspected holds the rows this read could open but not inspect.
	uninspected []windowsProcess
}

// incomplete reports the processes this read could open but not inspect. Any
// of them could be the process a caller is looking for, so a scan that has to
// establish "nothing matching is running" cannot establish it while this is
// non-nil. The rows it did read are still usable.
func (t processTable) incomplete() error {
	if len(t.uninspected) == 0 {
		return nil
	}
	details := make([]string, 0, len(t.uninspected))
	for _, row := range t.uninspected {
		details = append(details, fmt.Sprintf("PID %d (%s): %v", row.pid, row.image, row.inspectionErr))
	}
	return fmt.Errorf("%w: %d process(es) could be opened but not read: %s",
		ErrScanIncomplete, len(t.uninspected), strings.Join(details, "; "))
}

// parent returns child's parent row, and false when Windows cannot vouch for
// the link.
//
// Windows never clears a parent PID out of a child's record: the field keeps
// naming a number the kernel may since have handed to an unrelated process. So
// a row only counts as the parent when both creation times are known and it
// was created no later than the child — the one thing a recycled PID cannot
// forge. An unknown time proves nothing either way, so it certifies nothing.
// Without this test a long-lived agent whose launcher exited would adopt
// whatever new process inherited the number, and the scanner would read that
// stranger's session ID as its own parent envelope.
func (t processTable) parent(child windowsProcess) (windowsProcess, bool) {
	if child.ppid <= 0 || child.ppid == child.pid {
		return windowsProcess{}, false
	}
	parent, ok := t.byPID[child.ppid]
	if !ok {
		return windowsProcess{}, false
	}
	if !parent.creationOK || !child.creationOK || parent.creation > child.creation {
		return windowsProcess{}, false
	}
	return parent, true
}

// readProcessTable enumerates the host process table and enriches every row it
// can with an environment block.
//
// A row that could not be opened keeps its PID, PPID, image name and creation
// time; only the environment is missing. A row that could be opened but not
// read is retried once after a short pause, because the ordinary causes — a
// process still being created, or one in the middle of exiting — resolve
// themselves within that. Whether the result as a whole can be trusted is
// decided by processTableFromRows.
func readProcessTable() (processTable, error) {
	rows, err := systemProcesses()
	if err != nil {
		return processTable{}, err
	}
	retry := false
	for i := range rows {
		enrichProcess(&rows[i])
		retry = retry || rows[i].inspection == inspectionFailed
	}
	if retry {
		time.Sleep(inspectionRetryDelay)
		for i := range rows {
			if rows[i].inspection == inspectionFailed {
				enrichProcess(&rows[i])
			}
		}
	}
	return processTableFromRows(rows, os.Getpid())
}

// inspectionRetryDelay is how long readProcessTable waits before re-reading a
// process it could open but not read.
const inspectionRetryDelay = 50 * time.Millisecond

// processTableFromRows indexes an enriched table, and refuses it when the read
// proved nothing.
//
// The test is whether THIS process's own environment block came back. It always
// can: a process may always open itself, and its PEB is the same structure
// every other row is read through. So if even that one failed, the mechanism is
// unavailable on this host rather than the host being quiet — and a caller
// gating a start on "no orphan is running" has to be told the difference. An
// empty answer from a scan that could not look is the failure mode this whole
// package exists to avoid.
func processTableFromRows(rows []windowsProcess, self int) (processTable, error) {
	if len(rows) == 0 {
		return processTable{}, errors.New("proctable: the Windows process table snapshot reported no processes")
	}
	readable := 0
	selfReadable := false
	for _, row := range rows {
		if !row.envOK {
			continue
		}
		readable++
		if row.pid == self {
			selfReadable = true
		}
	}
	if !selfReadable {
		return processTable{}, fmt.Errorf(
			"proctable: enumerated %d processes but could not read this process's own environment block (%d readable): "+
				"process environment inspection is unavailable, so the scan proves nothing about what is running: %w",
			len(rows), readable, ErrUnsupported)
	}
	table := indexProcessTable(rows)
	for _, row := range rows {
		if row.uninspected() {
			table.uninspected = append(table.uninspected, row)
		}
	}
	return table, nil
}

func indexProcessTable(rows []windowsProcess) processTable {
	byPID := make(map[int]windowsProcess, len(rows))
	for _, row := range rows {
		byPID[row.pid] = row
	}
	return processTable{rows: rows, byPID: byPID}
}

// systemProcesses reads PID, PPID, image name and creation time for every
// process on the host from one SystemProcessInformation snapshot. No process is
// opened, so the table is complete regardless of what this user is allowed to
// inspect.
//
// All four fields come from the same kernel pass, which is what makes the
// creation time useful: it binds each row to one process incarnation, and a
// handle opened later can be checked against it. It is also known here for
// processes this user can never open, so parent links can be fenced across the
// whole table rather than only among readable rows.
func systemProcesses() ([]windowsProcess, error) {
	buffer, err := querySystemProcessInformation()
	if err != nil {
		return nil, err
	}
	entrySize := int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
	var rows []windowsProcess
	for offset := 0; ; {
		if offset+entrySize > len(buffer) {
			return nil, fmt.Errorf("proctable: process snapshot entry at offset %d overruns the %d-byte buffer", offset, len(buffer))
		}
		entry := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buffer[offset]))
		image, err := snapshotImageName(buffer, entry)
		if err != nil {
			return nil, err
		}
		row := windowsProcess{
			pid:   int(entry.UniqueProcessID),
			ppid:  int(entry.InheritedFromUniqueProcessID),
			image: image,
		}
		if created := filetimeUnixNanoseconds(entry.CreateTime); created > 0 {
			row.creation = created
			row.creationOK = true
		}
		rows = append(rows, row)
		if entry.NextEntryOffset == 0 {
			break
		}
		offset += int(entry.NextEntryOffset)
	}
	goruntime.KeepAlive(buffer)
	return rows, nil
}

// querySystemProcessInformation returns a SystemProcessInformation buffer,
// growing it until the whole table fits. Processes started between attempts
// can outgrow the size the previous attempt reported, hence the headroom and
// the bounded retry.
func querySystemProcessInformation() ([]byte, error) {
	size := uint32(512 << 10)
	for range 8 {
		buffer := make([]byte, size)
		var needed uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buffer[0]), size, &needed)
		if err == nil {
			if needed > 0 && needed <= size {
				buffer = buffer[:needed]
			}
			return buffer, nil
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return nil, fmt.Errorf("proctable: querying the process table: %w", err)
		}
		size = needed + needed/4 + 64<<10
	}
	return nil, errors.New("proctable: the process table kept outgrowing the buffer read to hold it")
}

// snapshotImageName decodes an entry's image name, which the kernel writes
// into the snapshot buffer itself. The name is checked to lie inside that
// buffer before it is read.
//
// The idle process has no image name at all. It is given Toolhelp's name for
// it, "[System Process]", so every row keeps a non-empty image.
func snapshotImageName(buffer []byte, entry *windows.SYSTEM_PROCESS_INFORMATION) (string, error) {
	if entry.UniqueProcessID == 0 {
		return "[System Process]", nil
	}
	length := int(entry.ImageName.Length)
	if length == 0 || entry.ImageName.Buffer == nil {
		return "", fmt.Errorf("proctable: process snapshot entry for PID %d has no image name", entry.UniqueProcessID)
	}
	start := uintptr(unsafe.Pointer(&buffer[0]))
	name := uintptr(unsafe.Pointer(entry.ImageName.Buffer))
	if length%2 != 0 || name < start || name+uintptr(length) > start+uintptr(len(buffer)) {
		return "", fmt.Errorf("proctable: image name of PID %d lies outside the process snapshot", entry.UniqueProcessID)
	}
	return string(utf16.Decode(unsafe.Slice(entry.ImageName.Buffer, length/2))), nil
}

// filetimeUnixNanoseconds converts a FILETIME-format time — 100-nanosecond
// intervals since 1601 — into the Unix nanoseconds GetProcessTimes readings
// are compared in. A zero or pre-1970 value is reported as 0, meaning unknown.
func filetimeUnixNanoseconds(value int64) int64 {
	if value <= 0 {
		return 0
	}
	filetime := windows.Filetime{LowDateTime: uint32(value), HighDateTime: uint32(value >> 32)}
	if nanoseconds := filetime.Nanoseconds(); nanoseconds > 0 {
		return nanoseconds
	}
	return 0
}

// enrichProcess reads the environment of the process row describes, and
// records how the attempt ended.
//
// The handle is checked against the snapshot's creation time before anything
// is read through it. A PID recycled since the snapshot would otherwise attach
// a later process's environment to this row's parent and image: a row that
// describes no process that ever existed, carrying a session ID it has no
// claim to.
func enrichProcess(row *windowsProcess) {
	row.env, row.envOK, row.inspectionErr = nil, false, nil
	if row.pid <= 0 {
		row.inspection = inspectionDenied
		return
	}
	handle, err := openProcessForInspection(row.pid)
	switch {
	case err == nil:
	case openErrorMeansProcessGone(err):
		row.inspection = inspectionGone
		return
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		row.inspection, row.inspectionErr = classifyRefusal(*row, err)
		return
	default:
		row.inspection, row.inspectionErr = inspectionFailed, err
		return
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	creation, err := handleCreationTime(handle)
	if err != nil {
		row.inspection, row.inspectionErr = inspectionFailed, err
		return
	}
	if !row.creationOK || creation != row.creation {
		row.inspection = inspectionGone
		return
	}
	environment, err := readProcessEnvironment(handle)
	if err != nil {
		if processHandleExited(handle) {
			row.inspection = inspectionGone
			return
		}
		row.inspection, row.inspectionErr = inspectionFailed, err
		return
	}
	row.env, row.envOK = environment, true
	row.inspection = inspected
}

// openProcessForInspection opens pid for the PEB walk.
//
// PROCESS_QUERY_INFORMATION is requested first because it is what the read
// needs on every supported release. PROCESS_QUERY_LIMITED_INFORMATION is a
// second chance rather than the first choice: it is granted in places the full
// right is not, and it is enough for ProcessBasicInformation, but it is not a
// superset. The caller sees the first error, which is the one that describes
// what was actually denied. SYNCHRONIZE lets a failed read be told apart from a
// process that exited underneath it.
func openProcessForInspection(pid int) (windows.Handle, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("proctable: invalid PID %d", pid)
	}
	const common = windows.PROCESS_VM_READ | windows.SYNCHRONIZE
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|common, false, uint32(pid))
	if err == nil {
		return handle, nil
	}
	limited, limitedErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|common, false, uint32(pid))
	if limitedErr != nil {
		return 0, fmt.Errorf("proctable: opening PID %d: %w", pid, err)
	}
	return limited, nil
}

// classifyRefusal decides what a process refusing the PEB walk says about it.
//
// A refusal alone does not mean another account. Windows checks a process's
// DACL and integrity label as well as its owner, so this user's own processes
// refuse too: an elevated one, whose memory the no-read-up policy keeps a lower
// integrity from reading, and one whose DACL was narrowed. Only the process
// token settles it, and PROCESS_QUERY_LIMITED_INFORMATION reaches the token far
// more often than the rights the walk needs. The handle is fenced against the
// snapshot row as the walk's own is, so a recycled PID cannot lend a later
// process's token to this row.
func classifyRefusal(row windowsProcess, refusal error) (inspection, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(row.pid))
	if err != nil {
		switch {
		case openErrorMeansProcessGone(err):
			return inspectionGone, nil
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return classifyByDescriptor(row, refusal)
		}
		return inspectionDenied, nil
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	creation, err := handleCreationTime(handle)
	if err != nil {
		return inspectionDenied, nil
	}
	if !row.creationOK || creation != row.creation {
		return inspectionGone, nil
	}
	owner, err := processOwnerOf(handle)
	if err != nil {
		return inspectionDenied, nil
	}
	self, err := currentProcessOwner()
	if err != nil {
		return inspectionDenied, nil
	}
	switch {
	case !owner.user.Equals(self.user):
		return inspectionForeign, nil
	case owner.integrity > self.integrity:
		return inspectionElevated, nil
	default:
		return inspectionFailed, fmt.Errorf("this user's process at no higher an integrity refused the read, so its DACL withholds it: %w", refusal)
	}
}

// classifyByDescriptor decides a process that refuses even a query-limited
// open, from the owner and integrity label of its own security descriptor.
//
// The handle that reads them cannot be fenced by creation time — that takes
// the right just refused — but no answer it gives clears a scan it should not:
// a PID that now names a later process means the one the row describes has
// exited, so it cannot be a session. An owner other than this user is left out
// of scope, as a refusal was before: a descriptor's owner does not always name
// the account the process runs as (an elevated process's is commonly the
// Administrators group).
func classifyByDescriptor(row windowsProcess, refusal error) (inspection, error) {
	owner, err := processObjectOwner(row.pid)
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return inspectionGone, nil
		}
		return inspectionDenied, nil
	}
	self, err := currentProcessOwner()
	if err != nil || !owner.user.Equals(self.user) {
		return inspectionDenied, nil
	}
	if owner.integrity > self.integrity {
		return inspectionElevated, nil
	}
	return inspectionFailed, fmt.Errorf("this user's process at no higher an integrity refused even a query-limited open, so its DACL withholds it: %w", refusal)
}

// systemMandatoryLabelACEType is SYSTEM_MANDATORY_LABEL_ACE_TYPE, the entry a
// security descriptor's SACL carries its integrity label in.
const systemMandatoryLabelACEType = 0x11

// mediumIntegrity is SECURITY_MANDATORY_MEDIUM_RID, the level Windows gives an
// object whose descriptor carries no integrity label.
const mediumIntegrity = 0x2000

// processObjectOwner reads the owner and integrity label from the security
// descriptor of the process pid names, which takes READ_CONTROL alone. An
// object's owner is granted READ_CONTROL whatever its DACL says, unless an
// OWNER RIGHTS entry takes that away as well.
func processObjectOwner(pid int) (processOwner, error) {
	handle, err := windows.OpenProcess(windows.READ_CONTROL, false, uint32(pid))
	if err != nil {
		return processOwner{}, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return processOwner{}, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return processOwner{}, err
	}
	if owner == nil || !owner.IsValid() {
		return processOwner{}, errors.New("proctable: process descriptor has no valid owner")
	}
	integrity, err := descriptorIntegrity(descriptor)
	if err != nil {
		return processOwner{}, err
	}
	return processOwner{user: owner, integrity: integrity}, nil
}

// descriptorIntegrity returns the RID of the integrity label in descriptor's
// SACL, or medium when it carries none.
func descriptorIntegrity(descriptor *windows.SECURITY_DESCRIPTOR) (uint32, error) {
	sacl, _, err := descriptor.SACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && sacl == nil) {
		return mediumIntegrity, nil
	}
	if err != nil {
		return 0, err
	}
	for i := range uint32(sacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(sacl, i, &ace); err != nil {
			return 0, err
		}
		if ace.Header.AceType != systemMandatoryLabelACEType {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sid.SubAuthorityCount() == 0 {
			return 0, errors.New("proctable: malformed integrity label")
		}
		return sid.SubAuthority(uint32(sid.SubAuthorityCount() - 1)), nil
	}
	return mediumIntegrity, nil
}

// processOwner is the account and mandatory integrity level a process token
// runs as.
type processOwner struct {
	user      *windows.SID
	integrity uint32
}

// currentProcessOwner is this process's own processOwner, read once.
var currentProcessOwner = sync.OnceValues(func() (processOwner, error) {
	return tokenOwner(windows.GetCurrentProcessToken())
})

func processOwnerOf(process windows.Handle) (processOwner, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return processOwner{}, err
	}
	defer token.Close() //nolint:errcheck
	return tokenOwner(token)
}

func tokenOwner(token windows.Token) (processOwner, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return processOwner{}, err
	}
	integrity, err := tokenIntegrity(token)
	if err != nil {
		return processOwner{}, err
	}
	return processOwner{user: user.User.Sid, integrity: integrity}, nil
}

// tokenIntegrity returns the RID of the token's mandatory integrity label:
// 0x1000 low, 0x2000 medium, 0x3000 high, 0x4000 system.
func tokenIntegrity(token windows.Token) (uint32, error) {
	size := uint32(64)
	for attempt := 0; ; attempt++ {
		buffer := make([]byte, size)
		err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buffer[0], uint32(len(buffer)), &size)
		if errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) && attempt == 0 && size > uint32(len(buffer)) {
			continue
		}
		if err != nil {
			return 0, err
		}
		label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0]))
		sid := label.Label.Sid
		if sid == nil || sid.SubAuthorityCount() == 0 {
			return 0, errors.New("proctable: token has no integrity label")
		}
		return sid.SubAuthority(uint32(sid.SubAuthorityCount() - 1)), nil
	}
}

// handleCreationTime returns the process creation time as Unix nanoseconds.
func handleCreationTime(handle windows.Handle) (int64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, fmt.Errorf("proctable: reading process times: %w", err)
	}
	nanoseconds := creation.Nanoseconds()
	if nanoseconds <= 0 {
		return 0, errors.New("proctable: process creation time is not set")
	}
	return nanoseconds, nil
}

// readParametersHeader walks handle's PEB to its RTL_USER_PROCESS_PARAMETERS
// and returns the fixed-size header.
//
// The header is fetched in one ReadProcessMemory, so the pointers and lengths
// read out of it describe one instant; reading fields one at a time lets a
// process that changes them between reads hand back a mixture.
func readParametersHeader(handle windows.Handle) ([]byte, error) {
	if pointerSize != 8 {
		return nil, errProcessInspectionUnsupported
	}

	information, err := queryProcessBasicInformation(handle)
	if err != nil {
		return nil, err
	}
	if information.PebBaseAddress == 0 {
		return nil, errors.New("proctable: process has no PEB")
	}

	parametersField, err := readRemoteMemory(handle, information.PebBaseAddress+pebProcessParametersOffset, pointerSize)
	if err != nil {
		return nil, fmt.Errorf("proctable: reading PEB: %w", err)
	}
	parametersAddress, ok := decodeUintptr(parametersField, 0)
	if !ok || parametersAddress == 0 {
		return nil, errors.New("proctable: process has no parameter block")
	}

	header, err := readRemoteMemory(handle, parametersAddress, paramsHeaderLength)
	if err != nil {
		return nil, fmt.Errorf("proctable: reading process parameters: %w", err)
	}
	return header, nil
}

// readProcessEnvironment returns handle's environment block. It reads nothing
// else, so a command line that cannot be read cannot cost the caller an
// environment that can.
func readProcessEnvironment(handle windows.Handle) (map[string]string, error) {
	header, err := readParametersHeader(handle)
	if err != nil {
		return nil, err
	}
	return readRemoteEnvironment(handle, header)
}

// readProcessCommandLine returns handle's command line.
func readProcessCommandLine(handle windows.Handle) (string, error) {
	header, err := readParametersHeader(handle)
	if err != nil {
		return "", err
	}
	commandLine, err := readRemoteUnicodeString(handle, header, paramsCommandLineOffset)
	if err != nil {
		return "", fmt.Errorf("proctable: reading command line: %w", err)
	}
	return commandLine, nil
}

func queryProcessBasicInformation(handle windows.Handle) (processBasicInformation, error) {
	var information processBasicInformation
	var returned uint32
	err := windows.NtQueryInformationProcess(
		handle,
		windows.ProcessBasicInformation,
		unsafe.Pointer(&information),
		uint32(unsafe.Sizeof(information)),
		&returned,
	)
	goruntime.KeepAlive(&information)
	if err != nil {
		return processBasicInformation{}, fmt.Errorf("proctable: querying process basic information: %w", err)
	}
	if returned < uint32(unsafe.Sizeof(information)) {
		return processBasicInformation{}, fmt.Errorf("proctable: process basic information returned %d bytes, want %d", returned, unsafe.Sizeof(information))
	}
	return information, nil
}

// readRemoteEnvironment reads the target's environment block and parses it.
//
// EnvironmentSize is used when it is plausible, and a page-by-page scan for the
// terminating double NUL takes over when it is not. The fallback exists because
// EnvironmentSize sits far enough into RTL_USER_PROCESS_PARAMETERS that a
// layout change there would otherwise turn into a confidently wrong length,
// while the double-NUL terminator is part of the block's own format.
//
// Either way the block is accepted only with its terminator in hand. A block
// read short parses into a map that is simply missing its tail, and the tail
// is where a session's GC_SESSION_ID can be.
func readRemoteEnvironment(handle windows.Handle, header []byte) (map[string]string, error) {
	address, ok := decodeUintptr(header, paramsEnvironmentOffset)
	if !ok || address == 0 {
		return nil, errors.New("proctable: process has no environment block")
	}
	declared, ok := decodeUintptr(header, paramsEnvironmentSizeOffset)
	if ok && declared >= 2 && declared <= maxEnvironmentBlock && declared%2 == 0 {
		if block, err := readRemoteMemory(handle, address, int(declared)); err == nil {
			if env, complete := parseCompleteEnvironmentBlock(decodeUTF16(block)); complete {
				return env, nil
			}
		}
	}
	units, err := scanRemoteEnvironment(handle, address)
	if err != nil {
		return nil, err
	}
	env, _ := parseCompleteEnvironmentBlock(units)
	return env, nil
}

// scanRemoteEnvironment reads the environment block a page at a time until the
// double NUL that ends it, bounded by maxEnvironmentBlock. It returns the
// units through that terminator, or an error. It never returns a block
// without one: running out of readable memory first means the block could not
// be read, not that it ended.
//
// A block is UTF-16, so it starts on an even address, and from one every read
// below is a whole number of units. An odd address is refused rather than
// scanned: each page after the first would decode a byte out of phase.
func scanRemoteEnvironment(handle windows.Handle, address uintptr) ([]uint16, error) {
	if address%2 != 0 {
		return nil, fmt.Errorf("proctable: environment block address %#x is not aligned for UTF-16", address)
	}
	var units []uint16
	next := address
	for len(units)*2 < maxEnvironmentBlock {
		size := environmentPage - int(next%environmentPage)
		chunk, err := readRemoteMemory(handle, next, size)
		if err != nil {
			return nil, fmt.Errorf("proctable: reading environment block at offset %d before its terminator: %w", len(units)*2, err)
		}
		searchFrom := max(len(units)-1, 0)
		units = append(units, decodeUTF16(chunk)...)
		if end := indexDoubleNULFrom(units, searchFrom); end >= 0 {
			return units[:end+2], nil
		}
		next += uintptr(size)
	}
	return nil, fmt.Errorf("proctable: environment block has no terminator within %d bytes", maxEnvironmentBlock)
}

// parseCompleteEnvironmentBlock parses units through the terminator that ends
// an environment block, and reports false when there is none.
func parseCompleteEnvironmentBlock(units []uint16) (map[string]string, bool) {
	end := indexDoubleNUL(units)
	if end < 0 {
		return nil, false
	}
	return parseEnvironmentBlock(units[:end+2]), true
}

// parseEnvironmentBlock decodes a Windows environment block: UTF-16LE
// KEY=VALUE entries separated by single NULs and terminated by an empty entry.
//
// Keys are upper-cased because Windows environment variable names are
// case-insensitive, so a lookup that matched only the exact spelling would miss
// a variable the OS considers the same one.
func parseEnvironmentBlock(units []uint16) map[string]string {
	env := make(map[string]string)
	start := 0
	for i := range units {
		if units[i] != 0 {
			continue
		}
		if i == start {
			break
		}
		entry := string(utf16.Decode(units[start:i]))
		start = i + 1
		key, value, ok := strings.Cut(entry, "=")
		// Windows stores per-drive working directories as entries whose key is
		// empty ("=C:=C:\dir"); they are not environment variables.
		if !ok || key == "" {
			continue
		}
		env[strings.ToUpper(key)] = value
	}
	return env
}

func indexDoubleNUL(units []uint16) int {
	return indexDoubleNULFrom(units, 0)
}

// indexDoubleNULFrom finds the first double NUL whose second unit is at or
// after from, so a scan that appends a page at a time need not re-search what
// it already has. The pair can straddle the previous page and this one.
func indexDoubleNULFrom(units []uint16, from int) int {
	for i := max(from, 1); i < len(units); i++ {
		if units[i] == 0 && units[i-1] == 0 {
			return i - 1
		}
	}
	return -1
}

// readRemoteUnicodeString follows the UNICODE_STRING at offset in header into
// the target's address space. An empty string is a legitimate value, not an
// error: a process started without arguments has a zero-length command line.
func readRemoteUnicodeString(handle windows.Handle, header []byte, offset int) (string, error) {
	length, ok := decodeUint16(header, offset)
	if !ok {
		return "", errors.New("proctable: parameter block is too short for a string field")
	}
	address, ok := decodeUintptr(header, offset+unicodeStringBufferOffset)
	if !ok {
		return "", errors.New("proctable: parameter block is too short for a string pointer")
	}
	if length == 0 || address == 0 {
		return "", nil
	}
	data, err := readRemoteMemory(handle, address, int(length))
	if err != nil {
		return "", err
	}
	return string(utf16.Decode(decodeUTF16(data))), nil
}

// readRemoteMemory copies size bytes out of another process.
//
// ReadProcessMemory is all-or-nothing across the requested range, so a short
// read means the range left mapped memory and the result cannot be trusted.
// The destination stays a plain []byte the kernel writes into; KeepAlive pins
// it across the call so nothing can retire the backing array while the kernel
// still holds its address.
func readRemoteMemory(handle windows.Handle, address uintptr, size int) ([]byte, error) {
	if address == 0 {
		return nil, errors.New("proctable: refusing to read from a null address")
	}
	if size <= 0 || size > maxEnvironmentBlock {
		return nil, fmt.Errorf("proctable: refusing to read %d bytes from another process", size)
	}
	buffer := make([]byte, size)
	var read uintptr
	err := windows.ReadProcessMemory(handle, address, &buffer[0], uintptr(size), &read)
	goruntime.KeepAlive(buffer)
	if err != nil {
		return nil, err
	}
	if int(read) != size {
		return nil, fmt.Errorf("proctable: read %d of %d bytes from another process", read, size)
	}
	return buffer, nil
}

func decodeUTF16(data []byte) []uint16 {
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return units
}

func decodeUintptr(data []byte, offset int) (uintptr, bool) {
	if offset < 0 || offset+8 > len(data) {
		return 0, false
	}
	return uintptr(binary.LittleEndian.Uint64(data[offset:])), true
}

func decodeUint16(data []byte, offset int) (uint16, bool) {
	if offset < 0 || offset+2 > len(data) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(data[offset:]), true
}

// isInfrastructureImage reports whether a Windows image name is tmux
// infrastructure rather than an agent.
//
// Toolhelp reports executables with their extension and in whatever case the
// file has on disk, neither of which isInfrastructureCommand — written against
// a Unix comm value — would ever match. Normalising both away first keeps one
// definition of what infrastructure is across all three platforms, so a root
// hidden from the sweep on Linux is hidden here too. Case folding is correct
// rather than merely lenient: Windows file names are case-insensitive, so
// TMUX.EXE and tmux.exe are the same executable.
func isInfrastructureImage(image string) bool {
	name := strings.ToLower(strings.TrimSpace(image))
	name = strings.TrimSuffix(name, ".exe")
	return isInfrastructureCommand(name)
}
