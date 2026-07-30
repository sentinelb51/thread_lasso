//go:build windows && amd64

package process

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ThreadState mirrors the kernel KTHREAD_STATE values reported by
// NtQuerySystemInformation.
type ThreadState uint32

const (
	StateInitialized ThreadState = iota
	StateReady
	StateRunning
	StateStandby
	StateTerminated
	StateWaiting
	StateTransition
	StateDeferredReady
	StateGateWaitObsolete
	StateWaitingForProcessInSwap
)

// WaitReason mirrors the kernel KWAIT_REASON values. The interesting ones for
// classification: WrQueue (thread-pool worker parked on a KQUEUE),
// WrDelayExecution (Sleep loop), WrUserRequest (event-driven wait).
type WaitReason uint32

const (
	Executive WaitReason = iota
	FreePage
	PageIn
	PoolAllocation
	DelayExecution
	Suspended
	UserRequest
	WrExecutive
	WrFreePage
	WrPageIn
	WrPoolAllocation
	WrDelayExecution
	WrSuspended
	WrUserRequest
	WrEventPair
	WrQueue
	WrLpcReceive
	WrLpcReply
	WrVirtualMemory
	WrPageOut
	WrRendezvous
	WrKeyedEvent
	WrTerminated
	WrProcessInSwap
	WrCpuRateControl
	WrCalloutStack
	WrKernel
	WrResource
	WrPushLock
	WrMutex
	WrQuantumEnd
	WrDispatchInt
	WrPreempted
	WrYieldExecution
	WrFastMutex
	WrGuardedMutex
	WrRundown
	WrAlertByThreadId
	WrDeferredPreempt
)

var waitReasonNames = [...]string{
	"Executive", "FreePage", "PageIn", "PoolAllocation", "DelayExecution",
	"Suspended", "UserRequest", "WrExecutive", "WrFreePage", "WrPageIn",
	"WrPoolAllocation", "WrDelayExecution", "WrSuspended", "WrUserRequest",
	"WrEventPair", "WrQueue", "WrLpcReceive", "WrLpcReply", "WrVirtualMemory",
	"WrPageOut", "WrRendezvous", "WrKeyedEvent", "WrTerminated",
	"WrProcessInSwap", "WrCpuRateControl", "WrCalloutStack", "WrKernel",
	"WrResource", "WrPushLock", "WrMutex", "WrQuantumEnd", "WrDispatchInt",
	"WrPreempted", "WrYieldExecution", "WrFastMutex", "WrGuardedMutex",
	"WrRundown", "WrAlertByThreadId", "WrDeferredPreempt",
}

func (w WaitReason) String() string {
	if int(w) < len(waitReasonNames) {
		return waitReasonNames[w]
	}
	return fmt.Sprintf("WaitReason(%d)", uint32(w))
}

// systemThreadInformation is the x64 layout of SYSTEM_THREAD_INFORMATION
// (undocumented but stable since Vista; System Informer relies on the same
// layout).
type systemThreadInformation struct {
	KernelTime      int64
	UserTime        int64
	CreateTime      int64
	WaitTime        uint32
	_               uint32 // align StartAddress to 8
	StartAddress    uintptr
	UniqueProcess   uintptr // CLIENT_ID
	UniqueThread    uintptr
	Priority        int32
	BasePriority    int32
	ContextSwitches uint32
	ThreadState     uint32
	WaitReason      uint32
	_               uint32 // tail pad to 8
}

// systemExtendedThreadInformation is the x64 layout of
// SYSTEM_EXTENDED_THREAD_INFORMATION, returned by
// SystemExtendedProcessInformation (class 57).
type systemExtendedThreadInformation struct {
	systemThreadInformation
	StackBase         uintptr
	StackLimit        uintptr
	Win32StartAddress uintptr
	TebBase           uintptr
	Reserved2         uintptr
	Reserved3         uintptr
	Reserved4         uintptr
}

// The parser walks raw kernel buffers with these exact sizes; a padding
// mistake must fail the build, not corrupt reads at runtime.
var (
	_ [80]byte  = [unsafe.Sizeof(systemThreadInformation{})]byte{}
	_ [136]byte = [unsafe.Sizeof(systemExtendedThreadInformation{})]byte{}
)

// ThreadSnapshot is one thread's state at a single sample instant, copied out
// of the kernel buffer into plain Go data.
type ThreadSnapshot struct {
	TID               uint32
	CreateTime        int64 // FILETIME ticks; part of the identity key with TID
	KernelTime        int64 // 100 ns units
	UserTime          int64 // 100 ns units
	WaitTime          uint32
	StartAddress      uintptr
	Win32StartAddress uintptr
	Priority          int32 // dynamic priority
	BasePriority      int32
	ContextSwitches   uint32 // cumulative, wraps at 2^32
	ThreadState       ThreadState
	WaitReason        WaitReason
	StackBase         uintptr
	StackLimit        uintptr
	TebBase           uintptr
}

// ProcessSnapshot is a single process entry from the system snapshot.
type ProcessSnapshot struct {
	PID            uint32
	ImageName      string
	CreateTime     int64
	UserTime       int64
	KernelTime     int64
	CycleTime      uint64 // sum over all threads, including exited ones
	BasePriority   int32
	HandleCount    uint32
	WorkingSetSize uintptr
	// Cumulative I/O transfer volumes; the byte-rate derived from these
	// distinguishes the Loading phase (heavy asset streaming) from gameplay.
	ReadTransferCount  int64
	WriteTransferCount int64
	Threads            []ThreadSnapshot
}

