package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/mail"
)

type stubWarmupCheck struct {
	name            string
	warmup          bool
	runDelay        time.Duration
	returnedStatus  doctor.CheckStatus
	returnedMessage string
	fixHint         string
	panicMessage    string
}

func (c stubWarmupCheck) Name() string { return c.name }

func (c stubWarmupCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.runDelay > 0 {
		time.Sleep(c.runDelay)
	}
	if c.panicMessage != "" {
		panic(c.panicMessage)
	}
	msg := c.returnedMessage
	if msg == "" {
		msg = "ok"
	}
	return &doctor.CheckResult{
		Name:    c.name,
		Status:  c.returnedStatus,
		Message: msg,
		FixHint: c.fixHint,
	}
}

func (c stubWarmupCheck) CanFix() bool { return false }

func (c stubWarmupCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c stubWarmupCheck) WarmupEligible() bool { return c.warmup }

type recordingWarmupMailer struct {
	sent    []mail.Message
	sendErr error
}

func (m *recordingWarmupMailer) Send(from, to, subject, body string) (mail.Message, error) {
	if m.sendErr != nil {
		return mail.Message{}, m.sendErr
	}
	msg := mail.Message{
		ID:      fmt.Sprintf("warmup-%d", len(m.sent)+1),
		From:    from,
		To:      to,
		Subject: subject,
		Body:    body,
	}
	m.sent = append(m.sent, msg)
	return msg, nil
}

func (m *recordingWarmupMailer) Inbox(string) ([]mail.Message, error) {
	return nil, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) Get(string) (mail.Message, error) {
	return mail.Message{}, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) Read(string) (mail.Message, error) {
	return mail.Message{}, errWarmupMailerNotImplemented
}
func (m *recordingWarmupMailer) MarkRead(string) error   { return errWarmupMailerNotImplemented }
func (m *recordingWarmupMailer) MarkUnread(string) error { return errWarmupMailerNotImplemented }
func (m *recordingWarmupMailer) Archive(string) error    { return errWarmupMailerNotImplemented }
func (m *recordingWarmupMailer) ArchiveMany([]string) ([]mail.ArchiveResult, error) {
	return nil, errWarmupMailerNotImplemented
}
func (m *recordingWarmupMailer) Delete(string) error { return errWarmupMailerNotImplemented }
func (m *recordingWarmupMailer) DeleteMany([]string) ([]mail.ArchiveResult, error) {
	return nil, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) Check(string) ([]mail.Message, error) {
	return nil, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) Reply(string, string, string, string) (mail.Message, error) {
	return mail.Message{}, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) Thread(string) ([]mail.Message, error) {
	return nil, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) All(string) ([]mail.Message, error) {
	return nil, errWarmupMailerNotImplemented
}

func (m *recordingWarmupMailer) Count(string) (int, int, error) {
	return 0, 0, errWarmupMailerNotImplemented
}

var errWarmupMailerNotImplemented = errors.New("not implemented")

func warmupTestConfig() *config.City {
	return &config.City{Workspace: config.Workspace{Name: "demo"}}
}

func runWarmupTest(t *testing.T, checks []doctor.Check, opts WarmupOpts) (*WarmupReport, *recordingWarmupMailer, string) {
	t.Helper()
	mailer, _ := opts.Mailer.(*recordingWarmupMailer)
	if opts.Mailer == nil {
		mailer = &recordingWarmupMailer{}
		opts.Mailer = mailer
	}
	var stderr bytes.Buffer
	if opts.Stderr == nil {
		opts.Stderr = &stderr
	}
	opts.checksOverride = checks
	report, err := RunWarmupChecks(context.Background(), t.TempDir(), warmupTestConfig(), opts)
	if err != nil {
		t.Fatalf("RunWarmupChecks returned error: %v", err)
	}
	if report == nil {
		t.Fatal("RunWarmupChecks returned nil report")
	}
	return report, mailer, stderr.String()
}

