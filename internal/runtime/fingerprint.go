package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
)

// BreakdownV1 is the typed shape of `core_hash_breakdown` stored in
// session metadata at start time and read back by the reconciler when
// drift is detected. Versioned alongside the fingerprint hash so the
// reconciler can distinguish a current-format breakdown from a legacy
// `map[string]string` payload written by older binaries.
type BreakdownV1 struct {
	Version   string               `json:"version"`
	Fields    map[string]string    `json:"fields"`
	CopyFiles []BreakdownCopyEntry `json:"copy_files,omitempty"`
}

// BreakdownCopyEntry is the per-entry record for the CopyFiles slice in
// BreakdownV1. Mirrors runtime.CopyEntry's fingerprint-relevant fields
// so the reconciler can render a per-entry diff at drift time.
type BreakdownCopyEntry struct {
	RelDst      string `json:"rel_dst"`
	Src         string `json:"src,omitempty"`
	Probed      bool   `json:"probed"`
	ContentHash string `json:"content_hash,omitempty"`
}

// FingerprintVersion namespaces the stored fingerprint hashes so the
// reconciler can detect hashes written by older binaries (no prefix or a
// different prefix) and silently rebaseline them instead of triggering a
// false-positive drain. Bump this constant whenever the inputs to or the
// algorithm of any Fingerprint helper change.
//
// v4: .gc/settings.json is no longer probed in CopyFiles; its fingerprint
// contribution is path-based only. Content changes to the managed runtime
// settings file no longer trigger stale-session cascades. (ga-zfm)
//
// v5: operational/host-tooling scripts (city-*.sh, update-*.sh) are excluded
// from the .gc/scripts probed CopyFiles content hash. Editing such a script no
// longer flips every agent's fingerprint into a fleet-wide config-drift drain.
// The bump rebaselines existing v4 hashes silently instead of draining the
// fleet once on rollout. (#3840)
//
// v6: operator-authored config env (workspace/provider/agent/rig-patch env,
// captured before passthrough/credentials/generated values are merged) gets
// its own dedicated Launch-tier identity field (Config.OperatorEnv), separate
// from Env and FingerprintExtra (Option A', ga-3a42sp). The bump rebaselines
// existing v5 hashes silently rather than draining the fleet. (ga-i91hrn)
//
// v7: FingerprintExtra hashes only its Restart-policy keys
// (FingerprintExtraChangePolicy), so a pool sizing or skill edit no longer
// drains the sessions it reaches. A v6 hash is an artifact only for config
// unchanged since (IsCoreHashArtifact), so a session whose config changed
// under v6 still drains once; every other version is rebaselined silently.
const FingerprintVersion = "v7"

// ChangePolicy is what a change to one launch input does to a running
// session. Only a Restart input is hashed into the fingerprints that decide
// a drain or restart; the other policies keep a change out of them.
type ChangePolicy int

const (
	// ChangeRestart drains or restarts the running session onto the change.
	// The default for any input not declared otherwise.
	ChangeRestart ChangePolicy = iota
	// ChangeNextStart takes effect when the session next starts naturally;
	// it never drains a session.
	ChangeNextStart
	// ChangeIgnore is an allocation input, not session identity.
	ChangeIgnore
)

// FingerprintExtraChangePolicy returns the declared policy of a
// FingerprintExtra key. Pool sizing only changes demand; skills are
// materialized at each start, so an edit reaches the next one; wake_mode
// only decides how the next wake resumes. Every other key (mcp:*,
// depends_on, and any new key) is Restart.
func FingerprintExtraChangePolicy(key string) ChangePolicy {
	switch {
	case key == "pool.min" || key == "pool.max":
		return ChangeIgnore
	case strings.HasPrefix(key, "skills:") || key == "wake_mode":
		return ChangeNextStart
	default:
		return ChangeRestart
	}
}

// restartFingerprintExtra is the Restart-policy subset of extra, the only
// part any fingerprint hashes; nil when no key is Restart, so an extra of
// only Ignore and NextStart keys hashes as no extra at all.
func restartFingerprintExtra(extra map[string]string) map[string]string {
	var out map[string]string
	for k, v := range extra {
		if FingerprintExtraChangePolicy(k) != ChangeRestart {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(extra))
		}
		out[k] = v
	}
	return out
}

