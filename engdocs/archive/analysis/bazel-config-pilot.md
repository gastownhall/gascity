# Bazel config pilot boundary

This experiment keeps the existing Go test path authoritative and carries the
working pure-Go contract target from the first pilot. A config `go_library`
requires the production import closure (32 internal packages on the current
`origin/main` checkout), including generated/embed and filesystem-sensitive
packages. Repository-wide Gazelle generation was intentionally not used: it
creates BUILD files for unrelated packages and can mutate source-adjacent
metadata.

To reproduce the closure inventory:

```sh
go list -test -deps ./internal/config \
  | awk '$1 ~ /^github.com\/gastownhall\/gascity\/internal\// && $1 !~ /\.test$/ \
      {sub("github.com/gastownhall/gascity/", "", $1); print $1}' \
  | sort -u
```

The resulting list is the review input for the next slice. Until each package
has explicit deps and declared data, `go test ./internal/config` remains the
only complete config validation; no partial Bazel config target is claimed.

The pilot includes `//internal/config:config_envname_test`, a deliberately
small feasibility target over the real `envname.go` production source. It is
not a replacement for the full config package or its 85-test suite.
