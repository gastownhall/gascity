package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/session"
)

// claimAuthorityEntries is the set of identities this city vouches for on
// routed work: every assignee identity of an open session (the identities the
// orphan sweep's openSessionOwnsWork accepts). It is the city's side of the
// claim-authority boundary: an armed store refuses a routed claim by anyone
// outside it.
func claimAuthorityEntries(cfg *config.City, openSessions []session.Info) []beads.ClaimAuthorityEntry {
	seen := map[string]bool{}
	var out []beads.ClaimAuthorityEntry
	add := func(identity string, source beads.ClaimAuthoritySource) {
		identity = strings.TrimSpace(identity)
		if identity == "" || seen[identity] {
			return
		}
		seen[identity] = true
		out = append(out, beads.ClaimAuthorityEntry{Identity: identity, Source: source})
	}
	for _, info := range openSessions {
		if info.Closed {
			continue
		}
		for _, identity := range sessionBeadAssigneeIdentitiesForConfigInfo(info, cfg) {
			add(identity, beads.ClaimAuthoritySession)
		}
	}
	return out
}

// claimAuthorityMode resolves the city's beads.claim_authority rollout gate:
// city config plus the GC_BEADS_CLAIM_AUTHORITY override, looked up in the
// process environment and then the city's workspace env.
func claimAuthorityMode(cfg *config.City) (string, error) {
	if cfg == nil {
		return beads.ClaimAuthorityModeOff, nil
	}
	workspaceEnv := expandEnvMap(cfg.Workspace.Env)
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{LookupEnv: func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := workspaceEnv[key]
		return v, ok
	}})
	if err != nil {
		return "", fmt.Errorf("resolving beads.claim_authority: %w", err)
	}
	return string(flags.BeadsClaimAuthority()), nil
}

// claimAuthorityPublisher names the city on the rows it writes.
func claimAuthorityPublisher(cityPath string, cfg *config.City, mode string) beads.ClaimAuthorityPublisher {
	cityName := cfg.EffectiveCityName()
	return beads.ClaimAuthorityPublisher{
		Tag:  beads.ClaimAuthorityCityTag(normalizePathForCompare(cityPath), cityName),
		City: cityName,
		Mode: mode,
	}
}

// claimAuthorityScope is one bead store the city's sessions claim from.
type claimAuthorityScope struct {
	ID   string
	Root string
}

// claimAuthorityScopes lists the city scope and every rig scope that use the
// bd store contract. A scope with no direct SQL endpoint is dropped later, by
// withScopeDoltConn.
func claimAuthorityScopes(cityPath string, cfg *config.City) []claimAuthorityScope {
	var scopes []claimAuthorityScope
	if scopeUsesManagedBdStoreContract(cityPath, cityPath) {
		scopes = append(scopes, claimAuthorityScope{ID: "city", Root: cityPath})
	}
	if cfg == nil {
		return scopes
	}
	for _, rig := range cfg.Rigs {
		root := strings.TrimSpace(rig.Path)
		if root == "" || samePath(root, cityPath) || !scopeUsesManagedBdStoreContract(cityPath, root) {
			continue
		}
		scopes = append(scopes, claimAuthorityScope{ID: "rig/" + rig.Name, Root: root})
	}
	return scopes
}

// claimAuthorityScopeReport is what one scope's reconcile found and did.
type claimAuthorityScopeReport struct {
	Scope claimAuthorityScope
	// Reached reports that the scope had a direct SQL endpoint to reconcile.
	Reached bool
	Report  beads.ClaimAuthorityReconcileReport
	Err     error
}

// claimAuthorityStoreConn opens the pinned connection a scope's
// claim-authority work runs over. Tests swap it.
var claimAuthorityStoreConn = func(cityPath, scopeRoot string, fn func(ctx context.Context, conn *sql.Conn) error) (bool, error) {
	return withScopeDoltConn(cityPath, scopeRoot, func(ctx context.Context, conn *sql.Conn, _ contract.DoltConnectionTarget) error {
		return fn(ctx, conn)
	})
}

