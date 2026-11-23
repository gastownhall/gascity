package observation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// statusProvider is the minimal provider fixture for the SID projection: the
// Fake answers per-handle metadata, and an explicit live listing stands in for
// the provider's own ListRunning.
type statusProvider struct {
	*runtime.Fake
	names []string
}

func (p *statusProvider) ListRunning(string) ([]string, error) { return p.names, nil }

type partialStatusProvider struct{ *statusProvider }

func (p *partialStatusProvider) StatusPartial() bool { return true }

type failedStatusProvider struct{ *statusProvider }

func (p *failedStatusProvider) ListRunning(string) ([]string, error) {
	return nil, errors.New("enumeration failed")
}

func TestRuntimeSessionsIncompleteProviderCannotProveInactive(t *testing.T) {
	p, records := statusSIDFixture(t)
	for _, provider := range []runtime.Provider{&partialStatusProvider{p}, &failedStatusProvider{p}, nil} {
		got, problems := ProjectRuntimeSessions(context.Background(), records, true, provider, nil, time.Now())
		if got.ProviderComplete || len(problems) == 0 {
			t.Fatal("partial provider admitted inactive rows")
		}
	}
}

func statusSIDFixture(t *testing.T) (*statusProvider, []NativeStatusIdentity) {
	t.Helper()
	p := &statusProvider{Fake: runtime.NewFake(), names: []string{"polecat-adhoc-1f196b09fa", "s-gcg-534258985155273443"}}
	records := []NativeStatusIdentity{{"adhoc", "tenders/gastown.polecat", "tenders/gastown.polecat-adhoc-1f196b09fa", p.names[0]}, {"active", "postman-out", "postman-out", p.names[1]}}
	for _, r := range records {
		_ = p.SetMeta(r.RuntimeName, "GC_SESSION_ID", r.ID)
		_ = p.SetMeta(r.RuntimeName, "GC_TEMPLATE", r.Template)
	}
	for i := 0; i < 5; i++ {
		s := string(rune('a' + i))
		records = append(records, NativeStatusIdentity{"asleep-" + s, "postman-out", "postman-out", "sleep-" + s})
	}
	return p, append(records, NativeStatusIdentity{ID: "never-launched", Template: "worker"})
}

func TestRuntimeSessionsSIDProjection(t *testing.T) {
	p, records := statusSIDFixture(t)
	now := time.Date(2026, 10, 7, 14, 50, 0, 0, time.UTC)
	got, problems := ProjectRuntimeSessions(context.Background(), records, true, p, nil, now)
	if !got.ProviderComplete || len(problems) != 0 || len(got.Sessions) != 8 || !got.ObservedAt.Equal(now) {
		t.Fatalf("incomplete SID projection: %v", problems)
	}
	live := 0
	for _, row := range got.Sessions {
		if row.Running {
			live++
		}
		if row.Provider != ProviderBoundaryType(p) {
			t.Fatal("provider boundary differs from observer")
		}
		if row.ID == "never-launched" && (row.RuntimeName != "" || row.Running) {
			t.Fatal("derived an inactive handle")
		}
	}
	if live != 2 {
		t.Fatalf("live SID count=%d", live)
	}
}

func TestRuntimeSessionsConflictsStayIncomplete(t *testing.T) {
	for _, name := range []string{"partial_store", "duplicate_sid", "duplicate_handle", "missing_raw_handle", "wrong_handle", "wrong_template", "live_closed_or_missing", "duplicate_live_handle", "duplicate_live_sid", "canceled"} {
		t.Run(name, func(t *testing.T) {
			p, records := statusSIDFixture(t)
			complete := true
			ctx := context.Background()
			switch name {
			case "partial_store":
				complete = false
			case "duplicate_sid":
				records = append(records, records[0])
			case "duplicate_handle":
				records[2].RuntimeName = records[0].RuntimeName
			case "missing_raw_handle":
				records[0].RuntimeName = ""
			case "wrong_handle":
				records[0].RuntimeName = "different"
			case "wrong_template":
				records[0].Template = "different"
			case "live_closed_or_missing":
				records = records[1:]
			case "duplicate_live_handle":
				p.names = append(p.names, p.names[0])
			case "duplicate_live_sid":
				_ = p.SetMeta(p.names[1], "GC_SESSION_ID", records[0].ID)
			case "canceled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			got, problems := ProjectRuntimeSessions(ctx, records, complete, p, nil, time.Now())
			if got.ProviderComplete || len(problems) == 0 {
				t.Fatal("ambiguity became complete")
			}
			for _, problem := range problems {
				if !strings.HasPrefix(problem, "runtime_sessions:") {
					t.Fatal("unbounded diagnostic")
				}
			}
		})
	}
}
