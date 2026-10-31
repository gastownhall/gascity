//go:build !windows

package main

import "os"

// runMapDirSafeToPublish reports whether the resolved run-map dir is safe to
// publish a proxy-trusted <session>.json into. os.MkdirAll self-provisions an
// owner-only 0o755 dir but is a no-op on a pre-existing one, so an externally
// provisioned dir keeps its own mode and must be re-checked here.
//
// A dir writable by neither group nor other is always safe: owner-only (0o755),
// or a read-only shared-group dir (0o750), where non-owners cannot create or
// replace entries. Directory write permission is what lets a non-owner create,
// rename, or delete entries, so group-write is gated exactly like other-write:
// a group- or other-writable dir is trusted only as a sticky handoff owned by
// root or this user — the manifold proxy's deliberate multi-user contract, where
// root provisions /run/gc-manifold-runmap as 0o1777 so each agent cell drops its
// own <session>.json and the proxy reads them (the /tmp trust model). A
// non-sticky group- or other-writable dir, or a sticky one owned by another
// user, is refused (CWE-732).
//
// This gate bounds the DIRECTORY's provisioner; it does NOT by itself make the
// shared handoff forgery-proof. The sticky bit only stops a non-owner from
// deleting or renaming over an EXISTING file — it does not stop first-writer
// squatting of a not-yet-existing, predictable <session>.json. So in the shared
// 0o1777 handoff a hostile co-uid can pre-plant a victim's file, and per-file
// run-map authenticity is therefore the READER's responsibility: the manifold
// proxy MUST authenticate each <session>.json by owner (st_uid), mode, and link
// state before trusting run_id — a hard precondition of using a shared handoff.
// writeRunMap additionally refuses to publish over a symlink or foreign-owned
// target (see publishRunMapKey) so the writer never silently blesses a squat, but
// a world-writable handoff cannot be made forgery-proof by the writer alone.
//
// Deployment trust model (verified against the deployed gc-manifold-proxy, which
// reads <session>.json with an unauthenticated os.ReadFile and provisions the dir
// 0o1777): the handoff lives inside a single fleet uid — every agent cell writes
// as that uid and root reads — so the residual forgery is intra-trust-domain.
// Exploiting it needs an already-compromised same-uid cell, and the asset is only
// a best-effort spend-correlation header that degrades safely (a forged or missing
// mapping mis-stamps or omits X-Gc-Run-Id; it never affects code, data, or
// privilege). Because every cell shares that uid, even reader-side owner
// authentication (st_uid == fleet uid) cannot separate a genuine publish from a
// forged one; real per-session anti-forgery needs a proxy-side control the writer
// cannot supply alone — an unguessable per-cell filename/nonce or a
// root-authenticated private channel. That out-of-repo reader/deploy hardening is
// tracked in ga-zzvsuls.
func runMapDirSafeToPublish(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	// Group- and other-write are gated identically: either bit lets a non-owner
	// create, rename, or delete the <session>.json the proxy trusts, so a
	// group-writable dir is no safer than a world-writable one (CWE-732).
	if info.Mode().Perm()&0o022 == 0 {
		return true
	}
	if info.Mode()&os.ModeSticky == 0 {
		return false
	}
	return runMapDirOwnedByTrustedUser(info)
}

func runMapDirUnsafeReason() string {
	return "is group/other-writable without a sticky trusted-owner handoff"
}

// runMapDirPrunable reports whether the writer may safely reap stale files from
// dir on the claim hot path: true only for a dir writable by neither group nor
// other — one this user provisioned (0o755) or a read-only shared-group dir
// (0o750), where every entry is created by this uid (or root) and os.Remove can
// actually unlink it. It is false for the shared manifold-proxy handoff (a
// group- or other-writable sticky dir, canonically root-owned 0o1777): a
// non-root writer cannot unlink another uid's <session>.json there (the sticky
// bit yields EPERM), so an in-process reap is a no-op — and scanning an
// attacker-fillable directory on every claim would let a local co-tenant inflate
// claim latency by filling it (CWE-400). Cleanup of the shared handoff is the
// provisioner's job (systemd-tmpfiles / the root proxy / tmpfs reboot).
func runMapDirPrunable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o022 == 0
}
