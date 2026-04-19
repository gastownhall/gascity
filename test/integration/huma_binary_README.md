# Huma binary integration test

`huma_binary_test.go` exercises the whole stack through the real `gc`
binary: it builds `cmd/gc`, starts `gc supervisor run` in an isolated
`GC_HOME`, waits for the HTTP listener, fetches `/openapi.json`, and
then invokes `gc cities` as a subprocess. If the binary compiles,
the Huma router boots, and the supervisor's HTTP socket is live, the
test passes.

It catches the class of bug that unit tests and in-process
round-trips cannot: binary wiring — build flags, command
registration, environment handling, supervisor bootstrap ordering,
socket paths. It's a smoke test, not a behavioral one.

Run it with:

```bash
go test -tags=integration ./test/integration/ -run TestHumaBinary
```

Or via the `make test-integration-huma` target.

The test is guarded by `//go:build integration` so it doesn't run in
the default `go test ./...` pass. It takes ~2 seconds on a warm
machine (most of that is `go build`).

### Platform notes

macOS caps AF_UNIX paths at ~104 characters. The test puts
`XDG_RUNTIME_DIR` under `/tmp/gcit-<random>` rather than the default
long `t.TempDir()` path so the supervisor's `supervisor.sock` path
stays under the limit. On Linux the XDG override is still used for
isolation; the path length is not a concern there.
