---
title: "Cross-Machine Mail Routing"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: mail
current_state: not-implemented
priority: low
author: trillium
date: 2026-03-21
labels: [mail, messaging, cross-machine]
---

# Cross-Machine Mail Routing

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

Inter-agent mail is stored as beads in the beads store. If the beads store is shared
across machines (issue 005), mail routing may already work without additional changes.
This issue tracks the gap analysis and any work needed.

## Current State: Not Implemented (but may be free)

### What Exists

- **Beadmail provider** (`internal/mail/`): Default mail implementation
- **Storage**: Mail messages are beads with `type: "message"`
- **Addressing**: `agent-name` or `agent-name@rig`
- **Delivery**: Recipient checks their inbox by querying beads assigned to them

### How Mail Currently Works

```
Sender agent → bd mail send --to polecat-furiosa --subject "..."
    → Creates a bead with type=message, assignee=polecat-furiosa
Recipient agent → bd mail list
    → Queries beads where assignee=self AND type=message
```

### The Cross-Machine Question

Since mail is stored in beads, and beads are in Dolt, if Dolt is network-accessible
(issue 005), then:

1. Agent on mini3 sends mail → writes bead to shared Dolt on mini2
2. Agent on mini2 checks inbox → reads bead from same Dolt

**This should work today** if the Dolt server is reachable.

### What Might Be Missing

1. **Nudge delivery**: After sending mail, the sender typically nudges the recipient.
   Nudging requires the recipient's tmux session, which is on a different machine.
   The nudge needs to route through the remote transport layer.

2. **Agent addressing**: Current addressing (`agent-name@rig`) has no machine
   dimension. If the same rig has agents on different machines, addressing is
   ambiguous. May need `agent-name@rig@machine` or similar.

3. **Delivery confirmation**: No mechanism to confirm a remote agent received/read
   mail. Currently relies on the agent polling their inbox.

## Likely Outcome

Mail routing will probably work with no changes once beads are shared (005) and
nudge delivery works cross-machine (via remote transport, 003). The main work is
testing and verifying, not building new features.

## Action Items

1. Validate mail works over shared Dolt (manual test with two machines)
2. Ensure nudge delivery routes through hybrid provider to remote agents
3. Consider whether `@machine` addressing is needed (probably not initially)

## Dependencies

- [005 — Distributed Beads](005-distributed-beads.md) (mail storage)
- [003 — Remote Transport](003-remote-transport.md) (nudge delivery to remote agents)

## Dependents

- None
