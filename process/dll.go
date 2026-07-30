package process

import "golang.org/x/sys/windows"

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procSetProcessAffinityMask   = kernel32.NewProc("SetProcessAffinityMask")
	procGetProcessAffinityMask   = kernel32.NewProc("GetProcessAffinityMask")
	procSetProcessDefaultCpuSets = kernel32.NewProc("SetProcessDefaultCpuSets")
	procGetProcessDefaultCpuSets = kernel32.NewProc("GetProcessDefaultCpuSets")

	procGetThreadDescription             = kernel32.NewProc("GetThreadDescription")
	procGetThreadPriority                = kernel32.NewProc("GetThreadPriority")
	procSetThreadPriority                = kernel32.NewProc("SetThreadPriority")
	procGetThreadTimes                   = kernel32.NewProc("GetThreadTimes")
	procQueryThreadCycleTime             = kernel32.NewProc("QueryThreadCycleTime")
	procSetThreadInformation             = kernel32.NewProc("SetThreadInformation")
	procGetThreadInformation             = kernel32.NewProc("GetThreadInformation")
	procGetThreadSelectedCpuSets         = kernel32.NewProc("GetThreadSelectedCpuSets")
	procSetThreadSelectedCpuSets         = kernel32.NewProc("SetThreadSelectedCpuSets")
	procSetThreadAffinityMask            = kernel32.NewProc("SetThreadAffinityMask")
	procSetThreadIdealProcessorEx        = kernel32.NewProc("SetThreadIdealProcessorEx")
	procGetSystemCpuSetInformation       = kernel32.NewProc("GetSystemCpuSetInformation")
	procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")

	// The PSAPI entry points live in kernel32 under a "K32" prefix on Win7+;
	// psapi.dll only forwards to them. x/sys/windows wraps most of the family
	// but not this one.
	procGetMappedFileNameW = kernel32.NewProc("K32GetMappedFileNameW")

	ntdll                        = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryInformationThread = ntdll.NewProc("NtQueryInformationThread")
	procNtSetInformationThread   = ntdll.NewProc("NtSetInformationThread")
)
