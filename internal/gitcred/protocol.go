package gitcred

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Request is a git-credential protocol request: the key=value lines git writes
// to a credential helper's stdin, terminated by a blank line or EOF. Only the
// keys the helper needs are captured; unknown keys are ignored per the
// protocol.
type Request struct {
	Protocol string
	Host     string
	Path     string
	Username string
}

// ReadRequest parses a git-credential request from r. It reads key=value lines
// until a blank line or EOF and ignores unrecognized keys, as the protocol
// requires. A line without '=' is a malformed request.
func ReadRequest(r io.Reader) (Request, error) {
	var req Request
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Request{}, fmt.Errorf("malformed request line %q", line)
		}
		switch key {
		case "protocol":
			req.Protocol = value
		case "host":
			req.Host = value
		case "path":
			req.Path = value
		case "username":
			req.Username = value
		}
	}
	if err := scanner.Err(); err != nil {
		return Request{}, fmt.Errorf("reading credential request: %w", err)
	}
	return req, nil
}

// WriteCredential writes a credential to w in the git-credential wire format
// ("username=...\npassword=...\n"). git reads exactly these two keys from a
// helper's "get" response.
func WriteCredential(w io.Writer, c Credential) error {
	if _, err := fmt.Fprintf(w, "username=%s\npassword=%s\n", c.Username, c.Password); err != nil {
		return fmt.Errorf("writing credential: %w", err)
	}
	return nil
}
