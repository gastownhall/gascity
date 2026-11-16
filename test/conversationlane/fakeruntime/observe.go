package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// This file is fakeruntime's observe.v1 profile.

// identityKeys are the identity env keys observe.v1 reports.
var identityKeys = []string{"GC_SESSION_ID", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN", "GT_PROCESS_NAMES"}

// boxObject is a box's object: a sequence number and its creation time.
type boxObject struct {
	ID      string `json:"id"`
	Created string `json:"created"`
}

// startConfig is the part of a start config the profile reads.
type startConfig struct {
	Command      string            `json:"command"`
	Env          map[string]string `json:"env"`
	ProcessNames []string          `json:"process_names"`
	FreshOnly    bool              `json:"fresh_only"`
}

// doObserve carries out an op the observe.v1 profile serves; ok is false
// for every other op.
func (s state) doObserve(op, name string, stdin io.Reader) (out string, code int, ok bool, err error) {
	switch op {
	case "protocol":
		return `{"version":0,"capabilities":["observe.v1","report-attachment"],"locality":"local"}` + "\n", 0, true, nil
	case "start":
		code, err := s.startFresh(name, stdin)
		return "", code, true, err
	case "observe":
		out, err := s.observeBox(name, stdin)
		return out, 0, true, err
	case "inventory":
		out, err := s.inventory()
		return out, 0, true, err
	case "kill-object":
		out, err := s.killObject(name, stdin)
		return out, 0, true, err
	case "is-attached":
		return "false\n", 0, true, nil
	case "peek":
		// A box has an empty screen; no box is exit 66, no screen.
		if !exists(s.path("box", name)) {
			return "", exitNoScreen, true, nil
		}
		return "", 0, true, nil
	}
	return "", 0, false, nil
}

// exitNoScreen is `peek`'s "no screen" (runtime.ExitCodeNoScreen).
const exitNoScreen = 66

// startFresh is start under the profile: a held name is exit 73 under
// fresh_only; the identity keys of the start env are seeded as metadata
// before the box exists.
func (s state) startFresh(name string, stdin io.Reader) (int, error) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return 0, err
	}
	var cfg startConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0, fmt.Errorf("start config: %w", err)
	}
	if exists(s.path("box", name)) {
		if cfg.FreshOnly {
			return exitFreshCollision, fmt.Errorf("session %s already exists", name)
		}
		return 0, fmt.Errorf("session %s already exists", name)
	}
	for _, key := range identityKeys {
		if v := cfg.Env[key]; v != "" {
			if err := os.WriteFile(s.path("meta", name, key), []byte(v), 0o644); err != nil {
				return 0, err
			}
		}
	}
	if _, set := cfg.Env["GT_PROCESS_NAMES"]; !set && len(cfg.ProcessNames) > 0 {
		if err := os.WriteFile(s.path("meta", name, "GT_PROCESS_NAMES"), []byte(strings.Join(cfg.ProcessNames, ",")), 0o644); err != nil {
			return 0, err
		}
	}
	if err := os.WriteFile(s.path("config", name, "json"), raw, 0o644); err != nil {
		return 0, err
	}
	seq, err := s.nextSeq()
	if err != nil {
		return 0, err
	}
	obj, err := json.Marshal(boxObject{ID: fmt.Sprintf("box-%d", seq), Created: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return 0, err
	}
	return 0, os.WriteFile(s.path("box", name), obj, 0o644)
}

// nextSeq returns the next box sequence number.
func (s state) nextSeq() (int, error) {
	seq := 0
	err := s.appendLocked("seq", nil, func(lines int) any {
		seq = lines + 1
		return seq
	})
	return seq, err
}

