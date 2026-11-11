package prwatchdog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const rewatchSHA = "0123456789abcdef0123456789abcdef01234567"

var rewatchT0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// fakeRewatchAPI records every call so tests can assert the cheap-first order
// and that nothing past the deciding step was ever requested.
type fakeRewatchAPI struct {
	runs    []WorkflowRun
	runsErr error

	checkRuns []CheckRun
	checkErr  error

	// pulls[n] is page n+1 of the open-PR listing.
	pulls   [][]PullRequest
	pullErr error

	rerunErr error

	runsCalls  []string
	checkCalls []string
	pullPages  []int
	reruns     []int64
}

func (f *fakeRewatchAPI) ListWatchdogRuns(_ context.Context, headSHA string) ([]WorkflowRun, error) {
	f.runsCalls = append(f.runsCalls, headSHA)
	return f.runs, f.runsErr
}

func (f *fakeRewatchAPI) FetchCheckRuns(_ context.Context, headSHA string) ([]CheckRun, error) {
	f.checkCalls = append(f.checkCalls, headSHA)
	return f.checkRuns, f.checkErr
}

func (f *fakeRewatchAPI) ListOpenPullRequests(_ context.Context, page int) ([]PullRequest, error) {
	f.pullPages = append(f.pullPages, page)
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if page < 1 || page > len(f.pulls) {
		return nil, nil
	}
	return f.pulls[page-1], nil
}

func (f *fakeRewatchAPI) Rerun(_ context.Context, runID int64) error {
	f.reruns = append(f.reruns, runID)
	return f.rerunErr
}

func (f *fakeRewatchAPI) assertCalls(t *testing.T, runs, checks, pullPages, reruns int) {
	t.Helper()
	if len(f.runsCalls) != runs || len(f.checkCalls) != checks || len(f.pullPages) != pullPages || len(f.reruns) != reruns {
		t.Fatalf("calls = {watchdog-run lists: %d, check-run fetches: %d, PR pages: %d, re-runs: %d}, want {%d, %d, %d, %d}",
			len(f.runsCalls), len(f.checkCalls), len(f.pullPages), len(f.reruns), runs, checks, pullPages, reruns)
	}
}

func watchdogRun(id int64, status, conclusion string, age time.Duration) WorkflowRun {
	return WorkflowRun{ID: id, RunAttempt: 1, Status: status, Conclusion: conclusion, CreatedAt: rewatchT0.Add(age)}
}

func passingChecks(sha string) []CheckRun {
	return []CheckRun{
		{Name: CheckName, HeadSHA: sha, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: rewatchT0, ID: 1},
		{Name: CIRequiredName, HeadSHA: sha, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: rewatchT0, ID: 2},
	}
}

func withMacSuccess(sha string, runs []CheckRun) []CheckRun {
	return append(runs, CheckRun{Name: MacCheckName, HeadSHA: sha, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: rewatchT0, ID: 3})
}

func prAt(n int, sha string, labels ...string) PullRequest {
	return PullRequest{Number: n, HeadSHA: sha, Labels: labels}
}

// filler returns a full page of open PRs none of which has head sha.
func filler(base int) []PullRequest {
	page := make([]PullRequest, openPullRequestsPerPage)
	for i := range page {
		page[i] = prAt(base+i, fmt.Sprintf("%040x", base+i))
	}
	return page
}

// healable is a fake whose latest watchdog run failed and whose current head
// has passing evidence: every step of the procedure says "re-run".
func healable() *fakeRewatchAPI {
	return &fakeRewatchAPI{
		runs:      []WorkflowRun{watchdogRun(900, "completed", "failure", 0)},
		checkRuns: passingChecks(rewatchSHA),
		pulls:     [][]PullRequest{{prAt(42, rewatchSHA)}},
	}
}

func TestRewatch_NoWatchdogRunIsANoOpAfterOneCall(t *testing.T) {
	api := &fakeRewatchAPI{}
	res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionNoOp {
		t.Fatalf("decision = %q, want %q (reason %q)", res.Decision, DecisionNoOp, res.Reason)
	}
	api.assertCalls(t, 1, 0, 0, 0)
	if got := api.runsCalls[0]; got != rewatchSHA {
		t.Fatalf("watchdog runs listed for %q, want %q", got, rewatchSHA)
	}
}

func TestRewatch_LatestRunInProgressIsANoOpAfterOneCall(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "waiting", "pending"} {
		t.Run(status, func(t *testing.T) {
			api := &fakeRewatchAPI{runs: []WorkflowRun{watchdogRun(900, status, "", 0)}}
			res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
			if err != nil {
				t.Fatal(err)
			}
			if res.Decision != DecisionNoOp {
				t.Fatalf("decision = %q, want no-op", res.Decision)
			}
			api.assertCalls(t, 1, 0, 0, 0)
		})
	}
}

