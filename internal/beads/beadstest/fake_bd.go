package beadstest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// FakeBd is a minimal, stateful stand-in for the bd CLI, backed by a MemStore,
// for driving a BdStore through conformance cases without a real bd binary.
// It speaks the subset of bd's JSON surface a BdStore uses for create, show,
// list, query, dep list, update, close and reopen; any other command fails
// loudly, so a case that needs more of bd fails rather than silently passing.
//
// Like bd, it keeps a bead's parent as a parent-child edge (`bd dep list`
// reports it), never lists an ephemeral bead through `bd list`, and answers
// ephemeral beads through `bd query "ephemeral=true AND ..."` and `bd show`.
type FakeBd struct {
	mu    sync.Mutex
	store *beads.MemStore
}

// NewFakeBd returns an empty fake bd.
func NewFakeBd() *FakeBd {
	return &FakeBd{store: beads.NewMemStore()}
}

// NewBdStore returns a BdStore whose bd commands run against f.
func (f *FakeBd) NewBdStore() *beads.BdStore {
	return beads.NewBdStore("/fake-bd", f.Run)
}

// Seed stores b directly, as a fixture, with its ParentID, Ephemeral flag and
// metadata as given, and returns it with its minted ID.
func (f *FakeBd) Seed(b beads.Bead) (beads.Bead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store.Create(b)
}

// SeedDep stores the edge issueID -> dependsOnID of depType directly, as a
// fixture.
func (f *FakeBd) SeedDep(issueID, dependsOnID, depType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store.DepAdd(issueID, dependsOnID, depType)
}

// Bead returns the fake's current row for id.
func (f *FakeBd) Bead(id string) (beads.Bead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store.Get(id)
}

// fakeBdIssue is the bd JSON row shape the fake emits (the fields BdStore
// decodes that the conformance cases depend on).
type fakeBdIssue struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Status         string            `json:"status"`
	IssueType      string            `json:"issue_type"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	Assignee       string            `json:"assignee"`
	Description    string            `json:"description"`
	Labels         []string          `json:"labels"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CloseReason    string            `json:"close_reason,omitempty"`
	Revision       int64             `json:"revision,omitempty"`
	Parent         string            `json:"parent,omitempty"`
	Ephemeral      bool              `json:"ephemeral,omitempty"`
	DependencyType string            `json:"dependency_type,omitempty"`
}

func toFakeBdIssue(b beads.Bead) fakeBdIssue {
	typ := b.Type
	if typ == "" {
		typ = "task"
	}
	return fakeBdIssue{
		ID: b.ID, Title: b.Title, Status: b.Status, IssueType: typ,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, Assignee: b.Assignee,
		Description: b.Description, Labels: b.Labels, Metadata: b.Metadata,
		CloseReason: b.CloseReason, Revision: b.Revision,
		Parent: b.ParentID, Ephemeral: b.Ephemeral,
	}
}

// Run is the beads.CommandRunner the fake exposes.
func (f *FakeBd) Run(_, name string, args ...string) ([]byte, error) {
	if name != "bd" {
		return nil, fmt.Errorf("fake bd: unexpected command %q", name)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("fake bd: no subcommand")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "create":
		flags, positional := parseFakeBdArgs(args[1:])
		return f.create(flags, positional)
	case "show":
		_, positional := parseFakeBdArgs(args[1:])
		return f.show(positional)
	case "list":
		flags, _ := parseFakeBdArgs(args[1:])
		return f.list(flags)
	case "update":
		flags, positional := parseFakeBdArgs(args[1:])
		return f.update(flags, positional)
	case "close":
		flags, positional := parseFakeBdArgs(args[1:])
		return f.closeIssues(flags, positional)
	case "reopen":
		_, positional := parseFakeBdArgs(args[1:])
		return f.reopen(positional)
	case "query":
		flags, positional := parseFakeBdArgs(args[1:])
		return f.query(flags, positional)
	case "dep":
		if len(args) > 1 && args[1] == "list" {
			flags, positional := parseFakeBdArgs(args[2:])
			return f.depList(flags, positional)
		}
	}
	return nil, fmt.Errorf("fake bd: unsupported command: bd %s", strings.Join(args, " "))
}

