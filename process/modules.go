//go:build windows && amd64

package process

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// memImage is MEM_IMAGE — the region type of a section mapped from a PE file.
// x/sys/windows exports MEM_COMMIT but not this one.
const memImage = 0x1000000

// The region sweep is bounded twice over: by the end of the x64 user-mode
// address space, and by a step count, so a process that keeps mapping memory
// underneath us can never turn one refresh into an unbounded syscall loop.
const (
	userAddressLimit = uintptr(0x7FFFFFFF0000)
	maxRegionSteps   = 1 << 16
)

// moduleRange is one loaded module's address span, resolved once at load time.
type moduleRange struct {
	base uintptr
	end  uintptr
	name string // lowercase base name, e.g. "amdxx64.dll"
}

// ModuleTable maps a thread start address to the module that owns it. Full
// mode only: building it requires PROCESS_QUERY_INFORMATION | PROCESS_VM_READ,
// which limited mode deliberately does not request.
//
// Two sources are merged, because the PEB loader list alone is not trustworthy
// on a protected process:
//
//   - EnumProcessModulesEx, the loader's own view;
//   - a VirtualQueryEx sweep for MEM_IMAGE regions, named through
//     GetMappedFileName. This finds images the loader list no longer mentions,
//     which is exactly what a protection that unlinks or manually maps its
//     modules leaves behind.
type ModuleTable struct {
	ranges []moduleRange // sorted by base, non-overlapping
}

// LoadModuleTable enumerates the loaded modules of pid and records their
// address ranges. The snapshot is a point-in-time view: modules mapped later
// resolve to "" until the caller reloads.
func LoadModuleTable(pid uint32) (*ModuleTable, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d) for modules: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	table := &ModuleTable{}
	loaderErr := table.addLoadedModules(handle)
	// Best-effort second pass. It costs one VirtualQueryEx per region, runs off
	// the poll loop, and is the only source that sees an unlinked module.
	table.addMappedImages(handle)

	if len(table.ranges) == 0 {
		if loaderErr != nil {
			return nil, loaderErr
		}
		return nil, errors.New("no modules resolved")
	}

	table.normalise()
	return table, nil
}

// Len reports how many distinct module ranges the table covers. Callers use it
// to tell a reload that found something new from one that did not.
func (t *ModuleTable) Len() int { return len(t.ranges) }

// Resolve returns the lowercase base name of the module owning addr and the
// offset into it, or "" when no known module covers the address.
func (t *ModuleTable) Resolve(addr uintptr) (name string, offset uintptr) {
	if addr == 0 || len(t.ranges) == 0 {
		return "", 0
	}
	// Ranges are sorted and non-overlapping, so the first range ending past
	// addr is the only one that can contain it.
	i := sort.Search(len(t.ranges), func(i int) bool { return t.ranges[i].end > addr })
	if i < len(t.ranges) && addr >= t.ranges[i].base {
		return t.ranges[i].name, addr - t.ranges[i].base
	}
	return "", 0
}

// addLoadedModules walks the PEB loader list via EnumProcessModulesEx.
func (t *ModuleTable) addLoadedModules(handle windows.Handle) error {
	// Two-call pattern: probe the required byte count, then enumerate.
	var needed uint32
	if err := windows.EnumProcessModulesEx(handle, nil, 0, &needed, windows.LIST_MODULES_ALL); err != nil {
		return fmt.Errorf("EnumProcessModulesEx probe: %w", err)
	}

	count := needed / uint32(unsafe.Sizeof(windows.Handle(0)))
	if count == 0 {
		return nil
	}

	modules := make([]windows.Handle, count)
	if err := windows.EnumProcessModulesEx(handle, &modules[0],
		count*uint32(unsafe.Sizeof(windows.Handle(0))), &needed, windows.LIST_MODULES_ALL); err != nil {
		return fmt.Errorf("EnumProcessModulesEx: %w", err)
	}
	// Re-derive count in case modules changed between the two calls.
	if got := needed / uint32(unsafe.Sizeof(windows.Handle(0))); got < count {
		count = got
	}

	nameBuf := make([]uint16, windows.MAX_PATH)
	for _, module := range modules[:count] {
		var info windows.ModuleInfo
		if err := windows.GetModuleInformation(handle, module, &info, uint32(unsafe.Sizeof(info))); err != nil {
			continue
		}

		name := ""
		if err := windows.GetModuleFileNameEx(handle, module, &nameBuf[0], uint32(len(nameBuf))); err == nil {
			name = baseName(windows.UTF16ToString(nameBuf))
		}

		t.ranges = append(t.ranges, moduleRange{
			base: info.BaseOfDll,
			end:  info.BaseOfDll + uintptr(info.SizeOfImage),
			name: name,
		})
	}

	return nil
}

