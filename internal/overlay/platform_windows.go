//go:build windows

package overlay

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// copilotHooksPath is GitHub Copilot CLI's repository hook file.
var copilotHooksPath = filepath.Join(".github", "hooks", "gascity.json")

// copilotInstructionsPath is GitHub Copilot CLI's repository instruction file.
var copilotInstructionsPath = filepath.Join(".github", "copilot-instructions.md")

// posixGCHookPrefix starts every command in the bundled Copilot hook file: it
// appends the usual go-install directories to PATH, then runs the gc that
// GC_BIN names, or gc.
const posixGCHookPrefix = `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" `

// powerShellGCHookPrefix is posixGCHookPrefix in PowerShell. Its fallback is
// gc.exe, because PowerShell resolves a bare gc to its Get-Content alias.
const powerShellGCHookPrefix = `$env:PATH = "$env:PATH;$HOME\go\bin;$HOME\.local\bin"; & $(if ($env:GC_BIN) { $env:GC_BIN } else { 'gc.exe' }) `

// AdaptSettingsForPlatform gives each bash command in Copilot CLI's hook file
// a powershell twin. On Windows, Copilot CLI runs a hook's powershell field and
// falls back to its bash field only when there is none; that bash is whatever
// PATH offers, typically WSL's, which has no gc.
//
// Only a command of the bundled shape is translated: posixGCHookPrefix
// followed by arguments that mean the same to PowerShell as to bash. An entry
// that already has a powershell field keeps it, and any other command is left
// as it is. Every other file is returned unchanged, as is a document that does
// not parse.
func AdaptSettingsForPlatform(relPath string, data []byte) []byte {
	switch {
	case isCopilotHooksPath(relPath):
		return adaptCopilotHooks(data)
	case isInstructionMarkdownPath(relPath):
		return adaptInstructionMarkdown(data)
	default:
		return data
	}
}

func platformAdaptsPath(relPath string) bool {
	return isCopilotHooksPath(relPath) || isInstructionMarkdownPath(relPath)
}

func platformAdaptsDirectoryCopies() bool {
	return true
}

// ManagedFileNeedsPlatformUpgrade returns a predicate that identifies the
// released Windows-managed bytes this build should silently upgrade. It is nil
// when relPath has no Windows-only managed-byte adaptation.
func ManagedFileNeedsPlatformUpgrade(relPath string, legacyData, desiredData []byte) func([]byte) bool {
	if !isInstructionMarkdownPath(relPath) || bytes.Equal(legacyData, desiredData) {
		return nil
	}
	legacy := append([]byte(nil), legacyData...)
	return func(existing []byte) bool {
		return bytes.Equal(existing, legacy)
	}
}

func preservePlatformManagedFile(relPath string, existing, legacyData, desiredData []byte) bool {
	if !isInstructionMarkdownPath(relPath) || bytes.Equal(legacyData, desiredData) {
		return false
	}
	return !bytes.Equal(existing, legacyData)
}

func platformPreserveExistingPath(relPath string) bool {
	return isRootAgentsMarkdownPath(relPath)
}

func isCopilotHooksPath(relPath string) bool {
	return strings.EqualFold(cleanPlatformRelPath(relPath), filepath.ToSlash(copilotHooksPath))
}

func isInstructionMarkdownPath(relPath string) bool {
	clean := cleanPlatformRelPath(relPath)
	return strings.EqualFold(clean, filepath.ToSlash(copilotInstructionsPath)) ||
		isRootAgentsMarkdownPath(clean)
}

func isRootAgentsMarkdownPath(relPath string) bool {
	return strings.EqualFold(cleanPlatformRelPath(relPath), "AGENTS.md")
}

func cleanPlatformRelPath(relPath string) string {
	return filepath.ToSlash(filepath.Clean(relPath))
}

func adaptCopilotHooks(data []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return data
	}
	changed := false
	for _, event := range hooks {
		entries, ok := event.([]any)
		if !ok {
			continue
		}
		for _, item := range entries {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, has := entry["powershell"]; has {
				continue
			}
			bash, ok := entry["bash"].(string)
			if !ok {
				continue
			}
			if ps, ok := powerShellGCHook(bash); ok {
				entry["powershell"] = ps
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	out, err := MarshalCanonicalJSON(doc)
	if err != nil {
		return data
	}
	return out
}

func adaptInstructionMarkdown(data []byte) []byte { return data }

func powerShellGCHook(bash string) (string, bool) {
	args, ok := strings.CutPrefix(bash, posixGCHookPrefix)
	if !ok || !portableHookArgs(args) {
		return "", false
	}
	return powerShellGCHookPrefix + args, true
}

// portableHookArgs reports whether args reach gc as the same argument list
// from PowerShell as from bash: words separated by single spaces, each either
// bare or wholly enclosed in double quotes, made of letters, digits and the
// characters -._/=: (and spaces, inside quotes). A word of hyphens such as --
// passes through both shells unchanged (measured on pwsh 7.6 and Windows
// PowerShell 5.1).
func portableHookArgs(args string) bool {
	words := 0
	for i := 0; i < len(args); {
		switch {
		case args[i] == ' ':
			i++
		case args[i] == '"':
			end := strings.IndexByte(args[i+1:], '"')
			if end <= 0 || !portableHookWord(args[i+1:i+1+end], true) {
				return false
			}
			i += end + 2
			if i < len(args) && args[i] != ' ' {
				return false
			}
			words++
		default:
			end := strings.IndexByte(args[i:], ' ')
			if end < 0 {
				end = len(args) - i
			}
			if !portableHookWord(args[i:i+end], false) {
				return false
			}
			i += end
			words++
		}
	}
	return words > 0
}

func portableHookWord(word string, quoted bool) bool {
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-._/=:", r):
		case quoted && r == ' ':
		default:
			return false
		}
	}
	return true
}
