package tokenrender

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/connect"
)

// DefaultInterval is the periodic render interval (DES-CN D5): revocation
// lands within one interval plus bd serve's one-second reload.
const DefaultInterval = 15 * time.Second

// jitterFraction is Run's symmetric jitter around the interval.
const jitterFraction = 0.1

// RowSource lists the session rows that earn a bearer line. Each call reads
// live state, never a cache, so a render's revocation is as fresh as its
// read. Implementations filter with EligibleTokenRow.
type RowSource interface {
	EligibleRows(ctx context.Context) ([]connect.TokenRow, error)
}

// Output is one token file a renderer owns: the file a bd serve's
// --auth-token-file names, and optionally that serve's operator token
// source, whose lines are copied first. gc never writes the operator source.
type Output struct {
	// Ledger names the output in logs and errors (a [connect.ledgers] key).
	Ledger string
	// TokenFile is the rendered file: a clean absolute path.
	TokenFile string
	// OperatorFile is the operator's token source: empty for none, or a clean
	// absolute path. Configured but unreadable leaves the output as it is.
	OperatorFile string
}

// Config configures a Renderer.
type Config struct {
	// City is the MAC input every renderer of the city shares.
	City string
	// Key is the city key K, at least connect.MinKeyLen bytes.
	Key []byte
	// Outputs are the token files this renderer owns. None is valid: such a
	// renderer only reads rows, so Ensure still refuses an ineligible row.
	Outputs []Output
	// Rows lists the eligible session rows.
	Rows RowSource
	// Interval is Run's render interval; zero means DefaultInterval.
	Interval time.Duration
	// Clock stamps token-file mtimes; nil means the real clock.
	Clock clock.Clock
	// Logf receives one line per change of a render cause; nil discards.
	Logf func(format string, args ...any)
}

// OutputReport is one output's result in a render.
type OutputReport struct {
	Ledger    string
	TokenFile string
	// Seeded is set when the render wrote the seed into a file that held no
	// gc line, before reading rows.
	Seeded bool
	// Changed is set when the render's final write replaced the file.
	Changed bool
	// OperatorGCLike counts gc-like lines dropped from the operator source.
	OperatorGCLike int
	// Err is the output's *Error, or nil when the file is current.
	Err error

	content []byte // the file's bytes after the render, when Err is nil
}

// Report is the result of one render.
type Report struct {
	// Rows is the number of distinct eligible rows the render read.
	Rows int
	// DroppedRows counts rows from the source that no bearer can carry.
	DroppedRows int
	// RowsErr is the render_rows_unavailable *Error, or nil.
	RowsErr error
	Outputs []OutputReport

	seq  uint64 // the render's row-read sequence number
	rows []connect.TokenRow
}

// Err joins every failure in the report; nil means every output is current.
func (r Report) Err() error {
	errs := []error{r.RowsErr}
	for _, o := range r.Outputs {
		if !errors.Is(o.Err, ErrRowsUnavailable) {
			errs = append(errs, o.Err)
		}
	}
	return errors.Join(errs...)
}

// Renderer renders a city's token files (DES-CN D5, D15). Renders in one
// process are serialized and coalesced; renders across processes serialize
// on the outputs' directory locks.
type Renderer struct {
	cfg      Config
	sentinel string

	sem chan struct{} // one render at a time in this process

	mu      sync.Mutex
	readSeq uint64  // row reads begun
	last    *Report // the latest completed render
	causes  map[string]string

	onQueued func() // test hook: an Ensure has taken its ticket

	after  func(time.Duration) <-chan time.Time
	jitter func() float64 // uniform in [0, 1)
}

