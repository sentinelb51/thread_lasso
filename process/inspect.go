//go:build windows && amd64

package process

import (
	"encoding/binary"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stackAlign is the only part of the stack geometry that is not configurable:
// x64 pushes 8-byte-aligned pointers, so a sweep at any other stride is reading
// noise.
const stackAlign = 8

// ScanLimits is the geometry of a stack sweep. A thread's committed stack is
// [StackLimit, StackBase). The frames it is running now sit just above
// StackLimit because the stack grows down; the frames it *started* with sit at
// the very top and are never overwritten, since nothing the thread does is
// shallower than the routine that called it. Those two ends answer two
// different questions, so both are swept, and each gets its own window.
type ScanLimits struct {
	StartupWindow int // bytes below StackBase holding the thread-start frames
	ActiveWindow  int // bytes above StackLimit swept for live frames

	// MinHits is how many pointers into a module make it a call path. One is as
	// likely to be a stale argument as a return address.
	MinHits int

	// MaxModules bounds what is kept per thread: enough to name what a thread is
	// doing without carrying a copy of the process's module list around.
	MaxModules int
}

// DefaultScanLimits is what an Inspector uses until told otherwise. The
// governor overrides these from the config; the probe and the tests use them
// as-is.
func DefaultScanLimits() ScanLimits {
	return ScanLimits{
		StartupWindow: 4 << 10,
		ActiveWindow:  96 << 10,
		MinHits:       2,
		MaxModules:    8,
	}
}

// sane clamps limits that would make a sweep meaningless or unbounded, so a
// configuration mistake cannot turn into a multi-megabyte read per thread.
func (l ScanLimits) sane() ScanLimits {
	defaults := DefaultScanLimits()

	if l.StartupWindow < stackAlign {
		l.StartupWindow = defaults.StartupWindow
	}
	if l.ActiveWindow < stackAlign {
		l.ActiveWindow = defaults.ActiveWindow
	}
	if l.MinHits < 1 {
		l.MinHits = defaults.MinHits
	}
	if l.MaxModules < 1 {
		l.MaxModules = defaults.MaxModules
	}

	return l
}

// startupModules are the frames every thread has and no thread is identified
// by: ntdll starts it, kernel32 thunks into the routine it was handed. The
// entry-point candidate is the deepest stack value that is *not* one of these.
var startupModules = map[string]bool{
	"ntdll.dll":      true,
	"kernel32.dll":   true,
	"kernelbase.dll": true,
}

// StartupModule reports whether a module is one every thread's stack contains
// by construction. Callers use it for the same reason the entry-point candidate
// skips them: "this thread has ntdll frames" is true of every thread alive and
// therefore identifies none of them.
func StartupModule(name string) bool { return startupModules[name] }

// tebWin32ThreadInfo is the offset of TEB.Win32ThreadInfo on x64. The kernel
// fills it the first time a thread calls into user32 or gdi32, so a non-null
// value is proof the thread talks to the window system — which no counter,
// wait reason or start address reveals.
const tebWin32ThreadInfo = 0x78

// pageExecutable covers every protection that permits execution.
const pageExecutable = windows.PAGE_EXECUTE | windows.PAGE_EXECUTE_READ |
	windows.PAGE_EXECUTE_READWRITE | windows.PAGE_EXECUTE_WRITECOPY

// StackTrace is what one thread's stack says about it.
//
// It is not an unwind: walking x64 frames properly needs the unwind tables from
// every loaded image, which is a disassembler's job, not a governor's. A
// pointer sweep is cruder but answers the two questions that matter here — where
// the thread started, and which subsystems its call path runs through — and it
// keeps working on a process that has scrubbed everything else.
type StackTrace struct {
	// Entry is the start routine recovered from the startup frames, or 0 when
	// nothing plausible was found. It is a heuristic: the highest-addressed
	// value on the startup window that points at committed executable memory
	// outside ntdll and kernel32.
	Entry uintptr

	// Modules are the lowercase base names of modules with frames on the stack,
	// most hits first.
	Modules []string

	// Read is how many bytes were actually read; 0 means the reads failed and
	// the rest of the struct is empty rather than negative evidence.
	Read int
}

// Inspector holds one process open for reading and answers questions that need
// its memory: which module owns an address, what is on a thread's stack, and
// whether a thread has ever touched the window system.
//
// Full mode only. Not safe for concurrent use — the governor loop owns it.
type Inspector struct {
	handle windows.Handle
	table  *ModuleTable
	limits ScanLimits
	buf    []byte
	hits   map[string]int // reused across traces to keep the sweep allocation-free
}

// OpenInspector opens pid and snapshots its modules.
func OpenInspector(pid uint32) (*Inspector, error) {
	handle, err := openForInspection(pid)
	if err != nil {
		return nil, err
	}

	table, err := loadModuleTable(handle)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}

	inspector := &Inspector{
		handle: handle,
		table:  table,
		hits:   make(map[string]int, 32),
	}
	inspector.SetLimits(DefaultScanLimits())

	return inspector, nil
}

