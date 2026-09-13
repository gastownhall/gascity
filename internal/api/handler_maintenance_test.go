package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// fakeMaintenanceLoop is a deterministic MaintenanceProvider for handler
// tests. Instead of exercising supervisor.StoreMaintenanceLoop against a
// real beads store (and the associated tmux/dolt dependencies), handler
// tests assert the HTTP response surface against canned provider answers.
type fakeMaintenanceLoop struct {
	lastRunAt   time.Time
	history     []supervisor.MaintenanceRun
	inFlight    bool
	inFlightAt  time.Time
	triggerRun  supervisor.MaintenanceRun
	triggerErr  error
	triggerCtx  context.Context
	triggerHits int
}

func (f *fakeMaintenanceLoop) LastRunAt() time.Time {
	return f.lastRunAt
}

func (f *fakeMaintenanceLoop) History() []supervisor.MaintenanceRun {
	out := make([]supervisor.MaintenanceRun, len(f.history))
	copy(out, f.history)
	return out
}

func (f *fakeMaintenanceLoop) InFlightStart() (time.Time, bool) {
	return f.inFlightAt, f.inFlight
}

func (f *fakeMaintenanceLoop) TriggerNow(ctx context.Context) (supervisor.MaintenanceRun, error) {
	f.triggerCtx = ctx
	f.triggerHits++
	if f.triggerErr != nil {
		return supervisor.MaintenanceRun{}, f.triggerErr
	}
	return f.triggerRun, nil
}

func maintenanceTestState(t *testing.T, loop MaintenanceProvider) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cfg.Maintenance.Dolt = config.DoltMaintenance{Enabled: true, Interval: "168h"}
	fs.maintenance = loop
	return fs
}

// TestHumaHandleMaintenanceStatus_Populated verifies the handler renders
// a fully populated body (interval, in-flight marker, last run, history,
// next scheduled) when the supervisor has real state. The JSON is
// decoded through a minimal shape so the test survives field-order
// changes in the generated spec.
func TestHumaHandleMaintenanceStatus_Populated(t *testing.T) {
	started := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Second)
	run := supervisor.MaintenanceRun{
		StartedAt: started, FinishedAt: finished, Stage: "done",
		BeforeBytes: 11_000_000_000, AfterBytes: 2_000_000_000,
	}
	loop := &fakeMaintenanceLoop{
		lastRunAt: started,
		history:   []supervisor.MaintenanceRun{run},
	}
	fs := maintenanceTestState(t, loop)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/maintenance/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Enabled       bool   `json:"enabled"`
		IntervalSec   int64  `json:"interval_seconds"`
		NextScheduled string `json:"next_scheduled"`
		History       []struct {
			Stage     string `json:"stage"`
			StartedAt string `json:"started_at"`
		} `json:"history"`
		LastRun *struct {
			Stage string `json:"stage"`
		} `json:"last_run"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.Enabled {
		t.Errorf("enabled = false; want true")
	}
	if resp.IntervalSec != int64((168 * time.Hour).Seconds()) {
		t.Errorf("IntervalSec = %d; want %d", resp.IntervalSec, int64((168 * time.Hour).Seconds()))
	}
	if resp.NextScheduled == "" {
		t.Errorf("NextScheduled empty; expected %s + 168h", started.Format(time.RFC3339))
	}
	if len(resp.History) != 1 || resp.History[0].Stage != "done" {
		t.Errorf("History = %+v; want one done entry", resp.History)
	}
	if resp.LastRun == nil || resp.LastRun.Stage != "done" {
		t.Errorf("LastRun = %+v; want stage=done", resp.LastRun)
	}
}

// TestHumaHandleMaintenanceStatus_Disabled verifies the handler returns
// 503 with the maintenance_disabled prefix when the supervisor's
// MaintenanceLoop() returns nil (e.g., [maintenance.dolt] enabled=false).
func TestHumaHandleMaintenanceStatus_Disabled(t *testing.T) {
	fs := newFakeState(t)
	fs.maintenance = nil
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/maintenance/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maintenance_disabled") {
		t.Errorf("body missing maintenance_disabled prefix:\n%s", rec.Body.String())
	}
}

// TestHumaHandleMaintenanceTriggerDoltGC_WaitSuccess verifies ?wait=true
// returns 202 with the full Run body when the cycle succeeds. The
// handler registers DefaultStatus=202 so even the synchronous path uses
// 202 (clients distinguish by body shape).
func TestHumaHandleMaintenanceTriggerDoltGC_WaitSuccess(t *testing.T) {
	started := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Second)
	loop := &fakeMaintenanceLoop{
		triggerRun: supervisor.MaintenanceRun{StartedAt: started, FinishedAt: finished, Stage: "done"},
	}
	fs := maintenanceTestState(t, loop)
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/maintenance/dolt-gc")+"?wait=true", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	if loop.triggerHits != 1 {
		t.Errorf("TriggerNow hits = %d; want 1", loop.triggerHits)
	}
	var resp struct {
		Accepted  bool   `json:"accepted"`
		StartedAt string `json:"started_at"`
		Run       *struct {
			Stage string `json:"stage"`
		} `json:"run"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.Accepted {
		t.Errorf("accepted = false; want true")
	}
	if resp.Run == nil || resp.Run.Stage != "done" {
		t.Errorf("Run = %+v; want populated with stage=done", resp.Run)
	}
}

