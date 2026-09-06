package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	poolTemplateReadyAssignee = "foundations/worker"
	poolMemberReadyAssignee   = "foundations/worker-1"
)

// exactAssigneeReadyReader models the raw beads contract deliberately: a
// --assignee read returns a row only when the requested spelling is byte-equal
// to one of the configured answers. Pool membership is therefore exercised in
// the generated Gas City query, not smuggled into the reader double.
const exactAssigneeReadyReader = `#!/bin/sh
set -eu
case "$1" in
  ready)
    case " $* " in
      *" --status in_progress "*) printf '[]'; exit 0 ;;
    esac
    assignee=""
    for arg in "$@"; do
      case "$arg" in
        --assignee=*) assignee=${arg#--assignee=} ;;
      esac
    done
    if [ -n "${READY_LOG:-}" ] && [ -n "$assignee" ]; then
      printf '%s\n' "$assignee" >> "$READY_LOG"
    fi
    if [ -n "${PRIMARY_ASSIGNEE:-}" ] && [ "$assignee" = "$PRIMARY_ASSIGNEE" ]; then
      printf '[{"id":"%s","status":"open","issue_type":"task","assignee":"%s"}]' "$PRIMARY_BEAD" "$PRIMARY_ASSIGNEE"
    elif [ -n "${FALLBACK_ASSIGNEE:-}" ] && [ "$assignee" = "$FALLBACK_ASSIGNEE" ]; then
      printf '[{"id":"%s","status":"open","issue_type":"task","assignee":"%s"}]' "$FALLBACK_BEAD" "$FALLBACK_ASSIGNEE"
    else
      printf '[]'
    fi
    ;;
  *) printf '[]' ;;
esac
`

type poolReadyQueryShape struct {
	name  string
	build func(*Agent, QueryTopology) string
}

func poolReadyQueryShapes() []poolReadyQueryShape {
	return []poolReadyQueryShape{
		{name: "work_query", build: (*Agent).EffectiveWorkQueryFor},
		{name: "assigned_ready_query", build: (*Agent).EffectiveAssignedReadyQueryFor},
	}
}

func poolReadyQueryTopologies() []struct {
	name string
	topo QueryTopology
} {
	return []struct {
		name string
		topo QueryTopology
	}{
		{
			name: "single_store_bd_ready",
			topo: QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}},
		},
		{
			name: "federated_gc_ready",
			topo: QueryTopology{
				Beads:          BeadsConfig{BDCompatibility: BeadsBDCompatibility105},
				FederatedReady: true,
			},
		},
	}
}

func configuredPoolTemplateAgent() *Agent {
	return &Agent{
		Name:              "worker",
		Dir:               "foundations",
		MinActiveSessions: ptrInt(0),
		MaxActiveSessions: ptrInt(3),
	}
}

func poolMemberReadyEnv(logPath string) map[string]string {
	return map[string]string{
		"GC_SESSION_ID":     "gc-session-worker-1",
		"GC_SESSION_NAME":   poolMemberReadyAssignee,
		"GC_ALIAS":          poolMemberReadyAssignee,
		"GC_AGENT":          poolMemberReadyAssignee,
		"GC_TEMPLATE":       poolTemplateReadyAssignee,
		"GC_SESSION_ORIGIN": "ephemeral",
		"READY_LOG":         logPath,
		"PRIMARY_ASSIGNEE":  "no-concrete-match",
		"PRIMARY_BEAD":      "ga-concrete-ready",
		"FALLBACK_ASSIGNEE": poolTemplateReadyAssignee,
		"FALLBACK_BEAD":     "ga-template-ready",
	}
}

func runPoolReadyQuery(t *testing.T, agent *Agent, topo QueryTopology, build func(*Agent, QueryTopology) string, env map[string]string) generatedQueryResult {
	t.Helper()
	gcScript := fakeBDEmpty
	bdScript := exactAssigneeReadyReader
	if topo.FederatedReady {
		gcScript = exactAssigneeReadyReader
		bdScript = fakeBDEmpty
	}
	return runGeneratedQueryWithBD(t, build(agent, topo), env, gcScript, bdScript)
}

// TestPoolMemberDefaultQueriesDiscoverTemplateAssignedReadyWork is the RED
// reproduction for ga-8vz95k. A configured pool member owns concrete runtime
// identities, but ready work may be assigned to the shared template. Both
// generated query surfaces must explicitly ask the exact reader for that
// template after exhausting the concrete identities.
func TestPoolMemberDefaultQueriesDiscoverTemplateAssignedReadyWork(t *testing.T) {
	for _, topology := range poolReadyQueryTopologies() {
		for _, shape := range poolReadyQueryShapes() {
			t.Run(topology.name+"/"+shape.name, func(t *testing.T) {
				logPath := filepath.Join(t.TempDir(), "ready-assignees")
				res := runPoolReadyQuery(t, configuredPoolTemplateAgent(), topology.topo, shape.build, poolMemberReadyEnv(logPath))
				if res.exit != 0 {
					t.Fatalf("generated query exited %d: stderr=%q stdout=%q", res.exit, res.stderr, res.stdout)
				}
				if !strings.Contains(res.stdout, "ga-template-ready") {
					t.Fatalf("generated query output = %q, want ready work assigned to pool template %q", res.stdout, poolTemplateReadyAssignee)
				}

				invocations, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatalf("read exact-reader invocation log: %v", err)
				}
				lines := strings.Fields(string(invocations))
				templateAt := -1
				lastConcreteAt := -1
				for i, assignee := range lines {
					switch assignee {
					case poolTemplateReadyAssignee:
						templateAt = i
					case "gc-session-worker-1", poolMemberReadyAssignee:
						lastConcreteAt = i
					}
				}
				if templateAt < 0 {
					t.Fatalf("exact reader assignees = %v, want an explicit %q fallback; raw --assignee matching must remain exact", lines, poolTemplateReadyAssignee)
				}
				if templateAt <= lastConcreteAt {
					t.Fatalf("exact reader assignees = %v, want every concrete identity before shared template %q", lines, poolTemplateReadyAssignee)
				}
			})
		}
	}
}

