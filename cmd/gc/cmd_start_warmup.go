package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/mail"
	"golang.org/x/sync/errgroup"
)

const (
	// DefaultWarmupPerCheckDeadline is the default maximum runtime for one
	// warm-up doctor check.
	DefaultWarmupPerCheckDeadline = 5 * time.Second
	// DefaultWarmupTotalDeadline is the default maximum runtime for the whole
	// warm-up doctor scan.
	DefaultWarmupTotalDeadline = 30 * time.Second
)

const (
	defaultWarmupMailFrom    = "gc-start-warmup"
	defaultWarmupMailTo      = "mayor"
	warmupMailBodyLimit      = 4096
	warmupTruncationSuffix   = "\n(truncated, see gc doctor for full output)\n"
	warmupTimeoutMessage     = "warmup-timeout"
	warmupMissingMailerError = "warmup mailer is required"
)

// WarmupCheckResult is one check outcome in a `gc start` warm-up scan.
type WarmupCheckResult struct {
	Scope   string
	Check   string
	Status  doctor.CheckStatus
	Message string
	FixHint string
	Timeout bool
	Panic   string
}

// ScopeWarmupResult groups warm-up check outcomes for one city or rig scope.
type ScopeWarmupResult struct {
	ScopePath    string
	ScopeDisplay string
	Severity     doctor.CheckStatus
	CheckResults []WarmupCheckResult
}

// WarmupReport summarizes a `gc start` warm-up scan.
type WarmupReport struct {
	StartedAt           time.Time
	CompletedAt         time.Time
	HighestSeverity     doctor.CheckStatus
	ScopeResults        []ScopeWarmupResult
	Failures            []WarmupCheckResult
	FailureSetHash      string
	MailSent            bool
	MailSendError       error
	SuppressedFromMayor bool
	SuppressionReason   string
}

// WarmupOpts configures RunWarmupChecks.
type WarmupOpts struct {
	Mailer           mail.Provider
	Stderr           io.Writer
	Now              func() time.Time
	PerCheckDeadline time.Duration
	TotalDeadline    time.Duration
	MailFrom         string
	MailTo           string
	checksOverride   []doctor.Check
}

// RunWarmupChecks runs warm-up-eligible doctor checks during `gc start`.
func RunWarmupChecks(ctx context.Context, cityPath string, cfg *config.City, opts WarmupOpts) (report *WarmupReport, err error) {
	settings := normalizeWarmupOpts(opts)
	report = &WarmupReport{StartedAt: settings.Now()}
	defer func() {
		if recovered := recover(); recovered != nil {
			report = &WarmupReport{
				StartedAt:       report.StartedAt,
				CompletedAt:     settings.Now(),
				HighestSeverity: doctor.StatusOK,
				MailSendError:   fmt.Errorf("warmup runner panic: %v", recovered),
			}
			fmt.Fprintf(settings.Stderr, "gc start: warmup: runner panic: %v\n", recovered) //nolint:errcheck // best-effort stderr
			err = nil
		}
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil && opts.checksOverride == nil {
		report.CompletedAt = settings.Now()
		report.HighestSeverity = doctor.StatusOK
		return report, nil
	}
	absCityPath, pathErr := filepath.Abs(cityPath)
	if pathErr == nil {
		cityPath = absCityPath
	}

	checks := opts.checksOverride
	if checks == nil {
		checks = buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
			Stderr:               io.Discard,
			ControllerRunning:    doctor.IsControllerRunning(cityPath),
			SkipCityDoltCheck:    gcDoltSkip() || (!scopeUsesManagedBdStoreContract(cityPath, cityPath) && !workspaceNeedsCityDoltCheck(cityPath, cfg)),
			SkipManagedDoltCheck: managedDoltOpsCheckSkip(cityPath, cfg, nil),
		})
	}
	eligible := warmupEligibleChecks(checks)
	if len(eligible) == 0 {
		report.CompletedAt = settings.Now()
		report.HighestSeverity = doctor.StatusOK
		return report, nil
	}

	parentCtx, cancel := context.WithTimeout(ctx, settings.TotalDeadline)
	defer cancel()
	rigScopes := warmupRigScopes(cityPath, cfg)
	results := make(chan WarmupCheckResult, len(eligible))
	group, groupCtx := errgroup.WithContext(parentCtx)
	for _, check := range eligible {
		check := check
		scopeDisplay, scopePath := warmupScopeForCheck(check.Name(), cityPath, rigScopes)
		group.Go(func() error {
			results <- runOneWarmupCheck(groupCtx, check, scopeDisplay, scopePath, settings.PerCheckDeadline)
			return nil
		})
	}
	_ = group.Wait()
	close(results)

	var collected []WarmupCheckResult
	scopePaths := make(map[string]string)
	for result := range results {
		collected = append(collected, result)
		if _, ok := scopePaths[result.Scope]; !ok {
			_, scopePath := warmupScopeForCheck(result.Check, cityPath, rigScopes)
			scopePaths[result.Scope] = scopePath
		}
	}
	report.ScopeResults = buildScopeWarmupResults(collected, scopePaths)
	report.Failures = warmupFailures(collected)
	report.HighestSeverity = highestWarmupSeverity(collected)
	report.FailureSetHash = warmupFailureSetHash(report.Failures)
	report.CompletedAt = settings.Now()

	if len(report.Failures) == 0 {
		return report, nil
	}
	subject := warmupMailSubject(report.Failures)
	body := warmupMailBody(report)
	if settings.Mailer == nil {
		report.MailSendError = errors.New(warmupMissingMailerError)
	} else if _, sendErr := settings.Mailer.Send(settings.MailFrom, settings.MailTo, subject, body); sendErr != nil {
		report.MailSendError = sendErr
	} else {
		report.MailSent = true
	}
	writeWarmupStderr(settings.Stderr, settings.MailTo, report)
	return report, nil
}

