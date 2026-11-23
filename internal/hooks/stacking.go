package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/overlay"
)

// stackingProvider describes a provider CLI that loads a user-level hook file
// alongside every project-level one and runs the hooks of both, instead of
// letting the nearer file win. Staging gc's project-level copy for such a
// provider fires every gc hook twice when the user layer already carries it.
type stackingProvider struct {
	// projectRelPath is the work-dir-relative, slash-separated project hook file.
	projectRelPath string
	// userPath resolves the user-level hook file from the session environment;
	// ok is false when it cannot be determined.
	userPath func(getenv func(string) string) (string, bool)
	// managedKind classifies one hook command under event as a gc-managed hook
	// kind, reporting the command's --city binding ("" when unbound).
	managedKind func(event, command string) (kind, city string, ok bool)
}

// stackingProviders is keyed by provider family.
var stackingProviders = map[string]stackingProvider{
	"codex": {
		projectRelPath: ".codex/hooks.json",
		userPath:       codexUserHooksPath,
		managedKind:    codexManagedHookKind,
	},
}

// Codex gc-managed hook kinds, one per command the core overlay ships.
const (
	codexKindSessionStartPrime = "SessionStart/prime"
	codexKindPreCompactHandoff = "PreCompact/handoff"
	codexKindPromptNudgeDrain  = "UserPromptSubmit/nudge-drain"
	codexKindPromptMailCheck   = "UserPromptSubmit/mail-check"
)

// StackedProjectHookPath returns the project-level hook file a stacking
// provider family loads in addition to its user-level one. ok is false for
// families whose hook files do not stack.
func StackedProjectHookPath(family string) (relPath string, ok bool) {
	sp, ok := stackingProviders[family]
	if !ok {
		return "", false
	}
	return sp.projectRelPath, true
}

// StackedUserHooksPath returns the user-level hook file a stacking provider
// family loads, resolved through getenv (the environment the provider process
// inherits). ok is false when the family does not stack or the path cannot be
// determined from the environment.
func StackedUserHooksPath(family string, getenv func(string) string) (string, bool) {
	sp, ok := stackingProviders[family]
	if !ok || getenv == nil {
		return "", false
	}
	return sp.userPath(getenv)
}

// codexUserHooksPath is $CODEX_HOME/hooks.json when CODEX_HOME is set, else
// $HOME/.codex/hooks.json. A relative CODEX_HOME or HOME resolves against the
// provider's working directory, which differs per session, so it is reported
// as undeterminable.
func codexUserHooksPath(getenv func(string) string) (string, bool) {
	if codexHome := getenv("CODEX_HOME"); codexHome != "" {
		if !filepath.IsAbs(codexHome) {
			return "", false
		}
		return filepath.Join(codexHome, "hooks.json"), true
	}
	home := getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return "", false
	}
	return filepath.Join(home, ".codex", "hooks.json"), true
}

// codexManagedHookKind classifies a Codex hook command as one of the
// gc-managed hook kinds, across every shipped generation of the core overlay
// (with or without --hook-format codex, with or without the gc hook run
// timeout wrapper, either PATH prefix, bare gc or "${GC_BIN:-gc}").
func codexManagedHookKind(event, command string) (string, string, bool) {
	_, _, env, args, city, ok := parseManagedGCCommandWithCity(command)
	if !ok {
		return "", "", false
	}
	switch event {
	case "SessionStart":
		if codexSessionStartArgsMatch(env, args) || codexLegacySessionStartRunArgsMatch(args) {
			return codexKindSessionStartPrime, city, true
		}
	case "PreCompact":
		if codexPreCompactArgsMatch(args) {
			return codexKindPreCompactHandoff, city, true
		}
	case "UserPromptSubmit":
		target, ok := codexManagedPromptTargetArgs(args, "codex")
		if !ok {
			target, ok = codexManagedPromptTargetArgs(args, "")
		}
		if !ok {
			return "", "", false
		}
		switch {
		case strings.HasPrefix(target, "nudge drain"):
			return codexKindPromptNudgeDrain, city, true
		case strings.HasPrefix(target, "mail check"):
			return codexKindPromptMailCheck, city, true
		}
	}
	return "", "", false
}

// hookEntry is one entry of a provider hooks document and the event it sits under.
type hookEntry struct {
	event   string
	command string
	// isCommand is false for an entry that carries no string command (a
	// non-command hook type or a malformed entry).
	isCommand bool
}

