//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/pb33f/libopenapi"
	openapivalidator "github.com/pb33f/libopenapi-validator"
)

// TestGCLiveContract_BeadsAndEvents covers real-world app API usage directly
// in this repo. It boots a real supervisor with isolated
// state, creates an isolated city and rig through the HTTP API, validates
// responses against the live OpenAPI document, exercises real Dolt-backed
// beads, mail, events, and subprocess agent sessions, and unregisters the city
// through the API.
func TestGCLiveContract_BeadsAndEvents(t *testing.T) {
	bin := buildGCBinary(t)

	root := shortTempDir(t)
	gcHome := filepath.Join(root, "home")
	runtimeDir := filepath.Join(root, "run")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := seedDoltIdentityForRoot(gcHome); err != nil {
		t.Fatalf("seed dolt identity: %v", err)
	}
	port := reserveFreePort(t)
	writeSupervisorConfig(t, gcHome, port)

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	env := append(integrationEnvFor(gcHome, runtimeDir, true), "GC_SESSION=subprocess")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, "supervisor", "run")
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	var supervisorLog strings.Builder
	go func() { _, _ = io.Copy(&supervisorLog, stderr) }()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("supervisor stderr:\n%s", supervisorLog.String())
		}
	})

	waitHTTP(t, baseURL+"/health", 10*time.Second)

	specBytes := liveContractRequest(t, baseURL, nil, http.MethodGet, "/openapi.json", nil, http.StatusOK)
	assertLiveContractSpec(t, specBytes)
	assertLiveContractRequiredOperations(t, specBytes)
	validator := liveContractValidator(t, specBytes)

	cityName := "real-world-app-contract-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cityDir := filepath.Join(root, "cities", cityName)
	createCity := liveContractJSON[struct {
		RequestID   string `json:"request_id"`
		EventCursor string `json:"event_cursor"`
	}](t, baseURL, validator, http.MethodPost, "/v0/city", map[string]string{
		"dir":           cityDir,
		"start_command": "bash " + agentScript("stuck-agent.sh"),
	}, http.StatusAccepted)
	if createCity.RequestID == "" {
		t.Fatalf("city create response missing request_id")
	}
	if createCity.EventCursor == "" {
		t.Fatalf("city create response missing event_cursor")
	}

	cityBase := "/v0/city/" + url.PathEscape(cityName)
	waitForLiveContractRequestID[struct {
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
		Path      string `json:"path"`
	}](t, baseURL, validator, "/v0/events", createCity.RequestID, "request.result.city.create", 120*time.Second, createCity.EventCursor)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/health", nil, http.StatusOK)
	// Use replay cursors so the open check verifies the SSE route without
	// waiting for a fresh event or the 15s idle heartbeat.
	assertLiveContractStreamOpens(t, baseURL, "/v0/events/stream?after_cursor=0")
	assertLiveContractStreamOpens(t, baseURL, cityBase+"/events/stream?after_seq=0")

	cityScopedBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "City-scoped fixture created immediately after async city.create completion.",
		"labels":      []string{"real-world-app-contract", "city-scope"},
		"title":       "real-world app contract city-scoped bead",
		"type":        "task",
	}, http.StatusCreated)
	if cityScopedBead.ID == "" || cityScopedBead.Status != "open" {
		t.Fatalf("city-scoped bead = %+v, want id and open status", cityScopedBead)
	}

	rigName := "alpha"
	rigDir := filepath.Join(cityDir, rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
		Rig    string `json:"rig"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/rigs", map[string]string{
		"name":   rigName,
		"path":   rigDir,
		"prefix": "rw" + strconv.FormatInt(time.Now().UnixNano(), 36),
	}, http.StatusCreated)
	waitForLiveContractRig(t, baseURL, validator, cityBase, rigName, rigDir, 30*time.Second)

	liveContractJSON[struct {
		Status   string `json:"status"`
		Provider string `json:"provider"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/providers", map[string]any{
		"name":        "contract-agent",
		"command":     "bash",
		"args":        []string{agentScript("stuck-agent.sh")},
		"prompt_mode": "none",
	}, http.StatusCreated)
	liveContractJSON[struct {
		Status string `json:"status"`
		Agent  string `json:"agent"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/agents", map[string]string{
		"name":     "worker",
		"dir":      rigName,
		"provider": "contract-agent",
	}, http.StatusCreated)
	targetAgent := rigName + "/worker"
	waitForLiveContractAgent(t, baseURL, validator, cityBase, targetAgent, 30*time.Second)

	publicProviders := liveContractJSON[struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/providers/public", nil, http.StatusOK)
	if len(publicProviders.Items) == 0 {
		t.Fatal("GET providers/public returned no providers")
	}
	liveContractJSON[map[string]any](t, baseURL, validator, http.MethodGet, cityBase+"/readiness?fresh=false", nil, http.StatusOK)
	liveContractJSON[map[string]any](t, baseURL, validator, http.MethodGet, cityBase+"/provider-readiness?fresh=false", nil, http.StatusOK)
	cfg := liveContractJSON[struct {
		Agents []contractConfigAgent `json:"agents"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/config", nil, http.StatusOK)
	if !liveContractConfigHasAgent(cfg.Agents, "worker", rigName) {
		t.Fatalf("GET config missing created agent %q; agents=%+v", targetAgent, cfg.Agents)
	}

	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	sessionID := createLiveContractAgentSession(t, baseURL, validator, cityBase, targetAgent, rigName, "mail-"+runID)
	rootBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Root fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"real-world-app-contract", "root"},
		"metadata": map[string]string{
			"real_world_app.contract.role":   "root",
			"real_world_app.contract.run_id": runID,
		},
		"priority": 2,
		"rig":      rigName,
		"title":    "real-world app contract root " + runID,
		"type":     "feature",
	}, http.StatusCreated)
	if rootBead.ID == "" || rootBead.Status != "open" || rootBead.Type != "feature" {
		t.Fatalf("root bead = %+v, want id, open status, feature type", rootBead)
	}
	if rootBead.Metadata["real_world_app.contract.run_id"] != runID {
		t.Fatalf("root metadata = %#v, want run_id=%q", rootBead.Metadata, runID)
	}
	idempotentKey := "real-world-app-contract-idempotent-" + runID
	idempotentBody := map[string]any{
		"description": "Idempotency fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"real-world-app-contract", "idempotency"},
		"rig":         rigName,
		"title":       "real-world app contract idempotent " + runID,
		"type":        "task",
	}
	firstReplay := liveContractRequestWithHeaders(t, baseURL, validator, http.MethodPost, cityBase+"/beads", idempotentBody, http.StatusCreated, map[string]string{
		"Idempotency-Key": idempotentKey,
	})
	secondReplay := liveContractRequestWithHeaders(t, baseURL, validator, http.MethodPost, cityBase+"/beads", idempotentBody, http.StatusCreated, map[string]string{
		"Idempotency-Key": idempotentKey,
	})
	if !bytes.Equal(firstReplay, secondReplay) {
		t.Fatalf("idempotent mutation replay body changed:\nfirst:  %s\nsecond: %s", string(firstReplay), string(secondReplay))
	}
	var idempotentBead beads.Bead
	if err := json.Unmarshal(firstReplay, &idempotentBead); err != nil {
		t.Fatalf("decode idempotent bead: %v", err)
	}

	waitForLiveContractAgent(t, baseURL, validator, cityBase, targetAgent, 30*time.Second)
	liveContractJSON[struct {
		Status string `json:"status"`
		Target string `json:"target"`
		Bead   string `json:"bead"`
		Mode   string `json:"mode"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/sling", map[string]any{
		"rig":    rigName,
		"target": targetAgent,
		"bead":   rootBead.ID,
		"force":  true,
	}, http.StatusOK)
	formulaName := "real-world-app-contract-work"
	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formula dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, formulaName+".toml"), []byte(`
formula = "real-world-app-contract-work"
version = 1
description = "Live contract preview fixture."

[vars]
[vars.issue]
description = "Work bead ID"
required = true

[[steps]]
id = "do-work"
title = "Do {{issue}}"
description = "Read and complete {{issue}}."
`), 0o644); err != nil {
		t.Fatalf("write formula fixture: %v", err)
	}
	liveContractJSON[struct {
		Name    string `json:"name"`
		Preview struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"preview"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/formulas/"+url.PathEscape(formulaName)+"/preview", map[string]any{
		"scope_kind": "city",
		"scope_ref":  cityName,
		"target":     targetAgent,
		"vars":       map[string]string{"issue": rootBead.ID},
	}, http.StatusOK)
	exerciseLiveContractFormulasAndWorkflows(t, baseURL, validator, cityBase, formulaName, targetAgent, rigName, rootBead.ID, runID)
	exerciseLiveContractOrders(t, baseURL, validator, cityBase, rigName, rootBead.ID, runID)

	lifecycleBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Lifecycle fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"real-world-app-contract", "lifecycle"},
		"metadata": map[string]string{
			"real_world_app.contract.role":   "lifecycle",
			"real_world_app.contract.run_id": runID,
		},
		"rig":   rigName,
		"title": "real-world app contract lifecycle " + runID,
		"type":  "task",
	}, http.StatusCreated)
	if lifecycleBead.ID == "" {
		t.Fatal("lifecycle bead missing id")
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(lifecycleBead.ID)+"/update", map[string]any{
		"metadata": map[string]string{
			"real_world_app.contract.metadata_update": "true",
			"real_world_app.permission_mode":          "default",
			"real_world_app.starred":                  "true",
		},
		"status": "in_progress",
	}, http.StatusOK)
	updatedLifecycle := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(lifecycleBead.ID), nil, http.StatusOK)
	if updatedLifecycle.Status != "in_progress" || updatedLifecycle.Metadata["real_world_app.contract.metadata_update"] != "true" {
		t.Fatalf("updated lifecycle bead = %+v, want in_progress plus metadata update", updatedLifecycle)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(lifecycleBead.ID)+"/close", nil, http.StatusOK)
	closedLifecycle := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(lifecycleBead.ID), nil, http.StatusOK)
	if closedLifecycle.Status != "closed" {
		t.Fatalf("closed lifecycle bead status = %q, want closed", closedLifecycle.Status)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(lifecycleBead.ID)+"/reopen", nil, http.StatusOK)
	reopenedLifecycle := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(lifecycleBead.ID), nil, http.StatusOK)
	if reopenedLifecycle.Status != "open" {
		t.Fatalf("reopened lifecycle bead status = %q, want open", reopenedLifecycle.Status)
	}

	childBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Child fixture that exercises parent and update semantics",
		"labels":      []string{"real-world-app-contract", "child", "needs-update"},
		"metadata": map[string]string{
			"real_world_app.contract.role":   "child",
			"real_world_app.contract.run_id": runID,
		},
		"parent":   rootBead.ID,
		"priority": 1,
		"rig":      rigName,
		"title":    "real-world app contract child " + runID,
		"type":     "task",
	}, http.StatusCreated)
	siblingBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Sibling fixture for list and filter coverage",
		"labels":      []string{"real-world-app-contract", "sibling"},
		"metadata": map[string]string{
			"real_world_app.contract.role":   "sibling",
			"real_world_app.contract.run_id": runID,
		},
		"parent":   rootBead.ID,
		"priority": 3,
		"rig":      rigName,
		"title":    "real-world app contract sibling " + runID,
		"type":     "bug",
	}, http.StatusCreated)
	if childBead.ParentID != rootBead.ID || childBead.Type != "task" {
		t.Fatalf("child bead = %+v, want parent=%q type=task", childBead, rootBead.ID)
	}
	if siblingBead.ParentID != rootBead.ID || siblingBead.Type != "bug" {
		t.Fatalf("sibling bead = %+v, want parent=%q type=bug", siblingBead, rootBead.ID)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(childBead.ID)+"/update", map[string]any{
		"description":   "Updated child fixture",
		"labels":        []string{"verified"},
		"metadata":      map[string]string{"real_world_app.contract.updated": "true"},
		"parent":        "",
		"priority":      4,
		"remove_labels": []string{"needs-update"},
		"status":        "in_progress",
		"title":         "real-world app contract child updated " + runID,
		"type":          "bug",
	}, http.StatusOK)
	updatedChild := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(childBead.ID), nil, http.StatusOK)
	if updatedChild.ParentID != "" || updatedChild.Status != "in_progress" || updatedChild.Type != "bug" || updatedChild.Priority == nil || *updatedChild.Priority != 4 {
		t.Fatalf("updated child = %+v, want cleared parent, in_progress, bug, priority 4", updatedChild)
	}
	if !containsString(updatedChild.Labels, "verified") || containsString(updatedChild.Labels, "needs-update") {
		t.Fatalf("updated child labels = %#v, want verified without needs-update", updatedChild.Labels)
	}
	if updatedChild.Metadata["real_world_app.contract.updated"] != "true" {
		t.Fatalf("updated child metadata = %#v, want real_world_app.contract.updated=true", updatedChild.Metadata)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(childBead.ID)+"/update", map[string]any{
		"metadata": map[string]string{"real_world_app.contract.parent_restored": "true"},
		"parent":   rootBead.ID,
	}, http.StatusOK)
	restoredChild := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(childBead.ID), nil, http.StatusOK)
	if restoredChild.ParentID != rootBead.ID {
		t.Fatalf("restored child parent = %q, want %q", restoredChild.ParentID, rootBead.ID)
	}
	deps := liveContractJSON[struct {
		Children []beads.Bead `json:"children"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(rootBead.ID)+"/deps", nil, http.StatusOK)
	if !beadListContains(deps.Children, childBead.ID) {
		t.Fatalf("deps children = %#v, want child %s", deps.Children, childBead.ID)
	}
	graph := liveContractJSON[struct {
		Beads []beads.Bead       `json:"beads"`
		Deps  []contractGraphDep `json:"deps"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/beads/graph/"+url.PathEscape(rootBead.ID), nil, http.StatusOK)
	if !beadListContains(graph.Beads, rootBead.ID) {
		t.Fatalf("graph beads = %#v, want root %s", graph.Beads, rootBead.ID)
	}
	if !beadListContains(graph.Beads, childBead.ID) {
		t.Fatalf("graph beads = %#v, want child %s", graph.Beads, childBead.ID)
	}
	if !beadListContains(graph.Beads, siblingBead.ID) {
		t.Fatalf("graph beads = %#v, want sibling %s", graph.Beads, siblingBead.ID)
	}
	for _, childID := range []string{childBead.ID, siblingBead.ID} {
		if !liveContractGraphHasEdge(graph.Deps, rootBead.ID, childID, "parent-child") {
			t.Fatalf("graph deps = %#v, want parent-child edge %s -> %s", graph.Deps, rootBead.ID, childID)
		}
	}
	list := liveContractJSON[struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/beads?label=real-world-app-contract&limit=50&rig="+url.QueryEscape(rigName), nil, http.StatusOK)
	if list.Total < 3 || !beadListContains(list.Items, rootBead.ID) || !beadListContains(list.Items, siblingBead.ID) {
		t.Fatalf("filtered beads = %+v, want root and sibling", list)
	}
	if listedSibling, ok := findLiveContractBead(list.Items, siblingBead.ID); ok && listedSibling.ParentID != rootBead.ID {
		t.Fatalf("filtered sibling parent = %q, want %q", listedSibling.ParentID, rootBead.ID)
	}

	message := liveContractJSON[struct {
		ID       string `json:"id"`
		ThreadID string `json:"thread_id"`
		Rig      string `json:"rig"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/mail", map[string]string{
		"rig":     rigName,
		"from":    "real-world-app-contract",
		"to":      sessionID,
		"subject": "real-world app contract mail " + runID,
		"body":    "Exercise the typed mail API contract.",
	}, http.StatusCreated)
	if message.ID == "" {
		t.Fatalf("mail send response missing id: %+v", message)
	}
	mailPath := cityBase + "/mail/" + url.PathEscape(message.ID)
	mailRigQuery := "?rig=" + url.QueryEscape(rigName)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, mailPath+"/read"+mailRigQuery, nil, http.StatusOK)
	readMessage := liveContractJSON[struct {
		ID   string `json:"id"`
		Read bool   `json:"read"`
	}](t, baseURL, validator, http.MethodGet, mailPath+"?rig="+url.QueryEscape(rigName), nil, http.StatusOK)
	if readMessage.ID != message.ID || !readMessage.Read {
		t.Fatalf("mail read state after read = %+v, want id=%q read=true", readMessage, message.ID)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, mailPath+"/mark-unread"+mailRigQuery, nil, http.StatusOK)
	unreadMessage := liveContractJSON[struct {
		ID   string `json:"id"`
		Read bool   `json:"read"`
	}](t, baseURL, validator, http.MethodGet, mailPath+"?rig="+url.QueryEscape(rigName), nil, http.StatusOK)
	if unreadMessage.ID != message.ID || unreadMessage.Read {
		t.Fatalf("mail read state after mark-unread = %+v, want id=%q read=false", unreadMessage, message.ID)
	}
	reply := liveContractJSON[struct {
		ID string `json:"id"`
	}](t, baseURL, validator, http.MethodPost, mailPath+"/reply"+mailRigQuery, map[string]string{
		"from":    targetAgent,
		"subject": "Re: real-world app contract mail " + runID,
		"body":    "Reply from live contract coverage.",
	}, http.StatusCreated)
	if reply.ID == "" {
		t.Fatalf("mail reply response missing id: %+v", reply)
	}
	if message.ThreadID == "" {
		t.Fatalf("mail send response missing thread_id: %+v", message)
	}
	liveContractJSON[struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Total int `json:"total"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/mail/thread/"+url.PathEscape(message.ThreadID)+"?rig="+url.QueryEscape(rigName), nil, http.StatusOK)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, mailPath+"/archive"+mailRigQuery, nil, http.StatusOK)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodDelete, mailPath+mailRigQuery, nil, http.StatusOK)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/session/"+url.PathEscape(sessionID)+"/close?delete=true", nil, http.StatusOK)

	exerciseLiveContractSessionLifecycle(t, baseURL, validator, cityBase, targetAgent, rigName, runID)

	events, err := liveContractEventList(baseURL, validator, "/v0/events?limit=50")
	if err != nil {
		t.Fatalf("GET /v0/events?limit=50: %v", err)
	}
	if events.Total == 0 {
		t.Fatal("GET /v0/events?limit=50 returned no events")
	}
	cityEvents, err := liveContractEventList(baseURL, validator, cityBase+"/events?limit=50")
	if err != nil {
		t.Fatalf("GET %s/events?limit=50: %v", cityBase, err)
	}
	if cityEvents.Total == 0 {
		t.Fatalf("GET %s/events?limit=50 returned no events", cityBase)
	}
	runLiveContractReadSweep(t, baseURL, validator, specBytes, cityName, rigName)

	for _, id := range []string{idempotentBead.ID, lifecycleBead.ID, siblingBead.ID, childBead.ID, rootBead.ID} {
		liveContractJSON[struct {
			Status string `json:"status"`
		}](t, baseURL, validator, http.MethodDelete, cityBase+"/bead/"+url.PathEscape(id), nil, http.StatusOK)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPatch, cityBase, map[string]bool{"suspended": true}, http.StatusOK)
	closeLiveContractRigSessions(t, baseURL, validator, cityBase, rigName)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPatch, cityBase, map[string]bool{"suspended": false}, http.StatusOK)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodDelete, cityBase+"/rig/"+url.PathEscape(rigName), nil, http.StatusOK)

	unregister := liveContractJSON[struct {
		RequestID   string `json:"request_id"`
		EventCursor string `json:"event_cursor"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/unregister", nil, http.StatusAccepted)
	if unregister.RequestID == "" {
		t.Fatalf("unregister response missing request_id")
	}
	if unregister.EventCursor == "" {
		t.Fatalf("unregister response missing event_cursor")
	}
	waitForLiveContractRequestID[struct {
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
		Path      string `json:"path"`
	}](t, baseURL, validator, "/v0/events", unregister.RequestID, "request.result.city.unregister", 120*time.Second, unregister.EventCursor)
	waitForCityAbsent(t, baseURL, validator, cityName, 45*time.Second)
}

