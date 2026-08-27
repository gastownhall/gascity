package beads

import (
	"strings"
	"testing"
)

// sideEffectWriteWrapper is the shape ConditionalWritesCapabilityTargeter
// exists for: a wrapper that performs the fenced write itself, because the
// wrapping exists for something that has to happen ON the write — appending an
// event, in the one production instance — and therefore forwards the whole
// trio. Forwarding the trio is exactly what makes its own capability answer
// vacuous.
type sideEffectWriteWrapper struct {
	Store
	wrote bool
}

func (w *sideEffectWriteWrapper) UpdateIfMatch(id string, revision int64, opts UpdateOpts) error {
	writer, ok := ConditionalWriterFor(w.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	w.wrote = true
	return writer.UpdateIfMatch(id, revision, opts)
}

func (w *sideEffectWriteWrapper) CloseIfMatch(id string, revision int64) error {
	writer, ok := ConditionalWriterFor(w.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	w.wrote = true
	return writer.CloseIfMatch(id, revision)
}

func (w *sideEffectWriteWrapper) DeleteIfMatch(id string, revision int64) error {
	writer, ok := ConditionalWriterFor(w.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	w.wrote = true
	return writer.DeleteIfMatch(id, revision)
}

func (w *sideEffectWriteWrapper) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := ConditionalWriterFor(w.Store)
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	w.wrote = true
	return writer.CompareAndSetMetadataKey(id, key, expected, next)
}

func (w *sideEffectWriteWrapper) ConditionalWritesCapabilityTarget() Store { return w.Store }

// TestCapabilityTargetIsAskedInsteadOfTheWrapper is the defect the seam closes.
// Without it a wrapper answers "yes, I implement conditional writes" about a
// backing that cannot perform one, and every surface that asks — the boot
// requirement, the status inspection — reports a capability nobody has.
func TestCapabilityTargetIsAskedInsteadOfTheWrapper(t *testing.T) {
	backing := NewMemStore()
	backing.DisableConditionalWrites = true
	wrapper := &sideEffectWriteWrapper{Store: backing}

	if _, err := RequiredConditionalWriter(backing); err == nil {
		t.Fatal("control: the bare incapable store satisfied the requirement, so nothing below discriminates")
	}

	_, err := RequiredConditionalWriter(wrapper)
	if err == nil {
		t.Fatal("the wrapper answered for a backing that cannot fence")
	}
	if !strings.Contains(err.Error(), "MemStore") {
		t.Errorf("the refusal names the wrapper rather than the store that cannot fence: %v", err)
	}

	insp := InspectConditionalWrites(wrapper)
	if insp.Capable {
		t.Errorf("inspection through the wrapper reported capable: %+v", insp)
	}
	if insp.StoreKind != "MemStore" {
		t.Errorf("StoreKind = %q, want the backing that holds the answer", insp.StoreKind)
	}
}

// TestCapabilityTargetDoesNotRedirectTheWriter is why this is not the resolve
// targeter. Pointing RESOLUTION down hands callers the bare engine, and a
// wrapper that exists for a write-path side effect stops having one — silently,
// on exactly the fenced writes that matter most.
func TestCapabilityTargetDoesNotRedirectTheWriter(t *testing.T) {
	backing := NewMemStore()
	created, err := backing.Create(Bead{Title: "fenced", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	wrapper := &sideEffectWriteWrapper{Store: backing}

	writer, err := RequiredConditionalWriter(wrapper)
	if err != nil {
		t.Fatalf("a capable backing was refused through the wrapper: %v", err)
	}
	title := "healed"
	if err := writer.UpdateIfMatch(created.ID, created.Revision, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("fenced update: %v", err)
	}
	if !wrapper.wrote {
		t.Fatal("the required writer bypassed the wrapper: its write-path side effect never ran")
	}
	if _, ok := AtomicConditionalCloserFor(wrapper); ok {
		t.Error("the wrapper advertises an atomic closer it does not implement")
	}
}
