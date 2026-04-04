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

## init-incomplete-gitignore
<!-- gh:gastownhall/gascity#301 -->
[← cities.md: What's inside](cities.md#whats-inside)

`gc init` and `gc rig add` generate an incomplete `.gitignore`. The current generated content is:

```
.dolt/
*.db
.beads-credential-key
```

This leaves the user unclear about what (if anything) from `.beads/` or `.gc/` needs to go into their repo or be copied to make another city. Related to the broader state separation design in [#159](https://github.com/gastownhall/gascity/issues/159).

**Expected:** `gc init` generates a city-root `.gitignore` that covers `.gc/`, `.beads/`, and `hooks/`.

**Actual:** Only `.dolt/` internals are excluded. Users must manually add the rest.

**Suggestion:** Have `gc init` write a `.gitignore` aligned with the three-category model (definitions, local bindings, managed state).

## pack-vs-toplevel-defaults
[← cities.md: Packs](cities.md#packs)

A fresh city created by `gc init` has default content in both the top-level directories (`prompts/`, `formulas/`, `scripts/`) and in `packs/gastown/` and `packs/maintenance/`. It's unclear what the principle is for which defaults live at the top level vs. inside a pack.

**Question:** Is there a design principle governing what goes in the city's top-level directories vs. the gastown or maintenance packs? The tutorial currently says "packs are how Gas City ships defaults" but that's only partially true — a lot of defaults live outside of packs.

**Suggestion:** Either clarify the principle so the tutorial can explain it, or consolidate defaults into packs so the statement is accurate.
