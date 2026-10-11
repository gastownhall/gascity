package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionResponseFromBead is the pre-S2 response builder: it constructs the
// session response directly from the raw *beads.Bead. It is retained only as the
// golden oracle for the wire-equivalence test below, proving the S2 refactor
// (building from session.Info + session.PersistedResponse) is byte-identical.
func sessionResponseFromBead(info session.Info, b *beads.Bead, cfg *config.City, sp runtime.Provider, hasDeferredQueue bool) sessionResponse {
	r := sessionToResponse(info, cfg)
	if b != nil && cfg != nil {
		agentTemplateOK := true
		agent, agentFound := findAgent(cfg, info.Template)
		if session.UseAgentTemplateForProviderResolution(legacySessionKind(b.Metadata), b.Metadata, info.Provider, agent.Provider, agentFound) {
			r.Kind = "agent"
			agentTemplateOK = agentFound
		} else {
			r.Kind = "provider"
		}
		if agentTemplateOK {
			rp, _ := resolveProviderForSessionOptions(info, b.Metadata, cfg)
			if rp != nil {
				merged := make(map[string]string, len(rp.EffectiveDefaults))
				for k, v := range rp.EffectiveDefaults {
					merged[k] = v
				}
				hasOverrides := false
				if overrides, err := session.ParseTemplateOverrides(b.Metadata); err == nil {
					for k, v := range overrides {
						if k != "initial_message" {
							merged[k] = v
							hasOverrides = true
						}
					}
				}
				if len(rp.EffectiveDefaults) > 0 || hasOverrides {
					r.Options = merged
				}
			}
		}
	}
	if b == nil || info.Closed {
		return r
	}
	var isRunning func(string) bool
	if sp != nil {
		isRunning = sp.IsRunning
	}
	r.Reason = session.LifecycleDisplayReasonWithLiveness(b.Status, b.Metadata, time.Now().UTC(), info.SessionName, isRunning)
	r.ConfiguredNamedSession = strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true"
	r.SubmissionCapabilities = session.SubmissionCapabilitiesForMetadata(b.Metadata, hasDeferredQueue)
	r.Metadata = filterMetadata(b.Metadata)
	return r
}

// TestSessionGetEnrichedWireByteIdentical is the keystone Get-path invariant:
// the PRODUCTION single-handle read composition (sessionGetEnriched =
// Store.GetPersistedResponse + Manager.EnrichInfo, the path that replaced the
// retired Manager.GetWithPersistedResponse) must produce a byte-identical
// session response. The golden builds the response the pre-cutover way (mgr.Get
// for Info plus a separate store.Get projected through PersistedResponseFromBead);
// the new path builds Info + PersistedResponse from the production composition,
// so this pins the real Get path, not a dead method.
func TestSessionGetEnrichedWireByteIdentical(t *testing.T) {
	cfg := &config.City{}
	for _, b := range wireSessionBeadFixtures() {
		b := b
		t.Run(b.ID, func(t *testing.T) {
			store := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
			mgr := session.NewManagerWithOptions(store, runtime.NewFake())

			// Golden: the pre-cutover double-read. mgr.Get for the runtime-enriched
			// Info, then a separate store.Get projected to PersistedResponse.
			goldenInfo, err := mgr.Get(b.ID)
			if err != nil {
				t.Fatalf("mgr.Get: %v", err)
			}
			rawBead, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("store.Get: %v", err)
			}
			golden := sessionResponseWithReason(goldenInfo, session.PersistedResponseFromBead(rawBead), cfg, nil, true)

			// New: the production Get composition the API handlers call.
			gotInfo, pr, err := sessionGetEnriched(session.NewStore(beads.SessionStore{Store: store}), mgr, b.ID)
			if err != nil {
				t.Fatalf("sessionGetEnriched: %v", err)
			}
			got := sessionResponseWithReason(gotInfo, pr, cfg, nil, true)

			goldenJSON, err := json.Marshal(golden)
			if err != nil {
				t.Fatalf("marshal golden: %v", err)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}
			if string(goldenJSON) != string(gotJSON) {
				t.Fatalf("wire mismatch for %s:\n golden = %s\n got    = %s", b.ID, goldenJSON, gotJSON)
			}
		})
	}
}

// wireSessionBeadFixtures returns representative persisted session beads spanning
// the states whose response JSON depends on bead status + metadata: creating,
// active, and closed, with alias/title/agent_name/permission-mode overrides and
// exposable metadata.
func wireSessionBeadFixtures() []beads.Bead {
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	mk := func(id, status string, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID:        id,
			Type:      session.BeadType,
			Status:    status,
			Title:     meta["__title"],
			Labels:    []string{session.LabelSession},
			Metadata:  meta,
			CreatedAt: created,
		}
	}
	return []beads.Bead{
		mk("s-active", "open", map[string]string{
			"__title":            "Active One",
			"template":           "polecat",
			"state":              "active",
			"alias":              "pc-1",
			"agent_name":         "polecat-7",
			"provider":           "claude",
			"session_name":       "s-active",
			"template_overrides": `{"permission_mode":"unrestricted"}`,
			"real_world_app_foo": "bar",
		}),
		mk("s-creating", "open", map[string]string{
			"__title":      "Creating One",
			"template":     "polecat",
			"state":        "creating",
			"provider":     "claude",
			"session_name": "s-creating",
		}),
		mk("s-named", "open", map[string]string{
			"__title":                       "Named One",
			"template":                      "mayor",
			"state":                         "asleep",
			"provider":                      "claude",
			"session_name":                  "s-named",
			session.NamedSessionMetadataKey: "true",
			"sleep_reason":                  "user-hold",
		}),
		mk("s-closed", "closed", map[string]string{
			"__title":      "Closed One",
			"template":     "polecat",
			"provider":     "claude",
			"session_name": "s-closed",
		}),
	}
}

// TestSessionResponseFromInfoWireByteIdentical is the keystone S2 invariant: the
// session response built from session.Info plus the persisted-response
// projection must be byte-identical to the response built directly from the raw
// bead. This guards that promoting the response path to speak domain types is an
// internal refactor of HOW responses are built, never WHAT they contain.
func TestSessionResponseFromInfoWireByteIdentical(t *testing.T) {
	cfg := &config.City{}
	for _, b := range wireSessionBeadFixtures() {
		b := b
		t.Run(b.ID, func(t *testing.T) {
			// Info + PR from the front-door single fetch: GetPersistedResponse runs
			// both projection codecs at the store edge, so info/pr are byte-identical
			// to the raw per-bead projections — but no raw codec is called in the
			// test. The same info feeds both builders, so this stays a
			// builder-vs-builder oracle (raw-bead path vs Info+PR path).
			store := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
			info, pr, err := session.NewStore(beads.SessionStore{Store: store}).GetPersistedResponse(b.ID)
			if err != nil {
				t.Fatalf("GetPersistedResponse: %v", err)
			}

			// Golden: built from the raw bead (the pre-S2 path).
			golden := sessionResponseFromBead(info, &b, cfg, nil, true)

			// New: built from Info + the persisted-response projection, with no
			// raw *beads.Bead crossing into the response builder.
			got := sessionResponseWithReason(info, pr, cfg, nil, true)

			goldenJSON, err := json.Marshal(golden)
			if err != nil {
				t.Fatalf("marshal golden: %v", err)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}
			if string(goldenJSON) != string(gotJSON) {
				t.Fatalf("wire mismatch for %s:\n golden = %s\n got    = %s", b.ID, goldenJSON, gotJSON)
			}
		})
	}
}
