//go:build windows && amd64

package ui

import (
	"testing"

	"ThreadOrchestra/governor"
)

func TestBuildRowsGroupsByRoleInFixedOrder(t *testing.T) {
	rows := []governor.ThreadRow{
		{TID: 1, Role: "job-worker", Bucket: "interactive", CyclesRate: 5e8},
		{TID: 2, Role: "render", Bucket: "critical", CyclesRate: 9e8},
		{TID: 3, Role: "job-worker", Bucket: "interactive", CyclesRate: 4e8},
		{TID: 4, Role: "main/sim", Bucket: "untouchable", CyclesRate: 3e9},
	}

	display := buildRows(rows, false)

	var headers []string
	for _, r := range display {
		if r.header {
			headers = append(headers, r.role)
		}
	}
	// roleOrder, not arrival order and not cycle rate.
	want := []string{"main/sim", "render", "job-worker"}
	if len(headers) != len(want) {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	for i := range want {
		if headers[i] != want[i] {
			t.Fatalf("headers = %v, want %v", headers, want)
		}
	}

	if countThreads(display) != len(rows) {
		t.Errorf("countThreads = %d, want %d", countThreads(display), len(rows))
	}

	// The job-worker header carries the group's combined rate.
	for _, r := range display {
		if r.header && r.role == "job-worker" {
			if r.count != 2 {
				t.Errorf("job-worker count = %d, want 2", r.count)
			}
			if r.cycles != 9e8 {
				t.Errorf("job-worker cycles = %v, want 9e8", r.cycles)
			}
		}
	}
}

// A role the classifier grows before roleOrder hears about it must still get a
// group rather than disappearing from the table.
func TestBuildRowsKeepsUnknownRoles(t *testing.T) {
	rows := []governor.ThreadRow{
		{TID: 1, Role: "physics", Bucket: "critical", CyclesRate: 1e9},
		{TID: 2, Role: "render", Bucket: "critical", CyclesRate: 1e9},
	}

	display := buildRows(rows, false)
	if countThreads(display) != 2 {
		t.Fatalf("countThreads = %d, want 2", countThreads(display))
	}

	found := false
	for _, r := range display {
		if r.header && r.role == "physics" {
			found = true
		}
	}
	if !found {
		t.Error("a role missing from roleOrder was dropped instead of appended")
	}
}

func TestNotable(t *testing.T) {
	tests := []struct {
		name string
		row  governor.ThreadRow
		want bool
	}{
		{"steered bucket", governor.ThreadRow{Bucket: "critical"}, true},
		{"starved", governor.ThreadRow{Bucket: "background", Starved: true}, true},
		{"tuned", governor.ThreadRow{Bucket: "background", Applied: "background cores"}, true},
		{"named by the game", governor.ThreadRow{Bucket: "background", Name: "BinkAsy0"}, true},
		{"burning cpu", governor.ThreadRow{Bucket: "background", CyclesRate: 5e7}, true},
		{"parked", governor.ThreadRow{Bucket: "background", CyclesRate: 1e3}, false},
		// The regression this guards: once entry points survive a scrubbed
		// Win32StartAddress, every thread resolves to a module, and a module
		// alone would let the whole idle pool through the filter.
		{"module alone is not enough", governor.ThreadRow{Bucket: "background", Module: "overwatch.exe"}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := notable(test.row); got != test.want {
				t.Errorf("notable() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBuildRowsShowAllIncludesFilteredThreads(t *testing.T) {
	rows := []governor.ThreadRow{
		{TID: 1, Role: "pool-idle", Bucket: "background", Module: "overwatch.exe"},
		{TID: 2, Role: "render", Bucket: "critical", CyclesRate: 1e9},
	}

	if got := countThreads(buildRows(rows, false)); got != 1 {
		t.Errorf("filtered: countThreads = %d, want 1", got)
	}
	if got := countThreads(buildRows(rows, true)); got != 2 {
		t.Errorf("show all: countThreads = %d, want 2", got)
	}
}

func TestFormatCycles(t *testing.T) {
	tests := []struct {
		rate float64
		want string
	}{
		{3.6e9, "3.60G"},
		{8.868e8, "886.8M"},
		{5.82e6, "5.8M"},
		{4.7e4, "47K"},
		{0, "—"},
	}

	for _, test := range tests {
		if got := formatCycles(test.rate); got != test.want {
			t.Errorf("formatCycles(%v) = %q, want %q", test.rate, got, test.want)
		}
	}
}