func TestRunWarmupChecks_ParallelExecution(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "a", warmup: true, runDelay: 200 * time.Millisecond},
		stubWarmupCheck{name: "b", warmup: true, runDelay: 200 * time.Millisecond},
		stubWarmupCheck{name: "c", warmup: true, runDelay: 200 * time.Millisecond},
	}

	start := time.Now()
	report, mailer, stderr := runWarmupTest(t, checks, WarmupOpts{})
	elapsed := time.Since(start)

	if elapsed >= 400*time.Millisecond {
		t.Fatalf("RunWarmupChecks elapsed %s, want <400ms", elapsed)
	}
	if report.HighestSeverity != doctor.StatusOK {
		t.Fatalf("HighestSeverity = %v, want StatusOK", report.HighestSeverity)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("sent mail count = %d, want 0", len(mailer.sent))
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunWarmupChecks_PerCheckDeadline(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "slow", warmup: true, runDelay: 10 * time.Second},
	}

	start := time.Now()
	report, mailer, stderr := runWarmupTest(t, checks, WarmupOpts{PerCheckDeadline: 100 * time.Millisecond})
	elapsed := time.Since(start)

	if elapsed >= 500*time.Millisecond {
		t.Fatalf("RunWarmupChecks elapsed %s, want <500ms", elapsed)
	}
	if len(report.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(report.Failures))
	}
	if !report.Failures[0].Timeout {
		t.Fatalf("Timeout = false, want true: %+v", report.Failures[0])
	}
	if report.Failures[0].Message != "warmup-timeout" {
		t.Fatalf("Message = %q, want warmup-timeout", report.Failures[0].Message)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("sent mail count = %d, want 1", len(mailer.sent))
	}
	if !strings.Contains(stderr, "warmup: 1 check(s) failed (Error)") {
		t.Fatalf("stderr = %q, want failure summary", stderr)
	}
}

func TestRunWarmupChecks_TotalDeadline(t *testing.T) {
	var checks []doctor.Check
	for i := 0; i < 10; i++ {
		checks = append(checks, stubWarmupCheck{name: fmt.Sprintf("slow-%02d", i), warmup: true, runDelay: 5 * time.Second})
	}

	start := time.Now()
	report, _, _ := runWarmupTest(t, checks, WarmupOpts{
		PerCheckDeadline: 5 * time.Second,
		TotalDeadline:    200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if elapsed >= 500*time.Millisecond {
		t.Fatalf("RunWarmupChecks elapsed %s, want <500ms", elapsed)
	}
	if report.HighestSeverity < doctor.StatusError {
		t.Fatalf("HighestSeverity = %v, want at least StatusError", report.HighestSeverity)
	}
	foundTimeout := false
	for _, failure := range report.Failures {
		foundTimeout = foundTimeout || failure.Timeout
	}
	if !foundTimeout {
		t.Fatalf("no timeout failure in %+v", report.Failures)
	}
}

func TestRunWarmupChecks_FailOpen_PanicInCheck(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "panicker", warmup: true, panicMessage: "boom"},
	}

	report, mailer, _ := runWarmupTest(t, checks, WarmupOpts{})

	if len(report.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(report.Failures))
	}
	if got := report.Failures[0].Panic; got != "boom" {
		t.Fatalf("Panic = %q, want boom", got)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("sent mail count = %d, want 1", len(mailer.sent))
	}
	if !strings.Contains(mailer.sent[0].Subject, "panicker") || !strings.Contains(mailer.sent[0].Body, "panicker") {
		t.Fatalf("mail should reference panicked check: %+v", mailer.sent[0])
	}
}