// ConfigFingerprint returns a deterministic hash of the Config fields that
// define an agent's behavioral identity. Changes to these fields indicate
// the agent should be restarted (via drain when drain ops are available).
//
// Included: Command, Lifecycle, Env, OperatorEnv (config-authored env
// identity), FingerprintExtra (Restart-policy keys only), PreStart, SessionSetup,
// SessionSetupScript, OverlayDir, effective provider overlay slots, CopyFiles,
// AcceptStartupDialogs, MouseOn, SessionLive.
//
// Excluded (observation-only hints): WorkDir, ReadyPromptPrefix,
// ReadyDelayMs, ProcessNames, EmitsPermissionWarning.
//
// The hash is a hex-encoded SHA-256 prefixed with FingerprintVersion. Same
// config always produces the same hash regardless of map iteration order.
func ConfigFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFields(h, cfg)
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%s:%x", FingerprintVersion, h.Sum(nil))
}

// CoreFingerprint returns a hash of only the "core" config fields —
// everything except SessionLive. A change to core fields triggers a
// drain + restart. A change to only SessionLive triggers re-apply
// without restart. Output is prefixed with FingerprintVersion.
func CoreFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFields(h, cfg)
	return fmt.Sprintf("%s:%x", FingerprintVersion, h.Sum(nil))
}

// prePolicyFingerprintVersion is the version before the FingerprintExtra
// change policy. Its hashes are the one version whose config the reconciler
// still checks, so that a session whose Restart inputs changed under v6 drains
// as it would have there; deleting early only turns such a drain into the
// silent rebaseline every other version gets.
//
// Removal condition: delete it, the PrePolicy* helpers, IsPrePolicyHash and
// the *HashMatches v6 arms once no session row in any city stores a v6: value
// in started_config_hash, started_provision_hash, started_launch_hash or
// started_live_hash, and no supported rollback target predates v7.
const prePolicyFingerprintVersion = "v6"

// PrePolicyCoreFingerprint is CoreFingerprint as the v6 binary computed it,
// every FingerprintExtra key hashed: a stored hash equal to it is a session
// started before the change policy whose config has not changed since.
func PrePolicyCoreFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFieldsExtra(h, cfg, cfg.FingerprintExtra)
	return fmt.Sprintf("%s:%x", prePolicyFingerprintVersion, h.Sum(nil))
}

// PrePolicyProvisionFingerprint is ProvisionFingerprint as the v6 binary
// computed it, every FingerprintExtra key hashed.
func PrePolicyProvisionFingerprint(cfg Config) string {
	h := sha256.New()
	hashProvisionFieldsExtra(h, cfg, cfg.FingerprintExtra)
	return fmt.Sprintf("%s:%x", prePolicyFingerprintVersion, h.Sum(nil))
}

// PrePolicyLaunchFingerprint is LaunchFingerprint as the v6 binary computed
// it: the same digest, which never hashed FingerprintExtra.
func PrePolicyLaunchFingerprint(cfg Config) string {
	h := sha256.New()
	hashLaunchFields(h, cfg)
	return fmt.Sprintf("%s:%x", prePolicyFingerprintVersion, h.Sum(nil))
}

// PrePolicyLiveFingerprint is LiveFingerprint as the v6 binary computed it:
// the same digest, which never hashed FingerprintExtra.
func PrePolicyLiveFingerprint(cfg Config) string {
	h := sha256.New()
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%s:%x", prePolicyFingerprintVersion, h.Sum(nil))
}

// IsPrePolicyHash reports whether a stored hash was computed by the v6
// binary, before the FingerprintExtra change policy.
func IsPrePolicyHash(stored string) bool {
	return strings.HasPrefix(stored, prePolicyFingerprintVersion+":")
}

// IsCoreHashArtifact reports whether a stored core hash that differs from
// cfg's CoreFingerprint differs only in how it was computed, not in the
// config: cfg's v6 hash, or any other legacy or version-mismatched hash. A v6
// hash of other config is a drift, as it was under v6. The reconciler
// silently rebaselines an artifact instead of draining.
func IsCoreHashArtifact(stored string, cfg Config) bool {
	if IsPrePolicyHash(stored) {
		return stored == PrePolicyCoreFingerprint(cfg)
	}
	return IsLegacyOrMismatchedVersion(stored)
}

