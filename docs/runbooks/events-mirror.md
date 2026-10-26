---
title: Ship the Event Log
description: Mirror a city's event log outside .gc so a log shipper such as the OpenTelemetry Collector can read it.
---

## Overview

Every city records its events in `.gc/events.jsonl`. gc keeps `.gc` at mode
`0700` and resets it to `0700` at every controller start, so a log shipper
running as another user cannot read the file there, and an ACL on `.gc` does
not survive a restart.

`[events.mirror]` makes gc write a copy of every event line to a file you
choose, outside `.gc`. The canonical `.gc/events.jsonl` does not change and is
still what gc reads. The mirror is write-only from gc's side: it exists for
the shipper.

```toml
[events.mirror]
path = "/var/log/gascity/hq/events.jsonl"
# mode = "0640"              # default
# max_size_bytes = 104857600 # default (100 MiB); 0 = rotate it yourself
```

Leave the section out, or leave `path` empty, and gc behaves exactly as it
does without the feature.

## How It Works

- **Who writes it.** The city's controller (the supervisor running the city,
  or a standalone `gc start` holding `.gc/controller.lock`) tails the canonical
  log and appends each event to the mirror. So the mirror holds every event,
  including those written by CLI commands and agents, a fraction of a second
  after they reach the canonical log. A lock in `.gc` keeps it to one writer.
- **Content.** Each mirror line is the canonical event line, in the same JSON
  encoding, with no added fields. The canonical log's rotation anchors
  (`events.rotated`) are mirrored too. Nothing is redacted: event subjects and
  messages can name beads, agents and paths, so filter in the shipper if they
  must not leave the host.
- **Resuming.** After a restart or a failure the controller continues after
  the last event the mirror already holds (reading `<path>` or, right after a
  rotation, `<path>.1`), so it neither duplicates nor skips events the
  canonical log still has. A new, empty mirror starts with the events
  recorded since the controller opened the log; older history is not
  replayed.
- **Failures never reach the city.** If the directory is missing, permission
  is denied or the disk is full, gc reports it on the controller's stderr
  (once a minute while it lasts), retries every 10 seconds and reports when it
  recovers. Event recording and the controller are unaffected.
- **Size.** When the next line would take the mirror past `max_size_bytes`, gc
  renames it to `<path>.1` (replacing the previous `.1`) and starts a new
  file, so the mirror uses at most about twice that on disk. With
  `max_size_bytes = 0` gc never rotates the mirror and you may rotate it with
  logrotate, either `create` (rename it away; gc reopens the path) or
  `copytruncate` (gc appends with `O_APPEND`, so writes continue at the new
  end).
- **Changes** to `[events.mirror]` take effect at the next controller start.

## Permissions

gc creates only the mirror file. It never creates the directory, and never
changes the directory's owner or mode, so provision the directory first:

- give it the shipper's group and the setgid bit (mode `2750`), so every file
  created in it, including `<path>.1` and a fresh file after rotation, gets
  the shipper's group;
- every directory above it must be traversable (`x`) by the shipper.

gc creates the file with `mode` (default `0640`: owner read-write, group
read) and sets that mode on each open, whatever the umask. gc does not
`chown`; the group comes from the setgid directory.

`path` must be absolute and outside the canonical log's directory. An
invalid `[events.mirror]` is a config warning: the mirror is disabled and the
city runs normally.

## Example: OpenTelemetry Collector on a Shared Host

The city runs as `ubuntu` under `/data/cities/hq`, `/data` is `0700`, and the
collector runs as user and group `gcotel`.

1. Provision the directory (as root, for example from configuration
   management):

   ```sh
   install -d -o ubuntu -g gcotel -m 2750 /var/log/gascity
   install -d -o ubuntu -g gcotel -m 2750 /var/log/gascity/hq
   ```

2. Point the city at it in `city.toml` and restart the controller:

   ```toml
   [events.mirror]
   path = "/var/log/gascity/hq/events.jsonl"
   ```

3. Read it with the collector's `filelog` receiver. Include `.1` so lines
   written just before a rotation are not missed; `filelog` fingerprints
   files, so a renamed file is not read twice:

   ```yaml
   receivers:
     filelog/gascity:
       include:
         - /var/log/gascity/*/events.jsonl
         - /var/log/gascity/*/events.jsonl.1
       start_at: beginning
       include_file_path: true
       operators:
         - type: json_parser
           timestamp:
             parse_from: attributes.ts
             layout_type: gotime
             layout: "2006-01-02T15:04:05.999999999Z07:00"
   ```

   Use the file path (`log.file.path`) or the city directory name to tell
   cities apart.

Each city needs its own mirror directory or file name; two cities must never
share one mirror path.