// parseFakeBdArgs splits bd arguments into flags (repeatable, "--k v" and
// "--k=v" spellings) and positional arguments. Boolean flags take no value.
func parseFakeBdArgs(args []string) (map[string][]string, []string) {
	boolean := map[string]bool{
		"--json": true, "--force": true, "--all": true, "--ephemeral": true, "--no-history": true, "--flat": true,
		"--include-infra": true, "--include-gates": true, "--include-templates": true, "--skip-labels": true,
	}
	flags := map[string][]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		if k, v, ok := strings.Cut(arg, "="); ok {
			flags[k] = append(flags[k], v)
			continue
		}
		if boolean[arg] || i+1 >= len(args) {
			flags[arg] = append(flags[arg], "")
			continue
		}
		flags[arg] = append(flags[arg], args[i+1])
		i++
	}
	return flags, positional
}

func lastFlag(flags map[string][]string, names ...string) (string, bool) {
	for _, name := range names {
		if v, ok := flags[name]; ok && len(v) > 0 {
			return v[len(v)-1], true
		}
	}
	return "", false
}

func (f *FakeBd) create(flags map[string][]string, positional []string) ([]byte, error) {
	b := beads.Bead{Status: "open"}
	if len(positional) > 0 {
		b.Title = positional[0]
	}
	if v, ok := lastFlag(flags, "-t", "--type"); ok {
		b.Type = v
	}
	if v, ok := lastFlag(flags, "--assignee"); ok {
		b.Assignee = v
	}
	if v, ok := lastFlag(flags, "--description"); ok {
		b.Description = v
	}
	if v, ok := lastFlag(flags, "--labels"); ok && v != "" {
		b.Labels = strings.Split(v, ",")
	}
	if v, ok := lastFlag(flags, "--metadata"); ok {
		if err := json.Unmarshal([]byte(v), &b.Metadata); err != nil {
			return nil, fmt.Errorf("fake bd create: --metadata: %w", err)
		}
	}
	created, err := f.store.Create(b)
	if err != nil {
		return nil, err
	}
	return json.Marshal(toFakeBdIssue(created))
}

func (f *FakeBd) show(ids []string) ([]byte, error) {
	out := make([]fakeBdIssue, 0, len(ids))
	for _, id := range ids {
		b, err := f.store.Get(id)
		if err != nil {
			return nil, fmt.Errorf("no issue found matching %q", id)
		}
		out = append(out, toFakeBdIssue(b))
	}
	return json.Marshal(out)
}

func (f *FakeBd) list(flags map[string][]string) ([]byte, error) {
	query := beads.ListQuery{AllowScan: true}
	if _, all := flags["--all"]; all {
		query.IncludeClosed = true
	}
	if v, ok := lastFlag(flags, "--status"); ok {
		query.Status = v
		query.IncludeClosed = query.IncludeClosed || v == "closed"
	}
	if v, ok := lastFlag(flags, "--assignee"); ok {
		query.Assignee = v
	}
	if v, ok := lastFlag(flags, "--label"); ok {
		query.Label = v
	}
	if v, ok := lastFlag(flags, "--type"); ok {
		query.Type = v
	}
	if v, ok := lastFlag(flags, "--parent"); ok {
		query.ParentID = v
	}
	for _, kv := range flags["--metadata-field"] {
		k, v, _ := strings.Cut(kv, "=")
		if query.Metadata == nil {
			query.Metadata = map[string]string{}
		}
		query.Metadata[k] = v
	}
	if v, ok := lastFlag(flags, "--limit", "-n"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			query.Limit = n
		}
	}
	// TierIssues: like bd list, never an ephemeral row.
	found, err := f.store.List(query)
	if err != nil {
		return nil, err
	}
	out := make([]fakeBdIssue, 0, len(found))
	for _, b := range found {
		out = append(out, toFakeBdIssue(b))
	}
	return json.Marshal(out)
}

