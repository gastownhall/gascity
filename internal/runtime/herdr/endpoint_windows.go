//go:build windows

package herdr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// herdrConfigDir returns the directory herdr resolves its configuration, and
// with it every session socket, under. It reproduces herdr's own Windows
// resolution, measured against herdr 0.9.1 because herdr documents none of it:
// the first of these variables present in the environment wins, and its value
// is used as given, even when it is empty or relative.
//
//	XDG_CONFIG_HOME   <value>
//	APPDATA           <value>
//	USERPROFILE       <value>\AppData\Roaming
//	HOME              <value>\.config
//
// When none is present, herdr falls back to the temp directory. os.UserConfigDir
// reads APPDATA alone, so it disagrees with herdr whenever XDG_CONFIG_HOME is set.
func herdrConfigDir() string {
	if dir, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok {
		return dir
	}
	if dir, ok := os.LookupEnv("APPDATA"); ok {
		return dir
	}
	if dir, ok := os.LookupEnv("USERPROFILE"); ok {
		return herdrJoin(dir, "AppData", "Roaming")
	}
	if dir, ok := os.LookupEnv("HOME"); ok {
		return herdrJoin(dir, ".config")
	}
	return os.TempDir()
}

// herdrJoin appends each element to base as herdr builds its paths (Rust's
// PathBuf::push with a relative element): after a separator unless base is
// empty, already ends in one, or is a bare drive ("C:"), and with nothing
// cleaned. Cleaning is not neutral here. filepath.Clean stops a ".." at the
// volume root (C:\..\x is C:\x), but in the pipe name herdr makes of the path,
// \\.\pipe\C:\..\x, the ".." removes "C:" when Windows normalizes the name.
func herdrJoin(base string, elem ...string) string {
	for _, e := range elem {
		if base != "" && !strings.HasSuffix(base, `\`) && !strings.HasSuffix(base, "/") && !isBareDrive(base) {
			base += `\`
		}
		base += e
	}
	return base
}

// isBareDrive reports whether p is a drive letter and colon alone.
func isBareDrive(p string) bool {
	return len(p) == 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z')
}

// herdrPipeName returns the named pipe herdr serves the socket at sockPath on.
// herdr's Windows build does not bind the path: it creates the pipe
// \\.\pipe\<sockPath> and leaves only a small pointer file at the path itself.
// Windows normalizes a pipe name when the pipe is created, so the name is
// normalized here by the same routine (GetFullPathName), not by Go's path
// cleaning, whose rules differ on trailing dots and spaces. Normalization can
// also leave the pipe namespace (\\.\pipe\..\x is the device \\.\x); such a
// name is refused rather than opened.
func herdrPipeName(sockPath string) (string, error) {
	const prefix = `\\.\pipe\`
	name, err := syscall.FullPath(prefix + sockPath)
	if err != nil {
		return "", fmt.Errorf("herdr socket %q: pipe name: %w", sockPath, err)
	}
	if len(name) <= len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", endpointRefusal{fmt.Errorf("herdr socket %q names no pipe: it normalizes to %q", sockPath, name)}
	}
	return name, nil
}

// dialUnix is the production connect, installed on every client by newClient
// and reached through the field of the same name. On Windows it connects to the
// named pipe herdr serves the socket path on, and keeps the connection only if
// this user's herdr could have created the pipe (verifyPipeCreator). The
// client's impersonation level is anonymous, so the server cannot act as this
// user.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	name, err := herdrPipeName(path)
	if err != nil {
		return nil, err
	}
	h, err := openPipe(ctx, name)
	if err != nil {
		return nil, err
	}
	return pipeConn(h, name)
}

// openPipe opens the client end of the pipe name the way go-winio's dial does:
// for reading and writing, overlapped, at the anonymous impersonation level,
// and retrying while every instance is busy until ctx ends. go-winio's dial
// itself is not used, because it leaks the handle it opened when the server
// hangs up before the dial returns, which a server can repeat at will.
func openPipe(ctx context.Context, name string) (windows.Handle, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: name, Err: err}
	}
	for {
		if err := ctx.Err(); err != nil {
			return windows.InvalidHandle, err
		}
		h, err := windows.CreateFile(name16,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_ANONYMOUS,
			0)
		if err == nil {
			return h, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return windows.InvalidHandle, &os.PathError{Op: "open", Path: name, Err: err}
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// pipeConn takes the handle h, open on the pipe name, and returns it as a
// connection once the pipe is verified; on any failure it closes h.
func pipeConn(h windows.Handle, name string) (net.Conn, error) {
	if err := verifyPipeCreator(h, name); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	// As in go-winio's dial, a server that has already hung up fails the dial.
	var flags uint32
	if err := windows.GetNamedPipeInfo(h, &flags, nil, nil, nil); err != nil {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f, err := winio.NewOpenFile(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	file, ok := f.(pipeFile)
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("herdr pipe %s: go-winio's file %T has no deadlines", name, f)
	}
	return &pipeClient{pipeFile: file, name: name}, nil
}

// pipeFile is the file go-winio's NewOpenFile makes of a handle: overlapped
// reads and writes, with deadlines.
type pipeFile interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// pipeClient is the client end of a herdr pipe, as a connection.
type pipeClient struct {
	pipeFile
	name string
}

// Read reads the pipe as a byte stream. herdr's pipes are byte pipes; a message
// pipe reports a message longer than b as ERROR_MORE_DATA and returns the rest
// on the next read, so that is not an error (go-winio reads its own message
// pipes the same way).
func (c *pipeClient) Read(b []byte) (int, error) {
	n, err := c.pipeFile.Read(b)
	if errors.Is(err, windows.ERROR_MORE_DATA) {
		err = nil
	}
	return n, err
}

func (c *pipeClient) LocalAddr() net.Addr  { return pipeAddr(c.name) }
func (c *pipeClient) RemoteAddr() net.Addr { return pipeAddr(c.name) }

func (c *pipeClient) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// pipeAddr is a pipe's name as a net.Addr.
type pipeAddr string

func (pipeAddr) Network() string  { return "pipe" }
func (a pipeAddr) String() string { return string(a) }

// verifyPipeCreator accepts the pipe h is open on only if this user's herdr
// could have created it, and runs before anything is written to it. A socket
// inside the user's own config directory can be bound only by a process that
// can write there, but a pipe name is machine-wide: any process can create
// herdr's pipe name before herdr does. The pipe's creator decides two things
// about it. Its owner is the creating token's default owner, so a pipe this
// user's herdr created carries one of pipeOwners, and any other owner means
// another account created it. A process below Medium integrity (a sandboxed
// one) labels what it creates with its own level, and a pipe with no label is
// Medium, so a label below Medium means such a process created the pipe; it
// could not have written the user's config directory.
func verifyPipeCreator(h windows.Handle, name string) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("herdr pipe %s: read owner: %w", name, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("herdr pipe %s: read owner: %w", name, err)
	}
	if owner == nil {
		return endpointRefusal{fmt.Errorf("herdr pipe %s has no owner: refusing a pipe whose creator cannot be confirmed", name)}
	}
	allowed, err := pipeOwners()
	if err != nil {
		return fmt.Errorf("herdr pipe %s: %w", name, err)
	}
	owned := false
	for _, sid := range allowed {
		if owner.Equals(sid) {
			owned = true
			break
		}
	}
	if !owned {
		return endpointRefusal{fmt.Errorf("herdr pipe %s is owned by %s, not by this user: refusing a pipe another account created", name, owner)}
	}
	level, err := mandatoryIntegrity(sd)
	if err != nil {
		return fmt.Errorf("herdr pipe %s: read integrity label: %w", name, err)
	}
	if level < mandatoryMediumRID {
		return endpointRefusal{fmt.Errorf("herdr pipe %s is labeled %s integrity, below Medium: refusing a pipe a lower-integrity process created", name, integrityName(level))}
	}
	return nil
}

// The mandatory label ACE type and the Medium integrity level, which
// x/sys/windows does not define.
const (
	systemMandatoryLabelACEType = 0x11
	mandatoryMediumRID          = 0x2000
)

// mandatoryIntegrity returns the integrity level sd's mandatory label names:
// the last subauthority of its S-1-16 SID. A descriptor with no label is
// Medium.
func mandatoryIntegrity(sd *windows.SECURITY_DESCRIPTOR) (uint32, error) {
	sacl, _, err := sd.SACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && sacl == nil) {
		return mandatoryMediumRID, nil
	}
	if err != nil {
		return 0, err
	}
	for i := uint32(0); i < uint32(sacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(sacl, i, &ace); err != nil {
			return 0, err
		}
		if ace.Header.AceType != systemMandatoryLabelACEType {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.IdentifierAuthority() != windows.SECURITY_MANDATORY_LABEL_AUTHORITY || sid.SubAuthorityCount() == 0 {
			return 0, fmt.Errorf("the mandatory label names %s, not an integrity level", sid)
		}
		return sid.SubAuthority(uint32(sid.SubAuthorityCount()) - 1), nil
	}
	return mandatoryMediumRID, nil
}

// integrityName names the integrity levels below Medium, and gives any other
// level as its number.
func integrityName(level uint32) string {
	switch level {
	case 0:
		return "Untrusted"

	case 0x1000:
		return "Low"

	default:
		return fmt.Sprintf("%#x", level)
	}
}

// pipeOwners lists the owners a pipe created by this user can carry: the user
// itself, and the token's default owner when that differs, as it does for an
// elevated administrator, whose new objects belong to the Administrators group.
// It is a variable so a test can require an owner the pipe does not have.
var pipeOwners = func() ([]*windows.SID, error) {
	return tokenPipeOwners(windows.GetCurrentProcessToken())
}

// tokenPipeOwners lists the owners a pipe created under token carries: the
// token's user, and the token's default owner when that differs.
func tokenPipeOwners(token windows.Token) ([]*windows.SID, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("token user: %w", err)
	}
	owners := []*windows.SID{user.User.Sid}
	// TOKEN_OWNER is a single PSID pointing into the same buffer.
	buf := make([]byte, 64)
	for {
		var n uint32
		err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], uint32(len(buf)), &n)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || n <= uint32(len(buf)) {
			return nil, fmt.Errorf("token default owner: %w", err)
		}
		buf = make([]byte, n)
	}
	def := *(**windows.SID)(unsafe.Pointer(&buf[0]))
	if def == nil || def.Equals(user.User.Sid) {
		return owners, nil
	}
	// def points into buf, which the compiler cannot see, so it may keep buf on
	// this function's stack; the stack is reused once the function returns.
	owned, err := def.Copy()
	if err != nil {
		return nil, fmt.Errorf("token default owner: %w", err)
	}
	return append(owners, owned), nil
}

// removeStaleSocket has nothing to clear on Windows. herdr serves the socket
// path as a named pipe, a kernel object that disappears with its server. The
// pointer file it leaves at the path after an unclean exit does not stop a new
// server from starting: the new server overwrites it.
func (c *client) removeStaleSocket() {}
