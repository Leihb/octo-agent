package fixture

// LastN returns the last n elements of s. If n >= len(s), it returns all of s.
func LastN(s []int, n int) []int {
	if n >= len(s) {
		return s
	}
	return s[len(s)-n+1:]
}
