//go:build windows && amd64

package process

import (
	"ThreadOrchestra/util"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Thread access rights (x/sys/windows only defines a subset).
const (
	ThreadQueryLimitedInformation = 0x0800
	ThreadSetLimitedInformation   = 0x0400
	ThreadQueryInformation        = 0x0040
	ThreadSetInformation          = 0x0020
)

// THREAD_INFORMATION_CLASS values for SetThreadInformation.
const (
	threadMemoryPriorityClass  = 0 // ThreadMemoryPriority
	threadPowerThrottlingClass = 3 // ThreadPowerThrottling
)

// THREADINFOCLASS value for Nt{Query,Set}InformationThread. Undocumented but
// stable; matches System Informer's ThreadIoPriority.
const threadIoPriorityClass = 33

const threadPowerThrottlingExecutionSpeed = 0x1 // THREAD_POWER_THROTTLING_EXECUTION_SPEED

// GetThreadPriority's failure sentinel (THREAD_PRIORITY_ERROR_RETURN).
const threadPriorityErrorReturn = 0x7FFFFFFF

// OpenThreadHandle opens a thread with the requested access rights.
func OpenThreadHandle(tid uint32, access uint32) (windows.Handle, error) {
	return windows.OpenThread(access, false, tid)
}

// ThreadDescription returns the thread's name set via SetThreadDescription
// (empty for unnamed threads). Requires THREAD_QUERY_LIMITED_INFORMATION.
func ThreadDescription(handle windows.Handle) (string, error) {
	var buffer *uint16

	// HRESULT GetThreadDescription(HANDLE, PWSTR *);
	ret, _, _ := procGetThreadDescription.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&buffer)),
	)

	if int32(ret) < 0 { // FAILED(hr)
		return "", fmt.Errorf("GetThreadDescription failed: HRESULT 0x%08x", uint32(ret))
	}

	if buffer == nil {
		return "", nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(buffer)))

	return windows.UTF16PtrToString(buffer), nil
}

// ThreadCycles returns the cumulative CPU cycle count for the thread.
// Requires THREAD_QUERY_LIMITED_INFORMATION.
func ThreadCycles(handle windows.Handle) (uint64, error) {
	var cycles uint64

	// BOOL QueryThreadCycleTime(HANDLE, PULONG64);
	ret, _, err := procQueryThreadCycleTime.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&cycles)),
	)

	if ret == 0 {
		return 0, fmt.Errorf("QueryThreadCycleTime failed: %w", err)
	}

	return cycles, nil
}

// ThreadPriorityOf returns the thread's base priority relative to the process
// priority class (THREAD_PRIORITY_* values).
func ThreadPriorityOf(handle windows.Handle) (int, error) {
	ret, _, err := procGetThreadPriority.Call(uintptr(handle))
	if int32(ret) == threadPriorityErrorReturn {
		return 0, fmt.Errorf("GetThreadPriority failed: %w", err)
	}

	return int(int32(ret)), nil
}

// SetThreadPriorityOf sets the thread's relative priority. Requires
// THREAD_SET_LIMITED_INFORMATION.
func SetThreadPriorityOf(handle windows.Handle, priority int) error {
	ret, _, err := procSetThreadPriority.Call(uintptr(handle), uintptr(priority))
	if ret == 0 {
		return fmt.Errorf("SetThreadPriority(%d) failed: %w", priority, err)
	}

	return nil
}

// ThreadSelectedCpuSets returns the thread's explicit CPU Set assignment as
// zero-based logical CPU indices; nil means no per-thread assignment.
func ThreadSelectedCpuSets(handle windows.Handle) ([]int, error) {
	var required uint32

	// Same two-call pattern as GetProcessDefaultCpuSets (see cpusets.go).
	ret, _, sysErr := procGetThreadSelectedCpuSets.Call(
		uintptr(handle),
		0,
		0,
		uintptr(unsafe.Pointer(&required)),
	)

	if ret == 0 {
		if sysErr != windows.ERROR_INSUFFICIENT_BUFFER {
			return nil, fmt.Errorf("GetThreadSelectedCpuSets failed: %w", sysErr)
		}
	}

	if required == 0 {
		return nil, nil
	}

	ids := make([]uint32, required)

	ret, _, sysErr = procGetThreadSelectedCpuSets.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&ids[0])),
		uintptr(required),
		uintptr(unsafe.Pointer(&required)),
	)

	if ret == 0 {
		return nil, fmt.Errorf("GetThreadSelectedCpuSets failed: %w", sysErr)
	}

	return util.CPUSetIDsToLogical(ids), nil
}

