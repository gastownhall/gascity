//go:build integration

package integration

import (
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

// TestGCLiveContract_BeadsAndEvents ports the MC live GC contract's
// API-only coverage into this repo. It boots a real supervisor, creates an
// isolated city and rig through the HTTP API, validates responses against the
// live OpenAPI document, exercises the bead lifecycle MC depends on, validates
// city and supervisor event list schemas, and unregisters the city through the
// API.
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
	env := integrationEnvFor(gcHome, runtimeDir, true)

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
	validator := liveContractValidator(t, specBytes)

	cityName := "mc-live-contract-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cityDir := filepath.Join(root, "cities", cityName)
	createCity := liveContractJSON[struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
		Path string `json:"path"`
	}](t, baseURL, validator, http.MethodPost, "/v0/city", map[string]string{
		"dir":      cityDir,
		"provider": "claude",
	}, http.StatusAccepted)
	if !createCity.OK || createCity.Name != cityName || createCity.Path != cityDir {
		t.Fatalf("city create response = %+v, want ok=true name=%q path=%q", createCity, cityName, cityDir)
	}

	cityBase := "/v0/city/" + url.PathEscape(cityName)
	waitForLiveContractRequestResult(t, baseURL, validator, "/v0/events", cityName, "city.create", "succeeded", 120*time.Second)
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodGet, cityBase+"/health", nil, http.StatusOK)
	assertLiveContractStreamOpens(t, baseURL, "/v0/events/stream")
	assertLiveContractStreamOpens(t, baseURL, cityBase+"/events/stream")

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
		"prefix": "mc-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	}, http.StatusCreated)
	waitForLiveContractRig(t, baseURL, validator, cityBase, rigName, rigDir, 30*time.Second)

	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	rootBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Root fixture created by TestGCLiveContract_BeadsAndEvents",
		"labels":      []string{"mc-live-contract", "root"},
		"metadata": map[string]string{
			"mc.contract.role":   "root",
			"mc.contract.run_id": runID,
		},
		"priority": 2,
		"rig":      rigName,
		"title":    "MC live contract root " + runID,
		"type":     "feature",
	}, http.StatusCreated)
	if rootBead.ID == "" || rootBead.Status != "open" || rootBead.Type != "feature" {
		t.Fatalf("root bead = %+v, want id, open status, feature type", rootBead)
	}
	if rootBead.Metadata["mc.contract.run_id"] != runID {
		t.Fatalf("root metadata = %#v, want run_id=%q", rootBead.Metadata, runID)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(rootBead.ID)+"/update", map[string]any{
		"metadata": map[string]string{
			"mc.contract.metadata_update": "true",
			"mc_permission_mode":          "default",
			"mc_starred":                  "true",
		},
		"status": "in_progress",
	}, http.StatusOK)
	updatedRoot := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(rootBead.ID), nil, http.StatusOK)
	if updatedRoot.Status != "in_progress" || updatedRoot.Metadata["mc.contract.metadata_update"] != "true" {
		t.Fatalf("updated root = %+v, want in_progress plus metadata update", updatedRoot)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(rootBead.ID)+"/close", nil, http.StatusOK)
	closedRoot := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(rootBead.ID), nil, http.StatusOK)
	if closedRoot.Status != "closed" {
		t.Fatalf("closed root status = %q, want closed", closedRoot.Status)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(rootBead.ID)+"/reopen", nil, http.StatusOK)
	reopenedRoot := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(rootBead.ID), nil, http.StatusOK)
	if reopenedRoot.Status != "open" {
		t.Fatalf("reopened root status = %q, want open", reopenedRoot.Status)
	}

	childBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Child fixture that exercises parent and update semantics",
		"labels":      []string{"mc-live-contract", "child", "needs-update"},
		"metadata": map[string]string{
			"mc.contract.role":   "child",
			"mc.contract.run_id": runID,
		},
		"parent":   rootBead.ID,
		"priority": 1,
		"rig":      rigName,
		"title":    "MC live contract child " + runID,
		"type":     "task",
	}, http.StatusCreated)
	siblingBead := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodPost, cityBase+"/beads", map[string]any{
		"description": "Sibling fixture for list and filter coverage",
		"labels":      []string{"mc-live-contract", "sibling"},
		"metadata": map[string]string{
			"mc.contract.role":   "sibling",
			"mc.contract.run_id": runID,
		},
		"parent":   rootBead.ID,
		"priority": 3,
		"rig":      rigName,
		"title":    "MC live contract sibling " + runID,
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
		"metadata":      map[string]string{"mc.contract.updated": "true"},
		"parent":        "",
		"priority":      4,
		"remove_labels": []string{"needs-update"},
		"status":        "in_progress",
		"title":         "MC live contract child updated " + runID,
		"type":          "bug",
	}, http.StatusOK)
	updatedChild := liveContractJSON[beads.Bead](t, baseURL, validator, http.MethodGet, cityBase+"/bead/"+url.PathEscape(childBead.ID), nil, http.StatusOK)
	if updatedChild.ParentID != "" || updatedChild.Status != "in_progress" || updatedChild.Type != "bug" || updatedChild.Priority == nil || *updatedChild.Priority != 4 {
		t.Fatalf("updated child = %+v, want cleared parent, in_progress, bug, priority 4", updatedChild)
	}
	if !containsString(updatedChild.Labels, "verified") || containsString(updatedChild.Labels, "needs-update") {
		t.Fatalf("updated child labels = %#v, want verified without needs-update", updatedChild.Labels)
	}
	if updatedChild.Metadata["mc.contract.updated"] != "true" {
		t.Fatalf("updated child metadata = %#v, want mc.contract.updated=true", updatedChild.Metadata)
	}

	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/bead/"+url.PathEscape(childBead.ID)+"/update", map[string]any{
		"metadata": map[string]string{"mc.contract.parent_restored": "true"},
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
	}](t, baseURL, validator, http.MethodGet, cityBase+"/beads?label=mc-live-contract&limit=50&rig="+url.QueryEscape(rigName), nil, http.StatusOK)
	if list.Total < 3 || !beadListContains(list.Items, rootBead.ID) || !beadListContains(list.Items, siblingBead.ID) {
		t.Fatalf("filtered beads = %+v, want root and sibling", list)
	}
	if listedSibling, ok := findLiveContractBead(list.Items, siblingBead.ID); ok && listedSibling.ParentID != rootBead.ID {
		t.Fatalf("filtered sibling parent = %q, want %q", listedSibling.ParentID, rootBead.ID)
	}

	waitForLiveContractRequestResult(t, baseURL, validator, cityBase+"/events", cityName, "city.create", "succeeded", 10*time.Second)
	liveContractJSON[contractEventList](t, baseURL, validator, http.MethodGet, "/v0/events?limit=50", nil, http.StatusOK)
	runLiveContractReadSweep(t, baseURL, validator, specBytes, cityName, rigName)

	for _, id := range []string{siblingBead.ID, childBead.ID, rootBead.ID} {
		liveContractJSON[struct {
			Status string `json:"status"`
		}](t, baseURL, validator, http.MethodDelete, cityBase+"/bead/"+url.PathEscape(id), nil, http.StatusOK)
	}
	liveContractJSON[struct {
		Status string `json:"status"`
	}](t, baseURL, validator, http.MethodDelete, cityBase+"/rig/"+url.PathEscape(rigName), nil, http.StatusOK)

	unregister := liveContractJSON[struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
		Path string `json:"path"`
	}](t, baseURL, validator, http.MethodPost, cityBase+"/unregister", nil, http.StatusAccepted)
	if !unregister.OK || unregister.Name != cityName {
		t.Fatalf("unregister response = %+v, want ok=true name=%q", unregister, cityName)
	}
	waitForCityAbsent(t, baseURL, validator, cityName, 45*time.Second)
}

