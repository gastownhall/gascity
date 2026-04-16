#!/bin/sh
case "${1:-}" in
  -h|--help)
    exec cat "$(dirname "$0")/help.md"
    ;;
esac
printf '%s\n' "mock: gc cli2 registry add"
