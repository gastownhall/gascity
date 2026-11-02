package main

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	namedSessionMetadataKey      = session.NamedSessionMetadataKey
	namedSessionIdentityMetadata = session.NamedSessionIdentityMetadata
	namedSessionModeMetadata     = session.NamedSessionModeMetadata
)

type namedSessionSpec = session.NamedSessionSpec

func normalizeNamedSessionTarget(target string) string {
	return session.NormalizeNamedSessionTarget(target)
}

func targetBasename(target string) string {
	return session.TargetBasename(target)
}

func findNamedSessionSpec(cfg *config.City, cityName, identity string) (namedSessionSpec, bool) {
	return session.FindNamedSessionSpec(cfg, cityName, identity)
}

func namedSessionBackingTemplate(spec namedSessionSpec) string {
	return session.NamedSessionBackingTemplate(spec)
}

// applyNamedTemplateOverrides stamps a configured named session's identity
// on the template resolved for its backing agent: legacy's desired state and
// the allocator's named create share it.
func applyNamedTemplateOverrides(tp *TemplateParams, spec namedSessionSpec, identity, boundStepID string) {
	tp.Alias = identity
	tp.TemplateName = namedSessionBackingTemplate(spec)
	tp.InstanceName = identity
	tp.ConfiguredNamedIdentity = identity
	tp.ConfiguredNamedMode = spec.Mode
	tp.BoundStepID = boundStepID
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_TEMPLATE"] = namedSessionBackingTemplate(spec)
	tp.Env["GC_ALIAS"] = identity
	tp.Env["GC_AGENT"] = identity
	tp.Env["GC_SESSION_ORIGIN"] = "named"
}

// namedSessionAssigneeMatchesSpec reports whether assignee names spec's session.
// Work routed to a named session is claimed under the session's runtime name
// (config.NamedSessionRuntimeName: "/" -> "--", "." -> "__"), not under its
// qualified identity, so both forms have to count. spec.SessionName is already
// an accepted alias for the identity in the resolver
// (session.ResolveNamedSessionSpecForConfigTarget); matching only the qualified
// form here left on-demand named sessions asleep on their own assigned work
// (ga-e70d2). A claim under the bare leaf a requalified session was renamed
// from is its demand only through its row (namedBareClaimOwner).
func namedSessionAssigneeMatchesSpec(spec namedSessionSpec, identity, assignee string) bool {
	if assignee == "" {
		return false
	}
	return assignee == identity || assignee == strings.TrimSpace(spec.SessionName)
}

// requalifiedPriorIdentity returns the bare identity a configured named
// session row was renamed in place from when `gc rig add --adopt` requalified
// it (gastownhall/gascity#5957), or "". identity is the row's configured named
// identity and aliasHistory its alias history. The bare form is the leaf of
// the row's own config entry, which the row's alias history must record — the
// trace the in-place rename leaves — so a born-qualified row never claims it.
// It does not depend on the leaf staying unique: a binding of the same leaf
// added after the rename leaves the row its bare form, and only a top-level
// entry configured under the leaf takes it (config.NamedSessionPriorIdentity).
// Work claimed under the bare form before the adopt is the renamed row's own.
// The orphan-release checks already count it through the whole alias history
// (session.AssigneeIdentities); the narrow release scope and the awake set's
// assignee matching, which leave alias history out, count it through this
// form.
func requalifiedPriorIdentity(cfg *config.City, identity string, aliasHistory []string) string {
	identity = strings.TrimSpace(identity)
	if cfg == nil || identity == "" {
		return ""
	}
	bare := config.NamedSessionPriorIdentity(cfg, config.FindNamedSession(cfg, identity))
	if bare == "" || bare == identity || !slices.Contains(aliasHistory, bare) {
		return ""
	}
	return bare
}

// requalifiedPriorIdentityInfo is requalifiedPriorIdentity for a session row.
func requalifiedPriorIdentityInfo(info session.Info, cfg *config.City) string {
	if !isNamedSessionInfo(info) {
		return ""
	}
	return requalifiedPriorIdentity(cfg, namedSessionIdentityInfo(info), info.AliasHistory)
}

