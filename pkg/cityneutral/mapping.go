package cityneutral

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// CityRun is the City-native run as this adapter reads it. Every field here is
// source-native: none of it is authority on the neutral side, and the neutral
// Team ID is absent by construction — there is no field on this struct that
// could carry one.
type CityRun struct {
	// RunID is City's own stable run identifier. It becomes source_entity_id,
	// which is a source-native ID, NOT a neutral one.
	RunID   string
	Status  string
	Started time.Time
	Ended   *time.Time
	// ProjectID and IssueID are source-native scope refs.
	ProjectID string
	IssueID   string
	// Version is City's monotonic revision of this run. It drives
	// source_version, so a re-read that changed nothing sends nothing new.
	Version uint64
	// Rig, Formula and City are City-shaped facts. They are display-only and
	// gated: with DisplayPolicy off they never leave the process, and they can
	// never reach an identity, a scope or a status field. Enforced by
	// [Mapper.MapRun] and asserted by the neutral-authority tests.
	Rig     string
	Formula string
	City    string
}

// CitySession is one City session under a run.
type CitySession struct {
	SessionID string
	Status    string
	Started   time.Time
	Ended     *time.Time
	Version   uint64
	// Complete says City considers this session's input closed. It is what
	// makes the adapter finalize, and it is one-way: a later chain that flips
	// it back does not un-finalize anything.
	Complete bool
	Records  []CityRecord
}

// CityRecord is one City transcript message.
type CityRecord struct {
	// MessageID is City's stable per-message identity. Together with the
	// session it is the dedup key: the same MessageID with the same payload is
	// a replay, and with a different payload it is a refusal.
	MessageID string
	// Ordinal is City's position within the session, 1-based. Contiguity is
	// checked on it, so a source that cannot produce one cannot use this
	// adapter's ordering guarantees.
	Ordinal uint64
	Role    string
	// Author is the represented author: who spoke. Never the uploader, never
	// the source, never an authorization input.
	Author *ContributorRef
	At     time.Time
	// Text is raw transcript content. It is only ever serialized onto the
	// transcript-record route; [ScanOutbound] refuses it anywhere else.
	Text string
	// ContentRef is a reference to content held elsewhere, for records whose
	// body this producer is not authorized to ship.
	ContentRef string
	Version    uint64
}

// CityChain is one run and its sessions, the unit [Producer.Push] operates on.
type CityChain struct {
	Run      CityRun
	Sessions []CitySession
}

// Source identifies this producer to the mapper. SourceID is the enrolled
// producer identity the server already knows; it is echoed back on reads as
// provenance and is NOT a credential.
type Source struct {
	SourceID string
	// Kind is the source class, e.g. "city". Used in the idempotency preimage
	// so two producers with the same native IDs cannot collide.
	Kind string
	// Epoch is the frozen source epoch. It advances only on a declared reset,
	// never on a restart, and the adapter refuses to guess one.
	Epoch uint64
}

// Mapper turns a City chain into neutral request bodies.
//
// It holds no transport and no state: give it the same chain twice and it
// produces byte-identical bodies, which is what makes the stable-identity
// digest meaningful.
type Mapper struct {
	Source Source
	// AllowDisplayContent gates free-form display egress (titles built from
	// City rig/formula names). Default off: an ungated producer would leak
	// tenant-shaped strings into a neutral record on nothing but a code path.
	AllowDisplayContent bool
	// AllowRawContent gates shipping message text at all. Off means every
	// record goes as a reference, which is the mode a City with no signed
	// content policy runs in.
	AllowRawContent bool
}

// statusMap is the City→neutral status projection. It is closed on the neutral
// side: an unrecognized City status becomes StatusUnknown rather than being
// passed through, because the neutral vocabulary is not City's to extend.
var statusMap = map[string]Status{
	"pending":   StatusPending,
	"queued":    StatusPending,
	"running":   StatusRunning,
	"active":    StatusRunning,
	"succeeded": StatusSucceeded,
	"done":      StatusSucceeded,
	"failed":    StatusFailed,
	"error":     StatusFailed,
	"cancelled": StatusCancelled, //nolint:misspell // frozen wire value of the neutral contract
	"canceled":  StatusCancelled,
}

