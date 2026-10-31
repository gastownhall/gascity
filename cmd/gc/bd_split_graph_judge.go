package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The split-city judge of `gc bd create --graph <plan>`. bd applies the plan
// in one transaction, minting one bead per node with that node's own type,
// labels and metadata, plus the parent links and edges the plan names. The
// judge reads the plan the way bd decodes it (the same Go types, so the same
// case-insensitive field match, duplicate-key and merge behavior), classifies
// every node, and holds every id-named endpoint to the cross-store edge rule.

// bdGraphPlan is the subset of bd's graph-apply plan the judge needs, decoded
// with bd's own field types.
type bdGraphPlan struct {
	Nodes []bdGraphNode `json:"nodes"`
	Edges []bdGraphEdge `json:"edges"`
}

// bdGraphNode is the subset of bd's graph-apply node the judge reads.
// Metadata is raw JSON per key, as bd decodes it: duplicate metadata objects
// merge into one map, which a beads.StringMap decode would not reproduce.
type bdGraphNode struct {
	Key          string                     `json:"key"`
	ID           string                     `json:"id"`
	Type         string                     `json:"type"`
	Labels       []string                   `json:"labels"`
	Metadata     map[string]json.RawMessage `json:"metadata"`
	MetadataRefs map[string]string          `json:"metadata_refs"`
	ParentID     string                     `json:"parent_id"`
	Deps         []bdGraphDep               `json:"deps"`
}

// bdGraphDep is the subset of a node's deps entry the judge reads.
type bdGraphDep struct {
	Target string `json:"target"`
}

// bdGraphEdge is the subset of bd's graph-apply edge the judge reads: its
// id-named endpoints. bd prefers an _id over its _key, so a key-named
// endpoint is plan-local only when the matching _id is empty.
type bdGraphEdge struct {
	FromID    string `json:"from_id"`
	ToID      string `json:"to_id"`
	SpawnerID string `json:"spawner_id"`
	ThreadID  string `json:"thread_id"`
}

// bd's graph-apply plan schema: the json tags of its plan, node, edge and dep
// types, identical in every pinned bd build. A field outside these is one gc
// has not vetted, so a plan carrying it is refused.
var (
	bdGraphPlanFields = []string{"commit_message", "nodes", "edges"}
	bdGraphNodeFields = []string{
		"key", "id", "title", "type", "status", "description", "design",
		"acceptance_criteria", "notes", "spec_id", "external_ref", "assignee",
		"assign_after_create", "owner", "priority", "estimate",
		"estimated_minutes", "due_at", "defer_until", "labels", "metadata",
		"metadata_refs", "parent", "parent_key", "parent_id", "deps",
		"ephemeral", "no_history", "storage_class", "wisp_type", "mol_type",
		"pinned", "event_kind", "actor", "target", "payload",
	}
	bdGraphEdgeFields = []string{"from_key", "from_id", "to_key", "to_id", "type", "gate", "spawner_key", "spawner_id", "thread_id"}
	bdGraphDepFields  = []string{"type", "target"}
)

