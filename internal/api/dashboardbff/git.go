package dashboardbff

import (
	"errors"
	"net/http"
	"strings"
)

// gitCommit is one parsed `git log` row. It mirrors shared/src/activity.ts
// GitCommit exactly: refs is omitted when empty (the SPA treats a missing
// refs field and an empty one the same, but the wire shape must match).
type gitCommit struct {
	Sha      string `json:"sha"`
	ShortSha string `json:"short_sha"`
	Author   string `json:"author"`
	Date     string `json:"date"`
	Subject  string `json:"subject"`
	Refs     string `json:"refs,omitempty"`
}

// gitCommitList is the GET /api/git/commits response, matching
// shared/src/activity.ts GitCommitList.
type gitCommitList struct {
	View  string      `json:"view"`
	Items []gitCommit `json:"items"`
}

// gitViews is the hardcoded set of accepted view names. Anything outside it is
// normalized to recent-main, matching the Node BFF (the enum is the auth
// boundary; the operator can never pass arbitrary git args).
var gitViews = map[string]struct{}{
	"recent-main": {},
	"recent-all":  {},
	"today":       {},
	"this-week":   {},
}

const defaultGitView = "recent-main"

// normalizeGitView returns view if it is a known view, otherwise the default.
// Mirrors git.ts: a missing or unrecognized view falls back to recent-main
// rather than erroring, so the SPA always receives a populated list.
func normalizeGitView(view string) string {
	if _, ok := gitViews[view]; ok {
		return view
	}
	return defaultGitView
}

// registerGit wires GET /api/git/commits onto the plane mux.
func (p *Plane) registerGit() {
	p.mux.HandleFunc("GET /api/git/commits", func(w http.ResponseWriter, r *http.Request) {
		view := normalizeGitView(r.URL.Query().Get("view"))
		result, err := p.exec.execGitLog(r.Context(), view)
		if err != nil {
			var ee *execError
			if errors.As(err, &ee) && ee.kind == execErrValidation {
				writeError(w, http.StatusBadRequest, "unknown git view")
				return
			}
			writeError(w, http.StatusBadGateway, "git log failed")
			return
		}
		writeJSON(w, http.StatusOK, gitCommitList{
			View:  view,
			Items: parseGitLog(sanitizeTerminalOutput(result.stdout)),
		})
	})
}

// parseGitLog turns tab-separated `git log` output into commits. The pretty
// format is "%H%x09%h%x09%an%x09%aI%x09%D%x09%s" — sha, short_sha, author,
// date, refs (%D, may be empty), subject. Lines with fewer than six fields or
// missing the required leading fields are skipped, matching git.ts::parseGitLog.
func parseGitLog(stdout string) []gitCommit {
	items := []gitCommit{}
	for _, line := range strings.Split(stdout, "\n") {
		if len(line) == 0 {
			continue
		}
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 6 {
			continue
		}
		sha, shortSha, author, date, refs, subject := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
		if sha == "" || shortSha == "" || author == "" || date == "" {
			continue
		}
		c := gitCommit{
			Sha:      sha,
			ShortSha: shortSha,
			Author:   author,
			Date:     date,
			Subject:  subject,
		}
		if refs != "" {
			c.Refs = refs
		}
		items = append(items, c)
	}
	return items
}
