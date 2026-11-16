//go:build windows

package config

import (
	"sync"

	"github.com/gastownhall/gascity/internal/genshell"
)

// generatedReadKinds are the query kinds whose default is a read program: it
// asks bd and gc for beads and writes nothing.
var generatedReadKinds = map[queryKind]bool{
	queryWork:               true,
	queryAssignedInProgress: true,
	queryAssignedReady:      true,
	queryRoutedPool:         true,
	queryPoolDemand:         true,
}

// generatedHookKinds are the query kinds whose default is a recovery hook: a
// script run whole, which writes through bd.
var generatedHookKinds = map[queryKind]bool{
	queryOnDeath: true,
	queryOnBoot:  true,
}

const generatedProgramRegistryLimit = 4096

// generatedPrograms holds exact commands this process generated. It is bounded
// so stale commands from prior config snapshots cannot accumulate forever.
var generatedPrograms = newGeneratedProgramRegistry(generatedProgramRegistryLimit)

// noteGeneratedQuery records command, the default this process built for kind:
// the exact read program, or a recovery hook whole.
func noteGeneratedQuery(kind queryKind, command string) {
	switch {
	case generatedReadKinds[kind]:
		if _, err := genshell.Parse(command); err != nil {
			return
		}
		generatedPrograms.Store(command)
	case generatedHookKinds[kind]:
		generatedPrograms.Store(command)
	}
}

// GeneratedProgram returns command as a program for the embedded interpreter
// when it is one this process generated exactly: a read program, which is a
// default work query in any of its forms or a default pool-demand check; or a
// default on_boot or on_death recovery hook, run as `sh -c command` runs it. A
// command that merely resembles one is not.
func GeneratedProgram(command string) (genshell.Program, bool) {
	if !generatedPrograms.Load(command) {
		return genshell.Program{}, false
	}
	prog, err := genshell.Parse(command)
	if err == nil {
		return prog, true
	}
	return genshell.Program{Script: command}, true
}

type generatedProgramRegistry struct {
	mu      sync.Mutex
	limit   int
	entries map[string]struct{}
	order   []string
}

func newGeneratedProgramRegistry(limit int) *generatedProgramRegistry {
	return &generatedProgramRegistry{
		limit:   limit,
		entries: map[string]struct{}{},
	}
}

func (r *generatedProgramRegistry) Store(command string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[command]; ok {
		for i, old := range r.order {
			if old == command {
				copy(r.order[i:], r.order[i+1:])
				r.order[len(r.order)-1] = command
				return
			}
		}
		return
	}
	r.entries[command] = struct{}{}
	r.order = append(r.order, command)
	for len(r.order) > r.limit {
		old := r.order[0]
		copy(r.order, r.order[1:])
		r.order = r.order[:len(r.order)-1]
		delete(r.entries, old)
	}
}

func (r *generatedProgramRegistry) Load(command string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[command]
	return ok
}