// requalifiedPriorIdentityOfBead is requalifiedPriorIdentityInfo for a caller
// holding the raw bead.
func requalifiedPriorIdentityOfBead(b beads.Bead, cfg *config.City) string {
	if !isNamedSessionBead(b) {
		return ""
	}
	return requalifiedPriorIdentity(cfg, namedSessionIdentity(b), session.AliasHistory(b.Metadata))
}

// keepsRequalifiedRuntimeName reports whether b is a configured named session
// row renamed in place by a requalification (requalifiedPriorIdentityOfBead)
// that still carries the runtime name its bare identity resolves to. The
// rename moves identity and alias, never the runtime, so that session_name is
// the row's own rather than a reconfiguration: legacy's reconfigured sweep
// keeps the row, as v2's planner does, and the session restarts on it after a
// stop instead of being re-minted under the qualified runtime name. A
// session_template change still moves the bare runtime name and so still
// sweeps the row.
func keepsRequalifiedRuntimeName(b beads.Bead, cfg *config.City, cityName string) bool {
	return keepsBareRuntimeName(requalifiedPriorIdentityOfBead(b, cfg), b.Metadata["session_name"], cfg, cityName)
}

// keepsRequalifiedRuntimeNameInfo is keepsRequalifiedRuntimeName for a
// session row.
func keepsRequalifiedRuntimeNameInfo(info session.Info, cfg *config.City, cityName string) bool {
	return keepsBareRuntimeName(requalifiedPriorIdentityInfo(info, cfg), info.SessionNameMetadata, cfg, cityName)
}

// keepsBareRuntimeName reports whether sessionName is the runtime name bare,
// a renamed row's requalified prior identity, resolves to.
func keepsBareRuntimeName(bare, sessionName string, cfg *config.City, cityName string) bool {
	return bare != "" && strings.TrimSpace(sessionName) == config.NamedSessionRuntimeName(cityName, cfg.Workspace, bare)
}

// requalifiedSpecRuntimeName returns the runtime name b's configured named
// session resolves to under its qualified identity (the spec's SessionName)
// when b is a row renamed in place that kept the bare form's runtime name
// (keepsRequalifiedRuntimeName), or "". That name is not b's session_name, yet
// it still names b: the named session's per-session lifecycle settings are
// registered under it (buildIdleTracker, buildMaxSessionAgeTracker,
// buildAssignedWorkDeferTracker, lifecycleSetting), and work claimed under it
// is the named session's (namedSessionAssigneeMatchesSpec). So the lifecycle
// lookups (lifecycleSessionNameInfo) and the identities b's work is matched
// against carry it, and the renamed row is that named session exactly as a
// born-qualified row is.
func requalifiedSpecRuntimeName(b beads.Bead, cfg *config.City, cityName string) string {
	if !keepsRequalifiedRuntimeName(b, cfg, cityName) {
		return ""
	}
	return config.NamedSessionRuntimeName(cityName, cfg.Workspace, namedSessionIdentity(b))
}

// requalifiedSpecRuntimeNameInfo is requalifiedSpecRuntimeName for a session
// row.
func requalifiedSpecRuntimeNameInfo(info session.Info, cfg *config.City, cityName string) string {
	if !keepsRequalifiedRuntimeNameInfo(info, cfg, cityName) {
		return ""
	}
	return config.NamedSessionRuntimeName(cityName, cfg.Workspace, namedSessionIdentityInfo(info))
}

// lifecycleSessionNameInfo returns the name info's per-session lifecycle
// settings (idle_timeout, max_session_age, assigned_work_defer_limit, and a
// mode="always" named session's exemption from them) are registered under,
// for a row whose runtime is named sessionName: the runtime name its named
// session resolves to when a requalification renamed the row in place and it
// kept the bare form's runtime name (requalifiedSpecRuntimeNameInfo), else
// sessionName itself. The runtime is still observed under sessionName.
func lifecycleSessionNameInfo(info session.Info, cfg *config.City, cityName, sessionName string) string {
	if name := requalifiedSpecRuntimeNameInfo(info, cfg, cityName); name != "" {
		return name
	}
	return sessionName
}

