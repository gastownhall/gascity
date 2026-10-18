package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/readywork"
)

// readyWorkFakeState is a city State that answers ready work: it records the
// query it was asked and answers with fixed rows or a fixed error.
type readyWorkFakeState struct {
	*fakeState
	rows  []readywork.ReadyBead
	err   error
	asked []readywork.Query
}

func (s *readyWorkFakeState) ReadyWork(_ context.Context, q readywork.Query) ([]readywork.ReadyBead, error) {
	s.asked = append(s.asked, q)
	return s.rows, s.err
}

// workReadyServer serves the API over state and returns a client of it.
func workReadyServer(t *testing.T, state State) *Client {
	t.Helper()
	srv := httptest.NewServer(NewSupervisorMux(&singleStateResolver{state: state}, nil, false, "test", "", time.Now()))
	t.Cleanup(srv.Close)
	return NewCityScopedClient(srv.URL, state.CityName())
}

// fullReadyBead is a row with EVERY field set to a non-zero value, so a field
// the wire drops or the client fails to convert shows up as a difference.
func fullReadyBead() readywork.ReadyBead {
	priority := 1
	deferUntil := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	blocked := false
	return readywork.ReadyBead{
		ID:           "gc-1",
		Title:        "title",
		Status:       "in_progress",
		Type:         "task",
		Priority:     &priority,
		CreatedAt:    time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Assignee:     "worker-1",
		From:         "sender",
		ParentID:     "gc-root",
		Ref:          "ref-1",
		Needs:        []string{"gc-0"},
		Description:  "description",
		Labels:       []string{"a", "b"},
		Metadata:     map[string]string{"gc.routed_to": "pool/a", "gc.kind": "workflow"},
		Dependencies: []readywork.ReadyBeadDep{{IssueID: "gc-1", DependsOnID: "gc-0", Type: "blocks"}},
		Ephemeral:    true,
		NoHistory:    true,
		DeferUntil:   &deferUntil,
		IsBlocked:    &blocked,
		CloseReason:  "reason",
		BlockedBy:    []readywork.ReadyBeadBlocker{{ID: "gc-0", Status: "open"}},
	}
}

// TestReadyWorkRoundTripsEveryRowFieldThroughTheWire pins the client's half of
// "the same rows, with the same fields": every field of a fully populated row
// survives the server's encode, the generated client's decode and the
// conversion back, and the result marshals to the server row's exact bytes.
// The fixture is checked by reflection to set every field, so a field added to
// readywork.ReadyBead without its conversion fails here.
func TestReadyWorkRoundTripsEveryRowFieldThroughTheWire(t *testing.T) {
	want := fullReadyBead()
	v := reflect.ValueOf(want)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves readywork.ReadyBead.%s zero; set it so the round trip covers it", v.Type().Field(i).Name)
		}
	}
	state := &readyWorkFakeState{fakeState: newFakeState(t), rows: []readywork.ReadyBead{want}}
	got, err := workReadyServer(t, state).ReadyWork(context.Background(), readywork.Query{})
	if err != nil {
		t.Fatalf("ReadyWork: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ReadyWork = %d rows, want 1", len(got))
	}
	gotJSON, _ := json.Marshal(got[0])
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("row after the wire:\n%s\nwant the server's bytes:\n%s", gotJSON, wantJSON)
	}
}

// TestReadyWorkSendsEveryQueryFieldToTheController pins the request half: every
// field of the reader's query reaches the controller unchanged, repeated flags
// in order. The fixture is checked by reflection to set every field.
func TestReadyWorkSendsEveryQueryFieldToTheController(t *testing.T) {
	want := readywork.Query{
		Assignee:       "worker-1",
		Unassigned:     true,
		MetadataFields: []string{"gc.routed_to=pool/a,b", "gc.kind"},
		ExcludeTypes:   []string{"epic", "molecule"},
		ExcludeLabels:  []string{"hold:mayor", "human"},
		Sort:           readywork.SortOldest,
		Limit:          20,
		Status:         readywork.StatusInProgress,
	}
	v := reflect.ValueOf(want)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves readywork.Query.%s zero; set it so the wire covers it", v.Type().Field(i).Name)
		}
	}
	state := &readyWorkFakeState{fakeState: newFakeState(t)}
	if _, err := workReadyServer(t, state).ReadyWork(context.Background(), want); err != nil {
		t.Fatalf("ReadyWork: %v", err)
	}
	if len(state.asked) != 1 || !reflect.DeepEqual(state.asked[0], want) {
		t.Fatalf("controller was asked %+v, want %+v", state.asked, want)
	}
}

// TestWorkReadyAnswersAnEmptyArrayNotNull: no ready work is an empty items
// array, never null, as `gc ready` prints [].
func TestWorkReadyAnswersAnEmptyArrayNotNull(t *testing.T) {
	state := &readyWorkFakeState{fakeState: newFakeState(t)}
	sm := NewSupervisorMux(&singleStateResolver{state: state}, nil, false, "test", "", time.Now())
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/city/"+state.CityName()+"/work/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /work/ready = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("body = %s, want an empty items array", rec.Body.String())
	}
}