// New validates cfg and returns a Renderer.
func New(cfg Config) (*Renderer, error) {
	if !connect.ValidCityName(cfg.City) {
		return nil, fmt.Errorf("token renderer: city %q is not [a-zA-Z0-9][a-zA-Z0-9._-]*", cfg.City)
	}
	sentinel, err := connect.Sentinel(cfg.Key, cfg.City)
	if err != nil {
		return nil, fmt.Errorf("token renderer: %w", err)
	}
	if cfg.Rows == nil {
		return nil, errors.New("token renderer: no row source")
	}
	if cfg.Interval < 0 {
		return nil, fmt.Errorf("token renderer: interval %s is negative", cfg.Interval)
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := validateOutputs(cfg.Outputs); err != nil {
		return nil, fmt.Errorf("token renderer: %w", err)
	}
	cfg.Outputs = slices.Clone(cfg.Outputs)
	cfg.Key = slices.Clone(cfg.Key)
	return &Renderer{
		cfg:      cfg,
		sentinel: sentinel,
		sem:      make(chan struct{}, 1),
		causes:   map[string]string{},
		after:    time.After,
		jitter:   rand.Float64,
	}, nil
}

func validateOutputs(outputs []Output) error {
	ledgers := map[string]bool{}
	tokenFiles := map[string]bool{}
	for i, o := range outputs {
		switch {
		case o.Ledger == "":
			return fmt.Errorf("outputs[%d]: ledger is empty", i)
		case ledgers[o.Ledger]:
			return fmt.Errorf("outputs[%d]: ledger %q is listed twice", i, o.Ledger)
		case !cleanAbs(o.TokenFile):
			return fmt.Errorf("outputs[%d] (%s): token file is not a clean absolute path", i, o.Ledger)
		case tokenFiles[o.TokenFile]:
			return fmt.Errorf("outputs[%d] (%s): token file %s is listed twice", i, o.Ledger, o.TokenFile)
		case o.OperatorFile != "" && !cleanAbs(o.OperatorFile):
			return fmt.Errorf("outputs[%d] (%s): operator file is not a clean absolute path", i, o.Ledger)
		}
		ledgers[o.Ledger] = true
		tokenFiles[o.TokenFile] = true
	}
	for i, o := range outputs {
		if o.OperatorFile != "" && tokenFiles[o.OperatorFile] {
			return fmt.Errorf("outputs[%d] (%s): operator file %s is also a rendered token file", i, o.Ledger, o.OperatorFile)
		}
	}
	return nil
}

func cleanAbs(p string) bool {
	return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p
}

// RenderOnce runs one render: for each output it reads the operator source,
// inspects the file and writes the seed into one that holds no gc line; then
// it reads the rows once, renders and writes each output. The error joins
// every cause that left an output not current.
func (r *Renderer) RenderOnce(ctx context.Context) (Report, error) {
	if err := r.acquire(ctx); err != nil {
		return Report{}, err
	}
	defer r.release()
	rep := r.render(ctx)
	return rep, rep.Err()
}

// Ensure returns once a render whose row read began after this call has put
// row's bearer into every output, or with the reason it did not:
// render_row_ineligible when the row was not among the eligible rows (closed,
// ineligible, or at another generation), otherwise the cause that left an
// output without the line. Concurrent Ensure calls share one render.
func (r *Renderer) Ensure(ctx context.Context, row connect.TokenRow) error {
	bearer, err := connect.MintBearer(r.cfg.Key, r.cfg.City, row.SessionID, row.Generation)
	if err != nil {
		return &Error{Cause: CauseRowIneligible, Detail: "row cannot carry a bearer", Err: err}
	}
	r.mu.Lock()
	ticket := r.readSeq
	r.mu.Unlock()
	if r.onQueued != nil {
		r.onQueued()
	}

	if err := r.acquire(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	rep := r.last
	r.mu.Unlock()
	if rep == nil || rep.seq <= ticket {
		fresh := r.render(ctx)
		rep = &fresh
	}
	r.release()
	return checkEnsured(*rep, row, bearer)
}

func checkEnsured(rep Report, row connect.TokenRow, bearer string) error {
	if rep.RowsErr != nil {
		return rep.RowsErr
	}
	if !slices.Contains(rep.rows, row) {
		return &Error{Cause: CauseRowIneligible, Detail: fmt.Sprintf("session %q generation %d is not an eligible row", row.SessionID, row.Generation)}
	}
	var errs []error
	for _, o := range rep.Outputs {
		switch {
		case o.Err != nil:
			errs = append(errs, o.Err)
		case !connect.TokenFileHoldsLine(o.content, bearer):
			errs = append(errs, &Error{Cause: CauseRowIneligible, Ledger: o.Ledger, Detail: fmt.Sprintf("session %q generation %d is missing from the rendered file", row.SessionID, row.Generation)})
		}
	}
	return errors.Join(errs...)
}

// Run renders every Interval, with ±10% jitter, until ctx ends, and returns
// ctx's error. Each distinct cause is logged once until it changes or clears.
func (r *Renderer) Run(ctx context.Context) error {
	for {
		if _, err := r.RenderOnce(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.after(r.nextInterval()):
		}
	}
}

func (r *Renderer) nextInterval() time.Duration {
	spread := float64(r.cfg.Interval) * jitterFraction
	return r.cfg.Interval + time.Duration((r.jitter()*2-1)*spread)
}

func (r *Renderer) acquire(ctx context.Context) error {
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Renderer) release() { <-r.sem }

// outputState carries one output through a render.
type outputState struct {
	rep      OutputReport
	operator []string
}

// render runs one render with r.sem held, records it as the latest, and
// logs cause changes.
func (r *Renderer) render(ctx context.Context) Report {
	rep := r.renderLocked(ctx)
	r.mu.Lock()
	r.last = &rep
	r.mu.Unlock()
	r.logCauses(rep)
	return rep
}

func (r *Renderer) renderLocked(ctx context.Context) Report {
	states := make([]*outputState, len(r.cfg.Outputs))
	for i, o := range r.cfg.Outputs {
		states[i] = &outputState{rep: OutputReport{Ledger: o.Ledger, TokenFile: o.TokenFile}}
	}
	unlock, err := LockOutputs(r.cfg.Outputs)
	if err != nil {
		// The row read below still runs so Ensure can tell an ineligible row
		// from a lock failure; every output carries the failure.
		for _, s := range states {
			s.rep.Err = &Error{Cause: CauseWriteFailed, Ledger: s.rep.Ledger, Err: err}
		}
		unlock = func() {}
	}
	defer unlock()
	if err == nil {
		for i, o := range r.cfg.Outputs {
			r.prepare(o, states[i])
		}
	}

	r.mu.Lock()
	r.readSeq++
	rep := Report{seq: r.readSeq}
	r.mu.Unlock()

	rows, rowsErr := r.cfg.Rows.EligibleRows(ctx)
	if rowsErr != nil {
		rep.RowsErr = &Error{Cause: CauseRowsUnavailable, Err: rowsErr}
		for _, s := range states {
			if s.rep.Err == nil {
				s.rep.Err = &Error{Cause: CauseRowsUnavailable, Ledger: s.rep.Ledger, Err: rowsErr}
			}
		}
	} else {
		rep.rows, rep.DroppedRows = r.usableRows(rows)
		rep.Rows = len(rep.rows)
		for i, o := range r.cfg.Outputs {
			if states[i].rep.Err == nil {
				r.write(o, states[i], rep.rows)
			}
		}
	}
	for _, s := range states {
		rep.Outputs = append(rep.Outputs, s.rep)
	}
	return rep
}

// prepare reads the operator source and the current file, applies the
// refusals, and writes the seed into a file that holds no gc line.
func (r *Renderer) prepare(o Output, s *outputState) {
	if o.OperatorFile != "" {
		data, err := readCapped(o.OperatorFile, connect.MaxTokenFileBytes)
		if err != nil {
			s.rep.Err = &Error{Cause: CauseOperatorUnreadable, Ledger: o.Ledger, Err: err}
			return
		}
		lines, gcLike, err := connect.ParseOperatorLines(data)
		if err != nil {
			s.rep.Err = &Error{Cause: CauseOperatorUnreadable, Ledger: o.Ledger, Detail: o.OperatorFile, Err: err}
			return
		}
		s.operator, s.rep.OperatorGCLike = lines, gcLike
	}
	current, _, err := readTokenFile(o.TokenFile)
	if err != nil {
		s.rep.Err = &Error{Cause: CauseWriteFailed, Ledger: o.Ledger, Err: err}
		return
	}
	view := connect.InspectTokenFile(current)
	switch {
	case view.NewerFormat:
		s.rep.Err = &Error{Cause: CauseNewerFormat, Ledger: o.Ledger, Detail: o.TokenFile + " holds a gc line of a newer format"}
		return
	case view.GCLines > 0 && !slices.Contains(view.Sentinels, r.sentinel):
		s.rep.Err = &Error{Cause: CauseForeignSentinel, Ledger: o.Ledger, Detail: o.TokenFile + " holds gc lines without this city's and key's sentinel"}
		return
	case view.GCLines > 0:
		return
	}
	seed, err := connect.RenderTokenFile(s.operator, r.cfg.Key, r.cfg.City, nil)
	if err != nil {
		s.rep.Err = renderError(o.Ledger, err)
		return
	}
	changed, err := writeTokenFile(o.TokenFile, seed, r.cfg.Clock.Now)
	if err != nil {
		s.rep.Err = &Error{Cause: CauseWriteFailed, Ledger: o.Ledger, Err: err}
		return
	}
	s.rep.Seeded = changed
}

// write renders the output from the rows and replaces the file.
func (r *Renderer) write(o Output, s *outputState, rows []connect.TokenRow) {
	content, err := connect.RenderTokenFile(s.operator, r.cfg.Key, r.cfg.City, rows)
	if err != nil {
		s.rep.Err = renderError(o.Ledger, err)
		return
	}
	changed, err := writeTokenFile(o.TokenFile, content, r.cfg.Clock.Now)
	if err != nil {
		s.rep.Err = &Error{Cause: CauseWriteFailed, Ledger: o.Ledger, Err: err}
		return
	}
	s.rep.Changed = changed
	s.rep.content = content
}

func renderError(ledger string, err error) *Error {
	if errors.Is(err, connect.ErrTokenFileTooLarge) {
		return &Error{Cause: CauseTooLarge, Ledger: ledger, Err: err}
	}
	return &Error{Cause: CauseWriteFailed, Ledger: ledger, Err: err}
}

// usableRows drops rows no bearer can carry and duplicates, and sorts the
// rest. A source should never return such rows; one bad row must not block
// every other row's revocation, so it is dropped and counted instead.
func (r *Renderer) usableRows(rows []connect.TokenRow) ([]connect.TokenRow, int) {
	out := make([]connect.TokenRow, 0, len(rows))
	dropped := 0
	for _, row := range rows {
		if !connect.ValidSessionID(row.SessionID) || row.Generation < 1 {
			dropped++
			continue
		}
		out = append(out, row)
	}
	slices.SortFunc(out, connect.CompareTokenRows)
	return slices.Compact(out), dropped
}

// logCauses logs each output's and the row read's cause when it changes,
// including when it clears.
func (r *Renderer) logCauses(rep Report) {
	type entry struct{ key, label, now string }
	var entries []entry
	rowsNow := ""
	if rep.RowsErr != nil {
		rowsNow = rep.RowsErr.Error()
	}
	entries = append(entries, entry{"rows", "session rows", rowsNow})
	if rep.DroppedRows > 0 {
		entries = append(entries, entry{"dropped", "session rows", fmt.Sprintf("dropped %d rows no bearer can carry", rep.DroppedRows)})
	} else {
		entries = append(entries, entry{"dropped", "session rows", ""})
	}
	for _, o := range rep.Outputs {
		now := ""
		if o.Err != nil && !errors.Is(o.Err, ErrRowsUnavailable) {
			now = o.Err.Error()
		}
		entries = append(entries, entry{"out:" + o.TokenFile, "ledger " + o.Ledger, now})
		gcLike := ""
		if o.OperatorGCLike > 0 {
			gcLike = fmt.Sprintf("dropped %d gc-like lines from the operator token source; they are never copied", o.OperatorGCLike)
		}
		entries = append(entries, entry{"gclike:" + o.TokenFile, "ledger " + o.Ledger, gcLike})
	}
	r.mu.Lock()
	var lines []string
	for _, e := range entries {
		prev := r.causes[e.key]
		if prev == e.now {
			continue
		}
		r.causes[e.key] = e.now
		if e.now == "" {
			lines = append(lines, fmt.Sprintf("connect token render (%s, city %s): cleared", e.label, r.cfg.City))
		} else {
			lines = append(lines, fmt.Sprintf("connect token render (%s, city %s): %s", e.label, r.cfg.City, e.now))
		}
	}
	r.mu.Unlock()
	for _, line := range lines {
		r.cfg.Logf("%s", line)
	}
}

// OutputsHoldGCLines reports whether any output's token file holds a gc
// line: DES-CN D7's test before the controller may create a key. An absent
// file holds none; a file that cannot be read is an error. Hold LockOutputs
// across this check and the key's creation.
func OutputsHoldGCLines(outputs []Output) (bool, error) {
	for _, o := range outputs {
		current, _, err := readTokenFile(o.TokenFile)
		if err != nil {
			return false, err
		}
		if connect.InspectTokenFile(current).GCLines > 0 {
			return true, nil
		}
	}
	return false, nil
}
