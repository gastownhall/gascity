package productmetrics

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func secondFixedEvent() Event {
	event := fixedEvent()
	event.EventID = "123e4567-e89b-42d3-a456-426614174000"
	event.OS = OSDarwin
	event.OccurredHourUTC = "2026-07-11T01:00:00Z"
	event.CommandID = CommandVersion
	return event
}

func encodedEventForUpload(t *testing.T, event Event) []byte {
	t.Helper()
	encoded, err := EncodeEvent(event)
	if err != nil {
		t.Fatalf("EncodeEvent: %v", err)
	}
	return encoded
}

func TestBuildUploadBatchPreservesCanonicalQueueEvents(t *testing.T) {
	first := encodedEventForUpload(t, fixedEvent())
	secondEvent := secondFixedEvent()
	second := encodedEventForUpload(t, secondEvent)
	claims := []claimedEventFile{
		{name: fixedEvent().EventID, body: first},
		{name: secondEvent.EventID, body: second},
	}

	prepared, err := buildUploadBatch(claims, uploadBatchIdentity{
		installationID: fixedEvent().InstallationID,
		releaseVersion: fixedEvent().ReleaseVersion,
	})
	if err != nil {
		t.Fatalf("buildUploadBatch: %v", err)
	}
	wantBody := `{"schema_version":1,"events":[` + string(first) + `,` + string(second) + `]}`
	if got := string(prepared.body); got != wantBody {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, wantBody)
	}
	if got, want := prepared.eventIDs, []string{fixedEvent().EventID, secondEvent.EventID}; !equalStrings(got, want) {
		t.Fatalf("event IDs = %#v, want %#v", got, want)
	}
	if prepared.releaseVersion != fixedEvent().ReleaseVersion {
		t.Fatalf("release version = %q, want %q", prepared.releaseVersion, fixedEvent().ReleaseVersion)
	}

	// Prepared bytes and IDs must not alias caller-owned buffers. S6 holds this
	// value across lock release and the HTTP attempt.
	claims[0].body[0] = '!'
	claims[0].name = secondEvent.EventID
	if got := string(prepared.body); got != wantBody {
		t.Fatal("prepared request body aliases caller storage")
	}
	if prepared.eventIDs[0] != fixedEvent().EventID {
		t.Fatal("prepared event IDs alias caller storage")
	}
}

func TestBuildUploadBatchRejectsPoisonAndIdentityMismatch(t *testing.T) {
	event := fixedEvent()
	canonical := encodedEventForUpload(t, event)
	identity := uploadBatchIdentity{
		installationID: event.InstallationID,
		releaseVersion: event.ReleaseVersion,
	}
	valid := []claimedEventFile{{name: event.EventID, body: canonical}}

	tooMany := make([]claimedEventFile, MaxBatchEvents+1)
	for i := range tooMany {
		copyEvent := event
		copyEvent.EventID = fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
		tooMany[i] = claimedEventFile{name: copyEvent.EventID, body: encodedEventForUpload(t, copyEvent)}
	}

	wrongEventID := event
	wrongEventID.EventID = secondFixedEvent().EventID
	wrongInstallation := event
	wrongInstallation.InstallationID = "00000000-0000-4000-8000-000000000001"
	wrongRelease := event
	wrongRelease.ReleaseVersion = "0.32.0"
	nonCanonical := append([]byte(" \n"), canonical...)
	oversized := append(append([]byte(nil), canonical...), bytes.Repeat([]byte(" "), maxEventFileBytes-len(canonical)+1)...)

	tests := map[string]struct {
		claims   []claimedEventFile
		identity uploadBatchIdentity
	}{
		"empty":                    {claims: nil, identity: identity},
		"too many":                 {claims: tooMany, identity: identity},
		"empty filename":           {claims: []claimedEventFile{{body: canonical}}, identity: identity},
		"noncanonical filename":    {claims: []claimedEventFile{{name: strings.ToUpper(event.EventID), body: canonical}}, identity: identity},
		"filename body mismatch":   {claims: []claimedEventFile{{name: event.EventID, body: encodedEventForUpload(t, wrongEventID)}}, identity: identity},
		"duplicate event ID":       {claims: append(append([]claimedEventFile(nil), valid...), valid...), identity: identity},
		"noncanonical queue bytes": {claims: []claimedEventFile{{name: event.EventID, body: nonCanonical}}, identity: identity},
		"oversized event file":     {claims: []claimedEventFile{{name: event.EventID, body: oversized}}, identity: identity},
		"malformed event":          {claims: []claimedEventFile{{name: event.EventID, body: []byte(`{`)}}, identity: identity},
		"wrong installation":       {claims: []claimedEventFile{{name: event.EventID, body: encodedEventForUpload(t, wrongInstallation)}}, identity: identity},
		"wrong release":            {claims: []claimedEventFile{{name: event.EventID, body: encodedEventForUpload(t, wrongRelease)}}, identity: identity},
		"invalid expected ID":      {claims: valid, identity: uploadBatchIdentity{installationID: "not-a-uuid", releaseVersion: event.ReleaseVersion}},
		"invalid expected release": {claims: valid, identity: uploadBatchIdentity{installationID: event.InstallationID, releaseVersion: "v0.31.0"}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := buildUploadBatch(test.claims, test.identity); err == nil {
				t.Fatal("buildUploadBatch unexpectedly accepted poison input")
			}
		})
	}
}