type contractEventList struct {
	Items []contractEvent `json:"items"`
	Total int             `json:"total"`
}

type contractEvent struct {
	Type    string          `json:"type"`
	Subject string          `json:"subject"`
	City    string          `json:"city"`
	Payload json.RawMessage `json:"payload"`
}

type liveContractRequiredOperation struct {
	Method       string
	OperationID  string
	PathTemplate string
}

var liveContractRequiredOperations = []liveContractRequiredOperation{
	{Method: http.MethodGet, OperationID: "get-v0-cities", PathTemplate: "/v0/cities"},
	{Method: http.MethodGet, OperationID: "get-health", PathTemplate: "/health"},
	{Method: http.MethodPost, OperationID: "post-v0-city", PathTemplate: "/v0/city"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-unregister", PathTemplate: "/v0/city/{cityName}/unregister"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-agents", PathTemplate: "/v0/city/{cityName}/agents"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-config", PathTemplate: "/v0/city/{cityName}/config"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-health", PathTemplate: "/v0/city/{cityName}/health"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-providers", PathTemplate: "/v0/city/{cityName}/providers"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-providers-public", PathTemplate: "/v0/city/{cityName}/providers/public"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-readiness", PathTemplate: "/v0/city/{cityName}/readiness"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-rigs", PathTemplate: "/v0/city/{cityName}/rigs"},
	{Method: http.MethodPost, OperationID: "create-rig", PathTemplate: "/v0/city/{cityName}/rigs"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-sessions", PathTemplate: "/v0/city/{cityName}/sessions"},
	{Method: http.MethodPost, OperationID: "create-session", PathTemplate: "/v0/city/{cityName}/sessions"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-session-by-id", PathTemplate: "/v0/city/{cityName}/session/{id}"},
	{Method: http.MethodPatch, OperationID: "patch-v0-city-by-city-name-session-by-id", PathTemplate: "/v0/city/{cityName}/session/{id}"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-session-by-id-agents", PathTemplate: "/v0/city/{cityName}/session/{id}/agents"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-session-by-id-agents-by-agent-id", PathTemplate: "/v0/city/{cityName}/session/{id}/agents/{agentId}"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-session-by-id-close", PathTemplate: "/v0/city/{cityName}/session/{id}/close"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-session-by-id-kill", PathTemplate: "/v0/city/{cityName}/session/{id}/kill"},
	{Method: http.MethodPost, OperationID: "send-session-message", PathTemplate: "/v0/city/{cityName}/session/{id}/messages"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-session-by-id-pending", PathTemplate: "/v0/city/{cityName}/session/{id}/pending"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-session-by-id-rename", PathTemplate: "/v0/city/{cityName}/session/{id}/rename"},
	{Method: http.MethodPost, OperationID: "respond-session", PathTemplate: "/v0/city/{cityName}/session/{id}/respond"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-session-by-id-stop", PathTemplate: "/v0/city/{cityName}/session/{id}/stop"},
	{Method: http.MethodGet, OperationID: "stream-session", PathTemplate: "/v0/city/{cityName}/session/{id}/stream"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-session-by-id-suspend", PathTemplate: "/v0/city/{cityName}/session/{id}/suspend"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-session-by-id-transcript", PathTemplate: "/v0/city/{cityName}/session/{id}/transcript"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-session-by-id-wake", PathTemplate: "/v0/city/{cityName}/session/{id}/wake"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-beads", PathTemplate: "/v0/city/{cityName}/beads"},
	{Method: http.MethodPost, OperationID: "create-bead", PathTemplate: "/v0/city/{cityName}/beads"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-bead-by-id", PathTemplate: "/v0/city/{cityName}/bead/{id}"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-bead-by-id-close", PathTemplate: "/v0/city/{cityName}/bead/{id}/close"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-bead-by-id-deps", PathTemplate: "/v0/city/{cityName}/bead/{id}/deps"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-bead-by-id-reopen", PathTemplate: "/v0/city/{cityName}/bead/{id}/reopen"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-bead-by-id-update", PathTemplate: "/v0/city/{cityName}/bead/{id}/update"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-beads-graph-by-root-id", PathTemplate: "/v0/city/{cityName}/beads/graph/{rootID}"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-mail", PathTemplate: "/v0/city/{cityName}/mail"},
	{Method: http.MethodPost, OperationID: "send-mail", PathTemplate: "/v0/city/{cityName}/mail"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-mail-by-id", PathTemplate: "/v0/city/{cityName}/mail/{id}"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-mail-by-id-archive", PathTemplate: "/v0/city/{cityName}/mail/{id}/archive"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-mail-by-id-mark-unread", PathTemplate: "/v0/city/{cityName}/mail/{id}/mark-unread"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-mail-by-id-read", PathTemplate: "/v0/city/{cityName}/mail/{id}/read"},
	{Method: http.MethodPost, OperationID: "reply-mail", PathTemplate: "/v0/city/{cityName}/mail/{id}/reply"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-mail-thread-by-id", PathTemplate: "/v0/city/{cityName}/mail/thread/{id}"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-formulas", PathTemplate: "/v0/city/{cityName}/formulas"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-formulas-feed", PathTemplate: "/v0/city/{cityName}/formulas/feed"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-formulas-by-name", PathTemplate: "/v0/city/{cityName}/formulas/{name}"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-formulas-by-name-preview", PathTemplate: "/v0/city/{cityName}/formulas/{name}/preview"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-formulas-by-name-runs", PathTemplate: "/v0/city/{cityName}/formulas/{name}/runs"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-orders", PathTemplate: "/v0/city/{cityName}/orders"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-orders-check", PathTemplate: "/v0/city/{cityName}/orders/check"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-orders-feed", PathTemplate: "/v0/city/{cityName}/orders/feed"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-orders-history", PathTemplate: "/v0/city/{cityName}/orders/history"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-order-history-by-bead-id", PathTemplate: "/v0/city/{cityName}/order/history/{bead_id}"},
	{Method: http.MethodPost, OperationID: "post-v0-city-by-city-name-sling", PathTemplate: "/v0/city/{cityName}/sling"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-convoy-by-id", PathTemplate: "/v0/city/{cityName}/convoy/{id}"},
	{Method: http.MethodDelete, OperationID: "delete-v0-city-by-city-name-convoy-by-id", PathTemplate: "/v0/city/{cityName}/convoy/{id}"},
	{Method: http.MethodGet, OperationID: "get-v0-city-by-city-name-workflow-by-workflow-id", PathTemplate: "/v0/city/{cityName}/workflow/{workflow_id}"},
	{Method: http.MethodGet, OperationID: "get-v0-events", PathTemplate: "/v0/events"},
	{Method: http.MethodGet, OperationID: "stream-supervisor-events", PathTemplate: "/v0/events/stream"},
}

type liveContractReadProbe struct {
	pathTemplate string
	path         string
	skipReason   string
}

type contractRigList struct {
	Items []contractRig `json:"items"`
	Total int           `json:"total"`
}

type contractRig struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type contractGraphDep struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type contractConfigAgent struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

func liveContractConfigHasAgent(agents []contractConfigAgent, name, dir string) bool {
	for _, agent := range agents {
		if agent.Name == name && agent.Dir == dir {
			return true
		}
	}
	return false
}

func createLiveContractAgentSession(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, targetAgent, rigName, label string) string {
	t.Helper()
	create := liveContractJSON[struct {
		RequestID   string `json:"request_id"`
		EventCursor string `json:"event_cursor"`
		Status      string `json:"status"`
	}](t, baseURL, v, http.MethodPost, cityBase+"/sessions", map[string]any{
		"alias":      "rw-" + label,
		"async":      true,
		"kind":       "agent",
		"name":       targetAgent,
		"project_id": rigName,
		"title":      "real-world app contract " + label,
	}, http.StatusAccepted)
	if create.RequestID == "" {
		t.Fatalf("%s session create response missing request_id", label)
	}
	if create.EventCursor == "" {
		t.Fatalf("%s session create response missing event_cursor", label)
	}
	result := waitForLiveContractRequestID[struct {
		RequestID string `json:"request_id"`
		Session   struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Alias string `json:"alias"`
			Rig   string `json:"rig"`
			State string `json:"state"`
		} `json:"session"`
	}](t, baseURL, v, cityBase+"/events", create.RequestID, "request.result.session.create", 120*time.Second, create.EventCursor)
	if result.Session.ID == "" {
		t.Fatalf("%s session create result missing session.id", label)
	}
	if result.Session.State == "creating" {
		t.Fatalf("%s session create result state = %q, want commandable state", label, result.Session.State)
	}
	if result.Session.Title != "real-world app contract "+label {
		t.Fatalf("%s session title = %q", label, result.Session.Title)
	}
	if result.Session.Rig != rigName {
		t.Fatalf("%s session rig = %q, want %q", label, result.Session.Rig, rigName)
	}
	if result.Session.Alias == "" {
		t.Fatalf("%s session missing controller-managed alias", label)
	}
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/session/"+url.PathEscape(result.Session.ID)+"/pending", nil, http.StatusOK)
	return result.Session.ID
}

func closeLiveContractRigSessions(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, rigName string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		sessions := liveContractJSON[struct {
			Items []struct {
				ID       string `json:"id"`
				Rig      string `json:"rig"`
				Template string `json:"template"`
				State    string `json:"state"`
			} `json:"items"`
		}](t, baseURL, v, http.MethodGet, cityBase+"/sessions?limit=100", nil, http.StatusOK)

		remaining := 0
		for _, sess := range sessions.Items {
			if sess.ID == "" || sess.State == "closed" {
				continue
			}
			if sess.Rig != rigName && !strings.HasPrefix(sess.Template, rigName+"/") {
				continue
			}
			remaining++
			liveContractJSON[struct {
				Status string `json:"status"`
			}](t, baseURL, v, http.MethodPost, cityBase+"/session/"+url.PathEscape(sess.ID)+"/close?delete=true", nil, http.StatusOK)
		}
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out closing %d live contract rig session(s)", remaining)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func exerciseLiveContractSessionLifecycle(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, targetAgent, rigName, runID string) {
	t.Helper()
	id := createLiveContractAgentSession(t, baseURL, v, cityBase, targetAgent, rigName, "lifecycle-"+runID)
	sessionPath := cityBase + "/session/" + url.PathEscape(id)

	detail := liveContractJSON[struct {
		ID string `json:"id"`
	}](t, baseURL, v, http.MethodGet, sessionPath, nil, http.StatusOK)
	if detail.ID != id {
		t.Fatalf("session detail id = %q, want %q", detail.ID, id)
	}

	patchedTitle := "real-world app contract patched " + runID
	patched := liveContractJSON[struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Alias string `json:"alias"`
	}](t, baseURL, v, http.MethodPatch, sessionPath, map[string]string{"title": patchedTitle}, http.StatusOK)
	if patched.ID != id || patched.Title != patchedTitle || patched.Alias == "" {
		t.Fatalf("patched session = %+v, want id=%q title=%q with alias", patched, id, patchedTitle)
	}

	renamedTitle := "real-world app contract renamed " + runID
	renamed := liveContractJSON[struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}](t, baseURL, v, http.MethodPost, sessionPath+"/rename", map[string]string{"title": renamedTitle}, http.StatusOK)
	if renamed.ID != id || renamed.Title != renamedTitle {
		t.Fatalf("renamed session = %+v, want id=%q title=%q", renamed, id, renamedTitle)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, v, http.MethodPost, sessionPath+"/suspend", nil, http.StatusOK)
	waitForLiveContractSessionState(t, baseURL, v, sessionPath, "suspended", 30*time.Second)

	wake := liveContractJSON[struct {
		ID string `json:"id"`
	}](t, baseURL, v, http.MethodPost, sessionPath+"/wake", nil, http.StatusOK)
	if wake.ID != id {
		t.Fatalf("wake id = %q, want %q", wake.ID, id)
	}

	msg := liveContractJSON[struct {
		RequestID   string `json:"request_id"`
		EventCursor string `json:"event_cursor"`
	}](t, baseURL, v, http.MethodPost, sessionPath+"/messages", map[string]string{
		"message": "real-world app contract message " + runID,
	}, http.StatusAccepted)
	if msg.RequestID == "" || msg.RequestID == id {
		t.Fatalf("message response = %+v, want request_id distinct from session id %q", msg, id)
	}
	if msg.EventCursor == "" {
		t.Fatalf("message response missing event_cursor")
	}
	waitForLiveContractRequestID[struct {
		RequestID string `json:"request_id"`
	}](t, baseURL, v, cityBase+"/events", msg.RequestID, "request.result.session.message", 120*time.Second, msg.EventCursor)

	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, sessionPath+"/pending", nil, http.StatusOK)
	liveContractRequestOneOf(t, baseURL, v, http.MethodPost, sessionPath+"/respond", map[string]string{
		"action": "deny",
		"text":   "real-world app contract no-pending response " + runID,
	}, []int{http.StatusConflict, http.StatusNotImplemented})
	transcript := liveContractJSON[struct {
		ID     string `json:"id"`
		Format string `json:"format"`
	}](t, baseURL, v, http.MethodGet, sessionPath+"/transcript?format=raw&tail=1", nil, http.StatusOK)
	if transcript.ID != id || transcript.Format != "raw" {
		t.Fatalf("raw transcript = %+v, want id=%q format=raw", transcript, id)
	}
	assertLiveContractStreamOpens(t, baseURL, sessionPath+"/stream?format=raw")

	agents := liveContractJSON[struct {
		Agents []struct {
			AgentID string `json:"agent_id"`
			ID      string `json:"id"`
		} `json:"agents"`
	}](t, baseURL, v, http.MethodGet, sessionPath+"/agents", nil, http.StatusOK)
	for _, agent := range agents.Agents {
		agentID := agent.AgentID
		if agentID == "" {
			agentID = agent.ID
		}
		if agentID != "" {
			liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, sessionPath+"/agents/"+url.PathEscape(agentID), nil, http.StatusOK)
			break
		}
	}

	stopped := liveContractJSON[struct {
		ID string `json:"id"`
	}](t, baseURL, v, http.MethodPost, sessionPath+"/stop", nil, http.StatusOK)
	if stopped.ID != id {
		t.Fatalf("stop id = %q, want %q", stopped.ID, id)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, v, http.MethodPost, sessionPath+"/close?delete=true", nil, http.StatusOK)

	killID := createLiveContractAgentSession(t, baseURL, v, cityBase, targetAgent, rigName, "kill-"+runID)
	killed := liveContractJSON[struct {
		ID string `json:"id"`
	}](t, baseURL, v, http.MethodPost, cityBase+"/session/"+url.PathEscape(killID)+"/kill", nil, http.StatusOK)
	if killed.ID != killID {
		t.Fatalf("kill id = %q, want %q", killed.ID, killID)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, v, http.MethodPost, cityBase+"/session/"+url.PathEscape(killID)+"/close?delete=true", nil, http.StatusOK)
}

func waitForLiveContractSessionState(t *testing.T, baseURL string, v openapivalidator.Validator, sessionPath, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		session := liveContractJSON[struct {
			State string `json:"state"`
		}](t, baseURL, v, http.MethodGet, sessionPath, nil, http.StatusOK)
		if session.State == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s state at %s", want, sessionPath)
}

func waitForLiveContractSessionCommandable(t *testing.T, baseURL string, v openapivalidator.Validator, sessionPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastState string
	for time.Now().Before(deadline) {
		session := liveContractJSON[struct {
			State string `json:"state"`
		}](t, baseURL, v, http.MethodGet, sessionPath, nil, http.StatusOK)
		lastState = session.State
		switch session.State {
		case "creating":
			time.Sleep(250 * time.Millisecond)
			continue
		case "crashed", "closed":
			t.Fatalf("session at %s reached state %q before command lifecycle checks", sessionPath, session.State)
		default:
			if session.State != "" {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session at %s to leave creating state; last state=%q", sessionPath, lastState)
}

func exerciseLiveContractFormulasAndWorkflows(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, formulaName, targetAgent, rigName, rootBeadID, runID string) {
	t.Helper()
	formulas := liveContractJSON[struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
		Total int `json:"total"`
	}](t, baseURL, v, http.MethodGet, cityBase+"/formulas?scope_kind=rig&scope_ref="+url.QueryEscape(rigName), nil, http.StatusOK)
	if formulas.Total < len(formulas.Items) || formulas.Total == 0 || !formulaListContains(formulas.Items, formulaName) {
		t.Fatalf("formula list = %+v, want %q", formulas, formulaName)
	}
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/formulas/feed?scope_kind=rig&scope_ref="+url.QueryEscape(rigName), nil, http.StatusOK)
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/formulas/"+url.PathEscape(formulaName)+"?scope_kind=rig&scope_ref="+url.QueryEscape(rigName)+"&target="+url.QueryEscape(targetAgent), nil, http.StatusOK)
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodPost, cityBase+"/formulas/"+url.PathEscape(formulaName)+"/preview", map[string]any{
		"scope_kind": "rig",
		"scope_ref":  rigName,
		"target":     targetAgent,
		"vars":       map[string]string{"issue": rootBeadID},
	}, http.StatusOK)
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/formulas/"+url.PathEscape(formulaName)+"/runs?limit=5&scope_kind=rig&scope_ref="+url.QueryEscape(rigName), nil, http.StatusOK)
	liveContractRequestOneOf(t, baseURL, v, http.MethodPost, cityBase+"/sling", map[string]any{
		"target": targetAgent,
	}, []int{http.StatusOK, http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusNotFound})
	workflow := liveContractJSON[beads.Bead](t, baseURL, v, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Workflow fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"real-world-app-contract", "workflow"},
		"metadata": map[string]string{
			"gc.kind":             "workflow",
			"gc.formula":          formulaName,
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "real-world-app-workflow-" + runID,
		},
		"rig":   rigName,
		"title": "real-world app contract workflow " + runID,
		"type":  "convoy",
	}, http.StatusCreated)
	workflowID := workflow.ID
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/workflow/"+url.PathEscape(workflowID)+"?scope_kind=rig&scope_ref="+url.QueryEscape(rigName), nil, http.StatusOK)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, v, http.MethodDelete, cityBase+"/workflow/"+url.PathEscape(workflowID)+"?scope_kind=rig&scope_ref="+url.QueryEscape(rigName), nil, http.StatusOK)

	convoyItem := liveContractJSON[beads.Bead](t, baseURL, v, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Disposable convoy item fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"real-world-app-contract", "convoy-item"},
		"rig":         rigName,
		"title":       "real-world app contract convoy item " + runID,
		"type":        "task",
	}, http.StatusCreated)
	convoy := liveContractJSON[beads.Bead](t, baseURL, v, http.MethodPost, cityBase+"/convoys", map[string]any{
		"rig":   rigName,
		"title": "real-world app contract convoy " + runID,
		"items": []string{convoyItem.ID},
	}, http.StatusCreated)
	if convoy.ID == "" {
		t.Fatalf("convoy create response missing id: %+v", convoy)
	}
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/convoy/"+url.PathEscape(convoy.ID), nil, http.StatusOK)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, v, http.MethodDelete, cityBase+"/convoy/"+url.PathEscape(convoy.ID), nil, http.StatusOK)
}

