package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type poolBootWire struct {
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Agent string `json:"agent,omitempty"`
}

type cityPoolBootWire struct {
	Status   string        `json:"status"`
	PoolBoot *poolBootWire `json:"pool_boot,omitempty"`
}

type citiesPoolBootWire struct {
	Items []cityPoolBootWire `json:"items"`
}

type startupPoolBootWire struct {
	Phase    string        `json:"phase"`
	PoolBoot *poolBootWire `json:"pool_boot,omitempty"`
}

type healthPoolBootWire struct {
	Startup *startupPoolBootWire `json:"startup"`
}

func TestSupervisorPoolBootStatusUsesOneOptionalTypedShape(t *testing.T) {
	cityField := requirePoolBootField(t, reflect.TypeOf(CityInfo{}), "CityInfo")
	startupField := requirePoolBootField(t, reflect.TypeOf(SupervisorStartup{}), "SupervisorStartup")

	if cityField.Type.Kind() != reflect.Pointer {
		t.Fatalf("CityInfo.PoolBoot kind = %s, want pointer so pool_boot can be omitted", cityField.Type.Kind())
	}
	if startupField.Type != cityField.Type {
		t.Fatalf("SupervisorStartup.PoolBoot type = %s, want the same shared type as CityInfo.PoolBoot (%s)", startupField.Type, cityField.Type)
	}

	statusType := cityField.Type.Elem()
	assertPoolBootStatusField(t, statusType, "Done", reflect.Int, "done")
	assertPoolBootStatusField(t, statusType, "Total", reflect.Int, "total")
	assertPoolBootStatusField(t, statusType, "Agent", reflect.String, "agent,omitempty")
}

func TestSupervisorPoolBootStatusProjectsAcrossCitiesAndHealth(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		progress *poolBootWire
	}{
		{
			name:   "structured progress preserves delimiter characters",
			status: "running_pool_on_boot",
			progress: &poolBootWire{
				Done:  2,
				Total: 5,
				Agent: "rig:blue/worker:slot/one",
			},
		},
		{
			name:   "phase begins with typed zero progress and no agent",
			status: "running_pool_on_boot",
			progress: &poolBootWire{
				Done:  0,
				Total: 4,
			},
		},
		{
			name:   "outside pool boot omits progress",
			status: "starting_agents",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			city := CityInfo{
				Name:    "bright-lights",
				Path:    "/cities/bright-lights",
				Running: false,
				Status:  tt.status,
			}
			if tt.progress != nil {
				setCityInfoPoolBoot(t, &city, *tt.progress)
			}

			resolver := &fakeCityResolver{
				cities: map[string]*fakeState{},
				listed: []CityInfo{city},
			}
			sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())

			var cities citiesPoolBootWire
			getSupervisorResponse(t, sm, "/v0/cities", &cities)
			if len(cities.Items) != 1 {
				t.Fatalf("GET /v0/cities returned %d items, want 1", len(cities.Items))
			}
			if got := cities.Items[0].Status; got != tt.status {
				t.Fatalf("GET /v0/cities status = %q, want bare phase token %q", got, tt.status)
			}

			var health healthPoolBootWire
			getSupervisorResponse(t, sm, "/health", &health)
			if health.Startup == nil {
				t.Fatal("GET /health omitted startup")
			}
			if got := health.Startup.Phase; got != tt.status {
				t.Fatalf("GET /health startup.phase = %q, want bare phase token %q", got, tt.status)
			}

			if !reflect.DeepEqual(cities.Items[0].PoolBoot, tt.progress) {
				t.Fatalf("GET /v0/cities pool_boot = %#v, want %#v", cities.Items[0].PoolBoot, tt.progress)
			}
			if !reflect.DeepEqual(health.Startup.PoolBoot, tt.progress) {
				t.Fatalf("GET /health startup.pool_boot = %#v, want %#v", health.Startup.PoolBoot, tt.progress)
			}
			if !reflect.DeepEqual(health.Startup.PoolBoot, cities.Items[0].PoolBoot) {
				t.Fatalf("pool_boot drifted between endpoints: /v0/cities=%#v /health=%#v", cities.Items[0].PoolBoot, health.Startup.PoolBoot)
			}
		})
	}
}

func requirePoolBootField(t *testing.T, owner reflect.Type, ownerName string) reflect.StructField {
	t.Helper()
	field, ok := owner.FieldByName("PoolBoot")
	if !ok {
		t.Fatalf("%s has no PoolBoot field; startup progress needs an optional typed pool_boot projection", ownerName)
	}
	if got := field.Tag.Get("json"); got != "pool_boot,omitempty" {
		t.Fatalf("%s.PoolBoot json tag = %q, want %q", ownerName, got, "pool_boot,omitempty")
	}
	return field
}

func assertPoolBootStatusField(t *testing.T, statusType reflect.Type, name string, kind reflect.Kind, jsonTag string) {
	t.Helper()
	field, ok := statusType.FieldByName(name)
	if !ok {
		t.Fatalf("%s has no %s field", statusType, name)
	}
	if field.Type.Kind() != kind {
		t.Fatalf("%s.%s kind = %s, want %s", statusType, name, field.Type.Kind(), kind)
	}
	if got := field.Tag.Get("json"); got != jsonTag {
		t.Fatalf("%s.%s json tag = %q, want %q", statusType, name, got, jsonTag)
	}
}

func setCityInfoPoolBoot(t *testing.T, city *CityInfo, progress poolBootWire) {
	t.Helper()
	field := reflect.ValueOf(city).Elem().FieldByName("PoolBoot")
	if !field.IsValid() {
		t.Fatal("CityInfo has no PoolBoot field; cannot project structured pool boot progress")
	}
	if field.Kind() != reflect.Pointer || !field.CanSet() || field.Type().Elem().Kind() != reflect.Struct {
		t.Fatalf("CityInfo.PoolBoot kind/settable = %s/%t, want settable pointer to struct", field.Kind(), field.CanSet())
	}

	status := reflect.New(field.Type().Elem())
	setPoolBootIntField(t, status.Elem(), "Done", progress.Done)
	setPoolBootIntField(t, status.Elem(), "Total", progress.Total)
	setPoolBootStringField(t, status.Elem(), "Agent", progress.Agent)
	field.Set(status)
}

func setPoolBootIntField(t *testing.T, status reflect.Value, name string, value int) {
	t.Helper()
	field := status.FieldByName(name)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.Int {
		t.Fatalf("pool boot %s field is missing or not a settable int", name)
	}
	field.SetInt(int64(value))
}

func setPoolBootStringField(t *testing.T, status reflect.Value, name, value string) {
	t.Helper()
	field := status.FieldByName(name)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.String {
		t.Fatalf("pool boot %s field is missing or not a settable string", name)
	}
	field.SetString(value)
}

func getSupervisorResponse(t *testing.T, sm *SupervisorMux, path string, out any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d; body: %s", path, rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
}
