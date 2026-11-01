// Package providerreadiness inspects configured provider runtimes without a model turn.
package providerreadiness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/execgrace"
)

const (
	// Configured means the native CLI reports local credentials configured.
	Configured = "configured"
	// NeedsAuth means the native CLI explicitly reports no credentials.
	NeedsAuth = "needs_auth"
	// NotInstalled means the selected executable cannot be started because it is absent.
	NotInstalled = "not_installed"
	// ProbeError means the local check failed to establish a verdict.
	ProbeError = "probe_error"
	// Unverified means no safe automatic protocol is available for this command.
	Unverified = "unverified"
)

// Target is the executable and environment used by one resolved provider.
// Family comes from provider inheritance, never from a user-chosen alias.
type Target struct {
	Family  string
	Command string
	Args    []string
	Env     map[string]string
}

// Result describes local configuration, not remote inference availability.
// Detail contains no provider output or credential values.
type Result struct{ Status, Detail string }

type statusOutput struct{ stdout, stderr []byte }

var runStatus = func(ctx context.Context, command string, args []string, env map[string]string) (statusOutput, error) {
	command, err := configuredExecutable(command, env)
	if err != nil {
		return statusOutput{}, err
	}
	cmd := exec.CommandContext(ctx, command, args...)
	execgrace.Apply(cmd, time.Second)
	cmd.Env = make([]string, 0, len(env))
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	sort.Strings(cmd.Env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return statusOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes()}, err
}

// configuredExecutable mirrors executable lookup using the worker environment.
// exec.Command otherwise resolves a bare name using the supervisor's PATH,
// before Cmd.Env is applied. An explicitly empty PATH must remain empty.
func configuredExecutable(command string, env map[string]string) (string, error) {
	if filepath.Base(command) != command {
		return command, nil
	}
	path, configured := env["PATH"]
	if !configured {
		return exec.LookPath(command)
	}
	for _, directory := range filepath.SplitList(path) {
		candidate := filepath.Join(directory, command)
		if !filepath.IsAbs(candidate) {
			candidate = "." + string(os.PathSeparator) + candidate
		}
		executable, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(executable) {
			return "", exec.ErrDot
		}
		return executable, nil
	}
	return "", exec.ErrNotFound
}

// Probe inspects the selected executable without starting a model conversation.
// It reports supported native login modes equally. Wrapper commands and provider
// families without a local status protocol remain explicitly unverified.
func Probe(parent context.Context, target Target) Result {
	if parent.Err() != nil {
		return Result{ProbeError, "provider check canceled"}
	}
	if target.Family != "claude" && target.Family != "codex" {
		return Result{Unverified, "no automatic local login-status check for this provider; verify it with a real task"}
	}
	if strings.ContainsAny(target.Command, " \t\n") {
		// An existing absolute executable may contain spaces. A compound launch
		// command cannot safely be given a native CLI's subcommands.
		if info, err := os.Stat(target.Command); err != nil || info.IsDir() {
			return Result{Unverified, "compound provider command requires a real task to verify readiness"}
		}
	}
	for _, arg := range target.Args {
		if strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "--profile=") || strings.HasPrefix(arg, "--settings=") || (strings.HasPrefix(arg, "-c") && len(arg) > 2) {
			return Result{Unverified, "provider configuration arguments require a real task to verify readiness"}
		}
		switch arg {
		case "run", "--config", "-c", "--profile", "--oss", "--local-provider", "--settings", "--setting-sources":
			return Result{Unverified, "provider wrapper or configuration arguments require a real task to verify readiness"}
		}
	}
	args := []string{"auth", "status", "--json"}
	if target.Family == "codex" {
		args = []string{"login", "status"}
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	output, err := runStatus(ctx, target.Command, args, target.Env)
	if ctx.Err() != nil {
		return Result{ProbeError, "provider status check canceled or timed out"}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return Result{NotInstalled, "configured provider executable is unavailable"}
	}
	if target.Family == "claude" {
		var status struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		if json.Unmarshal(output.stdout, &status) != nil || status.LoggedIn == nil {
			return Result{ProbeError, "configured Claude executable did not return a valid auth-status verdict"}
		}
		if !*status.LoggedIn {
			if target.Env["CLAUDE_CODE_USE_BEDROCK"] == "1" || target.Env["CLAUDE_CODE_USE_VERTEX"] == "1" || target.Env["CLAUDE_CODE_USE_FOUNDRY"] == "1" {
				return Result{Unverified, "cloud-provider credentials require a real task to verify readiness"}
			}
			return Result{NeedsAuth, "configured Claude runtime reports no login"}
		}
		if err != nil {
			return Result{ProbeError, "configured Claude status command failed"}
		}
		return Result{Status: Configured}
	}
	// Codex writes status to stderr in some versions. Diagnostics may precede
	// the verdict on either stream, but contradictory verdicts never mean ready.
	text := string(output.stdout) + "\n" + string(output.stderr)
	loggedIn, loggedOut := false, false
	for _, line := range strings.Split(text, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		loggedIn = loggedIn || strings.HasPrefix(line, "logged in")
		loggedOut = loggedOut || strings.HasPrefix(line, "not logged in")
	}
	if loggedOut && !loggedIn {
		return Result{NeedsAuth, "configured Codex runtime reports no login"}
	}
	if err == nil && loggedIn && !loggedOut {
		return Result{Status: Configured}
	}

	return Result{ProbeError, "configured Codex executable did not return a valid login-status verdict"}
}