func TestRewatch_LatestRunSuccessIsANoOpAfterOneCall(t *testing.T) {
	api := &fakeRewatchAPI{runs: []WorkflowRun{watchdogRun(900, "completed", "success", 0)}}
	res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionNoOp {
		t.Fatalf("decision = %q, want no-op", res.Decision)
	}
	api.assertCalls(t, 1, 0, 0, 0)
}

func TestRewatch_OnlyFailureAndCancelledConclusionsContinue(t *testing.T) {
	for _, tc := range []struct {
		conclusion string
		continues  bool
	}{
		{"failure", true},
		{"cancelled", true}, //nolint:misspell // the REST API's spelling
		{"success", false},
		{"skipped", false},
		{"neutral", false},
		{"timed_out", false},
		{"action_required", false},
		{"stale", false},
		{"startup_failure", false},
		// The check-run constant spells it with one l; the workflow-run API
		// uses two, so the one-l form must not be treated as that conclusion.
		{string(ConclusionCancelled), false},
	} {
		t.Run(tc.conclusion, func(t *testing.T) {
			api := healable()
			api.runs = []WorkflowRun{watchdogRun(900, "completed", tc.conclusion, 0)}
			res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
			if err != nil {
				t.Fatal(err)
			}
			if tc.continues {
				if res.Decision != DecisionRerun || len(api.reruns) != 1 {
					t.Fatalf("decision = %q, reruns = %v, want one re-run", res.Decision, api.reruns)
				}
				return
			}
			if res.Decision != DecisionNoOp {
				t.Fatalf("decision = %q, want no-op", res.Decision)
			}
			api.assertCalls(t, 1, 0, 0, 0)
		})
	}
}

func TestRewatch_LatestRunWinsByCreatedAtThenHigherID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runs      []WorkflowRun
		wantRerun int64 // 0: no re-run
	}{
		{"newer success supersedes older failure", []WorkflowRun{
			watchdogRun(1, "completed", "failure", 0),
			watchdogRun(2, "completed", "success", time.Minute),
		}, 0},
		{"newer failure supersedes older success, order independent", []WorkflowRun{
			watchdogRun(2, "completed", "failure", time.Minute),
			watchdogRun(1, "completed", "success", 0),
		}, 2},
		{"newer in-progress supersedes older failure", []WorkflowRun{
			watchdogRun(1, "completed", "failure", 0),
			watchdogRun(2, "in_progress", "", time.Minute),
		}, 0},
		{"tie on created_at goes to the higher id: success", []WorkflowRun{
			watchdogRun(7, "completed", "failure", 0),
			watchdogRun(8, "completed", "success", 0),
		}, 0},
		{"tie on created_at goes to the higher id: failure", []WorkflowRun{
			watchdogRun(8, "completed", "failure", 0),
			watchdogRun(7, "completed", "success", 0),
		}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := healable()
			api.runs = tc.runs
			if _, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA}); err != nil {
				t.Fatal(err)
			}
			if tc.wantRerun == 0 {
				api.assertCalls(t, 1, 0, 0, 0)
				return
			}
			if len(api.reruns) != 1 || api.reruns[0] != tc.wantRerun {
				t.Fatalf("reruns = %v, want exactly [%d]", api.reruns, tc.wantRerun)
			}
		})
	}
}

func TestRewatch_FailingPreCheckStopsBeforeAnyPullRequestCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []CheckRun
	}{
		{"no evidence at all", nil},
		{"preflight failed", []CheckRun{{Name: CheckName, HeadSHA: rewatchSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: rewatchT0, ID: 1}}},
		{"CI required still running", []CheckRun{
			{Name: CheckName, HeadSHA: rewatchSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: rewatchT0, ID: 1},
			{Name: CIRequiredName, HeadSHA: rewatchSHA, Status: StatusInProgress, StartedAt: rewatchT0, ID: 2},
		}},
		{"CI required failed", []CheckRun{
			{Name: CheckName, HeadSHA: rewatchSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: rewatchT0, ID: 1},
			{Name: CIRequiredName, HeadSHA: rewatchSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: rewatchT0, ID: 2},
		}},
		{"evidence belongs to another sha", passingChecks("ffffffffffffffffffffffffffffffffffffffff")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := healable()
			api.checkRuns = tc.runs
			res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
			if err != nil {
				t.Fatal(err)
			}
			if res.Decision != DecisionNoOp {
				t.Fatalf("decision = %q, want no-op", res.Decision)
			}
			api.assertCalls(t, 1, 1, 0, 0)
		})
	}
}

