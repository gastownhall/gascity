// Package main provides a tiny bd-compatible shim backed by file beads for
// integration tests that need deterministic local bead operations.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, stdout, stderr io.Writer) int {
	realBD := strings.TrimSpace(os.Getenv("GC_INTEGRATION_REAL_BD"))
	if len(args) == 0 {
		return proxy(realBD, args, stdout, stderr)
	}

	cityDir, ok := detectFileStoreCity()
	if !ok {
		return proxy(realBD, args, stdout, stderr)
	}

	code, handled, err := runFileStore(cityDir, args, stdout)
	if !handled {
		return proxy(realBD, args, stdout, stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err) //nolint:errcheck
		return 1
	}
	return code
}

func proxy(realBD string, args []string, stdout, stderr io.Writer) int {
	if realBD == "" {
		fmt.Fprintln(stderr, "bd shim: GC_INTEGRATION_REAL_BD not set") //nolint:errcheck
		return 1
	}
	cmd := exec.Command(realBD, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "bd shim: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func detectFileStoreCity() (string, bool) {
	if strings.TrimSpace(os.Getenv("GC_BEADS")) != "file" {
		return "", false
	}
	candidates := []string{
		strings.TrimSpace(os.Getenv("GC_CITY")),
		strings.TrimSpace(os.Getenv("GC_CITY_PATH")),
	}
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		if hasFileStore(cand) {
			return cand, true
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for dir := cwd; dir != "" && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if hasFileStore(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", false
}

func hasFileStore(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".gc", "beads.json"))
	return err == nil
}

func runFileStore(cityDir string, args []string, stdout io.Writer) (int, bool, error) {
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		return 0, false, err
	}
	defer recorder.Close() //nolint:errcheck

	switch args[0] {
	case "create":
		title, jsonOut, err := parseCreateArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		created, err := store.Create(beads.Bead{Title: title})
		if err != nil {
			return 0, true, err
		}
		record(recorder, events.BeadCreated, actor(), created.ID, created.Title)
		return 0, true, writeBead(stdout, created, jsonOut, true)
	case "show":
		id, jsonOut, err := parseShowArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		b, err := store.Get(id)
		if err != nil {
			return 0, true, err
		}
		return 0, true, writeBead(stdout, b, jsonOut, false)
	case "list":
		q, jsonOut, err := parseListArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		items, err := store.List(q)
		if err != nil {
			return 0, true, err
		}
		return 0, true, writeList(stdout, items, jsonOut)
	case "ready":
		q, jsonOut, err := parseReadyArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		items, err := store.List(q)
		if err != nil {
			return 0, true, err
		}
		return 0, true, writeList(stdout, items, jsonOut)
	case "close":
		id, jsonOut, err := parseCloseArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		if err := store.Close(id); err != nil {
			return 0, true, err
		}
		record(recorder, events.BeadClosed, actor(), id, "")
		if jsonOut {
			_, _ = fmt.Fprintln(stdout, `{"ok":true}`)
		}
		return 0, true, nil
	case "update":
		id, opts, jsonOut, err := parseUpdateArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		if err := store.Update(id, opts); err != nil {
			return 0, true, err
		}
		record(recorder, events.BeadUpdated, actor(), id, "")
		if jsonOut {
			b, err := store.Get(id)
			if err != nil {
				return 0, true, err
			}
			return 0, true, writeBead(stdout, b, true, false)
		}
		return 0, true, nil
	default:
		return 0, false, nil
	}
}

func parseCreateArgs(args []string) (title string, jsonOut bool, err error) {
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", false, fmt.Errorf("unsupported create flag %q", arg)
		}
		if title == "" {
			title = arg
			continue
		}
		title += " " + arg
	}
	if strings.TrimSpace(title) == "" {
		return "", false, fmt.Errorf("bd create: missing title")
	}
	return title, jsonOut, nil
}

func parseShowArgs(args []string) (id string, jsonOut bool, err error) {
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", false, fmt.Errorf("unsupported show flag %q", arg)
		}
		if id == "" {
			id = arg
			continue
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("bd show: missing bead id")
	}
	return id, jsonOut, nil
}

func parseCloseArgs(args []string) (id string, jsonOut bool, err error) {
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", false, fmt.Errorf("unsupported close flag %q", arg)
		}
		if id == "" {
			id = arg
			continue
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("bd close: missing bead id")
	}
	return id, jsonOut, nil
}