// addMappedImages sweeps the address space for MEM_IMAGE regions the loader
// list did not account for. An image is mapped as several regions (headers,
// .text, .rdata, …) that all share an AllocationBase, so they are folded back
// into one span per image before being named.
func (t *ModuleTable) addMappedImages(handle windows.Handle) {
	type span struct{ base, end uintptr }
	spans := make(map[uintptr]*span)

	var info windows.MemoryBasicInformation
	address := uintptr(0)
	for step := 0; step < maxRegionSteps && address < userAddressLimit; step++ {
		if err := windows.VirtualQueryEx(handle, address, &info, unsafe.Sizeof(info)); err != nil {
			break
		}

		next := info.BaseAddress + info.RegionSize
		if info.RegionSize == 0 || next <= address {
			break // no forward progress; bail rather than spin
		}

		if info.Type == memImage && info.State == windows.MEM_COMMIT && info.AllocationBase != 0 {
			if existing, ok := spans[info.AllocationBase]; ok {
				if next > existing.end {
					existing.end = next
				}
			} else {
				spans[info.AllocationBase] = &span{base: info.AllocationBase, end: next}
			}
		}

		address = next
	}

	nameBuf := make([]uint16, windows.MAX_PATH)
	for base, s := range spans {
		if t.covers(base) {
			continue // the loader list already knows this one
		}
		name := mappedFileName(handle, base, nameBuf)
		if name == "" {
			continue // unnamed executable mapping; nothing useful to report
		}
		t.ranges = append(t.ranges, moduleRange{base: s.base, end: s.end, name: name})
	}
}

// covers reports whether an existing range already contains addr. Used before
// the ranges are sorted, so it is a linear scan over a few hundred entries.
func (t *ModuleTable) covers(addr uintptr) bool {
	for i := range t.ranges {
		if addr >= t.ranges[i].base && addr < t.ranges[i].end {
			return true
		}
	}
	return false
}

// normalise sorts the ranges by base and drops overlaps, which is what lets
// Resolve binary-search. Overlap should not happen — covers() filters the
// sweep against the loader list — but a torn read during a module load must
// not silently break lookups for every other module.
func (t *ModuleTable) normalise() {
	sort.Slice(t.ranges, func(i, j int) bool { return t.ranges[i].base < t.ranges[j].base })

	kept := t.ranges[:0]
	var previousEnd uintptr
	for _, r := range t.ranges {
		if r.end <= r.base || r.base < previousEnd {
			continue
		}
		kept = append(kept, r)
		previousEnd = r.end
	}
	t.ranges = kept
}

// mappedFileName resolves the backing file of a mapped section. The result is
// an NT device path ("\Device\HarddiskVolume7\...\Overwatch_loader.dll"), so
// only the base name is kept.
func mappedFileName(handle windows.Handle, address uintptr, buf []uint16) string {
	ret, _, _ := procGetMappedFileNameW.Call(
		uintptr(handle),
		address,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if ret == 0 {
		return ""
	}
	return baseName(windows.UTF16ToString(buf[:ret]))
}

func baseName(path string) string {
	return strings.ToLower(filepath.Base(strings.ReplaceAll(path, `\`, `/`)))
}
