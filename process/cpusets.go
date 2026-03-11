package process

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const WindowsProcessSetLimitedInformation = 0x2000

// SetCpuSets sets the CPU Sets (Soft Affinity) for a process by PID.
func SetCpuSets(pid uint32, cores []uint32) error {
	if len(cores) == 0 {
		return nil
	}

	// CPU Sets require less privilege than hard affinities
	const access = WindowsProcessSetLimitedInformation

	handle, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return fmt.Errorf("failed to open process (PID: %d): %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// BOOL SetProcessDefaultCpuSets(HANDLE Process, const ULONG *CpuSetIds, ULONG CpuSetIdCount);
	// unsafe.Pointer to pass memory address of the first element in []cores.
	ret, _, sysErr := procSetProcessDefaultCpuSets.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&cores[0])),
		uintptr(len(cores)),
	)

	if ret == 0 {
		return fmt.Errorf("SetProcessDefaultCpuSets failed: %w", sysErr)
	}

	return nil
}
