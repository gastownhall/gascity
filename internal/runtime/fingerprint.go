package runtime

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
)

// ConfigFingerprint returns a deterministic hash of the Config fields that
// define an agent's behavioral identity. Changes to these fields indicate
// the agent should be restarted (via drain when drain ops are available).
//
// Included: Command, Env, FingerprintExtra (pool config, etc.),
// PreStart, SessionSetup, SessionSetupScript, OverlayDir, CopyFiles,
// SessionLive.
//
// Excluded (observation-only hints): WorkDir, ReadyPromptPrefix,
// ReadyDelayMs, ProcessNames, EmitsPermissionWarning.
//
// The hash is a hex-encoded SHA-256. Same config always produces the same
// hash regardless of map iteration order.
func ConfigFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFields(h, cfg)
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// CoreFingerprint returns a hash of only the "core" config fields —
// everything except SessionLive. A change to core fields triggers a
// drain + restart. A change to only SessionLive triggers re-apply
// without restart.
func CoreFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFields(h, cfg)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// LiveFingerprint returns a hash of only the SessionLive fields.
// Used by the reconciler to detect live-only drift.
func LiveFingerprint(cfg Config) string {
	h := sha256.New()
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// envFingerprintAllow is the set of env keys whose values define agent
// behavioral identity. Only these keys contribute to the config fingerprint.
//
// Allow-list rationale: the agent env contains ~50 GC_* vars from k8s
// service discovery, runtime identity, supervisor plumbing, etc. A deny
// list is fragile — any new var that leaks in causes spurious config-drift
// restarts (and token burn from wake/drain loops). An allow list is safe
// by default: new vars are ignored unless explicitly opted in.
//
// Categories:
//
//	Behavioral (restart needed if changed):
//	  BEADS_DIR       — where the agent finds work
//	  GC_CITY / GC_CITY_PATH — city identity and location
//	  GC_RIG*         — which rig the agent operates on
//	  GC_TEMPLATE     — agent template identity
//	  GC_DOLT_PORT    — how to reach dolt (ephemeral port)
//	  GC_SKILLS_DIR   — skill discovery path
//	  GC_BLESSED_BIN_DIR — trusted binary path
//	  GC_PUBLICATION_* — service publication config
//
//	Excluded (runtime/transport, changes don't require restart):
//	  GC_SESSION_*    — per-session identity
//	  GC_AGENT        — pool instance name
//	  GC_ALIAS        — public routing/display alias, synced live where possible
//	  GC_INSTANCE_TOKEN — restart nonce
//	  GC_*_EPOCH      — restart counters
//	  GC_HOME/GC_DIR  — derived paths
//	  GC_BIN          — gc binary path (agent doesn't call gc)
//	  GC_API_*        — supervisor bind address
//	  GC_CTRL_*       — k8s service discovery injection
//	  GC_PUBLICATIONS_FILE — file path, not behavioral
var envFingerprintAllow = map[string]bool{
	// City identity
	"GC_CITY":      true,
	"GC_CITY_PATH": true,

	// Rig scope
	"GC_RIG":      true,
	"GC_RIG_ROOT": true,
	"BEADS_DIR":   true,

	// Agent identity
	"GC_TEMPLATE": true,

	// Service connectivity — GC_DOLT_PORT intentionally excluded.
	// The dolt port is ephemeral (changes on every supervisor restart)
	// and including it causes spurious config-drift drains on every
	// restart. The agent reconnects to the new port automatically.

	// Tool/binary discovery
	"GC_SKILLS_DIR":      true,
	"GC_BLESSED_BIN_DIR": true,

	// Publication config
	"GC_PUBLICATION_PROVIDER":           true,
	"GC_PUBLICATION_PUBLIC_BASE_DOMAIN": true,
	"GC_PUBLICATION_PUBLIC_BASE_URL":    true,
	"GC_PUBLICATION_TENANT_BASE_DOMAIN": true,
	"GC_PUBLICATION_TENANT_BASE_URL":    true,
	"GC_PUBLICATION_TENANT_SLUG":        true,
}

// envFingerprintInclude returns true if the key should contribute to the
// config fingerprint. Uses an allow list — only explicitly listed keys
// are included.
func envFingerprintInclude(key string) bool {
	return envFingerprintAllow[key]
}

// hashCoreFields writes all config fields except SessionLive to the hash.
func hashCoreFields(h hash.Hash, cfg Config) {
	h.Write([]byte(cfg.Command)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})           //nolint:errcheck // hash.Write never errors

	hashSortedMapIncluded(h, cfg.Env, envFingerprintInclude)
	hashMCPServers(h, cfg.MCPServers)

	// FingerprintExtra carries additional identity fields (pool config, etc.)
	// that aren't part of the session command but should
	// trigger a restart on change. Prefixed with "fp:" to avoid collisions
	// with Env keys.
	if len(cfg.FingerprintExtra) > 0 {
		h.Write([]byte("fp")) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		hashSortedMap(h, cfg.FingerprintExtra)
	}

	// PreStart
	for _, ps := range cfg.PreStart {
		h.Write([]byte(ps)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel between slices

	// SessionSetup
	for _, ss := range cfg.SessionSetup {
		h.Write([]byte(ss)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel between slices

	h.Write([]byte(cfg.SessionSetupScript)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})                      //nolint:errcheck // hash.Write never errors

	h.Write([]byte(cfg.OverlayDir)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})              //nolint:errcheck // hash.Write never errors

	// CopyFiles — probed entries use ContentHash (stable when content
	// unchanged, even if files are recreated). Config-derived entries
	// use Src/RelDst paths. When a probed entry has an empty ContentHash
	// (transient I/O error), a stable sentinel is used instead of falling
	// back to path-based hashing, which would flip fingerprint modes.
	for _, cf := range cfg.CopyFiles {
		if cf.Probed {
			h.Write([]byte(cf.RelDst)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})         //nolint:errcheck // hash.Write never errors
			if cf.ContentHash != "" {
				h.Write([]byte(cf.ContentHash)) //nolint:errcheck // hash.Write never errors
			} else {
				h.Write([]byte("HASH_UNAVAILABLE")) //nolint:errcheck // stable sentinel for failed hash
			}
			h.Write([]byte{0}) //nolint:errcheck // hash.Write never errors
		} else {
			h.Write([]byte(cf.Src))    //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})         //nolint:errcheck // separator between Src and RelDst
			h.Write([]byte(cf.RelDst)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})         //nolint:errcheck // separator between entries
		}
	}
}

