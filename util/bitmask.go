package util

import "fmt"

func CoreArrayToBitmask(cores []int) uintptr {
	var mask uintptr
	for _, core := range cores {
		if core < 0 || core >= 64 {
			panic("core index out of bounds: " + fmt.Sprint(core))
		}

		mask |= 1 << core
	}

	return mask
}
