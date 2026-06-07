package workflows

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const workflowScriptRunner = `
const fs = require("fs");

const input = JSON.parse(fs.readFileSync(0, "utf8"));
const state = input.state || {};
state.issues = state.issues || [];
state.events = state.events || {};
state.comments = state.comments || {};
state.labels = state.labels || {};
state.now = state.now || new Date().toISOString();
state.botLogin = state.botLogin || "github-actions[bot]";

Date.now = () => new Date(state.now).getTime();

const ops = [];

function key(number) {
  return String(number);
}

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function issueFor(number) {
  return state.issues.find(issue => issue.number === number);
}

function labelsFor(number) {
  const k = key(number);
  if (!state.labels[k]) {
    const issue = issueFor(number);
    state.labels[k] = (issue?.labels || []).map(label =>
      typeof label === "string" ? label : label.name
    );
  }
  return state.labels[k];
}

function commentsFor(number) {
  const k = key(number);
  if (!state.comments[k]) state.comments[k] = [];
  return state.comments[k];
}

function eventsFor(number) {
  return state.events[key(number)] || [];
}

function labelListMatches(number, requested) {
  if (!requested) return true;
  const wanted = Array.isArray(requested) ? requested : String(requested).split(",");
  const current = labelsFor(number);
  return wanted.every(label => current.includes(label.trim()));
}

const issuesAPI = {
  listForRepo: async args => state.issues
    .filter(issue => !args.state || (issue.state || "open") === args.state)
    .filter(issue => labelListMatches(issue.number, args.labels))
    .map(clone),
  listEvents: async args => eventsFor(args.issue_number).map(clone),
  listComments: async args => {
    const since = args.since ? Date.parse(args.since) : -Infinity;
    return commentsFor(args.issue_number)
      .filter(comment => Date.parse(comment.created_at) >= since)
      .map(clone);
  },
  createComment: async args => {
    ops.push({
      type: "createComment",
      issue_number: args.issue_number,
      body: args.body,
    });
    commentsFor(args.issue_number).push({
      user: {login: state.botLogin},
      created_at: state.now,
      body: args.body,
    });
    return {data: {}};
  },
  update: async args => {
    ops.push({
      type: "updateIssue",
      issue_number: args.issue_number,
      state: args.state,
      state_reason: args.state_reason,
    });
    const issue = issueFor(args.issue_number);
    if (issue) {
      if (args.state) issue.state = args.state;
      if (args.state_reason) issue.state_reason = args.state_reason;
    }
    return {data: clone(issue || {})};
  },
  listLabelsOnIssue: async args => ({
    data: labelsFor(args.issue_number).map(name => ({name})),
  }),
  removeLabel: async args => {
    ops.push({
      type: "removeLabel",
      issue_number: args.issue_number,
      name: args.name,
    });
    state.labels[key(args.issue_number)] = labelsFor(args.issue_number)
      .filter(name => name !== args.name);
    return {data: {}};
  },
  addLabels: async args => {
    const labels = Array.isArray(args.labels) ? args.labels : [args.labels];
    ops.push({
      type: "addLabels",
      issue_number: args.issue_number,
      labels,
    });
    const current = labelsFor(args.issue_number);
    for (const label of labels) {
      if (!current.includes(label)) current.push(label);
    }
    return {data: {}};
  },
};

const github = {
  paginate: async (method, args) => {
    const result = await method(args);
    if (Array.isArray(result)) return result;
    if (Array.isArray(result?.data)) return result.data;
    return [];
  },
  rest: {issues: issuesAPI},
};

const core = {
  setFailed: message => {
    throw new Error(message);
  },
};

const logger = {
  log: (...args) => ops.push({type: "log", message: args.join(" ")}),
  error: (...args) => ops.push({type: "error", message: args.join(" ")}),
  warn: (...args) => ops.push({type: "warn", message: args.join(" ")}),
};

(async () => {
  try {
	    const run = new Function(
	      "github",
	      "context",
	      "core",
	      "console",
	      "\"use strict\"; return (async () => {\n" + input.script + "\n})();"
	    );
    await run(github, input.context || {}, core, logger);
    process.stdout.write(JSON.stringify({ok: true, ops, state}));
  } catch (err) {
    process.stdout.write(JSON.stringify({
      ok: false,
      error: String(err && (err.stack || err.message) || err),
      ops,
      state,
    }));
  }
})();
`

