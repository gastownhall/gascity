package mail

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func enabledRoster() CityRoster {
	return CityRoster{Local: "qlandia", Peers: []string{"gastown", "westeros"}}
}

func TestCityRosterEnabled(t *testing.T) {
	tests := []struct {
		name   string
		roster CityRoster
		want   bool
	}{
		{"zero value", CityRoster{}, false},
		{"local only", CityRoster{Local: "qlandia"}, false},
		{"peers only", CityRoster{Peers: []string{"gastown"}}, false},
		{"local and peers", enabledRoster(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.roster.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveCityAddress(t *testing.T) {
	roster := enabledRoster()
	tests := []struct {
		name      string
		recipient string
		wantKind  CityAddressKind
		wantAddr  string
	}{
		{"local prefix strips to rig-qualified rest", "qlandia/qcore/syl", CityAddressLocal, "qcore/syl"},
		{"local prefix strips to bare rest", "qlandia/mayor", CityAddressLocal, "mayor"},
		{"peer city is foreign, canonical as written", "gastown/mayor", CityAddressForeign, "gastown/mayor"},
		{"foreign split is on the first slash only", "gastown/qcore/dalinar", CityAddressForeign, "gastown/qcore/dalinar"},
		{"second peer", "westeros/human", CityAddressForeign, "westeros/human"},
		{"unknown first segment falls through", "qcore/syl", CityAddressNone, "qcore/syl"},
		{"bare name falls through", "mayor", CityAddressNone, "mayor"},
		{"human falls through", "human", CityAddressNone, "human"},
		{"bare local city name falls through", "qlandia", CityAddressNone, "qlandia"},
		{"bare peer city name falls through", "gastown", CityAddressNone, "gastown"},
		{"local city with empty rest falls through", "qlandia/", CityAddressNone, "qlandia/"},
		{"peer city with empty rest falls through", "gastown/", CityAddressNone, "gastown/"},
		{"city names match exactly, not case-folded", "Gastown/mayor", CityAddressNone, "Gastown/mayor"},
		{"surrounding whitespace is trimmed", "  gastown/mayor  ", CityAddressForeign, "gastown/mayor"},
		{"whitespace inside the first segment is not a city match", "gastown /mayor", CityAddressNone, "gastown /mayor"},
		{"foreign remainder trims a trailing slash like local resolution does", "gastown/mayor/", CityAddressForeign, "gastown/mayor"},
		{"foreign remainder trims surrounding whitespace", "gastown/ mayor", CityAddressForeign, "gastown/mayor"},
		{"foreign remainder that is only separators falls through", "gastown//", CityAddressNone, "gastown//"},
		{"local remainder trims a trailing slash", "qlandia/mayor/", CityAddressLocal, "mayor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, addr := roster.ResolveCityAddress(tt.recipient)
			if kind != tt.wantKind || addr != tt.wantAddr {
				t.Errorf("ResolveCityAddress(%q) = (%v, %q), want (%v, %q)",
					tt.recipient, kind, addr, tt.wantKind, tt.wantAddr)
			}
		})
	}
}

func TestResolveCityAddressDisabledRoster(t *testing.T) {
	var roster CityRoster
	kind, addr := roster.ResolveCityAddress("gastown/mayor")
	if kind != CityAddressNone || addr != "gastown/mayor" {
		t.Errorf("disabled roster: got (%v, %q), want (%v, %q)",
			kind, addr, CityAddressNone, "gastown/mayor")
	}
}

func TestUnknownCityErrorMessage(t *testing.T) {
	err := &UnknownCityError{City: "gastwn", Known: []string{"gastown", "qlandia", "westeros"}}
	msg := err.Error()
	if !strings.Contains(msg, "unknown city") || !strings.Contains(msg, `"gastwn"`) {
		t.Errorf("message %q should name the unknown city", msg)
	}
	for _, known := range []string{"gastown", "qlandia", "westeros"} {
		if !strings.Contains(msg, known) {
			t.Errorf("message %q should list known city %q", msg, known)
		}
	}
}

func TestRefuseUnknownCity(t *testing.T) {
	roster := enabledRoster()
	base := fmt.Errorf("unknown recipient %q", "x")
	rigs := []string{"qcore", "gascity"}

	tests := []struct {
		name      string
		roster    CityRoster
		recipient string
		wantCity  string // "" means the original error must come back unchanged
	}{
		{"unknown slash-form segment becomes unknown city", roster, "gastwn/mayor", "gastwn"},
		{"known rig segment keeps the original error", roster, "qcore/nobody", ""},
		{"local city segment keeps the original error", roster, "qlandia/nobody", ""},
		{"peer city segment keeps the original error", roster, "gastown/mayor", ""},
		{"no slash keeps the original error", roster, "nobody", ""},
		{"disabled roster keeps the original error", CityRoster{}, "gastwn/mayor", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RefuseUnknownCity(base, tt.recipient, tt.roster, rigs)
			var unknownCity *UnknownCityError
			if tt.wantCity == "" {
				if !errors.Is(got, base) {
					t.Errorf("got %v, want original error %v", got, base)
				}
				return
			}
			if !errors.As(got, &unknownCity) {
				t.Fatalf("got %v (%T), want *UnknownCityError", got, got)
			}
			if unknownCity.City != tt.wantCity {
				t.Errorf("City = %q, want %q", unknownCity.City, tt.wantCity)
			}
		})
	}
}

// The unknown-city refusal must replace the fall-through resolution error so a
// stale roster can never be spelled "session not found".
func TestRefuseUnknownCityDoesNotUnwrapToOriginal(t *testing.T) {
	sentinel := errors.New("session not found")
	got := RefuseUnknownCity(sentinel, "gastwn/mayor", enabledRoster(), nil)
	if errors.Is(got, sentinel) {
		t.Errorf("unknown-city error must not unwrap to the resolution error it replaces")
	}
}

func TestExpandLocalRecipients(t *testing.T) {
	roster := enabledRoster()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"local addresses gain their city-qualified alias",
			[]string{"qcore/syl", "mayor"},
			[]string{"qcore/syl", "mayor", "qlandia/qcore/syl", "qlandia/mayor"},
		},
		{
			"human gains the qualified alias",
			[]string{"human"},
			[]string{"human", "qlandia/human"},
		},
		{
			"already-qualified local form is not double-qualified",
			[]string{"qlandia/qcore/syl"},
			[]string{"qlandia/qcore/syl"},
		},
		{
			"foreign form is left alone",
			[]string{"gastown/mayor"},
			[]string{"gastown/mayor"},
		},
		{
			"duplicates collapse",
			[]string{"mayor", "qlandia/mayor"},
			[]string{"mayor", "qlandia/mayor"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := roster.ExpandLocalRecipients(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("ExpandLocalRecipients(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("ExpandLocalRecipients(%v)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExpandLocalRecipientsDisabledRoster(t *testing.T) {
	var roster CityRoster
	in := []string{"qcore/syl", "human"}
	got := roster.ExpandLocalRecipients(in)
	if len(got) != len(in) || got[0] != in[0] || got[1] != in[1] {
		t.Errorf("disabled roster must return recipients unchanged: got %v, want %v", got, in)
	}
}

func TestQualifySender(t *testing.T) {
	roster := enabledRoster()
	tests := []struct {
		name   string
		sender string
		want   string
	}{
		{"local identity gains the city prefix", "qcore/syl", "qlandia/qcore/syl"},
		{"bare identity gains the city prefix", "mayor", "qlandia/mayor"},
		{"human gains the city prefix", "human", "qlandia/human"},
		{"already locally qualified stays put", "qlandia/qcore/syl", "qlandia/qcore/syl"},
		{"foreign-qualified stays put", "gastown/mayor", "gastown/mayor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roster.QualifySender(tt.sender); got != tt.want {
				t.Errorf("QualifySender(%q) = %q, want %q", tt.sender, got, tt.want)
			}
		})
	}
}

func TestQualifySenderDisabledRoster(t *testing.T) {
	var roster CityRoster
	if got := roster.QualifySender("mayor"); got != "mayor" {
		t.Errorf("disabled roster must return sender unchanged: got %q", got)
	}
}
