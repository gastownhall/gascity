package gitcred

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultUsername is the username sent to the remote when a rule omits one.
// GitHub (and other token-as-password hosts) ignore the username but require a
// non-empty value; "x-access-token" is the GitHub App installation-token
// convention.
const DefaultUsername = "x-access-token"

// Credential is a resolved username/password pair handed to git over the
// credential-helper wire. The password is a token; it lives only in this value
// and the helper→git pipe, never in a file gc writes or an env gc exports.
type Credential struct {
	Username string
	Password string
}

// Resolve reads the single pointer field set on rule and returns the resolved
// credential. It never returns the secret in an error: every error names the
// pointer (the helper command, the file path, or the env var NAME), not its
// contents. The caller must have validated that exactly one pointer is set;
// Resolve treats an SSHKeyFile rule as unsupported here (ssh is handled via
// GIT_SSH_COMMAND on the injection side, not the credential helper).
func Resolve(rule Rule) (Credential, error) {
	username := strings.TrimSpace(rule.Username)
	if username == "" {
		username = DefaultUsername
	}
	switch {
	case rule.Helper != "":
		token, err := runHelper(rule.Helper)
		if err != nil {
			return Credential{}, err
		}
		if token == "" {
			return Credential{}, fmt.Errorf("credential helper %q produced no output", rule.Helper)
		}
		return Credential{Username: username, Password: token}, nil
	case rule.TokenFile != "":
		path := expandUser(rule.TokenFile)
		data, err := os.ReadFile(path)
		if err != nil {
			return Credential{}, fmt.Errorf("reading token file %q: %w", rule.TokenFile, err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return Credential{}, fmt.Errorf("token file %q is empty", rule.TokenFile)
		}
		return Credential{Username: username, Password: token}, nil
	case rule.TokenEnv != "":
		token := strings.TrimSpace(os.Getenv(rule.TokenEnv))
		if token == "" {
			return Credential{}, fmt.Errorf("environment variable %s is unset or empty", rule.TokenEnv)
		}
		return Credential{Username: username, Password: token}, nil
	default:
		return Credential{}, fmt.Errorf("rule for %q has no resolvable credential pointer", rule.Match)
	}
}

// RunCredentialCommand runs the $GC_GIT_CREDENTIAL_COMMAND helper for req. The
// command runs via "sh -c" with the git-credential request piped to its stdin.
// Empty stdout means the helper declines (ok=false, no error), matching git's
// own credential-helper convention.
func RunCredentialCommand(command string, req Request) (Credential, bool, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return Credential{}, false, nil
	}
	cmd := exec.Command("sh", "-c", command)
	var stdin strings.Builder
	writeRequestLine(&stdin, "protocol", req.Protocol)
	writeRequestLine(&stdin, "host", req.Host)
	writeRequestLine(&stdin, "path", req.Path)
	writeRequestLine(&stdin, "username", req.Username)
	stdin.WriteString("\n")
	cmd.Stdin = strings.NewReader(stdin.String())
	out, err := cmd.Output()
	if err != nil {
		return Credential{}, false, fmt.Errorf("running credential command: %w", err)
	}
	cred, ok := parseCredentialResponse(string(out))
	return cred, ok, nil
}

func writeRequestLine(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	b.WriteString(key)
	b.WriteString("=")
	b.WriteString(value)
	b.WriteString("\n")
}

// parseCredentialResponse reads a username=/password= response body. It returns
// ok=false when the body carries no password (an empty decline).
func parseCredentialResponse(body string) (Credential, bool) {
	var cred Credential
	hasPassword := false
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "username":
			cred.Username = value
		case "password":
			cred.Password = value
			hasPassword = true
		}
	}
	if !hasPassword {
		return Credential{}, false
	}
	if cred.Username == "" {
		cred.Username = DefaultUsername
	}
	return cred, true
}

func runHelper(command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running credential helper %q: %w", command, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// expandUser expands a leading "~" or "~/" to the current user's home
// directory. A path that does not begin with "~" is returned unchanged.
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}
