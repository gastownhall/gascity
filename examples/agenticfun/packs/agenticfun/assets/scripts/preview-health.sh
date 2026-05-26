#!/usr/bin/env sh
set -eu

url="${GC_PREVIEW_URL:-}"
if [ -z "$url" ]; then
  echo "GC_PREVIEW_URL not set; preview health skipped"
  exit 0
fi

if command -v curl >/dev/null 2>&1; then
  curl -fsS --max-time 10 "$url" >/dev/null
  echo "preview reachable: $url"
  exit 0
fi

echo "curl not found; cannot check preview $url"
exit 0