func normalizeWarmupOpts(opts WarmupOpts) WarmupOpts {
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PerCheckDeadline <= 0 {
		opts.PerCheckDeadline = DefaultWarmupPerCheckDeadline
	}
	if opts.TotalDeadline <= 0 {
		opts.TotalDeadline = DefaultWarmupTotalDeadline
	}
	if opts.MailFrom == "" {
		opts.MailFrom = defaultWarmupMailFrom
	}
	if opts.MailTo == "" {
		opts.MailTo = defaultWarmupMailTo
	}
	return opts
}

func warmupEligibleChecks(checks []doctor.Check) []doctor.Check {
	var eligible []doctor.Check
	for _, check := range checks {
		if check == nil {
			continue
		}
		if check.WarmupEligible() {
			eligible = append(eligible, check)
		}
	}
	return eligible
}

func runOneWarmupCheck(ctx context.Context, check doctor.Check, scopeDisplay string, scopePath string, deadline time.Duration) WarmupCheckResult {
	checkName := check.Name()
	checkCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	resultCh := make(chan WarmupCheckResult, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicText := fmt.Sprint(recovered)
				resultCh <- WarmupCheckResult{
					Scope:   scopeDisplay,
					Check:   checkName,
					Status:  doctor.StatusError,
					Message: "warmup-panic: " + panicText,
					Panic:   panicText,
				}
			}
		}()
		result := check.Run(&doctor.CheckContext{CityPath: scopePath, Verbose: false})
		if result == nil {
			resultCh <- WarmupCheckResult{
				Scope:   scopeDisplay,
				Check:   checkName,
				Status:  doctor.StatusError,
				Message: "warmup-empty-result",
			}
			return
		}
		resultCh <- WarmupCheckResult{
			Scope:   scopeDisplay,
			Check:   checkName,
			Status:  result.Status,
			Message: result.Message,
			FixHint: result.FixHint,
		}
	}()

	select {
	case result := <-resultCh:
		return result
	case <-checkCtx.Done():
		return WarmupCheckResult{
			Scope:   scopeDisplay,
			Check:   checkName,
			Status:  doctor.StatusError,
			Message: warmupTimeoutMessage,
			Timeout: true,
		}
	}
}

func warmupRigScopes(cityPath string, cfg *config.City) map[string]string {
	scopes := make(map[string]string)
	if cfg == nil {
		return scopes
	}
	for _, rig := range cfg.Rigs {
		name := strings.TrimSpace(rig.Name)
		if name == "" {
			continue
		}
		path := strings.TrimSpace(rig.Path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cityPath, path)
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		scopes[name] = path
	}
	return scopes
}

func warmupScopeForCheck(checkName string, cityPath string, rigScopes map[string]string) (string, string) {
	prefix, _, ok := strings.Cut(checkName, ":")
	if ok {
		if scopePath, found := rigScopes[prefix]; found {
			return prefix, scopePath
		}
	}
	return "city", cityPath
}

