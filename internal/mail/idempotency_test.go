package mail

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateIdempotencyKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"invoice-2026-09-01",
		"worker/attempt:42",
		"ключ-42",
		strings.Repeat("x", MaxIdempotencyKeyBytes),
	} {
		if err := ValidateIdempotencyKey(key); err != nil {
			t.Errorf("ValidateIdempotencyKey(%q): %v", key, err)
		}
	}

	for _, key := range []string{
		"",
		" leading",
		"trailing ",
		"embedded\nnewline",
		strings.Repeat("x", MaxIdempotencyKeyBytes+1),
		string([]byte{0xff}),
	} {
		err := ValidateIdempotencyKey(key)
		if !errors.Is(err, ErrInvalidIdempotencyKey) {
			t.Errorf("ValidateIdempotencyKey(%q) = %v, want ErrInvalidIdempotencyKey", key, err)
		}
	}
}
