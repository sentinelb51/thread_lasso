package process

import (
	"ThreadOrchestra/util"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SetAffinity restricts a process (by PID) to run only on the specified CPU cores.
func SetAffinity(pid uint32, cores []int) error {
	if len(cores) == 0 {
		return nil // Nothing to apply
	}

	// 1. Convert array of cores ([0, 1, 2]) into bitmask
	mask := util.CoreArrayToBitmask(cores)

	// PROCESS_SET_INFORMATION to set affinity
	// PROCESS_QUERY_INFORMATION sometimes required by Windows internals
	const access = windows.PROCESS_SET_INFORMATION | windows.PROCESS_QUERY_INFORMATION

	handle, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return fmt.Errorf("failed to open process (PID: %d): %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// Call() returns the return value, a potential secondary return, and an error.
	// Non-zero return value on success; 0 on failure.
	ret, _, err := procSetProcessAffinityMask.Call(uintptr(handle), mask)
	if ret == 0 {
		return fmt.Errorf("SetProcessAffinityMask failed: %w", err)
	}

	return nil
}

func Affinity(pid uint32) ([]int, error) {

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, fmt.Errorf("failed to open process (PID: %d): %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	var (
		processMask uintptr
		systemMask  uintptr
	)

	// BOOL GetProcessAffinityMask(HANDLE, PDWORD_PTR, PDWORD_PTR);
	ret, _, err := procGetProcessAffinityMask.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&processMask)),
		uintptr(unsafe.Pointer(&systemMask)),
	)

	if ret == 0 {
		return nil, fmt.Errorf("GetProcessAffinityMask failed: %w", err)
	}

	cores := util.BitmaskToCoreArray(processMask)
	return cores, nil
}