// IsLiveHashArtifact is IsCoreHashArtifact for a stored live hash: cfg's v6
// live hash, or any other legacy or version-mismatched hash. A v6 hash of
// another session_live is a live drift, applied without a restart.
func IsLiveHashArtifact(stored string, cfg Config) bool {
	if IsPrePolicyHash(stored) {
		return stored == PrePolicyLiveFingerprint(cfg)
	}
	return IsLegacyOrMismatchedVersion(stored)
}

// ProvisionHashMatches reports whether a stored provision hash is cfg's, as
// this binary or the v6 one computed it: the box needs no rebuild.
func ProvisionHashMatches(stored string, cfg Config) bool {
	return stored == ProvisionFingerprint(cfg) || stored == PrePolicyProvisionFingerprint(cfg)
}

// LaunchHashMatches reports whether a stored launch hash is cfg's, as this
// binary or the v6 one computed it: the agent needs no relaunch.
func LaunchHashMatches(stored string, cfg Config) bool {
	return stored == LaunchFingerprint(cfg) || stored == PrePolicyLaunchFingerprint(cfg)
}

// LiveFingerprint returns a hash of only the SessionLive fields.
// Used by the reconciler to detect live-only drift. Output is prefixed
// with FingerprintVersion.
func LiveFingerprint(cfg Config) string {
	h := sha256.New()
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%s:%x", FingerprintVersion, h.Sum(nil))
}

// IsLegacyOrMismatchedVersion reports whether the stored hash should
// trigger a silent rebaseline rather than a drift drain. Returns true for
// stored hashes with no version prefix (legacy bare hex) or a version
// prefix that does not match FingerprintVersion. Returns false for empty
// stored hashes (those are gated separately by the reconciler) and for
// stored hashes that share the current version prefix.
func IsLegacyOrMismatchedVersion(stored string) bool {
	if stored == "" {
		return false
	}
	colon := strings.IndexByte(stored, ':')
	if colon < 0 {
		// No colon: a bare hex hash from a pre-versioning binary, or an
		// otherwise malformed value. Either way, treat as legacy.
		return true
	}
	return stored[:colon] != FingerprintVersion
}

// IsVersionMismatchedHash reports whether the stored hash carries a
// well-formed `v<digits>:` prefix that does not match FingerprintVersion.
// Used to distinguish "another binary's version" from "no version prefix
// at all" for trace-outcome reporting. Returns false for empty stored,
// current-version stored, unversioned stored, and stored with a
// non-`v<digits>:` prefix shape.
func IsVersionMismatchedHash(stored string) bool {
	colon := strings.IndexByte(stored, ':')
	if colon < 1 {
		return false
	}
	prefix := stored[:colon]
	if prefix == FingerprintVersion {
		return false
	}
	if prefix[0] != 'v' {
		return false
	}
	for i := 1; i < len(prefix); i++ {
		if prefix[i] < '0' || prefix[i] > '9' {
			return false
		}
	}
	return true
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

	// Service connectivity — GC_DOLT_PORT intentionally excluded. The
	// managed port is ephemeral (changes on every supervisor restart) and
	// including it causes spurious config-drift drains on every restart.
	// gc withholds it for gc-managed mirrored scopes, whose raw bd follows
	// .beads/dolt-server.port. Sessions spawned before that, or while the
	// mirror disagreed with the published state, keep a port a restart can
	// move; they recover on respawn, not through this fingerprint.

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
	hashCoreFieldsExtra(h, cfg, restartFingerprintExtra(cfg.FingerprintExtra))
}

