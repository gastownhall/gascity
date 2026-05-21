# PackV2 Engineering Design Notes

This directory contains PackV2 engineering design notes, rollout ledgers, and
historical reconciliation docs. These files are not user-facing product
documentation and should not be used as the primary source for authoring a new
city or pack.

Use these sources in order:

| Need | Source |
|---|---|
| Current user-facing migration guidance | `docs/guides/migrating-to-pack-vnext.md` |
| Current user-facing shareable-pack guidance | `docs/guides/shareable-packs.md` |
| Generated config reference | `docs/reference/config.md` |
| PackV2 rollout/design history | this directory |
| Current implementation truth | code and tests |

Some notes in this directory intentionally preserve aspirational or transitional
language so we can understand why earlier decisions were made. When these files
disagree with shipped behavior, prefer the public guides, generated reference,
and tests unless a current design PR explicitly says otherwise.