// TestPoolMemberConcreteReadyIdentityPrecedesTemplate protects the existing
// identity order. Shared-template compatibility is a fallback for ready work,
// never a replacement for the concrete owner candidates.
func TestPoolMemberConcreteReadyIdentityPrecedesTemplate(t *testing.T) {
	for _, topology := range poolReadyQueryTopologies() {
		for _, shape := range poolReadyQueryShapes() {
			t.Run(topology.name+"/"+shape.name, func(t *testing.T) {
				logPath := filepath.Join(t.TempDir(), "ready-assignees")
				env := poolMemberReadyEnv(logPath)
				env["PRIMARY_ASSIGNEE"] = poolMemberReadyAssignee
				env["PRIMARY_BEAD"] = "ga-concrete-ready"

				res := runPoolReadyQuery(t, configuredPoolTemplateAgent(), topology.topo, shape.build, env)
				if res.exit != 0 {
					t.Fatalf("generated query exited %d: stderr=%q stdout=%q", res.exit, res.stderr, res.stdout)
				}
				if !strings.Contains(res.stdout, "ga-concrete-ready") || strings.Contains(res.stdout, "ga-template-ready") {
					t.Fatalf("generated query output = %q, want concrete ready work before shared-template fallback", res.stdout)
				}

				invocations, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatalf("read exact-reader invocation log: %v", err)
				}
				if strings.Contains(string(invocations), poolTemplateReadyAssignee+"\n") {
					t.Fatalf("exact reader assignees = %q, shared template was queried before the concrete match returned", invocations)
				}
			})
		}
	}
}

// TestTemplateAssignedReadyFallbackRequiresConfiguredEphemeralPoolMembership
// prevents suffix parsing or session-origin widening from turning a shared
// queue into cross-session ownership. Each case offers work only for the
// fallback assignee; the generated query must remain empty.
func TestTemplateAssignedReadyFallbackRequiresConfiguredEphemeralPoolMembership(t *testing.T) {
	tests := []struct {
		name     string
		agent    *Agent
		override map[string]string
	}{
		{
			name:  "ordinary_singleton",
			agent: &Agent{Name: "auditor", Dir: "foundations", MaxActiveSessions: ptrInt(1)},
			override: map[string]string{
				"GC_TEMPLATE":       "foundations/auditor",
				"FALLBACK_ASSIGNEE": "foundations/auditor",
			},
		},
		{
			name:  "numeric_suffix_lookalike",
			agent: &Agent{Name: "worker-1", Dir: "foundations", MaxActiveSessions: ptrInt(1)},
			override: map[string]string{
				"GC_SESSION_NAME":   "foundations/worker-1",
				"GC_ALIAS":          "foundations/worker-1",
				"GC_AGENT":          "foundations/worker-1",
				"GC_TEMPLATE":       "foundations/worker-1",
				"FALLBACK_ASSIGNEE": "foundations/worker",
			},
		},
		{
			name:  "configured_named_session",
			agent: configuredPoolTemplateAgent(),
			override: map[string]string{
				"GC_SESSION_ORIGIN": "named",
			},
		},
		{
			name:  "manual_session",
			agent: configuredPoolTemplateAgent(),
			override: map[string]string{
				"GC_SESSION_ORIGIN": "manual",
			},
		},
		{
			name: "member_of_different_pool",
			agent: &Agent{
				Name:              "worker-a",
				Dir:               "foundations",
				MinActiveSessions: ptrInt(0),
				MaxActiveSessions: ptrInt(3),
			},
			override: map[string]string{
				"GC_TEMPLATE":       "foundations/worker-a",
				"FALLBACK_ASSIGNEE": "foundations/worker-b",
			},
		},
	}

	for _, tt := range tests {
		for _, topology := range poolReadyQueryTopologies() {
			for _, shape := range poolReadyQueryShapes() {
				t.Run(tt.name+"/"+topology.name+"/"+shape.name, func(t *testing.T) {
					env := poolMemberReadyEnv("")
					for key, value := range tt.override {
						env[key] = value
					}
					res := runPoolReadyQuery(t, tt.agent, topology.topo, shape.build, env)
					if res.exit != 0 {
						t.Fatalf("generated query exited %d: stderr=%q stdout=%q", res.exit, res.stderr, res.stdout)
					}
					if output := strings.TrimSpace(res.stdout); output != "" && output != "[]" {
						t.Fatalf("generated query output = %q, want no shared-template ready fallback", res.stdout)
					}
				})
			}
		}
	}
}

// TestPoolMemberCustomWorkQueryRemainsVerbatim keeps configuration ownership
// explicit: shared-template compatibility applies only to generated defaults.
func TestPoolMemberCustomWorkQueryRemainsVerbatim(t *testing.T) {
	const custom = `custom-ready --assignee="$GC_AGENT"`
	agent := configuredPoolTemplateAgent()
	agent.WorkQuery = custom

	for _, topology := range poolReadyQueryTopologies() {
		for _, shape := range poolReadyQueryShapes() {
			if got := shape.build(agent, topology.topo); got != custom {
				t.Errorf("%s/%s generated %q, want custom work_query returned verbatim", topology.name, shape.name, got)
			}
		}
	}
}