var needsLabels = []string{"status/needs-info", "status/needs-repro"}

type workflowFile struct {
	path string
	On   any                    `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
}

type workflowScript struct {
	path   string
	job    string
	step   string
	script string
}

type scriptRun struct {
	OK    bool           `json:"ok"`
	Error string         `json:"error"`
	Ops   []workflowOp   `json:"ops"`
	State map[string]any `json:"state"`
	From  string         `json:"-"`
}

type workflowOp struct {
	Type        string   `json:"type"`
	Issue       int      `json:"issue_number"`
	Body        string   `json:"body"`
	Name        string   `json:"name"`
	Labels      []string `json:"labels"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason"`
	Message     string   `json:"message"`
}

func TestNeedsStatusLabelCreatesVisibleIdempotentRequestForReporter(t *testing.T) {
	repo := repoRoot(t)
	scripts := workflowScriptsFor(t, repo, "issues", "labeled", func(workflowScript) bool {
		return true
	})
	if len(scripts) == 0 {
		t.Fatal("no issues/labeled github-script workflow found for needs-info or needs-repro requests")
	}

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for _, label := range needsLabels {
		t.Run(label, func(t *testing.T) {
			state := lifecycleState(now, label, []string{now.Add(-time.Minute).Format(time.RFC3339)}, nil, false)
			result := runScripts(t, scripts, labeledContext(label), state)
			if result.hasErrors() {
				t.Fatalf("workflow scripts errored before creating request:\n%s", result.debugString())
			}

			requests := visibleRequestComments(result.ops(), label)
			if len(requests) != 1 {
				t.Fatalf("visible request comment count = %d, want 1 for %s\n%s", len(requests), label, result.debugString())
			}

			second := runScripts(t, scripts, labeledContext(label), result.state())
			if second.hasErrors() {
				t.Fatalf("workflow scripts errored on idempotency run:\n%s", second.debugString())
			}
			if comments := commentOps(second.ops()); len(comments) != 0 {
				t.Fatalf("second automation run created %d comment(s), want none\n%s", len(comments), second.debugString())
			}
		})
	}
}

func TestNeedsStatusLabelIgnoresReporterAuthoredRequestComment(t *testing.T) {
	repo := repoRoot(t)
	scripts := workflowScriptsFor(t, repo, "issues", "labeled", func(workflowScript) bool {
		return true
	})
	if len(scripts) == 0 {
		t.Fatal("no issues/labeled github-script workflow found for needs-info or needs-repro requests")
	}

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for _, label := range needsLabels {
		t.Run(label, func(t *testing.T) {
			comments := []map[string]any{
				reporterRequestComment(now.Add(-time.Minute), label),
			}
			state := lifecycleState(now, label, []string{now.Add(-2 * time.Minute).Format(time.RFC3339)}, comments, false)
			result := runScripts(t, scripts, labeledContext(label), state)
			if result.hasErrors() {
				t.Fatalf("workflow scripts errored before creating request:\n%s", result.debugString())
			}

			requests := visibleRequestComments(result.ops(), label)
			if len(requests) != 1 {
				t.Fatalf("visible request comment count = %d, want 1 for %s (reporter-authored matching comment must not suppress bot request)\n%s", len(requests), label, result.debugString())
			}
		})
	}
}

func TestNeedsStatusLabelReapplyPostsFreshRequestForReporter(t *testing.T) {
	repo := repoRoot(t)
	scripts := workflowScriptsFor(t, repo, "issues", "labeled", func(workflowScript) bool {
		return true
	})
	if len(scripts) == 0 {
		t.Fatal("no issues/labeled github-script workflow found for needs-info or needs-repro requests")
	}

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for _, label := range needsLabels {
		t.Run(label, func(t *testing.T) {
			labelTimes := []string{
				now.Add(-20 * 24 * time.Hour).Format(time.RFC3339),
				now.Add(-time.Minute).Format(time.RFC3339),
			}
			comments := []map[string]any{
				requestComment(now.Add(-19*24*time.Hour), label),
			}
			state := lifecycleState(now, label, labelTimes, comments, false)

			result := runScripts(t, scripts, labeledContext(label), state)
			if result.hasErrors() {
				t.Fatalf("workflow scripts errored on re-label run:\n%s", result.debugString())
			}

			requests := visibleRequestComments(result.ops(), label)
			if len(requests) != 1 {
				t.Fatalf("visible request comment count = %d, want 1 for %s (stale request before latest label must not suppress)\n%s", len(requests), label, result.debugString())
			}
		})
	}
}

