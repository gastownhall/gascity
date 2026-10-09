#!/usr/bin/env python3
"""THROWAWAY: request latency from this runner on warm HTTPS keep-alive
connections: gcping per-region Cloud Run endpoints and GCS object reads
(404 metadata lookup and 1-byte range read). Prints medians in ms."""
import http.client, json, statistics, time, sys

def warm(host, path, n=15, headers=None):
    c = http.client.HTTPSConnection(host, timeout=10)
    c.request("GET", path, headers=headers or {}); r = c.getresponse(); r.read()
    ts = []
    for _ in range(n):
        t = time.monotonic(); c.request("GET", path, headers=headers or {}); r = c.getresponse(); r.read()
        ts.append((time.monotonic() - t) * 1000)
    c.close()
    return round(statistics.median(ts), 1), r.status

def connect(host, n=10):
    import socket
    ts = []
    for _ in range(n):
        t = time.monotonic(); s = socket.create_connection((host, 443), timeout=5); ts.append((time.monotonic()-t)*1000); s.close()
    return round(statistics.median(ts), 1)

out = {}
out["tcp_rbe-west"] = connect("rbe-west.ops.gascity.com")
out["tcp_storage.googleapis.com"] = connect("storage.googleapis.com")
eps = json.load(__import__("urllib.request").request.urlopen("https://global.gcping.com/api/endpoints", timeout=10))
for k, v in eps.items():
    if k.startswith("us-"):
        out["gcping_" + k] = warm(v["URL"].split("//")[1], "/api/ping")[0]
out["gcs_404_bazel-mirror"] = warm("storage.googleapis.com", "/bazel-mirror/rrc-proto-does-not-exist")
out["gcs_range_bazel-mirror"] = warm("storage.googleapis.com", "/bazel-mirror/github.com/bazelbuild/bazel-skylib/releases/download/1.7.1/bazel-skylib-1.7.1.tar.gz", headers={"Range": "bytes=0-0"})
for k, v in out.items():
    print(f"{k}: {v}")
best = min(v for k, v in out.items() if k.startswith("gcping_"))
print(f"GCS_EST_MS={best}")
