// Package credsource parses and resolves the per-city / per-rig bearer
// credential source for a remote beads backend (city.toml [beads] credential,
// rigs.beads_credential): "env:NAME", "command:<argv>" or "file:<path>".
//
// The config file names WHERE the credential lives, never the credential
// itself: anything that is not one of the three source forms is refused, so a
// token pasted inline fails config load instead of being treated as a source.
// No error from this package ever carries a credential value, nor the raw
// configured string (which, when malformed, may be a pasted secret).
package credsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/shlex"
)

// Kind names where a credential source reads its token from.
type Kind string

const (
	// KindEnv reads the token from one named process environment variable.
	KindEnv Kind = "env"
	// KindCommand runs an argv (no shell) and reads the token from its stdout.
	KindCommand Kind = "command"
	// KindFile reads the token from a file.
	KindFile Kind = "file"
)

// Source is a parsed credential source. The zero value is "no source".
type Source struct {
	Kind Kind
	// Env is the variable name for KindEnv.
	Env string
	// Argv is the command for KindCommand, split shell-style but never run
	// through a shell.
	Argv []string
	// Path is the file for KindFile. A relative path is resolved against the
	// city directory (ResolveOptions.Dir).
	Path string
}

// IsZero reports whether s names no source.
func (s Source) IsZero() bool { return s.Kind == "" }

// String renders the source for diagnostics without any secret: the variable
// name, the command's program (never its arguments, which may carry one), or
// the file path.
func (s Source) String() string {
	switch s.Kind {
	case KindEnv:
		return "env:" + s.Env
	case KindCommand:
		if len(s.Argv) == 0 {
			return "command:"
		}
		return "command:" + s.Argv[0]
	case KindFile:
		return "file:" + s.Path
	default:
		return ""
	}
}

// Key identifies the source for cache comparison. It carries the whole argv
// (configuration, not a resolved credential) and is never logged.
func (s Source) Key() string {
	return string(s.Kind) + "\x00" + s.Env + "\x00" + strings.Join(s.Argv, "\x00") + "\x00" + s.Path
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ErrMalformed is the parse refusal. Its message never echoes the value.
var ErrMalformed = errors.New("malformed credential source")

// Parse parses a configured credential source. An empty string is the zero
// Source with no error (unset). The returned error never contains raw.
func Parse(raw string) (Source, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Source{}, nil
	}
	kind, rest, ok := strings.Cut(trimmed, ":")
	if !ok {
		return Source{}, fmt.Errorf("%w: want env:NAME, command:<argv> or file:<path>; inline secrets are not accepted (the value is not echoed)", ErrMalformed)
	}
	rest = strings.TrimSpace(rest)
	switch Kind(strings.TrimSpace(kind)) {
	case KindEnv:
		if !envNamePattern.MatchString(rest) {
			return Source{}, fmt.Errorf("%w: env: wants an environment variable name (letters, digits, underscore; the value is not echoed)", ErrMalformed)
		}
		return Source{Kind: KindEnv, Env: rest}, nil
	case KindCommand:
		argv, err := shlex.Split(rest)
		if err != nil || len(argv) == 0 {
			return Source{}, fmt.Errorf("%w: command: wants a non-empty argv with balanced quotes (run without a shell; the value is not echoed)", ErrMalformed)
		}
		return Source{Kind: KindCommand, Argv: argv}, nil
	case KindFile:
		if rest == "" {
			return Source{}, fmt.Errorf("%w: file: wants a path", ErrMalformed)
		}
		return Source{Kind: KindFile, Path: rest}, nil
	default:
		return Source{}, fmt.Errorf("%w: want env:NAME, command:<argv> or file:<path>; inline secrets are not accepted (the value is not echoed)", ErrMalformed)
	}
}

// Typed resolution failures. A *ResolveError wraps exactly one of them.
var (
	// ErrEmpty: the source resolved to no token (unset variable, empty file,
	// command that printed nothing). A configured source that yields nothing
	// fails closed; it never falls back to an ambient credential.
	ErrEmpty = errors.New("resolved to an empty credential")
	// ErrInvalidToken: the token contains whitespace or control characters
	// and cannot be sent as a bearer.
	ErrInvalidToken = errors.New("resolved credential is not a single bearer token (whitespace or control characters)")
	// ErrUnreadable: the file could not be read.
	ErrUnreadable = errors.New("credential file unreadable")
	// ErrCommandFailed: the command could not run, timed out or exited
	// non-zero. Its stdout and stderr are never included.
	ErrCommandFailed = errors.New("credential command failed")
)

// ResolveError is a typed resolution failure. It names the source (redacted,
// Source.String) and never carries the credential.
type ResolveError struct {
	Source string
	Err    error
}

// Error implements error.
func (e *ResolveError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("beads credential %s: %v", e.Source, e.Err)
}

// Unwrap exposes the typed cause.
func (e *ResolveError) Unwrap() error { return e.Err }

// DefaultCommandTimeout bounds one credential command run.
const DefaultCommandTimeout = 30 * time.Second

// maxFileBytes bounds a credential file read.
const maxFileBytes = 64 << 10

// ResolveOptions supplies the process seams a resolution reads through.
type ResolveOptions struct {
	// Getenv reads one variable for KindEnv. Nil uses os.Getenv.
	Getenv func(string) string
	// Environ is the environment a command runs with. Nil inherits the
	// process environment.
	Environ func() []string
	// Dir resolves a relative file path and is a command's working directory.
	Dir string
	// CommandTimeout bounds a command; zero uses DefaultCommandTimeout.
	CommandTimeout time.Duration
}

// Resolve reads the token now. Every failure is a *ResolveError.
func (s Source) Resolve(ctx context.Context, opts ResolveOptions) (string, error) {
	fail := func(err error) (string, error) { return "", &ResolveError{Source: s.String(), Err: err} }
	if ctx == nil {
		ctx = context.Background()
	}
	var raw string
	switch s.Kind {
	case KindEnv:
		getenv := opts.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		raw = getenv(s.Env)
	case KindFile:
		path := s.Path
		if !filepath.IsAbs(path) && opts.Dir != "" {
			path = filepath.Join(opts.Dir, path)
		}
		data, err := readBounded(path)
		if err != nil {
			return fail(fmt.Errorf("%w: %w", ErrUnreadable, err))
		}
		raw = string(data)
	case KindCommand:
		out, err := runCommand(ctx, s.Argv, opts)
		if err != nil {
			return fail(err)
		}
		raw = out
	default:
		return fail(fmt.Errorf("%w: no source configured", ErrMalformed))
	}
	token := strings.TrimSpace(raw)
	if token == "" {
		return fail(ErrEmpty)
	}
	for _, r := range token {
		if r <= ' ' || r == 0x7f {
			return fail(ErrInvalidToken)
		}
	}
	return token, nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-configured credential path
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxFileBytes)
	}
	return data, nil
}

func runCommand(parent context.Context, argv []string, opts ResolveOptions) (string, error) {
	timeout := opts.CommandTimeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- operator-configured credential command, no shell
	cmd.Dir = opts.Dir
	if opts.Environ != nil {
		cmd.Env = opts.Environ()
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// stderr is discarded: a helper may print secrets there, and this
	// package never relays a credential source's output into an error.
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: %w", ErrCommandFailed, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%w: exit status %d", ErrCommandFailed, exitErr.ExitCode())
		}
		return "", fmt.Errorf("%w: %w", ErrCommandFailed, err)
	}
	return stdout.String(), nil
}
