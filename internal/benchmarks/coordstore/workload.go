package coordstore

import (
	"time"
)

// WorkloadConfig describes a benchmark workload in terms of the traffic
// mix observed on the live gascity HQ store (S2-S4 findings).
//
// All rate fields are operations per second (float64 allows sub-1/s rates).
// Zero means the operation is not exercised.
type WorkloadConfig struct {
	// Name is a human-readable identifier used in scorecard output.
	Name string

	// --- Seed population ---
	// These describe the state the store is in when the workload begins.

	// MainOpenCount is the number of open main-tier records to pre-populate.
	// Live HQ: ~200 open tasks/sessions (S2).
	MainOpenCount int
	// MainClosedCount is the number of closed main-tier records (dead weight).
	// Live HQ: ~21,286 closed tasks (S2). Included to exercise index selectivity.
	MainClosedCount int
	// WispOpenCount is the number of open ephemeral records to pre-populate.
	// Live HQ: ~6,400 open mail/order wisps (S2).
	WispOpenCount int
	// DepEdgeCount is the number of dependency edges to pre-populate.
	// Live HQ: ~13 edges on HQ (sparse); S1 §V.3.
	DepEdgeCount int

	// --- Traffic mix (operations per second) ---

	// MailPollRate is the rate of ephemeral filter scans (FR-8).
	// This is the single hottest read path: ~150/s on live HQ (S3 R1).
	// Each poll scans all open wisps by (type=message, status=open, assignee=X).
	MailPollRate float64

	// PointReadRate is the rate of Get operations (FR-3).
	// Live HQ: ~143 new connections/s (S3); each carries ~8 setup queries.
	// FR-16 (zero-fork) eliminates connection overhead; this measures pure read.
	PointReadRate float64

	// FilterScanRate is the rate of main-tier filter scans (FR-2).
	// Live HQ: per-agent per reconcile tick (bd ready), ~20 agents polling
	// every 30–120s ≈ ~0.2/s aggregate. Lower than mail-poll.
	FilterScanRate float64

	// CreateRate is the rate of Create operations (FR-1).
	// Live HQ: ~20 tasks/day + ~3,500 order wisps/day ≈ ~0.04/s each.
	// Combined sustained: ~2.2 writes/s (S4).
	CreateRate float64

	// UpdateRate is the rate of Update operations (FR-1).
	// Live HQ: each bead updated ~3× before close; ~19 updates/min ≈ 0.3/s.
	UpdateRate float64

	// SetMetadataRate is the rate of SetMetadataBatch operations (FR-5).
	// Live HQ: high on session transitions. Estimate ~3/s.
	SetMetadataRate float64

	// BatchGetRate is the rate of BatchGet operations (FR-4).
	// Live HQ: label/dep hydration on every list result. ~10/s estimate.
	BatchGetRate float64

	// ReadyRate is the rate of Ready() queries (FR-9).
	// Live HQ: once per reconcile tick per agent, ~0.2/s.
	ReadyRate float64

	// DepOpRate is the rate of dep add/remove operations (FR-10).
	// Live HQ: sparse; ~0.1/s.
	DepOpRate float64

	// RecentScanRate is the rate of RecentScan operations (FR-18).
	// Live HQ: used for inbox-replay / archive; ~0.02/s.
	RecentScanRate float64

	// PurgeTerminalRate is the rate of main-tier terminal retention sweeps.
	// Zero disables retention cleanup in the workload driver.
	PurgeTerminalRate float64

	// PurgeTerminalOlderThan is the TTL for terminal main-tier records.
	// Records in terminal statuses with older last-update times are purged.
	PurgeTerminalOlderThan time.Duration

	// --- Run parameters ---

	// Duration is how long the workload driver runs.
	Duration time.Duration

	// Concurrency is the number of concurrent goroutines issuing requests.
	// Matches the number of concurrent agents in a typical HQ city (~20).
	Concurrency int
}

// MailAssignees is the set of assignees used in mail-poll simulation.
// Matches the ~20-agent population size observed in the discovery rig without
// baking any user-defined role names into SDK code.
var MailAssignees = []string{
	"rig/agent-01", "rig/agent-02", "rig/agent-03", "rig/agent-04",
	"rig/agent-05", "rig/agent-06", "rig/agent-07", "rig/agent-08",
	"rig/agent-09", "rig/agent-10", "rig/agent-11", "rig/agent-12",
	"rig/agent-13", "rig/agent-14", "rig/agent-15", "rig/agent-16",
	"rig/agent-17", "rig/agent-18", "rig/agent-19", "rig/agent-20",
}

// RealWorldWorkload mirrors the traffic mix measured on the live gascity HQ
// store (S2 volume census, S3 read profile, S4 write profile).
// This is the primary benchmark that determines whether a backend meets
// the discovery.md targets under realistic conditions.
var RealWorldWorkload = WorkloadConfig{
	Name: "realworld",

	MainOpenCount:   200,
	MainClosedCount: 21000,
	WispOpenCount:   6400,
	DepEdgeCount:    13,

	// Traffic mix from S3/S4 — read:write ≈ 265:1
	MailPollRate:      150.0, // dominant hot path (S3 R1)
	PointReadRate:     40.0,  // bd show / cache miss (S3 R5)
	FilterScanRate:    0.2,   // bd ready per reconcile (S3 R2)
	CreateRate:        1.5,   // tasks + order wisps (S4 W1, W8)
	UpdateRate:        0.3,   // bead field changes (S4 W2)
	SetMetadataRate:   3.0,   // session transitions (S4 W3)
	BatchGetRate:      10.0,  // label/dep hydration (S3 R4)
	ReadyRate:         0.2,   // reconcile tick (S3 R2)
	DepOpRate:         0.1,   // dep add/remove (S4 W5 approx.)
	RecentScanRate:    0.02,  // inbox-replay (S3 R11 proxy)
	PurgeTerminalRate: 0.033, // retention sweep, about once every 30s

	PurgeTerminalOlderThan: 10 * time.Minute,

	Duration:    30 * time.Second,
	Concurrency: 20,
}

// StressWorkload drives the store at the burst throughput target from
// discovery.md (500 reads/s). Used to verify the store holds up under peak.
var StressWorkload = WorkloadConfig{
	Name: "stress",

	MainOpenCount:   500,
	MainClosedCount: 25000,
	WispOpenCount:   10000,
	DepEdgeCount:    50,

	// 500 reads/s burst target (S3 throughput targets)
	MailPollRate:    300.0,
	PointReadRate:   150.0,
	FilterScanRate:  10.0,
	CreateRate:      5.0,
	UpdateRate:      2.0,
	SetMetadataRate: 5.0,
	BatchGetRate:    30.0,
	ReadyRate:       2.0,
	DepOpRate:       0.5,
	RecentScanRate:  0.1,

	Duration:    15 * time.Second,
	Concurrency: 50,
}

// SmokeWorkload is a short low-volume workload used in CI to verify
// correctness and latency without the full 30s run time.
var SmokeWorkload = WorkloadConfig{
	Name: "smoke",

	MainOpenCount:   50,
	MainClosedCount: 200,
	WispOpenCount:   200,
	DepEdgeCount:    5,

	MailPollRate:    5.0,
	PointReadRate:   5.0,
	FilterScanRate:  0.5,
	CreateRate:      0.5,
	UpdateRate:      0.2,
	SetMetadataRate: 0.5,
	BatchGetRate:    1.0,
	ReadyRate:       0.5,
	DepOpRate:       0.1,
	RecentScanRate:  0.1,

	Duration:    5 * time.Second,
	Concurrency: 5,
}