// roleMap is the City→neutral role projection, closed the same way.
var roleMap = map[string]Role{
	"user":      RoleUser,
	"human":     RoleUser,
	"assistant": RoleAssistant,
	"agent":     RoleAssistant,
	"system":    RoleSystem,
	"tool":      RoleTool,
}

var contributorKinds = map[string]bool{
	"human": true, "agent": true, "worker": true, "model": true, "system": true,
}

// secretLocatorPatterns mirror the server's refusal list. Screening at the
// producer is not redundant: it turns a leak into a local error with a source
// field name attached, instead of a 4xx an operator has to correlate.
var secretLocatorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^arn:aws:secretsmanager:`),
	regexp.MustCompile(`(?i)^arn:aws:kms:`),
	regexp.MustCompile(`(?i)^(vault|secret)://`),
	regexp.MustCompile(`(?i)^projects/[^/]+/secrets/`),
	regexp.MustCompile(`(?i)\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\b(sk|rk)-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`(?i)^(gh[pousr]|xox[baprs])_[A-Za-z0-9]{10,}`),
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)[?&](access_token|api_key|apikey|secret|password|signature)=`),
	regexp.MustCompile(`(?i)^(bearer|basic) [A-Za-z0-9+/._-]{16,}`),
}

// SecretLocator reports whether s carries credential evidence.
func SecretLocator(s string) bool {
	for _, re := range secretLocatorPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func validateNativeID(field, v string) error {
	switch {
	case strings.TrimSpace(v) == "":
		return fmt.Errorf("%w: %s is required", ErrInvalidChain, field)
	case len(v) > maxSourceNativeIDLen:
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidChain, field, maxSourceNativeIDLen)
	case strings.ContainsAny(v, "\x00\n\r"):
		return fmt.Errorf("%w: %s carries a control character", ErrInvalidChain, field)
	case SecretLocator(v):
		return fmt.Errorf("%w: %s", ErrCredentialLeak, field)
	}
	return nil
}

func (m Mapper) validateSource() error {
	if err := validateNativeID("source.source_id", m.Source.SourceID); err != nil {
		return err
	}
	if strings.TrimSpace(m.Source.Kind) == "" {
		return fmt.Errorf("%w: source.kind is required", ErrInvalidChain)
	}
	if m.Source.Epoch == 0 {
		return fmt.Errorf("%w: source.epoch is required and starts at 1", ErrInvalidChain)
	}
	return nil
}

// display builds the gated display title. City facts reach a neutral record
// only through here, only as a title, and only with the gate open.
func (m Mapper) display(parts ...string) string {
	if !m.AllowDisplayContent {
		return ""
	}
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || SecretLocator(p) {
			continue
		}
		kept = append(kept, p)
	}
	title := strings.Join(kept, " / ")
	if len(title) > maxDisplayTitleLength {
		title = title[:maxDisplayTitleLength]
	}
	return title
}

