package runtime

import (
	"crypto/sha256"
	"fmt"
	"hash"
)

// The de-conflation un-weld (Phase B) splits a session restart into two
// independent triggers: a change to a PROVISION (box-affecting) field requires
// rebuilding the box (Runtime.Provision — expensive); a change to a LAUNCH
// (agent-affecting) field only requires restarting the agent in the existing
// warm box (Transport.Launch — cheap). ProvisionFingerprint and LaunchFingerprint
// partition the exact set of fields CoreFingerprint hashes into those two halves.
//
// INVARIANT (pinned by fingerprint_partition_test.go): the two halves are
// disjoint and together cover every CoreFingerprint field — a change to any core
// field moves CoreFingerprint AND exactly one of {Provision, Launch}. No core
// change is ever silently dropped; it is only routed to the cheaper (relaunch) or
// more expensive (re-provision) restart.
//
// CoreFingerprint itself is UNCHANGED — these are additive hashes, and the PR0
// golden net pins Core byte-for-byte. Nothing consumes these yet; the reconciler
// gains relaunch-without-reprovision in a later step (Phase B2).
//
// Classification (settled adversarially; see
// docs/architecture/worker-runtime-transport-unweld-v0.md §6):
//
//	PROVISION (box):  Env (allow-listed), FingerprintExtra, PreStart,
//	                  OverlayDir, OverlayProviders, CopyFiles.
//	LAUNCH (agent):   Command, Lifecycle, Upstream, MCPServers,
//	                  AcceptStartupDialogs, MouseOn, SessionSetup,
//	                  SessionSetupScript.
//
// SessionSetup/SessionSetupScript are LAUNCH-half as of B2: the carriers now
// replay them idempotently on relaunch (tmux launchOrchestration; ssh/k8s
// runPostLaunchSetup), so a change to them is covered by a warm-box relaunch and
// must NOT force a reprovision. (They were PROVISION in B0, before relaunch
// existed.) Remaining caveat: MCPServers stays LAUNCH unless a future change
// materializes MCP config as staged on-disk box content (then it becomes
// PROVISION).

// ProvisionFingerprint returns a hash of only the box-affecting core fields. A
// change here means the box must be re-provisioned. Version-prefixed.
func ProvisionFingerprint(cfg Config) string {
	h := sha256.New()
	hashProvisionFields(h, cfg)
	return fmt.Sprintf("%s:%x", FingerprintVersion, h.Sum(nil))
}

// LaunchFingerprint returns a hash of only the agent-affecting core fields. A
// change here means the agent can be relaunched in the existing box.
// Version-prefixed.
func LaunchFingerprint(cfg Config) string {
	h := sha256.New()
	hashLaunchFields(h, cfg)
	return fmt.Sprintf("%s:%x", FingerprintVersion, h.Sum(nil))
}

// hashProvisionFields writes the box-affecting core fields to h, using the same
// per-field framing as hashCoreFields.
func hashProvisionFields(h hash.Hash, cfg Config) {
	hashSortedMapIncluded(h, cfg.Env, envFingerprintInclude)

	if len(cfg.FingerprintExtra) > 0 {
		h.Write([]byte("fp")) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		hashSortedMap(h, cfg.FingerprintExtra)
	}

	for _, ps := range cfg.PreStart {
		h.Write([]byte(ps)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel between slices

	h.Write([]byte(cfg.OverlayDir)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})              //nolint:errcheck // hash.Write never errors

	hashOverlayProviders(h, OverlayProviderNames(cfg))

	for _, cf := range cfg.CopyFiles {
		if cf.Probed {
			h.Write([]byte(cf.RelDst)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})         //nolint:errcheck // hash.Write never errors
			if cf.ContentHash != "" {
				h.Write([]byte(cf.ContentHash)) //nolint:errcheck // hash.Write never errors
			} else {
				h.Write([]byte("HASH_UNAVAILABLE")) //nolint:errcheck // stable sentinel
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

// hashLaunchFields writes the agent-affecting core fields to h, using the same
// per-field framing as hashCoreFields.
func hashLaunchFields(h hash.Hash, cfg Config) {
	h.Write([]byte(cfg.Command)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})           //nolint:errcheck // hash.Write never errors

	h.Write([]byte(cfg.Lifecycle)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})             //nolint:errcheck // hash.Write never errors

	hashMCPServers(h, cfg.MCPServers)
	hashOptionalBool(h, "accept_startup_dialogs", cfg.AcceptStartupDialogs)
	hashBool(h, "mouse_on", cfg.MouseOn)

	// SessionSetup + SessionSetupScript are LAUNCH-half (B2): the carriers replay
	// them idempotently on relaunch, so a change is covered by a warm-box relaunch
	// (no reprovision). Same per-field framing as the box half.
	for _, ss := range cfg.SessionSetup {
		h.Write([]byte(ss)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel between slices

	h.Write([]byte(cfg.SessionSetupScript)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})                      //nolint:errcheck // hash.Write never errors

	// Upstream (Phase C — model-serving selection identity) is LAUNCH-half: the
	// serving env is consumed by the agent at launch, so switching upstream
	// relaunches the agent in the warm box (B2.3) without reprovisioning. Same
	// conditional framing as hashCoreFields, so an unset Upstream contributes
	// nothing. The resolved credentials in Env are NOT hashed (allow-list).
	hashOptionalString(h, "upstream", cfg.Upstream)
}
