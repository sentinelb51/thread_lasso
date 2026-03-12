package util

import "fmt"

const (
	minCores = 0
	maxCores = 64
	// cpuSetBase = CPU Sets version of logical CPU 0.
	// CPU Set IDs start at 0x100 (256) and increment by 1, so: cpuSetID = 0x100 + logicalIndex.
	// Maybe get this value from GetSystemCpuSetInformation()?
	cpuSetBase uint32 = 0x100
)

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