func TestRewatch_PreCheckIsLabelAgnosticAndAtTheDeadline(t *testing.T) {
	// Mac evidence is absent. A label-agnostic pre-check must not need it, and
	// an "incomplete" CI is a failure now, not a reason to wait.
	api := healable()
	api.pulls = [][]PullRequest{{prAt(42, rewatchSHA)}}
	if _, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA}); err != nil {
		t.Fatal(err)
	}
	if len(api.reruns) != 1 {
		t.Fatalf("reruns = %v, want one: Check and CI / required are enough when no label asks for more", api.reruns)
	}
}

func TestRewatch_NotTheCurrentHeadOfAnOpenPRIsANoOp(t *testing.T) {
	api := healable()
	api.pulls = [][]PullRequest{{prAt(41, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), prAt(43, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}}
	res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionNoOp {
		t.Fatalf("decision = %q, want no-op", res.Decision)
	}
	if !strings.Contains(res.Reason, "not the current head of an open PR") {
		t.Fatalf("reason = %q, want it to say the sha is not the current head of an open PR", res.Reason)
	}
	if len(api.reruns) != 0 {
		t.Fatalf("reruns = %v, want none", api.reruns)
	}
}

func TestRewatch_OpenPullRequestPaging(t *testing.T) {
	t.Run("match on page 3 stops paging", func(t *testing.T) {
		api := healable()
		api.pulls = [][]PullRequest{filler(1000), filler(2000), append(filler(3000)[:5], prAt(77, rewatchSHA)), filler(4000)}
		res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(api.pullPages), "[1 2 3]"; got != want {
			t.Fatalf("pages requested = %s, want %s", got, want)
		}
		if res.PRNumber != 77 || len(api.reruns) != 1 {
			t.Fatalf("PRNumber = %d, reruns = %v, want PR 77 and one re-run", res.PRNumber, api.reruns)
		}
	})
	t.Run("exhausted after a short page", func(t *testing.T) {
		api := healable()
		api.pulls = [][]PullRequest{filler(1000), {prAt(5, "cccccccccccccccccccccccccccccccccccccccc")}}
		res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(api.pullPages), "[1 2]"; got != want {
			t.Fatalf("pages requested = %s, want %s", got, want)
		}
		if res.Decision != DecisionNoOp || len(api.reruns) != 0 {
			t.Fatalf("decision = %q, reruns = %v, want no-op", res.Decision, api.reruns)
		}
	})
	t.Run("exhausted on an empty page after full pages", func(t *testing.T) {
		api := healable()
		api.pulls = [][]PullRequest{filler(1000), {}}
		if _, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA}); err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(api.pullPages), "[1 2]"; got != want {
			t.Fatalf("pages requested = %s, want %s", got, want)
		}
	})
}

func TestRewatch_TwoOpenPRsSharingAHeadSHAReRunOnce(t *testing.T) {
	api := healable()
	api.pulls = [][]PullRequest{{prAt(10, rewatchSHA), prAt(11, rewatchSHA, "needs-mac")}}
	res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.reruns) != 1 {
		t.Fatalf("reruns = %v, want exactly one for one watchdog run", api.reruns)
	}
	if res.PRNumber != 10 {
		t.Fatalf("PRNumber = %d, want the first listed PR (10)", res.PRNumber)
	}
}

func TestRewatch_CurrentLabelsDecide(t *testing.T) {
	t.Run("needs-mac without Mac evidence does not re-run", func(t *testing.T) {
		api := healable()
		api.pulls = [][]PullRequest{{prAt(42, rewatchSHA, "needs-mac")}}
		res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		if err != nil {
			t.Fatal(err)
		}
		if res.Decision != DecisionNoOp || len(api.reruns) != 0 {
			t.Fatalf("decision = %q, reruns = %v, want no-op", res.Decision, api.reruns)
		}
		api.assertCalls(t, 1, 1, 1, 0)
	})
	t.Run("needs-review-formulas without that evidence does not re-run", func(t *testing.T) {
		api := healable()
		api.pulls = [][]PullRequest{{prAt(42, rewatchSHA, "needs-review-formulas")}}
		res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		if err != nil {
			t.Fatal(err)
		}
		if res.Decision != DecisionNoOp || len(api.reruns) != 0 {
			t.Fatalf("decision = %q, reruns = %v, want no-op", res.Decision, api.reruns)
		}
	})
	t.Run("needs-mac with Mac evidence re-runs", func(t *testing.T) {
		api := healable()
		api.checkRuns = withMacSuccess(rewatchSHA, passingChecks(rewatchSHA))
		api.pulls = [][]PullRequest{{prAt(42, rewatchSHA, "needs-mac")}}
		res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		if err != nil {
			t.Fatal(err)
		}
		if res.Decision != DecisionRerun || len(api.reruns) != 1 {
			t.Fatalf("decision = %q, reruns = %v, want one re-run", res.Decision, api.reruns)
		}
	})
	t.Run("unrelated labels are ignored", func(t *testing.T) {
		api := healable()
		api.pulls = [][]PullRequest{{prAt(42, rewatchSHA, "bug", "needs-mac-later")}}
		if _, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA}); err != nil {
			t.Fatal(err)
		}
		if len(api.reruns) != 1 {
			t.Fatalf("reruns = %v, want one", api.reruns)
		}
	})
}

