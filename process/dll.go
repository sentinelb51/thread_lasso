package process

import "golang.org/x/sys/windows"

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procSetProcessAffinityMask   = kernel32.NewProc("SetProcessAffinityMask")
	procGetProcessAffinityMask   = kernel32.NewProc("GetProcessAffinityMask")
	procSetProcessDefaultCpuSets = kernel32.NewProc("SetProcessDefaultCpuSets")
	procGetProcessDefaultCpuSets = kernel32.NewProc("GetProcessDefaultCpuSets")
)
