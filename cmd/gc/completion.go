package main

import (
	"io"
	"log"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
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

// completeOrderNames completes order names for commands whose first
// positional is an order name.
func completeOrderNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var aa []orders.Order
	quietDefaultLogger(func() {
		aa, _ = loadOrders(io.Discard, "gc completion")
	})
	candidates := make([]string, 0, len(aa))
	for _, o := range aa {
		if !strings.HasPrefix(o.Name, toComplete) {
			continue
		}
		candidates = append(candidates, o.Name+"\t"+orderCompletionDescription(o))
	}
	return candidates, cobra.ShellCompDirectiveNoFileComp
}

// quietDefaultLogger runs fn with the default log.Logger's output redirected
// to io.Discard, then restores it. Needed because some internal paths (e.g.,
// orders discovery) write migration warnings via log.Printf, which would
// corrupt the terminal during tab completion.
func quietDefaultLogger(fn func()) {
	orig := log.Default().Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(orig)
	fn()
}

// rigNameCandidates returns rig names (with path descriptions) as cobra
// completion entries. Extracted so that both the positional-arg completer
// and the --rig flag completer can share it.
func rigNameCandidates(toComplete string) []string {
	cityPath, err := resolveCity()
	if err != nil {
		return nil
	}
	cfg, err := loadCityConfigFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), io.Discard)
	if err != nil {
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	candidates := make([]string, 0, len(cfg.Rigs)+1)
	// HQ rig first.
	cityName := cfg.EffectiveCityName()
	if strings.HasPrefix(cityName, toComplete) {
		candidates = append(candidates, cityName+"\tHQ — "+cityPath)
	}
	for i := range cfg.Rigs {
		name := cfg.Rigs[i].Name
		if !strings.HasPrefix(name, toComplete) {
			continue
		}
		desc := cfg.Rigs[i].Path
		if cfg.Rigs[i].Suspended {
			desc += " (suspended)"
		}
		candidates = append(candidates, name+"\t"+desc)
	}
	return candidates
}

// loadSessionsForCompletion returns session info without triggering the
// slow live-state and attachment checks performed by the non-JSON path of
// `gc session list`. This mirrors the JSON-path of cmdSessionList.
func loadSessionsForCompletion() []session.Info {
	cityPath, err := resolveCity()
	if err != nil {
		return nil
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	providerCtx := loadSessionProviderContext()
	allSessionBeads, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return nil
	}
	sessionBeads := newSessionBeadSnapshot(allSessionBeads)
	sp := newSessionProviderFromContext(providerCtx, sessionBeads)
	catalog, err := workerSessionCatalogWithConfig("", store, sp, providerCtx.cfg)
	if err != nil {
		return nil
	}
	return catalog.ListFullFromBeads(allSessionBeads, "", "").Sessions
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
	return typ + ", " + timing
}