// SetLimits adopts new sweep geometry, resizing the read buffer to match. The
// buffer only ever grows: shrinking it would mean reallocating whenever the
// user nudged a window down and back up.
func (i *Inspector) SetLimits(limits ScanLimits) {
	if i == nil {
		return
	}

	i.limits = limits.sane()
	if size := max(i.limits.ActiveWindow, i.limits.StartupWindow); size > len(i.buf) {
		i.buf = make([]byte, size)
	}
}

func (i *Inspector) Close() {
	if i == nil || i.handle == 0 {
		return
	}
	windows.CloseHandle(i.handle)
	i.handle = 0
}

// Resolve maps an address to the module owning it. See ModuleTable.Resolve.
func (i *Inspector) Resolve(addr uintptr) (name string, offset uintptr) {
	if i == nil || i.table == nil {
		return "", 0
	}
	return i.table.Resolve(addr)
}

// ModuleCount reports how many module ranges the current table covers.
func (i *Inspector) ModuleCount() int {
	if i == nil || i.table == nil {
		return 0
	}
	return i.table.Len()
}

// Reload rebuilds the module table on the existing handle and reports the new
// range count. A protected process finishes mapping its real modules long after
// a scanner first sees it.
func (i *Inspector) Reload() (int, error) {
	if i == nil || i.handle == 0 {
		return 0, nil
	}

	table, err := loadModuleTable(i.handle)
	if err != nil {
		return i.ModuleCount(), err
	}
	i.table = table

	return table.Len(), nil
}

// Trace sweeps a thread's stack. Both windows are read on every call: the live
// one changes constantly, and the startup one is cheap enough not to be worth
// caching separately.
func (i *Inspector) Trace(t *ThreadSnapshot) StackTrace {
	var trace StackTrace
	if i == nil || i.handle == 0 || t.StackLimit == 0 || t.StackBase <= t.StackLimit {
		return trace
	}

	for name := range i.hits {
		delete(i.hits, name)
	}
	committed := t.StackBase - t.StackLimit

	// Live frames, from the committed low-water mark upward. Frames left behind
	// by earlier calls are swept too, and that is deliberate: nothing rewrites
	// the stack below the current frame, so a thread parked on a queue still
	// shows the subsystem it was last inside.
	trace.Read += i.sweep(t.StackLimit, min(committed, uintptr(i.limits.ActiveWindow)), nil)

	// Startup frames. The routine RtlUserThreadStart was handed is still spilled
	// near the top of the stack even when both start-address fields read zero —
	// user mode can rewrite ETHREAD, but it cannot go back and unwrite the
	// arguments its own thread was created with.
	var candidate uintptr
	top := min(committed, uintptr(i.limits.StartupWindow))
	trace.Read += i.sweep(t.StackBase-top, top, &candidate)
	if candidate != 0 && i.executable(candidate) {
		trace.Entry = candidate
	}

	trace.Modules = i.rank()

	return trace
}

