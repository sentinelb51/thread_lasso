//go:build windows && amd64

package process

import (
	"encoding/binary"
	"testing"
)

// testInspector builds an Inspector with a fixed module table and no process
// handle. Everything the sweep does after the read is pure.
func testInspector() *Inspector {
	table := &ModuleTable{ranges: []moduleRange{
		{base: 0x7ff600000000, end: 0x7ff600100000, name: "overwatch.exe"},
		{base: 0x7ff700000000, end: 0x7ff700100000, name: "dxgi.dll"},
		{base: 0x7ffa00000000, end: 0x7ffa00100000, name: "ntdll.dll"},
		{base: 0x7ffb00000000, end: 0x7ffb00100000, name: "kernel32.dll"},
	}}
	table.normalise()

	inspector := &Inspector{table: table, hits: make(map[string]int)}
	inspector.SetLimits(DefaultScanLimits())

	return inspector
}

// stackBytes lays out values as a little-endian stack image, lowest address
// first — the order the sweep walks them in.
func stackBytes(values ...uintptr) []byte {
	buf := make([]byte, len(values)*stackAlign)
	for i, value := range values {
		binary.LittleEndian.PutUint64(buf[i*stackAlign:], uint64(value))
	}
	return buf
}

func TestFoldCountsFramesPerModule(t *testing.T) {
	i := testInspector()
	i.fold(stackBytes(
		0x7ffa00000100,  // ntdll
		0x0000000000000, // a zeroed slot
		0x7ff700000200,  // dxgi
		0x00000000dead,  // not in any module
		0x7ff700000300,  // dxgi
		0x7ff700000400,  // dxgi
		0x7ff600000500,  // overwatch
	), nil)

	if got := i.hits["dxgi.dll"]; got != 3 {
		t.Errorf("dxgi frames = %d, want 3", got)
	}
	if got := i.hits["ntdll.dll"]; got != 1 {
		t.Errorf("ntdll frames = %d, want 1", got)
	}

	// One pointer is as likely to be a stale argument as a return address, so
	// rank drops it; three is a call path.
	modules := i.rank()
	if len(modules) != 1 || modules[0] != "dxgi.dll" {
		t.Errorf("rank() = %v, want only dxgi.dll (the rest are single hits)", modules)
	}
}

func TestFoldIgnoresKernelAndUnalignedValues(t *testing.T) {
	i := testInspector()

	// A kernel-space value is not a user frame however plausible it looks.
	i.fold(stackBytes(0xffff800000000000, 0xffffffffffffffff), nil)
	if len(i.hits) != 0 {
		t.Errorf("hits = %v, want none: kernel addresses are not user frames", i.hits)
	}

	// A trailing partial pointer must not be read past.
	buf := stackBytes(0x7ff700000200)
	if got := len(buf); got != stackAlign {
		t.Fatalf("stackBytes produced %d bytes, want %d", got, stackAlign)
	}
	i.fold(append(buf, 0xff, 0xff, 0xff), nil)
	if got := i.hits["dxgi.dll"]; got != 1 {
		t.Errorf("dxgi frames = %d, want 1", got)
	}
}

// The entry-point candidate is what recovers a start address from a process
// that scrubbed both fields, so the two rules it rests on are worth pinning:
// it ignores the frames every thread has, and it takes the outermost one.
func TestFoldPicksTheOutermostNonStartupFrame(t *testing.T) {
	i := testInspector()

	var candidate uintptr
	i.fold(stackBytes(
		0x7ff700000200, // dxgi — a live frame, lower on the stack
		0x7ff600000700, // overwatch — the thread's own routine
		0x7ffb00000800, // kernel32!BaseThreadInitThunk
		0x7ffa00000900, // ntdll!RtlUserThreadStart, the outermost frame of all
	), &candidate)

	if candidate != 0x7ff600000700 {
		t.Errorf("candidate = 0x%x, want 0x7ff600000700: the highest frame outside ntdll/kernel32", candidate)
	}
}

func TestFoldFindsNoCandidateWhenOnlyStartupFramesArePresent(t *testing.T) {
	i := testInspector()

	var candidate uintptr
	i.fold(stackBytes(0x7ffa00000100, 0x7ffb00000200), &candidate)

	if candidate != 0 {
		t.Errorf("candidate = 0x%x, want 0: ntdll and kernel32 identify nothing", candidate)
	}
}

func TestRankOrdersByFrameCount(t *testing.T) {
	i := testInspector()
	i.hits["ntdll.dll"] = 9
	i.hits["dxgi.dll"] = 40
	i.hits["overwatch.exe"] = 12
	i.hits["kernel32.dll"] = 1 // below minStackHits

	want := []string{"dxgi.dll", "overwatch.exe", "ntdll.dll"}
	got := i.rank()
	if len(got) != len(want) {
		t.Fatalf("rank() = %v, want %v", got, want)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Fatalf("rank() = %v, want %v", got, want)
		}
	}
}
