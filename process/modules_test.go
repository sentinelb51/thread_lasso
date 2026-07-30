//go:build windows && amd64

package process

import "testing"

func TestModuleTableResolve(t *testing.T) {
	table := &ModuleTable{ranges: []moduleRange{
		{base: 0x7ff600000000, end: 0x7ff600010000, name: "overwatch.exe"},
		{base: 0x7ff700000000, end: 0x7ff700004000, name: "overwatch_loader.dll"},
		{base: 0x7ffa00000000, end: 0x7ffa00002000, name: "ntdll.dll"},
	}}
	table.normalise()

	tests := []struct {
		name   string
		addr   uintptr
		module string
		offset uintptr
	}{
		{"first byte of a module", 0x7ff600000000, "overwatch.exe", 0},
		{"inside a module", 0x7ff600002678, "overwatch.exe", 0x2678},
		{"last byte of a module", 0x7ff60000ffff, "overwatch.exe", 0xffff},
		{"one past the end", 0x7ff600010000, "", 0},
		{"between modules", 0x7ff650000000, "", 0},
		{"below every module", 0x1000, "", 0},
		{"above every module", 0x7ffb00000000, "", 0},
		{"a scrubbed address resolves to nothing", 0, "", 0},
		{"a later module", 0x7ffa00000100, "ntdll.dll", 0x100},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module, offset := table.Resolve(test.addr)
			if module != test.module || offset != test.offset {
				t.Errorf("Resolve(%#x) = (%q, %#x), want (%q, %#x)",
					test.addr, module, offset, test.module, test.offset)
			}
		})
	}
}

// A torn read while the target loads a module can produce overlapping ranges.
// Resolve binary-searches, so an overlap would corrupt lookups for unrelated
// modules, not just the two involved.
func TestModuleTableNormaliseDropsOverlaps(t *testing.T) {
	table := &ModuleTable{ranges: []moduleRange{
		{base: 0x3000, end: 0x4000, name: "third.dll"},
		{base: 0x1000, end: 0x2000, name: "first.dll"},
		{base: 0x1800, end: 0x2800, name: "overlapping.dll"},
		{base: 0x5000, end: 0x5000, name: "empty.dll"},
	}}
	table.normalise()

	if table.Len() != 2 {
		t.Fatalf("Len() = %d, want 2 (overlapping and empty ranges dropped)", table.Len())
	}
	if module, _ := table.Resolve(0x1900); module != "first.dll" {
		t.Errorf("Resolve(0x1900) = %q, want first.dll", module)
	}
	if module, _ := table.Resolve(0x3500); module != "third.dll" {
		t.Errorf("Resolve(0x3500) = %q, want third.dll", module)
	}
}

func TestThreadSnapshotEntryPoint(t *testing.T) {
	tests := []struct {
		name  string
		win32 uintptr
		start uintptr
		want  uintptr
	}{
		{name: "prefers the Win32 value", win32: 0x1000, start: 0x2000, want: 0x1000},
		{name: "falls back when it is scrubbed", win32: 0, start: 0x2000, want: 0x2000},
		{name: "both missing", win32: 0, start: 0, want: 0},
		{name: "ignores kernel addresses", win32: 0, start: 0xFFFF800000001000, want: 0},
		{name: "ignores a kernel Win32 value", win32: 0xFFFF800000001000, start: 0x2000, want: 0x2000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := ThreadSnapshot{Win32StartAddress: test.win32, StartAddress: test.start}
			if got := snapshot.EntryPoint(); got != test.want {
				t.Errorf("EntryPoint() = %#x, want %#x", got, test.want)
			}
		})
	}
}
