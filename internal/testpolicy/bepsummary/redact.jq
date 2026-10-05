# Redacts a Bazel --build_event_json_file stream before it leaves the runner
# (bazel.yml uploads each lane's BEP file as an artifact). Run as
#
#   jq -R -c -f internal/testpolicy/bepsummary/redact.jq BEP_JSON
#
# A raw BEP file holds the expanded command line (optionsParsed,
# structuredCommandLine, unstructuredCommandLine: --remote_executor, the
# client key path, any --remote_header), progress output, workspace status
# and bytestream:// URIs that name the remote cache. This program keeps only
# the events and fields bepEvent (bep.go) decodes; nothing else is passed on.
# TestRedactProgramKeepsEveryDecodedField fails when bep.go reads a field
# this program drops.
#
# Input is read as raw lines (-R): a line that is not a JSON object (an
# interrupted Bazel leaves a partial last event) becomes the JSON string
# "unparsable BEP line", which the summary cannot decode either, so a bad
# last line still reads as truncated and a bad middle line still fails.

def present: with_entries(select(.value != null));

def keep(f): if . == null then null else f | present end;

def testid: keep({label, run, shard, attempt, configuration: (.configuration | keep({id}))});

try (
  fromjson
  | select(.started or .testResult or .testSummary or .buildMetrics.actionSummary or .finished)
  | {
      id: ({testResult: (.id.testResult | testid), testSummary: (.id.testSummary | testid)} | present),
      started: (.started | keep({uuid, command, buildToolVersion, startTime})),
      testResult: (.testResult | keep({
        status,
        cachedLocally,
        testAttemptDurationMillis,
        executionInfo: (.executionInfo | keep({strategy, cachedRemotely}))
      })),
      testSummary: (.testSummary | keep({overallStatus, totalRunCount, attemptCount, shardCount, totalNumCached})),
      buildMetrics: (.buildMetrics.actionSummary | keep({
        actionSummary: ({
          actionsCreated,
          actionsExecuted,
          runnerCount: (.runnerCount | if . == null then null else map(keep({name, count, execKind})) end),
          actionCacheStatistics: (.actionCacheStatistics | keep({hits, misses}))
        } | present)
      })),
      finished: (.finished | keep({exitCode: (.exitCode | keep({name, code}))}))
    }
  | present
) catch "unparsable BEP line"
