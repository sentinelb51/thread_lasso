//go:build windows && amd64

package process

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
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
type ModuleTable struct {
	ranges []moduleRange
}

// LoadModuleTable enumerates the loaded modules of pid and records their
// address ranges. The snapshot is taken once; a module loaded later (rare for
// a running game past warmup) simply resolves to "".
func LoadModuleTable(pid uint32) (*ModuleTable, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d) for modules: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// Two-call pattern: probe the required byte count, then enumerate.
	var needed uint32
	if err := windows.EnumProcessModulesEx(handle, nil, 0, &needed, windows.LIST_MODULES_ALL); err != nil {
		return nil, fmt.Errorf("EnumProcessModulesEx probe: %w", err)
	}

	count := needed / uint32(unsafe.Sizeof(windows.Handle(0)))
	if count == 0 {
		return &ModuleTable{}, nil
	}

	modules := make([]windows.Handle, count)
	if err := windows.EnumProcessModulesEx(handle, &modules[0],
		count*uint32(unsafe.Sizeof(windows.Handle(0))), &needed, windows.LIST_MODULES_ALL); err != nil {
		return nil, fmt.Errorf("EnumProcessModulesEx: %w", err)
	}
	// Re-derive count in case modules changed between the two calls.
	if got := needed / uint32(unsafe.Sizeof(windows.Handle(0))); got < count {
		count = got
	}

	table := &ModuleTable{ranges: make([]moduleRange, 0, count)}
	nameBuf := make([]uint16, windows.MAX_PATH)

	for _, module := range modules[:count] {
		var info windows.ModuleInfo
		if err := windows.GetModuleInformation(handle, module, &info, uint32(unsafe.Sizeof(info))); err != nil {
			continue
		}

		name := ""
		if err := windows.GetModuleFileNameEx(handle, module, &nameBuf[0], uint32(len(nameBuf))); err == nil {
			name = strings.ToLower(filepath.Base(windows.UTF16ToString(nameBuf)))
		}

		table.ranges = append(table.ranges, moduleRange{
			base: info.BaseOfDll,
			end:  info.BaseOfDll + uintptr(info.SizeOfImage),
			name: name,
		})
	}

	return table, nil
}

// Resolve returns the lowercase base name of the module owning addr, or ""
// when no loaded module covers it.
func (t *ModuleTable) Resolve(addr uintptr) string {
	for i := range t.ranges {
		r := &t.ranges[i]
		if addr >= r.base && addr < r.end {
			return r.name
		}
	}
	return ""
}
