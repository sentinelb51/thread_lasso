//go:build windows && amd64

package governor

import (
	"time"

	"ThreadOrchestra/process"
)

// A protected process finishes mapping its real modules well after the point
// where a scanner first sees it, so one snapshot at attach time is not enough.
// Reloads are throttled and capped: they are a recovery path, not a poll.
const (
	moduleReloadInterval = 15 * time.Second
	moduleReloadLimit    = 8
)

// ModuleIndex resolves thread start addresses to module names, reloading the
// underlying table while addresses are still failing to resolve.
type ModuleIndex struct {
	pid   uint32
	table *process.ModuleTable

	lastLoad time.Time
	reloads  int
	misses   int // unresolved non-zero addresses since the last reload
}

// NewModuleResolver builds a full-mode start-address → module-name resolver by
// snapshotting the target's loaded modules. Returns an error (and the governor
// falls back to behavioral classification) when the process cannot be opened
// for module enumeration — e.g. limited rights or anti-cheat.
func NewModuleResolver(pid uint32) (*ModuleIndex, error) {
	table, err := process.LoadModuleTable(pid)
	if err != nil {
		return nil, err
	}
	return &ModuleIndex{pid: pid, table: table, lastLoad: time.Now()}, nil
}

// Resolve returns the module owning addr and the offset into it. A zero
// address — every thread of a process that scrubs its start addresses, if the
// kernel field is scrubbed too — resolves to "" without counting as a miss:
// there is nothing a reload could fix.
func (m *ModuleIndex) Resolve(addr uintptr) (name string, offset uintptr) {
	if m == nil || addr == 0 {
		return "", 0
	}
	name, offset = m.table.Resolve(addr)
	if name == "" {
		m.misses++
	}
	return name, offset
}

// Refresh reloads the module table when addresses went unresolved since the
// last attempt. Call it once per tick; it does nothing until the throttle
// interval has passed, and gives up once reloads stop finding new modules.
func (m *ModuleIndex) Refresh() {
	if m == nil || m.misses == 0 || m.reloads >= moduleReloadLimit {
		return
	}
	if time.Since(m.lastLoad) < moduleReloadInterval {
		return
	}

	m.lastLoad = time.Now()
	m.misses = 0

	table, err := process.LoadModuleTable(m.pid)
	if err != nil {
		m.reloads++ // count failures too, so a permanently denied handle stops retrying
		return
	}
	if table.Len() <= m.table.Len() {
		// Nothing new appeared; the misses are addresses no module owns
		// (manually mapped code), not a stale table. Burn a retry.
		m.reloads++
	} else {
		m.reloads = 0
	}
	m.table = table
}

// ModuleCount reports how many module ranges the index currently covers.
func (m *ModuleIndex) ModuleCount() int {
	if m == nil || m.table == nil {
		return 0
	}
	return m.table.Len()
}
