# Stale Per-Worktree gc Binaries

## The problem

When git worktrees are created for polecat agents, the `gc` binary
that created the worktree may be copied into the worktree directory.
If the canonical `gc` binary is later rebuilt (e.g., `make install`
from a newer commit), the per-worktree copies are **not** refreshed.

Any `gc` invocation that initializes a bead provider calls
`installBeadHooks`, which writes hook scripts from templates compiled
into that specific binary. A stale binary writes old templates,
silently overwriting hooks installed by the canonical (newer) binary.

## The fix: forward-only hook stamps

Each hook script now contains a version stamp:

```sh
#!/bin/sh
# gc-hook-stamp: 2026-04-29T10:01:25Z abc1234
```

`installBeadHooks` reads the on-disk stamp before writing. If the
on-disk hook was installed by a newer binary (later build date), the
write is skipped. This makes hook installation forward-only — stale
binaries cannot regress hook content.

Rules:
- Dev builds (`date=unknown`) always write (most permissive for development)
- Legacy hooks (no stamp) are always upgraded
- Stamped hooks are only overwritten by equal-or-newer builds

## Remaining gap: stale binaries themselves

The version stamp prevents stale hooks, but stale per-worktree `gc`
binaries still exist. They will silently use old versions of any
*other* compiled-in template or behavior. The stamp only protects
hook scripts.

To fully resolve this, per-worktree `gc` binaries should be either:
- **Symlinked** to the canonical binary (so `make install` updates them)
- **Replaced with thin shims** that `exec` the canonical binary
- **Refreshed** by a post-install step

Until one of these is implemented, `make install` alone does not
propagate to worktrees. After rebuilding `gc`, manually update or
remove stale copies:

```bash
find .gc/worktrees -name gc -type f | while read f; do
  ln -sf "$(which gc)" "$f"
done
```
