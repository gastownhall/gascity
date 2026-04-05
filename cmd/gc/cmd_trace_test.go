package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTraceStartStopStatusOfflineFallback(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdTraceStart("repo/polecat", "15m", false, string(TraceModeDetail), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStart = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "armed manual repo/polecat") {
		t.Fatalf("start output = %q, want armed confirmation", got)
	}

	status, _, err := traceStatusLocal(cityDir)
	if err != nil {
		t.Fatalf("traceStatusLocal: %v", err)
	}
	if status == nil {
		t.Fatal("traceStatusLocal returned nil status")
	}
	if len(status.ActiveArms) != 1 {
		t.Fatalf("active arms = %d, want 1", len(status.ActiveArms))
	}
	arm := status.ActiveArms[0]
	if arm.ScopeValue != "repo/polecat" {
		t.Fatalf("scope_value = %q, want repo/polecat", arm.ScopeValue)
	}
	if arm.Level != TraceModeDetail {
		t.Fatalf("level = %q, want detail", arm.Level)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceStatus(&stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStatus = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"head_seq": 0`) || !strings.Contains(got, "repo/polecat") {
		t.Fatalf("status output = %q, want head_seq and arm info", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceStop("repo/polecat", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStop = %d; stderr=%s", code, stderr.String())
	}
	status, _, err = traceStatusLocal(cityDir)
	if err != nil {
		t.Fatalf("traceStatusLocal after stop: %v", err)
	}
	if status == nil {
		t.Fatal("traceStatusLocal after stop returned nil status")
	}
	if len(status.ActiveArms) != 0 {
		t.Fatalf("active arms after stop = %d, want 0", len(status.ActiveArms))
	}
}

func TestTraceControllerSocketCommands(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	startReq := traceControlRequest{
		Action:         "start",
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		For:            "10m",
		ActorKind:      "cli",
		CommandSummary: traceCommandSummary("trace.start", "repo/polecat", "10m", false),
	}
	pokeCh1 := make(chan struct{}, 1)
	startReply := sendTraceSocketCommand(t, cityDir, "trace-arm", startReq, pokeCh1)
	if !startReply.OK {
		t.Fatalf("start reply error: %s", startReply.Error)
	}
	if startReply.Status == nil || len(startReply.Status.ActiveArms) != 1 {
		t.Fatalf("start reply status = %#v", startReply.Status)
	}
	select {
	case <-pokeCh1:
	default:
		t.Fatal("expected pokeCh to be signaled on start")
	}

	pokeCh2 := make(chan struct{}, 1)
	statusReply := sendTraceStatusSocketCommand(t, cityDir, pokeCh2)
	if !statusReply.OK {
		t.Fatalf("status reply error: %s", statusReply.Error)
	}
	if statusReply.Status == nil || len(statusReply.Status.ActiveArms) != 1 {
		t.Fatalf("status reply = %#v", statusReply.Status)
	}
	select {
	case <-pokeCh2:
		t.Fatal("did not expect pokeCh to be signaled on status")
	default:
	}

	stopReq := traceControlRequest{
		Action:         "stop",
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		All:            false,
		ActorKind:      "cli",
		CommandSummary: traceCommandSummary("trace.stop", "repo/polecat", "", false),
	}
	pokeCh3 := make(chan struct{}, 1)
	stopReply := sendTraceSocketCommand(t, cityDir, "trace-stop", stopReq, pokeCh3)
	if !stopReply.OK {
		t.Fatalf("stop reply error: %s", stopReply.Error)
	}
	if stopReply.Status == nil || len(stopReply.Status.ActiveArms) != 0 {
		t.Fatalf("stop reply status = %#v", stopReply.Status)
	}
	select {
	case <-pokeCh3:
	default:
		t.Fatal("expected pokeCh to be signaled on stop")
	}
}

func TestTraceControllerSocketInvalidRequestDoesNotPoke(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	cityDir := t.TempDir()
	convergenceReqCh := make(chan convergenceRequest, 1)
	pokeCh := make(chan struct{}, 1)
	controlDispatcherCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityDir, func() {}, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	if _, err := fmt.Fprintln(client, "trace-arm:{not-json}"); err != nil {
		t.Fatalf("write invalid trace-arm: %v", err)
	}
	reply := readTraceSocketReply(t, client)
	if reply.OK {
		t.Fatal("invalid trace-arm unexpectedly succeeded")
	}
	select {
	case <-pokeCh:
		t.Fatal("invalid trace-arm should not poke controller")
	default:
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller socket handler did not exit")
	}
}

func TestTraceShowAndReasonsWithoutTemplateFilter(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	defer store.Close() //nolint:errcheck

	rec := newTraceRecord(TraceRecordDecision)
	rec.TraceID = "cycle-1"
	rec.TickID = "tick-1"
	rec.RecordID = "record-1"
	rec.Template = "repo/polecat"
	rec.SessionName = "polecat-1"
	rec.SiteCode = TraceSiteReconcilerWakeDecision
	rec.ReasonCode = TraceReasonIdle
	rec.OutcomeCode = TraceOutcomeApplied
	rec.Ts = time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdTraceShow("", "", "", "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceShow = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "repo/polecat") {
		t.Fatalf("trace show output = %q, want repo/polecat", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceReasons("", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceReasons = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, string(TraceReasonIdle)) {
		t.Fatalf("trace reasons output = %q, want idle reason", got)
	}
}

func sendTraceSocketCommand(t *testing.T, cityDir, command string, req traceControlRequest, pokeCh chan struct{}) traceControlReply {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	convergenceReqCh := make(chan convergenceRequest, 1)
	controlDispatcherCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityDir, func() {}, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := fmt.Fprintf(client, "%s:%s\n", command, payload); err != nil {
		t.Fatalf("write command: %v", err)
	}
	reply := readTraceSocketReply(t, client)
	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller socket handler did not exit")
	}
	return reply
}

func sendTraceStatusSocketCommand(t *testing.T, cityDir string, pokeCh chan struct{}) traceControlReply {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	convergenceReqCh := make(chan convergenceRequest, 1)
	controlDispatcherCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityDir, func() {}, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	if _, err := fmt.Fprintln(client, "trace-status"); err != nil {
		t.Fatalf("write status command: %v", err)
	}
	reply := readTraceSocketReply(t, client)
	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller socket handler did not exit")
	}
	return reply
}

func readTraceSocketReply(t *testing.T, conn net.Conn) traceControlReply {
	t.Helper()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		t.Fatal("read reply: connection closed")
	}
	var reply traceControlReply
	if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}
