package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionLifecycleShadowInput is the complete effect-free input needed by the
// first shadow action family. RuntimeObserved distinguishes a known-dead
// session from a failed or skipped runtime probe.
type sessionLifecycleShadowInput struct {
	Info              session.Info
	RuntimeObserved   bool
	RuntimeAlive      bool
	ObservedAt        time.Time
	StartupTimeout    time.Duration
	RollbackAvailable bool
}

// sessionLifecycleShadowProjection is one immutable generation of status-plan
// inputs keyed by the durable session ID.
type sessionLifecycleShadowProjection struct {
	generation uint64
	byID       map[string]sessionLifecycleShadowInput
	keys       []string
}

func newSessionLifecycleShadowProjection(generation uint64, inputs []sessionLifecycleShadowInput) (*sessionLifecycleShadowProjection, error) {
	if generation == 0 {
		return nil, fmt.Errorf("building lifecycle shadow projection: generation is zero")
	}
	projection := &sessionLifecycleShadowProjection{
		generation: generation,
		byID:       make(map[string]sessionLifecycleShadowInput, len(inputs)),
		keys:       make([]string, 0, len(inputs)),
	}
	for _, input := range inputs {
		id := input.Info.ID
		if id == "" || strings.TrimSpace(id) != id {
			return nil, fmt.Errorf("building lifecycle shadow projection: session id %q is not canonical", id)
		}
		if _, exists := projection.byID[id]; exists {
			return nil, fmt.Errorf("building lifecycle shadow projection: duplicate session id %q", id)
		}
		projection.byID[id] = cloneSessionLifecycleShadowInput(input)
		projection.keys = append(projection.keys, id)
	}
	sort.Strings(projection.keys)
	return projection, nil
}

func (p *sessionLifecycleShadowProjection) Generation() uint64 {
	if p == nil {
		return 0
	}
	return p.generation
}

// Read returns a detached exact-key input. It never scans the projection.
func (p *sessionLifecycleShadowProjection) Read(id string) (sessionLifecycleShadowInput, bool) {
	if p == nil {
		return sessionLifecycleShadowInput{}, false
	}
	input, ok := p.byID[id]
	if !ok {
		return sessionLifecycleShadowInput{}, false
	}
	return cloneSessionLifecycleShadowInput(input), true
}

// Keys returns the deterministic full-census order used only for startup and
// anti-entropy. Normal reconciliation calls Read with one exact key.
func (p *sessionLifecycleShadowProjection) Keys() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.keys...)
}

func cloneSessionLifecycleShadowInput(input sessionLifecycleShadowInput) sessionLifecycleShadowInput {
	cloned := input
	cloned.Info.Labels = append([]string(nil), input.Info.Labels...)
	cloned.Info.AliasHistory = append([]string(nil), input.Info.AliasHistory...)
	return cloned
}

type sessionLifecycleStatusOutcome uint8

const (
	sessionLifecycleStatusUnknown sessionLifecycleStatusOutcome = iota
	sessionLifecycleStatusNoop
	sessionLifecycleStatusHeal
	sessionLifecycleStatusPark
)

type sessionLifecycleStatusReason string

const (
	sessionLifecycleStatusReasonUnknown            sessionLifecycleStatusReason = ""
	sessionLifecycleStatusReasonConverged          sessionLifecycleStatusReason = "converged"
	sessionLifecycleStatusReasonHeal               sessionLifecycleStatusReason = "heal"
	sessionLifecycleStatusReasonTerminal           sessionLifecycleStatusReason = "terminal"
	sessionLifecycleStatusReasonRuntimeUnknown     sessionLifecycleStatusReason = "runtime_unknown"
	sessionLifecycleStatusReasonObservationUnknown sessionLifecycleStatusReason = "observation_unknown"
	sessionLifecycleStatusReasonInvalidInput       sessionLifecycleStatusReason = "invalid_input"
)

// sessionLifecycleStatusPlan describes at most one legacy-compatible metadata
// heal. It contains no writer or runtime capability.
type sessionLifecycleStatusPlan struct {
	SessionID string
	Outcome   sessionLifecycleStatusOutcome
	Reason    sessionLifecycleStatusReason
	Patch     session.MetadataPatch
}

func planSessionLifecycleStatus(input sessionLifecycleShadowInput) sessionLifecycleStatusPlan {
	plan := sessionLifecycleStatusPlan{SessionID: input.Info.ID}
	if input.Info.ID == "" || strings.TrimSpace(input.Info.ID) != input.Info.ID {
		plan.Outcome = sessionLifecycleStatusPark
		plan.Reason = sessionLifecycleStatusReasonInvalidInput
		return plan
	}
	if input.Info.Closed {
		plan.Outcome = sessionLifecycleStatusNoop
		plan.Reason = sessionLifecycleStatusReasonTerminal
		return plan
	}
	if !input.RuntimeObserved {
		plan.Outcome = sessionLifecycleStatusPark
		plan.Reason = sessionLifecycleStatusReasonRuntimeUnknown
		return plan
	}
	if input.ObservedAt.IsZero() {
		plan.Outcome = sessionLifecycleStatusPark
		plan.Reason = sessionLifecycleStatusReasonObservationUnknown
		return plan
	}

	patch := healStatePatchWithRollbackInfo(
		input.Info,
		input.RuntimeAlive,
		input.RuntimeObserved,
		&clock.Fake{Time: input.ObservedAt},
		input.StartupTimeout,
		input.RollbackAvailable,
	)
	if len(patch) == 0 {
		plan.Outcome = sessionLifecycleStatusNoop
		plan.Reason = sessionLifecycleStatusReasonConverged
		return plan
	}
	plan.Outcome = sessionLifecycleStatusHeal
	plan.Reason = sessionLifecycleStatusReasonHeal
	plan.Patch = cloneSessionLifecycleStatusPatch(patch)
	return plan
}

func cloneSessionLifecycleStatusPatch(patch map[string]string) session.MetadataPatch {
	if len(patch) == 0 {
		return nil
	}
	cloned := make(session.MetadataPatch, len(patch))
	for key, value := range patch {
		cloned[key] = value
	}
	return cloned
}
