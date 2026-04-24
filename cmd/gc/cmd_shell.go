package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	shellHookMarkerBegin = "# >>> gc shell integration >>>"
	shellHookMarkerEnd   = "# <<< gc shell integration <<<"
)

func newShellCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Manage the Gas City shell integration hook",
		Long: `The shell integration adds a completion hook to your shell RC file that
provides tab-completion for gc commands and flags.

Subcommands: install, remove, status.`,
	}
	cmd.AddCommand(
		newShellInstallCmd(stdout, stderr),
		newShellRemoveCmd(stdout, stderr),
		newShellStatusCmd(stdout, stderr),
	)
	return cmd
}

func newShellInstallCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "install [bash|zsh|fish]",
		Short: "Install or update shell integration",
		Long: `Install or update the gc shell completion hook.

If no shell is specified, the shell is detected from $SHELL.
The completion script is written to ~/.gc/completions/ and a source line
is added to your shell RC file.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmdShellInstall(cmd.Root(), args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newShellRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove",
		Short: "Remove shell integration",
		Long:  `Remove the gc shell completion hook from your shell RC file and delete the completion script.`,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdShellRemove(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newShellStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show shell integration status",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdShellStatus(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// detectShell returns "bash", "zsh", or "fish" from the SHELL env var.
func detectShell(shellEnv string) (string, error) {
	parts := strings.Split(shellEnv, "/")
	base := strings.ToLower(parts[len(parts)-1])
	switch base {
	case "bash", "zsh", "fish":
		return base, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (expected bash, zsh, or fish)", base)
	}
}

// shellRCFile returns the canonical RC file path for a shell.
func shellRCFile(sh string) (string, error) {
	rcFiles, err := shellRCFiles(sh)
	if err != nil {
		return "", err
	}
	switch sh {
	case "bash":
		// Prefer .bashrc; fall back to .bash_profile if .bashrc doesn't exist.
		rc := rcFiles[0]
		if _, err := os.Stat(rc); err == nil {
			return rc, nil
		}
		return rcFiles[1], nil
	case "zsh", "fish":
		return rcFiles[0], nil
	default:
		return "", fmt.Errorf("unsupported shell %q", sh)
	}
}

// shellRCFiles returns all RC files relevant to a shell.
func shellRCFiles(sh string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("determining home directory: %w", err)
	}
	switch sh {
	case "bash":
		return []string{
			filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".bash_profile"),
		}, nil
	case "zsh":
		return []string{filepath.Join(home, ".zshrc")}, nil
	case "fish":
		return []string{filepath.Join(home, ".config", "fish", "config.fish")}, nil
	default:
		return nil, fmt.Errorf("unsupported shell %q", sh)
	}
}

// installedShellRCFile returns the RC file that currently contains the hook.
func installedShellRCFile(sh string) (string, error) {
	rcFiles, err := shellRCFiles(sh)
	if err != nil {
		return "", err
	}
	for _, rcFile := range rcFiles {
		hasHook, err := rcFileHasHook(rcFile)
		if err != nil || !hasHook {
			continue
		}
		return rcFile, nil
	}
	return "", os.ErrNotExist
}

// completionDir returns ~/.gc/completions.
func completionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gc", "completions"), nil
}

// completionFile returns the path to the completion script for a shell.
func completionFile(sh string) (string, error) {
	dir, err := completionDir()
	if err != nil {
		return "", err
	}
	switch sh {
	case "bash":
		return filepath.Join(dir, "gc.bash"), nil
	case "zsh":
		return filepath.Join(dir, "_gc"), nil
	case "fish":
		return filepath.Join(dir, "gc.fish"), nil
	default:
		return "", fmt.Errorf("unsupported shell %q", sh)
	}
}

// generateCompletion generates the completion script for the given shell
// using cobra's built-in generators.
func generateCompletion(root *cobra.Command, sh string) ([]byte, error) {
	var buf bytes.Buffer
	switch sh {
	case "bash":
		if err := root.GenBashCompletionV2(&buf, true); err != nil {
			return nil, err
		}
	case "zsh":
		if err := root.GenZshCompletion(&buf); err != nil {
			return nil, err
		}
	case "fish":
		if err := root.GenFishCompletion(&buf, true); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported shell %q", sh)
	}
	return buf.Bytes(), nil
}

// hookBlock returns the lines to insert into the RC file.
func hookBlock(sh, compFile string) string {
	var source string
	switch sh {
	case "fish":
		source = fmt.Sprintf("test -f %q && source %q", compFile, compFile)
	default: // bash, zsh
		source = fmt.Sprintf("[[ -f %q ]] && source %q", compFile, compFile)
	}
	return shellHookMarkerBegin + "\n" + source + "\n" + shellHookMarkerEnd + "\n"
}

func cmdShellInstall(root *cobra.Command, args []string, stdout, stderr io.Writer) int {
	sh, err := resolveShellArg(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc shell install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Generate completion script.
	script, err := generateCompletion(root, sh)
	if err != nil {
		fmt.Fprintf(stderr, "gc shell install: generating completion: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Write completion script to file.
	compFile, err := completionFile(sh)
	if err != nil {
		fmt.Fprintf(stderr, "gc shell install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(compFile), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc shell install: creating directory: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := atomicWriteFile(compFile, script); err != nil {
		fmt.Fprintf(stderr, "gc shell install: writing completion script: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Wrote completion script to %s\n", compFile) //nolint:errcheck // best-effort stdout

	// Add source line to RC file.
	rcFile, err := shellRCFile(sh)
	if err != nil {
		fmt.Fprintf(stderr, "gc shell install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if existingRC, err := installedShellRCFile(sh); err == nil {
		rcFile = existingRC
	}
	if err := os.MkdirAll(filepath.Dir(rcFile), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc shell install: creating directory: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	installed, err := rcFileHasHook(rcFile)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc shell install: reading %s: %v\n", rcFile, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if installed {
		// Update in place — the completion script is already refreshed on disk.
		if err := rcFileReplaceHook(rcFile, hookBlock(sh, compFile)); err != nil {
			fmt.Fprintf(stderr, "gc shell install: updating %s: %v\n", rcFile, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Updated hook in %s\n", rcFile) //nolint:errcheck // best-effort stdout
	} else {
		if err := rcFileAppendHook(rcFile, hookBlock(sh, compFile)); err != nil {
			fmt.Fprintf(stderr, "gc shell install: updating %s: %v\n", rcFile, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Added hook to %s\n", rcFile) //nolint:errcheck // best-effort stdout
	}

	fmt.Fprintf(stdout, "Restart your shell or run: source %s\n", rcFile) //nolint:errcheck // best-effort stdout
	return 0
}

func cmdShellRemove(stdout, stderr io.Writer) int {
	// Try all shells — remove whatever we find.
	removed := false
	for _, sh := range []string{"bash", "zsh", "fish"} {
		compFile, err := completionFile(sh)
		if err != nil {
			continue
		}
		if _, err := os.Stat(compFile); err == nil {
			if err := os.Remove(compFile); err != nil {
				fmt.Fprintf(stderr, "gc shell remove: removing %s: %v\n", compFile, err) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stdout, "Removed %s\n", compFile) //nolint:errcheck // best-effort stdout
				removed = true
			}
		}

		rcFiles, err := shellRCFiles(sh)
		if err != nil {
			continue
		}
		for _, rcFile := range rcFiles {
			has, err := rcFileHasHook(rcFile)
			if err != nil || !has {
				continue
			}
			if err := rcFileRemoveHook(rcFile); err != nil {
				fmt.Fprintf(stderr, "gc shell remove: updating %s: %v\n", rcFile, err) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stdout, "Removed hook from %s\n", rcFile) //nolint:errcheck // best-effort stdout
				removed = true
			}
		}
	}
	if !removed {
		fmt.Fprintln(stdout, "No shell integration found to remove.") //nolint:errcheck // best-effort stdout
	}
	return 0
}

func cmdShellStatus(stdout, _ io.Writer) int {
	found := false
	for _, sh := range []string{"bash", "zsh", "fish"} {
		compFile, err := completionFile(sh)
		if err != nil {
			continue
		}
		rcFiles, err := shellRCFiles(sh)
		if err != nil {
			continue
		}
		hasScript := false
		if _, err := os.Stat(compFile); err == nil {
			hasScript = true
		}
		hasHook := false
		rcFile, err := shellRCFile(sh)
		if err != nil {
			continue
		}
		for _, candidate := range rcFiles {
			candidateHasHook, err := rcFileHasHook(candidate)
			if err != nil || !candidateHasHook {
				continue
			}
			hasHook = true
			rcFile = candidate
			break
		}

		if hasScript || hasHook {
			found = true
			status := "installed"
			if hasScript && !hasHook {
				status = "completion script exists but RC hook missing"
			} else if !hasScript && hasHook {
				status = "RC hook present but completion script missing"
			}
			fmt.Fprintf(stdout, "%s: %s\n", sh, status)     //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stdout, "  script: %s\n", compFile) //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stdout, "  rc:     %s\n", rcFile)   //nolint:errcheck // best-effort stdout
		}
	}
	if !found {
		fmt.Fprintln(stdout, "Shell integration is not installed.") //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Run: gc shell install")               //nolint:errcheck // best-effort stdout
	}
	return 0
}

// ── RC file manipulation ────────────────────────────────────────────────

// rcFileHasHook reports whether the RC file contains our marker block.
func rcFileHasHook(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return bytes.Contains(data, []byte(shellHookMarkerBegin)), nil
}

// rcFileAppendHook appends the hook block to the RC file.
func rcFileAppendHook(path, block string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	// Ensure we start on a new line.
	_, err = f.WriteString("\n" + block)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// rcFileReplaceHook replaces the existing hook block in the RC file.
func rcFileReplaceHook(path, block string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	result := replaceHookBlock(string(data), block)
	return atomicWriteFile(path, []byte(result))
}

// rcFileRemoveHook removes the hook block from the RC file.
func rcFileRemoveHook(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	result := replaceHookBlock(string(data), "")
	return atomicWriteFile(path, []byte(result))
}

// replaceHookBlock replaces or removes the marker block in content.
func replaceHookBlock(content, replacement string) string {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(content))
	inBlock := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == shellHookMarkerBegin {
			inBlock = true
			if replacement != "" {
				out.WriteString(replacement)
			}
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == shellHookMarkerEnd {
				inBlock = false
			}
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// atomicWriteFile writes data to a temp file then renames into place.
func atomicWriteFile(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func resolveShellArg(args []string) (string, error) {
	if len(args) > 0 {
		sh := strings.ToLower(strings.TrimSpace(args[0]))
		switch sh {
		case "bash", "zsh", "fish":
			return sh, nil
		default:
			return "", fmt.Errorf("unsupported shell %q (expected bash, zsh, or fish)", args[0])
		}
	}
	return detectShell(os.Getenv("SHELL"))
}
