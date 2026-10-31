//go:build integration

package main

// The facts the split-city judge of `gc bd create --graph` rests on, against
// the pinned bd on both of its transports. The judge classifies each node as
// written and never copies a parent's labels onto a node; it decodes metadata
// with bd's own types; it treats an endpoint naming a node's explicit id as
// that node; and it judges a metadata_refs key as the id of the node the ref
// names. Only bd can show those hold, so this test applies one plan and reads
// back what bd wrote.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestGcBdCreateGraphJudgeFactsRealBd(t *testing.T) {
	for _, transport := range []struct {
		name string
		init []string
	}{{"embedded", nil}, {"proxied-server", []string{"--proxied-server"}}} {
		t.Run(transport.name, func(t *testing.T) {
			ws, read, must := realBdWorkspace(t, transport.init...)
			if len(transport.init) > 0 {
				t.Cleanup(func() {
					if out, err := read("dolt", "stop"); err != nil {
						t.Logf("stopping the workspace's proxy: %v\n%s", err, out)
					}
				})
			}
			plan := `{"nodes":[
 {"key":"parent","title":"parent","labels":["on-parent-only"]},
 {"key":"child","title":"child","type":"feat","labels":[" padded "],"parent_key":"parent","metadata":{"a":"1"},"metadata":{"b":"2"}},
 {"key":"mail","title":"mail","type":"message"},
 {"key":"pinned","title":"pinned","id":"tt-pinned1"},
 {"key":"refs","title":"refs","metadata":{"root":"direct"},"metadata_refs":{"root":"pinned","minted":"parent"},"deps":[{"target":"tt-pinned1"}]}],
 "edges":[{"from_id":"tt-pinned1","to_key":"parent","type":"blocks"}]}`
			path := filepath.Join(ws, "plan.json")
			if err := os.WriteFile(path, []byte(plan), 0o644); err != nil {
				t.Fatal(err)
			}
			var created struct {
				IDs map[string]string `json:"ids"`
			}
			out := must("create", "--graph", path, "--json")
			if err := json.Unmarshal(out, &created); err != nil || len(created.IDs) != 5 {
				t.Fatalf("decoding the graph result: %v\n%s", err, out)
			}

			child := realBdShow(t, must, created.IDs["child"])
			if !slices.Equal(child.Labels, []string{" padded "}) {
				t.Errorf("child labels = %q, want the padded label as written and none of the parent's", child.Labels)
			}
			if child.Type != "feature" {
				t.Errorf("child type = %q, want bd's alias feat -> feature", child.Type)
			}
			if child.Metadata["a"] != "1" || child.Metadata["b"] != "2" {
				t.Errorf("child metadata = %v, want the two metadata objects merged", child.Metadata)
			}
			if mail := realBdShow(t, must, created.IDs["mail"]); mail.Type != "message" {
				t.Errorf("mail type = %q, want message as written", mail.Type)
			}
			if created.IDs["pinned"] != "tt-pinned1" {
				t.Fatalf("pinned minted as %q, want its explicit id", created.IDs["pinned"])
			}
			refs := realBdShow(t, must, created.IDs["refs"])
			if refs.Metadata["root"] != "tt-pinned1" || refs.Metadata["minted"] != created.IDs["parent"] {
				t.Errorf("refs metadata = %v, want root=tt-pinned1 over the written value and minted=%s", refs.Metadata, created.IDs["parent"])
			}
			// A deps target and an edge from_id naming the pinned node's explicit
			// id bind that node, minted by the same plan.
			if !realBdDependsOn(t, must, created.IDs["refs"], "tt-pinned1", "blocks") {
				t.Errorf("refs does not block on tt-pinned1 through its deps target")
			}
			if !realBdDependsOn(t, must, "tt-pinned1", created.IDs["parent"], "blocks") {
				t.Errorf("tt-pinned1 does not block on %s through the edge's from_id", created.IDs["parent"])
			}
		})
	}
}

// realBdDependsOn reports whether a real bd lists an edge of type depType from
// id to target.
func realBdDependsOn(t *testing.T, must func(args ...string) []byte, id, target, depType string) bool {
	t.Helper()
	var deps []struct {
		ID   string `json:"id"`
		Type string `json:"dependency_type"`
	}
	if err := json.Unmarshal(must("dep", "list", id, "--json"), &deps); err != nil {
		t.Fatalf("decoding bd dep list %s: %v", id, err)
	}
	return slices.ContainsFunc(deps, func(d struct {
		ID   string `json:"id"`
		Type string `json:"dependency_type"`
	},
	) bool {
		return d.ID == target && d.Type == depType
	})
}