// GuiThread reports whether the thread has a Win32 thread info block, i.e. has
// called into user32 or gdi32 at least once. Reads one pointer from the TEB.
func (i *Inspector) GuiThread(tebBase uintptr) bool {
	if i == nil || i.handle == 0 || tebBase == 0 {
		return false
	}

	var value uintptr
	var read uintptr
	err := windows.ReadProcessMemory(i.handle, tebBase+tebWin32ThreadInfo,
		(*byte)(unsafe.Pointer(&value)), unsafe.Sizeof(value), &read)

	return err == nil && read == unsafe.Sizeof(value) && value != 0
}

// sweep reads size bytes at base and folds every pointer-aligned value that
// lands inside a known module into the hit counts. Returns the bytes read.
//
// When candidate is non-nil it also records the highest-addressed hit outside
// startupModules. Offsets are walked in ascending order and the caller passes
// the low end of the window, so the last such hit wins — on the startup window
// that is the outermost frame, which is the thread's own start routine.
func (i *Inspector) sweep(base, size uintptr, candidate *uintptr) int {
	if size < stackAlign {
		return 0
	}
	if size > uintptr(len(i.buf)) {
		size = uintptr(len(i.buf))
	}

	// A partial copy is normal: the sweep window can run into a decommitted
	// guard page. Whatever was read before the fault is still real stack, so the
	// error is ignored and the byte count is what decides.
	var read uintptr
	windows.ReadProcessMemory(i.handle, base, &i.buf[0], size, &read)
	if read == 0 {
		return 0
	}
	if read > size {
		read = size
	}

	i.fold(i.buf[:read], candidate)

	return int(read)
}

// fold is the sweep's parsing half, split out from the read so it can be tested
// without a live process. Every pointer-aligned value that lands inside a known
// module counts as a frame; values are walked from the low address up, so the
// last non-startup hit is the highest-addressed one.
func (i *Inspector) fold(buf []byte, candidate *uintptr) {
	for offset := 0; offset+stackAlign <= len(buf); offset += stackAlign {
		value := uintptr(binary.LittleEndian.Uint64(buf[offset:]))
		if !UserAddress(value) {
			continue
		}

		name, _ := i.table.Resolve(value)
		if name == "" {
			continue
		}
		i.hits[name]++

		if candidate != nil && !startupModules[name] {
			*candidate = value
		}
	}
}

// rank orders the swept modules by how many frames each accounts for, dropping
// the ones supported by a single pointer.
func (i *Inspector) rank() []string {
	type hit struct {
		name  string
		count int
	}

	hits := make([]hit, 0, len(i.hits))
	for name, count := range i.hits {
		if count >= i.limits.MinHits {
			hits = append(hits, hit{name, count})
		}
	}
	if len(hits) == 0 {
		return nil
	}

	sort.Slice(hits, func(a, b int) bool {
		if hits[a].count != hits[b].count {
			return hits[a].count > hits[b].count
		}
		return hits[a].name < hits[b].name // stable across ticks
	})
	if len(hits) > i.limits.MaxModules {
		hits = hits[:i.limits.MaxModules]
	}

	names := make([]string, len(hits))
	for k := range hits {
		names[k] = hits[k].name
	}

	return names
}

// executable reports whether addr points at committed, executable memory. It is
// what separates a recovered entry point from a stale pointer to a string.
func (i *Inspector) executable(addr uintptr) bool {
	var info windows.MemoryBasicInformation
	if err := windows.VirtualQueryEx(i.handle, addr, &info, unsafe.Sizeof(info)); err != nil {
		return false
	}

	return info.State == windows.MEM_COMMIT && info.Protect&pageExecutable != 0
}