func buildScopeWarmupResults(results []WarmupCheckResult, scopePaths map[string]string) []ScopeWarmupResult {
	grouped := make(map[string][]WarmupCheckResult)
	for _, result := range results {
		grouped[result.Scope] = append(grouped[result.Scope], result)
	}
	scopes := make([]string, 0, len(grouped))
	for scope := range grouped {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i] == "city" {
			return true
		}
		if scopes[j] == "city" {
			return false
		}
		return scopes[i] < scopes[j]
	})

	scopeResults := make([]ScopeWarmupResult, 0, len(scopes))
	for _, scope := range scopes {
		checks := grouped[scope]
		sort.Slice(checks, func(i, j int) bool {
			return checks[i].Check < checks[j].Check
		})
		scopeResults = append(scopeResults, ScopeWarmupResult{
			ScopePath:    scopePaths[scope],
			ScopeDisplay: scope,
			Severity:     highestWarmupSeverity(checks),
			CheckResults: checks,
		})
	}
	return scopeResults
}

func warmupFailures(results []WarmupCheckResult) []WarmupCheckResult {
	var failures []WarmupCheckResult
	for _, result := range results {
		if result.Status >= doctor.StatusWarning {
			failures = append(failures, result)
		}
	}
	sortWarmupFailures(failures)
	return failures
}

func sortWarmupFailures(failures []WarmupCheckResult) {
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].Scope != failures[j].Scope {
			return failures[i].Scope < failures[j].Scope
		}
		return failures[i].Check < failures[j].Check
	})
}

func highestWarmupSeverity(results []WarmupCheckResult) doctor.CheckStatus {
	highest := doctor.StatusOK
	for _, result := range results {
		if result.Status > highest {
			highest = result.Status
		}
	}
	return highest
}

func warmupFailureSetHash(failures []WarmupCheckResult) string {
	if len(failures) == 0 {
		return ""
	}
	sorted := append([]WarmupCheckResult(nil), failures...)
	sortWarmupFailures(sorted)
	var b strings.Builder
	for _, failure := range sorted {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", failure.Scope, failure.Check, warmupStatusString(failure.Status))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func warmupMailSubject(failures []WarmupCheckResult) string {
	if len(failures) == 0 {
		return "city warm-up: 0 doctor check(s) failed"
	}
	firstCheck := failures[0].Check
	for _, failure := range failures[1:] {
		if failure.Check != firstCheck {
			return fmt.Sprintf("city warm-up: %d doctor check(s) failed", len(failures))
		}
	}
	return firstCheck + " alert during city warm-up"
}

func warmupMailBody(report *WarmupReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "city warm-up: %d doctor check(s) failed (%s)\n\n", len(report.Failures), warmupStatusString(report.HighestSeverity))
	for _, failure := range report.Failures {
		fmt.Fprintf(&b, "%s %s — %s: %s", warmupIcon(failure.Status), failure.Scope, failure.Check, failure.Message)
		if failure.FixHint != "" {
			fmt.Fprintf(&b, "\n  fix: %s", failure.FixHint)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n— see `gc doctor` for full details.\n")
	return truncateWarmupMailBody(b.String())
}

func truncateWarmupMailBody(body string) string {
	if len(body) <= warmupMailBodyLimit {
		return body
	}
	limit := warmupMailBodyLimit - len(warmupTruncationSuffix)
	var b strings.Builder
	for _, r := range body {
		if b.Len()+len(string(r)) > limit {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString(warmupTruncationSuffix)
	return b.String()
}

func warmupIcon(status doctor.CheckStatus) string {
	switch status {
	case doctor.StatusWarning:
		return "⚠"
	case doctor.StatusError:
		return "✗"
	default:
		return "✓"
	}
}

func writeWarmupStderr(stderr io.Writer, mailTo string, report *WarmupReport) {
	if len(report.Failures) == 0 {
		return
	}
	if report.MailSendError != nil {
		fmt.Fprintf(stderr, "gc start: warmup: %d check(s) failed (%s); mail send error: %v\n", len(report.Failures), warmupStatusString(report.HighestSeverity), report.MailSendError) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "gc start: warmup: %d check(s) failed (%s); see mail to %s and `gc doctor` for details\n", len(report.Failures), warmupStatusString(report.HighestSeverity), mailTo) //nolint:errcheck // best-effort stderr
}

func warmupStatusString(status doctor.CheckStatus) string {
	switch status {
	case doctor.StatusOK:
		return "OK"
	case doctor.StatusWarning:
		return "Warning"
	case doctor.StatusError:
		return "Error"
	default:
		return "Unknown"
	}
}

func defaultMailProvider(cityPath string) mail.Provider {
	name := os.Getenv("GC_MAIL")
	if name == "" {
		name = mailProviderNameForCity(cityPath)
	}
	if strings.HasPrefix(name, "exec:") || name == "fake" || name == "fail" {
		return newCommandMailProviderNamed(name, nil)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	return newCommandMailProviderNamed(name, store)
}
