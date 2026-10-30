package worker

import "sync"

// formulaNameMemoCap bounds the memo. Past the cap the memo is cleared rather
// than evicted piecemeal, which costs one re-read per live acting bead and
// keeps the structure trivial.
const formulaNameMemoCap = 1024

// FormulaNameMemo remembers the formula an acting bead's run was compiled
// from, keyed by the acting bead id. An acting bead's run root never changes,
// so a verdict stays valid for the life of the bead. An empty verdict for a
// root that was found but not compiled from a formula is cached too; a bead
// that does not exist is not a verdict and is not cached, since it may be
// reachable on a later lookup.
//
// Usage attribution runs on every controller tick and every prompt, and the
// API builds a fresh Factory per request, so the memo must outlive the
// Factory: a long-lived caller holds one and hands it to each Factory it
// builds through FactoryConfig.FormulaMemo.
//
// A nil memo is valid and derives on every call.
type FormulaNameMemo struct {
	mu      sync.Mutex
	entries map[string]string
}

// NewFormulaNameMemo returns an empty memo.
func NewFormulaNameMemo() *FormulaNameMemo {
	return &FormulaNameMemo{entries: make(map[string]string)}
}

// resolve returns the memoized formula for the acting bead id, or runs derive
// and memoizes its verdict. Failures are returned, never cached.
func (m *FormulaNameMemo) resolve(id string, derive func() (string, error)) (string, error) {
	if m == nil {
		return derive()
	}
	m.mu.Lock()
	name, ok := m.entries[id]
	m.mu.Unlock()
	if ok {
		return name, nil
	}
	name, err := derive()
	if err != nil {
		return name, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= formulaNameMemoCap {
		m.entries = make(map[string]string)
	}
	m.entries[id] = name
	return name, nil
}