func exerciseLiveContractOrders(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, rigName, rootBeadID, runID string) {
	t.Helper()
	scopedName := "real-world-app-contract-" + runID + ":rig:" + rigName
	orderRun := liveContractJSON[beads.Bead](t, baseURL, v, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Order history fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"order-run:" + scopedName, "real-world-app-contract"},
		"metadata": map[string]string{
			"convergence.gate_stdout": "hello from live contract",
		},
		"rig":   rigName,
		"title": "real-world app contract order history " + runID,
		"type":  "task",
	}, http.StatusCreated)
	defer liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, v, http.MethodDelete, cityBase+"/bead/"+url.PathEscape(orderRun.ID), nil, http.StatusOK)

	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/orders", nil, http.StatusOK)
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/orders/check", nil, http.StatusOK)
	liveContractJSON[map[string]any](t, baseURL, v, http.MethodGet, cityBase+"/orders/feed?scope_kind=rig&scope_ref="+url.QueryEscape(rigName)+"&limit=25", nil, http.StatusOK)
	history := liveContractJSON[struct {
		Entries []struct {
			BeadID    string `json:"bead_id"`
			StoreRef  string `json:"store_ref"`
			HasOutput bool   `json:"has_output"`
		} `json:"entries"`
	}](t, baseURL, v, http.MethodGet, cityBase+"/orders/history?scoped_name="+url.QueryEscape(scopedName)+"&limit=20", nil, http.StatusOK)
	if len(history.Entries) == 0 || history.Entries[0].BeadID != orderRun.ID {
		t.Fatalf("order history = %+v, want entry for %q", history, orderRun.ID)
	}
	detailPath := cityBase + "/order/history/" + url.PathEscape(history.Entries[0].BeadID)
	if history.Entries[0].StoreRef != "" {
		detailPath += "?store_ref=" + url.QueryEscape(history.Entries[0].StoreRef)
	}
	detail := liveContractJSON[struct {
		BeadID string `json:"bead_id"`
		Output string `json:"output"`
	}](t, baseURL, v, http.MethodGet, detailPath, nil, http.StatusOK)
	if detail.BeadID != orderRun.ID || !strings.Contains(detail.Output, "hello from live contract") {
		t.Fatalf("order history detail = %+v, want output for %q", detail, orderRun.ID)
	}
}