func TestCloseStaleNeedsLabelsRequiresVisibleRequestAfterLatestLabelEvent(t *testing.T) {
	repo := repoRoot(t)
	scripts := workflowScriptsFor(t, repo, "schedule", "", func(script workflowScript) bool {
		return mentionsNeedsLabel(script.script)
	})
	if len(scripts) == 0 {
		t.Fatal("no scheduled github-script workflow found for stale needs-info or needs-repro closure")
	}

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		labelAges  []time.Duration
		comments   []map[string]any
		wantClosed bool
	}{
		{
			name:       "silent_label_is_not_closed",
			labelAges:  []time.Duration{15 * 24 * time.Hour},
			wantClosed: false,
		},
		{
			name:      "request_before_latest_label_does_not_count",
			labelAges: []time.Duration{20 * 24 * time.Hour, 15 * 24 * time.Hour},
			comments: []map[string]any{
				requestComment(now.Add(-19*24*time.Hour), "status/needs-info"),
			},
			wantClosed: false,
		},
		{
			name:      "request_younger_than_fourteen_days_stays_open",
			labelAges: []time.Duration{20 * 24 * time.Hour},
			comments: []map[string]any{
				requestComment(now.Add(-13*24*time.Hour), "status/needs-info"),
			},
			wantClosed: false,
		},
		{
			name:      "visible_request_after_latest_label_closes_after_fourteen_days",
			labelAges: []time.Duration{16 * 24 * time.Hour},
			comments: []map[string]any{
				requestComment(now.Add(-15*24*time.Hour), "status/needs-info"),
			},
			wantClosed: true,
		},
		{
			name:      "reporter_authored_request_comment_does_not_seed_close_clock",
			labelAges: []time.Duration{16 * 24 * time.Hour},
			comments: []map[string]any{
				reporterRequestComment(now.Add(-15*24*time.Hour), "status/needs-info"),
			},
			wantClosed: false,
		},
	}

	for _, label := range needsLabels {
		for _, tc := range tests {
			t.Run(label+"/"+tc.name, func(t *testing.T) {
				labelTimes := make([]string, 0, len(tc.labelAges))
				for _, age := range tc.labelAges {
					labelTimes = append(labelTimes, now.Add(-age).Format(time.RFC3339))
				}
				comments := cloneCommentsForLabel(tc.comments, label)
				state := lifecycleState(now, label, labelTimes, comments, false)

				result := runScripts(t, scripts, scheduledContext(), state)
				if result.hasErrors() {
					t.Fatalf("scheduled workflow scripts errored:\n%s", result.debugString())
				}

				closed := len(closedIssueOps(result.ops())) > 0
				if closed != tc.wantClosed {
					t.Fatalf("closed = %v, want %v\n%s", closed, tc.wantClosed, result.debugString())
				}
				if tc.wantClosed && len(commentOps(result.ops())) == 0 {
					t.Fatalf("stale close updated the issue without a visible close comment\n%s", result.debugString())
				}
			})
		}
	}
}

