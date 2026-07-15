package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/spf13/cobra"
)

// Tab completion is load-bearing: these functions are called on every
// keystroke after <TAB>. They must be fast and never write to the terminal,
// since any stderr output would appear as garbage under the user's prompt.
// All errors are swallowed; a failed completion returns an empty candidate
// list with ShellCompDirectiveNoFileComp so the shell doesn't fall back to
// filename completion.

// completeSessionIDs completes session IDs and aliases for commands whose
// first positional argument is a session ID-or-alias.
func completeSessionIDs(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	sessions := loadSessionsForCompletion()
	candidates := make([]string, 0, len(sessions)*2)
	for _, s := range sessions {
		desc := sessionCompletionDescription(s)
		if strings.HasPrefix(s.ID, toComplete) {
			candidates = append(candidates, s.ID+"\t"+desc)
		}
		if s.Alias != "" && s.Alias != s.ID && strings.HasPrefix(s.Alias, toComplete) {
			candidates = append(candidates, s.Alias+"\t"+desc)
		}
	}
	return candidates, cobra.ShellCompDirectiveNoFileComp
}

// completeRigNames completes rig names for commands whose first positional
// is a rig name.
func completeRigNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return rigNameCandidates(toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeRigFlagNames completes rig names for --rig flags. Flag completion
// must ignore existing positional args; a user often completes --rig after
// typing the command's required positional.
func completeRigFlagNames(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return rigNameCandidates(toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeOrderNames completes order names for commands whose first
// positional is an order name.
func completeOrderNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	aa := loadOrdersForCompletion()
	candidates := make([]string, 0, len(aa))
	for _, o := range aa {
		if !strings.HasPrefix(o.Name, toComplete) {
			continue
		}
		candidates = append(candidates, o.Name+"\t"+orderCompletionDescription(o))
	}
	return candidates, cobra.ShellCompDirectiveNoFileComp
}

// completeCityNames completes registered city names for commands whose first
// positional is a city path-or-name. It uses ShellCompDirectiveDefault (not
// NoFileComp) because these commands also accept a directory path, so the
// shell should still offer filesystem paths alongside the registered names.
func completeCityNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cityNameCandidates(toComplete), cobra.ShellCompDirectiveDefault
}

// cityNameCandidates returns registered city names (with their paths as
// descriptions) as cobra completion entries, filtered by the prefix being
// typed. Reads only the supervisor registry, so it works from any cwd.
func cityNameCandidates(toComplete string) []string {
	var candidates []string
	quietDefaultLogger(func() {
		entries, err := supervisor.NewRegistry(supervisor.RegistryPath()).List()
		if err != nil {
			return
		}
		candidates = make([]string, 0, len(entries))
		for _, e := range entries {
			name := e.EffectiveName()
			if name == "" || !strings.HasPrefix(name, toComplete) {
				continue
			}
			candidates = append(candidates, name+"\t"+e.Path)
		}
	})
	return candidates
}

// quietDefaultLogger runs fn with the default log.Logger's output redirected
// to io.Discard, then restores it. Needed because some internal paths (e.g.,
// orders discovery) write migration warnings via log.Printf, which would
// corrupt the terminal during tab completion. This helper is intended only for
// one-shot completion paths; it is not safe against concurrent log writer
// mutation.
func quietDefaultLogger(fn func()) {
	orig := log.Default().Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(orig)
	fn()
}

// rigNameCandidates returns rig names with path descriptions as cobra
// completion entries.
func rigNameCandidates(toComplete string) []string {
	var candidates []string
	quietDefaultLogger(func() {
		cityPath, err := resolveCityForCompletionContext(false)
		if err != nil {
			return
		}
		cfg, err := loadCityConfigWithoutBuiltinPackRefreshFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), io.Discard)
		if err != nil {
			return
		}
		resolveRigPaths(cityPath, cfg.Rigs)
		suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
		candidates = make([]string, 0, len(cfg.Rigs))
		for i := range cfg.Rigs {
			name := cfg.Rigs[i].Name
			if !strings.HasPrefix(name, toComplete) {
				continue
			}
			desc := cfg.Rigs[i].Path
			if suspensionstate.EffectiveRigSuspended(suspState, name, cfg.Rigs[i].EffectiveSuspendedOnStart()) {
				desc += " (suspended)"
			}
			candidates = append(candidates, name+"\t"+desc)
		}
	})
	return candidates
}

func resolveCityForCompletion() (string, error) {
	return resolveCityForCompletionContext(true)
}

