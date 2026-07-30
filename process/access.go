//go:build windows && amd64

package process

import "golang.org/x/sys/windows"

// AccessMode selects how much access the tool requests on the game process
// and its threads. "limited" is the default and keeps the footprint minimal
// (relevant for anti-cheat protected games); "full" unlocks module
// resolution, memory/I-O priority, EcoQoS and ideal-processor tuning.
type AccessMode string

const (
	AccessLimited AccessMode = "limited"
	AccessFull    AccessMode = "full"
)

// ThreadAccess returns the thread access rights to request for this mode.
func (m AccessMode) ThreadAccess() uint32 {
	access := uint32(ThreadQueryLimitedInformation | ThreadSetLimitedInformation)
	if m == AccessFull {
		access |= ThreadQueryInformation | ThreadSetInformation
	}

	return access
}

// ProcessAccess returns the process access rights to request for this mode.
func (m AccessMode) ProcessAccess() uint32 {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if m == AccessFull {
		access |= windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ
	}

	return access
}

// Capabilities describes which tunables are actually usable for a given
// granted access level; actuators consult this instead of assuming the
// configured mode was honoured (anti-cheat may strip rights per handle).
type Capabilities struct {
	SetPriority       bool // THREAD_SET_LIMITED_INFORMATION
	SetCpuSets        bool // THREAD_SET_LIMITED_INFORMATION
	SetMemoryPriority bool // THREAD_SET_INFORMATION
	SetIoPriority     bool // THREAD_SET_INFORMATION
	SetEcoQoS         bool // THREAD_SET_INFORMATION
	SetIdealCpu       bool // THREAD_SET_INFORMATION
	QueryCycles       bool // THREAD_QUERY_LIMITED_INFORMATION
	QueryDescription  bool // THREAD_QUERY_LIMITED_INFORMATION
}

// CapabilitiesFor maps granted thread access rights to usable tunables.
func CapabilitiesFor(grantedAccess uint32) Capabilities {
	queryLimited := grantedAccess&(ThreadQueryLimitedInformation|ThreadQueryInformation) != 0
	setLimited := grantedAccess&(ThreadSetLimitedInformation|ThreadSetInformation) != 0
	setFull := grantedAccess&ThreadSetInformation != 0

	return Capabilities{
		SetPriority:       setLimited,
		SetCpuSets:        setLimited,
		SetMemoryPriority: setFull,
		SetIoPriority:     setFull,
		SetEcoQoS:         setFull,
		SetIdealCpu:       setFull,
		QueryCycles:       queryLimited,
		QueryDescription:  queryLimited,
	}
}
