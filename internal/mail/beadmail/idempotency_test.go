package beadmail

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

type failSecondCreateAtomicStore struct {
	*beads.SQLiteStore
}

type emptyPrefixAtomicStore struct {
	*beads.SQLiteStore
}

func (*emptyPrefixAtomicStore) IDPrefix() string { return "" }

func (s *failSecondCreateAtomicStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return s.SQLiteStore.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&failSecondCreateTx{Tx: tx})
	})
}

type failSecondCreateTx struct {
	beads.Tx
	creates int
}

func (tx *failSecondCreateTx) Create(bead beads.Bead) (beads.Bead, error) {
	tx.creates++
	if tx.creates == 2 {
		return beads.Bead{}, errors.New("injected message create failure")
	}
	return tx.Tx.Create(bead)
}

func openIdempotencySQLiteStore(t *testing.T) *beads.SQLiteStore {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store, ok := opened.(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("OpenSQLiteStore returned %T, want *beads.SQLiteStore", opened)
	}
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("CloseStore: %v", err)
		}
	})
	return store
}

func TestSendIdempotentRetryReturnsOriginalMessage(t *testing.T) {
	store := openIdempotencySQLiteStore(t)
	p := New(store)
	request := mail.IdempotentSendRequest{
		From:           "human",
		To:             "mayor",
		Subject:        "Review",
		Body:           "Please review PR 42",
		IdempotencyKey: "pr-42-review",
	}

	first, err := p.SendIdempotent(request)
	if err != nil {
		t.Fatalf("first SendIdempotent: %v", err)
	}
	second, err := p.SendIdempotent(request)
	if err != nil {
		t.Fatalf("retry SendIdempotent: %v", err)
	}
	if !first.Created || second.Created {
		t.Fatalf("Created = first %v, retry %v; want true, false", first.Created, second.Created)
	}
	if first.Message.ID == "" || second.Message.ID != first.Message.ID {
		t.Fatalf("message IDs = first %q, retry %q; want the same non-empty ID", first.Message.ID, second.Message.ID)
	}

	messages, err := store.List(beads.ListQuery{Type: messageBeadType, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != first.Message.ID {
		t.Fatalf("messages = %#v, want only %s", messages, first.Message.ID)
	}
	records, err := store.ListByMetadata(map[string]string{idempotencyRecordMetadataKey: idempotencyRecordMarker}, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("list idempotency records: %v", err)
	}
	if len(records) != 1 || records[0].Status != "closed" || records[0].Ephemeral || !records[0].NoHistory {
		t.Fatalf("idempotency records = %#v, want one closed durable no-history record", records)
	}
}

func TestSendIdempotentConflictingRequestFailsClosed(t *testing.T) {
	store := openIdempotencySQLiteStore(t)
	p := New(store)
	request := mail.IdempotentSendRequest{From: "human", To: "mayor", Subject: "Review", Body: "one", IdempotencyKey: "same-key"}
	if _, err := p.SendIdempotent(request); err != nil {
		t.Fatalf("first SendIdempotent: %v", err)
	}
	request.Body = "two"
	if _, err := p.SendIdempotent(request); !errors.Is(err, mail.ErrIdempotencyConflict) {
		t.Fatalf("conflicting SendIdempotent = %v, want ErrIdempotencyConflict", err)
	}

	messages, err := store.List(beads.ListQuery{Type: messageBeadType, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Description != "one" {
		t.Fatalf("messages after conflict = %#v, want only original body", messages)
	}
}

func TestSendIdempotentCorruptMessageMappingFailsClosed(t *testing.T) {
	store := openIdempotencySQLiteStore(t)
	p := New(store)
	request := mail.IdempotentSendRequest{From: "human", To: "mayor", Body: "one", IdempotencyKey: "corrupt-key"}
	first, err := p.SendIdempotent(request)
	if err != nil {
		t.Fatalf("first SendIdempotent: %v", err)
	}
	if err := store.SetMetadata(first.Message.ID, idempotencyRecordIDMetadataKey, "wrong-record"); err != nil {
		t.Fatalf("corrupt message mapping: %v", err)
	}
	if _, err := p.SendIdempotent(request); !errors.Is(err, mail.ErrIdempotencyRecordCorrupt) {
		t.Fatalf("retry with corrupt message mapping = %v, want ErrIdempotencyRecordCorrupt", err)
	}
	messages, err := store.List(beads.ListQuery{Type: messageBeadType, Status: "open", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != first.Message.ID {
		t.Fatalf("messages after corrupt retry = %#v, want only original %s", messages, first.Message.ID)
	}
}

func TestSendIdempotentRetryAfterMessageArchiveKeepsOriginalID(t *testing.T) {
	store := openIdempotencySQLiteStore(t)
	p := New(store)
	request := mail.IdempotentSendRequest{From: "human", To: "mayor", Body: "archive me", IdempotencyKey: "archive-retry"}
	first, err := p.SendIdempotent(request)
	if err != nil {
		t.Fatalf("first SendIdempotent: %v", err)
	}
	if err := p.Archive(first.Message.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	retry, err := p.SendIdempotent(request)
	if err != nil {
		t.Fatalf("retry after archive: %v", err)
	}
	if retry.Created || retry.Message.ID != first.Message.ID {
		t.Fatalf("retry after archive = %+v, want original ID %s without recreation", retry, first.Message.ID)
	}
	archived, err := store.Get(first.Message.ID)
	if err != nil || archived.Status != "closed" {
		t.Fatalf("archived original = (%+v, %v), want the original closed message", archived, err)
	}
	if err := store.Delete(first.Message.ID); err != nil {
		t.Fatalf("purge archived message: %v", err)
	}
	retry, err = p.SendIdempotent(request)
	if err != nil {
		t.Fatalf("retry after purge: %v", err)
	}
	if retry.Created || retry.Message.ID != first.Message.ID {
		t.Fatalf("retry after purge = %+v, want original ID %s without recreation", retry, first.Message.ID)
	}
	if _, err := store.Get(first.Message.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get message after purged retry = %v, want ErrNotFound", err)
	}
}

func TestSendIdempotentRollsBackRecordWhenMessageCreateFails(t *testing.T) {
	base := openIdempotencySQLiteStore(t)
	store := &failSecondCreateAtomicStore{SQLiteStore: base}
	p := New(store)
	request := mail.IdempotentSendRequest{From: "human", To: "mayor", Body: "must be atomic", IdempotencyKey: "rollback-key"}
	if _, err := p.SendIdempotent(request); err == nil || !strings.Contains(err.Error(), "injected message create failure") {
		t.Fatalf("SendIdempotent = %v, want injected failure", err)
	}
	items, err := base.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("atomic rollback left rows: %#v", items)
	}
}

func TestSendIdempotentConcurrentCallersCreateOneMessage(t *testing.T) {
	store := openIdempotencySQLiteStore(t)
	p := New(store)
	request := mail.IdempotentSendRequest{From: "human", To: "mayor", Subject: "Review", Body: "concurrent", IdempotencyKey: "concurrent-key"}

	const callers = 16
	start := make(chan struct{})
	results := make(chan mail.IdempotentSendResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := p.SendIdempotent(request)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SendIdempotent: %v", err)
		}
	}
	created := 0
	messageID := ""
	for result := range results {
		if result.Created {
			created++
		}
		if messageID == "" {
			messageID = result.Message.ID
		}
		if result.Message.ID != messageID {
			t.Fatalf("concurrent message ID = %q, want %q", result.Message.ID, messageID)
		}
	}
	if created != 1 {
		t.Fatalf("Created=true count = %d, want exactly 1", created)
	}
	messages, err := store.List(beads.ListQuery{Type: messageBeadType, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != messageID {
		t.Fatalf("messages = %#v, want only %s", messages, messageID)
	}
}

func TestSendIdempotentRejectsNonAtomicStoreBeforeWriting(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	_, err := p.SendIdempotent(mail.IdempotentSendRequest{From: "human", To: "mayor", Body: "no partial write", IdempotencyKey: "unsupported"})
	if !errors.Is(err, mail.ErrIdempotencyUnsupported) {
		t.Fatalf("SendIdempotent = %v, want ErrIdempotencyUnsupported", err)
	}
	items, listErr := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("non-atomic store contains %#v after refusal, want no writes", items)
	}
}

func TestSendIdempotentRejectsEmptyStoreNamespaceBeforeWriting(t *testing.T) {
	base := openIdempotencySQLiteStore(t)
	store := &emptyPrefixAtomicStore{SQLiteStore: base}
	p := New(store)
	_, err := p.SendIdempotent(mail.IdempotentSendRequest{From: "human", To: "mayor", Body: "no namespace", IdempotencyKey: "unsupported-prefix"})
	if !errors.Is(err, mail.ErrIdempotencyUnsupported) {
		t.Fatalf("SendIdempotent = %v, want ErrIdempotencyUnsupported", err)
	}
	items, listErr := base.List(beads.ListQuery{IncludeClosed: true, AllowScan: true, TierMode: beads.TierBoth})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("empty-namespace store contains %#v after refusal, want no writes", items)
	}
}