// namedBareClaimOwner returns a test of whether a claim under assignee is
// spec's demand by the bare leaf spec was requalified from
// (gastownhall/gascity#5957). It holds only when spec's canonical row owns
// the leaf: the row is still stamped with it while it awaits its in-place
// rename (session.NamedSessionIdentityIsPriorForm), or it was renamed from it
// (requalifiedPriorIdentityInfo). Config alone does not decide it: a seat
// with no row, or a born-qualified one, neither owns nor can see work claimed
// under the leaf, so counting that work would start a seat for nothing it can
// reach. canonical finds the row, once, on the first claim under the leaf; a
// nil canonical counts no bare claim.
func namedBareClaimOwner(cfg *config.City, spec namedSessionSpec, canonical func(namedSessionSpec) (session.Info, bool)) func(assignee string) bool {
	leaf := config.NamedSessionPriorIdentity(cfg, spec.Named)
	if leaf == "" || canonical == nil {
		return func(string) bool { return false }
	}
	owned, looked := false, false
	return func(assignee string) bool {
		if assignee != leaf {
			return false
		}
		if !looked {
			looked = true
			if row, ok := canonical(spec); ok {
				stamped := namedSessionIdentityInfo(row)
				awaitingRename := stamped == leaf && session.NamedSessionIdentityIsPriorForm(spec, stamped)
				owned = awaitingRename || requalifiedPriorIdentityInfo(row, cfg) == leaf
			}
		}
		return owned
	}
}

// findNamedSessionSpecForAssignee resolves the configured named session that a
// bead's assignee refers to, accepting every form a real claim can carry.
//
// findNamedSessionSpec alone is not enough: it matches the qualified identity
// (and the V2 bare leaf), but a named session claims work under its tmux-safe
// runtime name — "seth.seth" claims as "seth__seth". Callers that resolved
// assignees with the identity-only lookup were therefore inert for the one form
// that actually appears on claimed beads (ga-e70d2).
//
// The fallback deliberately reuses namedSessionAssigneeMatchesSpec rather than
// session.ResolveNamedSessionSpecForConfigTarget. That resolver also accepts
// bare template names because it resolves USER input (`gc session wake seth`);
// an assignee is not user input, and widening claim resolution that far would
// preserve routes for names no session ever claimed under. Config load rejects
// identity/session-name collisions city-wide (config.validateNamedSessions), so
// at most one spec can match.
func findNamedSessionSpecForAssignee(cfg *config.City, cityName, assignee string) (namedSessionSpec, bool) {
	if cfg == nil || strings.TrimSpace(assignee) == "" {
		return namedSessionSpec{}, false
	}
	if spec, ok := findNamedSessionSpec(cfg, cityName, assignee); ok {
		return spec, true
	}
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if namedSessionAssigneeMatchesSpec(spec, identity, assignee) {
			return spec, true
		}
	}
	return namedSessionSpec{}, false
}

func resolveNamedSessionSpecForConfigTarget(cfg *config.City, cityName, target, rigContext string) (namedSessionSpec, bool, error) {
	return session.ResolveNamedSessionSpecForConfigTarget(cfg, cityName, target, rigContext)
}

func findNamedSessionSpecForTarget(cfg *config.City, cityName, target string) (namedSessionSpec, bool, error) {
	return session.FindNamedSessionSpecForTarget(cfg, cityName, target, currentRigContext(cfg))
}

func isNamedSessionBead(b beads.Bead) bool {
	return session.IsNamedSessionBead(b)
}

// isNamedSessionInfo is the session.Info mirror of isNamedSessionBead:
// session.IsNamedSessionBead reads the trimmed configured_named_session flag,
// which Info.ConfiguredNamedSession already projects identically.
func isNamedSessionInfo(i session.Info) bool {
	return i.ConfiguredNamedSession
}

