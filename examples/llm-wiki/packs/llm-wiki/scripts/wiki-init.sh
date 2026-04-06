#!/usr/bin/env bash
# wiki-init.sh — Create the wiki directory structure with starter files.
#
# Usage: wiki-init.sh [wiki_root]
#   wiki_root defaults to "wiki" in the current directory.

set -euo pipefail

WIKI_ROOT="${1:-wiki}"

if [ -d "$WIKI_ROOT" ]; then
  echo "Wiki directory already exists at $WIKI_ROOT — skipping."
  exit 0
fi

mkdir -p "$WIKI_ROOT/pages"

cat > "$WIKI_ROOT/index.md" <<'EOF'
# Wiki Index

Master catalog of all wiki pages. Maintained by the archivist.

<!-- Add pages below as: - [Page Title](pages/path/to/page.md) — One-line summary -->
EOF

cat > "$WIKI_ROOT/log.md" <<'EOF'
# Wiki Activity Log

Chronological record of all wiki changes. Append-only.

<!-- Format: YYYY-MM-DDTHH:MM:SS | Action | Source | Affected Pages -->
EOF

cat > "$WIKI_ROOT/schema.md" <<'EOF'
# Wiki Schema

This file defines how wiki pages are organized. It is provided by
the consumer pack and should not be modified by the archivist.

## Default Schema

Until a domain-specific schema is provided, use a flat structure:

```
pages/
└── <entity-name>.md
```

Replace this file with a domain-specific schema that defines:
- Page categories and directory structure
- Naming conventions for pages
- Required sections within each page type
- Cross-reference conventions
EOF

echo "Wiki initialized at $WIKI_ROOT"
