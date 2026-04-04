# Tutorial Issues

Issues discovered during tutorial editing. Each heading is an anchor referenced from tutorial sidebars. When filed to GitHub, add `<!-- gh:gastownhall/gascity#NNN -->` after the heading.

---

## sling-after-init
<!-- gh:gastownhall/gascity#286 -->
<!-- gh:gastownhall/gascity#287 -->
[← cities.md: Cities, Rigs, and Packs](cities.md#cities-rigs-and-packs)

`gc sling claude` or `gc sling mayor` on a new city fails to dispatch. The supervisor hasn't fully started the city yet — the tmux server may not be running when init returns. Subsequently, `gc session peek` returns "session not found" because the session bead hasn't been materialized.

**Expected:** `gc sling` and `gc session peek` work immediately after `gc init` completes.

**Actual:** No tmux server running. Sling either fails or silently drops the work. Peek can't find the session.

**Suggestion:** `gc init` step 8 should block until the city is actually accepting commands.

## init-no-gitignore
[← cities.md: What's inside](cities.md#whats-inside)

`gc init` doesn't generate a `.gitignore` for the city root. Users who version their city need to create one manually to exclude `.gc/`, `.beads/`, and `hooks/`.

**Expected:** `gc init` generates a `.gitignore` that excludes local state.

**Actual:** No `.gitignore` is created. The `.beads/` directory has its own internal `.gitignore`, but the city root doesn't.

**Suggestion:** Have `gc init` write a `.gitignore` with `.gc/`, `.beads/`, and `hooks/`.
