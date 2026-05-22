package extmsg

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestPublishRequestSnakeCaseWire pins the wire format of PublishRequest.
// Without explicit json tags, Go marshals fields as PascalCase and the
// adapter's snake_case decoder silently drops underscore-bearing fields
// (Go's case-insensitive match does not bridge the underscore boundary).
// This test fails loudly if the tags are removed or renamed.
func TestPublishRequestSnakeCaseWire(t *testing.T) {
	req := PublishRequest{
		Conversation: ConversationRef{
			Provider:       "slack",
			ConversationID: "C123",
			Kind:           ConversationRoom,
		},
		Text:             "hello",
		ReplyToMessageID: "1700000000.000100",
		IdempotencyKey:   "idem-xyz",
		Metadata:         map[string]string{"thread_ts": "1700000000.000100"},
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal(PublishRequest): %v", err)
	}

	mustContain := []string{
		`"conversation":`,
		`"text":"hello"`,
		`"reply_to_message_id":"1700000000.000100"`,
		`"idempotency_key":"idem-xyz"`,
		`"metadata":`,
	}
	for _, want := range mustContain {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("PublishRequest JSON missing %q\nfull body: %s", want, body)
		}
	}

	mustNotContain := []string{
		`"ReplyToMessageID"`,
		`"IdempotencyKey"`,
		`"Metadata"`,
		`"Text"`,
		`"Conversation"`,
	}
	for _, banned := range mustNotContain {
		if bytes.Contains(body, []byte(banned)) {
			t.Errorf("PublishRequest JSON contains PascalCase key %q (tags missing or wrong)\nfull body: %s", banned, body)
		}
	}
}

// TestWirePublishReceiptDecodesSnakeCase pins decode of an adapter
// /publish response into the wire-shaped intermediate type. This is
// the path HTTPAdapter.Publish takes — it must decode adapter
// snake_case correctly so that MessageID, FailureKind, RetryAfter, and
// Metadata round-trip with non-zero values. If the wire shim's tags
// regress, this test fails because each field's want-value is
// distinguishable from its zero value (the bug-mode the test is
// guarding against).
func TestWirePublishReceiptDecodesSnakeCase(t *testing.T) {
	// Shape an adapter would write (snake_case, all fields populated).
	adapterBody := []byte(`{
		"conversation": {"provider":"slack","conversation_id":"C123","kind":"room"},
		"message_id": "1700000000.000100",
		"delivered": true,
		"failure_kind": "rate_limited",
		"retry_after": 5000000000,
		"metadata": {"thread_ts":"1700000000.000100"}
	}`)

	var wire wirePublishReceipt
	if err := json.Unmarshal(adapterBody, &wire); err != nil {
		t.Fatalf("Unmarshal(wirePublishReceipt): %v", err)
	}
	receipt := wire.toPublishReceipt()

	if got, want := receipt.MessageID, "1700000000.000100"; got != want {
		t.Errorf("MessageID = %q, want %q (json tag missing — silent drop)", got, want)
	}
	if !receipt.Delivered {
		t.Errorf("Delivered = false, want true")
	}
	if got, want := receipt.FailureKind, PublishFailureRateLimited; got != want {
		t.Errorf("FailureKind = %q, want %q (json tag missing — silent drop)", got, want)
	}
	if got, want := receipt.RetryAfter, 5*time.Second; got != want {
		t.Errorf("RetryAfter = %v, want %v (json tag missing — silent drop)", got, want)
	}
	if got, want := receipt.Conversation.ConversationID, "C123"; got != want {
		t.Errorf("Conversation.ConversationID = %q, want %q", got, want)
	}
	if got, want := receipt.Metadata["thread_ts"], "1700000000.000100"; got != want {
		t.Errorf("Metadata[thread_ts] = %q, want %q (json tag missing — silent drop)", got, want)
	}
}

// TestPublishReceiptStaysPascalCaseOnAPISurface guards the Huma API
// contract: PublishReceipt is exposed as OutboundResult.Receipt at
// POST /extmsg/outbound, where the public schema (internal/api/openapi.json)
// advertises PascalCase keys. If someone tags PublishReceipt's fields
// in an attempt to "fix" the snake_case bug at the type level, this
// test fails because Go's json.Marshal of an untagged struct uses the
// field name verbatim.
func TestPublishReceiptStaysPascalCaseOnAPISurface(t *testing.T) {
	receipt := PublishReceipt{
		MessageID: "M1",
		Conversation: ConversationRef{
			Provider:       "slack",
			ConversationID: "C1",
			Kind:           ConversationRoom,
		},
		Delivered:   true,
		FailureKind: PublishFailureRateLimited,
		RetryAfter:  5 * time.Second,
		Metadata:    map[string]string{"k": "v"},
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("Marshal(PublishReceipt): %v", err)
	}
	mustContain := []string{
		`"MessageID":"M1"`,
		`"FailureKind":"rate_limited"`,
		`"RetryAfter":`,
		`"Delivered":true`,
	}
	for _, want := range mustContain {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("PublishReceipt JSON missing %q (Huma API surface contract drift)\nfull body: %s", want, body)
		}
	}
}
