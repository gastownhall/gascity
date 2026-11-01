package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

const providerUnresolvedTemplateSample = 8

// ProviderUnresolvedGroup names a provider executable and the distinct templates
// it prevents from starting. Templates is a bounded, deterministically sorted sample.
type ProviderUnresolvedGroup struct {
	Provider         string
	Command          string
	DroppedTemplates int
	Templates        []string
}

type unresolvedProviderTemplates struct {
	provider, command string
	templates         map[string]bool
}

// Shared by value copies of agentBuildParams and concurrent pool resolution.
type providerUnresolvedSet struct {
	mu     sync.Mutex
	groups map[string]*unresolvedProviderTemplates
}

func newProviderUnresolvedSet() *providerUnresolvedSet { return &providerUnresolvedSet{} }

func (s *providerUnresolvedSet) record(template string, err error) bool {
	if s == nil {
		return false
	}
	var missing *config.ProviderNotInPATHError
	if !errors.As(err, &missing) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups == nil {
		s.groups = make(map[string]*unresolvedProviderTemplates)
	}
	key := missing.Provider + "\x00" + missing.Command
	g := s.groups[key]
	if g == nil {
		g = &unresolvedProviderTemplates{provider: missing.Provider, command: missing.Command, templates: make(map[string]bool)}
		s.groups[key] = g
	}
	g.templates[template] = true
	return true
}

func (s *providerUnresolvedSet) snapshot() []ProviderUnresolvedGroup {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.groups) == 0 {
		return nil
	}
	out := make([]ProviderUnresolvedGroup, 0, len(s.groups))
	for _, g := range s.groups {
		templates := make([]string, 0, len(g.templates))
		for name := range g.templates {
			if name != "" {
				templates = append(templates, name)
			}
		}
		sort.Strings(templates)
		out = append(out, ProviderUnresolvedGroup{Provider: g.provider, Command: g.command, DroppedTemplates: len(g.templates), Templates: templates[:min(len(templates), providerUnresolvedTemplateSample)]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Command < out[j].Command
	})
	return out
}

func providerUnresolvedProviderNames(groups []ProviderUnresolvedGroup) []string {
	var names []string
	for _, g := range groups {
		if len(names) == 0 || names[len(names)-1] != g.Provider {
			names = append(names, g.Provider)
		}
	}
	return names
}

// Each owning reconcile goroutine keeps its own episode memory.
type providerUnresolvedAlerts struct{ live map[string]bool }

func (a *providerUnresolvedAlerts) report(groups []ProviderUnresolvedGroup, desiredCount *int, mode string, stderr io.Writer, record func(events.Event), now time.Time) {
	live := make(map[string]bool, len(groups))
	for _, g := range groups {
		key := g.Provider + "\x00" + g.Command
		live[key] = true
		if a.live[key] {
			continue
		}
		message := fmt.Sprintf("provider %q command %q is not on PATH: cannot build %d template(s) (%s). Install the provider CLI or correct [providers.%s].command, then run gc doctor and gc reload --soft.", g.Provider, g.Command, g.DroppedTemplates, strings.Join(g.Templates, ", "), g.Provider)
		if desiredCount != nil {
			message += fmt.Sprintf(" desired_session_count=%d", *desiredCount)
		}
		if stderr != nil {
			fmt.Fprintln(stderr, message)
		} //nolint:errcheck // best-effort diagnostic
		if record != nil {
			record(events.Event{Type: events.ProviderUnresolved, Ts: now.UTC(), Actor: "gc", Subject: g.Provider, Message: message, Payload: events.ProviderUnresolvedPayloadJSON(events.ProviderUnresolvedPayload{
				Provider: g.Provider, Command: g.Command, DroppedTemplates: g.DroppedTemplates, Templates: g.Templates, DesiredSessionCount: desiredCount, Reconciler: mode,
			})})
		}
	}
	a.live = live
}

func (cr *CityRuntime) reportUnresolvedProviders(result DesiredStateResult) {
	if cr == nil {
		return
	}
	var record func(events.Event)
	if cr.rec != nil {
		record = cr.rec.Record
	}
	count := len(result.State)
	cr.providerUnresolvedAlerts.report(result.ProviderUnresolved, &count, "legacy", cr.stderr, record, time.Now())
}

// Allocation transport refusals explain missing new seats; failed template
// resolutions also explain existing/manual sessions that cannot wake.
func unresolvedProvidersForPass(w *World, a *allocDecision) []ProviderUnresolvedGroup {
	set := newProviderUnresolvedSet()
	for _, decision := range a.Trace {
		if decision.Reason == "ineligible:"+gateTransport {
			set.record(decision.Template, w.TransportErrors[decision.Template])
		}
	}
	if w.Templates != nil {
		for key, resolution := range w.Templates.entries {
			set.record(key.Template, resolution.Err)
		}
	}
	return set.snapshot()
}