func TestRewatch_WouldPassReRunsExactlyTheLatestRun(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled"} { //nolint:misspell // the REST API's spelling
		t.Run(conclusion, func(t *testing.T) {
			api := healable()
			api.runs = []WorkflowRun{
				watchdogRun(800, "completed", "failure", -time.Hour),
				{ID: 900, RunAttempt: 3, Status: "completed", Conclusion: conclusion, CreatedAt: rewatchT0},
			}
			res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
			if err != nil {
				t.Fatal(err)
			}
			api.assertCalls(t, 1, 1, 1, 1)
			if api.reruns[0] != 900 {
				t.Fatalf("re-ran %d, want the latest run 900", api.reruns[0])
			}
			if res.Decision != DecisionRerun || res.RunID != 900 || res.RunAttempt != 3 || res.PRNumber != 42 || res.HeadSHA != rewatchSHA {
				t.Fatalf("result = %+v, want decision re-run for run 900 attempt 3 on PR 42", res)
			}
			if res.Checks.Check != "success" || res.Checks.CIRequired != "success" {
				t.Fatalf("result checks = %+v, want the evaluated check states", res.Checks)
			}
		})
	}
}

func TestRewatch_DryRunStopsBeforeThePOST(t *testing.T) {
	api := healable()
	res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	api.assertCalls(t, 1, 1, 1, 0)
	if res.Decision != DecisionWouldRerun || res.RunID != 900 || res.PRNumber != 42 {
		t.Fatalf("result = %+v, want a would-re-run decision naming run 900 and PR 42", res)
	}
}

func TestRewatch_RerunRefusals(t *testing.T) {
	t.Run("unknown refusal is a failure", func(t *testing.T) {
		for _, refusal := range []error{
			&APIError{Status: 403, Body: `{"message":"Resource not accessible by integration"}`},
			&APIError{Status: 404, Body: `{"message":"Not Found"}`},
			&APIError{Status: 422, Body: `{"message":"Validation Failed"}`},
			&APIError{Status: 500, Body: `boom`},
			errors.New("connection reset"),
		} {
			api := healable()
			api.rerunErr = refusal
			res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
			if err == nil {
				t.Fatalf("refusal %v returned no error; an unknown refusal must fail", refusal)
			}
			if !errors.Is(err, refusal) && !strings.Contains(err.Error(), refusal.Error()) {
				t.Fatalf("error %q does not carry the refusal %q", err, refusal)
			}
			if len(api.reruns) != 1 {
				t.Fatalf("reruns = %v, want exactly one attempt and no client-side retry", api.reruns)
			}
			if res.RunID != 900 {
				t.Fatalf("result = %+v, want it populated so the summary can still be written", res)
			}
		}
	})
	t.Run("a 403 that mentions the 50-run limit is a failure after one POST", func(t *testing.T) {
		// No public source quotes the refusal for the 50-run limit, so this body
		// is invented and deliberately unpinned: it must reach the operator.
		api := healable()
		api.rerunErr = &APIError{Status: 403, Body: `{"message":"This workflow run has already been re-run 50 times"}`}
		res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		if err == nil {
			t.Fatalf("a 403 mentioning the 50-run limit returned no error (treated as a benign no-op); reruns=%v", api.reruns)
		}
		api.assertCalls(t, 1, 1, 1, 1)
		if res.Decision == DecisionRerun || res.RunID != 900 {
			t.Fatalf("result = %+v, want it populated for the summary and not a re-run", res)
		}
	})
	t.Run("rate limit during the POST is a failure", func(t *testing.T) {
		api := healable()
		api.rerunErr = &RateLimitError{Reset: rewatchT0, Err: errors.New("limited")}
		_, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
		var limited *RateLimitError
		if !errors.As(err, &limited) {
			t.Fatalf("err = %v, want *RateLimitError", err)
		}
		if len(api.reruns) != 1 {
			t.Fatalf("reruns = %v, want one attempt", api.reruns)
		}
	})
	t.Run("a benign refusal is a no-op that names the refusal", func(t *testing.T) {
		if len(benignRerunRefusals) != 2 {
			t.Fatalf("benignRerunRefusals has %d entries, want exactly the two the ruling allows (already running, past the 30-day limit)", len(benignRerunRefusals))
		}
		for _, benign := range benignRerunRefusals {
			api := healable()
			api.rerunErr = &APIError{Status: benign.Status, Body: benign.Body}
			res, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
			if err != nil {
				t.Fatalf("benign refusal %+v returned error %v, want exit-0 no-op", benign, err)
			}
			if res.Decision != DecisionNoOp || len(api.reruns) != 1 {
				t.Fatalf("decision = %q, reruns = %v, want no-op after one attempt", res.Decision, api.reruns)
			}
			if !strings.Contains(res.Reason, benign.Name) {
				t.Fatalf("reason = %q, want it to name the refusal %q", res.Reason, benign.Name)
			}
		}
	})
	t.Run("a benign body under another status is not benign", func(t *testing.T) {
		for _, benign := range benignRerunRefusals {
			api := healable()
			api.rerunErr = &APIError{Status: benign.Status + 1, Body: benign.Body}
			if _, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA}); err == nil {
				t.Fatalf("status %d with the body of %q returned no error", benign.Status+1, benign.Name)
			}
		}
	})
}

