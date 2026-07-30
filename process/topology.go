//go:build windows && amd64

package process

import (
	"fmt"
	"sort"
	"unsafe"
)

// systemCpuSetInformation mirrors SYSTEM_CPU_SET_INFORMATION (x64, 32 bytes).
// The public struct is a nested union; every entry we care about has
// Type == cpuSetInformationType, so the CpuSet arm is flattened here. Fields
// after AllFlags are laid out to match the C struct's padding exactly.
type systemCpuSetInformation struct {
	Size                  uint32
	Type                  uint32
	Id                    uint32
	Group                 uint16
	LogicalProcessorIndex uint8
	CoreIndex             uint8
	LastLevelCacheIndex   uint8
	NumaNodeIndex         uint8
	EfficiencyClass       uint8
	AllFlags              uint8
	SchedulingClass       uint32 // union with Reserved
	AllocationTag         uint64
}

const cpuSetInformationType = 0 // CpuSetInformation

func init() {
	if unsafe.Sizeof(systemCpuSetInformation{}) != 32 {
		panic("systemCpuSetInformation must be 32 bytes")
	}
}

// Topology describes the machine's logical/physical CPU layout, derived from
// GetSystemCpuSetInformation. It is used to validate the assumed CPU Set base
// and to spread critical threads across distinct physical cores (SMT-aware).
type Topology struct {
	Base      uint32 // CPU Set ID of logical CPU 0
	Logical   int    // count of logical processors
	coreLeads []int  // one representative logical CPU per physical core, ascending
}

// LoadTopology queries the system CPU-set table. Single processor group is
// assumed (gaming machines, <= 64 logical CPUs); group is not consulted.
func LoadTopology() (*Topology, error) {
	var length uint32

	// First call sizes the buffer (returns FALSE with the required length).
	procGetSystemCpuSetInformation.Call(0, 0, uintptr(unsafe.Pointer(&length)), 0, 0)
	if length == 0 {
		return nil, fmt.Errorf("GetSystemCpuSetInformation returned zero length")
	}

	buffer := make([]byte, length)
	ret, _, err := procGetSystemCpuSetInformation.Call(
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(length),
		uintptr(unsafe.Pointer(&length)),
		0, // Process = NULL: system-wide information
		0, // Flags
	)
	if ret == 0 {
		return nil, fmt.Errorf("GetSystemCpuSetInformation failed: %w", err)
	}

	base := ^uint32(0)
	logical := 0
	leadByCore := map[uint8]uint8{}
	var cores []uint8

	for offset := 0; offset+int(unsafe.Sizeof(systemCpuSetInformation{})) <= int(length); {
		info := (*systemCpuSetInformation)(unsafe.Pointer(&buffer[offset]))
		if info.Size == 0 {
			break // malformed; stop rather than loop forever
		}

		if info.Type == cpuSetInformationType {
			logical++
			if info.Id < base {
				base = info.Id
			}
			if lead, ok := leadByCore[info.CoreIndex]; !ok {
				leadByCore[info.CoreIndex] = info.LogicalProcessorIndex
				cores = append(cores, info.CoreIndex)
			} else if info.LogicalProcessorIndex < lead {
				leadByCore[info.CoreIndex] = info.LogicalProcessorIndex
			}
		}

		offset += int(info.Size)
	}

	if logical == 0 {
		return nil, fmt.Errorf("GetSystemCpuSetInformation reported no CPU sets")
	}

	sort.Slice(cores, func(i, j int) bool { return cores[i] < cores[j] })
	leads := make([]int, len(cores))
	for i, core := range cores {
		leads[i] = int(leadByCore[core])
	}

	return &Topology{Base: base, Logical: logical, coreLeads: leads}, nil
}

// PhysicalCoreLeads returns one representative logical CPU per physical core,
// ascending by core index. Spreading critical threads across these avoids
// piling frame-critical work onto SMT siblings of the same core.
func (t *Topology) PhysicalCoreLeads() []int {
	return t.coreLeads
}
