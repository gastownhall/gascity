package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/google/uuid"
)

// cmdRigAddRemote routes a `gc rig add` to a REMOTE city over the control plane.
// A remote city can't see the client filesystem, so this drives server-side
// provisioning: it forwards --git-url (required), a client-minted request_id (or
// --request-id for a resume), and the rig identity flags, then renders the async
// provisioning progress and terminal result. Modes that need the client's local
// state — a positional path, --adopt (reads the client's .beads/), --include,
// --start-suspended — are refused with a clear message before any wire call.
// A remote mutation carries a request-bound X-GC-City-Write grant automatically
// (gate G18) and is non-fallbackable (gate G1).
func cmdRigAddRemote(c *api.Client, target *remoteTarget, args []string,
	gitURL, requestID, nameFlag, prefixFlag, defaultBranchFlag string,
	includes []string, startSuspended, adopt, jsonOutput bool,
	stdout, stderr io.Writer,
) int {
	fail := func(code, message string) int {
		if jsonOutput {
			return writeJSONError(stdout, stderr, code, message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Refusals — fail fast, before any wire call, in an order that reports the
	// most specific mismatch first.
	if len(args) > 0 {
		return fail("unsupported_remote", "gc rig add: a remote city cannot see a client filesystem path; use --git-url for a server-side clone")
	}
	if strings.TrimSpace(gitURL) == "" {
		return fail("invalid_arguments", "gc rig add: a remote rig add requires --git-url (the server clones it)")
	}
	if adopt {
		return fail("unsupported_remote", "gc rig add: --adopt reads the client's .beads/ directory and is not supported for a remote city")
	}
	if len(includes) > 0 {
		return fail("unsupported_remote", "gc rig add: --include is not supported for a remote city yet")
	}
	if startSuspended {
		return fail("unsupported_remote", "gc rig add: --start-suspended is not supported for a remote city yet")
	}

	name := strings.TrimSpace(nameFlag)
	if name == "" {
		name = deriveRigNameFromGitURL(gitURL)
	}
	if name == "" {
		return fail("invalid_arguments", "gc rig add: cannot derive a rig name from --git-url; pass --name")
	}

	if strings.TrimSpace(requestID) == "" {
		requestID = uuid.NewString()
	}

	// Echo the resolved target (human mode only) so the operator can see which
	// city a mutation is about to hit.
	if !jsonOutput {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}

	progressOut := stdout
	if jsonOutput {
		progressOut = io.Discard // JSONL purity: a single object on stdout
	}
	onProgress := func(p api.RigProvisionProgressPayload) {
		detail := strings.TrimSpace(p.Detail)
		if detail == "" {
			detail = p.Step
		}
		if p.Warn {
			fmt.Fprintf(stderr, "gc rig add: %s\n", detail) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintln(progressOut, detail) //nolint:errcheck // best-effort stdout
		}
	}

	res, err := c.RigCreate(api.RigCreateRequest{
		Name:          name,
		Prefix:        prefixFlag,
		DefaultBranch: defaultBranchFlag,
		GitURL:        gitURL,
		RequestID:     requestID,
	}, onProgress)
	if err != nil {
		return renderRemoteRigAddError(err, target, gitURL, name, prefixFlag, defaultBranchFlag, jsonOutput, stdout, stderr)
	}

	rigName := res.Rig
	if rigName == "" {
		rigName = name
	}
	if jsonOutput {
		result := managementActionResult{
			Command:       commandName("rig", "add"),
			Action:        "add",
			Name:          rigName,
			Rig:           rigName,
			Prefix:        res.Prefix,
			DefaultBranch: res.DefaultBranch,
			Status:        res.Status,
			RequestID:     res.RequestID,
		}
		if err := writeManagementActionJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	switch res.Status {
	case "exists":
		fmt.Fprintf(stdout, "exists → %s (idempotent replay)\n", rigName) //nolint:errcheck // best-effort stdout
	default: // provisioned (or a sync created, which the remote git_url path never returns)
		line := "provisioned → " + rigName
		var extras []string
		if res.Prefix != "" {
			extras = append(extras, "prefix "+res.Prefix)
		}
		if res.DefaultBranch != "" {
			extras = append(extras, "branch "+res.DefaultBranch)
		}
		if len(extras) > 0 {
			line += " (" + strings.Join(extras, ", ") + ")"
		}
		fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// renderRemoteRigAddError renders a remote rig-add failure. A lost stream, a
// hit-the-deadline-but-still-running provision, or a rolled-back provision prints
// the request_id plus an idempotent re-attach recipe; a rig-name conflict points
// at a body-independent passive event watch (re-POSTing your body under another
// request's in-flight id would 409). prefix/defaultBranch are the flags of the
// ORIGINAL invocation: the re-attach recipe must reproduce them (the server
// digest hashes name+prefix+default_branch+git_url) or the retry 409s.
func renderRemoteRigAddError(err error, target *remoteTarget, gitURL, name, prefix, defaultBranch string, jsonOutput bool, stdout, stderr io.Writer) int {
	fail := func(code, message string) int {
		if jsonOutput {
			return writeJSONError(stdout, stderr, code, message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}
	flags := remoteInvocationFlags(target)

	var conflict *api.RigCreateConflictError
	var waitErr *api.RigCreateWaitError
	var deadlineErr *api.RigCreateDeadlineError
	var failedErr *api.RigCreateFailedError
	switch {
	case errors.As(err, &conflict):
		if conflict.Code == "rig_name_conflict" && conflict.InFlightRequestID != "" {
			// The in-flight request_id is bound to ANOTHER request's body; a re-POST
			// of your body under it would 409. Watch its events instead of re-adding.
			msg := fmt.Sprintf("gc rig add: %v\n"+
				"another request is provisioning this rig; watch it instead of re-submitting:\n%s",
				conflict, rigWatchRecipe(flags, conflict.InFlightRequestID, conflict.EventCursor))
			return fail("rig_name_conflict", msg)
		}
		return fail("rig_create_conflict", "gc rig add: "+conflict.Error())
	case errors.As(err, &deadlineErr):
		msg := fmt.Sprintf("gc rig add: %v\n"+
			"the provision continues server-side. Re-attach the wait (idempotent):\n%s\n"+
			"or inspect its events without streaming:\n%s",
			deadlineErr,
			rigAddReplayRecipe(flags, gitURL, name, prefix, defaultBranch, deadlineErr.RequestID),
			rigWatchRecipe(flags, deadlineErr.RequestID, ""))
		return fail("rig_stream_deadline", msg)
	case errors.As(err, &waitErr):
		msg := fmt.Sprintf("gc rig add: lost the provisioning stream: %v (request_id=%s)\n"+
			"the provision continues server-side. Resume the wait (idempotent):\n%s\n"+
			"or inspect the terminal event without streaming:\n%s",
			waitErr.Err, waitErr.RequestID,
			rigAddReplayRecipe(flags, gitURL, name, prefix, defaultBranch, waitErr.RequestID),
			rigWatchRecipe(flags, waitErr.RequestID, ""))
		return fail("rig_stream_lost", msg)
	case errors.As(err, &failedErr):
		msg := fmt.Sprintf("gc rig add: %s: %s (request_id=%s)\n"+
			"the provision rolled back. Retry the same request_id to re-clone cleanly:\n%s",
			failedErr.Code, failedErr.Message, failedErr.RequestID,
			rigAddReplayRecipe(flags, gitURL, name, prefix, defaultBranch, failedErr.RequestID))
		return fail("rig_provision_failed", msg)
	default:
		return fail("rig_add_failed", "gc rig add: "+err.Error())
	}
}

// shellSingleQuote wraps s in single quotes so an interpolated value (a git URL,
// a rig name) survives copy-paste as a single shell word even when it carries a
// space or a shell metacharacter. An embedded single quote is closed, escaped as
// a literal, and reopened — the standard '\” seam.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rigAddReplayRecipe builds the idempotent re-POST recipe: re-running gc rig add
// with the SAME request_id and the SAME digest-affecting flags replays the
// in-flight / rolled-back provision instead of starting a new one. The git URL is
// credential-redacted (a token must never reach stderr, a log, or --json output;
// the operator re-adds it) and every interpolated value is shell-quoted.
// prefix/default_branch are emitted ONLY when set on the original invocation:
// the server digest hashes them, and an omitted value must stay omitted to
// reproduce the same digest (a spurious 409 otherwise).
func rigAddReplayRecipe(flags, gitURL, name, prefix, defaultBranch, requestID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  gc %s rig add --git-url %s --name %s",
		flags, shellSingleQuote(gitcred.RedactUserinfo(gitURL)), shellSingleQuote(name))
	if strings.TrimSpace(prefix) != "" {
		fmt.Fprintf(&b, " --prefix %s", shellSingleQuote(prefix))
	}
	if strings.TrimSpace(defaultBranch) != "" {
		fmt.Fprintf(&b, " --default-branch %s", shellSingleQuote(defaultBranch))
	}
	fmt.Fprintf(&b, " --request-id %s", shellSingleQuote(requestID))
	return b.String()
}

// rigWatchRecipe builds the BODY-INDEPENDENT passive watch: it observes an
// in-flight provision by request_id (progress stream + terminal events) and never
// re-POSTs a body, so it cannot 409 against another request's in-flight id.
// eventCursor, when set (and not the replay-everything "0"), seeds --after so the
// watch resumes from where the provision began instead of replaying the whole log.
func rigWatchRecipe(flags, requestID, eventCursor string) string {
	after := ""
	if c := strings.TrimSpace(eventCursor); c != "" && c != "0" {
		after = " --after " + shellSingleQuote(c)
	}
	match := shellSingleQuote("request_id=" + requestID)
	var b strings.Builder
	fmt.Fprintf(&b, "  gc %s events --follow --type %s --payload-match %s%s\n", flags, events.RigProvisionProgress, match, after)
	fmt.Fprintf(&b, "  gc %s events --watch --type %s --payload-match %s\n", flags, events.RequestResultRigCreate, match)
	fmt.Fprintf(&b, "  gc %s events --watch --type %s --payload-match %s", flags, events.RequestFailed, match)
	return b.String()
}

// remoteInvocationFlags renders the flags that re-select target for a resume
// recipe: --context <name> for a named context, else the ad-hoc --city-url pair.
func remoteInvocationFlags(target *remoteTarget) string {
	if target == nil {
		return ""
	}
	if target.Ctx != nil && strings.TrimSpace(target.Ctx.Name) != "" {
		return "--context " + target.Ctx.Name
	}
	return fmt.Sprintf("--city-url %s --city-name %s", target.BaseURL, target.CityName)
}

// deriveRigNameFromGitURL mirrors the local basename default (cmd_rig.go): the
// last path segment of the git URL with a trailing slash and .git suffix
// stripped. It is client-side sugar; the server independently re-validates the
// name (validateRigName).
func deriveRigNameFromGitURL(gitURL string) string {
	s := strings.TrimSpace(gitURL)
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimSpace(s)
}
