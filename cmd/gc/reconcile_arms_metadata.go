package main

import (
	"cmp"
	"maps"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's row-metadata arm (CONTRACT v5 M1, A7; plan C7d): a desired
// row's identity converged to its configuration before A8 hashes it (v5
// B4): legacy's per-row identity refresh (SESS-713), the managed alias under
// the city alias lock (SESS-717/718), the stale pool slot (SESS-719) and a
// canonical singleton's collapse (POOL-046). The rest of the refresh
// (SESS-714..716) is C7d-2's.

// Row-metadata decide reasons.
const (
	decideRowIdentity  = "row-metadata:identity"
	decideRowAlias     = "row-metadata:alias" // the identity with alias work under the lock
	decideRuntimeAlias = "runtime-alias"      // a written alias's runtime sync
)

// rowIdentity is a desired row's identity decision: the keys to write, the
// keys that wait for the alias (legacy's alias-guarded batch), the title and
// label deltas, and the alias work.
type rowIdentity struct {
	Meta, Guarded session.MetadataPatch
	Title         *string
	Add, Remove   []string
	// AliasWork: the row's alias is not its managed Alias, so the effect
	// checks Alias under the lock (Owner its availability owner) and writes
	// it, or records the conflict.
	AliasWork    bool
	Alias, Owner string
	ManagedPool  bool // a managed pool instance: a conflict clears a wrong alias
}

// desiredRowIdentity is legacy's identity refresh of an existing desired
// row (syncSessionBeadsWithSnapshotAndRigStores' update arm, then
// syncDesiredPoolSlots, after POOL-046's collapse), pure: tp is the row's
// resolution (the memo, the realize path for a pool seat).
func desiredRowIdentity(cfg *config.City, tp TemplateParams, info session.Info) rowIdentity {
	id := rowIdentity{Meta: session.MetadataPatch{}, Guarded: session.MetadataPatch{}}
	cfgAgent := findAgentByTemplate(cfg, tp.TemplateName)
	named := strings.TrimSpace(tp.ConfiguredNamedIdentity) != ""
	if cfgAgent != nil && cfgAgent.UsesCanonicalSingletonPoolIdentity() && !named && !isManualSessionInfoForAgent(info, cfgAgent) && !isNamedSessionInfo(info) {
		p := nonExpandingPoolIdentityPatchInfo(cfgAgent, info) // POOL-046; its alias is the managed alias's, below
		for _, k := range []string{"agent_name", "pool_slot"} {
			if v, ok := p.metadata[k]; ok {
				id.Meta[k] = v
			}
		}
		id.Title, id.Add, id.Remove = p.title, p.addLabels, p.removeLabels
		info = info.ApplyPatch(id.Meta)
	}
	origin := templateParamsSessionOrigin(tp)
	agentName, poolSlot := tp.TemplateName, tp.PoolSlot
	if poolSlot <= 0 {
		poolSlot = resolvePoolSlot(tp.InstanceName, tp.TemplateName)
	}
	if poolSlot > 0 || tp.InstanceName != "" && tp.InstanceName != tp.TemplateName {
		agentName = tp.InstanceName
	}
	managedPool, poolInstance := origin == "ephemeral", poolSlot > 0
	transient := usesTransientPoolSlotIdentity(cfgAgent)
	alias := strings.TrimSpace(tp.Alias)
	if alias == "" && managedPool && poolInstance && !transient {
		alias = strings.TrimSpace(tp.InstanceName)
	}
	if slot, err := strconv.Atoi(strings.TrimSpace(info.PoolSlot)); poolSlot <= 0 && err == nil && slot > 0 {
		poolInstance = true
		if alias == "" && !transient {
			alias = strings.TrimSpace(info.AgentName)
			if cfgAgent != nil && cfgAgent.UsesCanonicalSingletonPoolIdentity() {
				alias = cfgAgent.QualifiedName()
			}
		}
	}
	qualified := tp.TemplateName
	if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
		qualified = tp.RigName + "/" + tp.TemplateName
	}
	set := func(m session.MetadataPatch, k, v, have string) {
		if have != v {
			m[k] = v
		}
	}
	if info.Template == "" || tp.RigName != "" && !strings.Contains(info.Template, "/") {
		id.Meta["template"] = qualified
	}
	set(id.Meta, "session_origin", origin, info.SessionOrigin)
	if managedPool && !info.PoolManaged {
		id.Meta[poolManagedMetadataKey] = boolMetadata(true)
	}
	if named {
		if info.PoolManaged {
			id.Meta[poolManagedMetadataKey] = ""
		}
		set(id.Meta, "pool_slot", "", info.PoolSlot)
	}
	if alias == "" && managedPool && !poolInstance && info.PoolManaged && cfgAgent != nil && cfgAgent.UsesCanonicalSingletonPoolIdentity() &&
		strings.TrimSpace(info.PoolAliasConflict) == cfgAgent.QualifiedName() {
		alias = cfgAgent.QualifiedName()
	}
	id.AliasWork, id.Alias, id.ManagedPool = strings.TrimSpace(info.Alias) != alias, alias, managedPool && poolInstance
	switch {
	case named:
		id.Owner = tp.ConfiguredNamedIdentity
	case id.ManagedPool:
		id.Owner = alias
	}
	if info.PoolSlot == "" {
		slotKeys := id.Meta
		if id.AliasWork && id.ManagedPool {
			slotKeys = id.Guarded
		}
		if slot := tp.PoolSlot; slot > 0 {
			slotKeys["pool_slot"] = strconv.Itoa(slot)
		} else if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
			slotKeys["pool_slot"] = strconv.Itoa(slot)
		}
	}
	existing := strings.TrimSpace(info.AgentName)
	legacyTemplate := agentName != "" && agentName != tp.TemplateName && (existing == tp.TemplateName || existing == targetBasename(tp.TemplateName))
	if (existing == "" || legacyTemplate) && agentName != "" {
		id.Guarded["agent_name"] = agentName
	}
	set(id.Meta, "dependency_only", boolMetadata(tp.DependencyOnly), info.DependencyOnlyMetadata)
	if named {
		set(id.Meta, namedSessionMetadataKey, boolMetadata(true), boolMetadata(info.ConfiguredNamedSession))
		set(id.Meta, namedSessionIdentityMetadata, tp.ConfiguredNamedIdentity, info.ConfiguredNamedIdentity)
		set(id.Meta, namedSessionModeMetadata, tp.ConfiguredNamedMode, info.ConfiguredNamedMode)
	} else {
		for k, v := range map[string]string{namedSessionMetadataKey: boolMetadata(info.ConfiguredNamedSession), namedSessionIdentityMetadata: info.ConfiguredNamedIdentity, namedSessionModeMetadata: info.ConfiguredNamedMode} {
			if v != "" && v != "false" {
				id.Meta[k] = ""
			}
		}
	}
	// SESS-719: a pool row's slot follows the allocation's (realize).
	if cfgAgent != nil && cfgAgent.SupportsInstanceExpansion() && !cfgAgent.UsesCanonicalSingletonPoolIdentity() && !named && !tp.ManualSession &&
		tp.PoolSlot > 0 && (tp.Alias == "" || strings.TrimSpace(info.Alias) == tp.Alias) {
		if !info.PoolManaged {
			id.Meta[poolManagedMetadataKey] = boolMetadata(true)
		}
		set(id.Meta, "pool_slot", strconv.Itoa(tp.PoolSlot), info.PoolSlot)
	}
	return id
}

