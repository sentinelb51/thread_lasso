//go:build windows && amd64

package governor

import "ThreadOrchestra/process"

// NewModuleResolver builds a full-mode start-address → module-name resolver by
// snapshotting the target's loaded modules once. Returns an error (and the
// governor falls back to behavioral classification) when the process cannot be
// opened for module enumeration — e.g. limited rights or anti-cheat.
func NewModuleResolver(pid uint32) (ModuleResolver, error) {
	table, err := process.LoadModuleTable(pid)
	if err != nil {
		return nil, err
	}

	return table.Resolve, nil
}
