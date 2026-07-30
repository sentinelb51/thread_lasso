//go:build windows && amd64

package governor

import (
	"fmt"
	"time"
)

// ThreadRow is one thread's line in the UI/report — an immutable copy, safe
// to hand to another goroutine.
type ThreadRow struct {
	TID          uint32
	Name         string  // game-set thread description; "" when unnamed
	Module       string  // "" in limited mode, or when the entry point is unresolvable
	ModuleOffset uintptr // offset of the entry point into Module
	Ordinal      int     // 1-based, stable index among the threads sharing this role
	CreateTime   int64   // FILETIME ticks; orders the synthetic role labels
	CyclesRate   float64
	SwitchRate   float64
	Quantum      float64 // cycles per switch
	Priority     int32   // dynamic
	BasePriority int32
	WaitProfile  string // dominant wait reason, e.g. "WrQueue 92%"
	Role         string
	Confidence   float64
	Bucket       string
	Stable       bool
	Applied      string // last action applied by the governor ("" = none)
	Starved      bool   // high ready-state ratio
}

// Identity is the human-facing name for a thread, best evidence first:
//
//  1. the name the game passed to SetThreadDescription. Engines rarely bother —
//     under Overwatch only middleware like Bink names its threads;
//  2. the module and offset that own the entry point, the same thing a debugger
//     shows ("overwatch.exe+0x2678fa0"). Every thread of a worker pool shares
//     one, which makes it the handle to write an auto.force rule against;
//  3. a synthetic label from the classified role, so a thread whose entry point
//     was scrubbed is still something you can refer to across ticks.
//
// It never surfaces a bare hex address: without a module to anchor it, an
// entry point is a number that identifies nothing.
func (r ThreadRow) Identity() string {
	switch {
	case r.Name != "":
		return r.Name
	case r.Module != "":
		return fmt.Sprintf("%s+0x%x", r.Module, r.ModuleOffset)
	case r.Role != "" && r.Role != "unknown" && r.Ordinal > 0:
		return fmt.Sprintf("%s #%d", r.Role, r.Ordinal)
	default:
		return "—"
	}
}

// Identified reports whether the thread has a name the process itself supplied
// — a description or a resolved module — rather than one we invented.
func (r ThreadRow) Identified() bool { return r.Name != "" || r.Module != "" }

// ViewModel is the full per-tick state published to the UI/report.
type ViewModel struct {
	At           time.Time
	GameName     string
	PID          uint32
	Phase        string
	Optimisation string
	Aggression   string
	AccessMode   string
	Focused      bool     // game holds the foreground; false = metrics/actuation paused
	Warnings     []string // capability / privilege warnings, set once at start
	TotalCycles  float64  // process cycles/sec
	ReadRate     float64  // disk bytes/sec
	ThreadCount  int
	Rows         []ThreadRow // sorted by CyclesRate descending
}