// identityPatch is id's write on info: aliasErr is the live availability
// check's (nil when there is no alias work or the alias is free). An alias
// written comes with its history, the conflict cleared, a started row's
// drift rebaseline (rebaseline) and the guarded keys; a conflict is recorded
// on its transition only (no per-pass revalidation, no retry timer) and
// clears a managed pool row's wrong alias; with no alias work, the conflict
// markers clear and the guarded keys merge. A blocked alias (a recorded
// conflict the census does not show free) writes the plain keys alone.
func identityPatch(id rowIdentity, info session.Info, blocked bool, aliasErr error, rebaseline func() session.MetadataPatch, nowRFC string) session.MetadataPatch {
	out := maps.Clone(id.Meta)
	merge := func() {
		queueClearPoolAliasConflictMetadataInfo(out, info)
		maps.Copy(out, id.Guarded)
	}
	switch {
	case blocked:
	case !id.AliasWork:
		merge()
	case aliasErr == nil:
		maps.Copy(out, session.UpdatedAliasMetadataFromInfo(info, id.Alias))
		if strings.TrimSpace(info.StartedConfigHash) != "" {
			maps.Copy(out, rebaseline())
		}
		merge()
	default:
		if !aliasConflictRecorded(info, id.Alias) {
			count, _ := strconv.Atoi(strings.TrimSpace(info.PoolAliasConflictCount))
			out[poolAliasConflictMetadataKey], out[poolAliasConflictCountMetadataKey], out[poolAliasConflictAtMetadataKey] = id.Alias, strconv.Itoa(max(count, 0)+1), nowRFC
		}
		if current := strings.TrimSpace(info.Alias); id.ManagedPool && current != "" && current != id.Alias {
			out["alias"] = ""
		}
	}
	if len(out) > 0 || id.Title != nil || len(id.Add)+len(id.Remove) > 0 { // a title or label delta writes too
		out["synced_at"] = nowRFC
	}
	return out
}

