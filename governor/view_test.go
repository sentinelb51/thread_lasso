//go:build windows && amd64

package governor

import "testing"

func TestThreadRowIdentity(t *testing.T) {
	tests := []struct {
		name string
		row  ThreadRow
		want string
	}{
		{
			name: "a game-set description wins",
			row:  ThreadRow{Name: "BinkAsy0", Module: "binkawin64.dll", Role: "pool-idle", Ordinal: 3},
			want: "BinkAsy0",
		},
		{
			name: "module and offset when the thread is unnamed",
			row:  ThreadRow{Module: "overwatch.exe", ModuleOffset: 0x2678fa0, Role: "job-worker", Ordinal: 2},
			want: "overwatch.exe+0x2678fa0",
		},
		{
			name: "what the stack says when the origin was scrubbed",
			row:  ThreadRow{Activity: "dxgi.dll", Role: "render", Ordinal: 2},
			want: "[dxgi.dll]",
		},
		{
			name: "synthetic label when nothing resolved",
			row:  ThreadRow{Role: "render", Ordinal: 2},
			want: "render #2",
		},
		{
			name: "an unknown role is not worth a label",
			row:  ThreadRow{Role: "unknown", Ordinal: 4},
			want: "—",
		},
		{
			name: "nothing at all",
			row:  ThreadRow{},
			want: "—",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.row.Identity(); got != test.want {
				t.Errorf("Identity() = %q, want %q", got, test.want)
			}
		})
	}
}

// "This thread has ntdll frames" is true of every thread in the process, so it
// must not displace the role label that at least tells two of them apart.
func TestActivityOfSkipsTheFramesEveryThreadHas(t *testing.T) {
	tests := []struct {
		name  string
		stack []string
		want  string
	}{
		{"the busiest meaningful module wins", []string{"dxgi.dll", "ntdll.dll"}, "dxgi.dll"},
		{"startup modules are skipped over", []string{"ntdll.dll", "kernel32.dll", "ws2_32.dll"}, "ws2_32.dll"},
		{"nothing but startup modules identifies nothing", []string{"ntdll.dll", "kernelbase.dll"}, ""},
		{"an unswept stack", nil, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := activityOf(test.stack); got != test.want {
				t.Errorf("activityOf(%v) = %q, want %q", test.stack, got, test.want)
			}
		})
	}
}

func TestThreadRowIdentified(t *testing.T) {
	if (ThreadRow{Role: "render", Ordinal: 1}).Identified() {
		t.Error("a synthetic role label must not count as identified")
	}
	if !(ThreadRow{Module: "ntdll.dll"}).Identified() {
		t.Error("a resolved module identifies a thread")
	}
	if !(ThreadRow{Name: "BinkAsy0"}).Identified() {
		t.Error("a game-set description identifies a thread")
	}
}

// The label a thread is referred to by must not move when the table re-sorts,
// so ordinals come from creation order, never from the cycle rate the rows
// arrive in.
func TestAssignOrdinalsIsIndependentOfRowOrder(t *testing.T) {
	rows := []ThreadRow{
		{TID: 30, CreateTime: 300, Role: "render", CyclesRate: 900},
		{TID: 10, CreateTime: 100, Role: "render", CyclesRate: 100},
		{TID: 20, CreateTime: 200, Role: "job-worker", CyclesRate: 500},
		{TID: 40, CreateTime: 400, Role: "render", CyclesRate: 700},
		{TID: 50, CreateTime: 400, Role: "job-worker", CyclesRate: 50},
	}
	assignOrdinals(rows)

	want := map[uint32]int{30: 2, 10: 1, 20: 1, 40: 3, 50: 2}
	for _, row := range rows {
		if row.Ordinal != want[row.TID] {
			t.Errorf("tid %d: ordinal = %d, want %d", row.TID, row.Ordinal, want[row.TID])
		}
	}

	// Same threads, opposite order: the ordinals must be identical.
	reversed := make([]ThreadRow, len(rows))
	for i := range rows {
		reversed[i] = rows[len(rows)-1-i]
		reversed[i].Ordinal = 0
	}
	assignOrdinals(reversed)
	for _, row := range reversed {
		if row.Ordinal != want[row.TID] {
			t.Errorf("reversed, tid %d: ordinal = %d, want %d", row.TID, row.Ordinal, want[row.TID])
		}
	}
}
