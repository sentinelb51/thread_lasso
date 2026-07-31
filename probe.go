package main

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/process"
	"ThreadOrchestra/scanner"
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

// probeStackModules is how many stack modules a probe line lists before it
// truncates. Enough to see a call path, short enough to stay on one line.
const probeStackModules = 3

// probe dumps, for every thread of the running game, each raw field the four
// start-address recovery routes read — before any of them is preferred over
// another. It exists because "no thread has a start address" has several very
// different causes with the same symptom, and the only way to tell them apart
// is to look at the fields side by side:
//
//   - both snapshot fields zero while StackBase and TebBase are populated means
//     the record is readable and those two fields specifically were cleared.
//     Clearing Win32StartAddress is a user-mode operation; clearing the
//     kernel's copy as well takes a driver;
//   - the whole record empty means we are not being told, and the answer is
//     privilege or an information class the kernel filters, not the process;
//   - the handle query disagreeing with the snapshot means the filtering is in
//     the system-wide path only, and the per-handle route is the fix;
//   - the stack column filled while everything else is empty means the origin
//     was erased after the fact but the thread's own creation frames still say
//     what it was.
//
// Runs once against the first configured game found, prints, and exits.
func probe(ctx context.Context, configuration config.Config) error {
	game, gameProcess, err := scanner.Process(ctx, configuration)
	if err != nil {
		return err
	}
	_ = game

	pid := uint32(gameProcess.Pid())
	fmt.Printf("probe: %s (pid %d)\n", gameProcess.Executable(), pid)

	snapshot, err := process.NewSnapshotSampler().Snapshot(pid)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	inspector, err := process.OpenInspector(pid)
	if err != nil {
		return fmt.Errorf("open for inspection: %w", err)
	}
	defer inspector.Close()

	fmt.Printf("        %d threads, %d module ranges\n\n", len(snapshot.Threads), inspector.ModuleCount())

	rows := make([]probeRow, 0, len(snapshot.Threads))
	for i := range snapshot.Threads {
		rows = append(rows, probeThread(inspector, &snapshot.Threads[i]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].tid < rows[j].tid })

	printProbeSummary(rows)
	printProbeRows(rows)

	return nil
}

type probeRow struct {
	tid       uint32
	win32     uintptr // snapshot ETHREAD.Win32StartAddress
	kernel    uintptr // snapshot ETHREAD.StartAddress
	queried   uintptr // NtQueryInformationThread on a handle
	stack     uintptr // recovered from the thread's startup frames
	hasStack  bool    // StackBase/StackLimit were populated
	hasTeb    bool
	module    string
	activity  []string
	gui       bool
	ioPending bool
	note      string
}

func probeThread(inspector *process.Inspector, snapshot *process.ThreadSnapshot) probeRow {
	row := probeRow{
		tid:      snapshot.TID,
		win32:    snapshot.Win32StartAddress,
		kernel:   snapshot.StartAddress,
		hasStack: snapshot.StackBase != 0 && snapshot.StackLimit != 0,
		hasTeb:   snapshot.TebBase != 0,
	}

	// A handle of our own, at full rights: the probe deliberately does not reuse
	// the governor's cache, so a failure here is attributable to this thread
	// rather than to something the cache did earlier.
	handle, err := process.OpenThreadHandle(snapshot.TID, process.AccessFull.ThreadAccess())
	if err != nil {
		row.note = "no handle: " + err.Error()
	} else {
		defer windows.CloseHandle(handle)

		if address, err := process.ThreadStartAddress(handle); err == nil {
			row.queried = address
		} else if row.note == "" {
			row.note = "query failed"
		}
		row.ioPending, _ = process.ThreadIoPending(handle)
	}

	trace := inspector.Trace(snapshot)
	row.stack = trace.Entry
	row.activity = trace.Modules
	row.gui = inspector.GuiThread(snapshot.TebBase)

	// The module is reported for whichever address we would actually have used.
	for _, address := range []uintptr{row.win32, row.kernel, row.queried, row.stack} {
		if !process.UserAddress(address) {
			continue
		}
		if name, offset := inspector.Resolve(address); name != "" {
			row.module = fmt.Sprintf("%s+0x%x", name, offset)
			break
		}
	}

	return row
}

func printProbeSummary(rows []probeRow) {
	total := len(rows)
	var win32, kernel, queried, stack, stacks, tebs, gui, pending int
	for _, row := range rows {
		if process.UserAddress(row.win32) {
			win32++
		}
		if process.UserAddress(row.kernel) {
			kernel++
		}
		if process.UserAddress(row.queried) {
			queried++
		}
		if process.UserAddress(row.stack) {
			stack++
		}
		if row.hasStack {
			stacks++
		}
		if row.hasTeb {
			tebs++
		}
		if row.gui {
			gui++
		}
		if row.ioPending {
			pending++
		}
	}

	fmt.Println("  route                              threads")
	fmt.Printf("  snapshot Win32StartAddress         %d/%d\n", win32, total)
	fmt.Printf("  snapshot kernel StartAddress       %d/%d\n", kernel, total)
	fmt.Printf("  NtQueryInformationThread(handle)   %d/%d\n", queried, total)
	fmt.Printf("  startup stack frames               %d/%d\n", stack, total)
	fmt.Println()
	fmt.Printf("  snapshot StackBase+StackLimit      %d/%d   (control: the same struct's other pointers)\n", stacks, total)
	fmt.Printf("  snapshot TebBase                   %d/%d\n", tebs, total)
	fmt.Printf("  TEB.Win32ThreadInfo set            %d/%d   (has called into user32/gdi32)\n", gui, total)
	fmt.Printf("  I/O request outstanding            %d/%d\n", pending, total)
	fmt.Println()
}

func printProbeRows(rows []probeRow) {
	fmt.Printf("  %-7s %-18s %-18s %-18s %-18s %-28s %s\n",
		"TID", "WIN32", "KERNEL", "QUERY", "STACK", "MODULE", "ON STACK")

	for _, row := range rows {
		activity := strings.Join(row.activity[:min(len(row.activity), probeStackModules)], " ")
		if row.note != "" {
			activity = row.note
		}

		fmt.Printf("  %-7d %-18s %-18s %-18s %-18s %-28s %s\n",
			row.tid,
			probeAddress(row.win32),
			probeAddress(row.kernel),
			probeAddress(row.queried),
			probeAddress(row.stack),
			probeField(row.module),
			activity,
		)
	}
}

func probeAddress(address uintptr) string {
	if address == 0 {
		return "-"
	}
	if !process.UserAddress(address) {
		return fmt.Sprintf("0x%x(kernel)", address)
	}

	return fmt.Sprintf("0x%x", address)
}

func probeField(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