// hashLiveFields writes SessionLive fields to the hash.
func hashLiveFields(h hash.Hash, cfg Config) {
	for _, sl := range cfg.SessionLive {
		h.Write([]byte(sl)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel
}

// hashSortedMapIncluded writes map entries to h in deterministic sorted-key
// order, only including keys for which the include function returns true.
func hashSortedMapIncluded(h hash.Hash, m map[string]string, include func(string) bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if include(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))    //nolint:errcheck // hash.Write never errors
		h.Write([]byte{'='})  //nolint:errcheck // hash.Write never errors
		h.Write([]byte(m[k])) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}
}

// hashSortedMap writes map entries to h in deterministic sorted-key order.
func hashSortedMap(h hash.Hash, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))    //nolint:errcheck // hash.Write never errors
		h.Write([]byte{'='})  //nolint:errcheck // hash.Write never errors
		h.Write([]byte(m[k])) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}
}

func hashMCPServers(h hash.Hash, servers []MCPServerConfig) {
	for _, server := range NormalizeMCPServerConfigs(servers) {
		h.Write([]byte(server.Name))      //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                //nolint:errcheck // hash.Write never errors
		h.Write([]byte(server.Transport)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                //nolint:errcheck // hash.Write never errors
		h.Write([]byte(server.Command))   //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                //nolint:errcheck // hash.Write never errors
		for _, arg := range server.Args {
			h.Write([]byte(arg)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})   //nolint:errcheck // hash.Write never errors
		}
		h.Write([]byte{1}) //nolint:errcheck // sentinel between args/env
		hashSortedMap(h, server.Env)
		h.Write([]byte{1})          //nolint:errcheck // sentinel between env/url
		h.Write([]byte(server.URL)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})          //nolint:errcheck // hash.Write never errors
		hashSortedMap(h, server.Headers)
		h.Write([]byte{2}) //nolint:errcheck // sentinel between servers
	}
}

// CoreFingerprintBreakdown returns per-field hash components of the core
// fingerprint. Used to diagnose config-drift by comparing breakdowns
// from session start vs reconcile time.
func CoreFingerprintBreakdown(cfg Config) map[string]string {
	fieldHash := func(fn func(h hash.Hash)) string {
		h := sha256.New()
		fn(h)
		return fmt.Sprintf("%x", h.Sum(nil))[:16]
	}
	return map[string]string{
		"Command": fieldHash(func(h hash.Hash) {
			h.Write([]byte(cfg.Command))
		}),
		"Env": fieldHash(func(h hash.Hash) {
			hashSortedMapIncluded(h, cfg.Env, envFingerprintInclude)
		}),
		"MCPServers": fieldHash(func(h hash.Hash) {
			hashMCPServers(h, cfg.MCPServers)
		}),
		"FPExtra": fieldHash(func(h hash.Hash) {
			if len(cfg.FingerprintExtra) > 0 {
				h.Write([]byte("fp"))
				h.Write([]byte{0})
				hashSortedMap(h, cfg.FingerprintExtra)
			}
		}),
		"PreStart": fieldHash(func(h hash.Hash) {
			for _, ps := range cfg.PreStart {
				h.Write([]byte(ps))
				h.Write([]byte{0})
			}
		}),
		"SessionSetup": fieldHash(func(h hash.Hash) {
			for _, ss := range cfg.SessionSetup {
				h.Write([]byte(ss))
				h.Write([]byte{0})
			}
		}),
		"SessionSetupScript": fieldHash(func(h hash.Hash) {
			h.Write([]byte(cfg.SessionSetupScript))
		}),
		"OverlayDir": fieldHash(func(h hash.Hash) {
			h.Write([]byte(cfg.OverlayDir))
		}),
		"CopyFiles": fieldHash(func(h hash.Hash) {
			for _, cf := range cfg.CopyFiles {
				if cf.Probed {
					h.Write([]byte(cf.RelDst))
					h.Write([]byte{0})
					if cf.ContentHash != "" {
						h.Write([]byte(cf.ContentHash))
					} else {
						h.Write([]byte("HASH_UNAVAILABLE"))
					}
					h.Write([]byte{0})
				} else {
					h.Write([]byte(cf.Src))
					h.Write([]byte{0})
					h.Write([]byte(cf.RelDst))
					h.Write([]byte{0})
				}
			}
		}),
	}
}