// SetThreadCpuSets assigns CPU Sets to a thread, overriding the process
// default. An empty core list clears the assignment (reverting to the process
// default). Requires THREAD_SET_LIMITED_INFORMATION.
func SetThreadCpuSets(handle windows.Handle, cores []int) error {
	var (
		idsPtr uintptr
		count  uintptr
	)

	if len(cores) > 0 {
		ids := util.LogicalToCpuSetIDs(cores)
		idsPtr = uintptr(unsafe.Pointer(&ids[0]))
		count = uintptr(len(ids))
	}

	// BOOL SetThreadSelectedCpuSets(HANDLE, const ULONG *, ULONG);
	ret, _, err := procSetThreadSelectedCpuSets.Call(uintptr(handle), idsPtr, count)
	if ret == 0 {
		return fmt.Errorf("SetThreadSelectedCpuSets failed: %w", err)
	}

	return nil
}

// SetThreadAffinity sets the thread's hard affinity mask and returns the
// previous mask (for the revert journal). A zero return means failure.
// SetThreadAffinityMask needs THREAD_SET_LIMITED_INFORMATION plus
// THREAD_QUERY_LIMITED_INFORMATION, both granted in limited mode.
func SetThreadAffinity(handle windows.Handle, mask uintptr) (uintptr, error) {
	// DWORD_PTR SetThreadAffinityMask(HANDLE, DWORD_PTR);
	previous, _, err := procSetThreadAffinityMask.Call(uintptr(handle), mask)
	if previous == 0 {
		return 0, fmt.Errorf("SetThreadAffinityMask(0x%x) failed: %w", mask, err)
	}

	return previous, nil
}

// ProcessorNumber mirrors PROCESSOR_NUMBER (not defined in x/sys).
type ProcessorNumber struct {
	Group    uint16
	Number   uint8
	Reserved uint8
}

// SetThreadIdealProcessor sets the scheduler's preferred processor for the
// thread and returns the previous ideal processor (for the revert journal).
// Single processor group assumed (group 0). Requires THREAD_SET_INFORMATION.
func SetThreadIdealProcessor(handle windows.Handle, cpu int) (ProcessorNumber, error) {
	ideal := ProcessorNumber{Group: 0, Number: uint8(cpu)}
	var previous ProcessorNumber

	// BOOL SetThreadIdealProcessorEx(HANDLE, PPROCESSOR_NUMBER, PPROCESSOR_NUMBER);
	ret, _, err := procSetThreadIdealProcessorEx.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&ideal)),
		uintptr(unsafe.Pointer(&previous)),
	)

	if ret == 0 {
		return previous, fmt.Errorf("SetThreadIdealProcessorEx(%d) failed: %w", cpu, err)
	}

	return previous, nil
}

// RestoreThreadIdealProcessor re-applies a previously captured ideal
// processor.
func RestoreThreadIdealProcessor(handle windows.Handle, previous ProcessorNumber) error {
	ret, _, err := procSetThreadIdealProcessorEx.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&previous)),
		0,
	)

	if ret == 0 {
		return fmt.Errorf("SetThreadIdealProcessorEx(restore) failed: %w", err)
	}

	return nil
}

