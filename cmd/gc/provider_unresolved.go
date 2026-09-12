package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// providerUnresolvedTemplateSample bounds how many template identities one
// group carries. A city with a hundred agents on one uninstalled provider
// should produce a readable trace record and a readable log line, not a
// hundred-entry list repeated on every reconcile tick; the count is reported
// separately so nothing is lost by truncating the names.
const providerUnresolvedTemplateSample = 8

// ProviderUnresolvedGroup reports one provider whose command did not resolve
// on PATH during a desired-state build, together with what that cost.
//
// This is the structured record ob-woag found missing. When a template's
// provider binary is absent the controller drops every session using it from
// the desired set; before this type the only trace of that was a line on the
// supervisor's stderr, so `desired_session_count` went to 0 with the word
// "provider" appearing nowhere in the reconciler trace and nothing at all in
// .gc/events.jsonl.
type ProviderUnresolvedGroup struct {
	// Provider is the configured provider reference that failed.
	Provider string
	// Command is the binary lookPath could not find.
	Command string
	// DroppedTemplates counts every template resolution this provider
	// failed during the build — the size of the hole in the desired set.
	DroppedTemplates int
	// Templates samples the affected identities, sorted and capped at
	// providerUnresolvedTemplateSample.
	Templates []string
}

// providerUnresolvedSet accumulates PATH-resolution failures across one
// desired-state build, grouped by provider so a single uninstalled binary
// shared by many agents reports once.
//
// The set is held by POINTER on agentBuildParams because several build paths
// take a value copy of the params struct (resolveTemplateForSessionBeadInfo
// does `local := *bp` to scope a beadNames override). A slice field on the
// struct would collect failures into whichever copy happened to see them and
// lose them at the seam; the shared pointer makes the accumulation
// copy-transparent. The mutex is for the pool-evaluation fan-out, which
// resolves templates from more than one goroutine.
type providerUnresolvedSet struct {
	mu     sync.Mutex
	groups map[string]*ProviderUnresolvedGroup
}

func newProviderUnresolvedSet() *providerUnresolvedSet {
	return &providerUnresolvedSet{groups: map[string]*ProviderUnresolvedGroup{}}
}

// record adds one failure if err is (or wraps) a provider-not-in-PATH error,
// and reports whether it did. Any other resolution error is somebody else's
// diagnostic: unknown-provider and bad-option faults are config errors the
// config-valid / config-refs / provider-catalog checks already name, and
// re-reporting them here would dilute the one signal this set exists to carry.
// Nil-receiver-safe so build paths without an accumulator (focused unit
// fixtures) cost one branch.
func (s *providerUnresolvedSet) record(template string, err error) bool {
	if s == nil || err == nil {
		return false
	}
	var notInPath *config.ProviderNotInPATHError
	if !errors.As(err, &notInPath) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := notInPath.Provider + "\x00" + notInPath.Command
	g, ok := s.groups[key]
	if !ok {
		g = &ProviderUnresolvedGroup{Provider: notInPath.Provider, Command: notInPath.Command}
		s.groups[key] = g
	}
	g.DroppedTemplates++
	if template != "" && len(g.Templates) < providerUnresolvedTemplateSample && !slices.Contains(g.Templates, template) {
		g.Templates = append(g.Templates, template)
	}
	return true
}

// snapshot returns the accumulated groups in a stable order, safe to hand to
// the caller (templates are copied, not aliased).
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
		templates := append([]string(nil), g.Templates...)
		sort.Strings(templates)
		out = append(out, ProviderUnresolvedGroup{
			Provider:         g.Provider,
			Command:          g.Command,
			DroppedTemplates: g.DroppedTemplates,
			Templates:        templates,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Command < out[j].Command
	})
	return out
}

// providerUnresolvedProviderNames lists the failing provider references in
// stable order, for trace fields that want the names without the per-group
// detail.
func providerUnresolvedProviderNames(groups []ProviderUnresolvedGroup) []string {
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		if !slices.Contains(names, g.Provider) {
			names = append(names, g.Provider)
		}
	}
	return names
}

// key is the group's stable identity, used by the controller's
// once-per-episode alert latch.
func (g ProviderUnresolvedGroup) key() string { return g.Provider + "\x00" + g.Command }

// reportUnresolvedProviders turns a desired-state build's provider-PATH
// failures into the two operator-visible records ob-woag found missing: a
// controller log line and a .gc/events.jsonl entry. The reconciler trace is
// written at the build site itself (buildDesiredStateWithSessionBeadsAt); this
// is the half that reaches someone who is not already reading a trace.
//
// One alert per episode, not per tick. The desired-state build re-derives the
// same failure every time it runs and the cached demand snapshot replays the
// result in between, so the unlatched version of this would write a line and
// an event on every controller tick for as long as the binary stays
// uninstalled. Recovery clears the latch, so a provider that is installed and
// then removed again alerts a second time.
func (cr *CityRuntime) reportUnresolvedProviders(result DesiredStateResult) {
	if cr == nil {
		return
	}
	if len(result.ProviderUnresolved) == 0 && len(cr.providerUnresolvedAlerted) == 0 {
		return
	}
	if cr.providerUnresolvedAlerted == nil {
		cr.providerUnresolvedAlerted = map[string]bool{}
	}
	live := make(map[string]bool, len(result.ProviderUnresolved))
	for _, group := range result.ProviderUnresolved {
		key := group.key()
		live[key] = true
		if cr.providerUnresolvedAlerted[key] {
			continue
		}
		cr.providerUnresolvedAlerted[key] = true
		cr.emitProviderUnresolvedAlert(group, len(result.State))
	}
	for key := range cr.providerUnresolvedAlerted {
		if !live[key] {
			delete(cr.providerUnresolvedAlerted, key)
		}
	}
}

// emitProviderUnresolvedAlert writes one episode's alert to the controller log
// and records it as a ProviderUnresolved event.
//
// The message carries desired_session_count deliberately: the reported symptom
// was a city that started clean and stayed empty, and the number that made it
// look normal is the number that has to appear beside its cause.
func (cr *CityRuntime) emitProviderUnresolvedAlert(group ProviderUnresolvedGroup, desiredSessionCount int) {
	templates := strings.Join(group.Templates, ", ")
	if templates == "" {
		templates = "(no template named)"
	}
	msg := fmt.Sprintf(
		"provider %q command %q is not on PATH: %d template(s) dropped from the desired state (%s); "+
			"desired_session_count=%d. Install the provider CLI or repoint [providers.%s].command, then `gc doctor` (check provider-path) to confirm.",
		group.Provider, group.Command, group.DroppedTemplates, templates, desiredSessionCount, group.Provider,
	)
	if cr.stderr != nil {
		fmt.Fprintf(cr.stderr, "%s: %s\n", cr.logPrefix, msg) //nolint:errcheck // best-effort stderr
	}
	if cr.rec == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{ //nolint:errcheck // map of scalars and strings cannot fail
		"provider":              group.Provider,
		"command":               group.Command,
		"dropped_templates":     group.DroppedTemplates,
		"templates":             group.Templates,
		"desired_session_count": desiredSessionCount,
	})
	cr.rec.Record(events.Event{
		Type:    events.ProviderUnresolved,
		Ts:      time.Now().UTC(),
		Actor:   "gc",
		Subject: group.Provider,
		Message: msg,
		Payload: payload,
	})
}