func formulaListContains(items []struct {
	Name string `json:"name"`
}, want string,
) bool {
	for _, item := range items {
		if item.Name == want {
			return true
		}
	}
	return false
}

func liveContractValidator(t *testing.T, specBytes []byte) openapivalidator.Validator {
	t.Helper()
	doc, err := libopenapi.NewDocument(specBytes)
	if err != nil {
		t.Fatalf("build OpenAPI document: %v", err)
	}
	v, errs := openapivalidator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatalf("construct OpenAPI validator: %v", errs)
	}
	return v
}

func liveContractJSON[T any](t *testing.T, baseURL string, v openapivalidator.Validator, method, path string, body any, wantStatus int) T {
	t.Helper()
	raw := liveContractRequest(t, baseURL, v, method, path, body, wantStatus)
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s decode response: %v\nbody: %s", method, path, err, string(raw))
	}
	return out
}

func liveContractRequest(t *testing.T, baseURL string, v openapivalidator.Validator, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	return liveContractRequestWithHeaders(t, baseURL, v, method, path, body, wantStatus, nil)
}

func liveContractRequestWithHeaders(t *testing.T, baseURL string, v openapivalidator.Validator, method, path string, body any, wantStatus int, headers map[string]string) []byte {
	t.Helper()
	req, err := liveContractHTTPRequest(baseURL, method, path, body)
	if err != nil {
		t.Fatalf("%s %s build request: %v", method, path, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read response: %v", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, resp.StatusCode, wantStatus, string(raw))
	}
	if v != nil {
		validateLiveContractResponse(t, v, req, resp, raw)
	}
	return raw
}

func liveContractRequestOneOf(t *testing.T, baseURL string, v openapivalidator.Validator, method, path string, body any, wantStatuses []int) []byte {
	t.Helper()
	req, err := liveContractHTTPRequest(baseURL, method, path, body)
	if err != nil {
		t.Fatalf("%s %s build request: %v", method, path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read response: %v", method, path, err)
	}
	if !intListContains(wantStatuses, resp.StatusCode) {
		t.Fatalf("%s %s status = %d, want one of %v; body: %s", method, path, resp.StatusCode, wantStatuses, string(raw))
	}
	if v != nil {
		validateLiveContractResponse(t, v, req, resp, raw)
	}
	return raw
}

func intListContains(items []int, want int) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func liveContractHTTPRequest(baseURL, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-GC-Request", "live-contract")
	}
	return req, nil
}

func assertLiveContractStreamOpens(t *testing.T, baseURL, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastStatus int
	var lastBody string
	var lastContentType string
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			cancel()
			t.Fatalf("build stream request %s: %v", path, err)
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		lastErr = nil
		lastStatus = resp.StatusCode
		lastContentType = resp.Header.Get("Content-Type")
		if resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			cancel()
			if !strings.Contains(lastContentType, "text/event-stream") {
				t.Fatalf("GET %s stream content-type = %q, want text/event-stream", path, lastContentType)
			}
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		lastBody = string(raw)
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET %s stream: %v", path, lastErr)
	}
	t.Fatalf("GET %s stream status = %d, want 200; body: %s", path, lastStatus, lastBody)
}