// ErrProcessNotFound is returned by Snapshot when the PID no longer exists;
// callers treat it as "game exited", not as a failure.
var ErrProcessNotFound = errors.New("process not present in system snapshot")

const (
	snapshotInitialSize = 512 * 1024
	snapshotGrowSlack   = 64 * 1024 // thread count changes between the size probe and the retry
	snapshotMaxRetries  = 5
)

// SnapshotSampler owns a reusable buffer for NtQuerySystemInformation so the
// steady-state poll loop does not allocate. Not safe for concurrent use.
type SnapshotSampler struct {
	buf []byte
}

func NewSnapshotSampler() *SnapshotSampler {
	return &SnapshotSampler{buf: make([]byte, snapshotInitialSize)}
}

// Snapshot returns the target process's entry from a full-system
// SystemExtendedProcessInformation query. No process handle is required.
func (s *SnapshotSampler) Snapshot(pid uint32) (ProcessSnapshot, error) {
	var retLen uint32

	for attempt := 0; ; attempt++ {
		err := windows.NtQuerySystemInformation(
			windows.SystemExtendedProcessInformation,
			unsafe.Pointer(&s.buf[0]),
			uint32(len(s.buf)),
			&retLen,
		)

		if err == nil {
			break
		}

		if err != windows.STATUS_INFO_LENGTH_MISMATCH || attempt >= snapshotMaxRetries {
			return ProcessSnapshot{}, fmt.Errorf("NtQuerySystemInformation failed: %w", err)
		}

		s.buf = make([]byte, int(retLen)+snapshotGrowSlack)
	}

	return parseSnapshot(s.buf[:retLen], pid)
}

// parseSnapshot walks the SYSTEM_PROCESS_INFORMATION entries in buf looking
// for pid. Every unsafe view is bounds-checked against buf and copied out
// before returning — the backing buffer is reused on the next poll.
func parseSnapshot(buf []byte, pid uint32) (ProcessSnapshot, error) {
	const headerSize = unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{})
	const threadSize = unsafe.Sizeof(systemExtendedThreadInformation{})

	for offset := uintptr(0); ; {
		if offset+headerSize > uintptr(len(buf)) {
			return ProcessSnapshot{}, fmt.Errorf("snapshot walk out of bounds at offset %d", offset)
		}

		entry := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[offset]))

		if uint32(entry.UniqueProcessID) == pid && pid != 0 {
			threadsEnd := offset + headerSize + uintptr(entry.NumberOfThreads)*threadSize
			if threadsEnd > uintptr(len(buf)) {
				return ProcessSnapshot{}, fmt.Errorf("thread array out of bounds for PID %d", pid)
			}

			return copyProcessEntry(entry, &buf[offset+headerSize]), nil
		}

		if entry.NextEntryOffset == 0 {
			return ProcessSnapshot{}, ErrProcessNotFound
		}

		offset += uintptr(entry.NextEntryOffset)
	}
}

func copyProcessEntry(entry *windows.SYSTEM_PROCESS_INFORMATION, threadBase *byte) ProcessSnapshot {
	snapshot := ProcessSnapshot{
		PID:                uint32(entry.UniqueProcessID),
		ImageName:          ntUnicodeToString(entry.ImageName),
		CreateTime:         entry.CreateTime,
		UserTime:           entry.UserTime,
		KernelTime:         entry.KernelTime,
		CycleTime:          entry.CycleTime,
		BasePriority:       entry.BasePriority,
		HandleCount:        entry.HandleCount,
		WorkingSetSize:     entry.WorkingSetSize,
		ReadTransferCount:  entry.ReadTransferCount,
		WriteTransferCount: entry.WriteTransferCount,
		Threads:            make([]ThreadSnapshot, entry.NumberOfThreads),
	}

	raw := unsafe.Slice((*systemExtendedThreadInformation)(unsafe.Pointer(threadBase)), entry.NumberOfThreads)
	for i := range raw {
		t := &raw[i]
		snapshot.Threads[i] = ThreadSnapshot{
			TID:               uint32(t.UniqueThread),
			CreateTime:        t.CreateTime,
			KernelTime:        t.KernelTime,
			UserTime:          t.UserTime,
			WaitTime:          t.WaitTime,
			StartAddress:      t.StartAddress,
			Win32StartAddress: t.Win32StartAddress,
			Priority:          t.Priority,
			BasePriority:      t.BasePriority,
			ContextSwitches:   t.ContextSwitches,
			ThreadState:       ThreadState(t.ThreadState),
			WaitReason:        WaitReason(t.WaitReason),
			StackBase:         t.StackBase,
			StackLimit:        t.StackLimit,
			TebBase:           t.TebBase,
		}
	}

	return snapshot
}

func ntUnicodeToString(s windows.NTUnicodeString) string {
	if s.Buffer == nil || s.Length == 0 {
		return ""
	}

	return windows.UTF16ToString(unsafe.Slice(s.Buffer, s.Length/2))
}
