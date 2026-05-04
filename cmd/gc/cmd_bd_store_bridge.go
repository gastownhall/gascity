package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

type bdStoreBridgeCreateRequest struct {
	Title       string            `json:"title"`
	Type        string            `json:"type,omitempty"`
	Priority    *int              `json:"priority,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	ParentID    string            `json:"parent_id,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Needs       []string          `json:"needs,omitempty"`
	Description string            `json:"description,omitempty"`
	Assignee    string            `json:"assignee,omitempty"`
	From        string            `json:"from,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type bdStoreBridgeUpdateRequest struct {
	Title        *string           `json:"title,omitempty"`
	Status       *string           `json:"status,omitempty"`
	Type         *string           `json:"type,omitempty"`
	Priority     *int              `json:"priority,omitempty"`
	Description  *string           `json:"description,omitempty"`
	ParentID     *string           `json:"parent_id,omitempty"`
	Assignee     *string           `json:"assignee,omitempty"`
	Labels       []string          `json:"labels,omitempty"`
	RemoveLabels []string          `json:"remove_labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type bdStoreBridgeBead struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	Type        string            `json:"type"`
	Priority    *int              `json:"priority,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Assignee    string            `json:"assignee,omitempty"`
	From        string            `json:"from,omitempty"`
	ParentID    string            `json:"parent_id,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Needs       []string          `json:"needs,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func newBdStoreBridgeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:                "bd-store-bridge <op> [args...]",
		Short:              "Internal bd-backed exec store bridge",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			op, opArgs, dir, host, port, user, err := parseBdStoreBridgeCommandArgs(args)
			if err != nil {
				fmt.Fprintf(stderr, "gc bd-store-bridge: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := runBdStoreBridge(op, opArgs, dir, host, port, user, os.Stdin, stdout); err != nil {
				fmt.Fprintf(stderr, "gc bd-store-bridge: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func parseBdStoreBridgeCommandArgs(args []string) (op string, opArgs []string, dir, host, port, user string, err error) {
	user = "root"
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			if i+1 >= len(args) {
				return "", nil, "", "", "", "", fmt.Errorf("flag %s requires a value", name)
			}
			value = args[i+1]
			i++
		}
		switch name {
		case "--dir":
			dir = value
		case "--host":
			host = value
		case "--port":
			port = value
		case "--user":
			user = value
		default:
			return "", nil, "", "", "", "", fmt.Errorf("unknown bridge flag %s", name)
		}
		i++
	}
	if i >= len(args) {
		return "", nil, "", "", "", "", fmt.Errorf("usage: bd-store-bridge --dir <dir> --host <host> --port <port> <op> [args...]")
	}
	return args[i], args[i+1:], dir, host, port, user, nil
}

func bdStoreBridgePassword() string {
	password := strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD"))
	if password == "" {
		password = strings.TrimSpace(os.Getenv("BEADS_DOLT_PASSWORD"))
	}
	return password
}

func runBdStoreBridge(op string, args []string, dir, host, port, user string, stdin io.Reader, stdout io.Writer) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("missing --dir")
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("missing --host")
	}
	if strings.TrimSpace(port) == "" {
		return fmt.Errorf("missing --port")
	}
	password := bdStoreBridgePassword()
	store := beads.NewBdStore(dir, beads.ExecCommandRunnerWithEnv(bdStoreBridgeEnv(dir, host, port, user, password)))
	switch op {
	case "create":
		var req bdStoreBridgeCreateRequest
		if err := decodeJSON(stdin, &req); err != nil {
			return err
		}
		created, err := store.Create(beads.Bead{
			Title:       req.Title,
			Type:        req.Type,
			Priority:    req.Priority,
			Labels:      req.Labels,
			ParentID:    req.ParentID,
			Ref:         req.Ref,
			Needs:       req.Needs,
			Description: req.Description,
			Assignee:    req.Assignee,
			From:        req.From,
			Metadata:    req.Metadata,
		})
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBead(created))
	case "get":
		if len(args) < 1 {
			return fmt.Errorf("usage: get <id>")
		}
		bead, err := store.Get(args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBead(bead))
	case "update":
		if len(args) < 1 {
			return fmt.Errorf("usage: update <id>")
		}
		var req bdStoreBridgeUpdateRequest
		if err := decodeJSON(stdin, &req); err != nil {
			return err
		}
		return store.Update(args[0], beads.UpdateOpts{
			Title:        req.Title,
			Status:       req.Status,
			Type:         req.Type,
			Priority:     req.Priority,
			Description:  req.Description,
			ParentID:     req.ParentID,
			Assignee:     req.Assignee,
			Labels:       req.Labels,
			RemoveLabels: req.RemoveLabels,
			Metadata:     req.Metadata,
		})
	case "close":
		if len(args) < 1 {
			return fmt.Errorf("usage: close <id>")
		}
		return store.Close(args[0])
	case "reopen":
		if len(args) < 1 {
			return fmt.Errorf("usage: reopen <id>")
		}
		return store.Reopen(args[0])
	case "list":
		query := beads.ListQuery{AllowScan: true}
		for _, arg := range args {
			switch {
			case strings.HasPrefix(arg, "--status="):
				query.Status = strings.TrimPrefix(arg, "--status=")
			case strings.HasPrefix(arg, "--assignee="):
				query.Assignee = strings.TrimPrefix(arg, "--assignee=")
			case strings.HasPrefix(arg, "--type="):
				query.Type = strings.TrimPrefix(arg, "--type=")
			case strings.HasPrefix(arg, "--limit="):
				parsed, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
				if err != nil {
					return fmt.Errorf("parse limit %q: %w", arg, err)
				}
				query.Limit = parsed
			}
		}
		items, err := store.List(query)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBeads(items))
	case "ready":
		items, err := beads.ReadyLive(store)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBeads(items))
	case "children":
		if len(args) < 1 {
			return fmt.Errorf("usage: children <parent-id>")
		}
		items, err := store.Children(args[0], beads.IncludeClosed)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBeads(items))
	case "list-by-label":
		if len(args) < 1 {
			return fmt.Errorf("usage: list-by-label <label> [limit]")
		}
		limit := 0
		if len(args) > 1 && strings.TrimSpace(args[1]) != "" {
			parsed, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("parse limit %q: %w", args[1], err)
			}
			limit = parsed
		}
		items, err := store.ListByLabel(args[0], limit, beads.IncludeClosed)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBeads(items))
	case "set-metadata":
		if len(args) < 2 {
			return fmt.Errorf("usage: set-metadata <id> <key>")
		}
		value, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		return store.SetMetadata(args[0], args[1], string(value))
	case "delete":
		if len(args) < 1 {
			return fmt.Errorf("usage: delete <id>")
		}
		return store.Delete(args[len(args)-1])
	case "dep-add":
		if len(args) < 3 {
			return fmt.Errorf("usage: dep-add <issue-id> <depends-on-id> <type>")
		}
		return store.DepAdd(args[0], args[1], args[2])
	case "dep-remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: dep-remove <issue-id> <depends-on-id>")
		}
		return store.DepRemove(args[0], args[1])
	case "dep-list":
		if len(args) < 1 {
			return fmt.Errorf("usage: dep-list <id> [direction]")
		}
		direction := ""
		if len(args) > 1 {
			direction = args[1]
		}
		deps, err := store.DepList(args[0], direction)
		if err != nil {
			return err
		}
		return writeJSON(stdout, deps)
	default:
		return fmt.Errorf("unsupported operation %q", op)
	}
}

