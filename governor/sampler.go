//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/process"
	"ThreadOrchestra/thread"
	"time"

	"golang.org/x/sys/windows"
)

// ThreadSample joins one thread's kernel snapshot with the handle-based reads
// for the same tick.
type ThreadSample struct {
	process.ThreadSnapshot
	Cycles       uint64 // cumulative; valid only when HasCycles
	HasCycles    bool
	IoPending    bool // an I/O request was outstanding; valid only when HasIoPending
	HasIoPending bool
	Description  string
	Entry        *thread.Entry
}

// Sample is everything measured in one poll tick.
type Sample struct {
	At       time.Time
	Tick     uint64
	Process  process.ProcessSnapshot
	Threads  []ThreadSample
	InputTID uint32 // thread owning the foreground window, if it belongs to the game
	Focused  bool   // the game currently holds the foreground window
}

// Sampler produces Samples for one game process.
type Sampler struct {
	pid       uint32
	snapshots *process.SnapshotSampler
	cache     *thread.Cache
	tick      uint64
}

func NewSampler(pid uint32, mode process.AccessMode) *Sampler {
	return &Sampler{
		pid:       pid,
		snapshots: process.NewSnapshotSampler(),
		cache:     thread.NewCache(mode),
	}
}

// Cache exposes the handle cache for actuation and teardown.
func (s *Sampler) Cache() *thread.Cache {
	return s.cache
}

// Sample takes one measurement. Returns process.ErrProcessNotFound when the
// game has exited.
func (s *Sampler) Sample() (Sample, error) {
	snapshot, err := s.snapshots.Snapshot(s.pid)
	if err != nil {
		return Sample{}, err
	}

	s.tick++
	entries := s.cache.Sync(snapshot.Threads)

	sample := Sample{
		At:      time.Now(),
		Tick:    s.tick,
		Process: snapshot,
		Threads: make([]ThreadSample, len(snapshot.Threads)),
	}

	for i, snap := range snapshot.Threads {
		threadSample := ThreadSample{
			ThreadSnapshot: snap,
			Entry:          entries[i],
			Description:    entries[i].Description,
		}

		// Handle may be gone mid-tick (thread died); skip, don't fail.
		if entries[i].Handle != 0 && entries[i].Capabilities.QueryCycles {
			if cycles, err := process.ThreadCycles(entries[i].Handle); err == nil {
				threadSample.Cycles = cycles
				threadSample.HasCycles = true
			}
		}
		// The only per-thread I/O signal Windows offers. One syscall per thread
		// per tick, and the sole way to tell a thread waiting on a socket from
		// one waiting on a lock.
		if entries[i].Handle != 0 && entries[i].Capabilities.QueryIoPending {
			if pending, err := process.ThreadIoPending(entries[i].Handle); err == nil {
				threadSample.IoPending = pending
				threadSample.HasIoPending = true
			}
		}

		sample.Threads[i] = threadSample
	}

	sample.InputTID, sample.Focused = s.foreground()

	return sample, nil
}

// foreground inspects the OS foreground window. focused reports whether it
// belongs to the game (the game is the active app); tid is the TID of the
// thread owning that window — hard evidence for the Input role, obtained
// without any process handle. When the game is not focused both are zero/false.
func (s *Sampler) foreground() (tid uint32, focused bool) {
	hwnd := windows.GetForegroundWindow()
	if hwnd == 0 {
		return 0, false
	}

	var pid uint32
	tid, err := windows.GetWindowThreadProcessId(hwnd, &pid)
	if err != nil || pid != s.pid {
		return 0, false
	}

	return tid, true
}
