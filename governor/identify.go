//go:build windows && amd64

package governor

import (
	"time"

	"ThreadOrchestra/config"
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
)

// Reloads exist because a protected process finishes mapping its real modules
// well after the point where a scanner first sees it. They are throttled and
// capped by config.Scan: a recovery path, not a poll.
//
// Stack sweeps cost a couple of ReadProcessMemory calls per thread. Refreshing
// every thread every tick would read megabytes a second for information that
// changes on the scale of what a thread is *for*, not what it is doing right
// now, so scan.stack_scans_per_tick spreads a full pass over several ticks.

// EntrySource records which route produced a thread's start address. The
// governor reports the distribution once per session, because on a process that
// hides them the answer to "which sources still work" is the whole diagnosis.
type EntrySource int

const (
	EntryNone   EntrySource = iota
	EntryWin32              // SYSTEM_EXTENDED_THREAD_INFORMATION.Win32StartAddress
	EntryKernel             // SYSTEM_THREAD_INFORMATION.StartAddress
	EntryQuery              // NtQueryInformationThread on the thread handle
	EntryStack              // recovered from the thread's own startup frames
)

var entrySourceNames = map[EntrySource]string{
	EntryNone:   "none",
	EntryWin32:  "Win32StartAddress",
	EntryKernel: "kernel StartAddress",
	EntryQuery:  "handle query",
	EntryStack:  "stack scan",
}

func (s EntrySource) String() string { return entrySourceNames[s] }

// identity is everything we have managed to learn about one thread that is not
// a counter: where it started, and what its stack says it does.
type identity struct {
	Entry        uintptr
	Source       EntrySource
	Module       string
	ModuleOffset uintptr
	Stack        []string
	GUI          bool

	queried bool      // the handle query has been tried; it only ever needs trying once
	stackAt time.Time // last sweep; zero means never
}

// Identifier owns the process-memory view for one session and caches what it
// learns per thread. Full mode only.
//
// The four start-address routes are tried in cost order, and every one of them
// exists because the one before it can be defeated:
//
//  1. Win32StartAddress from the system snapshot — free, and the field the
//     owning process can overwrite through NtSetInformationThread;
//  2. the kernel's own StartAddress from the same snapshot — also free, and not
//     settable from user mode, but the kernel filters this information class in
//     its own right;
//  3. NtQueryInformationThread against a handle we already hold — a syscall per
//     thread, but a different code path with different filtering;
//  4. the thread's startup stack frames — the arguments its creation was made
//     with, which nothing can rewrite after the fact.
type Identifier struct {
	inspector *process.Inspector
	known     map[thread.Key]*identity
	scan      config.Scan

	lastLoad time.Time
	reloads  int
	misses   int // unresolved non-zero addresses since the last reload
	scans    int // stack sweeps performed this tick
}

// NewIdentifier opens the target for inspection and snapshots its modules.
// Returns an error — and the governor falls back to behavioural classification
// — when the process cannot be opened, e.g. limited rights or anti-cheat.
func NewIdentifier(pid uint32, scan config.Scan) (*Identifier, error) {
	inspector, err := process.OpenInspector(pid)
	if err != nil {
		return nil, err
	}

	id := &Identifier{
		inspector: inspector,
		known:     make(map[thread.Key]*identity),
		lastLoad:  time.Now(),
	}
	id.Retune(scan)

	return id, nil
}

// Retune adopts new scanning budgets. Anything already learned is kept: the
// budgets govern how much work is done per tick, not what the answers were.
func (id *Identifier) Retune(scan config.Scan) {
	if id == nil {
		return
	}

	id.scan = scan
	id.inspector.SetLimits(process.ScanLimits{
		StartupWindow: scan.StackStartupWindowKB << 10,
		ActiveWindow:  scan.StackActiveWindowKB << 10,
		MinHits:       scan.StackMinHits,
		MaxModules:    scan.StackMaxModules,
	})
}

func (id *Identifier) Close() {
	if id == nil {
		return
	}
	id.inspector.Close()
}

// BeginTick resets the per-tick stack-sweep budget and adopts the tick's
// scanning settings, so an edit in the settings panel takes effect immediately
// rather than at the next session.
func (id *Identifier) BeginTick(scan config.Scan) {
	if id == nil {
		return
	}

	if scan != id.scan {
		id.Retune(scan)
	}
	id.scans = 0
}