func TestAuthorActivityClearsNeedsLabelsAndPreventsStaleClosure(t *testing.T) {
	repo := repoRoot(t)
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	t.Run("issue_author_reply_removes_needs_labels", func(t *testing.T) {
		scripts := workflowScriptsFor(t, repo, "issue_comment", "created", func(script workflowScript) bool {
			return mentionsNeedsLabel(script.script)
		})
		if len(scripts) == 0 {
			t.Fatal("no issue_comment workflow found for needs-info or needs-repro author responses")
		}

		state := lifecycleState(now, "status/needs-info", []string{now.Add(-time.Hour).Format(time.RFC3339)}, nil, false)
		state["labels"] = map[string]any{"3142": []string{"status/needs-info", "status/needs-repro"}}

		result := runScripts(t, scripts, authorIssueCommentContext(), state)
		if result.hasErrors() {
			t.Fatalf("issue_comment workflow scripts errored:\n%s", result.debugString())
		}

		removed := removedLabels(result.ops())
		for _, label := range needsLabels {
			if !removed[label] {
				t.Fatalf("removed labels = %v, want %s removed\n%s", sortedKeys(removed), label, result.debugString())
			}
		}
	})

	t.Run("pull_request_update_by_author_removes_needs_labels", func(t *testing.T) {
		scripts := workflowScriptsFor(t, repo, "pull_request_target", "synchronize", func(script workflowScript) bool {
			return mentionsNeedsLabel(script.script)
		})
		if len(scripts) == 0 {
			t.Fatal("no pull_request_target synchronize workflow found for needs-info or needs-repro author updates")
		}

		state := lifecycleState(now, "status/needs-info", []string{now.Add(-time.Hour).Format(time.RFC3339)}, nil, true)
		state["labels"] = map[string]any{"3142": []string{"status/needs-info", "status/needs-repro"}}

		result := runScripts(t, scripts, authorPullRequestSynchronizeContext(), state)
		if result.hasErrors() {
			t.Fatalf("pull_request_target workflow scripts errored:\n%s", result.debugString())
		}

		removed := removedLabels(result.ops())
		for _, label := range needsLabels {
			if !removed[label] {
				t.Fatalf("removed labels = %v, want %s removed\n%s", sortedKeys(removed), label, result.debugString())
			}
		}
	})

	t.Run("author_reply_after_visible_request_prevents_stale_close", func(t *testing.T) {
		scripts := workflowScriptsFor(t, repo, "schedule", "", func(script workflowScript) bool {
			return mentionsNeedsLabel(script.script)
		})
		if len(scripts) == 0 {
			t.Fatal("no scheduled stale workflow found")
		}

		for _, label := range needsLabels {
			state := lifecycleState(now, label, []string{now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)}, []map[string]any{
				requestComment(now.Add(-19*24*time.Hour), label),
				{
					"user":       map[string]any{"login": "reporter"},
					"created_at": now.Add(-18 * 24 * time.Hour).Format(time.RFC3339),
					"body":       "Here are the requested details.",
				},
			}, false)

			result := runScripts(t, scripts, scheduledContext(), state)
			if result.hasErrors() {
				t.Fatalf("scheduled workflow scripts errored for %s:\n%s", label, result.debugString())
			}
			if closed := closedIssueOps(result.ops()); len(closed) != 0 {
				t.Fatalf("author response should prevent stale close for %s\n%s", label, result.debugString())
			}
		}
	})
}

