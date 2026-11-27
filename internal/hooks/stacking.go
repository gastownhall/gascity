package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
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
	// managedCurrent reports whether a gc-managed hook command under event,
	// bound to city ("" when unbound), is in the form gc ships today.
	managedCurrent func(event, command, city string) bool
}

// stackingProviders is keyed by provider family.
var stackingProviders = map[string]stackingProvider{
	"codex": {
		projectRelPath: ".codex/hooks.json",
		userPath:       codexUserHooksPath,
		managedKind:    codexManagedHookKind,
		managedCurrent: codexManagedHookCurrent,
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

// codexManagedHookCurrent reports whether command, a gc-managed Codex hook
// under event bound to city ("" when unbound), is in the form gc ships today.
// Since #6579 that form invokes gc as "${GC_BIN:-gc}" and no longer prepends
// gc's install dirs to PATH: a bare gc, or a PATH with those dirs first, runs
// whatever gc they hold, which can be an older build than the session's.
// Every generation before #6579 does both. The arguments are the ones
// hooks.Install and gc doctor upgrade every older generation to, compared as
// parsed commands, so shell quoting and the spelling of --city do not matter.
func codexManagedHookCurrent(event, command, city string) bool {
	prefix, gcToken, _, _, ok := parseManagedGCCommand(command)
	if !ok || gcToken != managedGCBinInvocation || strings.HasPrefix(prefix, canonicalGCPathPrefix) {
		return false
	}
	upgraded, changed := upgradeCodexHookCommand(event, command, city)
	switch {
	case upgraded == "":
		return false
	case !changed:
		return true
	default:
		return sameManagedGCCommand(command, upgraded)
	}
}

// sameManagedGCCommand reports whether two gc-managed hook commands run the
// same gc invocation: the same gc token, environment, --city binding and
// arguments once shell quoting is removed.
func sameManagedGCCommand(a, b string) bool {
	_, tokenA, envA, argsA, cityA, okA := parseManagedGCCommandWithCity(a)
	_, tokenB, envB, argsB, cityB, okB := parseManagedGCCommandWithCity(b)
	return okA && okB && tokenA == tokenB && cityA == cityB && maps.Equal(envA, envB) && slices.Equal(argsA, argsB)
}

// hookEntry is one entry of a provider hooks document and where it sits.
type hookEntry struct {
	event string
	// scope is the entry's group without its hook list, in canonical JSON: the
	// matcher and any other field that decides when the provider runs the
	// group.
	scope string
	// shape is the entry without its command, in canonical JSON: its type,
	// timeout and any other field that shapes how the provider runs it.
	shape   string
	command string
	// isCommand is false for an entry that carries no string command (a
	// non-command hook type, for example).
	isCommand bool
}

// hookSignature identifies a gc-managed hook entry for coverage: the event it
// runs on, the gc-managed kind of its command, and its scope and shape, so two
// entries share a signature only when the provider runs them alike.
type hookSignature struct {
	event, kind, scope, shape string
}

func (e hookEntry) signature(kind string) hookSignature {
	return hookSignature{event: e.event, kind: kind, scope: e.scope, shape: e.shape}
}

// walkHookEntries calls fn for every entry under doc["hooks"][event][group]["hooks"].
// It reports false when the document does not have that shape: a hooks value
// that is not an object, an event that is not a list of groups, a group that
// is not an object holding a list of hooks, or an entry that is not an object.
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
			scope, ok := canonicalWithout(group, "hooks")
			if !ok {
				return false
			}
			for _, rawEntry := range entries {
				entry, ok := rawEntry.(map[string]any)
				if !ok {
					return false
				}
				shape, ok := canonicalWithout(entry, "command")
				if !ok {
					return false
				}
				command, isCommand := entry["command"].(string)
				fn(hookEntry{event: event, scope: scope, shape: shape, command: command, isCommand: isCommand})
			}
		}
	}
	return true
}

// canonicalWithout returns m without key, in canonical JSON.
func canonicalWithout(m map[string]any, key string) (string, bool) {
	rest := maps.Clone(m)
	delete(rest, key)
	data, err := overlay.MarshalCanonicalJSON(rest)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// decodeHookDoc parses data as exactly one JSON object.
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
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected data after the JSON object")
	}
	return doc, nil
}

// UserLayerCoverage is the verdict of ProjectHooksCoveredByUserLayer.
type UserLayerCoverage int

const (
	// UserLayerMissesHooks reports that the user layer does not run every hook
	// the project file would, so the provider needs the project file.
	UserLayerMissesHooks UserLayerCoverage = iota
	// UserLayerCovers reports that the user layer runs every hook the project
	// file would, so staging the project file only makes each of them fire
	// twice.
	UserLayerCovers
	// UserLayerStale reports that the user layer carries every hook the
	// project file would, but some only in a form older than the one gc ships,
	// so the provider still needs the project file to run the current form.
	UserLayerStale
)

