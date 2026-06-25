package dashboardbff

import "regexp"

// cityNameRE matches a managed city name: alphanumeric with internal hyphens,
// no path separators and no leading/trailing hyphen. Names are validated
// before any resolver lookup as a defensive measure; the authoritative path
// always comes from the resolver, never from joining the name.
var cityNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

func validCityName(name string) bool {
	return name != "" && len(name) <= 64 && cityNameRE.MatchString(name)
}

// firstNonEmpty returns a if it is non-empty, otherwise fallback.
func firstNonEmpty(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}
