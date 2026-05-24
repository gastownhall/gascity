package api

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestMailMessagesFromGenList_Valid(t *testing.T) {
	createdAt := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	threadID := "thr-1"
	replyTo := "msg-0"
	prio := int64(2)
	cc := []string{"alice", "bob"}
	rig := "myrig"
	items := []genclient.Message{
		{
			Id:        "msg-1",
			From:      "alice",
			To:        "mayor",
			Subject:   "hi",
			Body:      "hello",
			CreatedAt: createdAt,
			Read:      false,
			ThreadId:  &threadID,
			ReplyTo:   &replyTo,
			Priority:  &prio,
			Cc:        &cc,
			Rig:       &rig,
		},
		{
			Id:        "msg-2",
			From:      "bob",
			To:        "mayor",
			Subject:   "re: hi",
			Body:      "world",
			CreatedAt: createdAt,
			Read:      true,
		},
	}
	body := &genclient.MailListBody{Items: &items, Total: int64(len(items))}

	got := mailMessagesFromGenList(body)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "msg-1" || got[0].From != "alice" || got[0].Subject != "hi" ||
		got[0].Body != "hello" || !got[0].CreatedAt.Equal(createdAt) || got[0].Read {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].ThreadID != "thr-1" || got[0].ReplyTo != "msg-0" || got[0].Priority != 2 ||
		got[0].Rig != "myrig" || len(got[0].CC) != 2 {
		t.Errorf("got[0] optional fields = %+v", got[0])
	}
	if got[1].ID != "msg-2" || !got[1].Read || got[1].ThreadID != "" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestMailMessagesFromGenList_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := mailMessagesFromGenList(nil)
		if got == nil {
			t.Fatal("want non-nil slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
	t.Run("nil items", func(t *testing.T) {
		body := &genclient.MailListBody{}
		got := mailMessagesFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
	t.Run("empty items slice", func(t *testing.T) {
		items := []genclient.Message{}
		body := &genclient.MailListBody{Items: &items}
		got := mailMessagesFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
}

func TestMailMessagesFromGenList_PartialMissingFields(t *testing.T) {
	// Messages with optional pointer fields absent must decode without
	// panicking to zero values on the destination struct.
	createdAt := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	items := []genclient.Message{
		{Id: "msg-1", From: "alice", To: "mayor", Subject: "hi", Body: "", CreatedAt: createdAt, Read: false},
	}
	body := &genclient.MailListBody{Items: &items, Total: 1}

	got := mailMessagesFromGenList(body)

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ThreadID != "" || got[0].ReplyTo != "" || got[0].Priority != 0 ||
		got[0].Rig != "" || got[0].CC != nil {
		t.Errorf("got[0] = %+v, want zero-value optionals", got[0])
	}
}

func TestMailCountFromGen_Valid(t *testing.T) {
	partial := true
	partialErrs := []string{"rig a: boom"}
	body := &genclient.MailCountOutputBody{
		Total:         5,
		Unread:        2,
		Partial:       &partial,
		PartialErrors: &partialErrs,
	}

	got := mailCountFromGen(body)

	if got.Total != 5 || got.Unread != 2 {
		t.Errorf("counts = (%d,%d), want (5,2)", got.Total, got.Unread)
	}
	if !got.Partial || len(got.PartialErrors) != 1 || got.PartialErrors[0] != "rig a: boom" {
		t.Errorf("partial surface = %+v", got)
	}
}

func TestMailCountFromGen_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := mailCountFromGen(nil)
		if got.Total != 0 || got.Unread != 0 || got.Partial || got.PartialErrors != nil {
			t.Errorf("got = %+v, want zero", got)
		}
	})
	t.Run("zero body", func(t *testing.T) {
		body := &genclient.MailCountOutputBody{Total: 0, Unread: 0}
		got := mailCountFromGen(body)
		if got.Total != 0 || got.Unread != 0 || got.Partial || got.PartialErrors != nil {
			t.Errorf("got = %+v, want zero", got)
		}
	})
}

func TestMailCountFromGen_PartialMissingFields(t *testing.T) {
	// A count body without partial/partial_errors pointers set must still
	// decode cleanly (nil pointers left as zero value, no panic).
	body := &genclient.MailCountOutputBody{Total: 3, Unread: 1}

	got := mailCountFromGen(body)

	if got.Total != 3 || got.Unread != 1 {
		t.Errorf("counts = (%d,%d), want (3,1)", got.Total, got.Unread)
	}
	if got.Partial || got.PartialErrors != nil {
		t.Errorf("partial defaults = (%v, %v), want (false, nil)", got.Partial, got.PartialErrors)
	}
}

func TestMailMessageFromGen_NilOptionals(t *testing.T) {
	createdAt := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	g := genclient.Message{
		Id:        "msg-1",
		From:      "alice",
		To:        "bob",
		Subject:   "",
		Body:      "",
		CreatedAt: createdAt,
		Read:      true,
	}

	got := mailMessageFromGen(g)

	if got.ID != "msg-1" || got.From != "alice" || got.To != "bob" || !got.Read {
		t.Errorf("got = %+v", got)
	}
	if got.ThreadID != "" || got.ReplyTo != "" || got.Rig != "" || got.Priority != 0 || got.CC != nil {
		t.Errorf("optional fields = %+v, want zero", got)
	}
}
