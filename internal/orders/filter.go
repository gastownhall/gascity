package orders

// FilterEnabled returns a slice containing only enabled orders without modifying aa.
func FilterEnabled(aa []Order) []Order {
	if len(aa) == 0 {
		return aa
	}
	filtered := make([]Order, 0, len(aa))
	for _, a := range aa {
		if a.IsEnabled() {
			filtered = append(filtered, a)
		}
	}
	return filtered
}