func TestDecodeUploadAcknowledgementRequiresExactCompleteSet(t *testing.T) {
	first := fixedEvent().EventID
	second := secondFixedEvent().EventID
	for _, test := range []struct {
		name   string
		status int
		action string
		want   uploadResponseKind
	}{
		{name: "accepted lower bound", status: 200, action: "accepted", want: uploadResponseAccepted},
		{name: "accepted upper bound", status: 299, action: "accepted", want: uploadResponseAccepted},
		{name: "duplicate", status: 409, action: "duplicate", want: uploadResponseDuplicate},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"schema_version":1,"app":"gascity","action":%q,"event_ids":[%q,%q]}`, test.action, second, first))
			got, err := decodeUploadAcknowledgement(test.status, "application/json", body, []string{first, second})
			if err != nil {
				t.Fatalf("decodeUploadAcknowledgement: %v", err)
			}
			if got != test.want {
				t.Fatalf("kind = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDecodeUploadAcknowledgementFailsClosed(t *testing.T) {
	first := fixedEvent().EventID
	second := secondFixedEvent().EventID
	accepted := `{"schema_version":1,"app":"gascity","action":"accepted","event_ids":["` + first + `","` + second + `"]}`
	duplicate := strings.Replace(accepted, `"accepted"`, `"duplicate"`, 1)

	tests := map[string]struct {
		status      int
		contentType string
		body        string
		submitted   []string
	}{
		"empty":                  {status: 200, contentType: "application/json", submitted: []string{first, second}},
		"partial":                {status: 200, contentType: "application/json", body: strings.Replace(accepted, `,"`+second+`"`, "", 1), submitted: []string{first, second}},
		"extra":                  {status: 200, contentType: "application/json", body: strings.Replace(accepted, `]}`, `,"00000000-0000-4000-8000-000000000001"]}`, 1), submitted: []string{first, second}},
		"duplicate response ID":  {status: 200, contentType: "application/json", body: strings.Replace(accepted, `"`+second+`"`, `"`+first+`"`, 1), submitted: []string{first, second}},
		"wrong action for 2xx":   {status: 200, contentType: "application/json", body: duplicate, submitted: []string{first, second}},
		"wrong action for 409":   {status: 409, contentType: "application/json", body: accepted, submitted: []string{first, second}},
		"unsupported status":     {status: 199, contentType: "application/json", body: accepted, submitted: []string{first, second}},
		"generic 409":            {status: 409, contentType: "application/json", body: `{}`, submitted: []string{first, second}},
		"wrong content type":     {status: 200, contentType: "application/json; charset=utf-8", body: accepted, submitted: []string{first, second}},
		"missing content type":   {status: 200, body: accepted, submitted: []string{first, second}},
		"unknown field":          {status: 200, contentType: "application/json", body: strings.Replace(accepted, `}`, `,"extra":true}`, 1), submitted: []string{first, second}},
		"duplicate key":          {status: 200, contentType: "application/json", body: strings.Replace(accepted, `"app":"gascity"`, `"app":"gascity","app":"gascity"`, 1), submitted: []string{first, second}},
		"case-folded field":      {status: 200, contentType: "application/json", body: strings.Replace(accepted, `"app"`, `"APP"`, 1), submitted: []string{first, second}},
		"wrong schema":           {status: 200, contentType: "application/json", body: strings.Replace(accepted, `"schema_version":1`, `"schema_version":2`, 1), submitted: []string{first, second}},
		"wrong app":              {status: 200, contentType: "application/json", body: strings.Replace(accepted, `"gascity"`, `"beads"`, 1), submitted: []string{first, second}},
		"invalid response ID":    {status: 200, contentType: "application/json", body: strings.Replace(accepted, first, strings.ToUpper(first), 1), submitted: []string{first, second}},
		"trailing JSON":          {status: 200, contentType: "application/json", body: accepted + `{}`, submitted: []string{first, second}},
		"oversized response":     {status: 200, contentType: "application/json", body: accepted + strings.Repeat(" ", maxUploadResponseBytes-len(accepted)+1), submitted: []string{first, second}},
		"empty submitted set":    {status: 200, contentType: "application/json", body: accepted},
		"invalid submitted ID":   {status: 200, contentType: "application/json", body: accepted, submitted: []string{strings.ToUpper(first), second}},
		"duplicate submitted ID": {status: 200, contentType: "application/json", body: accepted, submitted: []string{first, first}},
		"too many submitted IDs": {status: 200, contentType: "application/json", body: accepted, submitted: append(bytesToStrings(MaxBatchEvents), first)},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeUploadAcknowledgement(test.status, test.contentType, []byte(test.body), test.submitted); err == nil {
				t.Fatal("decodeUploadAcknowledgement unexpectedly accepted invalid acknowledgement")
			}
		})
	}
}

func bytesToStrings(count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
	}
	return values
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestUploadCodecDTOsHaveClosedShapes(t *testing.T) {
	for typ, want := range map[reflect.Type][]string{
		reflect.TypeOf(acknowledgementWire{}): {"SchemaVersion", "App", "Action", "EventIDs"},
		reflect.TypeOf(claimedEventFile{}):    {"name", "body"},
		reflect.TypeOf(preparedUploadBatch{}): {"body", "eventIDs", "installationID", "releaseVersion"},
	} {
		if typ.NumField() != len(want) {
			t.Fatalf("%s has %d fields, want %d", typ, typ.NumField(), len(want))
		}
		for i, field := range want {
			if typ.Field(i).Name != field {
				t.Errorf("%s field %d = %s, want %s", typ, i, typ.Field(i).Name, field)
			}
		}
		assertNoOpenDTOType(t, typ, map[reflect.Type]bool{})
	}
}

func FuzzDecodeUploadAcknowledgement(f *testing.F) {
	submitted := []string{fixedEvent().EventID}
	f.Add(uint16(200), "application/json", []byte(acceptedBody(submitted, "accepted")))
	f.Add(uint16(409), "application/json", []byte(acceptedBody(submitted, "duplicate")))
	f.Add(uint16(200), "text/html", []byte(`<html>`))
	f.Fuzz(func(t *testing.T, status uint16, contentType string, body []byte) {
		kind, err := decodeUploadAcknowledgement(int(status), contentType, body, submitted)
		if err == nil && kind != uploadResponseAccepted && kind != uploadResponseDuplicate {
			t.Fatalf("successful decoder returned kind %v", kind)
		}
	})
}
