//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

var unresolvedScanCommands sync.Map

// processScanState holds the accumulated process-table scan scope on the
// CityRuntime; Windows is the only platform that scopes the scan.
type processScanState struct {
	processScanScope            []string
	processScanScopeInitialized bool
}

func configureProcessTableScan(sp runtime.Provider, cfg *config.City) {
	scoped, ok := sp.(interface{ SetScanExecutables([]string) })
	if !ok || cfg == nil {
		return
	}
	scoped.SetScanExecutables(cityRuntimeExecutables(cfg))
}

// refreshProcessTableScan widens the accumulated scan scope from the outgoing
// config current to cover next, and hands it to sp.
func (s *processScanState) refreshProcessTableScan(sp runtime.Provider, current, next *config.City) {
	if !s.processScanScopeInitialized {
		s.processScanScope = cityRuntimeExecutables(current)
		s.processScanScopeInitialized = true
	}
	nextScope := cityRuntimeExecutables(next)
	if s.processScanScope == nil || nextScope == nil {
		s.processScanScope = nil
	} else {
		// Earlier sessions may outlive both their config entry and their provider.
		s.processScanScope = append(s.processScanScope, nextScope...)
		slices.Sort(s.processScanScope)
		s.processScanScope = slices.Compact(s.processScanScope)
	}
	if scoped, ok := sp.(interface{ SetScanExecutables([]string) }); ok {
		scoped.SetScanExecutables(slices.Clone(s.processScanScope))
	}
}

func cityRuntimeExecutables(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	names := []string{"gc", "bd", "cmd", "pwsh", "powershell"}
	if self, err := os.Executable(); err == nil {
		names = append(names, self)
	}
	resolved := true
	addCommand := func(source, command string) {
		if strings.TrimSpace(command) == "" {
			return
		}
		programs := runtime.ShellCommandPrograms(command)
		if len(programs) == 0 {
			resolved = false
			key := struct{ source, command string }{source, command}
			if _, loaded := unresolvedScanCommands.LoadOrStore(key, struct{}{}); !loaded {
				log.Printf("runtime scan: unresolved %s; retaining strict process inspection; use a literal executable command", source)
			}
		}
		names = append(names, programs...)
	}
	addProvider := func(source string, p config.ProviderSpec) {
		addCommand(source+".command", p.Command)
		addCommand(source+".acp_command", p.ACPCommand)
		names = append(names, p.PathCheck)
		names = append(names, p.ProcessNames...)
		addCommand(source+".resume_command", p.ResumeCommand)
	}
	for name, p := range config.BuiltinProviders() {
		addProvider(fmt.Sprintf("builtin_providers[%q]", name), p)
	}
	for name, p := range cfg.Providers {
		addProvider(fmt.Sprintf("providers[%q]", name), p)
	}
	for name, p := range cfg.ResolvedProviders {
		source := fmt.Sprintf("resolved_providers[%q]", name)
		addCommand(source+".command", p.Command)
		addCommand(source+".acp_command", p.ACPCommand)
		names = append(names, p.ProcessNames...)
		addCommand(source+".resume_command", p.ResumeCommand)
	}
	addCommand("workspace.start_command", cfg.Workspace.StartCommand)
	for i, agent := range cfg.Agents {
		source := fmt.Sprintf("agents[%d]", i)
		addCommand(source+".start_command", agent.StartCommand)
		addCommand(source+".resume_command", agent.ResumeCommand)
		names = append(names, agent.ProcessNames...)
	}
	for _, rt := range cfg.Runtimes {
		names = append(names, rt.Command)
	}
	if !resolved {
		return nil
	}
	names = slices.DeleteFunc(names, func(name string) bool { return strings.TrimSpace(name) == "" })
	slices.Sort(names)
	return slices.Compact(names)
}