func TestRewatch_FetchErrorsFailWithoutRetry(t *testing.T) {
	boom := errors.New("api exploded")
	limited := &RateLimitError{Reset: rewatchT0, Err: errors.New("slow down")}
	for _, tc := range []struct {
		name  string
		setup func(*fakeRewatchAPI, error)
		calls [4]int // runs, checks, pull pages, reruns
	}{
		{"watchdog run list", func(f *fakeRewatchAPI, e error) { f.runsErr = e }, [4]int{1, 0, 0, 0}},
		{"check run fetch", func(f *fakeRewatchAPI, e error) { f.checkErr = e }, [4]int{1, 1, 0, 0}},
		{"pull request list", func(f *fakeRewatchAPI, e error) { f.pullErr = e }, [4]int{1, 1, 1, 0}},
	} {
		for _, injected := range []error{boom, limited} {
			t.Run(fmt.Sprintf("%s/%T", tc.name, injected), func(t *testing.T) {
				api := healable()
				tc.setup(api, injected)
				_, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: rewatchSHA})
				if err == nil {
					t.Fatal("want an error: a broken helper must be visible, not a quiet no-op")
				}
				if !errors.Is(err, injected) {
					t.Fatalf("err = %v, want it to wrap %v", err, injected)
				}
				api.assertCalls(t, tc.calls[0], tc.calls[1], tc.calls[2], tc.calls[3])
			})
		}
	}
}

func TestRewatch_RejectsAMalformedHeadSHABeforeAnyCall(t *testing.T) {
	for _, bad := range []string{
		"",
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		strings.ToUpper(rewatchSHA),
		"0123456789abcdef0123456789abcdef0123456g",
		"abc; rm -rf /",
		"$(id)",
		rewatchSHA + "\n",
		rewatchSHA[:39] + "`",
		"../" + rewatchSHA[:37],
	} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			api := healable()
			if _, err := Rewatch(context.Background(), api, RewatchRequest{HeadSHA: bad}); err == nil {
				t.Fatal("want an error")
			}
			api.assertCalls(t, 0, 0, 0, 0)
		})
	}
}

func TestValidateHeadSHA(t *testing.T) {
	if err := ValidateHeadSHA(rewatchSHA); err != nil {
		t.Fatalf("ValidateHeadSHA(%q) = %v, want nil", rewatchSHA, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.ToUpper(rewatchSHA), rewatchSHA + "0", rewatchSHA + "\n"} {
		if err := ValidateHeadSHA(bad); err == nil {
			t.Errorf("ValidateHeadSHA(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateRepository(t *testing.T) {
	for _, ok := range []string{"gastownhall/gascity", "a/b", "Org-1/repo.name_2"} {
		if err := ValidateRepository(ok); err != nil {
			t.Errorf("ValidateRepository(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "gascity", "a/b/c", "/b", "a/", "a b/c", "a/b;id", "a/$(id)", "a/b\n", "../x/y", "a/b?x=1", "https://x/y"} {
		if err := ValidateRepository(bad); err == nil {
			t.Errorf("ValidateRepository(%q) = nil, want an error", bad)
		}
	}
}
