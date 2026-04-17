#!/bin/bash
# Verify a Python 3.11+ interpreter is available.
# gc import scripts use tomllib, which is stdlib in 3.11+.
set -e

# Try common interpreter names in order. The scripts use #!/usr/bin/env python3.11
# by preference, falling back to python3 if it's 3.11+.
for candidate in python3.11 python3.12 python3.13 python3; do
    if command -v "$candidate" >/dev/null 2>&1; then
        VERSION=$("$candidate" -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")' 2>/dev/null)
        MAJOR=$("$candidate" -c 'import sys; print(sys.version_info.major)' 2>/dev/null)
        MINOR=$("$candidate" -c 'import sys; print(sys.version_info.minor)' 2>/dev/null)
        if [ "$MAJOR" -ge 3 ] && [ "$MINOR" -ge 11 ]; then
            echo "OK: $candidate ($VERSION)"
            exit 0
        fi
    fi
done

echo "FAIL: no Python 3.11+ found in PATH (need tomllib from stdlib)"
echo "  Install with: brew install python@3.11"
exit 1
