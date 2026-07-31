package cityartifact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CityPart is one part of a City artifact's content, as City holds it.
type CityPart struct {
	// Sequence is City's own 1-based part ordering. It is what contiguity is
	// judged on, and it is never sent as an identity.
	Sequence int
	// Bytes are the part's content. They travel on the content route and
	// nowhere else.
	Bytes []byte
	// MediaType is the part's own media type. It is normalized before it
	// leaves; an unparseable one is a refusal, not a passthrough.
	MediaType string
}

// CityArtifact is one artifact as City knows it, before any mapping.
//
// ArtifactID is City's own stable native ID. It is the dedup and checkpoint
// identity of this artifact and it is NEVER sent as the artifact's identity:
// the server mints that, and City reads it back.
type CityArtifact struct {
	ArtifactID string
	Kind       string
	MediaType  string
	// Version is City's monotonic version of this artifact's definition. A
	// bumped version is a new idempotency identity; an unbumped one replays.
	Version uint64

	// Links are the same-workspace resources this artifact attaches to. Run and
	// session IDs are server-minted neutral Team IDs City read off an earlier
	// response, not City-native run names.
	ProjectID string
	IssueID   string
	RunID     string
	SessionID string

	// City-shaped facts. They are breadcrumbs for City's own logs; the mapper
	// refuses to place any of them in an outbound body, and the create body has
	// no member that could hold one.
	City    string
	Rig     string
	Formula string

	// Parts is the content manifest. An artifact with no parts is legal: it
	// finalizes empty, exactly as a custom producer's would.
	Parts []CityPart
	// Complete is City's claim that the manifest is whole. Only a complete
	// artifact is finalized, and only a finalized artifact is readable.
	Complete bool
}

// Source is the enrolled producer identity this adapter uploads under. It is
// resolved from the credential at enrolment; this package treats it as read-only
// fact and never derives authority from a City name.
type Source struct {
	SourceID string
	Kind     string
	// Epoch is the declared reset generation. A bumped epoch restarts this
	// producer's checkpoint; it never rewrites what the server already accepted.
	Epoch uint64
	// Reset declares that this epoch is a reset of the checkpointed one. It is
	// required for an epoch advance to be honoured and is ignored otherwise.
	Reset *ResetDeclaration
}

// Mapper turns a City artifact into the closed request bodies of the frozen
// artifact operations, refusing every mapping that would let a City fact, a
// malformed link or a raw upstream field reach the server.
type Mapper struct {
	Source Source
}

const (
	defaultKind      = "file"
	defaultMediaType = "application/octet-stream"
)

// canonicalKinds is the server's closed kind vocabulary. City may not invent a
// kind, so an unmapped City kind lands on the default rather than traveling raw.
var canonicalKinds = []string{"file", "log", "report", "bundle", "diff", "trace"}

// cityKindMap folds City's own artifact vocabulary onto the closed one. It is a
// mapping, not a passthrough: a City kind with no entry becomes the default.
var cityKindMap = map[string]string{
	"transcript": "log",
	"logs":       "log",
	"review":     "report",
	"summary":    "report",
	"patch":      "diff",
	"workspace":  "bundle",
	"profile":    "trace",
}

var (
	// Neutral Team ID shapes. A run or session link is a server-minted ID City
	// read back off a response; anything else in that slot is a City-shaped
	// value trying to become a link authority.
	runIDPattern     = regexp.MustCompile(`^run_[0-9a-z]{16,}$`)
	sessionIDPattern = regexp.MustCompile(`^ses_[0-9a-z]{16,}$`)
	// cityLocatorPattern is a City-native locator: a scheme or a prefixed name
	// that only means something inside City.
	cityLocatorPattern = regexp.MustCompile(`(?i)^(gc://|city[:/]|rig[:/]|formula[:/])`)
)

