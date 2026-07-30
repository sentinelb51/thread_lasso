//go:build windows && amd64

package governor

import "time"

// ThreadRow is one thread's line in the UI/report — an immutable copy, safe
// to hand to another goroutine.
type ThreadRow struct {
	TID          uint32
	Name         string // game-set thread description; "" when unnamed
	Module       string // "" in limited mode
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