type contractEventList struct {
	Items []contractEvent `json:"items"`
	Total int             `json:"total"`
}

type contractEvent struct {
	Type    string `json:"type"`
	Subject string `json:"subject"`
	City    string `json:"city"`
	Payload struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"payload"`
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
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, resp.StatusCode, wantStatus, string(raw))
	}
	if v != nil {
		validateLiveContractResponse(t, v, req, resp, raw)
	}
	return raw
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("build stream request %s: %v", path, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s stream: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s stream status = %d, want 200; body: %s", path, resp.StatusCode, string(raw))
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("GET %s stream content-type = %q, want text/event-stream", path, contentType)
	}
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

func waitForLiveContractRequestResult(t *testing.T, baseURL string, v openapivalidator.Validator, path, subject, operation, wantStatus string, timeout time.Duration) {
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
			if event.Subject != subject || event.Type != "request.result" {
				continue
			}
			var payload struct {
				Operation string `json:"operation"`
				Status    string `json:"status"`
			}
			raw, _ := json.Marshal(event.Payload)
			if json.Unmarshal(raw, &payload) != nil {
				continue
			}
			if payload.Operation != operation {
				continue
			}
			if payload.Status == "failed" && wantStatus != "failed" {
				t.Fatalf("request.result(%s) for %s failed: %+v", operation, subject, event.Payload)
			}
			if payload.Status == wantStatus {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for request.result(%s, %s) for %s from %s; last error: %v", operation, wantStatus, subject, path, lastErr)
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
				t.Fatalf("event %s for %s failed: %+v", event.Type, subject, event.Payload)
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
	if len(cityPayloadRefs) != 1 || !strings.Contains(cityPayloadRefs[0], "CityLifecyclePayload") {
		t.Fatalf("EventPayload city lifecycle branches = %#v, want exactly CityLifecyclePayload", cityPayloadRefs)
	}
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
