// config_hash.go provides canonical config hashing for session-first drift
// detection. Unlike runtime.CoreFingerprint which hashes a runtime.Config,
// canonicalConfigHash operates on TemplateParams + overlay — producing the
// same hash regardless of whether the config came from agent resolution or
// session bead overlay reconstruction.
package main

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// canonicalConfigHash computes a SHA-256 hash over the behavioral fields of
// a resolved template, optionally merged with overlay overrides. Only fields
// that require a session restart when changed are included:
//
// Included: command, prompt content hash, sorted env, work_dir, pre_start,
// session_setup, session_setup_script, session_live, overlay_dir, copy_files,
// nudge.
//
// Excluded: name, title, pool scaling, provider name, rig name —
// these don't affect session behavior.
//
// Returns the first 16 hex characters of the SHA-256. Same config always
// produces the same hash regardless of map iteration order.
func canonicalConfigHash(params TemplateParams, overlay map[string]string) string {
	h := sha256.New()

	// Command — may be overridden by overlay.
	command := params.Command
	if v, ok := overlay["command"]; ok && v != "" {
		command = v
	}
	h.Write([]byte(command)) //nolint:errcheck
	h.Write([]byte{0})       //nolint:errcheck

	// Prompt — strip the beacon prefix before hashing. resolveTemplate
	// prepends a time-stamped beacon line ("[city] agent • timestamp\n\n...").
	// The beacon changes every tick; hashing it would cause false drift.
	// Overlay prompts don't have beacons, so no stripping needed.
	prompt := params.Prompt
	if v, ok := overlay["prompt"]; ok {
		prompt = v
	} else {
		prompt = stripBeaconPrefix(prompt)
	}
	h.Write([]byte(prompt)) //nolint:errcheck
	h.Write([]byte{0})      //nolint:errcheck

	// Environment — merge params.Env with overlay env entries (overlay.env.KEY).
	env := make(map[string]string, len(params.Env))
	for k, v := range params.Env {
		env[k] = v
	}
	for k, v := range overlay {
		if len(k) > 4 && k[:4] == "env." {
			env[k[4:]] = v
		}
	}
	hashSortedStringMap(h, env)

	// WorkDir.
	workDir := params.WorkDir
	if v, ok := overlay["work_dir"]; ok && v != "" {
		workDir = v
	}
	h.Write([]byte(workDir)) //nolint:errcheck
	h.Write([]byte{0})       //nolint:errcheck

	// Nudge.
	h.Write([]byte(params.Hints.Nudge)) //nolint:errcheck
	h.Write([]byte{0})                  //nolint:errcheck

	// PreStart.
	for _, ps := range params.Hints.PreStart {
		h.Write([]byte(ps)) //nolint:errcheck
		h.Write([]byte{0})  //nolint:errcheck
	}
	h.Write([]byte{1}) //nolint:errcheck

	// SessionSetup.
	for _, ss := range params.Hints.SessionSetup {
		h.Write([]byte(ss)) //nolint:errcheck
		h.Write([]byte{0})  //nolint:errcheck
	}
	h.Write([]byte{1}) //nolint:errcheck

	// SessionSetupScript.
	h.Write([]byte(params.Hints.SessionSetupScript)) //nolint:errcheck
	h.Write([]byte{0})                               //nolint:errcheck

	// SessionLive.
	for _, sl := range params.Hints.SessionLive {
		h.Write([]byte(sl)) //nolint:errcheck
		h.Write([]byte{0})  //nolint:errcheck
	}
	h.Write([]byte{1}) //nolint:errcheck

	// OverlayDir.
	h.Write([]byte(params.Hints.OverlayDir)) //nolint:errcheck
	h.Write([]byte{0})                       //nolint:errcheck

	// CopyFiles. Mirrors runtime.hashCoreFields only in excluding probed
	// workDir entries marked SkipFingerprint, so this hash stays stable if
	// that dormant path is ever wired into drift detection (#682). Unlike
	// runtime hashing, this canonical hash still fingerprints CopyFiles by
	// Src and RelDst here; it does not use ContentHash or a sentinel.
	for _, cf := range params.Hints.CopyFiles {
		if cf.Probed && cf.SkipFingerprint {
			continue
		}
		h.Write([]byte(cf.Src))    //nolint:errcheck
		h.Write([]byte{0})         //nolint:errcheck
		h.Write([]byte(cf.RelDst)) //nolint:errcheck
		h.Write([]byte{0})         //nolint:errcheck
	}

	// FPExtra (pool config, etc.).
	if len(params.FPExtra) > 0 {
		h.Write([]byte("fp")) //nolint:errcheck
		h.Write([]byte{0})    //nolint:errcheck
		hashSortedStringMap(h, params.FPExtra)
	}

	sum := fmt.Sprintf("%x", h.Sum(nil))
	if len(sum) > 16 {
		return sum[:16]
	}
	return sum
}

// stripBeaconPrefix removes the time-stamped beacon line from a prompt.
// The beacon format is "[city] agent • timestamp\n\n<prompt body>".
// Only strips when the first line matches the beacon pattern (contains "•").
// If no beacon is detected, the prompt is returned unchanged.
func stripBeaconPrefix(prompt string) string {
	if !strings.HasPrefix(prompt, "[") {
		return prompt
	}
	idx := strings.Index(prompt, "\n\n")
	if idx < 0 {
		return prompt
	}
	// Only strip if the prefix looks like a beacon (contains bullet separator).
	if !strings.Contains(prompt[:idx], "•") {
		return prompt
	}
	return prompt[idx+2:]
}

// hashSortedStringMap writes map entries to h in deterministic sorted order.
func hashSortedStringMap(h interface{ Write([]byte) (int, error) }, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))    //nolint:errcheck
		h.Write([]byte{'='})  //nolint:errcheck
		h.Write([]byte(m[k])) //nolint:errcheck
		h.Write([]byte{0})    //nolint:errcheck
	}
}
