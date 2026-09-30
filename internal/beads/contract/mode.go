package contract

import "strings"

// MigrateDoltModeJournalFile is the name beads gives the in-flight journal of
// `bd migrate from-server-to-proxied-server` (beads cmd/bd/migrate_dolt_mode.go
// `migrateJournalFileName`).
//
// bd writes dolt_mode=proxied-server into metadata.json while the journal is
// still at `prepared` and removes the journal only after `committed`, so the
// journal's presence — not the mode — is what says whether the migration
// finished. gc watches for it under bd's name; bd's `--json` result reports the
// requested transition, not the phase it reached, so there is nothing structured
// to prefer over the filename. TestMigrateJournalFileMatchesPinnedBeads keeps
// this constant honest against the pinned module.
const MigrateDoltModeJournalFile = "dolt-mode-migration.json"

// IsDoltBackend reports whether backend uses Gas City's Dolt contract.
func IsDoltBackend(backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	return backend == "" || backend == "dolt" || backend == "bd"
}

// IsProxiedDoltMode reports whether mode selects Beads' proxied-server path
// for a backend that Gas City treats as Dolt.
func IsProxiedDoltMode(backend, mode string) bool {
	if !IsDoltBackend(backend) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(mode), "proxied-server")
}
