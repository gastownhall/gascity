package main

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// maxGuardedPoolSlots bounds how many pool instance names the guard
// enumerates for an agent whose pool is unbounded but names its instances.
const maxGuardedPoolSlots = 1024

// taskWorkDirGuard rejects a claimed work bead's work_dir when that directory
// is the home of a session other than the candidate being launched. A work
// bead's legacy work_dir is the worktree creator's record; when the creator
// was a different agent (the bead was re-routed, or an operator stamped it),
// following it launches this session into that agent's home, where two live
// sessions then share a checkout. Both task resolvers (the live lookup and
// the snapshot index) apply the same guard so they cannot disagree.
//
// A directory is foreign when it is (a) the configured work_dir of an agent
// other than the candidate's own template, counting every pool instance name,
// or (b) the work_dir or gc.work_dir of an open session bead other than the
// candidate's. The candidate's own configured and recorded directories are
// never foreign.
type taskWorkDirGuard struct {
	cityPath string
	session  string
	rowDir   string
	homes    map[string]string
}

// newTaskWorkDirGuard builds the guard for candidate over cfg and the open
// session rows. Either may be empty: the corresponding half of the guard is
// then empty.
func newTaskWorkDirGuard(cityPath string, cfg *config.City, sessions []sessionpkg.Info, candidate startCandidate) *taskWorkDirGuard {
	g := &taskWorkDirGuard{
		cityPath: cityPath,
		session:  guardSessionLabel(candidate),
		rowDir:   cleanHomeDir(cityPath, preparedStartSessionWorkDir(candidate.info)),
		homes:    make(map[string]string),
	}
	own := map[string]bool{}
	for _, dir := range []string{candidate.info.WorkDir, candidate.info.WorkDirCanonical} {
		if dir = cleanHomeDir(cityPath, dir); dir != "" {
			own[dir] = true
		}
	}
	ownTemplate := ""
	if cfg != nil {
		ownTemplate = candidate.logicalTemplate(cfg)
	}
	g.addAgentHomes(cfg, ownTemplate, own)
	g.addSessionHomes(sessions, candidate.info.ID)
	for dir := range own {
		delete(g.homes, dir)
	}
	return g
}

func guardSessionLabel(candidate startCandidate) string {
	if name := strings.TrimSpace(candidate.name()); name != "" {
		return name
	}
	return candidate.info.ID
}

// cleanHomeDir anchors dir to the city and cleans it so equal directories
// compare equal however they were written. Empty stays empty.
func cleanHomeDir(cityPath, dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	return filepath.Clean(resolveWorkDirAgainstCity(cityPath, dir))
}

// addAgentHomes records each configured agent's work_dir (and each pool
// instance's) as a home owned by that agent. Agents of the candidate's own
// template are not foreign: their directories go to own instead.
func (g *taskWorkDirGuard) addAgentHomes(cfg *config.City, ownTemplate string, own map[string]bool) {
	if cfg == nil {
		return
	}
	cityName := loadedCityName(cfg, g.cityPath)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		// An agent with no work_dir launches in the shared rig or city root,
		// which is no private home.
		if strings.TrimSpace(a.WorkDir) == "" {
			continue
		}
		isOwn := ownTemplate != "" && a.QualifiedName() == ownTemplate
		for _, qualified := range agentHomeNames(a) {
			wd, err := resolveConfiguredWorkDirPathUnvalidated(g.cityPath, cityName, qualified, a, cfg.Rigs)
			if err != nil {
				continue
			}
			dir := cleanHomeDir(g.cityPath, wd)
			switch {
			case dir == "":
			case isOwn:
				own[dir] = true
			default:
				g.homes[dir] = "agent " + qualified
			}
		}
	}
}

// agentHomeNames lists the qualified names an agent's sessions can run
// under: the agent itself, plus each pool instance when the pool expands.
func agentHomeNames(a *config.Agent) []string {
	names := []string{a.QualifiedName()}
	if !a.SupportsInstanceExpansion() || a.UsesCanonicalSingletonPoolIdentity() {
		return names
	}
	slots := len(a.NamepoolNames)
	if m := a.EffectiveMaxActiveSessions(); m != nil {
		slots = *m
	}
	if slots > maxGuardedPoolSlots {
		slots = maxGuardedPoolSlots
	}
	for slot := 1; slot <= slots; slot++ {
		names = append(names, a.QualifiedInstanceName(poolInstanceName(a.Name, slot, a)))
	}
	return names
}

// openSessionHomes passes through the open session rows a guard reads, noting
// a listing failure: the rows then come back partial or empty, the
// configured-agent half still guards, and a start must not stall on a read.
func openSessionHomes(infos []sessionpkg.Info, err error) []sessionpkg.Info {
	if err != nil {
		log.Printf("task work_dir guard: %v; guarding with the session rows that loaded", err)
	}
	return infos
}

// addSessionHomes records the work_dir and gc.work_dir of every open session
// other than ownID.
func (g *taskWorkDirGuard) addSessionHomes(sessions []sessionpkg.Info, ownID string) {
	for _, info := range sessions {
		if info.ID == ownID {
			continue
		}
		owner := "session " + info.ID
		if name := strings.TrimSpace(info.SessionNameMetadata); name != "" {
			owner += " (" + name + ")"
		}
		for _, dir := range []string{info.WorkDir, info.WorkDirCanonical} {
			if dir = cleanHomeDir(g.cityPath, dir); dir != "" {
				g.homes[dir] = owner
			}
		}
	}
}

// foreignOwner names the agent or session whose home dir is, or "" when dir
// is not another session's home. A nil guard rejects nothing.
func (g *taskWorkDirGuard) foreignOwner(dir string) string {
	if g == nil {
		return ""
	}
	return g.homes[cleanHomeDir(g.cityPath, dir)]
}

// admit reports whether dir, named by work bead beadID, may be the launch
// dir. A foreign home is refused with a warning; an accepted dir that differs
// from the session row's is noted at info.
func (g *taskWorkDirGuard) admit(beadID, dir string) bool {
	if g == nil {
		return true
	}
	if owner := g.foreignOwner(dir); owner != "" {
		log.Printf("session %q: ignoring work_dir %q of bead %s: it is the home of %s", g.session, dir, beadID, owner)
		return false
	}
	if clean := cleanHomeDir(g.cityPath, dir); clean != g.rowDir {
		log.Printf("session %q: bead %s overrides launch dir %q -> %q", g.session, beadID, g.rowDir, clean)
	}
	return true
}