func workflowScriptsFor(t *testing.T, repo, event, action string, keep func(workflowScript) bool) []workflowScript {
	t.Helper()

	workflowPaths, err := filepath.Glob(filepath.Join(repo, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("listing workflows: %v", err)
	}

	var scripts []workflowScript
	for _, path := range workflowPaths {
		wf := readWorkflow(t, path)
		if !workflowMatchesEvent(wf.On, event, action) {
			continue
		}
		for jobName, job := range wf.Jobs {
			for _, step := range job.Steps {
				if !strings.Contains(step.Uses, "actions/github-script") {
					continue
				}
				script := strings.TrimSpace(step.With["script"])
				if script == "" {
					continue
				}
				candidate := workflowScript{
					path:   relPath(t, repo, path),
					job:    jobName,
					step:   step.Name,
					script: script,
				}
				if keep == nil || keep(candidate) {
					scripts = append(scripts, candidate)
				}
			}
		}
	}
	sort.Slice(scripts, func(i, j int) bool {
		return scripts[i].path+"/"+scripts[i].job+"/"+scripts[i].step < scripts[j].path+"/"+scripts[j].job+"/"+scripts[j].step
	})
	return scripts
}

func readWorkflow(t *testing.T, path string) workflowFile {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	wf.path = path
	return wf
}

func workflowMatchesEvent(on any, event, action string) bool {
	switch typed := on.(type) {
	case string:
		return typed == event
	case []any:
		for _, entry := range typed {
			if value, ok := entry.(string); ok && value == event {
				return true
			}
		}
		return false
	case map[string]any:
		spec, ok := typed[event]
		if !ok {
			return false
		}
		if action == "" {
			return true
		}
		return eventSpecMatchesAction(spec, action)
	default:
		return false
	}
}

func eventSpecMatchesAction(spec any, action string) bool {
	if spec == nil {
		return true
	}
	specMap, ok := spec.(map[string]any)
	if !ok {
		return true
	}
	types, ok := specMap["types"]
	if !ok {
		return true
	}
	switch typed := types.(type) {
	case string:
		return typed == action
	case []any:
		for _, entry := range typed {
			if value, ok := entry.(string); ok && value == action {
				return true
			}
		}
	}
	return false
}

type scriptResults []scriptRun

func runScripts(t *testing.T, scripts []workflowScript, context, state map[string]any) scriptResults {
	t.Helper()

	currentState := cloneMap(t, state)
	results := make(scriptResults, 0, len(scripts))
	for _, script := range scripts {
		run := runScript(t, script, context, currentState)
		results = append(results, run)
		if run.State != nil {
			currentState = run.State
		}
	}
	return results
}

func runScript(t *testing.T, script workflowScript, context, state map[string]any) scriptRun {
	t.Helper()

	input := map[string]any{
		"script":  script.script,
		"context": context,
		"state":   state,
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshaling workflow input: %v", err)
	}

	cmd := exec.Command("node", "-e", workflowScriptRunner)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("running %s %s: %v\nstderr:\n%s", script.path, script.step, err, stderr.String())
	}

	var run scriptRun
	if err := json.Unmarshal(output, &run); err != nil {
		t.Fatalf("decoding workflow script result from %s %s: %v\nstdout:\n%s\nstderr:\n%s", script.path, script.step, err, string(output), stderr.String())
	}
	run.From = script.path + " :: " + script.step
	return run
}

func (r scriptResults) hasErrors() bool {
	for _, run := range r {
		if !run.OK {
			return true
		}
	}
	return false
}

func (r scriptResults) ops() []workflowOp {
	var ops []workflowOp
	for _, run := range r {
		ops = append(ops, run.Ops...)
	}
	return ops
}

func (r scriptResults) state() map[string]any {
	if len(r) == 0 {
		return nil
	}
	return r[len(r)-1].State
}

func (r scriptResults) debugString() string {
	var b strings.Builder
	for _, run := range r {
		fmt.Fprintf(&b, "script: %s\n", run.From)
		if !run.OK {
			fmt.Fprintf(&b, "error: %s\n", run.Error)
		}
		for _, op := range run.Ops {
			fmt.Fprintf(&b, "op: %+v\n", op)
		}
	}
	return b.String()
}

func lifecycleState(now time.Time, label string, labelTimes []string, comments []map[string]any, pullRequest bool) map[string]any {
	issue := map[string]any{
		"number": 3142,
		"title":  "Need a repro request",
		"state":  "open",
		"user":   map[string]any{"login": "reporter"},
		"labels": []map[string]any{{"name": label}},
	}
	if pullRequest {
		issue["pull_request"] = map[string]any{"url": "https://api.github.test/repos/org/repo/pulls/3142"}
	}

	events := make([]map[string]any, 0, len(labelTimes))
	for _, labelTime := range labelTimes {
		events = append(events, map[string]any{
			"event":      "labeled",
			"label":      map[string]any{"name": label},
			"created_at": labelTime,
		})
	}

	return map[string]any{
		"now":      now.Format(time.RFC3339),
		"botLogin": "github-actions[bot]",
		"issues":   []map[string]any{issue},
		"events":   map[string]any{"3142": events},
		"comments": map[string]any{"3142": comments},
		"labels":   map[string]any{"3142": []string{label}},
	}
}

func labeledContext(label string) map[string]any {
	issue := map[string]any{
		"number": 3142,
		"user":   map[string]any{"login": "reporter"},
	}
	return map[string]any{
		"eventName": "issues",
		"repo":      map[string]any{"owner": "gastownhall", "repo": "gascity"},
		"issue":     map[string]any{"number": 3142},
		"payload": map[string]any{
			"action": "labeled",
			"issue":  issue,
			"label":  map[string]any{"name": label},
		},
	}
}

func scheduledContext() map[string]any {
	return map[string]any{
		"eventName": "schedule",
		"repo":      map[string]any{"owner": "gastownhall", "repo": "gascity"},
	}
}

