//go:build windows && amd64

package process

import (
	"errors"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	testHeaderSize = int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
	testThreadSize = int(unsafe.Sizeof(systemExtendedThreadInformation{}))
)

// writeEntry appends one process entry with the given threads to buf and
// returns the extended buffer.
func writeEntry(buf []byte, pid uint32, next uint32, threads []systemExtendedThreadInformation) []byte {
	entry := windows.SYSTEM_PROCESS_INFORMATION{
		NextEntryOffset: next,
		NumberOfThreads: uint32(len(threads)),
		UniqueProcessID: uintptr(pid),
		BasePriority:    8,
	}

	entryBytes := unsafe.Slice((*byte)(unsafe.Pointer(&entry)), testHeaderSize)
	buf = append(buf, entryBytes...)

	for i := range threads {
		threadBytes := unsafe.Slice((*byte)(unsafe.Pointer(&threads[i])), testThreadSize)
		buf = append(buf, threadBytes...)
	}

	return buf
}

func testThread(tid uint32, contextSwitches uint32) systemExtendedThreadInformation {
	t := systemExtendedThreadInformation{
		Win32StartAddress: 0xDEADBEEF,
		StackBase:         0x2000,
		StackLimit:        0x1000,
	}
	t.UniqueThread = uintptr(tid)
	t.CreateTime = 123456789
	t.ContextSwitches = contextSwitches
	t.ThreadState = uint32(StateWaiting)
	t.WaitReason = uint32(WrQueue)
	return t
}

func TestParseSnapshotWalk(t *testing.T) {
	firstSize := uint32(testHeaderSize + 1*testThreadSize)

	var buf []byte
	buf = writeEntry(buf, 111, firstSize, []systemExtendedThreadInformation{
		testThread(1101, 42),
	})
	buf = writeEntry(buf, 222, 0, []systemExtendedThreadInformation{
		testThread(2201, 7),
		testThread(2202, 9),
	})

	// Second entry, reached via NextEntryOffset.
	snap, err := parseSnapshot(buf, 222)
	if err != nil {
		t.Fatalf("parse pid 222: %v", err)
	}
	if len(snap.Threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(snap.Threads))
	}
	if snap.Threads[1].TID != 2202 || snap.Threads[1].ContextSwitches != 9 {
		t.Errorf("thread copy mismatch: %+v", snap.Threads[1])
	}
	if snap.Threads[0].WaitReason != WrQueue || snap.Threads[0].ThreadState != StateWaiting {
		t.Errorf("state/wait reason mismatch: %+v", snap.Threads[0])
	}
	if snap.Threads[0].Win32StartAddress != 0xDEADBEEF {
		t.Errorf("start address mismatch: %#x", snap.Threads[0].Win32StartAddress)
	}

	// First entry.
	snap, err = parseSnapshot(buf, 111)
	if err != nil {
		t.Fatalf("parse pid 111: %v", err)
	}
	if len(snap.Threads) != 1 || snap.Threads[0].TID != 1101 {
		t.Fatalf("unexpected threads for pid 111: %+v", snap.Threads)
	}

	// Absent PID.
	_, err = parseSnapshot(buf, 999)
	if !errors.Is(err, ErrProcessNotFound) {
		t.Fatalf("expected ErrProcessNotFound, got %v", err)
	}
}

func TestParseSnapshotBounds(t *testing.T) {
	// Claim 3 threads but provide only 1 — must error, not read out of bounds.
	var buf []byte
	buf = writeEntry(buf, 111, 0, []systemExtendedThreadInformation{testThread(1101, 1)})
	entry := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[0]))
	entry.NumberOfThreads = 3

	if _, err := parseSnapshot(buf, 111); err == nil {
		t.Fatal("expected bounds error for truncated thread array")
	}

	// Truncated header.
	if _, err := parseSnapshot(buf[:8], 111); err == nil {
		t.Fatal("expected bounds error for truncated header")
	}
}

func TestSnapshotSelf(t *testing.T) {
	sampler := NewSnapshotSampler()

	snap, err := sampler.Snapshot(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("self snapshot: %v", err)
	}

	if snap.PID != uint32(os.Getpid()) {
		t.Errorf("PID mismatch: %d", snap.PID)
	}
	if len(snap.Threads) == 0 {
		t.Fatal("expected at least one thread in own process")
	}

	self := windows.GetCurrentThreadId()
	found := false
	for _, thread := range snap.Threads {
		if thread.TID == self {
			found = true
		}
		if thread.CreateTime == 0 {
			t.Errorf("thread %d has zero create time", thread.TID)
		}
	}
	if !found {
		t.Errorf("current thread %d not present in snapshot", self)
	}
}

func TestSnapshotNotFound(t *testing.T) {
	sampler := NewSnapshotSampler()

	// PID 4 is System (always exists); a PID that is not a multiple of 4 is
	// never valid on modern Windows.
	_, err := sampler.Snapshot(0xFFFFFFFD)
	if !errors.Is(err, ErrProcessNotFound) {
		t.Fatalf("expected ErrProcessNotFound, got %v", err)
	}
}
