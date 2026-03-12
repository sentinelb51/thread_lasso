package process

import (
	"ThreadOrchestra/util"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	WindowsProcessSetLimitedInformation = 0x2000
)

// SetCpuSets sets the CPU Sets (Soft Affinity) for a process by PID.
// cores is a list of zero-based logical CPU indices (same convention as affinity).
func SetCpuSets(pid uint32, cores []int) error {
	if len(cores) == 0 {
		return nil
	}

	// Convert zero-based logical CPU indices to Windows CPU Set IDs.
	ids := util.LogicalToCpuSetIDs(cores)

	// CPU Sets require less privilege than hard affinities.
	const access = WindowsProcessSetLimitedInformation

	handle, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return fmt.Errorf("failed to open process (PID: %d): %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// BOOL SetProcessDefaultCpuSets(HANDLE Process, const ULONG *CpuSetIds, ULONG CpuSetIdCount);
	ret, _, sysErr := procSetProcessDefaultCpuSets.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&ids[0])),
		uintptr(len(ids)),
	)

	if ret == 0 {
		return fmt.Errorf("SetProcessDefaultCpuSets failed: %w", sysErr)
	}

	return nil
}

// CpuSets returns the CPU Sets (soft affinities) assigned to a process by PID.
// A nil slice means no CPU Set restriction is applied.
func CpuSets(pid uint32) ([]int, error) {

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, fmt.Errorf("failed to open process (PID: %d): %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// How many CPU Sets are assigned to this process.
	var allocated uint32

	// First call with NULL buffer and 0 count.
	// Docs: returns TRUE + count=0 when no CPU sets are assigned;
	// returns FALSE + ERROR_INSUFFICIENT_BUFFER + required count when some are assigned.
	// BOOL GetProcessDefaultCpuSets(HANDLE, PULONG, ULONG, PULONG);
	ret, _, sysErr := procGetProcessDefaultCpuSets.Call(
		uintptr(handle),
		0, // NULL — we query only
		0, // 0 capacity
		uintptr(unsafe.Pointer(&allocated)),
	)

	if ret == 0 {
		// ERROR_INSUFFICIENT_BUFFER is the expected signal that CPU sets are assigned
		// and allocated now holds the required count. Any other error is genuine.
		if sysErr != windows.ERROR_INSUFFICIENT_BUFFER {
			return nil, fmt.Errorf("GetProcessDefaultCpuSets failed: %w", sysErr)
		}
	}

	if allocated == 0 {
		return nil, nil // No CPU Set restriction applied.
	}

	// Second call: retrieve the actual CPU Set IDs.
	ids := make([]uint32, allocated)

	ret, _, sysErr = procGetProcessDefaultCpuSets.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&ids[0])),
		uintptr(allocated),
		uintptr(unsafe.Pointer(&allocated)),
	)

	if ret == 0 {
		return nil, fmt.Errorf("GetProcessDefaultCpuSets failed: %w", sysErr)
	}

	// Convert Windows CPU Set IDs back to zero-based logical CPU indices.
	cores := util.CPUSetIDsToLogical(ids)

	return cores, nil
}
