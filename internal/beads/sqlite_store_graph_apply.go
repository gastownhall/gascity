package beads

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"strings"
)

const sqliteGraphEdgeMetadataKVPrefix = "gascity.graph-edge-metadata.v1/"

// Compile-time proof that the SQLite store provides the graph-apply
// capability — i.e. it satisfies the GraphStore seam (coordrouter.GraphStore is
// GraphApplyStore) and the optional tier-aware extension.
var (
	_ GraphApplyStore        = (*SQLiteStore)(nil)
	_ StorageGraphApplyStore = (*SQLiteStore)(nil)
	// The two halves of the edge-payload contract. Reading without writing is
	// what let the infra-class copy detect a payload it could not carry;
	// asserting both here keeps a refactor from removing the half that closed it.
	_ DepMetadataReader = (*SQLiteStore)(nil)
	_ DepMetadataWriter = (*SQLiteStore)(nil)
)

// ApplyGraphPlan atomically instantiates an entire bead graph (nodes + edges) in
// one transaction, returning the symbolic-key -> concrete-ID map. It is the
// SQLite store's graph-apply capability — the operation ClassGraph consumers use
// (via beads.GraphApplyFor) to pour a formula-v2 topology. It is the in-process,
// single-transaction analog of the per-bead Create + DepAdd the classic path
// performs, with no fork/exec or per-bead commit.
func (s *SQLiteStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage is ApplyGraphPlan with an explicit physical storage
// tier for every node in the plan (history / no_history / ephemeral), mirroring
// the tier-selection the policy chokepoint applies on the classic path.
func (s *SQLiteStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, fmt.Errorf("sqlite graph apply: plan is nil")
	}
	if err := validateGraphApplyPlan(plan); err != nil {
		return nil, fmt.Errorf("sqlite graph apply: %w", err)
	}
	ephemeral, noHistory, err := graphStorageFlags(storage)
	if err != nil {
		return nil, fmt.Errorf("sqlite graph apply: %w", err)
	}

	result := &GraphApplyResult{IDs: make(map[string]string, len(plan.Nodes))}

	// Pass 1: materialize each node into a Bead with a concrete ID (so symbolic
	// keys in edges and MetadataRefs can resolve) but do not touch the DB yet.
	staged := make([]Bead, len(plan.Nodes))
	for i, node := range plan.Nodes {
		key := node.Key
		priority := node.Priority
		if priority == nil {
			defaultPriority := 2
			priority = &defaultPriority
		}
		b := s.normalizeCreate(Bead{
			Title:       node.Title,
			Description: node.Description,
			Type:        node.Type,
			Priority:    priority,
			Assignee:    node.Assignee,
			From:        node.From,
			ParentID:    node.ParentID,
			Labels:      append([]string(nil), node.Labels...),
			Metadata:    maps.Clone(node.Metadata),
			Ephemeral:   ephemeral,
			NoHistory:   noHistory,
		})
		result.IDs[key] = b.ID
		staged[i] = b
	}

	// Pass 2: one transaction — create every node, then wire every edge. The
	// parent relationship rides the bead's parent_id column (matching Create and
	// the Children query); plan.Edges become deps rows (matching DepAdd).
	//
	// Pass 1 minted IDs OUTSIDE the tx against a possibly-stale sequence floor,
	// so a concurrent process may have already claimed those suffixes. Before
	// creating anything, re-mint any colliding auto id in-tx and fix up the
	// symbolic key map. Symbolic parent and metadata references resolve only
	// after re-minting; raw ParentID and Metadata values are literal external
	// data and must not be changed merely because they resemble a provisional
	// ID. The shared seen map keeps two nodes in this plan from minting the same
	// fresh id while neither is committed yet.
	//
	// retryOnBusy may invoke the closure more than once, so each attempt rebuilds
	// its working state from the immutable Pass-1 snapshots (origStaged,
	// origKeyToID) rather than mutating shared state across attempts.
	origStaged := staged
	origKeyToID := result.IDs
	err = retryOnBusy(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite graph apply: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck

		// Fresh working copies for this attempt.
		working := make([]Bead, len(origStaged))
		for i := range origStaged {
			working[i] = cloneBead(origStaged[i])
		}
		keyToID := maps.Clone(origKeyToID)

		seen := make(map[string]bool, len(working))
		remap := make(map[string]string)
		for i := range working {
			fresh, err := s.mintUniqueIDTx(ctx, tx, working[i].ID, seen)
			if err != nil {
				return err
			}
			if fresh != working[i].ID {
				remap[working[i].ID] = fresh
				working[i].ID = fresh
			}
		}
		if len(remap) > 0 {
			for k, v := range keyToID {
				if nv, ok := remap[v]; ok {
					keyToID[k] = nv
				}
			}
		}

		// Preserve reference provenance through collision re-minting. Only the
		// plan's declared symbolic references receive freshly minted IDs; raw
		// ParentID and Metadata values remain exactly as supplied by the caller.
		for i, node := range plan.Nodes {
			if node.ParentKey != "" {
				working[i].ParentID = keyToID[node.ParentKey]
			}
			for metaKey, refKey := range node.MetadataRefs {
				if working[i].Metadata == nil {
					working[i].Metadata = make(map[string]string, len(node.MetadataRefs))
				}
				working[i].Metadata[metaKey] = keyToID[refKey]
			}
		}

		for _, b := range working {
			if err := s.clearClaimFenceTx(ctx, tx, b.ID); err != nil {
				return err
			}
			if err := s.upsertBeadTx(ctx, tx, b); err != nil {
				return err
			}
		}
		parentPairs := sqliteGraphApplyParentDepPairs(working)
		for i, edge := range plan.Edges {
			from := graphApplyResolveRef(edge.FromKey, edge.FromID, keyToID)
			to := graphApplyResolveRef(edge.ToKey, edge.ToID, keyToID)
			if from == "" || to == "" {
				return fmt.Errorf("sqlite graph apply: edge %d has an unresolved endpoint (from=%q to=%q)", i, from, to)
			}
			depType := edge.Type
			if depType == "" {
				depType = "blocks"
			}
			if parentPairs[sqliteGraphApplyDepPairKey(from, to)] {
				if depType == "parent-child" {
					continue
				}
				return fmt.Errorf("sqlite graph apply: edge %d %s->%s duplicates a parent-child relationship with dependency type %q", i, from, to, depType)
			}
			if parentPairs[sqliteGraphApplyDepPairKey(to, from)] && sqliteGraphApplyCycleRelevantDependencyType(depType) {
				return fmt.Errorf("sqlite graph apply: edge %d %s->%s creates a blocking reverse of a parent-child relationship", i, from, to)
			}
			if err := s.depAddWithMetadataTx(ctx, tx, from, to, edge.Type, edge.Metadata); err != nil {
				return err
			}
		}
		for _, b := range working {
			if b.ParentID == "" {
				continue
			}
			if err := s.depAddTx(ctx, tx, b.ID, b.ParentID, "parent-child"); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// Publish this attempt's final key->ID mapping only after a clean commit.
		result.IDs = keyToID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// graphApplyResolveRef resolves an edge endpoint with the same literal-ID
// precedence as the native graph applier. A plan that supplies both values has
// an explicit external target; symbolic-only endpoints resolve within this plan.
func graphApplyResolveRef(key, id string, keyToID map[string]string) string {
	if id != "" {
		return id
	}
	if key != "" {
		return keyToID[key]
	}
	return ""
}

func sqliteGraphApplyParentDepPairs(nodes []Bead) map[string]bool {
	pairs := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if node.ID != "" && node.ParentID != "" {
			pairs[sqliteGraphApplyDepPairKey(node.ID, node.ParentID)] = true
		}
	}
	return pairs
}

func sqliteGraphApplyDepPairKey(issueID, dependsOnID string) string {
	return issueID + "\x00" + dependsOnID
}

func sqliteGraphApplyCycleRelevantDependencyType(depType string) bool {
	return depType == "blocks" || depType == "conditional-blocks"
}

// DepMetadata returns the opaque metadata retained for one Graph dependency.
// The deployed deps table has no metadata column, so GraphApply stores this
// migration-compatible sidecar in kv. It is intentionally a narrow SQLite
// projection rather than an expansion of the stable Dep wire model.
func (s *SQLiteStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	if err := s.ensureOpen(); err != nil {
		return "", false, err
	}
	var depType string
	err := s.readDB.QueryRowContext(context.Background(), `
		SELECT dep_type FROM deps WHERE issue_id=? AND depends_on_id=?`, issueID, dependsOnID,
	).Scan(&depType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading Graph dependency metadata type %s -> %s: %w", issueID, dependsOnID, err)
	}
	var metadata string
	err = s.readDB.QueryRowContext(context.Background(), `SELECT value FROM kv WHERE key=?`, sqliteGraphEdgeMetadataKey(issueID, dependsOnID, depType)).Scan(&metadata)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading Graph dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
	}
	return metadata, true, nil
}

func sqliteGraphEdgeMetadataKey(issueID, dependsOnID, depType string) string {
	return sqliteGraphEdgeMetadataKVPrefix + sqliteGraphEdgeMetadataPart(issueID) + "/" + sqliteGraphEdgeMetadataPart(dependsOnID) + "/" + sqliteGraphEdgeMetadataPart(depType)
}

func sqliteGraphEdgeMetadataPairPrefix(issueID, dependsOnID string) string {
	return sqliteGraphEdgeMetadataKVPrefix + sqliteGraphEdgeMetadataPart(issueID) + "/" + sqliteGraphEdgeMetadataPart(dependsOnID) + "/"
}

func sqliteGraphEdgeMetadataPart(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// clearGraphEdgeMetadataForBeadsTx removes Graph-only edge metadata for every
// dependency that touches one of ids. Dependency rows are stored separately,
// so callers use this in the same transaction that removes those rows.
func (s *SQLiteStore) clearGraphEdgeMetadataForBeadsTx(ctx context.Context, tx *sql.Tx, ids []string) error {
	// No capacity hint: the id lists here are small, and arithmetic inside an
	// allocation size is what code scanners flag as overflow-prone.
	var clauses []string
	var args []any
	for _, id := range ids {
		if id == "" {
			continue
		}
		part := sqliteGraphEdgeMetadataPart(id)
		// Base64 URL encoding can contain "_", which SQL LIKE treats as a
		// wildcard. GLOB's only wildcard metacharacters are absent from the
		// encoding alphabet, so these patterns match encoded IDs exactly.
		clauses = append(clauses, "key GLOB ?", "key GLOB ?")
		args = append(args,
			sqliteGraphEdgeMetadataKVPrefix+part+"/*",
			sqliteGraphEdgeMetadataKVPrefix+"*/"+part+"/*",
		)
	}
	if len(clauses) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE `+strings.Join(clauses, " OR "), args...); err != nil {
		return fmt.Errorf("clearing Graph dependency metadata for deleted beads: %w", err)
	}
	return nil
}
