package util

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"binkasy*", "BinkAsy_worker", true},   // case-insensitive prefix
		{"binkasy*", "BinkAsy", true},          // '*' matches empty
		{"*", "anything", true},                // lone star
		{"", "", true},                         // empty matches empty
		{"", "x", false},                       // empty matches nothing else
		{"vivoxsdk.dll", "vivoxsdk.dll", true}, // literal
		{"vivox*.dll", "vivoxsdk.dll", true},   // star in the middle
		{"amd??64.dll", "amdxx64.dll", true},   // two single-char wildcards
		{"amd?64.dll", "amdxx64.dll", false},   // one '?' can't span two chars
		{"*worker*", "pool_worker_3", true},    // surrounded
		{"telemetry", "TELEMETRY", true},       // full case fold
		{"*.dll", "kernel32.exe", false},       // suffix mismatch
	}

	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