func resolveCityForCompletionContext(honorRigFlag bool) (string, error) {
	if city := strings.TrimSpace(cityFlag); city != "" {
		return resolveCityFlagValue(city)
	}
	if honorRigFlag {
		if rig := strings.TrimSpace(rigFlag); rig != "" {
			ctx, err := resolveRigForCompletion(rig)
			if err != nil {
				return "", err
			}
			return ctx.CityPath, nil
		}
	}
	if cityPath, ok := resolveExplicitCityPathEnv(); ok {
		return cityPath, nil
	}
	if cityPath, ok := resolveCityPathFromGCDir(); ok {
		return cityPath, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if ctx, ok := lookupRigFromCwd(cwd); ok {
		return ctx.CityPath, nil
	}
	return findCity(cwd)
}

func resolveRigForCompletion(nameOrPath string) (resolvedContext, error) {
	matches, _, err := registeredRigBindingsByName(nameOrPath, false)
	if err != nil {
		return resolvedContext{}, err
	}
	if len(matches) > 0 {
		return resolveRigBindingMatches(nameOrPath, matches)
	}

	abs, err := filepath.Abs(nameOrPath)
	if err != nil {
		return resolvedContext{}, err
	}
	matches, _, err = registeredRigBindingsByPath(abs, false)
	if err != nil {
		return resolvedContext{}, err
	}
	if len(matches) > 0 {
		return resolveRigBindingMatches(abs, matches)
	}
	return resolvedContext{}, os.ErrNotExist
}

func loadOrdersForCompletion() []orders.Order {
	var aa []orders.Order
	quietDefaultLogger(func() {
		cityPath, err := resolveCityForCompletion()
		if err != nil {
			return
		}
		cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
		if err != nil {
			return
		}
		var code int
		aa, code = loadAllOrders(cityPath, cfg, io.Discard, "gc completion")
		if code != 0 {
			aa = nil
		}
	})
	return aa
}

// loadSessionsForCompletion returns session info without triggering the
// slow live-state and attachment checks performed by the non-JSON path of
// `gc session list`. This mirrors the JSON-path of cmdSessionList.
func loadSessionsForCompletion() []session.Info {
	var sessions []session.Info
	quietDefaultLogger(func() {
		cityPath, err := resolveCityForCompletion()
		if err != nil {
			return
		}
		store, err := openCityStoreAt(cityPath)
		if err != nil {
			return
		}
		cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
		if err != nil {
			return
		}
		// loadSessionsForCompletion reads only session-class state (session beads
		// plus the session catalog), so route through the session
		// coordination-class store for relocation-safety.
		sessStore := cliSessionStore(store, cfg, cityPath)
		providerCtx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
		// One union scan via the snapshot loader (front-door migration keeps
		// ListAllSessionBeads out of the CLI) feeds both the provider and the
		// typed listing.
		sessionBeads, err := loadSessionBeadSnapshot(sessStore)
		if err != nil {
			return
		}
		sp, err := newSessionProviderFromContext(providerCtx, sessionBeads)
		if err != nil {
			return
		}
		catalog, err := workerSessionCatalogWithConfig("", sessStore, sp, providerCtx.cfg)
		if err != nil {
			return
		}
		sessions = catalog.ListFromInfos(sessionBeads.OpenInfos(), "", "")
		// loadSessionBeadSnapshot loads unsorted; restore the created-desc order the
		// retired sorted completion feed produced so `gc <cmd> <TAB>` candidates
		// surface newest-first (shared comparator with the session lister).
		sortSessionsCreatedDesc(sessions)
	})
	return sessions
}

// sessionCompletionDescription formats a session as "alias (state)" or
// "template (state)" when no alias is set. Title is omitted to keep the
// zsh completion menu scannable.
func sessionCompletionDescription(s session.Info) string {
	target := s.Alias
	if target == "" {
		target = s.Template
	}
	if target == "" {
		target = "-"
	}
	state := string(s.State)
	if state == "" {
		state = "closed"
	}
	return target + " (" + state + ")"
}

// orderCompletionDescription formats an order as "<type>, <timing>" where
// type is "formula" or "exec" and timing is interval/schedule/event.
func orderCompletionDescription(o orders.Order) string {
	typ := "formula"
	if o.IsExec() {
		typ = "exec"
	}
	timing := o.Interval
	if timing == "" {
		timing = o.Schedule
	}
	if timing == "" {
		timing = o.On
	}
	if timing == "" {
		timing = "-"
	}
	if o.Rig != "" {
		return typ + ", " + timing + " (rig: " + o.Rig + ")"
	}
	return typ + ", " + timing
}