func parseListArgs(args []string) (beads.ListQuery, bool, error) {
	q := beads.ListQuery{AllowScan: true}
	jsonOut := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			jsonOut = true
		case arg == "--unassigned":
			q.Assignee = ""
		case strings.HasPrefix(arg, "--assignee="):
			q.Assignee = strings.TrimPrefix(arg, "--assignee=")
		case arg == "--assignee" && i+1 < len(args):
			i++
			q.Assignee = args[i]
		case strings.HasPrefix(arg, "--status="):
			q.Status = strings.TrimPrefix(arg, "--status=")
		case arg == "--status" && i+1 < len(args):
			i++
			q.Status = args[i]
		case strings.HasPrefix(arg, "--label="):
			q.Label = strings.TrimPrefix(arg, "--label=")
		case arg == "--label" && i+1 < len(args):
			i++
			q.Label = args[i]
		case strings.HasPrefix(arg, "--metadata-field="):
			key, value, ok := strings.Cut(strings.TrimPrefix(arg, "--metadata-field="), "=")
			if !ok || key == "" {
				return q, jsonOut, fmt.Errorf("invalid metadata-field %q", arg)
			}
			if q.Metadata == nil {
				q.Metadata = map[string]string{}
			}
			q.Metadata[key] = value
		case arg == "--metadata-field" && i+1 < len(args):
			i++
			key, value, ok := strings.Cut(args[i], "=")
			if !ok || key == "" {
				return q, jsonOut, fmt.Errorf("invalid metadata-field %q", args[i])
			}
			if q.Metadata == nil {
				q.Metadata = map[string]string{}
			}
			q.Metadata[key] = value
		case strings.HasPrefix(arg, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				return q, jsonOut, err
			}
			q.Limit = n
		case arg == "--limit" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return q, jsonOut, err
			}
			q.Limit = n
		case strings.HasPrefix(arg, "-"):
			return q, jsonOut, fmt.Errorf("unsupported list flag %q", arg)
		}
	}
	return q, jsonOut, nil
}

func parseReadyArgs(args []string) (beads.ListQuery, bool, error) {
	q, jsonOut, err := parseListArgs(args)
	if err != nil {
		return q, jsonOut, err
	}
	q.AllowScan = true
	q.IncludeClosed = false
	if q.Status == "" {
		q.Status = "open"
	}
	return q, jsonOut, nil
}

func parseUpdateArgs(args []string) (id string, opts beads.UpdateOpts, jsonOut bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			jsonOut = true
		case arg == "--claim":
			assignee := actor()
			opts.Assignee = &assignee
			status := "in_progress"
			opts.Status = &status
		case strings.HasPrefix(arg, "--assignee="):
			assignee := strings.TrimPrefix(arg, "--assignee=")
			opts.Assignee = &assignee
		case arg == "--assignee" && i+1 < len(args):
			i++
			assignee := args[i]
			opts.Assignee = &assignee
		case strings.HasPrefix(arg, "--status="):
			status := strings.TrimPrefix(arg, "--status=")
			opts.Status = &status
		case arg == "--status" && i+1 < len(args):
			i++
			status := args[i]
			opts.Status = &status
		case strings.HasPrefix(arg, "-"):
			return "", opts, false, fmt.Errorf("unsupported update flag %q", arg)
		case id == "":
			id = arg
		default:
			return "", opts, false, fmt.Errorf("unexpected update arg %q", arg)
		}
	}
	if id == "" {
		return "", opts, false, fmt.Errorf("bd update: missing bead id")
	}
	return id, opts, jsonOut, nil
}

func openFileStore(cityDir string) (beads.Store, *events.FileRecorder, error) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityDir, ".gc", "beads.json"))
	if err != nil {
		return nil, nil, err
	}
	store.SetLocker(beads.NewFileFlock(filepath.Join(cityDir, ".gc", "beads.json.lock")))
	recorder, err := events.NewFileRecorder(filepath.Join(cityDir, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return nil, nil, err
	}
	return store, recorder, nil
}

func actor() string {
	for _, key := range []string{"BEADS_ACTOR", "GC_SESSION_NAME", "GC_AGENT"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "human"
}

func record(recorder *events.FileRecorder, eventType string, actorName, subject, message string) {
	recorder.Record(events.Event{
		Type:    eventType,
		Actor:   actorName,
		Subject: subject,
		Message: message,
	})
}

func writeBead(stdout io.Writer, b beads.Bead, jsonOut bool, created bool) error {
	if jsonOut {
		return json.NewEncoder(stdout).Encode(map[string]any{
			"id":       b.ID,
			"title":    b.Title,
			"status":   b.Status,
			"assignee": b.Assignee,
			"type":     b.Type,
		})
	}
	if created {
		_, err := fmt.Fprintf(stdout, "Created bead: %s\n", b.ID)
		return err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "ID: %s\n", b.ID)
	fmt.Fprintf(&sb, "Title: %s\n", b.Title)
	fmt.Fprintf(&sb, "Status: %s\n", b.Status)
	if b.Assignee != "" {
		fmt.Fprintf(&sb, "Assignee: %s\n", b.Assignee)
	}
	_, err := io.WriteString(stdout, sb.String())
	return err
}

func writeList(stdout io.Writer, items []beads.Bead, jsonOut bool) error {
	if jsonOut {
		out := make([]map[string]any, 0, len(items))
		for _, b := range items {
			out = append(out, map[string]any{
				"id":       b.ID,
				"title":    b.Title,
				"status":   b.Status,
				"assignee": b.Assignee,
				"type":     b.Type,
			})
		}
		return json.NewEncoder(stdout).Encode(out)
	}
	if len(items) == 0 {
		_, err := fmt.Fprintln(stdout, "No beads.")
		return err
	}
	for _, b := range items {
		if _, err := fmt.Fprintf(stdout, "%s  %s  %s\n", b.ID, b.Status, b.Title); err != nil {
			return err
		}
	}
	return nil
}