// walkHookEntries calls fn for every entry under doc["hooks"][event][group]["hooks"].
// It reports false when the document does not have that shape.
func walkHookEntries(doc map[string]any, fn func(hookEntry)) bool {
	rawHooks, ok := doc["hooks"]
	if !ok {
		return true
	}
	events, ok := rawHooks.(map[string]any)
	if !ok {
		return false
	}
	for event, rawGroups := range events {
		groups, ok := rawGroups.([]any)
		if !ok {
			return false
		}
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				return false
			}
			entries, ok := group["hooks"].([]any)
			if !ok {
				return false
			}
			for _, rawEntry := range entries {
				entry, _ := rawEntry.(map[string]any)
				command, isCommand := entry["command"].(string)
				fn(hookEntry{event: event, command: command, isCommand: isCommand})
			}
		}
	}
	return true
}

func decodeHookDoc(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return doc, nil
}

// ProjectHooksCoveredByUserLayer reports whether the project-level hook file a
// stacking provider family would receive from sources (the overlay documents
// staging merges into it) is redundant with userDoc, the user-level hook file
// the provider also loads. It is covered only when every hook entry in every
// source is gc-managed, the sources carry at least one such entry and nothing
// but hooks, and for each gc-managed kind they contribute userDoc holds a
// gc-managed entry of the same kind that is unbound or bound to cityPath.
// Skipping a covered file therefore never drops a pack-authored hook.
//
// An unparseable userDoc returns an error; an unparseable source returns an
// error too. Both mean not covered.
func ProjectHooksCoveredByUserLayer(family, cityPath string, userDoc []byte, sources [][]byte) (bool, error) {
	sp, ok := stackingProviders[family]
	if !ok {
		return false, nil
	}
	needed := make(map[string]bool)
	for _, src := range sources {
		doc, err := decodeHookDoc(src)
		if err != nil {
			return false, fmt.Errorf("parsing overlay hooks source: %w", err)
		}
		for key := range doc {
			if key != "hooks" {
				return false, nil
			}
		}
		allManaged := true
		shaped := walkHookEntries(doc, func(e hookEntry) {
			kind, _, managed := sp.managedKind(e.event, e.command)
			if !e.isCommand || !managed {
				allManaged = false
				return
			}
			needed[kind] = true
		})
		if !shaped || !allManaged {
			return false, nil
		}
	}
	if len(needed) == 0 {
		return false, nil
	}
	user, err := decodeHookDoc(userDoc)
	if err != nil {
		return false, fmt.Errorf("parsing user hooks: %w", err)
	}
	have := make(map[string]bool)
	walkHookEntries(user, func(e hookEntry) {
		if !e.isCommand {
			return
		}
		kind, city, managed := sp.managedKind(e.event, e.command)
		if !managed {
			return
		}
		if city != "" && filepath.Clean(city) != filepath.Clean(cityPath) {
			return
		}
		have[kind] = true
	})
	for kind := range needed {
		if !have[kind] {
			return false, nil
		}
	}
	return true, nil
}

// StripManagedProjectHooks removes every gc-managed hook entry of any shipped
// generation from a stacking provider's project-level hook document, dropping
// groups and events it empties. rest is the remaining document in canonical
// JSON; empty reports that nothing is left; changed reports that an entry was
// removed. A document with no gc-managed entry is returned unchanged.
func StripManagedProjectHooks(family string, doc []byte) (rest []byte, empty, changed bool, err error) {
	sp, ok := stackingProviders[family]
	if !ok {
		return doc, false, false, fmt.Errorf("provider family %q has no stacking hook files", family)
	}
	root, err := decodeHookDoc(doc)
	if err != nil {
		return doc, false, false, fmt.Errorf("parsing project hooks: %w", err)
	}
	events, ok := root["hooks"].(map[string]any)
	if !ok {
		return doc, false, false, nil
	}
	for event, rawGroups := range events {
		groups, ok := rawGroups.([]any)
		if !ok {
			continue
		}
		keptGroups := make([]any, 0, len(groups))
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			entries, isList := group["hooks"].([]any)
			if !ok || !isList {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			kept := make([]any, 0, len(entries))
			for _, rawEntry := range entries {
				entry, _ := rawEntry.(map[string]any)
				if command, isCommand := entry["command"].(string); isCommand {
					if _, _, managed := sp.managedKind(event, command); managed {
						changed = true
						continue
					}
				}
				kept = append(kept, rawEntry)
			}
			if len(kept) == 0 && len(entries) > 0 {
				continue
			}
			group["hooks"] = kept
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 && len(groups) > 0 {
			delete(events, event)
			continue
		}
		events[event] = keptGroups
	}
	if !changed {
		return doc, false, false, nil
	}
	if len(events) == 0 {
		delete(root, "hooks")
	}
	if len(root) == 0 {
		return nil, true, true, nil
	}
	out, err := overlay.MarshalCanonicalJSON(root)
	if err != nil {
		return doc, false, false, fmt.Errorf("encoding project hooks: %w", err)
	}
	return out, false, true, nil
}