// hashCoreFieldsExtra is hashCoreFields with FingerprintExtra replaced by
// extra.
func hashCoreFieldsExtra(h hash.Hash, cfg Config, extra map[string]string) {
	h.Write([]byte(cfg.Command)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})           //nolint:errcheck // hash.Write never errors

	h.Write([]byte(cfg.Lifecycle)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})             //nolint:errcheck // hash.Write never errors

	hashSortedMapIncluded(h, cfg.Env, envFingerprintInclude)
	hashMCPServers(h, cfg.MCPServers)

	// FingerprintExtra carries additional fields that aren't part of the
	// session command; only its Restart-policy keys are identity. Prefixed
	// with "fp:" to avoid collisions with Env keys.
	hashFingerprintExtra(h, extra)

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

	hashOverlayProviders(h, OverlayProviderNames(cfg))
	hashOptionalBool(h, "accept_startup_dialogs", cfg.AcceptStartupDialogs)
	hashBool(h, "mouse_on", cfg.MouseOn)

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

	// Upstream (Phase C — the model-serving selection identity). LAUNCH-half:
	// also hashed by hashLaunchFields, so switching upstream relaunches the agent
	// in the warm box (B2.3) rather than reprovisioning. The resolved serving env
	// (ANTHROPIC_*) lives in Env and is NOT hashed (the allow-list excludes it),
	// so a credential rotation moves no fingerprint. Optional/conditional, so an
	// unset Upstream leaves every existing config's fingerprint byte-identical.
	hashOptionalString(h, "upstream", cfg.Upstream)

	// OperatorEnv (Option A', ga-3a42sp — config-authored env identity).
	// LAUNCH-half: also hashed by hashLaunchFields, so a config-authored env
	// change relaunches the agent in the warm box rather than reprovisioning.
	// Prefixed with "operator_env" so its framing cannot collide with the
	// FingerprintExtra map hashed above, even when both carry the same
	// key/value pairs. Conditional, so an empty/nil OperatorEnv leaves every
	// existing config's fingerprint byte-identical.
	if len(cfg.OperatorEnv) > 0 {
		h.Write([]byte("operator_env")) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})              //nolint:errcheck // hash.Write never errors
		hashSortedMap(h, cfg.OperatorEnv)
	}
}

// hashFingerprintExtra writes extra to h, framed by "fp"; nothing when it is
// empty.
func hashFingerprintExtra(h hash.Hash, extra map[string]string) {
	if len(extra) == 0 {
		return
	}
	h.Write([]byte("fp")) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	hashSortedMap(h, extra)
}

// hashOptionalString contributes name+value to the hash only when value is
// non-empty, so adding a new optional string field leaves the fingerprint of
// every config that does not set it byte-identical (no FingerprintVersion bump).
func hashOptionalString(h hash.Hash, name, value string) {
	if value == "" {
		return
	}
	h.Write([]byte(name))  //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})     //nolint:errcheck // hash.Write never errors
	h.Write([]byte(value)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})     //nolint:errcheck // hash.Write never errors
}