// query answers bd's wisp-tier query: `bd query --json "ephemeral=true AND
// k=v AND ..." [--all] [--limit N]`, ephemeral rows only, for the keys a
// BdStore sends (id, parent, status, type, label, assignee).
func (f *FakeBd) query(flags map[string][]string, positional []string) ([]byte, error) {
	if len(positional) != 1 {
		return nil, fmt.Errorf("fake bd query: want one expression, got %v", positional)
	}
	query := beads.ListQuery{AllowScan: true, TierMode: beads.TierWisps}
	if _, all := flags["--all"]; all {
		query.IncludeClosed = true
	}
	ephemeral := false
	for _, clause := range strings.Split(positional[0], " AND ") {
		k, v, ok := strings.Cut(strings.TrimSpace(clause), "=")
		if !ok {
			return nil, fmt.Errorf("fake bd query: unsupported clause %q", clause)
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "ephemeral":
			ephemeral = v == "true"
		case "id":
			query.IDs = []string{v}
		case "parent":
			query.ParentID = v
		case "status":
			query.Status = v
			query.IncludeClosed = query.IncludeClosed || v == "closed"
		case "type":
			query.Type = v
		case "label":
			query.Label = v
		case "assignee":
			query.Assignee = v
		default:
			return nil, fmt.Errorf("fake bd query: unsupported key %q", k)
		}
	}
	if !ephemeral {
		return nil, fmt.Errorf("fake bd query: only the ephemeral=true tier query is supported: %q", positional[0])
	}
	found, err := f.store.List(query)
	if err != nil {
		return nil, err
	}
	limit := 0
	if v, ok := lastFlag(flags, "--limit", "-n"); ok {
		limit, _ = strconv.Atoi(v)
	}
	out := make([]fakeBdIssue, 0, len(found))
	for _, b := range found {
		if !b.Ephemeral {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, toFakeBdIssue(b))
	}
	return json.Marshal(out)
}

// edges returns every stored edge plus each bead's parent as the parent-child
// edge bd keeps it as.
func (f *FakeBd) edges() ([]beads.Dep, error) {
	all, err := f.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		return nil, err
	}
	var edges []beads.Dep
	for _, b := range all {
		if b.ParentID != "" {
			edges = append(edges, beads.Dep{IssueID: b.ID, DependsOnID: b.ParentID, Type: "parent-child"})
		}
		deps, err := f.store.DepList(b.ID, "down")
		if err != nil {
			return nil, err
		}
		edges = append(edges, deps...)
	}
	return edges, nil
}

// depList answers `bd dep list`. With --direction=up it prints the rows of
// the beads that depend on any listed id (filtered by --type), each carrying
// dependency_type. Down, one id prints the rows of the beads it depends on;
// several ids print the raw dependency records, as bd does.
func (f *FakeBd) depList(flags map[string][]string, ids []string) ([]byte, error) {
	edges, err := f.edges()
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	depType, _ := lastFlag(flags, "--type", "-t")
	direction, _ := lastFlag(flags, "--direction")
	if direction == "up" || len(ids) == 1 {
		out := []fakeBdIssue{}
		for _, e := range edges {
			if depType != "" && e.Type != depType {
				continue
			}
			other := e.DependsOnID
			if direction == "up" {
				if !wanted[e.DependsOnID] {
					continue
				}
				other = e.IssueID
			} else if !wanted[e.IssueID] {
				continue
			}
			b, err := f.store.Get(other)
			if err != nil {
				continue
			}
			row := toFakeBdIssue(b)
			row.DependencyType = e.Type
			out = append(out, row)
		}
		return json.Marshal(out)
	}
	type record struct {
		IssueID     string `json:"issue_id"`
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	}
	out := []record{}
	for _, e := range edges {
		if !wanted[e.IssueID] || (depType != "" && e.Type != depType) {
			continue
		}
		out = append(out, record{IssueID: e.IssueID, DependsOnID: e.DependsOnID, Type: e.Type})
	}
	return json.Marshal(out)
}

