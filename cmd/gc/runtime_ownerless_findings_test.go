package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ownerlessFindingsProvider can make GC_SESSION_ID unreadable during a scan
// and records the follow-up runtime listing.
type ownerlessFindingsProvider struct {
	*ownerlessFake
	ownerIDErrors     map[string]error
	disappearOnIDRead map[string]bool
	listCalls         int
}

func (p *ownerlessFindingsProvider) GetMeta(name, key string) (string, error) {
	if key == "GC_SESSION_ID" {
		if p.disappearOnIDRead[name] {
			delete(p.visible, name)
			delete(p.live, name)
			delete(p.dead, name)
			return "", runtime.ErrSessionNotFound
		}
		if err := p.ownerIDErrors[name]; err != nil {
			return "", err
		}
	}
	return p.ownerlessFake.GetMeta(name, key)
}

func (p *ownerlessFindingsProvider) ListRunning(prefix string) ([]string, error) {
	p.listCalls++
	return p.ownerlessFake.ListRunning(prefix)
}

func TestOwnerlessRuntimeFindingsSkipRuntimeGoneDuringIDRead(t *testing.T) {
	c := newOwnerlessCase(t)
	delete(c.sp.visible, "named-1")
	c.sp.visible["restarting"] = true
	sp := &ownerlessFindingsProvider{
		ownerlessFake:     c.sp,
		disappearOnIDRead: map[string]bool{"restarting": true},
	}

	findings, err := ownerlessRuntimeFindings(c.owners(), sp, c.city, ownerlessT0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings %q, want no finding for a runtime gone on re-list", findings)
	}
	if sp.listCalls != 2 {
		t.Fatalf("ListRunning called %d times, want initial list and presence re-check", sp.listCalls)
	}
}

func TestOwnerlessRuntimeFindingsSkipYoungUnreadableOrMissingID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ownerIDErr error
		created    time.Time
	}{
		{name: "unreadable", ownerIDErr: errors.New("metadata temporarily unavailable"), created: ownerlessT0.Add(-30 * time.Second)},
		{name: "missing", created: ownerlessT0.Add(-30 * time.Second)},
		{name: "unknown age"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newOwnerlessCase(t)
			delete(c.sp.visible, "named-1")
			c.sp.visible[tc.name] = true
			c.sp.dead[tc.name] = true
			if !tc.created.IsZero() {
				c.sp.created[tc.name] = tc.created
			}
			sp := &ownerlessFindingsProvider{
				ownerlessFake: c.sp,
				ownerIDErrors: map[string]error{tc.name: tc.ownerIDErr},
			}

			findings, err := ownerlessRuntimeFindings(c.owners(), sp, c.city, ownerlessT0, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 0 {
				t.Fatalf("findings %q, want no finding during the startup grace", findings)
			}
		})
	}
}