// MapRun projects a City run onto the neutral upsert body.
//
// The neutral-authority rule lives here: rig, formula and city name are
// arguments to [Mapper.display] and to nothing else. source_entity_id is the
// City run ID and project/issue are City's own scope refs; none of them is a
// neutral ID, and the neutral ID is not an input to this function at all.
func (m Mapper) MapRun(run CityRun) (Upsert, error) {
	if err := m.validateSource(); err != nil {
		return Upsert{}, err
	}
	if err := validateNativeID("run.run_id", run.RunID); err != nil {
		return Upsert{}, err
	}
	if run.Version == 0 {
		return Upsert{}, fmt.Errorf("%w: run.version is required and starts at 1", ErrInvalidChain)
	}
	if run.Started.IsZero() {
		return Upsert{}, fmt.Errorf("%w: run.started is required", ErrInvalidChain)
	}
	for field, v := range map[string]string{"run.project_id": run.ProjectID, "run.issue_id": run.IssueID} {
		if v == "" {
			continue
		}
		if err := validateNativeID(field, v); err != nil {
			return Upsert{}, err
		}
	}
	// A City fact that has managed to look like a neutral Team ID is still not
	// one, and the cheapest place to say so is before it is serialized.
	for field, v := range map[string]string{"run.rig": run.Rig, "run.formula": run.Formula, "run.city": run.City} {
		if looksNeutralID(v) {
			return Upsert{}, fmt.Errorf("%w: %s", ErrNeutralAuthority, field)
		}
	}
	up := Upsert{
		SourceEntityID: run.RunID,
		SourceVersion:  run.Version,
		Epoch:          m.Source.Epoch,
		Status:         mapStatus(run.Status),
		Lifecycle:      LifecyclePartial,
		StartedAt:      run.Started.UTC(),
		ProjectID:      run.ProjectID,
		IssueID:        run.IssueID,
		Title:          m.display(run.Rig, run.Formula),
		Coverage:       CoverageKnown,
	}
	if run.Ended != nil {
		e := run.Ended.UTC()
		if e.Before(up.StartedAt) {
			return Upsert{}, fmt.Errorf("%w: run.ended precedes run.started", ErrInvalidChain)
		}
		up.EndedAt = &e
	}
	return up, nil
}

// MapSession projects a City session onto the neutral upsert body. The run it
// belongs to is not in the body: it comes from the path, so a session can never
// claim a run the credential does not reach.
func (m Mapper) MapSession(sess CitySession) (Upsert, error) {
	if err := m.validateSource(); err != nil {
		return Upsert{}, err
	}
	if err := validateNativeID("session.session_id", sess.SessionID); err != nil {
		return Upsert{}, err
	}
	if sess.Version == 0 {
		return Upsert{}, fmt.Errorf("%w: session.version is required and starts at 1", ErrInvalidChain)
	}
	if sess.Started.IsZero() {
		return Upsert{}, fmt.Errorf("%w: session.started is required", ErrInvalidChain)
	}
	up := Upsert{
		SourceEntityID: sess.SessionID,
		SourceVersion:  sess.Version,
		Epoch:          m.Source.Epoch,
		Status:         mapStatus(sess.Status),
		Lifecycle:      LifecyclePartial,
		StartedAt:      sess.Started.UTC(),
		Coverage:       CoverageKnown,
	}
	if sess.Ended != nil {
		e := sess.Ended.UTC()
		if e.Before(up.StartedAt) {
			return Upsert{}, fmt.Errorf("%w: session.ended precedes session.started", ErrInvalidChain)
		}
		up.EndedAt = &e
	}
	return up, nil
}

// MapRecord projects a City message onto the transcript ingest body.
//
// Author is the represented author and travels as attribution. The uploader is
// the credential the request rides on and appears nowhere in this body; the
// source is [Source.SourceID] and is likewise server-side provenance, not a
// body field. Keeping all three apart is the whole point of this function.
func (m Mapper) MapRecord(rec CityRecord) (TranscriptRecordIngest, error) {
	if err := m.validateSource(); err != nil {
		return TranscriptRecordIngest{}, err
	}
	if err := validateNativeID("record.message_id", rec.MessageID); err != nil {
		return TranscriptRecordIngest{}, err
	}
	if rec.Ordinal == 0 {
		return TranscriptRecordIngest{}, fmt.Errorf("%w: record.ordinal is required and starts at 1", ErrInvalidChain)
	}
	if rec.At.IsZero() {
		return TranscriptRecordIngest{}, fmt.Errorf("%w: record.at is required", ErrInvalidChain)
	}
	version := rec.Version
	if version == 0 {
		version = 1
	}
	ord := rec.Ordinal
	in := TranscriptRecordIngest{
		SourceMessageID: rec.MessageID,
		SourceVersion:   version,
		Epoch:           m.Source.Epoch,
		Ordinal:         &ord,
		Role:            mapRole(rec.Role),
		OccurredAt:      rec.At.UTC(),
		Coverage:        CoverageKnown,
	}
	if rec.Author != nil {
		if !contributorKinds[rec.Author.Kind] {
			return TranscriptRecordIngest{}, fmt.Errorf("%w: unknown author kind %q", ErrInvalidChain, rec.Author.Kind)
		}
		if err := validateNativeID("record.author.ref", rec.Author.Ref); err != nil {
			return TranscriptRecordIngest{}, err
		}
		author := *rec.Author
		in.Author = &author
	}
	switch {
	case m.AllowRawContent && rec.Text != "":
		if SecretLocator(rec.Text) {
			return TranscriptRecordIngest{}, fmt.Errorf("%w: record.text", ErrCredentialLeak)
		}
		in.Text = rec.Text
	case rec.ContentRef != "":
		if err := validateNativeID("record.content_ref", rec.ContentRef); err != nil {
			return TranscriptRecordIngest{}, err
		}
		in.ContentRef = rec.ContentRef
		in.Coverage = CoveragePartial
	case rec.Text != "":
		// Content exists but this producer is not authorized to ship it. The
		// record still goes, because a transcript with a known-withheld turn is
		// truer than a transcript with a missing one, and `unavailable` is the
		// server's word for exactly that.
		in.Coverage = CoverageUnavailable
	}
	return in, nil
}

