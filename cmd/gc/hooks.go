package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// beadHooks maps bd hook filenames to the Gas City event types they emit.
var beadHooks = map[string]string{
	"on_create": "bead.created",
	"on_close":  "bead.closed",
	"on_update": "bead.updated",
}

// hookStampLine returns a version-stamp comment for hook scripts. The stamp
// embeds the build date and commit so that installBeadHooks can enforce
// forward-only writes — a stale gc binary will not overwrite hooks installed
// by a newer binary.
func hookStampLine() string {
	return fmt.Sprintf("# gc-hook-stamp: %s %s", date, commit)
}

// parseHookStampDate extracts the build date from a hook script's stamp line.
// Returns empty string if no stamp is found.
func parseHookStampDate(content []byte) string {
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("# gc-hook-stamp: ")) {
			parts := bytes.Fields(line)
			if len(parts) >= 3 {
				return string(parts[2])
			}
		}
	}
	return ""
}

// hookCityBlock returns the shell fragment that defines CITY_PATH and the
// matching --city argument to gc event emit. When cityPath is empty the
// fragment is empty — preserving the original "walk up from cwd" behavior
// for callers that have not yet been updated to thread the city path.
//
// The cityPath is embedded at install time so the hook script does not
// depend on the bd write's cwd being a descendant of the city. Sibling-rig
// topologies (rig at /Code/foo, city at /Code/bar) used to silently drop
// events because findCity walked up from the rig and never reached the city.
func hookCityBlock(cityPath string) (def, arg string) {
	if cityPath == "" {
		return "", ""
	}
	return fmt.Sprintf("CITY_PATH=%s\n", shellSingleQuote(cityPath)), ` --city "$CITY_PATH"`
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// hookScript returns the shell script content for a bd hook that forwards
// events to the Gas City event log via gc event emit. Failures are
// captured to ${BEADS_DIR}/hooks.log so silent breakage (missing gc on
// PATH, store-resolution errors) is diagnosable after the fact.
//
// When cityPath is non-empty it is baked into the script as
// a shell-quoted CITY_PATH and gc event emit is invoked with --city
// "$CITY_PATH". This avoids the findCity walk-up that fails when
// the bd write's cwd is a sibling (not a descendant) of the city.
func hookScript(eventType, cityPath string) string {
	cityDef, cityArg := hookCityBlock(cityPath)
	return fmt.Sprintf(`#!/bin/sh
%[1]s
# Installed by gc — forwards bd events to Gas City event log.
# Args: $1=issue_id  $2=event_type  stdin=issue JSON
GC_BIN="${GC_BIN:-gc}"
HOOK_LOG="${BEADS_DIR:-.beads}/hooks.log"
%[5]sDATA=$(cat)
PAYLOAD=$(printf '{"bead":%%s}' "$DATA")
title=$(echo "$DATA" | grep -o '"title":"[^"]*"' | head -1 | cut -d'"' -f4)
(
  "$GC_BIN"%[6]s event emit %[2]s --subject "$1" --message "$title" --payload "$PAYLOAD" --bead-payload "$1" 2>>"$HOOK_LOG" \
    || echo "[$(date -u +%%FT%%TZ)] %[3]s $1: gc event emit %[2]s failed (gc=$GC_BIN)" >>"$HOOK_LOG" 2>/dev/null \
    || true
) </dev/null >/dev/null 2>&1 &
`, hookStampLine(), eventType, hookNameFromEventType(eventType), "", cityDef, cityArg)
}

// hookNameFromEventType maps event types back to the hook filename
// (e.g. "bead.created" → "on_create") so diagnostic log lines name the
// hook the operator can grep for.
func hookNameFromEventType(eventType string) string {
	for hook, evt := range beadHooks {
		if evt == eventType {
			return hook
		}
	}
	return "hook"
}

// closeHookScript returns the on_close hook script. It forwards the
// bead.closed event, triggers convoy autoclose for the closed bead's
// parent convoy (if any), and auto-closes any open molecule/wisp
// children attached to the closed bead. Workflow-control watches the city
// event stream directly, so the close hook no longer sends a separate poke.
//
// Failures of any of the three gc invocations are logged with a dated
// diagnostic line to ${BEADS_DIR}/hooks.log. Without this, missing gc
// or a store-resolution error here leaves no record at all and the
// cascade-close of sling scaffolding fails invisibly.
//
// When cityPath is non-empty it is baked into the script as
// a shell-quoted CITY_PATH and the gc invocations carry --city "$CITY_PATH".
func closeHookScript(cityPath string) string {
	cityDef, cityArg := hookCityBlock(cityPath)
	return fmt.Sprintf(`#!/bin/sh
%[1]s
# Installed by gc — forwards bd close events, auto-closes completed convoys,
# and auto-closes orphaned wisps.
# Args: $1=issue_id  $2=event_type  stdin=issue JSON
GC_BIN="${GC_BIN:-gc}"
HOOK_LOG="${BEADS_DIR:-.beads}/hooks.log"
%[2]sDATA=$(cat)
PAYLOAD=$(printf '{"bead":%%s}' "$DATA")
title=$(echo "$DATA" | grep -o '"title":"[^"]*"' | head -1 | cut -d'"' -f4)
(
  "$GC_BIN"%[3]s event emit bead.closed --subject "$1" --message "$title" --payload "$PAYLOAD" --bead-payload "$1" 2>>"$HOOK_LOG" \
    || echo "[$(date -u +%%FT%%TZ)] on_close $1: gc event emit bead.closed failed (gc=$GC_BIN)" >>"$HOOK_LOG" 2>/dev/null \
    || true
  # Auto-close parent convoy if all siblings are now closed.
  "$GC_BIN"%[3]s convoy autoclose "$1" 2>>"$HOOK_LOG" \
    || echo "[$(date -u +%%FT%%TZ)] on_close $1: gc convoy autoclose failed (gc=$GC_BIN)" >>"$HOOK_LOG" 2>/dev/null \
    || true
  # Auto-close open molecule/wisp children so they don't outlive the parent.
  "$GC_BIN"%[3]s wisp autoclose "$1" 2>>"$HOOK_LOG" \
    || echo "[$(date -u +%%FT%%TZ)] on_close $1: gc wisp autoclose failed (gc=$GC_BIN)" >>"$HOOK_LOG" 2>/dev/null \
    || true
  # Auto-close parent molecule when all step children are terminal (#1039).
  "$GC_BIN"%[3]s molecule autoclose "$1" 2>>"$HOOK_LOG" \
    || echo "[$(date -u +%%FT%%TZ)] on_close $1: gc molecule autoclose failed (gc=$GC_BIN)" >>"$HOOK_LOG" 2>/dev/null \
    || true
) </dev/null >/dev/null 2>&1 &
`, hookStampLine(), cityDef, cityArg)
}

// installBeadHooks writes bd hook scripts into dir/.beads/hooks/ so that
// bd mutations (create, close, update) emit events to the Gas City event
// log. Forward-only — a stale gc binary will not overwrite hooks installed
// by a newer binary. Returns nil on success.
//
// cityPath is baked into each generated hook script so gc event emit
// can resolve the city without walking up from the bd write's cwd. When
// cityPath is "" the scripts are generated without the --city flag,
// preserving the legacy "walk up from cwd" behavior; this is appropriate
// for tests and for callers that genuinely don't know the city yet.
func installBeadHooks(dir, cityPath string) error {
	hooksDir := filepath.Join(dir, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("creating hooks directory: %w", err)
	}

	for filename, eventType := range beadHooks {
		path := filepath.Join(hooksDir, filename)
		content := hookScript(eventType, cityPath)
		if filename == "on_close" {
			content = closeHookScript(cityPath)
		}

		if existing, err := os.ReadFile(path); err == nil {
			if date != "unknown" {
				onDiskDate := parseHookStampDate(existing)
				if onDiskDate != "" && onDiskDate != "unknown" && date < onDiskDate {
					continue
				}
			}
		}

		if err := fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, path, []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing hook %s: %w", filename, err)
		}
	}
	return nil
}