// ProjectHooksCoveredByUserLayer reports whether the project-level hook file a
// stacking provider family would receive from sources (the overlay documents
// staging merges into it) is redundant with userDoc, the user-level hook file
// the provider also loads.
//
// The file is covered only when every hook entry in every source is
// gc-managed, unbound or bound to cityPath, the sources carry at least one
// such entry and nothing but hooks, and each source entry has an equivalent in
// userDoc: an entry under the same event, in a group with the same matcher
// (and every other group field), with the same fields besides its command,
// whose command is the same gc-managed kind, unbound or bound to cityPath, in
// the form gc ships today. Skipping a covered file therefore never drops a
// pack-authored hook or one bound to another city, never narrows when a gc
// hook runs, and never leaves a gc hook running only in an older form.
// A userDoc that would cover the file but carries some of those entries only
// in an older form is UserLayerStale.
//
// An unparseable source or userDoc returns an error, as does a userDoc that is
// not shaped as a hooks document; each means the file is not covered.
func ProjectHooksCoveredByUserLayer(family, cityPath string, userDoc []byte, sources [][]byte) (UserLayerCoverage, error) {
	sp, ok := stackingProviders[family]
	if !ok {
		return UserLayerMissesHooks, nil
	}
	needed, err := sp.sourceHookSignatures(cityPath, sources)
	if err != nil || len(needed) == 0 {
		return UserLayerMissesHooks, err
	}
	current, anyForm, err := sp.userHookSignatures(cityPath, userDoc)
	if err != nil {
		return UserLayerMissesHooks, err
	}
	switch {
	case hasEverySignature(current, needed):
		return UserLayerCovers, nil
	case hasEverySignature(anyForm, needed):
		return UserLayerStale, nil
	default:
		return UserLayerMissesHooks, nil
	}
}

// boundElsewhere reports whether city, the --city binding of a gc-managed
// command ("" when unbound), names a city other than cityPath.
func boundElsewhere(city, cityPath string) bool {
	return city != "" && filepath.Clean(city) != filepath.Clean(cityPath)
}

// sourceHookSignatures returns the signatures of the hook entries sources
// carry, or none when a source carries anything but gc-managed hooks that are
// unbound or bound to cityPath.
func (sp stackingProvider) sourceHookSignatures(cityPath string, sources [][]byte) (map[hookSignature]bool, error) {
	needed := make(map[hookSignature]bool)
	for _, src := range sources {
		doc, err := decodeHookDoc(src)
		if err != nil {
			return nil, fmt.Errorf("parsing overlay hooks source: %w", err)
		}
		for key := range doc {
			if key != "hooks" {
				return nil, nil
			}
		}
		coverable := true
		shaped := walkHookEntries(doc, func(e hookEntry) {
			kind, city, managed := sp.managedKind(e.event, e.command)
			if !e.isCommand || !managed || boundElsewhere(city, cityPath) {
				coverable = false
				return
			}
			needed[e.signature(kind)] = true
		})
		if !shaped || !coverable {
			return nil, nil
		}
	}
	return needed, nil
}

// userHookSignatures returns the signatures of userDoc's gc-managed entries
// that are unbound or bound to cityPath: current holds those in the form gc
// ships today, anyForm those in any form. A userDoc that cannot be parsed, or
// is not shaped as a hooks document, returns an error.
func (sp stackingProvider) userHookSignatures(cityPath string, userDoc []byte) (current, anyForm map[hookSignature]bool, err error) {
	user, err := decodeHookDoc(userDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing user hooks: %w", err)
	}
	current = make(map[hookSignature]bool)
	anyForm = make(map[hookSignature]bool)
	shaped := walkHookEntries(user, func(e hookEntry) {
		if !e.isCommand {
			return
		}
		kind, city, managed := sp.managedKind(e.event, e.command)
		if !managed || boundElsewhere(city, cityPath) {
			return
		}
		anyForm[e.signature(kind)] = true
		if sp.managedCurrent(e.event, e.command, city) {
			current[e.signature(kind)] = true
		}
	})
	if !shaped {
		return nil, nil, fmt.Errorf("parsing user hooks: not a hooks document")
	}
	return current, anyForm, nil
}

func hasEverySignature(have, want map[hookSignature]bool) bool {
	for sig := range want {
		if !have[sig] {
			return false
		}
	}
	return true
}

// StripManagedProjectHooks removes from a stacking provider's project-level
// hook document every gc-managed entry that userDoc, the user-level hook file
// the provider also loads, already runs: an entry of any shipped generation,
// unbound or bound to cityPath, whose event, kind, scope and shape match an
// entry userDoc carries in the form gc ships today, unbound or bound to
// cityPath. Every other entry is kept, and groups and events left empty are
// dropped. rest is the remaining document in canonical JSON; empty reports
// that nothing is left; changed reports that an entry was removed. A document
// with no such entry is returned unchanged. An unparseable userDoc or doc
// returns an error, as does a userDoc that is not shaped as a hooks document.
func StripManagedProjectHooks(family, cityPath string, userDoc, doc []byte) (rest []byte, empty, changed bool, err error) {
	sp, ok := stackingProviders[family]
	if !ok {
		return doc, false, false, fmt.Errorf("provider family %q has no stacking hook files", family)
	}
	covered, _, err := sp.userHookSignatures(cityPath, userDoc)
	if err != nil {
		return doc, false, false, err
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
			scope, scoped := canonicalWithout(group, "hooks")
			if !ok || !isList || !scoped {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			kept := make([]any, 0, len(entries))
			for _, rawEntry := range entries {
				if sp.coveredEntry(event, scope, cityPath, rawEntry, covered) {
					changed = true
					continue
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

// coveredEntry reports whether rawEntry, a hook entry under event in a group
// whose scope is scope, is a gc-managed command, unbound or bound to cityPath,
// whose signature covered holds.
func (sp stackingProvider) coveredEntry(event, scope, cityPath string, rawEntry any, covered map[hookSignature]bool) bool {
	entry, ok := rawEntry.(map[string]any)
	if !ok {
		return false
	}
	command, isCommand := entry["command"].(string)
	if !isCommand {
		return false
	}
	kind, city, managed := sp.managedKind(event, command)
	if !managed || boundElsewhere(city, cityPath) {
		return false
	}
	shape, ok := canonicalWithout(entry, "command")
	return ok && covered[hookSignature{event: event, kind: kind, scope: scope, shape: shape}]
}