// aliasConflictRecorded reports a conflict already recorded for alias.
func aliasConflictRecorded(info session.Info, alias string) bool {
	return strings.TrimSpace(info.PoolAliasConflict) == alias && strings.TrimSpace(info.PoolAliasConflictCount) != ""
}

// aliasTaken reports id's alias held against row k on the census, by the
// live check's rule (session.AliasConflictInfo: another row's session name,
// alias or concrete identity, or a configured named session's reservation)
// over k's leg: a recorded conflict is retried only once it reads free (v5
// M1).
func (w *World) aliasTaken(k rowKey, id rowIdentity, info session.Info) bool {
	rows := make([]session.Info, 0, len(w.Census.Rows))
	for key, row := range w.Census.Rows {
		if key.Leg == k.Leg {
			rows = append(rows, row.Info)
		}
	}
	return session.AliasConflictInfo(rows, w.Env.Cfg, id.Alias, info.ID, id.Owner, &info) != nil
}

// rowIdentityOf is the identity decision on row k, read as info: the
// resolution, whether its alias work is blocked, and the lock it takes
// (the target alias, or the alias it clears).
func (w *World) rowIdentityOf(k rowKey, info session.Info) (id rowIdentity, blocked bool, lock string, ok bool) {
	res, resolved := w.Templates.lookup(info)
	if !resolved || res.Err != nil || w.Env == nil || w.Env.Cfg == nil {
		return rowIdentity{}, false, "", false
	}
	id = desiredRowIdentity(w.Env.Cfg, res.TP, info)
	blocked = id.AliasWork && aliasConflictRecorded(info, id.Alias) && w.aliasTaken(k, id, info)
	if w.a7Deferred(k) { // the plain keys alone, as legacy's unlocked batch
		blocked, id.Title, id.Add, id.Remove = id.AliasWork, nil, nil, nil
	}
	if id.AliasWork && !blocked {
		lock = cmp.Or(id.Alias, strings.TrimSpace(info.Alias))
	}
	return id, blocked, lock, true
}

// a7Deferred reports A7's alias, title and label work deferred on row k:
// its last refusal was A7's own persistent one (the alias lock, or labels
// the store cannot guard), and its backoff has not run out twice over, so
// between A7's retries the row's other arms take its passes (on a lock
// error legacy writes the unlocked batch and goes on).
func (w *World) a7Deferred(k rowKey) bool {
	rec := w.Backoff[rowBackoffKey(k)]
	return (rec.Cause == causeAliasLock || rec.Cause == causeLabelsUnguarded) && rec.Until.Add(vetoBackoff(rec.Consecutive)).After(w.Now)
}

// armRowMetadata is A7: a desired row whose identity is not converged gets
// the row-metadata verb (not in a suspended city, whose legacy refreshes
// nothing), taking the alias lock when its alias work needs it
// (intent.AliasLock); the verb's step decides by the same patch, so a
// proposal always has a write. A row A7 changes starts on the next pass,
// its identity written ("metadata not converged → A7 first"). Once
// converged, a row whose alias A7 wrote gets its runtime synced.
func armRowMetadata(r *rowFacts) (intent, bool) {
	if r.entry == nil || !r.entry.InDesired || r.w.CitySuspended { // a suspended city's legacy builds no desired state
		return intent{}, false
	}
	id, blocked, lock, ok := r.w.rowIdentityOf(r.k, r.row.Info)
	switch {
	case !ok:
		return intent{}, false
	case lock != "" || len(identityPatch(id, r.row.Info, blocked, nil, nil, "")) > 0:
		reason := decideRowIdentity
		if lock != "" {
			reason = decideRowAlias
		}
		it, _ := r.heal(intentRowMetadata, reason, nil)
		it.AliasLock = lock
		return it, true
	case r.w.AliasOwed[r.k]:
		r.rests |= restRuntime // the sync reads the runtime fresh
		return r.heal(intentRuntimeAlias, decideRuntimeAlias, nil)
	}
	return intent{}, false
}
