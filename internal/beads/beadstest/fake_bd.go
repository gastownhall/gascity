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
// list, update, close and reopen; any other command fails loudly, so a case
// that needs more of bd fails rather than silently passing.
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

// fakeBdIssue is the bd JSON row shape the fake emits (the fields BdStore
// decodes that the conformance cases depend on).
type fakeBdIssue struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	IssueType   string            `json:"issue_type"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Assignee    string            `json:"assignee"`
	Description string            `json:"description"`
	Labels      []string          `json:"labels"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CloseReason string            `json:"close_reason,omitempty"`
	Revision    int64             `json:"revision,omitempty"`
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
	flags, positional := parseFakeBdArgs(args[1:])
	switch args[0] {
	case "create":
		return f.create(flags, positional)
	case "show":
		return f.show(positional)
	case "list":
		return f.list(flags)
	case "update":
		return f.update(flags, positional)
	case "close":
		return f.closeIssues(flags, positional)
	case "reopen":
		return f.reopen(positional)
	case "query":
		// The wisp-tier query: the fake holds no wisps.
		return []byte("[]"), nil
	case "dep":
		if len(args) > 1 && args[1] == "list" {
			// The fake holds no dependency edges.
			return []byte("[]"), nil
		}
	}
	return nil, fmt.Errorf("fake bd: unsupported command: bd %s", strings.Join(args, " "))
}

// parseFakeBdArgs splits bd arguments into flags (repeatable, "--k v" and
// "--k=v" spellings) and positional arguments. Boolean flags take no value.
func parseFakeBdArgs(args []string) (map[string][]string, []string) {
	boolean := map[string]bool{"--json": true, "--force": true, "--all": true, "--ephemeral": true, "--no-history": true, "--flat": true}
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
	if v, ok := lastFlag(flags, "--limit", "-n"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			query.Limit = n
		}
	}
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

func (f *FakeBd) update(flags map[string][]string, positional []string) ([]byte, error) {
	if len(positional) != 1 {
		return nil, fmt.Errorf("fake bd update: want one id, got %v", positional)
	}
	id := positional[0]
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
	if err := f.store.Update(id, opts); err != nil {
		return nil, fmt.Errorf("no issue found matching %q: %w", id, err)
	}
	return f.show([]string{id})
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