// TestHumaHandleMaintenanceTriggerDoltGC_Conflict verifies that
// MaintenanceInProgressError from the loop maps to 409 with a detail
// string that carries the maintenance-in-progress marker and the
// JSON-encoded started_at.
func TestHumaHandleMaintenanceTriggerDoltGC_Conflict(t *testing.T) {
	inFlightStart := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	loop := &fakeMaintenanceLoop{
		triggerErr: &supervisor.MaintenanceInProgressError{StartedAt: inFlightStart},
	}
	fs := maintenanceTestState(t, loop)
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/maintenance/dolt-gc"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	var pd struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&pd); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if !strings.HasPrefix(pd.Detail, "maintenance-in-progress") {
		t.Errorf("detail missing maintenance-in-progress prefix: %q", pd.Detail)
	}
	if !strings.Contains(pd.Detail, inFlightStart.Format(time.RFC3339)) {
		t.Errorf("detail missing started_at: %q", pd.Detail)
	}
}

// TestHumaHandleMaintenanceTriggerDoltGC_Disabled verifies the 503
// response when the maintenance loop is not configured (defense-in-depth
// — the CLI also has to handle the nil-loop case gracefully).
func TestHumaHandleMaintenanceTriggerDoltGC_Disabled(t *testing.T) {
	fs := newFakeState(t)
	fs.maintenance = nil
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/maintenance/dolt-gc"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maintenance_disabled") {
		t.Errorf("body missing maintenance_disabled:\n%s", rec.Body.String())
	}
}

// TestMaintenanceConflictFromError verifies the error translator handles
// both in-progress and generic errors without panicking on nil StartedAt.
func TestMaintenanceConflictFromError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want409  bool
		wantBody string
	}{
		{
			name:     "in-progress-with-start",
			err:      &supervisor.MaintenanceInProgressError{StartedAt: time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)},
			want409:  true,
			wantBody: `"started_at":"2026-04-22T03:00:00Z"`,
		},
		{
			name:     "in-progress-zero-start",
			err:      &supervisor.MaintenanceInProgressError{},
			want409:  true,
			wantBody: `"type":"maintenance-in-progress"`,
		},
		{
			name:     "generic-error",
			err:      errors.New("boom"),
			want409:  false,
			wantBody: "boom",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := maintenanceConflictFromError(tc.err)
			if got == nil {
				t.Fatal("maintenanceConflictFromError = nil; want error")
			}
			msg := got.Error()
			if tc.want409 {
				if !strings.Contains(msg, "maintenance-in-progress") {
					t.Errorf("error missing maintenance-in-progress marker: %q", msg)
				}
				if !strings.Contains(msg, tc.wantBody) {
					t.Errorf("error missing body fragment %q: %q", tc.wantBody, msg)
				}
			} else if !strings.Contains(msg, tc.wantBody) {
				t.Errorf("error = %q; want contains %q", msg, tc.wantBody)
			}
		})
	}
}