func bdStoreBridgeEnv(dir, host, port, user, password string) map[string]string {
	env := map[string]string{}
	for _, key := range []string{
		"BEADS_DIR",
		"BEADS_CREDENTIALS_FILE",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_DATABASE",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_DATABASE",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BD_EXPORT_AUTO",
		"GC_BEADS",
		"GC_BEADS_PREFIX",
		"GC_DOLT_DATABASE",
		"GC_DOLT_HOST",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
	} {
		env[key] = ""
	}
	env["BEADS_DIR"] = dir + "/.beads"
	env["GC_DOLT_HOST"] = host
	env["BEADS_DOLT_SERVER_HOST"] = host
	env["GC_DOLT_PORT"] = port
	env["BEADS_DOLT_SERVER_PORT"] = port
	env["GC_DOLT_USER"] = user
	env["BEADS_DOLT_SERVER_USER"] = user
	env["GC_DOLT_PASSWORD"] = password
	env["BEADS_DOLT_PASSWORD"] = password
	env["BEADS_DOLT_AUTO_START"] = "0"
	env["BD_EXPORT_AUTO"] = "false"
	return env
}

func decodeJSON(r io.Reader, dest any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	payload := strings.TrimSpace(string(data))
	if payload == "" {
		payload = "{}"
	}
	if err := json.Unmarshal([]byte(payload), dest); err != nil {
		return fmt.Errorf("parse stdin JSON: %w", err)
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(data)); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

func bridgeBeads(items []beads.Bead) []bdStoreBridgeBead {
	out := make([]bdStoreBridgeBead, 0, len(items))
	for _, item := range items {
		out = append(out, bridgeBead(item))
	}
	return out
}

func bridgeBead(item beads.Bead) bdStoreBridgeBead {
	return bdStoreBridgeBead{
		ID:          item.ID,
		Title:       item.Title,
		Status:      item.Status,
		Type:        item.Type,
		Priority:    item.Priority,
		CreatedAt:   item.CreatedAt,
		Assignee:    item.Assignee,
		From:        item.From,
		ParentID:    item.ParentID,
		Ref:         item.Ref,
		Needs:       item.Needs,
		Description: item.Description,
		Labels:      item.Labels,
		Metadata:    item.Metadata,
	}
}
