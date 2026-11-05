# OSC companion

Keep the companion pure Go and its tap within the existing 8 MiB resident-memory
and stream-throughput gates. Linux tap initialization disables huge pages for
its process before capture and re-execs once so runtime startup allocations
inherit that policy. Keep argv, environment and stdin intact; report a failed
initialization. Preserve caller heap
limits and the existing GC/parallelism policy. Relay and version do not run tap
memory initialization.

Use the built companion and the existing 80 MiB stream/RSS check for resource
evidence. A Go heap soft limit is not an RSS bound. Preserve the byte codec,
nonce-fenced publication, retry/coalescing and terminal relay contracts.
