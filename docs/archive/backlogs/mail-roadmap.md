---
title: "Mail Roadmap"
---

Tracks the full Gastown mail feature set and when we expect to need each
piece. Nothing here is speculative — every feature exists in Gastown
production. The question is ordering.

## Phase 1 — Basic Mail ✓

Minimum viable mail. Human ↔ agent conversation, agents check inbox in loop.
**Implemented.**

| Feature | Status |
|---------|--------|
| Mail = bead with type "message" | ✓ |
| `gc mail send <to> -s "subject" -m "body"` | ✓ |
| `gc mail inbox [agent]` | ✓ |
| `gc mail read <id>` (marks read, keeps open) | ✓ |
| `gc mail peek <id>` (view without marking read) | ✓ |
| `from` / `to` fields | ✓ |
| Unread tracking via "read" label | ✓ |
| Implicit "human" sender/recipient | ✓ |
| Validate recipient exists | ✓ |

## Phase 2 — Agent-to-Agent Coordination ✓

**Implemented.** Subject/body separation, threading, and reply.

| Feature | Status |
|---------|--------|
| Agent → agent mail | ✓ |
| `-s` / `--subject` flag | ✓ |
| Reply-to / threading via labels | ✓ |
| `gc mail reply <id> -s "..." -m "..."` | ✓ |
| `gc mail thread <thread-id>` | ✓ |

## Phase 3 — Message Lifecycle ✓

**Implemented.** Read/unread toggle, archive, delete, count.

| Feature | Status |
|---------|--------|
| `gc mail archive <id>` | ✓ |
| `gc mail delete <id>` | ✓ |
| `gc mail mark-read <id>` | ✓ |
| `gc mail mark-unread <id>` | ✓ |
| `gc mail count [agent]` | ✓ |
| Wisps (ephemeral, default) | Deferred — needs patrol/cleanup |
| `--permanent` flag | Deferred — needs wisps first |
| Pinned messages | Deferred — needs context cycling |
| Stale message archival | Deferred — needs session restart awareness |

## Phase 4 — Priority & Urgency

When health patrol exists and can act on priority.

| Feature | Status |
|---------|--------|
| Priority field in Message struct | ✓ (field exists, CLI not yet exposed) |
| CC field in Message struct | ✓ (field exists, CLI not yet exposed) |
| `--urgent` flag | Deferred — needs priority CLI |
| Nudge on send (`--notify`) | ✓ |
| Idle-aware notification | Deferred — needs tmux idle detection |
| Nudge enqueue for busy agents | Deferred — needs nudge queue |
| Priority-stratified inbox check | Deferred — needs priority CLI |

## Phase 5 — Routing & Groups

When multi-project and team packs exist.

| Feature | Why deferred |
|---------|-------------|
| Queue messages (claiming) | Needs ephemeral worker pools |
| `gc mail claim` / `gc mail release` | Queue consumer commands |
| Announce/channel (broadcast) | Needs subscriber concept |
| @group expansion (@town, @rig) | Needs project scoping |
| CC recipients CLI | CC field exists; CLI support deferred |
| List addresses (fan-out) | Needs messaging.json config |

## Phase 6 — Delivery Guarantees

When reliability matters at scale.

| Feature | Why deferred |
|---------|-------------|
| Two-phase delivery (pending → acked) | Needs delivery tracking |
| Idempotent ack with timestamp reuse | Needs two-phase first |
| Bounded concurrent acks | Optimization for scale |
| `--no-notify` / suppress notify | Needs nudge infrastructure |
| DND / muted agents | Needs health config |

## Gastown Features We May Never Need

These exist in Gastown but may not apply to Gas City's model.

| Feature | Reason |
|---------|--------|
| Legacy JSONL storage | Gas City is beads-only |
| Crew-specific inbox paths | No hardcoded roles |
| `gt mail search` | Nice to have, not essential |
| Message type field (task/scavenge/notification/reply) | May not need structured types |
