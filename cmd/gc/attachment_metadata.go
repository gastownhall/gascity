package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

func beadFromGetters(id string, getters ...BeadQuerier) (beads.Bead, bool) {
	for _, getter := range getters {
		if getter == nil {
			continue
		}
		b, err := getter.Get(id)
		if err == nil {
			return b, true
		}
	}
	return beads.Bead{}, false
}

func collectAttachedBeads(parent beads.Bead, store beads.Store, childQuerier BeadChildQuerier) ([]beads.Bead, error) {
	var (
		attachments []beads.Bead
		firstErr    error
	)
	seen := make(map[string]struct{})

	addByID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || store == nil {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		attached, err := store.Get(id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		seen[id] = struct{}{}
		attachments = append(attachments, attached)
	}

	addByID(parent.Metadata["molecule_id"])
	addByID(parent.Metadata["workflow_id"])

	if childQuerier != nil {
		children, err := childQuerier.Children(parent.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			for _, child := range children {
				if !isAttachedRoot(child) {
					continue
				}
				if _, ok := seen[child.ID]; ok {
					continue
				}
				seen[child.ID] = struct{}{}
				attachments = append(attachments, child)
			}
		}
	}

	return attachments, firstErr
}

func attachmentLabel(b beads.Bead) string {
	if isWorkflowAttachment(b) {
		return "workflow"
	}
	return "molecule"
}

func isAttachedRoot(b beads.Bead) bool {
	return isWorkflowAttachment(b) || isMoleculeAttachment(b)
}

func isWorkflowAttachment(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Metadata["gc.kind"]), "workflow") ||
		strings.EqualFold(strings.TrimSpace(b.Metadata["gc.formula_contract"]), "graph.v2")
}

func isMoleculeAttachment(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Type), "molecule")
}