func mapStatus(s string) Status {
	if v, ok := statusMap[strings.ToLower(strings.TrimSpace(s))]; ok {
		return v
	}
	return StatusUnknown
}

func mapRole(r string) Role {
	if v, ok := roleMap[strings.ToLower(strings.TrimSpace(r))]; ok {
		return v
	}
	return RoleUnknown
}

// neutralIDPattern is the server's Team ID shape. City strings that match it
// are refused where they would be mistaken for one.
var neutralIDPattern = regexp.MustCompile(`^(run|ses|rec)_[0-9a-z]{16,}$`)

func looksNeutralID(v string) bool { return neutralIDPattern.MatchString(strings.TrimSpace(v)) }

// forbiddenBodyFields are members no request body of this adapter may carry.
// They are the fields that would let a producer assert something only the
// server may decide: ownership, the uploader, the tenant, or the neutral ID.
var forbiddenBodyFields = map[string]bool{
	"id": true, "run_id": true, "session_id": true, "record_id": true,
	"uploaded_by": true, "uploader": true, "uploader_id": true,
	"tenant": true, "tenant_id": true, "workspace": true, "workspace_id": true,
	"source_id": true, "key_id": true, "actor": true, "actor_id": true,
	"principal": true, "token": true, "authorization": true, "credential": true,
}

// ScanOutbound is the provenance-and-content scanner: it walks the serialized
// form of a body about to be sent and refuses it if the bytes carry credential
// evidence, an ownership assertion, or raw content on a route not authorized to
// carry it.
//
// allowContent is true only for the transcript-record route. Scanning the
// serialized form rather than the struct is deliberate: it is the bytes that
// leave, and a future field added to a DTO is covered without anyone
// remembering to update this function.
func ScanOutbound(body any, allowContent bool) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cityneutral: scan encode: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("cityneutral: scan decode: %w", err)
	}
	return scanObject(generic, allowContent, "")
}

func scanObject(obj map[string]any, allowContent bool, path string) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		at := k
		if path != "" {
			at = path + "." + k
		}
		if forbiddenBodyFields[k] {
			return fmt.Errorf("%w: body carries %s", ErrNeutralAuthority, at)
		}
		if (k == "text" || k == "content" || k == "body") && !allowContent {
			return fmt.Errorf("%w: %s", ErrContentRoute, at)
		}
		switch v := obj[k].(type) {
		case string:
			if SecretLocator(v) {
				return fmt.Errorf("%w: %s", ErrCredentialLeak, at)
			}
		case map[string]any:
			if err := scanObject(v, allowContent, at); err != nil {
				return err
			}
		case []any:
			for i, item := range v {
				nested, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if err := scanObject(nested, allowContent, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
