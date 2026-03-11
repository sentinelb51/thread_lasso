package process

import "golang.org/x/sys/windows"

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procSetProcessAffinityMask   = kernel32.NewProc("SetProcessAffinityMask")
	procSetProcessDefaultCpuSets = kernel32.NewProc("SetProcessDefaultCpuSets")
)