func TestRunWarmupChecks_FailOpen_MailerError(t *testing.T) {
	mailer := &recordingWarmupMailer{sendErr: errors.New("smtp dead")}
	checks := []doctor.Check{
		stubWarmupCheck{name: "bad", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}

	report, _, stderr := runWarmupTest(t, checks, WarmupOpts{Mailer: mailer})

	if report.MailSendError == nil || !strings.Contains(report.MailSendError.Error(), "smtp dead") {
		t.Fatalf("MailSendError = %v, want smtp dead", report.MailSendError)
	}
	if report.MailSent {
		t.Fatal("MailSent = true, want false")
	}
	if !strings.Contains(stderr, "mail send error: smtp dead") {
		t.Fatalf("stderr = %q, want mail error", stderr)
	}
}

func TestRunWarmupChecks_FailOpen_RunnerPanic(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "bad", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}
	var stderr bytes.Buffer
	opts := WarmupOpts{Mailer: nil, Stderr: &stderr, checksOverride: checks}

	report, err := RunWarmupChecks(context.Background(), t.TempDir(), warmupTestConfig(), opts)
	if err != nil {
		t.Fatalf("RunWarmupChecks error = %v, want nil", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
	if report.MailSendError == nil {
		t.Fatal("MailSendError is nil, want missing mailer error")
	}
	if report.MailSent {
		t.Fatal("MailSent = true, want false")
	}
}

func TestRunWarmupChecks_AllOK_Silent(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "a", warmup: true},
		stubWarmupCheck{name: "b", warmup: true},
	}

	report, mailer, stderr := runWarmupTest(t, checks, WarmupOpts{})

	if len(report.Failures) != 0 {
		t.Fatalf("failures = %d, want 0", len(report.Failures))
	}
	if report.FailureSetHash != "" {
		t.Fatalf("FailureSetHash = %q, want empty", report.FailureSetHash)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("sent mail count = %d, want 0", len(mailer.sent))
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunWarmupChecks_NoEligibleChecks(t *testing.T) {
	var checks []doctor.Check
	for i := 0; i < 10; i++ {
		checks = append(checks, stubWarmupCheck{name: fmt.Sprintf("check-%d", i), warmup: false, returnedStatus: doctor.StatusError})
	}

	report, mailer, stderr := runWarmupTest(t, checks, WarmupOpts{})

	if len(report.ScopeResults) != 0 {
		t.Fatalf("ScopeResults = %d, want 0", len(report.ScopeResults))
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("sent mail count = %d, want 0", len(mailer.sent))
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunWarmupChecks_MailSubject_SingleCheck(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "core-pg:auth", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}

	_, mailer, _ := runWarmupTest(t, checks, WarmupOpts{})

	if len(mailer.sent) != 1 {
		t.Fatalf("sent mail count = %d, want 1", len(mailer.sent))
	}
	if got, want := mailer.sent[0].Subject, "core-pg:auth alert during city warm-up"; got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
}

func TestRunWarmupChecks_MailSubject_MultipleChecks(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "a", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
		stubWarmupCheck{name: "b", warmup: true, returnedStatus: doctor.StatusWarning, returnedMessage: "warn"},
		stubWarmupCheck{name: "c", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}

	_, mailer, _ := runWarmupTest(t, checks, WarmupOpts{})

	if got, want := mailer.sent[0].Subject, "city warm-up: 3 doctor check(s) failed"; got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
}

func TestRunWarmupChecks_MailBody_BoundedTo4KB(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "huge", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: strings.Repeat("x", 8*1024)},
	}

	_, mailer, _ := runWarmupTest(t, checks, WarmupOpts{})

	if len(mailer.sent) != 1 {
		t.Fatalf("sent mail count = %d, want 1", len(mailer.sent))
	}
	body := mailer.sent[0].Body
	if len(body) > 4096 {
		t.Fatalf("body length = %d, want <=4096", len(body))
	}
	if !strings.HasSuffix(body, "(truncated, see gc doctor for full output)\n") {
		t.Fatalf("body suffix = %q, want truncation marker", body[len(body)-80:])
	}
}

func TestRunWarmupChecks_MailBody_ExcludesSecretsByDefault(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "leaky", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "password=hunter2"},
	}

	_, mailer, _ := runWarmupTest(t, checks, WarmupOpts{})

	if !strings.Contains(mailer.sent[0].Body, "password=hunter2") {
		t.Fatalf("body = %q, want content-agnostic inclusion for slice-2", mailer.sent[0].Body)
	}
}