func namedSessionIdentity(b beads.Bead) string {
	return session.NamedSessionIdentity(b)
}

// namedSessionIdentityInfo is the session.Info mirror of namedSessionIdentity:
// session.NamedSessionIdentityInfo reads the trimmed configured_named_identity,
// which Info.ConfiguredNamedIdentity carries verbatim.
func namedSessionIdentityInfo(i session.Info) string {
	return session.NamedSessionIdentityInfo(i)
}

func configuredNamedSessionBeadHasSpec(b beads.Bead, cfg *config.City, cityName string) bool {
	if cfg == nil || !isNamedSessionBead(b) {
		return false
	}
	identity := namedSessionIdentity(b)
	if identity == "" {
		return false
	}
	_, ok := findNamedSessionSpec(cfg, cityName, identity)
	return ok
}

// configuredNamedSessionBeadHasSpecInfo is the session.Info mirror of
// configuredNamedSessionBeadHasSpec: isNamedSessionInfo and namedSessionIdentityInfo
// are the equivalence-proven siblings, and findNamedSessionSpec keys off the
// projected identity string identically.
func configuredNamedSessionBeadHasSpecInfo(i session.Info, cfg *config.City, cityName string) bool {
	if cfg == nil || !isNamedSessionInfo(i) {
		return false
	}
	identity := namedSessionIdentityInfo(i)
	if identity == "" {
		return false
	}
	_, ok := findNamedSessionSpec(cfg, cityName, identity)
	return ok
}

func namedSessionMode(b beads.Bead) string {
	return session.NamedSessionMode(b)
}

// namedSessionModeInfo is the session.Info mirror of namedSessionMode:
// session.NamedSessionModeInfo trims the raw configured_named_mode
// (Info.ConfiguredNamedMode), identical to the bead form.
func namedSessionModeInfo(i session.Info) string {
	return session.NamedSessionModeInfo(i)
}

// recyclableDeadConfiguredNamePhantomInfo reports the configured identity a
// process-dead session is squatting, when it is not that identity's canonical
// owner and is therefore safe to close and recycle. Used by the reconciler's
// Info-based close-orphan path.
func recyclableDeadConfiguredNamePhantomInfo(i session.Info, cfg *config.City, cityName string) (string, bool) {
	return session.RecyclableDeadConfiguredNamePhantomInfo(i, cfg, cityName)
}

func namedSessionContinuityEligible(b beads.Bead) bool {
	return session.NamedSessionContinuityEligible(b)
}

func findCanonicalNamedSessionInfo(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (session.Info, bool) {
	if sessionBeads == nil {
		return session.Info{}, false
	}
	return session.FindCanonicalNamedSessionInfo(sessionBeads.OpenInfos(), spec)
}

// findClosedNamedSessionBead searches for a closed bead that was previously
// the canonical bead for the given named session identity. Uses a targeted
// metadata query (Store.ListByMetadata) so only matching beads are returned
// — no bulk scan of all closed beads.
func findClosedNamedSessionBead(store beads.Store, identity string) (beads.Bead, bool) {
	bead, ok, _ := session.FindClosedNamedSessionBead(store, identity)
	return bead, ok
}

func findClosedNamedSessionBeadForSessionName(store beads.Store, identity, sessionName string) (beads.Bead, bool) {
	bead, ok, _ := session.FindClosedNamedSessionBeadForSessionName(store, identity, sessionName)
	return bead, ok
}

func findNamedSessionConflictInfo(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (session.Info, bool) {
	if sessionBeads == nil {
		return session.Info{}, false
	}
	return session.FindNamedSessionConflictInfo(sessionBeads.OpenInfos(), spec)
}

func findConflictingNamedSessionSpecForBead(cfg *config.City, cityName string, b beads.Bead) (namedSessionSpec, bool, error) {
	return session.FindConflictingNamedSessionSpecForBead(cfg, cityName, b)
}
