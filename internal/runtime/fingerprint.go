package runtime

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"sort"
)

// ConfigFingerprint returns a deterministic hash of the Config fields that
// define an agent's behavioral identity. Changes to these fields indicate
// the agent should be restarted (via drain when drain ops are available).
//
// Included: Command, Env, FingerprintExtra (pool config, etc.),
// Nudge, PreStart, SessionSetup, SessionSetupScript, OverlayDir, CopyFiles,
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
//	  GC_CITY*        — city identity and location
//	  GC_RIG*         — which rig the agent operates on
//	  GC_TEMPLATE     — agent template identity
//	  GC_ALIAS        — agent display identity
//	  GC_DOLT_PORT    — how to reach dolt (ephemeral port)
//	  GC_SKILLS_DIR   — skill discovery path
//	  GC_BLESSED_BIN_DIR — trusted binary path
//	  GC_PUBLICATION_* — service publication config
//
//	Excluded (runtime/transport, changes don't require restart):
//	  GC_SESSION_*    — per-session identity
//	  GC_AGENT        — pool instance name
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
	"GC_CITY_ROOT": true,
	"GC_CITY_PATH": true,

	// Rig scope
	"GC_RIG":      true,
	"GC_RIG_ROOT": true,
	"BEADS_DIR":   true,

	// Agent identity
	"GC_TEMPLATE": true,
	"GC_ALIAS":    true,

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

	// FingerprintExtra carries additional identity fields (pool config, etc.)
	// that aren't part of the session command but should
	// trigger a restart on change. Prefixed with "fp:" to avoid collisions
	// with Env keys.
	if len(cfg.FingerprintExtra) > 0 {
		h.Write([]byte("fp")) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		hashSortedMap(h, cfg.FingerprintExtra)
	}

	// Nudge
	h.Write([]byte(cfg.Nudge)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})         //nolint:errcheck // hash.Write never errors

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

	// CopyFiles
	for _, cf := range cfg.CopyFiles {
		h.Write([]byte(cf.Src))    //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})         //nolint:errcheck // separator between Src and RelDst
		h.Write([]byte(cf.RelDst)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})         //nolint:errcheck // separator between entries
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
