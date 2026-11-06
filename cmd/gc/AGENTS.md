# CLI implementation

Session-model ownership diagnostics inspect work separately from message beads:
mail assignees are delivery recipients and may name historical sessions. Resolve
known session aliases and alias history before treating an owner token as a
missing bead ID, and report failures from either coordination-class store.
