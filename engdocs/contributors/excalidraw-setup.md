---
title: Excalidraw Setup
description: How to install and use Excalidraw to author diagrams — MCP server for authoring, CLI for rendering to SVG, plus a shared palette.
---

This page explains how to set up Excalidraw end-to-end so that diagrams can be authored conversationally from a Claude Code session and committed as `.excalidraw` source files plus rendered SVGs.

You can either follow the steps yourself, or paste the [prompt at the bottom](#prompt-for-another-claude-session) into a fresh Claude session and have it do the setup for you.

## What you get

- `.excalidraw` JSON sources committed to the repo, editable in [excalidraw.com](https://excalidraw.com) and through the Excalidraw MCP server.
- Rendered `.svg` files committed alongside, so docs pages render without a build step.
- A `make` target that regenerates only the SVGs whose source has changed.

## 1. Install — two parts

### (a) Excalidraw MCP server

The MCP server lets Claude Code author and edit `.excalidraw` files conversationally. It's HTTP-based, so there is no local install. Add it to the repo's `.mcp.json`:

```json
{
  "mcpServers": {
    "excalidraw": {
      "type": "http",
      "url": "https://mcp.excalidraw.com"
    }
  }
}
```

Restart Claude Code after saving `.mcp.json` so the MCP server is picked up. You should see tools named `mcp__excalidraw__*` become available.

> **Note:** The official Excalidraw MCP can render diagrams inline and upload them to excalidraw.com, but **cannot write files to disk**. That is why a separate CLI is needed for SVG output (next step).

### (b) CLI renderer (`.excalidraw` → SVG)

Rendering is handled by [`@swiftlysingh/excalidraw-cli`](https://www.npmjs.com/package/@swiftlysingh/excalidraw-cli), run via `npx` — no global install needed. Requires Node 18+.

Smoke-test the renderer:

```bash
npx -y @swiftlysingh/excalidraw-cli --help
```

## 2. Folder layout

```
docs/diagrams/
  excalidraw/             # source .excalidraw files (JSON, committed)
  excalidraw-rendered/    # generated .svg files (also committed)
```

Both directories are committed. The SVGs are committed so that docs pages render without a build step in production.

## 3. Makefile target

Add a `diagrams-excalidraw` target that renders every `.excalidraw` to SVG, skipping files whose SVG is already up-to-date (mtime check):

```makefile
diagrams-excalidraw:
	@set -e; \
	src_dir=docs/diagrams/excalidraw; \
	out_dir=docs/diagrams/excalidraw-rendered; \
	mkdir -p "$$out_dir"; \
	shopt -s nullglob 2>/dev/null || true; \
	rendered=0; \
	for f in "$$src_dir"/*.excalidraw; do \
	  [ -e "$$f" ] || continue; \
	  base=$$(basename "$$f" .excalidraw); \
	  out="$$out_dir/$$base.svg"; \
	  if [ ! -e "$$out" ] || [ "$$f" -nt "$$out" ]; then \
	    echo "excalidraw -> $$out"; \
	    npx -y @swiftlysingh/excalidraw-cli convert "$$f" --format svg --output "$$out"; \
	    rendered=$$((rendered+1)); \
	  fi; \
	done; \
	echo "excalidraw: rendered $$rendered file(s)"
```

## 4. Authoring workflow

When asking Claude to draw or modify a diagram:

1. Use the Excalidraw MCP tools (`mcp__excalidraw__*`) to compose the diagram interactively.
2. Export with `mcp__excalidraw__export_to_excalidraw` to write the JSON source into `docs/diagrams/excalidraw/<name>.excalidraw`.
3. Run `make diagrams-excalidraw` to regenerate the SVG.
4. Reference the SVG from any Markdown/MDX page:

   ```markdown
   ![Alt text](/diagrams/excalidraw-rendered/<name>.svg)
   ```

   Adjust the path to match your docs framework's static-asset rules.

## 5. Style guide — shared palette

Use this consistent pastel palette across all diagrams so that recurring concepts get the same colour treatment.

**Node fill / stroke:**

| Role | Fill | Stroke |
| --- | --- | --- |
| Blue — entry / user-facing | `#a5d8ff` | `#1e1e1e` |
| Yellow — decision / gateway | `#fff3bf` | `#1e1e1e` |
| Orange — intermediate processing | `#ffd8a8` | `#1e1e1e` |
| Purple — compute / app tier | `#d0bfff` | `#1e1e1e` |
| Green — success / persistence | `#b2f2bb` | `#1e1e1e` |
| Mint — read-only / replica | `#c3fae8` | `#1e1e1e` |
| Red/pink — error / cache | `#ffc9c9` | `#1e1e1e` |

**Container fills** are very-light versions of the same hues with a saturated matching stroke (e.g. `#e5dbff` fill with `#8b5cf6` stroke for a purple compute group).

**Edge colours:**

- Success / healthy: `#22c55e`
- Failure: `#ef4444`
- Retry / neutral: `#757575`

## 6. Verify the round-trip

Create a small test diagram to confirm the toolchain works:

1. Use the Excalidraw MCP to draw a 3-node flow: Start (blue oval) → Process (orange rectangle) → Done (green rectangle).
2. Export it to `docs/diagrams/excalidraw/hello.excalidraw`.
3. Run `make diagrams-excalidraw`.
4. Confirm `docs/diagrams/excalidraw-rendered/hello.svg` exists and renders correctly.

## Prompt for another Claude session

If you want a fresh Claude Code session to do the setup for you, paste the prompt below:

````md
I want to author diagrams in **Excalidraw** and commit both the source and rendered SVGs. Set up the two pieces below, then create one sample diagram so I can verify the round-trip.

## 1. Install — two parts

**(a) Excalidraw MCP server** — lets you author/edit `.excalidraw` files conversationally from Claude. It's HTTP, no local install. Add to the repo's `.mcp.json`:

```json
{
  "mcpServers": {
    "excalidraw": {
      "type": "http",
      "url": "https://mcp.excalidraw.com"
    }
  }
}
```

After saving `.mcp.json`, tell me to restart Claude Code so the MCP server is picked up. The official MCP can render inline and upload to excalidraw.com but **cannot write files to disk** — that is why we need part (b).

**(b) CLI renderer for `.excalidraw` → SVG** — `@swiftlysingh/excalidraw-cli`, run via `npx` (no global install). Requires Node 18+.

Smoke-test the renderer:

```bash
npx -y @swiftlysingh/excalidraw-cli --help
```

## 2. Folder layout + Makefile target

```
docs/diagrams/
  excalidraw/             # source .excalidraw files (JSON, committed)
  excalidraw-rendered/    # generated .svg files (also committed)
```

Add a `diagrams-excalidraw` target to the `Makefile` that renders every `.excalidraw` to SVG, skipping files whose SVG is already up-to-date (mtime check). Use this exact recipe:

```makefile
diagrams-excalidraw:
	@set -e; \
	src_dir=docs/diagrams/excalidraw; \
	out_dir=docs/diagrams/excalidraw-rendered; \
	mkdir -p "$$out_dir"; \
	shopt -s nullglob 2>/dev/null || true; \
	rendered=0; \
	for f in "$$src_dir"/*.excalidraw; do \
	  [ -e "$$f" ] || continue; \
	  base=$$(basename "$$f" .excalidraw); \
	  out="$$out_dir/$$base.svg"; \
	  if [ ! -e "$$out" ] || [ "$$f" -nt "$$out" ]; then \
	    echo "excalidraw -> $$out"; \
	    npx -y @swiftlysingh/excalidraw-cli convert "$$f" --format svg --output "$$out"; \
	    rendered=$$((rendered+1)); \
	  fi; \
	done; \
	echo "excalidraw: rendered $$rendered file(s)"
```

## 3. Authoring workflow

When I ask you to draw or modify a diagram:

1. Use the Excalidraw MCP tools (`mcp__excalidraw__*`) to compose it interactively.
2. Export with `mcp__excalidraw__export_to_excalidraw` to write the JSON source into `docs/diagrams/excalidraw/<name>.excalidraw`.
3. Run `make diagrams-excalidraw` to regenerate the SVG.
4. Reference the SVG from any Markdown/MDX page as `![alt](/diagrams/excalidraw-rendered/<name>.svg)` (adjust path to match your docs framework).

## 4. Style guide for the diagrams themselves

Use this consistent pastel palette across all diagrams (background / stroke):

- Blue (entry / user-facing): `#a5d8ff` / `#1e1e1e`
- Yellow (decision / gateway): `#fff3bf` / `#1e1e1e`
- Orange (intermediate processing): `#ffd8a8` / `#1e1e1e`
- Purple (compute / app tier): `#d0bfff` / `#1e1e1e`
- Green (success / persistence): `#b2f2bb` / `#1e1e1e`
- Mint (read-only / replica): `#c3fae8` / `#1e1e1e`
- Red/pink (error / cache): `#ffc9c9` / `#1e1e1e`
- Container fills are very-light versions of the same hues (e.g. `#e5dbff` with `#8b5cf6` stroke).

For edges: green `#22c55e` for success/healthy, red `#ef4444` for failure, grey `#757575` for retry/neutral.

## 5. Verify

Create `docs/diagrams/excalidraw/hello.excalidraw` containing a 3-node flow (Start → Process → Done) using the palette above, run `make diagrams-excalidraw`, and show me the generated SVG path. Stop there — I will review before we add more.
````
