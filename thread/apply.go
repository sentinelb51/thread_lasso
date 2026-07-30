//go:build windows && amd64

package thread

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/process"
)

// This file is the per-thread "how": it captures the pre-change value into the
// revert journal (once, the first time a field is touched) and then writes the
// new value. Policy — which threads to change and to what — lives in the
// governor. Every method is a no-op that returns (false, nil) when the handle
// lacks the required capability, so callers can invoke them unconditionally.

// RaisePriorityTo sets the relative thread priority to target, but only when
// the thread is currently below it (promotions never lower a thread).
func (e *Entry) RaisePriorityTo(target int) (bool, error) {
	return e.movePriority(target, +1)
}

// LowerPriorityTo sets the relative thread priority to target, but only when
// the thread is currently above it (demotions never raise a thread).
func (e *Entry) LowerPriorityTo(target int) (bool, error) {
	return e.movePriority(target, -1)
}

func (e *Entry) movePriority(target, dir int) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetPriority {
		return false, nil
	}

	current, err := process.ThreadPriorityOf(e.Handle)
	if err != nil {
		return false, err
	}
	if (dir > 0 && current >= target) || (dir < 0 && current <= target) {
		return false, nil
	}

	if e.original().Priority == nil {
		captured := current
		e.original().Priority = &captured
	}

	if err := process.SetThreadPriorityOf(e.Handle, target); err != nil {
		return false, err
	}
	return true, nil
}

// SetPriorityTo sets the relative thread priority to an exact value regardless
// of the current one — used by manual rules, which are absolute overrides
// rather than the directional nudges the auto governor makes.
func (e *Entry) SetPriorityTo(target int) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetPriority {
		return false, nil
	}

	current, err := process.ThreadPriorityOf(e.Handle)
	if err != nil {
		return false, err
	}
	if current == target {
		return false, nil
	}

	if e.original().Priority == nil {
		captured := current
		e.original().Priority = &captured
	}

	if err := process.SetThreadPriorityOf(e.Handle, target); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyAffinity pins the thread to a hard-affinity mask built from the given
// logical CPU indices. Out-of-range cores are dropped; an empty resulting mask
// is a no-op. SetThreadAffinityMask needs only limited rights.
func (e *Entry) ApplyAffinity(cores []int) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetPriority {
		return false, nil
	}

	var mask uintptr
	for _, core := range cores {
		if core >= 0 && core < 64 {
			mask |= 1 << uint(core)
		}
	}
	if mask == 0 {
		return false, nil
	}

	if e.original().Affinity == nil {
		previous, err := process.SetThreadAffinity(e.Handle, mask)
		if err != nil {
			return false, err
		}
		captured := previous
		e.original().Affinity = &captured
		return true, nil
	}

	if _, err := process.SetThreadAffinity(e.Handle, mask); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyRule enacts one manual config.Thread rule and returns short labels for
// each field it actually changed (empty when nothing applied — e.g. the handle
// lacks the capability). Every field is optional and applied independently.
func (e *Entry) ApplyRule(rule config.Thread) []string {
	var applied []string

	if rule.Priority != nil {
		if ok, _ := e.SetPriorityTo(*rule.Priority); ok {
			applied = append(applied, "prio")
		}
	}
	if rule.IOPriority != nil {
		if ok, _ := e.ApplyIoPriority(*rule.IOPriority); ok {
			applied = append(applied, "io")
		}
	}
	if len(rule.CPUSets) > 0 {
		if ok, _ := e.ApplyCpuSets(rule.CPUSets); ok {
			applied = append(applied, "cpuset")
		}
	}
	if len(rule.Affinity) > 0 {
		if ok, _ := e.ApplyAffinity(rule.Affinity); ok {
			applied = append(applied, "affinity")
		}
	}

	return applied
}

// ApplyCpuSets overrides the thread's per-thread CPU-set assignment.
func (e *Entry) ApplyCpuSets(cores []int) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetCpuSets {
		return false, nil
	}

	if !e.original().CpuSetsCaptured {
		previous, err := process.ThreadSelectedCpuSets(e.Handle)
		if err != nil {
			return false, err
		}
		e.original().CpuSets = previous
		e.original().CpuSetsCaptured = true
	}

	if err := process.SetThreadCpuSets(e.Handle, cores); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyMemoryPriority lowers (or raises) the thread's memory priority.
func (e *Entry) ApplyMemoryPriority(priority uint32) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetMemoryPriority {
		return false, nil
	}

	if e.original().MemoryPriority == nil {
		previous, err := process.ThreadMemoryPriority(e.Handle)
		if err != nil {
			return false, err
		}
		captured := previous
		e.original().MemoryPriority = &captured
	}

	if err := process.SetThreadMemoryPriority(e.Handle, priority); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyIoPriority sets the thread's I/O priority hint (0..3).
func (e *Entry) ApplyIoPriority(priority int) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetIoPriority {
		return false, nil
	}

	if e.original().IoPriority == nil {
		previous, err := process.ThreadIoPriority(e.Handle)
		if err != nil {
			return false, err
		}
		captured := previous
		e.original().IoPriority = &captured
	}

	if err := process.SetThreadIoPriority(e.Handle, priority); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyEcoQoS enables EcoQoS (power throttling) for the thread. There is no
// query API, so the journal only records that a reset is owed on teardown.
func (e *Entry) ApplyEcoQoS() (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetEcoQoS {
		return false, nil
	}

	if err := process.SetThreadEcoQoS(e.Handle, true); err != nil {
		return false, err
	}
	e.original().EcoQoSChanged = true
	return true, nil
}

// ApplyIdealProcessor steers the scheduler's preferred processor. The first
// call returns the previous ideal processor, which is what we journal.
func (e *Entry) ApplyIdealProcessor(cpu int) (bool, error) {
	if e.Handle == 0 || !e.Capabilities.SetIdealCpu {
		return false, nil
	}

	if e.original().IdealCpu == nil {
		previous, err := process.SetThreadIdealProcessor(e.Handle, cpu)
		if err != nil {
			return false, err
		}
		captured := previous
		e.original().IdealCpu = &captured
		return true, nil
	}

	if _, err := process.SetThreadIdealProcessor(e.Handle, cpu); err != nil {
		return false, err
	}
	return true, nil
}