// secretLocatorPatterns catch credential evidence and pre-signed locators in
// outbound strings. A signed URL is exactly as forbidden as a bearer token: both
// are ways to reach bytes outside the authorized route.
var secretLocatorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/-]{8,}`),
	regexp.MustCompile(`(?i)\bauthorization\s*:`),
	regexp.MustCompile(`(?i)x-amz-(signature|credential|security-token)`),
	regexp.MustCompile(`(?i)[?&](signature|sig|token|access_token|api_key)=`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bey[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
}

// SecretLocator reports whether a string carries credential evidence or a
// pre-signed locator.
func SecretLocator(s string) bool {
	for _, re := range secretLocatorPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func (m Mapper) validateSource() error {
	if strings.TrimSpace(m.Source.SourceID) == "" {
		return fmt.Errorf("%w: source id is required", ErrInvalidArtifact)
	}
	if strings.TrimSpace(m.Source.Kind) == "" {
		return fmt.Errorf("%w: source kind is required", ErrInvalidArtifact)
	}
	return nil
}

func validateNativeID(field, v string) error {
	switch {
	case strings.TrimSpace(v) == "":
		return fmt.Errorf("%w: %s is required", ErrInvalidArtifact, field)
	case len(v) > maxSourceNativeIDLen:
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidArtifact, field, maxSourceNativeIDLen)
	case strings.ContainsAny(v, " \t\r\n"):
		return fmt.Errorf("%w: %s contains whitespace", ErrInvalidArtifact, field)
	case SecretLocator(v):
		return fmt.Errorf("%w: %s", ErrCredentialLeak, field)
	}
	return nil
}

// MapLinks normalizes and validates the artifact's links.
//
// It runs before anything is created and therefore before any byte is uploaded:
// a link this adapter can already tell is malformed or City-shaped never reaches
// the server, and a link the server rejects as foreign fails the create, which
// is the only call that carries links. Either way no content leaves the process.
func (m Mapper) MapLinks(a CityArtifact) (Links, error) {
	l := Links{
		ProjectID: strings.TrimSpace(a.ProjectID),
		IssueID:   strings.TrimSpace(a.IssueID),
		RunID:     strings.TrimSpace(a.RunID),
		SessionID: strings.TrimSpace(a.SessionID),
	}
	cityFacts := []string{a.City, a.Rig, a.Formula}
	for field, v := range map[string]string{
		"links.project_id": l.ProjectID,
		"links.issue_id":   l.IssueID,
		"links.run_id":     l.RunID,
		"links.session_id": l.SessionID,
	} {
		if v == "" {
			continue
		}
		if err := validateLinkValue(field, v, cityFacts); err != nil {
			return Links{}, err
		}
	}
	if l.RunID != "" && !runIDPattern.MatchString(l.RunID) {
		return Links{}, fmt.Errorf("%w: links.run_id %q is not a server-minted run id", ErrCityAuthority, l.RunID)
	}
	if l.SessionID != "" && !sessionIDPattern.MatchString(l.SessionID) {
		return Links{}, fmt.Errorf("%w: links.session_id %q is not a server-minted session id", ErrCityAuthority, l.SessionID)
	}
	return l, nil
}

func validateLinkValue(field, v string, cityFacts []string) error {
	switch {
	case len(v) > maxLinkIDLen:
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidArtifact, field, maxLinkIDLen)
	case strings.ContainsAny(v, " \t\r\n"):
		return fmt.Errorf("%w: %s contains whitespace", ErrInvalidArtifact, field)
	case SecretLocator(v):
		return fmt.Errorf("%w: %s", ErrCredentialLeak, field)
	case cityLocatorPattern.MatchString(v):
		return fmt.Errorf("%w: %s is a City locator", ErrCityAuthority, field)
	}
	for _, fact := range cityFacts {
		if fact != "" && strings.EqualFold(fact, v) {
			return fmt.Errorf("%w: %s repeats a City name", ErrCityAuthority, field)
		}
	}
	return nil
}

// MapCreate builds the create body. The result carries a closed kind, a
// normalized media type and verified links, and nothing else — there is no slot
// for a City name, a producer, a status or an ID.
func (m Mapper) MapCreate(a CityArtifact) (CreateRequest, error) {
	if err := m.validateSource(); err != nil {
		return CreateRequest{}, err
	}
	if err := validateNativeID("artifact_id", a.ArtifactID); err != nil {
		return CreateRequest{}, err
	}
	links, err := m.MapLinks(a)
	if err != nil {
		return CreateRequest{}, err
	}
	mediaType := normalizeMediaType(a.MediaType)
	if mediaType == "" {
		return CreateRequest{}, fmt.Errorf("%w: media_type %q is not a media type", ErrInvalidArtifact, a.MediaType)
	}
	return CreateRequest{Kind: mapKind(a.Kind), MediaType: mediaType, Links: links}, nil
}

// MapPart builds one content part. Bytes are copied so a later mutation of the
// City slice cannot change what this adapter believes it acknowledged.
func (m Mapper) MapPart(a CityArtifact, p CityPart) (Part, error) {
	switch {
	case p.Sequence < 1:
		return Part{}, fmt.Errorf("%w: part sequence must be positive, got %d", ErrInvalidArtifact, p.Sequence)
	case len(p.Bytes) == 0:
		return Part{}, fmt.Errorf("%w: part %d is empty", ErrInvalidArtifact, p.Sequence)
	case len(p.Bytes) > maxPartBytes:
		return Part{}, fmt.Errorf("%w: part %d exceeds %d bytes", ErrInvalidArtifact, p.Sequence, maxPartBytes)
	}
	mediaType := normalizeMediaType(firstNonEmpty(p.MediaType, a.MediaType))
	if mediaType == "" {
		return Part{}, fmt.Errorf("%w: part %d media_type is not a media type", ErrInvalidArtifact, p.Sequence)
	}
	out := Part{Bytes: append([]byte(nil), p.Bytes...), MediaType: mediaType, Sequence: p.Sequence}
	return out, nil
}

// MapFinalize builds the finalize body. The asserted digest is taken over the
// manifest this adapter actually uploaded, in sequence order, so an assertion
// can only ever describe content that left this process.
func (m Mapper) MapFinalize(digest string) FinalizeRequest {
	return FinalizeRequest{Digest: digest}
}

func mapKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	if mapped, ok := cityKindMap[k]; ok {
		return mapped
	}
	for _, canonical := range canonicalKinds {
		if k == canonical {
			return k
		}
	}
	return defaultKind
}

// normalizeMediaType keeps a media type to its lowercased type/subtype, exactly
// as the server does. Parameters are dropped rather than echoed: a charset City
// sent is not a fact about the stored bytes, and an unbounded parameter string
// is one more place an upstream secret could ride.
func normalizeMediaType(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return defaultMediaType
	}
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	parts := strings.Split(v, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	for _, p := range parts {
		if strings.ContainsAny(p, " \t\r\n\"\\") {
			return ""
		}
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// forbiddenBodyFields are members no outbound artifact body may carry. Identity,
// provenance and state are server-derived; a body that tries to set one is a
// City fact reaching for authority. Locators are here for the same reason a
// signed URL is refused on the way in: an artifact API hands over content, never
// a way to fetch content elsewhere.
var forbiddenBodyFields = map[string]bool{
	"id":           true,
	"artifact_id":  true,
	"source_id":    true,
	"producer":     true,
	"status":       true,
	"digest":       true,
	"byte_size":    true,
	"created_at":   true,
	"updated_at":   true,
	"finalized_at": true,
	"display":      true,
	"city":         true,
	"rig":          true,
	"formula":      true,
	"token":        true,
	"url":          true,
	"signed_url":   true,
	"location":     true,
	"href":         true,
	"bucket":       true,
	"object_key":   true,
	"etag":         true,
}

// contentBearingFields are members that would put bytes inside a JSON body. No
// artifact body in this package has one: content travels as [Part.Bytes] to the
// content route and nowhere else, and [Part] does not serialize its bytes.
var contentBearingFields = map[string]bool{
	"bytes":   true,
	"content": true,
	"text":    true,
	"body":    true,
	"data":    true,
}

// ScanOutbound is the last gate before a body is handed to the client seam.
//
// It works on the encoded body rather than on the Go type, so a member added to
// a request struct later is scanned without anyone remembering to update a list
// of accessors. allowed names the members this particular body is entitled to
// carry despite the lists below — today that is the finalize body's asserted
// digest, which is the producer's claim about content it uploaded rather than a
// server-derived field being overwritten.
func ScanOutbound(body any, allowed ...string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cityartifact: scan encode: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("cityartifact: scan decode: %w", err)
	}
	permitted := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		permitted[k] = true
	}
	return scanObject(generic, permitted, "")
}

func scanObject(obj map[string]any, permitted map[string]bool, path string) error {
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
		if !permitted[k] {
			if forbiddenBodyFields[k] {
				return fmt.Errorf("%w: body carries %s", ErrCityAuthority, at)
			}
			if contentBearingFields[k] {
				return fmt.Errorf("%w: %s", ErrContentRoute, at)
			}
		}
		switch v := obj[k].(type) {
		case string:
			if SecretLocator(v) {
				return fmt.Errorf("%w: %s", ErrCredentialLeak, at)
			}
		case map[string]any:
			if err := scanObject(v, permitted, at); err != nil {
				return err
			}
		case []any:
			for i, item := range v {
				nested, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if err := scanObject(nested, permitted, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
