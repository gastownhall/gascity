package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// claimReasonNamespace: the claim sits in a rig with federation = "rig" and
// its identity is outside the rig's namespace and is none of this city's own
// pool sessions of the rig. Detail names the namespace. The verdict is
// foreign: another city that attaches the same store may hold it.
const claimReasonNamespace claimProvenanceReason = "namespace"

// rigNamespaceProvenance is claimIdentityProvenance asked about a claim in one
// rig's store: the verdict, plus what the rig's namespace contributed.
type rigNamespaceProvenance struct {
	claimProvenance
	// Checked reports that the rig is guarded and its namespace decided the
	// identity's admission (admitted, refused, or a failed lookup).
	Checked bool
	// Via is the admission arm that admitted the identity: admitLexical
	// ("lexical") or admitSessionRecord ("session_record"); admitNone
	// otherwise.
	Via admitVia
}

// rigNamespaceClaimProvenance decides whether this city can vouch for
// identity as the holder of a claim in the store of its rig rigName:
//
//   - rigName must be a configured rig of this city, matched exactly (case
//     included); otherwise the verdict is unknown, reason no_config. An
//     unknown rig is never read as an unguarded one.
//   - The rig's namespace is decided by store identity
//     (guardedRigNamespaceForRigName), as the guarded store, gc bd and the
//     hook claim decide it: an unguarded rig whose store is a guarded rig's,
//     or a guarded rig whose store is not its own, admits no identity.
//   - An unguarded rig: provenance() unchanged.
//   - A guarded rig whose namespace refuses identity: foreign, reason
//     namespace. That is decided without a store read unless identity embeds
//     a pool session id, which only this city's session records can admit.
//   - A failed session lookup: unknown, reason lookup_failed. It protects the
//     claim and is never answered local.
//   - Admitted as one of this city's own pool sessions of the rig: local,
//     reason session_record, detail the record's bead id.
//   - Admitted lexically: provenance() (the roster and the session records
//     still have to vouch for it, as on any rig).
//
// cityPath is the city's real path. sessions must be this city's own
// resolver; provenance is today's claimIdentityProvenance for the identity.
func rigNamespaceClaimProvenance(cfg *config.City, cityPath, rigName, identity string, sessions ownSessionResolver, provenance func() claimProvenance) rigNamespaceProvenance {
	if !rigConfigHasRigNamed(cfg, rigName) {
		return rigNamespaceProvenance{claimProvenance: claimProvenance{
			Verdict: claimVerdictUnknown, Reason: claimReasonNoConfig, Detail: fmt.Sprintf("unknown rig %q", rigName),
		}}
	}
	ns := guardedRigNamespaceForRigName(cfg, cityPath, rigName, sessions)
	if ns == nil {
		return rigNamespaceProvenance{claimProvenance: provenance()}
	}
	out := rigNamespaceProvenance{Checked: true}
	if ns.namespace() == "" {
		detail := fmt.Sprintf("rig %q admits no identity (federation = %q)", rigName, config.RigFederationRig)
		if ns.denyReason != "" {
			detail += ": " + ns.denyReason
		}
		out.claimProvenance = claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: detail}
		return out
	}
	a := ns.admission(strings.TrimSpace(identity))
	switch {
	case a.Err != nil:
		out.claimProvenance = claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonLookupFailed, Detail: a.Err.Error()}
	case a.Via == admitSessionRecord:
		out.Via = a.Via
		out.claimProvenance = claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonSessionRecord, Detail: a.Record.ID}
	case a.Via == admitLexical:
		out.Via = a.Via
		out.claimProvenance = provenance()
	default:
		out.claimProvenance = claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: "outside " + ns.namespace()}
	}
	return out
}