func authorIssueCommentContext() map[string]any {
	return map[string]any{
		"eventName": "issue_comment",
		"repo":      map[string]any{"owner": "gastownhall", "repo": "gascity"},
		"payload": map[string]any{
			"issue": map[string]any{
				"number": 3142,
				"user":   map[string]any{"login": "reporter"},
			},
			"comment": map[string]any{
				"user": map[string]any{"login": "reporter"},
				"body": "Here is the requested information.",
			},
		},
	}
}

func authorPullRequestSynchronizeContext() map[string]any {
	return map[string]any{
		"eventName": "pull_request_target",
		"repo":      map[string]any{"owner": "gastownhall", "repo": "gascity"},
		"payload": map[string]any{
			"sender": map[string]any{"login": "reporter"},
			"pull_request": map[string]any{
				"number": 3142,
				"user":   map[string]any{"login": "reporter"},
			},
		},
	}
}

func requestComment(createdAt time.Time, label string) map[string]any {
	return map[string]any{
		"user":       map[string]any{"login": "github-actions[bot]"},
		"created_at": createdAt.Format(time.RFC3339),
		"body":       requestBody(label),
	}
}

func reporterRequestComment(createdAt time.Time, label string) map[string]any {
	return map[string]any{
		"user":       map[string]any{"login": "reporter"},
		"created_at": createdAt.Format(time.RFC3339),
		"body":       requestBody(label),
	}
}

func requestBody(label string) string {
	switch label {
	case "status/needs-repro":
		return "Please reply with a minimal reproduction within 14 days so we can continue triage."
	default:
		return "Please reply with the requested information within 14 days so we can continue triage."
	}
}

func cloneCommentsForLabel(comments []map[string]any, label string) []map[string]any {
	out := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		next := make(map[string]any, len(comment))
		for key, value := range comment {
			next[key] = value
		}
		body, ok := next["body"].(string)
		if ok {
			for _, needsLabel := range needsLabels {
				body = strings.ReplaceAll(body, requestBody(needsLabel), requestBody(label))
			}
			next["body"] = body
		}
		out = append(out, next)
	}
	return out
}

func visibleRequestComments(ops []workflowOp, label string) []workflowOp {
	var out []workflowOp
	for _, op := range ops {
		if op.Type == "createComment" && isVisibleRequest(op.Body, label) {
			out = append(out, op)
		}
	}
	return out
}

func isVisibleRequest(body, label string) bool {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "closing") {
		return false
	}
	if !regexp.MustCompile(`14\s*-?\s*days?`).MatchString(lower) {
		return false
	}
	if !strings.Contains(lower, "reply") && !strings.Contains(lower, "respond") {
		return false
	}
	switch label {
	case "status/needs-repro":
		return strings.Contains(lower, "repro") || strings.Contains(lower, "reproduction")
	case "status/needs-info":
		return strings.Contains(lower, "info") || strings.Contains(lower, "information") || strings.Contains(lower, "details")
	default:
		return false
	}
}

func commentOps(ops []workflowOp) []workflowOp {
	var out []workflowOp
	for _, op := range ops {
		if op.Type == "createComment" {
			out = append(out, op)
		}
	}
	return out
}

func closedIssueOps(ops []workflowOp) []workflowOp {
	var out []workflowOp
	for _, op := range ops {
		if op.Type == "updateIssue" && op.State == "closed" {
			out = append(out, op)
		}
	}
	return out
}

func removedLabels(ops []workflowOp) map[string]bool {
	out := make(map[string]bool)
	for _, op := range ops {
		if op.Type == "removeLabel" {
			out[op.Name] = true
		}
	}
	return out
}

func mentionsNeedsLabel(script string) bool {
	for _, label := range needsLabels {
		if strings.Contains(script, label) {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("could not find repo root containing go.mod")
		}
		dir = next
	}
}

func relPath(t *testing.T, root, path string) string {
	t.Helper()

	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relpath %s from %s: %v", path, root, err)
	}
	return filepath.ToSlash(rel)
}

func cloneMap(t *testing.T, input map[string]any) map[string]any {
	t.Helper()

	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshaling clone input: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshaling clone input: %v", err)
	}
	return out
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
