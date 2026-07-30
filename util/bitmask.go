package util

import "fmt"

const (
	minCores = 0
	maxCores = 64
	// DefaultCpuSetBase is the CPU Set ID of logical CPU 0 on a single-group
	// machine: IDs start at 0x100 and increment by 1, so cpuSetID = base+index.
	DefaultCpuSetBase uint32 = 0x100
)

// cpuSetBase is the live base, seeded to the documented default and validated
// (or corrected) once at startup from GetSystemCpuSetInformation via
// SetCpuSetBase. The governor sets it before any thread CPU-set write, so
// treating it as package state is safe for the single-threaded tuning loop.
var cpuSetBase = DefaultCpuSetBase

// SetCpuSetBase overrides the assumed CPU Set base with a value probed from the
// OS. A zero base is ignored (keeps the default) since it would be nonsensical.
func SetCpuSetBase(base uint32) {
	if base > 0 {
		cpuSetBase = base
	}
}

// CpuSetBase returns the base CPU Set ID currently in use.
func CpuSetBase() uint32 { return cpuSetBase }

func CoreArrayToBitmask(cores []int) uintptr {
	var mask uintptr
	for _, core := range cores {
		if core < minCores || core >= maxCores {
			panic("core index out of bounds: " + fmt.Sprint(core))
		}

		mask |= 1 << core
	}

	return mask
}

func BitmaskToCoreArray(mask uintptr) []int {
	var cores []int
	for i := 0; i < maxCores; i++ {
		if (mask & (1 << i)) != 0 {
			cores = append(cores, i)
		}
	}

	return cores
}

func LogicalToCpuSetIDs(cores []int) []uint32 {
	ids := make([]uint32, len(cores))
	for i, core := range cores {
		if core < 0 {
			panic("core index must be non-negative: " + fmt.Sprint(core))
		}

		ids[i] = cpuSetBase + uint32(core)
	}

	return ids
}

func CPUSetIDsToLogical(ids []uint32) []int {
	cores := make([]int, len(ids))
	for i, id := range ids {
		if id < cpuSetBase {
			panic("CPU Set ID below base: " + fmt.Sprint(id))
		}

		cores[i] = int(id - cpuSetBase)
	}

	return cores
}