// fakeBdUpdateFlags are the `bd update` flags the fake applies. An update
// carrying any other flag fails, so a BdStore write the fake does not model
// fails its case instead of reporting success with the write dropped.
var fakeBdUpdateFlags = map[string]bool{
	"--json": true, "--status": true, "-s": true, "--title": true, "--assignee": true,
	"--description": true, "--set-metadata": true, "--unset-metadata": true,
	"--if-status": true, "--if-assignee": true,
}

// fakeBdPreconditionError is bd's refusal of an --if-status/--if-assignee
// update whose precondition does not hold: exit 13 with nothing written, which
// BdStore reads as a lost compare-and-swap rather than a failure.
type fakeBdPreconditionError struct{ msg string }

func (e fakeBdPreconditionError) Error() string { return e.msg }

// ExitCode is bd's precondition exit status.
func (fakeBdPreconditionError) ExitCode() int { return 13 }

func (f *FakeBd) update(flags map[string][]string, positional []string) ([]byte, error) {
	for name := range flags {
		if !fakeBdUpdateFlags[name] {
			return nil, fmt.Errorf("fake bd update: unsupported flag %s", name)
		}
	}
	if len(positional) == 0 {
		return nil, fmt.Errorf("fake bd update: want at least one id")
	}
	for _, id := range positional {
		if err := f.checkUpdatePreconditions(id, flags); err != nil {
			return nil, err
		}
	}
	var opts beads.UpdateOpts
	if v, ok := lastFlag(flags, "--status", "-s"); ok {
		opts.Status = &v
	}
	if v, ok := lastFlag(flags, "--title"); ok {
		opts.Title = &v
	}
	if v, ok := lastFlag(flags, "--assignee"); ok {
		opts.Assignee = &v
	}
	if v, ok := lastFlag(flags, "--description"); ok {
		opts.Description = &v
	}
	for _, kv := range flags["--set-metadata"] {
		k, v, _ := strings.Cut(kv, "=")
		if opts.Metadata == nil {
			opts.Metadata = map[string]string{}
		}
		opts.Metadata[k] = v
	}
	// bd applies every unset after every set, as the store does.
	opts.UnsetMetadata = flags["--unset-metadata"]
	for _, id := range positional {
		if err := f.store.Update(id, opts); err != nil {
			return nil, fmt.Errorf("no issue found matching %q: %w", id, err)
		}
	}
	return f.show(positional)
}

// checkUpdatePreconditions applies bd's --if-status and --if-assignee: the
// update runs only when the bead holds exactly that status and assignee.
func (f *FakeBd) checkUpdatePreconditions(id string, flags map[string][]string) error {
	wantStatus, fenceStatus := lastFlag(flags, "--if-status")
	wantAssignee, fenceAssignee := lastFlag(flags, "--if-assignee")
	if !fenceStatus && !fenceAssignee {
		return nil
	}
	current, err := f.store.Get(id)
	if err != nil {
		return fmt.Errorf("no issue found matching %q: %w", id, err)
	}
	if (fenceStatus && current.Status != wantStatus) || (fenceAssignee && current.Assignee != wantAssignee) {
		return fakeBdPreconditionError{msg: fmt.Sprintf("fake bd update %s: precondition failed: status %q assignee %q", id, current.Status, current.Assignee)}
	}
	return nil
}

func (f *FakeBd) closeIssues(flags map[string][]string, ids []string) ([]byte, error) {
	reason, _ := lastFlag(flags, "--reason", "-r")
	for _, id := range ids {
		if reason != "" {
			if err := f.store.SetMetadata(id, "close_reason", reason); err != nil {
				return nil, fmt.Errorf("no issue found matching %q", id)
			}
		}
		if err := f.store.Close(id); err != nil {
			return nil, fmt.Errorf("no issue found matching %q", id)
		}
	}
	return f.show(ids)
}

func (f *FakeBd) reopen(ids []string) ([]byte, error) {
	for _, id := range ids {
		if err := f.store.Reopen(id); err != nil {
			return nil, fmt.Errorf("no issue found matching %q", id)
		}
	}
	return f.show(ids)
}