// TestWorkReadyStatuses pins the failure contract the CLI's fallback rests on:
// the status, and for a 503 the problem code that tells a failed read
// (store-unavailable) from a controller that does not serve ready work or
// whose config is not current (service-unavailable). A store the controller
// does not hold open counts as a failed read.
func TestWorkReadyStatuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state State
		query string
		want  int
		code  string
	}{
		{name: "malformed metadata filter is 400", state: &readyWorkFakeState{fakeState: newFakeState(t)}, query: "?metadata_field=%3Dvalue", want: http.StatusBadRequest},
		{name: "unknown status is rejected", state: &readyWorkFakeState{fakeState: newFakeState(t)}, query: "?status=bogus", want: http.StatusUnprocessableEntity},
		// A misspelled filter must not widen the answer to everything.
		{name: "unknown query parameter is rejected", state: &readyWorkFakeState{fakeState: newFakeState(t)}, query: "?bogus=1", want: http.StatusUnprocessableEntity},
		{name: "a state that cannot answer is 503", state: newFakeState(t), want: http.StatusServiceUnavailable, code: apierr.ServiceUnavailable.Code},
		{name: "a failed read is 503 store-unavailable", state: &readyWorkFakeState{fakeState: newFakeState(t), err: errors.New(`rig "alpha" store: unreachable`)}, want: http.StatusServiceUnavailable, code: apierr.StoreUnavailable.Code},
		{name: "a config that is not current is 503 service-unavailable", state: &readyWorkFakeState{fakeState: newFakeState(t), err: fmt.Errorf("refusing: %w", ErrReadyWorkConfigNotCurrent)}, want: http.StatusServiceUnavailable, code: apierr.ServiceUnavailable.Code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewSupervisorMux(&singleStateResolver{state: tc.state}, nil, false, "test", "", time.Now())
			rec := httptest.NewRecorder()
			sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/city/"+tc.state.CityName()+"/work/ready"+tc.query, nil))
			if rec.Code != tc.want {
				t.Fatalf("GET /work/ready%s = %d, want %d (body %s)", tc.query, rec.Code, tc.want, rec.Body.String())
			}
			if tc.code == "" {
				return
			}
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem %s: %v", rec.Body.String(), err)
			}
			if problem.Code != tc.code {
				t.Fatalf("GET /work/ready%s problem code = %q, want %q (body %s)", tc.query, problem.Code, tc.code, rec.Body.String())
			}
		})
	}
}

// TestReadyWorkClientReportsAFailedReadAsAFallbackableError pins that a 503 the
// controller answers reaches the CLI as an error, which is its cue to read
// in-process; it must never decode into an empty answer.
func TestReadyWorkClientReportsAFailedReadAsAFallbackableError(t *testing.T) {
	state := &readyWorkFakeState{fakeState: newFakeState(t), err: errors.New(`rig "alpha" store: unreachable`)}
	rows, err := workReadyServer(t, state).ReadyWork(context.Background(), readywork.Query{})
	if err == nil {
		t.Fatalf("ReadyWork = %v rows and no error over a failed read", rows)
	}
	if !strings.Contains(err.Error(), `rig "alpha" store: unreachable`) {
		t.Fatalf("error = %v, want the failing leg named", err)
	}
}

// TestReadyWorkClientRefusesAQueryTheReaderRefuses pins the client's door to the
// reader's: a negative limit is refused before it is sent, never dropped from
// the request and answered unlimited.
func TestReadyWorkClientRefusesAQueryTheReaderRefuses(t *testing.T) {
	state := &readyWorkFakeState{fakeState: newFakeState(t)}
	q := readywork.Query{Limit: -1}
	rows, err := workReadyServer(t, state).ReadyWork(context.Background(), q)
	if want := q.Validate(); err == nil || want == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("ReadyWork(limit -1) = %v rows, error %v, want the reader's refusal %v", rows, err, want)
	}
	if len(state.asked) != 0 {
		t.Fatalf("controller was asked %+v, want the query refused before it is sent", state.asked)
	}
}

// TestWorkReadyEnumsAreTheReadersAcceptedValues pins the published status and
// sort enums to the values the reader accepts, so a status or order the reader
// gains cannot be rejected at the API's door while the CLI accepts it.
func TestWorkReadyEnumsAreTheReadersAcceptedValues(t *testing.T) {
	field := func(name string) reflect.StructField {
		f, ok := reflect.TypeOf(WorkReadyInput{}).FieldByName(name)
		if !ok {
			t.Fatalf("WorkReadyInput has no field %s", name)
		}
		return f
	}
	if got, want := strings.Split(field("Status").Tag.Get("enum"), ","), readywork.KnownStatuses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("status enum = %v, want the reader's statuses %v", got, want)
	}
	if got, want := strings.Split(field("Sort").Tag.Get("enum"), ","), []string{readywork.SortOldest, readywork.SortNewest}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sort enum = %v, want the reader's orders %v", got, want)
	}
	// The limit's floor: the API refuses below 0, and so must the reader, or a
	// negative --limit would be refused at one door and read unlimited at the other.
	if got := field("Limit").Tag.Get("minimum"); got != "0" {
		t.Fatalf("limit minimum = %q, want 0", got)
	}
	if err := (readywork.Query{Limit: -1}).Validate(); err == nil {
		t.Fatal("the reader accepts a negative limit the API refuses")
	}
	if err := (readywork.Query{Limit: 0}).Validate(); err != nil {
		t.Fatalf("the reader refuses limit 0, which the API accepts as unlimited: %v", err)
	}
}