func hashOptionalBool(h hash.Hash, name string, value *bool) {
	if value == nil {
		return
	}
	h.Write([]byte(name)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	if *value {
		h.Write([]byte("true")) //nolint:errcheck // hash.Write never errors
	} else {
		h.Write([]byte("false")) //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{0}) //nolint:errcheck // hash.Write never errors
}

func hashBool(h hash.Hash, name string, value bool) {
	h.Write([]byte(name)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	if value {
		h.Write([]byte("true")) //nolint:errcheck // hash.Write never errors
	} else {
		h.Write([]byte("false")) //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{0}) //nolint:errcheck // hash.Write never errors
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

func hashOverlayProviders(h hash.Hash, providers []string) {
	HashOverlayProviderNames(h, providers)
}

// HashOverlayProviderNames writes the overlay-provider fingerprint component
// using the same framing as CoreFingerprint.
func HashOverlayProviderNames(h io.Writer, providers []string) {
	if len(providers) == 0 {
		return
	}
	h.Write([]byte("overlay-providers")) //nolint:errcheck
	h.Write([]byte{0})                   //nolint:errcheck
	for _, provider := range providers {
		h.Write([]byte(provider)) //nolint:errcheck
		h.Write([]byte{0})        //nolint:errcheck
	}
	h.Write([]byte{1}) //nolint:errcheck
}

// CoreFingerprintBreakdown returns per-field hash components of the core
// fingerprint plus a typed view of the CopyFiles slice. Used to diagnose
// config-drift by comparing breakdowns from session start vs reconcile
// time. The Version field carries the current FingerprintVersion so the
// reconciler can detect a legacy `map[string]string` payload (no Version)
// and fall back to the prior diff renderer.
func CoreFingerprintBreakdown(cfg Config) BreakdownV1 {
	fieldHash := func(fn func(h hash.Hash)) string {
		h := sha256.New()
		fn(h)
		return fmt.Sprintf("%x", h.Sum(nil))[:16]
	}
	fields := map[string]string{
		"Command": fieldHash(func(h hash.Hash) {
			h.Write([]byte(cfg.Command))
		}),
		"Lifecycle": fieldHash(func(h hash.Hash) {
			h.Write([]byte(cfg.Lifecycle))
		}),
		"Env": fieldHash(func(h hash.Hash) {
			hashSortedMapIncluded(h, cfg.Env, envFingerprintInclude)
		}),
		"MCPServers": fieldHash(func(h hash.Hash) {
			hashMCPServers(h, cfg.MCPServers)
		}),
		// Every key, whatever its policy: the breakdown only diagnoses a
		// drift the core hash found, and stored breakdowns stay comparable.
		"FPExtra": fieldHash(func(h hash.Hash) {
			hashFingerprintExtra(h, cfg.FingerprintExtra)
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
		"OverlayProviders": fieldHash(func(h hash.Hash) {
			hashOverlayProviders(h, OverlayProviderNames(cfg))
		}),
		"AcceptStartupDialogs": fieldHash(func(h hash.Hash) {
			hashOptionalBool(h, "accept_startup_dialogs", cfg.AcceptStartupDialogs)
		}),
		"MouseOn": fieldHash(func(h hash.Hash) {
			hashBool(h, "mouse_on", cfg.MouseOn)
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
	var copyEntries []BreakdownCopyEntry
	if len(cfg.CopyFiles) > 0 {
		copyEntries = make([]BreakdownCopyEntry, 0, len(cfg.CopyFiles))
		for _, cf := range cfg.CopyFiles {
			copyEntries = append(copyEntries, BreakdownCopyEntry{
				RelDst:      cf.RelDst,
				Src:         cf.Src,
				Probed:      cf.Probed,
				ContentHash: cf.ContentHash,
			})
		}
	}
	return BreakdownV1{
		Version:   FingerprintVersion,
		Fields:    fields,
		CopyFiles: copyEntries,
	}
}

// CoreFingerprintDriftFields returns sorted core fingerprint field names whose
// current hashes differ from the stored per-field breakdown.
func CoreFingerprintDriftFields(storedBreakdown BreakdownV1, current Config) []string {
	if len(storedBreakdown.Fields) == 0 {
		return nil
	}
	return diffBreakdownFields(storedBreakdown.Fields, CoreFingerprintBreakdown(current).Fields)
}

// CoreFingerprintDriftFieldsFromJSON returns sorted drifted field names from a
// JSON-encoded stored breakdown. It accepts both current BreakdownV1 payloads
// and legacy map[string]string payloads.
func CoreFingerprintDriftFieldsFromJSON(storedJSON string, current Config) []string {
	storedFields, _, _ := parseStoredBreakdown(storedJSON)
	if len(storedFields) == 0 {
		return nil
	}
	return diffBreakdownFields(storedFields, CoreFingerprintBreakdown(current).Fields)
}

// LogCoreFingerprintDrift writes diagnostic output when config-drift is
// detected. The stored breakdown is supplied as a JSON-encoded string;
// the renderer first attempts to decode it as a current-format
// BreakdownV1, then falls back to a legacy map[string]string payload
// when the decoded Version is empty. Both paths emit the same
// per-field "drifted fields" / "stored-hash=/current-hash=" header. The
// new path additionally renders a per-entry CopyFiles diff with
// `[ ]`/`[~]`/`[+]`/`[-]` markers; the legacy path keeps the prior
// `CopyFiles[N]:` per-entry format for byte-for-byte upgrade compat.
func LogCoreFingerprintDrift(w io.Writer, name string, storedJSON string, current Config) {
	currentBd := CoreFingerprintBreakdown(current)

	storedFields, storedCopy, isV1 := parseStoredBreakdown(storedJSON)

	diffs := diffBreakdownFields(storedFields, currentBd.Fields)

	if len(diffs) == 0 {
		if len(storedFields) == 0 {
			fmt.Fprintf(w, "  config-drift-diag %s: no stored breakdown (pre-upgrade session); current field hashes: %v\n", name, currentBd.Fields) //nolint:errcheck // best-effort diag
		} else {
			fmt.Fprintf(w, "  config-drift-diag %s: no per-field diff (possible sentinel/ordering issue)\n", name) //nolint:errcheck // best-effort diag
		}
		return
	}

	fmt.Fprintf(w, "  config-drift-diag %s: drifted fields: %s\n", name, strings.Join(diffs, ", ")) //nolint:errcheck // best-effort diag
	for _, field := range diffs {
		fmt.Fprintf(w, "    %s: stored-hash=%s current-hash=%s\n", field, storedFields[field], currentBd.Fields[field]) //nolint:errcheck // best-effort diag
		switch field {
		case "Command":
			fmt.Fprintf(w, "    Command: %q\n", current.Command) //nolint:errcheck // best-effort diag
		case "Env":
			fmt.Fprintf(w, "    Env: %v\n", filteredEnv(current.Env)) //nolint:errcheck // best-effort diag
		case "MCPServers":
			fmt.Fprintf(w, "    MCPServers: %+v\n", NormalizeMCPServerConfigs(current.MCPServers)) //nolint:errcheck // best-effort diag
		case "FPExtra":
			extra := restartFingerprintExtra(current.FingerprintExtra)      // the keys that can drift; the breakdown hashes them all
			fmt.Fprintf(w, "    FPExtra: %v (len=%d)\n", extra, len(extra)) //nolint:errcheck // best-effort diag
			if storedBreakdownVersion(storedJSON) == prePolicyFingerprintVersion {
				// A v6 baseline hashed every key, so a pool or skill edit since
				// its start is part of the drift it drains once for.
				prior := nonRestartFingerprintExtra(current.FingerprintExtra)
				fmt.Fprintf(w, "    FPExtra (v6 baseline, drains once): %v (len=%d)\n", prior, len(prior)) //nolint:errcheck // best-effort diag
			}
		case "PreStart":
			fmt.Fprintf(w, "    PreStart: %v\n", current.PreStart) //nolint:errcheck // best-effort diag
		case "OverlayDir":
			fmt.Fprintf(w, "    OverlayDir: %q\n", current.OverlayDir) //nolint:errcheck // best-effort diag
		case "OverlayProviders":
			fmt.Fprintf(w, "    OverlayProviders: %v\n", OverlayProviderNames(current)) //nolint:errcheck // best-effort diag
		case "SessionSetup":
			fmt.Fprintf(w, "    SessionSetup: %v\n", current.SessionSetup) //nolint:errcheck // best-effort diag
		case "SessionSetupScript":
			fmt.Fprintf(w, "    SessionSetupScript len: %d\n", len(current.SessionSetupScript)) //nolint:errcheck // best-effort diag
		case "CopyFiles":
			if isV1 {
				logCopyFilesEntryDiff(w, storedCopy, currentBd.CopyFiles)
			} else {
				for i, cf := range current.CopyFiles {
					fmt.Fprintf(w, "    CopyFiles[%d]: RelDst=%q ContentHash=%q\n", i, cf.RelDst, cf.ContentHash) //nolint:errcheck // best-effort diag
				}
			}
		}
	}
}

// storedBreakdownVersion is the version a JSON-encoded stored breakdown was
// written under, "" for a legacy or unreadable one.
func storedBreakdownVersion(stored string) string {
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stored)), &v); err != nil {
		return ""
	}
	return v.Version
}

// nonRestartFingerprintExtra is the Ignore and NextStart subset of extra.
func nonRestartFingerprintExtra(extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range extra {
		if FingerprintExtraChangePolicy(k) != ChangeRestart {
			out[k] = v
		}
	}
	return out
}

// parseStoredBreakdown decodes the JSON-encoded stored breakdown and
// returns the per-field map, the typed CopyFiles entries, and a flag
// indicating which format was used. If the JSON parses as a BreakdownV1
// with a non-empty Version, the per-entry CopyFiles slice is returned
// and isV1=true. If Version is empty (or the JSON is a bare
// map[string]string), the map is returned and isV1=false. Empty input
// yields an empty map and isV1=true (no CopyFiles to diff).
func parseStoredBreakdown(stored string) (fields map[string]string, copyEntries []BreakdownCopyEntry, isV1 bool) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return nil, nil, true
	}
	var v1 BreakdownV1
	if err := json.Unmarshal([]byte(stored), &v1); err == nil && v1.Version != "" {
		if v1.Fields == nil {
			v1.Fields = map[string]string{}
		}
		return v1.Fields, v1.CopyFiles, true
	}
	var legacy map[string]string
	if err := json.Unmarshal([]byte(stored), &legacy); err == nil {
		return legacy, nil, false
	}
	return nil, nil, false
}

// diffBreakdownFields returns the sorted list of field names where the
// stored and current per-field hashes differ.
func diffBreakdownFields(stored, current map[string]string) []string {
	var diffs []string
	for field, ch := range current {
		if stored[field] != ch {
			diffs = append(diffs, field)
		}
	}
	sort.Strings(diffs)
	return diffs
}

// logCopyFilesEntryDiff renders the per-entry CopyFiles diff block. Each
// entry is keyed by RelDst; the union of stored and current RelDsts is
// printed in alphabetical order with one of four markers:
//
//	[ ]  unchanged (Probed, ContentHash, Src all match)
//	[~]  same RelDst, differing render (ContentHash, Probed flag, or Src)
//	[+]  present in current but not in stored
//	[-]  present in stored but not in current
//
// Format per line:
//
//	[<marker>] <RelDst>  stored=<rendered>  current=<rendered>
//
// Where each side is rendered as `(absent)`, `<8-char-hex> (probed)`,
// `HASH_UNAVAILABLE (probed)` (when probed with empty hash), or
// `src=<src>` (when non-probed). The two-space separator between the
// stored and current columns is enforced by the validator regex.
func logCopyFilesEntryDiff(w io.Writer, stored, current []BreakdownCopyEntry) {
	storedByDst := make(map[string]BreakdownCopyEntry, len(stored))
	for _, e := range stored {
		storedByDst[e.RelDst] = e
	}
	currentByDst := make(map[string]BreakdownCopyEntry, len(current))
	for _, e := range current {
		currentByDst[e.RelDst] = e
	}

	// Size hint to the larger of the two inputs; map/slice grow as needed.
	// Avoids len(a)+len(b) so the size computation can't overflow when the
	// inputs trace back through parsed JSON metadata (CWE-190).
	capHint := len(storedByDst)
	if len(currentByDst) > capHint {
		capHint = len(currentByDst)
	}
	keys := make([]string, 0, capHint)
	seen := make(map[string]bool, capHint)
	for k := range storedByDst {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range currentByDst {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	fmt.Fprintf(w, "    CopyFiles: stored=%d entries  current=%d entries\n", len(stored), len(current)) //nolint:errcheck // best-effort diag

	for _, dst := range keys {
		s, sOK := storedByDst[dst]
		c, cOK := currentByDst[dst]
		var marker string
		switch {
		case sOK && !cOK:
			marker = "[-]"
		case !sOK && cOK:
			marker = "[+]"
		case copyEntryEqual(s, c):
			marker = "[ ]"
		default:
			marker = "[~]"
		}
		storedRender := "(absent)"
		if sOK {
			storedRender = renderCopyEntry(s)
		}
		currentRender := "(absent)"
		if cOK {
			currentRender = renderCopyEntry(c)
		}
		fmt.Fprintf(w, "    %s %s  stored=%s  current=%s\n", marker, dst, storedRender, currentRender) //nolint:errcheck // best-effort diag
	}
}

// copyEntryEqual reports whether two entries have identical render-
// affecting fields.
func copyEntryEqual(a, b BreakdownCopyEntry) bool {
	return a.Probed == b.Probed && a.ContentHash == b.ContentHash && a.Src == b.Src
}

// renderCopyEntry formats one side of a CopyFiles entry diff. Probed
// entries render as `<8-char-hex> (probed)` (or the literal
// `HASH_UNAVAILABLE (probed)` when ContentHash is empty); non-probed
// entries render as `src=<src>`.
func renderCopyEntry(e BreakdownCopyEntry) string {
	if e.Probed {
		hash := e.ContentHash
		if hash == "" {
			hash = "HASH_UNAVAILABLE"
		} else if len(hash) > 8 {
			hash = hash[:8]
		}
		return fmt.Sprintf("%s (probed)", hash)
	}
	return fmt.Sprintf("src=%s", e.Src)
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
