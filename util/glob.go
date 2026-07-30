package util

import "strings"

// Match reports whether name matches a shell-style glob pattern. It supports
// '*' (any run, including empty) and '?' (exactly one character). Matching is
// case-insensitive because Windows thread names and module names are compared
// without regard to case ("BinkAsy*" must match "binkasy_worker").
func Match(pattern, name string) bool {
	return globMatch(strings.ToLower(pattern), strings.ToLower(name))
}

// globMatch is an iterative wildcard matcher with backtracking on '*', so it
// runs in O(len(pattern)*len(s)) worst case without recursion.
func globMatch(pattern, s string) bool {
	px, sx := 0, 0
	star, mark := -1, 0

	for sx < len(s) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]):
			px++
			sx++
		case px < len(pattern) && pattern[px] == '*':
			// Remember the '*' position and where we were, then try to match
			// zero characters first; backtrack to consume more if needed.
			star, mark = px, sx
			px++
		case star != -1:
			px = star + 1
			mark++
			sx = mark
		default:
			return false
		}
	}

	// Trailing '*'s in the pattern can still match the empty remainder.
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}

	return px == len(pattern)
}