// judgeBdCreateGraph judges `create --graph <planPath>` node by node and
// forwards the argv unchanged when every node keeps its class and every edge
// stays inside the work stores; bd re-reads the plan itself.
func judgeBdCreateGraph(in bdClassGuardInput, planPath string) bdClassGuard {
	who := fmt.Sprintf("gc bd %s --graph %s", in.path, planPath)
	path := planPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.scopeRoot, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return in.refuse("%s: refused on this split city: gc reads the plan to judge each bead it would mint, and could not (%v). Nothing was written.", who, err)
	}
	if problem := bdGraphPlanUnvettedField(data); problem != "" {
		return in.refuse("%s: refused on this split city: %s. Nothing was written.", who, problem)
	}
	var plan bdGraphPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return in.refuse("%s: refused on this split city: the plan cannot be read as bd reads it (%v). Nothing was written.", who, err)
	}
	keys := map[string]bool{}
	for _, n := range plan.Nodes {
		keys[n.Key] = true
	}
	classKeys := coordclass.ClassifierMetadataKeys()
	var endpoints, ids []string
	for _, n := range plan.Nodes {
		node := fmt.Sprintf("%s node %q", who, n.Key)
		ids = append(ids, n.ID)
		for k := range n.MetadataRefs {
			if slices.Contains(classKeys, k) {
				return in.refuse("%s: refused on this split city: its metadata_refs sets %s, a key that decides the bead's storage class. Nothing was written.", node, k)
			}
		}
		meta, err := bdGraphNodeMetadata(n.Metadata)
		if err != nil {
			return in.refuse("%s: refused on this split city: its metadata cannot be read (%v). Nothing was written.", node, err)
		}
		// bd stores the node's type and labels as written; gc reads them back
		// trimmed. Both spellings must keep the bead's class.
		for _, shape := range []beads.Bead{
			{Type: n.Type, Labels: n.Labels, Metadata: meta},
			{Type: strings.TrimSpace(n.Type), Labels: normalizeBdLabels(n.Labels), Metadata: meta},
		} {
			if flip := coordclass.CheckFlip(beads.Bead{}, shape, in.holds); flip != nil {
				return in.refuse("%s", classFlipRefusalText(node, flip))
			}
		}
		endpoints = append(endpoints, n.ParentID)
		for _, d := range n.Deps {
			// bd resolves a deps target as a plan key first, then as an id.
			if !keys[d.Target] {
				endpoints = append(endpoints, d.Target)
			}
		}
	}
	for _, e := range plan.Edges {
		endpoints = append(endpoints, e.FromID, e.ToID, e.SpawnerID, e.ThreadID)
	}
	// The node ids are judged after every node's static class checks, so a
	// plan those refuse asks the binding nothing.
	if why := in.mintIDRefusal(ids...); why != "" {
		return in.refuse("%s: refused on this split city: a node's explicit id cannot be minted here: %s. Nothing was written.", who, why)
	}
	if refusal := in.checkEdgeEndpoints(endpoints...); refusal != "" {
		return in.refuse("%s", refusal)
	}
	return in.pass()
}

// bdGraphNodeMetadata coerces a node's raw metadata values to strings the way
// gc reads them back (beads.StringMap).
func bdGraphNodeMetadata(raw map[string]json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var fields beads.StringMap
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// bdGraphPlanUnvettedField returns a description of the first field of the
// plan outside bd's pinned graph-plan schema, at plan, node, edge or dep
// level, or "" when every field is known. Keys are matched as bd's own
// unknown-field check matches them (lowercased), so a spelling that only
// Unicode case folding maps onto a known tag is refused, not missed.
func bdGraphPlanUnvettedField(data []byte) string {
	top, problem := bdGraphObjectFields("the plan", data, bdGraphPlanFields)
	if problem != "" {
		return problem
	}
	for k, v := range top {
		kind := strings.ToLower(k)
		if kind != "nodes" && kind != "edges" {
			continue
		}
		items, problem := bdGraphArray(fmt.Sprintf("the plan's %q", k), v)
		if problem != "" {
			return problem
		}
		for i, item := range items {
			if kind == "edges" {
				if _, problem := bdGraphObjectFields(fmt.Sprintf("edge %d", i), item, bdGraphEdgeFields); problem != "" {
					return problem
				}
				continue
			}
			node, problem := bdGraphObjectFields(fmt.Sprintf("node %d", i), item, bdGraphNodeFields)
			if problem != "" {
				return problem
			}
			for nk, nv := range node {
				if strings.ToLower(nk) != "deps" {
					continue
				}
				deps, problem := bdGraphArray(fmt.Sprintf("node %d's %q", i, nk), nv)
				if problem != "" {
					return problem
				}
				for j, d := range deps {
					if _, problem := bdGraphObjectFields(fmt.Sprintf("node %d dep %d", i, j), d, bdGraphDepFields); problem != "" {
						return problem
					}
				}
			}
		}
	}
	return ""
}

// bdGraphObjectFields decodes obj as a JSON object (null is no object, as bd
// reads it) and checks each of its keys against known.
func bdGraphObjectFields(where string, obj json.RawMessage, known []string) (map[string]json.RawMessage, string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return nil, fmt.Sprintf("%s is not a JSON object (%v)", where, err)
	}
	for k := range m {
		if !slices.Contains(known, strings.ToLower(k)) {
			return nil, fmt.Sprintf("%s has the field %q, which gc does not know how bd reads", where, k)
		}
	}
	return m, ""
}

// bdGraphArray decodes v as a JSON array (null is an empty one).
func bdGraphArray(where string, v json.RawMessage) ([]json.RawMessage, string) {
	var items []json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, fmt.Sprintf("%s is not a JSON array (%v)", where, err)
	}
	return items, ""
}
