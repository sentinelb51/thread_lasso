//go:build windows && amd64

package thread

import (
	"ThreadOrchestra/process"
	"fmt"
)

// Original holds the pre-modification values of every field we changed on a
// thread. Fields are captured once, immediately before the first change to
// that field, and restored on shutdown/rollback. Pointer fields are nil when
// the field was never touched.
type Original struct {
	Priority        *int
	Affinity        *uintptr // previous hard-affinity mask
	CpuSetsCaptured bool
	CpuSets         []int // nil = thread had no per-thread CPU set; restore clears
	IoPriority      *int
	MemoryPriority  *uint32
	EcoQoSChanged   bool // no query API exists; revert returns control to the system
	IdealCpu        *process.ProcessorNumber
}

// original returns the entry's journal, creating it on first use.
func (e *Entry) original() *Original {
	if e.Original == nil {
		e.Original = &Original{}
	}

	return e.Original
}

// Touched reports whether any setting on this thread was modified.
func (e *Entry) Touched() bool {
	return e.Original != nil
}

// Restore re-applies every journaled value on a single thread. It keeps going
// on individual failures (the thread may have partially lost rights) and
// returns the combined error, clearing the journal on full success. Must be
// fast: it runs inside the ~5s CTRL_CLOSE shutdown window.
func (e *Entry) Restore() error {
	if e.Original == nil || e.Handle == 0 {
		return nil
	}

	var errs []error
	o := e.Original

	if o.Priority != nil {
		if err := process.SetThreadPriorityOf(e.Handle, *o.Priority); err != nil {
			errs = append(errs, err)
		}
	}

	if o.Affinity != nil {
		if _, err := process.SetThreadAffinity(e.Handle, *o.Affinity); err != nil {
			errs = append(errs, err)
		}
	}

	if o.CpuSetsCaptured {
		// nil original clears the per-thread assignment (process default applies).
		if err := process.SetThreadCpuSets(e.Handle, o.CpuSets); err != nil {
			errs = append(errs, err)
		}
	}

	if o.IoPriority != nil {
		if err := process.SetThreadIoPriority(e.Handle, *o.IoPriority); err != nil {
			errs = append(errs, err)
		}
	}

	if o.MemoryPriority != nil {
		if err := process.SetThreadMemoryPriority(e.Handle, *o.MemoryPriority); err != nil {
			errs = append(errs, err)
		}
	}

	if o.EcoQoSChanged {
		if err := process.ResetThreadEcoQoS(e.Handle); err != nil {
			errs = append(errs, err)
		}
	}

	if o.IdealCpu != nil {
		if err := process.RestoreThreadIdealProcessor(e.Handle, *o.IdealCpu); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("restore TID %d: %d of %d fields failed (first: %v)",
			e.Key.TID, len(errs), touchedFields(o), errs[0])
	}

	e.Original = nil
	return nil
}

func touchedFields(o *Original) int {
	n := 0
	if o.Priority != nil {
		n++
	}
	if o.Affinity != nil {
		n++
	}
	if o.CpuSetsCaptured {
		n++
	}
	if o.IoPriority != nil {
		n++
	}
	if o.MemoryPriority != nil {
		n++
	}
	if o.EcoQoSChanged {
		n++
	}
	if o.IdealCpu != nil {
		n++
	}

	return n
}

// RestoreAll reverts every touched thread in the cache. Returns the number of
// threads restored and any errors encountered.
func RestoreAll(cache *Cache) (int, []error) {
	restored := 0
	var errs []error

	for _, entry := range cache.Entries() {
		if !entry.Touched() {
			continue
		}

		if err := entry.Restore(); err != nil {
			errs = append(errs, err)
			continue
		}
		restored++
	}

	return restored, errs
}
