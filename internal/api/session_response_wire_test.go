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

// TestGetWithPersistedResponseWireByteIdentical is the keystone S3 invariant:
// collapsing the redundant raw store.Get beside mgr.Get into the single-fetch
// session.Manager.GetWithPersistedResponse must produce a byte-identical
// session response. The golden builds the response the pre-S3 way (mgr.Get for
// Info plus a separate store.Get projected through PersistedResponseFromBead);
// the new path builds Info + PersistedResponse from the single domain call.
func TestGetWithPersistedResponseWireByteIdentical(t *testing.T) {
	cfg := &config.City{}
	for _, b := range wireSessionBeadFixtures() {
		b := b
		t.Run(b.ID, func(t *testing.T) {
			store := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
			mgr := session.NewManager(store, runtime.NewFake())

			// Golden: the pre-S3 double-read. mgr.Get for the runtime-enriched
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

			// New: the single-fetch domain call.
			gotInfo, pr, err := mgr.GetWithPersistedResponse(b.ID)
			if err != nil {
				t.Fatalf("GetWithPersistedResponse: %v", err)
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
			info := session.InfoFromPersistedBead(b)

			// Golden: built from the raw bead (the pre-S2 path).
			golden := sessionResponseFromBead(info, &b, cfg, nil, true)

			// New: built from Info + the persisted-response projection, with no
			// raw *beads.Bead crossing into the response builder.
			pr := session.PersistedResponseFromBead(b)
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
