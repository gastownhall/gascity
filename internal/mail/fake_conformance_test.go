package mail_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
)

var _ mail.Provider = (*mail.Fake)(nil)

func TestFakeConformance(t *testing.T) {
	mailtest.RunProviderTests(t, func(_ *testing.T) mail.Provider {
		return mail.NewFake()
	})
}

func TestFakeUsesSuppliedClockAndThreadIDs(t *testing.T) {
	times := []time.Time{
		time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 16, 10, 1, 0, 0, time.UTC),
		time.Date(2026, time.July, 16, 10, 2, 0, 0, time.UTC),
	}
	threadIDs := []string{"thread-first", "thread-second"}
	nextTime := 0
	nextThreadID := 0
	fake := mail.NewFakeWithOptions(mail.FakeOptions{
		Now: func() time.Time {
			value := times[nextTime]
			nextTime++
			return value
		},
		NewThreadID: func() string {
			value := threadIDs[nextThreadID]
			nextThreadID++
			return value
		},
	})

	first, err := fake.Send("alice", "bob", "first", "first body")
	if err != nil {
		t.Fatalf("Send first: %v", err)
	}
	reply, err := fake.Reply(first.ID, "bob", "reply", "reply body")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	second, err := fake.Send("alice", "bob", "second", "second body")
	if err != nil {
		t.Fatalf("Send second: %v", err)
	}

	if !first.CreatedAt.Equal(times[0]) || !reply.CreatedAt.Equal(times[1]) || !second.CreatedAt.Equal(times[2]) {
		t.Errorf("CreatedAt values = [%v %v %v], want %v", first.CreatedAt, reply.CreatedAt, second.CreatedAt, times)
	}
	if first.ThreadID != threadIDs[0] || reply.ThreadID != threadIDs[0] || second.ThreadID != threadIDs[1] {
		t.Errorf("ThreadID values = [%q %q %q], want [%q %q %q]", first.ThreadID, reply.ThreadID, second.ThreadID, threadIDs[0], threadIDs[0], threadIDs[1])
	}
}