// box reads name's object; ok is false when no box holds the name.
func (s state) box(name string) (obj boxObject, ok bool, err error) {
	data, err := os.ReadFile(s.path("box", name))
	if errors.Is(err, os.ErrNotExist) {
		return obj, false, nil
	}
	if err != nil {
		return obj, false, err
	}
	if len(data) == 0 {
		// A box a start without the profile made: no object to report.
		return boxObject{ID: "box-legacy-" + name, Created: "0"}, true, nil
	}
	return obj, true, json.Unmarshal(data, &obj)
}

// identity is name's identity env, from its metadata.
func (s state) identity(name string) map[string]string {
	id := map[string]string{}
	for _, key := range identityKeys {
		if v, err := os.ReadFile(s.path("meta", name, key)); err == nil {
			id[key] = string(v)
		}
	}
	return id
}

// agentAlive reports whether any of names is name's start command.
func (s state) agentAlive(name string, names []string) bool {
	raw, err := os.ReadFile(s.path("config", name, "json"))
	if err != nil {
		return false
	}
	var cfg startConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	fields := strings.Fields(cfg.Command)
	return len(fields) > 0 && slices.Contains(names, fields[0])
}

// observeEntry is an observe answer, and an inventory entry.
type observeEntry struct {
	Name     string            `json:"name,omitempty"`
	State    string            `json:"state"`
	Object   *boxObject        `json:"object,omitempty"`
	Agent    string            `json:"agent,omitempty"`
	Attached *bool             `json:"attached,omitempty"`
	Identity map[string]string `json:"identity"`
}

// observeBox answers observe for name, the process names on stdin.
func (s state) observeBox(name string, stdin io.Reader) (string, error) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	obj, ok, err := s.box(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return `{"state":"absent"}` + "\n", nil
	}
	e := observeEntry{State: "running", Object: &obj, Identity: s.identity(name)}
	if names := strings.Fields(string(raw)); len(names) > 0 {
		e.Agent = "dead"
		if s.agentAlive(name, names) {
			e.Agent = "alive"
		}
	}
	return jsonLine(e)
}

// inventory lists every box with its attributes: always complete.
func (s state) inventory() (string, error) {
	boxes, err := filepath.Glob(s.path("box", "*"))
	if err != nil {
		return "", err
	}
	detached := false
	inv := struct {
		Complete bool           `json:"complete"`
		Sessions []observeEntry `json:"sessions"`
	}{Complete: true, Sessions: []observeEntry{}}
	for _, p := range boxes {
		name := strings.TrimPrefix(filepath.Base(p), "box.")
		obj, ok, err := s.box(name)
		if err != nil || !ok {
			continue
		}
		e := observeEntry{Name: name, State: "running", Object: &obj, Attached: &detached, Identity: s.identity(name)}
		if names := strings.Split(s.identity(name)["GT_PROCESS_NAMES"], ","); names[0] != "" {
			e.Agent = "dead"
			if s.agentAlive(name, names) {
				e.Agent = "alive"
			}
		}
		inv.Sessions = append(inv.Sessions, e)
	}
	return jsonLine(inv)
}

// killObject kills name's box only while it is the object on stdin: a
// corpse kill finds the box live (fakeruntime keeps no corpses); a zombie
// kill takes a box whose agent is dead, and finds any other changed.
func (s state) killObject(name string, stdin io.Reader) (string, error) {
	var req struct {
		Object boxObject `json:"object"`
		Mode   string    `json:"mode"`
	}
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return "", fmt.Errorf("kill-object request: %w", err)
	}
	obj, ok, err := s.box(name)
	if err != nil {
		return "", err
	}
	if !ok || obj != req.Object {
		return "gone\n", nil
	}
	switch req.Mode {
	case "corpse":
		return "live\n", nil
	case "zombie":
		if s.agentAlive(name, strings.Split(s.identity(name)["GT_PROCESS_NAMES"], ",")) {
			return "changed\n", nil
		}
		return "killed\n", s.stop(name)
	}
	return "", fmt.Errorf("kill-object: unknown mode %q", req.Mode)
}

func jsonLine(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}