func validateLiveContractResponse(t *testing.T, v openapivalidator.Validator, req *http.Request, resp *http.Response, raw []byte) {
	t.Helper()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	ok, valErrs := v.ValidateHttpResponse(req, resp)
	_ = resp.Body.Close()
	if ok {
		return
	}
	var details strings.Builder
	for _, ve := range valErrs {
		fmt.Fprintf(&details, "%s - %s\n", ve.Message, ve.Reason)
		for _, se := range ve.SchemaValidationErrors {
			fmt.Fprintf(&details, "  %s at %s\n", se.Reason, se.FieldPath)
		}
	}
	t.Fatalf("%s %s response does not match OpenAPI schema:\n%sbody: %s", req.Method, req.URL.Path, details.String(), string(raw))
}

func waitForLiveContractRequestID[T any](t *testing.T, baseURL string, v openapivalidator.Validator, path, requestID, successType string, timeout time.Duration, eventCursor string) T {
	t.Helper()
	env := waitForLiveContractRequestEvent(t, baseURL, path, requestID, successType, timeout, eventCursor)
	var payload T
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("%s payload for request_id=%s did not decode: %v\npayload: %s", successType, requestID, err, string(env.Payload))
	}
	return payload
}

func waitForLiveContractRequestEvent(t *testing.T, baseURL, path, requestID, successType string, timeout time.Duration, eventCursor string) contractEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cursor := strings.TrimSpace(eventCursor)
	streamPath := path
	if strings.HasSuffix(streamPath, "/events") && streamPath != "/v0/events" {
		streamPath += "/stream"
		if cursor != "" {
			streamPath += "?after_seq=" + url.QueryEscape(cursor)
		} else {
			streamPath += "?after_seq=0"
		}
	} else {
		streamPath = strings.TrimSuffix(streamPath, "/") + "/stream"
		if cursor != "" {
			streamPath += "?after_cursor=" + url.QueryEscape(cursor)
		} else {
			streamPath += "?after_cursor=0"
		}
	}
	if path == "/v0/events" {
		streamPath = "/v0/events/stream"
		if cursor != "" {
			streamPath += "?after_cursor=" + url.QueryEscape(cursor)
		} else {
			streamPath += "?after_cursor=0"
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+streamPath, nil)
	if err != nil {
		t.Fatalf("build SSE request %s: %v", streamPath, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-GC-Request", "live-contract")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s SSE: %v", streamPath, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("GET %s SSE status = %d, want 200; body: %s", streamPath, resp.StatusCode, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	observed := 0
	var recent []string
	remember := func(event contractEvent) {
		observed++
		req := liveContractEventPayloadRequestID(event.Payload)
		desc := event.Type
		if event.City != "" {
			desc += " city=" + event.City
		}
		if event.Subject != "" {
			desc += " subject=" + event.Subject
		}
		if req != "" {
			desc += " request_id=" + req
		}
		recent = append(recent, desc)
		if len(recent) > 8 {
			recent = recent[1:]
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		case line == "":
			if data.Len() == 0 {
				continue
			}
			var event contractEvent
			if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
				t.Fatalf("decode SSE event from %s: %v; data=%s", streamPath, err, data.String())
			}
			data.Reset()
			remember(event)
			if event.Type == successType && liveContractEventPayloadRequestID(event.Payload) == requestID {
				return event
			}
			if event.Type == "request.failed" && liveContractEventPayloadRequestID(event.Payload) == requestID {
				var payload struct {
					ErrorCode    string `json:"error_code"`
					ErrorMessage string `json:"error_message"`
				}
				_ = json.Unmarshal(event.Payload, &payload)
				t.Fatalf("request.failed for request_id=%s: %s: %s", requestID, payload.ErrorCode, payload.ErrorMessage)
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("scan SSE %s: %v", streamPath, err)
	}
	t.Fatalf("timed out waiting for %s for request_id=%s from %s after observing %d SSE data frames; recent=%v", successType, requestID, streamPath, observed, recent)
	return contractEvent{}
}

func liveContractEventPayloadRequestID(raw json.RawMessage) string {
	var payload struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.RequestID
}

func waitForLiveContractEvent(t *testing.T, baseURL string, v openapivalidator.Validator, path, subject, eventType string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		events, err := liveContractEventList(baseURL, v, path)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		for _, event := range events.Items {
			if event.Subject == subject && event.Type == eventType {
				return
			}
			if event.Subject == subject && strings.HasSuffix(event.Type, "_failed") {
				t.Fatalf("event %s for %s failed: %s", event.Type, subject, string(event.Payload))
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s for %s from %s; last error: %v", eventType, subject, path, lastErr)
}

func liveContractEventList(baseURL string, v openapivalidator.Validator, path string) (contractEventList, error) {
	req, err := liveContractHTTPRequest(baseURL, http.MethodGet, path, nil)
	if err != nil {
		return contractEventList{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return contractEventList{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return contractEventList{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return contractEventList{}, fmt.Errorf("GET %s status %d: %s", path, resp.StatusCode, string(raw))
	}
	if v != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		ok, valErrs := v.ValidateHttpResponse(req, resp)
		_ = resp.Body.Close()
		if !ok {
			return contractEventList{}, fmt.Errorf("GET %s response does not match OpenAPI schema: %v; body: %s", path, valErrs, string(raw))
		}
	}
	var events contractEventList
	if err := json.Unmarshal(raw, &events); err != nil {
		return contractEventList{}, err
	}
	return events, nil
}

func waitForCityAbsent(t *testing.T, baseURL string, v openapivalidator.Validator, cityName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cities := liveContractJSON[struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			Total int `json:"total"`
		}](t, baseURL, v, http.MethodGet, "/v0/cities", nil, http.StatusOK)
		found := false
		for _, city := range cities.Items {
			if city.Name == cityName {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for city %q to disappear from /v0/cities", cityName)
}

func waitForLiveContractRig(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, rigName, rigDir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		rigs, err := liveContractRigList(baseURL, v, cityBase)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		for _, rig := range rigs.Items {
			if rig.Name == rigName && rig.Path == rigDir {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for rig %q at %q; last error: %v", rigName, rigDir, lastErr)
}

func liveContractRigList(baseURL string, v openapivalidator.Validator, cityBase string) (contractRigList, error) {
	req, err := liveContractHTTPRequest(baseURL, http.MethodGet, cityBase+"/rigs", nil)
	if err != nil {
		return contractRigList{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return contractRigList{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return contractRigList{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return contractRigList{}, fmt.Errorf("GET %s/rigs status %d: %s", cityBase, resp.StatusCode, string(raw))
	}
	if v != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		ok, valErrs := v.ValidateHttpResponse(req, resp)
		_ = resp.Body.Close()
		if !ok {
			return contractRigList{}, fmt.Errorf("GET %s/rigs response does not match OpenAPI schema: %v", cityBase, valErrs)
		}
	}
	var rigs contractRigList
	if err := json.Unmarshal(raw, &rigs); err != nil {
		return rigs, err
	}
	return rigs, nil
}

func waitForLiveContractAgent(t *testing.T, baseURL string, v openapivalidator.Validator, cityBase, targetAgent string, timeout time.Duration) {
	t.Helper()
	dir, base, ok := strings.Cut(targetAgent, "/")
	if !ok || dir == "" || base == "" {
		t.Fatalf("target agent %q is not a qualified rig agent", targetAgent)
	}
	path := cityBase + "/agent/" + url.PathEscape(dir) + "/" + url.PathEscape(base)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := liveContractHTTPRequest(baseURL, http.MethodGet, path, nil)
		if err != nil {
			t.Fatalf("GET %s build request: %v", path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if v != nil {
				resp.Body = io.NopCloser(bytes.NewReader(raw))
				validateLiveContractResponse(t, v, req, resp, raw)
				_ = resp.Body.Close()
			}
			return
		}
		lastErr = fmt.Errorf("GET %s status %d: %s", path, resp.StatusCode, string(raw))
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for agent %q at %s; last error: %v", targetAgent, path, lastErr)
}

func runLiveContractReadSweep(t *testing.T, baseURL string, v openapivalidator.Validator, specBytes []byte, cityName, rigName string) {
	t.Helper()
	probes := collectLiveContractReadProbes(t, specBytes, cityName, rigName)
	if len(probes) == 0 {
		t.Fatal("OpenAPI read sweep found no GET probes")
	}
	for _, probe := range probes {
		t.Run("GET "+probe.pathTemplate, func(t *testing.T) {
			if probe.skipReason != "" {
				t.Skip(probe.skipReason)
			}
			liveContractRequest(t, baseURL, v, http.MethodGet, probe.path, nil, http.StatusOK)
		})
	}
}

func collectLiveContractReadProbes(t *testing.T, specBytes []byte, cityName, rigName string) []liveContractReadProbe {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("decode OpenAPI paths: %v", err)
	}

	probes := make([]liveContractReadProbe, 0, len(spec.Paths))
	for pathTemplate, pathItem := range spec.Paths {
		if _, ok := pathItem["get"]; !ok {
			continue
		}
		if strings.Contains(pathTemplate, "/stream") || hasLiveContractUnboundPathParams(pathTemplate) {
			continue
		}
		path := strings.ReplaceAll(pathTemplate, "{cityName}", url.PathEscape(cityName))
		path = appendLiveContractDefaultQuery(path, pathTemplate, rigName)
		probes = append(probes, liveContractReadProbe{
			pathTemplate: pathTemplate,
			path:         path,
			skipReason:   liveContractProbeSkipReason(pathTemplate),
		})
	}
	return probes
}

func hasLiveContractUnboundPathParams(pathTemplate string) bool {
	for _, part := range strings.Split(pathTemplate, "/") {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") && part != "{cityName}" {
			return true
		}
	}
	return false
}

func appendLiveContractDefaultQuery(path, pathTemplate, rigName string) string {
	query := url.Values{}
	switch {
	case strings.HasSuffix(pathTemplate, "/formulas") || strings.HasSuffix(pathTemplate, "/formulas/feed"):
		query.Set("scope_kind", "rig")
		query.Set("scope_ref", rigName)
	case strings.HasSuffix(pathTemplate, "/orders/feed"):
		query.Set("limit", "25")
		query.Set("scope_kind", "rig")
		query.Set("scope_ref", rigName)
	case strings.HasSuffix(pathTemplate, "/readiness") || strings.HasSuffix(pathTemplate, "/provider-readiness"):
		query.Set("fresh", "false")
	case strings.HasSuffix(pathTemplate, "/beads"):
		query.Set("limit", "50")
	case strings.HasSuffix(pathTemplate, "/mail"):
		query.Set("limit", "50")
	}
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

func liveContractProbeSkipReason(pathTemplate string) string {
	switch {
	case strings.HasSuffix(pathTemplate, "/extmsg/bindings"),
		strings.HasSuffix(pathTemplate, "/extmsg/groups"),
		strings.HasSuffix(pathTemplate, "/extmsg/transcript"):
		return "requires a real session/conversation identity"
	case strings.HasSuffix(pathTemplate, "/orders/history"):
		return "requires a scoped order name fixture"
	default:
		return ""
	}
}

func assertLiveContractSpec(t *testing.T, specBytes []byte) {
	t.Helper()
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				OneOf      []map[string]string `json:"oneOf"`
				Properties map[string]any      `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}
	for _, field := range []string{"metadata", "parent"} {
		if _, ok := spec.Components.Schemas["BeadCreateInputBody"].Properties[field]; !ok {
			t.Fatalf("BeadCreateInputBody missing %q property", field)
		}
	}
	if _, ok := spec.Components.Schemas["BeadUpdateBody"].Properties["parent"]; !ok {
		t.Fatal("BeadUpdateBody missing parent property")
	}

	var cityPayloadRefs []string
	for _, branch := range spec.Components.Schemas["EventPayload"].OneOf {
		ref := branch["$ref"]
		if strings.Contains(ref, "City") {
			cityPayloadRefs = append(cityPayloadRefs, ref)
		}
	}
	for _, want := range []string{"CityLifecyclePayload", "CityCreateSucceededPayload", "CityUnregisterSucceededPayload"} {
		if !refListContainsSchema(cityPayloadRefs, want) {
			t.Fatalf("EventPayload city branches = %#v, missing %s", cityPayloadRefs, want)
		}
	}
}

func assertLiveContractRequiredOperations(t *testing.T, specBytes []byte) {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("decode OpenAPI paths: %v", err)
	}

	type operationShape struct {
		Method       string
		PathTemplate string
	}
	operations := make(map[string]operationShape)
	for pathTemplate, pathItem := range spec.Paths {
		for method, operation := range pathItem {
			if operation.OperationID == "" {
				continue
			}
			operations[operation.OperationID] = operationShape{
				Method:       strings.ToUpper(method),
				PathTemplate: pathTemplate,
			}
		}
	}

	for _, required := range liveContractRequiredOperations {
		found, ok := operations[required.OperationID]
		if !ok {
			t.Fatalf("OpenAPI missing required real-world app GC operation %s (%s %s)", required.OperationID, required.Method, required.PathTemplate)
		}
		if found.Method != required.Method || found.PathTemplate != required.PathTemplate {
			t.Fatalf("OpenAPI operation %s = %s %s, want %s %s", required.OperationID, found.Method, found.PathTemplate, required.Method, required.PathTemplate)
		}
	}
}

func refListContainsSchema(refs []string, schema string) bool {
	for _, ref := range refs {
		if strings.HasSuffix(ref, "/"+schema) {
			return true
		}
	}
	return false
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func beadListContains(items []beads.Bead, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func findLiveContractBead(items []beads.Bead, id string) (beads.Bead, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return beads.Bead{}, false
}

func liveContractGraphHasEdge(items []contractGraphDep, from, to, kind string) bool {
	for _, item := range items {
		if item.From == from && item.To == to && item.Kind == kind {
			return true
		}
	}
	return false
}