// reconcileClaimAuthority publishes entries into every bd scope of the city
// and arms the scopes the city's mode asks for. prune is true on the
// controller tick, which sees the whole open-session snapshot, and false on a
// pre-start publish, which adds one session's identities. Each scope is
// reconciled independently; one unreachable store does not stop the rest.
func reconcileClaimAuthority(cityPath string, cfg *config.City, entries []beads.ClaimAuthorityEntry, prune bool, now time.Time) ([]claimAuthorityScopeReport, error) {
	mode, err := claimAuthorityMode(cfg)
	if err != nil {
		return nil, err
	}
	pub := claimAuthorityPublisher(cityPath, cfg, mode)
	cityName := cfg.EffectiveCityName()
	request := beads.ClaimAuthorityReconcile{
		Publisher: pub,
		Entries:   entries,
		Now:       now,
		Prune:     prune,
		HolderObservable: func(assignee string) bool {
			return poolAssigneeIsLocallyObservable(cfg, cityName, assignee)
		},
	}
	var reports []claimAuthorityScopeReport
	for _, scope := range claimAuthorityScopes(cityPath, cfg) {
		sr := claimAuthorityScopeReport{Scope: scope}
		sr.Reached, sr.Err = claimAuthorityStoreConn(cityPath, scope.Root, func(ctx context.Context, conn *sql.Conn) error {
			var rerr error
			sr.Report, rerr = beads.ReconcileClaimAuthority(ctx, conn, request)
			return rerr
		})
		if sr.Err != nil {
			sr.Err = fmt.Errorf("claim authority on %s: %w", scope.ID, sr.Err)
		}
		reports = append(reports, sr)
	}
	return reports, nil
}

// claimAuthorityLogf receives the controller's claim-authority log lines.
// Tests swap it.
var claimAuthorityLogf = log.Printf

// claimAuthorityLastSkip remembers the last skip reason logged per scope, so
// a store that stays unarmed tick after tick is reported once per change.
var claimAuthorityLastSkip sync.Map

// logClaimAuthorityReports logs failures, newly armed stores and changes in
// why a store stays unarmed.
func logClaimAuthorityReports(op, cityPath string, reports []claimAuthorityScopeReport, err error) {
	if err != nil {
		claimAuthorityLogf("%s: claim authority: %v", op, err)
		return
	}
	for _, sr := range reports {
		key := cityPath + "\x00" + sr.Scope.Root
		switch {
		case sr.Err != nil:
			claimAuthorityLogf("%s: %v", op, sr.Err)
		case sr.Report.ArmedNow:
			claimAuthorityLogf("%s: claim authority armed on %s", op, sr.Scope.ID)
		}
		if prev, _ := claimAuthorityLastSkip.Swap(key, sr.Report.Skipped); prev != sr.Report.Skipped && sr.Report.Skipped != "" {
			claimAuthorityLogf("%s: claim authority not armed on %s: %s", op, sr.Scope.ID, sr.Report.Skipped)
		}
	}
}

// publishClaimAuthorityOnTick is the controller tick's full publish: the
// identities of every open session in the snapshot, with pruning.
func publishClaimAuthorityOnTick(cityPath string, cfg *config.City, openSessions []session.Info) {
	if cfg == nil {
		return
	}
	reports, err := reconcileClaimAuthority(cityPath, cfg, claimAuthorityEntries(cfg, openSessions), true, time.Now().UTC())
	logClaimAuthorityReports("controller tick", cityPath, reports, err)
}

// publishClaimAuthorityBeforeStart adds one session's identities, plus the
// city's static forms, before its runtime starts, so a session started in
// the middle of a tick can claim before the next tick's full publish sees
// it.
func publishClaimAuthorityBeforeStart(cityPath string, cfg *config.City, info session.Info) {
	if cfg == nil {
		return
	}
	reports, err := reconcileClaimAuthority(cityPath, cfg, claimAuthorityEntries(cfg, []session.Info{info}), false, time.Now().UTC())
	logClaimAuthorityReports("session start", cityPath, reports, err)
}
