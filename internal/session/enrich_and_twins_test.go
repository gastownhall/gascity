package session

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// legacyEnrichFromBead reproduces the pre-refactor infoFromBead overlay reading
// directly from the raw bead (via transportForBead). It is the independent
// oracle for EnrichInfo: if transportForInfo ever diverges from transportForBead,
// or the overlay logic drifts, the equivalence assertion below catches it. (This
// is NOT a copy of EnrichInfo's body — it reads the BEAD, EnrichInfo reads the
// INFO.)
func legacyEnrichFromBead(m *Manager, b beads.Bead) Info {
	info := infoFromPersistedBead(b)
	sessName := info.SessionName
	if !info.Closed {
		transport, _ := m.transportForBead(b, sessName)
		info.Transport = transport
		_ = m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
		if m.sp != nil && info.State == StateActive && !m.sp.IsRunning(sessName) {
			info.State = StateAsleep
		}
	}
	if info.State == StateActive && m.sp != nil {
		info.Attached = m.sp.IsAttached(sessName)
		if t, err := m.sp.GetLastActivity(sessName); err == nil && !t.IsZero() {
			info.LastActive = t
		}
	}
	return info
}

// TestEnrichInfoMatchesBeadOverlay is the identity-refactor oracle for EnrichInfo:
// EnrichInfo(infoFromPersistedBead(b)) must equal the legacy bead-reading overlay
// across a corpus that exercises every overlay branch (transport metadata fallback,
// mcp→acp, pending-create resolver, running/attached enrichment, stale-active
// downgrade, closed skip). Explicit outcome assertions guard against a vacuous pass.
func TestEnrichInfoMatchesBeadOverlay(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-running", runtime.Config{}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	fake.SetAttached("s-running", true)
	activeAt := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	fake.SetActivity("s-running", activeAt)

	m := NewManagerWithOptions(beads.NewMemStore(), fake, WithCityPath(""), WithTransportResolver(func(template, _ string) string {
		if template == "pending-tmpl" {
			return "tmux"
		}
		return ""
	}))

	mk := func(id, status string, meta map[string]string) beads.Bead {
		return beads.Bead{ID: id, Type: BeadType, Status: status, Labels: []string{LabelSession}, Metadata: meta}
	}
	corpus := map[string]beads.Bead{
		"running-active": mk("s-ra", "open", map[string]string{"session_name": "s-running", "state": "active", "provider": "claude"}),
		"stale-active":   mk("s-sa", "open", map[string]string{"session_name": "s-gone", "state": "active", "provider": "claude"}),
		"asleep":         mk("s-as", "open", map[string]string{"session_name": "s-running", "state": "asleep", "provider": "claude"}),
		"closed-raw":     mk("s-cl", "closed", map[string]string{"session_name": "s-running", "state": "active", "provider": "claude"}),
		"acp-provider":   mk("s-acp", "open", map[string]string{"session_name": "s-acp", "state": "asleep", "provider": "acp"}),
		"mcp-identity":   mk("s-mcp", "open", map[string]string{"session_name": "s-mcp", "state": "asleep", MCPIdentityMetadataKey: "mcp-x"}),
		"pending-create": mk("s-pc", "open", map[string]string{"session_name": "s-pc", "state": "creating", "pending_create_claim": "true", "template": "pending-tmpl"}),
	}

	for name, b := range corpus {
		want := legacyEnrichFromBead(m, b)
		got := m.EnrichInfo(infoFromPersistedBead(b))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: EnrichInfo diverged from the bead overlay\n got=%+v\nwant=%+v", name, got, want)
		}
		// infoFromBead must be exactly the composition (the refactor identity).
		if fb := m.infoFromBead(b); !reflect.DeepEqual(fb, got) {
			t.Errorf("%s: infoFromBead != EnrichInfo(infoFromPersistedBead(b))\n infoFromBead=%+v\n composed=%+v", name, fb, got)
		}
	}

	// Non-vacuous outcome checks.
	if got := m.EnrichInfo(infoFromPersistedBead(corpus["running-active"])); !got.Attached || !got.LastActive.Equal(activeAt) || got.State != StateActive {
		t.Errorf("running-active: attached=%v lastActive=%v state=%q, want attached, %v, active", got.Attached, got.LastActive, got.State, activeAt)
	}
	if got := m.EnrichInfo(infoFromPersistedBead(corpus["stale-active"])); got.State != StateAsleep {
		t.Errorf("stale-active: state=%q, want asleep (stale-active downgrade)", got.State)
	}
	if got := m.EnrichInfo(infoFromPersistedBead(corpus["mcp-identity"])); got.Transport != "acp" {
		t.Errorf("mcp-identity: transport=%q, want acp", got.Transport)
	}
	if got := m.EnrichInfo(infoFromPersistedBead(corpus["pending-create"])); got.Transport != "tmux" {
		t.Errorf("pending-create: transport=%q, want tmux (resolver)", got.Transport)
	}
	if got := m.EnrichInfo(infoFromPersistedBead(corpus["closed-raw"])); got.Attached || got.State != "" {
		t.Errorf("closed: attached=%v state=%q, want not-attached and blanked state", got.Attached, got.State)
	}

	// EnrichInfos applies the same overlay element-wise.
	infos := []Info{infoFromPersistedBead(corpus["running-active"]), infoFromPersistedBead(corpus["stale-active"])}
	enriched := m.EnrichInfos(infos)
	if len(enriched) != 2 || !enriched[0].Attached || enriched[1].State != StateAsleep {
		t.Errorf("EnrichInfos = %+v, want [attached, asleep]", enriched)
	}
}

