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

## orders-toplevel-directory

[← 06-orders.md: A simple order](06-orders.md#a-simple-order)

Orders can live in two places: `formulas/orders/` (nested) or top-level `orders/` (peer). There is no semantic difference — same scanning, same priority, same behavior. But the two locations create unnecessary ambiguity, and they aren't even symmetrical: cities support both, packs only support `formulas/orders/`.

Top-level `orders/` is the better choice. Orders aren't formulas — they *reference* formulas. They're a peer concept (scheduling vs. workflow definition) and belong as a sibling directory alongside `formulas/`, `prompts/`, and `packs/`. Nesting them under `formulas/` misrepresents the relationship and adds a level of indirection for no reason.

**Suggestion:** Standardize on top-level `orders/` for both cities and packs. Deprecate `formulas/orders/`. This eliminates the arbitrary city-vs-pack divergence and picks the simpler, more conceptually honest location.

## orders-file-per-order

[← 06-orders.md: A simple order](06-orders.md#a-simple-order)

Each order currently requires its own subdirectory containing a single `order.toml`. No order directory in the codebase contains anything besides `order.toml` — the directory exists solely to provide the order name. This is unnecessary indirection.

Formulas use a flat file model: `formulas/pancakes.formula.toml`, where the filename provides the name. Orders should follow the same pattern: `orders/health-check.order.toml`. One file per order, filename is the name, no wrapper directories.

This is consistent with the formula convention, eliminates empty directories, and makes order definitions easier to browse (`ls orders/` shows all orders at a glance instead of a list of directories you have to drill into).

**Suggestion:** Adopt `orders/<name>.order.toml` as the canonical order file format. The `[order]` table wrapper inside the TOML can stay as-is. Deprecate the `orders/<name>/order.toml` subdirectory pattern.
