// Package bepsummary turns Bazel Build Event Protocol JSON files
// (--build_event_json_file) into a per-invocation cache report: how many
// test targets were cache hits (local action cache, remote cache, disk
// cache) versus executed (remotely or locally), their outcomes, and the
// action-level runner and action-cache statistics. CI renders it into the
// GitHub job summary and keeps the JSON form as an artifact so cross-phase
// test skipping can be tracked across pre-push, PR, and main runs.
package bepsummary

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// int64Value decodes a proto3-JSON integer. Bazel writes int64 fields as
// strings ("38") and int32 fields as numbers; both decode here.
type int64Value int64

func (v *int64Value) UnmarshalJSON(data []byte) error {
	data = bytes.Trim(data, `"`)
	if len(data) == 0 || string(data) == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return fmt.Errorf("integer field %q: %w", data, err)
	}
	*v = int64Value(n)
	return nil
}

// bepEvent is the subset of build_event_stream.BuildEvent this package reads.
// Unknown fields and event kinds are ignored.
// redact.jq, which bazel.yml applies before a lane uploads its BEP file, must
// keep every field decoded here (TestRedactProgramKeepsEveryDecodedField).
type bepEvent struct {
	ID struct {
		TestResult  *bepTestID `json:"testResult"`
		TestSummary *bepTestID `json:"testSummary"`
	} `json:"id"`
	Started       *bepStarted       `json:"started"`
	TestResult    *bepTestResult    `json:"testResult"`
	TestSummary   *bepTestSummary   `json:"testSummary"`
	BuildMetrics  *bepBuildMetrics  `json:"buildMetrics"`
	BuildFinished *bepBuildFinished `json:"finished"`
}

type bepTestID struct {
	Label         string     `json:"label"`
	Run           int64Value `json:"run"`
	Shard         int64Value `json:"shard"`
	Attempt       int64Value `json:"attempt"`
	Configuration struct {
		ID string `json:"id"`
	} `json:"configuration"`
}

type bepStarted struct {
	UUID             string `json:"uuid"`
	Command          string `json:"command"`
	BuildToolVersion string `json:"buildToolVersion"`
	StartTime        string `json:"startTime"`
}

type bepTestResult struct {
	Status                    string     `json:"status"`
	CachedLocally             bool       `json:"cachedLocally"`
	TestAttemptDurationMillis int64Value `json:"testAttemptDurationMillis"`
	ExecutionInfo             struct {
		Strategy       string `json:"strategy"`
		CachedRemotely bool   `json:"cachedRemotely"`
	} `json:"executionInfo"`
}

type bepTestSummary struct {
	OverallStatus  string     `json:"overallStatus"`
	TotalRunCount  int64Value `json:"totalRunCount"`
	AttemptCount   int64Value `json:"attemptCount"`
	ShardCount     int64Value `json:"shardCount"`
	TotalNumCached int64Value `json:"totalNumCached"`
}

type bepBuildMetrics struct {
	ActionSummary *struct {
		ActionsCreated  int64Value `json:"actionsCreated"`
		ActionsExecuted int64Value `json:"actionsExecuted"`
		RunnerCount     []struct {
			Name     string     `json:"name"`
			Count    int64Value `json:"count"`
			ExecKind string     `json:"execKind"`
		} `json:"runnerCount"`
		ActionCacheStatistics *struct {
			Hits   int64Value `json:"hits"`
			Misses int64Value `json:"misses"`
		} `json:"actionCacheStatistics"`
	} `json:"actionSummary"`
}

type bepBuildFinished struct {
	ExitCode *struct {
		Name string     `json:"name"`
		Code int64Value `json:"code"`
	} `json:"exitCode"`
}

// stream is the decoded content of one BEP JSON file.
type stream struct {
	events []bepEvent
	// truncated reports that the final line was incomplete JSON, which is
	// what a Bazel client killed mid-write (canceled job, timeout) leaves.
	truncated bool
}

// readStream decodes newline-delimited BEP JSON. A malformed line is an
// error unless it is the last line, which marks the stream truncated.
func readStream(r io.Reader) (stream, error) {
	var s stream
	br := bufio.NewReader(r)
	var pendingErr error
	lineNo := 0
	for {
		line, readErr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			lineNo++
			if pendingErr != nil {
				return stream{}, pendingErr
			}
			var ev bepEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				pendingErr = fmt.Errorf("line %d: %w", lineNo, err)
			} else {
				s.events = append(s.events, ev)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return stream{}, fmt.Errorf("reading BEP stream: %w", readErr)
		}
	}
	s.truncated = pendingErr != nil
	return s, nil
}