// TestSessionMatchesFiltersInfoEquivalence pins sessionMatchesFiltersInfo against
// the bead form across an open/closed corpus (including the closed-bead-with-raw-
// active-state trap) and every state/template filter shape, so the Info form is
// byte-identical. The awake→active normalization and the "active," empty-member
// (legacy empty-state) semantics are exercised.
func TestSessionMatchesFiltersInfoEquivalence(t *testing.T) {
	mk := func(status string, meta map[string]string) beads.Bead {
		return beads.Bead{ID: "s", Type: BeadType, Status: status, Labels: []string{LabelSession}, Metadata: meta}
	}
	corpus := []beads.Bead{
		mk("open", map[string]string{"state": "active", "template": "worker"}),
		mk("open", map[string]string{"state": "asleep", "template": "worker"}),
		mk("open", map[string]string{"state": "awake", "template": "w2"}),        // normalizes to active
		mk("open", map[string]string{"template": ""}),                            // legacy empty state
		mk("closed", map[string]string{"state": "active", "template": "worker"}), // the trap: raw state active but closed
		mk("closed", map[string]string{"template": ""}),
	}
	stateFilters := []string{"", "all", "active", "asleep", "open", "closed", "active,", "active,asleep"}
	templateFilters := []string{"", "worker", "w2", "none"}

	for _, b := range corpus {
		info := infoFromPersistedBead(b)
		for _, sf := range stateFilters {
			for _, tf := range templateFilters {
				want := sessionMatchesFilters(b, sf, tf)
				got := sessionMatchesFiltersInfo(info, sf, tf)
				if got != want {
					t.Errorf("bead(status=%s meta=%v) stateFilter=%q templateFilter=%q: Info form=%v, bead form=%v",
						b.Status, b.Metadata, sf, tf, got, want)
				}
			}
		}
	}

	// Exotic (out-of-band) status row: the SDK invariant is binary open/closed,
	// but Info carries no raw Status — the twin has only Closed (== status=="closed").
	// So for a non-open, non-closed status the twin's `sf=="open"` (!Closed) is
	// WIDER than the bead form's exact `b.Status=="open"`. Pin that documented
	// delta on the "open" filter, and pin that the forms still AGREE for every
	// other filter (status not consulted as 'open'), so the delta is exactly
	// scoped — not a general divergence.
	exotic := beads.Bead{ID: "s", Type: BeadType, Status: "archived", Labels: []string{LabelSession}, Metadata: map[string]string{"state": "active"}}
	exoticInfo := infoFromPersistedBead(exotic)
	if got, want := sessionMatchesFiltersInfo(exoticInfo, "open", ""), sessionMatchesFilters(exotic, "open", ""); !got || want {
		t.Errorf("exotic-status open-filter: Info form=%v bead form=%v, want the documented delta (twin matches on !Closed, bead requires status==open)", got, want)
	}
	for _, sf := range []string{"", "all", "active", "asleep", "closed", "active,"} {
		if sessionMatchesFiltersInfo(exoticInfo, sf, "") != sessionMatchesFilters(exotic, sf, "") {
			t.Errorf("exotic-status stateFilter=%q: forms diverge OUTSIDE the documented 'open' delta", sf)
		}
	}
}

// TestMailboxInfoTwinsMatchBeadForms pins the three Info-taking mailbox codecs
// against their bead forms across alias-history shapes: alias precedence, id
// fallback, session_name last-resort vs unconditional-append, history dedupe/
// normalization, and the empty-everything case.
func TestMailboxInfoTwinsMatchBeadForms(t *testing.T) {
	beadShapes := []beads.Bead{
		{ID: "sess-1", Metadata: map[string]string{"alias": "mayor", "session_name": "sn-1"}},
		{ID: "sess-1", Metadata: map[string]string{"session_name": "sn-1"}},
		{Metadata: map[string]string{"session_name": "sn-1"}}, // no id: session_name fallback
		{ID: "sess-1", Metadata: map[string]string{"alias": "  mayor  "}},
		{ID: "sess-1", Metadata: map[string]string{"alias": "mayor", "alias_history": "deacon,mayor,polecat", "session_name": "sn-1"}},
		{ID: "sess-1", Metadata: map[string]string{"alias_history": " deacon , deacon , polecat ", "session_name": "sn-1"}},
		{ID: "sess-1", Metadata: map[string]string{}},
		{Metadata: map[string]string{}}, // empty everything
	}
	for i, b := range beadShapes {
		info := infoFromPersistedBead(b)
		if got, want := MailboxAddressFromInfo(info), MailboxAddress(b); got != want {
			t.Errorf("shape %d: MailboxAddressFromInfo=%q, MailboxAddress=%q", i, got, want)
		}
		if got, want := MailboxAddressesFromInfo(info), MailboxAddresses(b); !reflect.DeepEqual(got, want) {
			t.Errorf("shape %d: MailboxAddressesFromInfo=%#v, MailboxAddresses=%#v", i, got, want)
		}
		if got, want := MailboxAddressesIncludingRuntimeNameFromInfo(info), MailboxAddressesIncludingRuntimeName(b); !reflect.DeepEqual(got, want) {
			t.Errorf("shape %d: ...IncludingRuntimeNameFromInfo=%#v, ...IncludingRuntimeName=%#v", i, got, want)
		}
	}
}