func TestRunWarmupChecks_FailureSetHash_Deterministic(t *testing.T) {
	checksA := []doctor.Check{
		stubWarmupCheck{name: "b", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
		stubWarmupCheck{name: "a", warmup: true, returnedStatus: doctor.StatusWarning, returnedMessage: "warn"},
	}
	checksB := []doctor.Check{
		stubWarmupCheck{name: "a", warmup: true, returnedStatus: doctor.StatusWarning, returnedMessage: "warn"},
		stubWarmupCheck{name: "b", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}

	reportA, _, _ := runWarmupTest(t, checksA, WarmupOpts{})
	reportB, _, _ := runWarmupTest(t, checksB, WarmupOpts{})

	if reportA.FailureSetHash == "" {
		t.Fatal("FailureSetHash is empty, want sha256 hex")
	}
	if reportA.FailureSetHash != reportB.FailureSetHash {
		t.Fatalf("hashes differ: %q != %q", reportA.FailureSetHash, reportB.FailureSetHash)
	}
	if ok, _ := regexp.MatchString(`^[0-9a-f]{64}$`, reportA.FailureSetHash); !ok {
		t.Fatalf("hash = %q, want 64 hex chars", reportA.FailureSetHash)
	}
}

func TestRunWarmupChecks_FailureSetHash_DiffersOnSeverityEscalation(t *testing.T) {
	warning, _, _ := runWarmupTest(t, []doctor.Check{
		stubWarmupCheck{name: "same", warmup: true, returnedStatus: doctor.StatusWarning, returnedMessage: "warn"},
	}, WarmupOpts{})
	errReport, _, _ := runWarmupTest(t, []doctor.Check{
		stubWarmupCheck{name: "same", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}, WarmupOpts{})

	if warning.FailureSetHash == errReport.FailureSetHash {
		t.Fatalf("hashes equal across severity escalation: %q", warning.FailureSetHash)
	}
}

func TestRunWarmupChecks_StderrSummaryLineFormat(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "a", warmup: true, returnedStatus: doctor.StatusWarning, returnedMessage: "warn"},
		stubWarmupCheck{name: "b", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}

	_, _, stderr := runWarmupTest(t, checks, WarmupOpts{})

	want := "gc start: warmup: 2 check(s) failed (Error); see mail to mayor and `gc doctor` for details\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

func TestRunWarmupChecks_CustomMailTo(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "bad", warmup: true, returnedStatus: doctor.StatusError, returnedMessage: "bad"},
	}

	_, mailer, stderr := runWarmupTest(t, checks, WarmupOpts{MailTo: "ops"})

	if len(mailer.sent) != 1 {
		t.Fatalf("sent mail count = %d, want 1", len(mailer.sent))
	}
	if got := mailer.sent[0].To; got != "ops" {
		t.Fatalf("mail To = %q, want ops", got)
	}
	if !strings.Contains(stderr, "see mail to ops") {
		t.Fatalf("stderr = %q, want \"see mail to ops\"", stderr)
	}
}

func TestRunWarmupChecks_StderrSilentOnOK(t *testing.T) {
	checks := []doctor.Check{
		stubWarmupCheck{name: "ok", warmup: true},
	}

	_, _, stderr := runWarmupTest(t, checks, WarmupOpts{})

	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunWarmupChecks_NilCfg_Reported(t *testing.T) {
	var stderr bytes.Buffer
	report, err := RunWarmupChecks(context.Background(), t.TempDir(), nil, WarmupOpts{
		Mailer: &recordingWarmupMailer{},
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("RunWarmupChecks error = %v, want nil", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
	if report.HighestSeverity != doctor.StatusOK {
		t.Fatalf("HighestSeverity = %v, want StatusOK", report.HighestSeverity)
	}
	if len(report.ScopeResults) != 0 {
		t.Fatalf("ScopeResults = %d, want 0", len(report.ScopeResults))
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunWarmupChecks_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	checks := []doctor.Check{
		stubWarmupCheck{name: "slow", warmup: true, runDelay: 5 * time.Second},
	}
	cancel()
	var stderr bytes.Buffer
	opts := WarmupOpts{
		Mailer:         &recordingWarmupMailer{},
		Stderr:         &stderr,
		TotalDeadline:  5 * time.Second,
		checksOverride: checks,
	}

	done := make(chan struct{})
	var report *WarmupReport
	var err error
	go func() {
		defer close(done)
		report, err = RunWarmupChecks(ctx, t.TempDir(), warmupTestConfig(), opts)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunWarmupChecks did not return within 1s after context cancellation")
	}
	if err != nil {
		t.Fatalf("RunWarmupChecks error = %v, want nil", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
}

func TestDefaultMailProviderUsesStartedCityPath(t *testing.T) {
	t.Setenv("GC_MAIL", "")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[mail]\nprovider = \"fake\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	provider := defaultMailProvider(cityDir)
	if provider == nil {
		t.Fatal("defaultMailProvider returned nil")
	}
	if _, err := provider.Send("gc-start-warmup", "mayor", "subject", "body"); err != nil {
		t.Fatalf("fake mail provider Send returned error: %v", err)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
