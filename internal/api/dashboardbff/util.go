package dashboardbff

// firstNonEmpty returns a if it is non-empty, otherwise fallback.
func firstNonEmpty(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}