// Identify returns what is known about one thread, doing as little work as the
// cache allows. Safe on a nil Identifier (limited mode), where it reports
// nothing and every caller degrades to behaviour-only classification.
func (id *Identifier) Identify(key thread.Key, sample *ThreadSample) identity {
	if id == nil {
		return identity{}
	}

	known, ok := id.known[key]
	if !ok {
		known = &identity{}
		id.known[key] = known
	}

	id.recoverEntry(known, sample)
	id.sweepStack(known, sample)

	// Resolving is retried while it fails, because the address can be owned by a
	// module that is not mapped yet — see Refresh.
	if known.Entry != 0 && known.Module == "" {
		known.Module, known.ModuleOffset = id.inspector.Resolve(known.Entry)
		if known.Module == "" {
			id.misses++
		}
	}

	return *known
}

// recoverEntry walks the start-address routes in cost order, stopping at the
// first that yields a user-mode address. The two snapshot fields are re-read
// every tick because they are free; the handle query runs at most once.
func (id *Identifier) recoverEntry(known *identity, sample *ThreadSample) {
	if known.Entry != 0 {
		return
	}

	switch {
	case process.UserAddress(sample.Win32StartAddress):
		known.Entry, known.Source = sample.Win32StartAddress, EntryWin32
		return
	case process.UserAddress(sample.StartAddress):
		known.Entry, known.Source = sample.StartAddress, EntryKernel
		return
	}

	if known.queried {
		return
	}
	entry := sample.Entry
	if entry == nil || entry.Handle == 0 || !entry.Capabilities.QueryStartAddress {
		return // no handle yet; try again once the cache opens one
	}

	known.queried = true
	if address, err := process.ThreadStartAddress(entry.Handle); err == nil &&
		process.UserAddress(address) {
		known.Entry, known.Source = address, EntryQuery
	}
}

// sweepStack refreshes a thread's stack fingerprint when its turn comes round,
// and takes the recovered entry point if nothing cheaper found one.
func (id *Identifier) sweepStack(known *identity, sample *ThreadSample) {
	interval := time.Duration(id.scan.StackIntervalS * float64(time.Second))
	if id.scans >= id.scan.StackScansPerTick || time.Since(known.stackAt) < interval {
		return
	}

	id.scans++
	first := known.stackAt.IsZero()
	known.stackAt = time.Now()

	trace := id.inspector.Trace(&sample.ThreadSnapshot)
	if trace.Read == 0 {
		return
	}
	known.Stack = trace.Modules

	if known.Entry == 0 && trace.Entry != 0 {
		known.Entry, known.Source = trace.Entry, EntryStack
	}
	// The TEB flag is set once by the kernel and never cleared, so one read per
	// thread is enough — it rides along with the first sweep.
	if first {
		known.GUI = id.inspector.GuiThread(sample.TebBase)
	}
}

// Refresh reloads the module table when addresses went unresolved since the
// last attempt. Call it once per tick; it does nothing until the throttle
// interval has passed, and gives up once reloads stop finding new modules.
func (id *Identifier) Refresh() {
	if id == nil || id.misses == 0 || id.reloads >= id.scan.ModuleReloadLimit {
		return
	}
	if time.Since(id.lastLoad) < time.Duration(id.scan.ModuleReloadIntervalS*float64(time.Second)) {
		return
	}

	id.lastLoad = time.Now()
	id.misses = 0

	before := id.inspector.ModuleCount()
	after, err := id.inspector.Reload()
	if err != nil || after <= before {
		// Either the reload failed, or nothing new appeared and the misses are
		// addresses no module owns — manually mapped code, which is exactly what
		// a protection leaves behind. Burn a retry either way.
		id.reloads++
		return
	}

	// A bigger table can resolve addresses that failed before. Nothing needs
	// resetting for that: Identify re-runs Resolve every tick for any thread
	// whose module is still unknown, and the recovered address itself is worth
	// keeping either way — it groups a worker pool into a cohort whether or not
	// we can put a name to it.
	id.reloads = 0
}

// Prune drops cached identities for threads that no longer exist.
func (id *Identifier) Prune(live map[thread.Key]*Series) {
	if id == nil {
		return
	}
	for key := range id.known {
		if _, ok := live[key]; !ok {
			delete(id.known, key)
		}
	}
}

// ModuleCount reports how many module ranges the index currently covers.
func (id *Identifier) ModuleCount() int {
	if id == nil {
		return 0
	}
	return id.inspector.ModuleCount()
}

// Sources counts the threads attributed to each recovery route. This is the
// diagnostic the governor reports: on an unprotected process everything lands
// in EntryWin32, and every thread that lands anywhere else is a thread whose
// origin something took the trouble to hide.
func (id *Identifier) Sources() map[EntrySource]int {
	if id == nil {
		return nil
	}

	counts := make(map[EntrySource]int, len(entrySourceNames))
	for _, known := range id.known {
		counts[known.Source]++
	}

	return counts
}
