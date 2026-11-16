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

// poolAnchoredTemplateFakeBD is a fake bd for a configured pool member whose
// shared template also has ready work. When anchored, the member owns an
// in-progress graph workflow root (its remembered anchor); readyStep adds a
// ready step of that workflow. Ready work assigned to the shared template is
// always on offer, and --assignee is matched exactly so the member's own
// identity never stands in for the template.
func poolAnchoredTemplateFakeBD(anchored, readyStep bool) string {
	list := `printf '[]'`
	if anchored {
		list = `printf '[{"id":"workflow-root","status":"in_progress","assignee":"` + poolMemberReadyAssignee +
			`","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"` + poolTemplateReadyAssignee + `"}}]'`
	}
	anchorReady := `printf '[]'`
	if readyStep {
		anchorReady = `printf '[{"id":"anchored-ready-step","status":"open","assignee":"","metadata":{"gc.kind":"task","gc.root_bead_id":"workflow-root","gc.routed_to":"` +
			poolTemplateReadyAssignee + `"}}]'`
	}
	return `#!/bin/sh
set -eu
case "$1" in
  list)
    ` + list + `
    ;;
  show)
    printf '[{"id":"workflow-root","dependencies":[]}]'
    ;;
  ready)
    assignee=""
    for arg in "$@"; do
      case "$arg" in
        --assignee=*) assignee=${arg#--assignee=} ;;
      esac
    done
    case "$*" in
      *"--metadata-field gc.root_bead_id=workflow-root"*)
        ` + anchorReady + `
        ;;
      *)
        if [ "$assignee" = "` + poolTemplateReadyAssignee + `" ]; then
          printf '[{"id":"ga-template-ready","status":"open","issue_type":"task","assignee":"` + poolTemplateReadyAssignee + `"}]'
        else
          printf '[]'
        fi
        ;;
    esac
    ;;
  *)
    printf '[]'
    ;;
esac
`
}

// TestPoolMemberAnchoredWorkflowPrecedesTemplateAssignedReadyWork pins where
// the shared-template tier sits among the graph-anchor tiers of the default
// work_query. A pool member that remembers an in-progress graph workflow root
// resumes that workflow, and falls back to the root itself when none of its
// steps is ready, before it looks at work assigned to the shared template: an
// anchored session must not hop to unrelated work, the rule
// TestEffectiveWorkQueryPrefersReadyStepOverAssignedGraphWorkflowRoot pins for
// routed work. The unanchored control shows the same fake does serve the
// template row, so the two anchored cases cannot pass vacuously.
func TestPoolMemberAnchoredWorkflowPrecedesTemplateAssignedReadyWork(t *testing.T) {
	tests := []struct {
		name      string
		anchored  bool
		readyStep bool
		want      string
	}{
		{name: "anchored_ready_step", anchored: true, readyStep: true, want: "anchored-ready-step"},
		{name: "anchored_root_fallback", anchored: true, readyStep: false, want: "workflow-root"},
		{name: "unanchored_control", anchored: false, readyStep: false, want: "ga-template-ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runEffectiveWorkQuery(t, *configuredPoolTemplateAgent(), map[string]string{
				"GC_SESSION_ID":     "gc-session-worker-1",
				"GC_SESSION_NAME":   poolMemberReadyAssignee,
				"GC_ALIAS":          poolMemberReadyAssignee,
				"GC_AGENT":          poolMemberReadyAssignee,
				"GC_TEMPLATE":       poolTemplateReadyAssignee,
				"GC_SESSION_ORIGIN": "ephemeral",
			}, poolAnchoredTemplateFakeBD(tt.anchored, tt.readyStep))

			ids := workQueryOutputIDOrder(t, out)
			if len(ids) != 1 || ids[0] != tt.want {
				t.Fatalf("EffectiveWorkQuery() candidates = %v (output %q), want only %q", ids, out, tt.want)
			}
		})
	}
}

// poolTemplateEphemeralFakeBD models a default-compatibility (non-1.0.5)
// single store: every ordinary `bd ready` read is empty, and the open
// ephemeral snapshot (`bd query ... status=open`) returns the rows in
// $OPEN_EPHEMERAL_ROWS. Template-assigned wisps are therefore reachable only
// through the ephemeral probe.
const poolTemplateEphemeralFakeBD = `#!/bin/sh
set -eu
case "$1" in
  query)
    case "$*" in
      *"status=open"*) printf '%s' "${OPEN_EPHEMERAL_ROWS:-[]}" ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`

func openEphemeralRow(id, assignee string) string {
	return `{"id":"` + id + `","status":"open","issue_type":"task","ephemeral":true,"assignee":"` + assignee + `","dependency_count":0}`
}

// TestPoolMemberDefaultCompatDiscoversTemplateAssignedEphemeralReadyWork
// covers the default (non-1.0.5) bd compatibility, where ordinary ready reads
// omit wisps and the concrete-identity loop follows each read with the
// ephemeral probe. The shared-template tier must do the same, or wisps
// assigned to the template stay invisible on default-compat cities.
func TestPoolMemberDefaultCompatDiscoversTemplateAssignedEphemeralReadyWork(t *testing.T) {
	templateRow := openEphemeralRow("ga-template-wisp", poolTemplateReadyAssignee)
	concreteRow := openEphemeralRow("ga-concrete-wisp", poolMemberReadyAssignee)
	tests := []struct {
		name   string
		rows   string
		origin string
		want   string
	}{
		{name: "template_wisp", rows: "[" + templateRow + "]", origin: "ephemeral", want: "ga-template-wisp"},
		{name: "unset_origin_template_wisp", rows: "[" + templateRow + "]", origin: "", want: "ga-template-wisp"},
		{name: "concrete_wisp_wins", rows: "[" + templateRow + "," + concreteRow + "]", origin: "ephemeral", want: "ga-concrete-wisp"},
		{name: "named_origin_gets_nothing", rows: "[" + templateRow + "]", origin: "named", want: ""},
	}
	for _, tt := range tests {
		for _, shape := range poolReadyQueryShapes() {
			t.Run(tt.name+"/"+shape.name, func(t *testing.T) {
				env := poolMemberReadyEnv("")
				env["GC_SESSION_ORIGIN"] = tt.origin
				env["OPEN_EPHEMERAL_ROWS"] = tt.rows
				res := runGeneratedQueryWithBD(t, shape.build(configuredPoolTemplateAgent(), QueryTopology{}), env, fakeBDEmpty, poolTemplateEphemeralFakeBD)
				if res.exit != 0 {
					t.Fatalf("generated query exited %d: stderr=%q stdout=%q", res.exit, res.stderr, res.stdout)
				}
				if tt.want == "" {
					if output := strings.TrimSpace(res.stdout); output != "" && output != "[]" {
						t.Fatalf("generated query output = %q, want no shared-template ephemeral fallback", res.stdout)
					}
					return
				}
				if !strings.Contains(res.stdout, tt.want) {
					t.Fatalf("generated query output = %q, want %q", res.stdout, tt.want)
				}
				for _, other := range []string{"ga-template-wisp", "ga-concrete-wisp"} {
					if other != tt.want && strings.Contains(res.stdout, other) {
						t.Fatalf("generated query output = %q, want only %q", res.stdout, tt.want)
					}
				}
			})
		}
	}
}