// CoreFingerprintDriftFields returns sorted core fingerprint field names whose
// current hashes differ from the stored per-field breakdown.
func CoreFingerprintDriftFields(storedBreakdown map[string]string, current Config) []string {
	if len(storedBreakdown) == 0 {
		return nil
	}
	return coreFingerprintDriftFields(storedBreakdown, CoreFingerprintBreakdown(current))
}

func coreFingerprintDriftFields(storedBreakdown, currentBreakdown map[string]string) []string {
	var diffs []string
	for field, ch := range currentBreakdown {
		sh := storedBreakdown[field]
		if sh != ch {
			diffs = append(diffs, field)
		}
	}
	sort.Strings(diffs)
	return diffs
}

// LogCoreFingerprintDrift writes diagnostic output when config-drift is
// detected, showing per-field hash breakdown and values for the current
// config. Compare against stored breakdown (from session start metadata)
// to identify which field changed.
func LogCoreFingerprintDrift(w io.Writer, name string, storedBreakdown map[string]string, current Config) {
	currentBreakdown := CoreFingerprintBreakdown(current)
	diffs := coreFingerprintDriftFields(storedBreakdown, currentBreakdown)
	if len(diffs) == 0 {
		// No stored breakdown available or all fields match — log full breakdown.
		if len(storedBreakdown) == 0 {
			fmt.Fprintf(w, "  config-drift-diag %s: no stored breakdown (pre-upgrade session); current field hashes: %v\n", name, currentBreakdown) //nolint:errcheck // best-effort diag
		} else {
			fmt.Fprintf(w, "  config-drift-diag %s: no per-field diff (possible sentinel/ordering issue)\n", name) //nolint:errcheck // best-effort diag
		}
		return
	}
	fmt.Fprintf(w, "  config-drift-diag %s: drifted fields: %s\n", name, strings.Join(diffs, ", ")) //nolint:errcheck // best-effort diag
	for _, field := range diffs {
		fmt.Fprintf(w, "    %s: stored-hash=%s current-hash=%s\n", field, storedBreakdown[field], currentBreakdown[field]) //nolint:errcheck // best-effort diag
		switch field {
		case "Command":
			fmt.Fprintf(w, "    Command: %q\n", current.Command) //nolint:errcheck // best-effort diag
		case "Env":
			fmt.Fprintf(w, "    Env: %v\n", filteredEnv(current.Env)) //nolint:errcheck // best-effort diag
		case "MCPServers":
			fmt.Fprintf(w, "    MCPServers: %+v\n", NormalizeMCPServerConfigs(current.MCPServers)) //nolint:errcheck // best-effort diag
		case "FPExtra":
			fmt.Fprintf(w, "    FPExtra: %v (len=%d)\n", current.FingerprintExtra, len(current.FingerprintExtra)) //nolint:errcheck // best-effort diag
		case "PreStart":
			fmt.Fprintf(w, "    PreStart: %v\n", current.PreStart) //nolint:errcheck // best-effort diag
		case "OverlayDir":
			fmt.Fprintf(w, "    OverlayDir: %q\n", current.OverlayDir) //nolint:errcheck // best-effort diag
		case "SessionSetup":
			fmt.Fprintf(w, "    SessionSetup: %v\n", current.SessionSetup) //nolint:errcheck // best-effort diag
		case "SessionSetupScript":
			fmt.Fprintf(w, "    SessionSetupScript len: %d\n", len(current.SessionSetupScript)) //nolint:errcheck // best-effort diag
		case "CopyFiles":
			for i, cf := range current.CopyFiles {
				fmt.Fprintf(w, "    CopyFiles[%d]: RelDst=%q ContentHash=%q\n", i, cf.RelDst, cf.ContentHash) //nolint:errcheck // best-effort diag
			}
		}
	}
}

// filteredEnv returns only the allow-listed env keys for diagnostic output.
func filteredEnv(env map[string]string) map[string]string {
	out := make(map[string]string)
	for k, v := range env {
		if envFingerprintInclude(k) {
			out[k] = v
		}
	}
	return out
}