// ThreadMemoryPriority returns the thread's current memory (page) priority.
// Requires THREAD_QUERY_INFORMATION.
func ThreadMemoryPriority(handle windows.Handle) (uint32, error) {
	var priority uint32

	ret, _, err := procGetThreadInformation.Call(
		uintptr(handle),
		threadMemoryPriorityClass,
		uintptr(unsafe.Pointer(&priority)),
		unsafe.Sizeof(priority),
	)

	if ret == 0 {
		return 0, fmt.Errorf("GetThreadInformation(memory priority) failed: %w", err)
	}

	return priority, nil
}

// SetThreadMemoryPriority sets the thread's memory (page) priority, 1-5 where
// 5 = MEMORY_PRIORITY_NORMAL. Requires THREAD_SET_INFORMATION.
func SetThreadMemoryPriority(handle windows.Handle, priority uint32) error {
	// MEMORY_PRIORITY_INFORMATION { ULONG MemoryPriority; }
	info := priority

	ret, _, err := procSetThreadInformation.Call(
		uintptr(handle),
		threadMemoryPriorityClass,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)

	if ret == 0 {
		return fmt.Errorf("SetThreadInformation(memory priority %d) failed: %w", priority, err)
	}

	return nil
}

// threadPowerThrottlingState mirrors THREAD_POWER_THROTTLING_STATE.
type threadPowerThrottlingState struct {
	Version     uint32
	ControlMask uint32
	StateMask   uint32
}

// SetThreadEcoQoS opts a thread into (enable=true) EcoQoS execution-speed
// throttling. Requires THREAD_SET_INFORMATION.
func SetThreadEcoQoS(handle windows.Handle, enable bool) error {
	state := threadPowerThrottlingState{
		Version:     1,
		ControlMask: threadPowerThrottlingExecutionSpeed,
	}
	if enable {
		state.StateMask = threadPowerThrottlingExecutionSpeed
	}

	return setThreadPowerThrottling(handle, &state)
}

// ResetThreadEcoQoS returns EcoQoS control to the system. This is the correct
// revert: ControlMask=0 means "system decides", whereas ControlMask set with
// StateMask=0 would force throttling OFF, which is not the pre-change state.
func ResetThreadEcoQoS(handle windows.Handle) error {
	return setThreadPowerThrottling(handle, &threadPowerThrottlingState{Version: 1})
}

func setThreadPowerThrottling(handle windows.Handle, state *threadPowerThrottlingState) error {
	ret, _, err := procSetThreadInformation.Call(
		uintptr(handle),
		threadPowerThrottlingClass,
		uintptr(unsafe.Pointer(state)),
		unsafe.Sizeof(*state),
	)

	if ret == 0 {
		return fmt.Errorf("SetThreadInformation(power throttling) failed: %w", err)
	}

	return nil
}

// ThreadIoPriority returns the thread's I/O priority hint (0 = Very Low,
// 1 = Low, 2 = Normal, 3 = High).
func ThreadIoPriority(handle windows.Handle) (int, error) {
	var priority uint32

	// NTSTATUS NtQueryInformationThread(HANDLE, THREADINFOCLASS, PVOID, ULONG, PULONG);
	status, _, _ := procNtQueryInformationThread.Call(
		uintptr(handle),
		threadIoPriorityClass,
		uintptr(unsafe.Pointer(&priority)),
		unsafe.Sizeof(priority),
		0,
	)

	if status != 0 {
		return 0, fmt.Errorf("NtQueryInformationThread(io priority) failed: NTSTATUS 0x%08x", uint32(status))
	}

	return int(priority), nil
}

// SetThreadIoPriority sets the thread's I/O priority hint. Raising above
// Normal (2) requires SeIncreaseBasePriorityPrivilege. Requires
// THREAD_SET_INFORMATION.
func SetThreadIoPriority(handle windows.Handle, priority int) error {
	value := uint32(priority)

	status, _, _ := procNtSetInformationThread.Call(
		uintptr(handle),
		threadIoPriorityClass,
		uintptr(unsafe.Pointer(&value)),
		unsafe.Sizeof(value),
	)

	if status != 0 {
		return fmt.Errorf("NtSetInformationThread(io priority %d) failed: NTSTATUS 0x%08x", priority, uint32(status))
	}

	return nil
}
