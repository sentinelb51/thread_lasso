//go:build windows && amd64

package thread

import (
	"ThreadOrchestra/process"
	"os"
	"testing"
)

func TestCacheSyncSelf(t *testing.T) {
	sampler := process.NewSnapshotSampler()

	snap, err := sampler.Snapshot(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("self snapshot: %v", err)
	}

	cache := NewCache(process.AccessLimited)
	defer cache.Close()

	live := cache.Sync(snap.Threads)
	if len(live) != len(snap.Threads) {
		t.Fatalf("expected %d entries, got %d", len(snap.Threads), len(live))
	}

	opened := 0
	for _, entry := range live {
		if entry.Handle == 0 {
			continue
		}
		opened++

		if !entry.Capabilities.QueryCycles {
			t.Errorf("TID %d: expected QueryCycles capability under limited mode", entry.Key.TID)
		}

		if _, err := process.ThreadCycles(entry.Handle); err != nil {
			t.Errorf("TID %d: cycle query failed: %v", entry.Key.TID, err)
		}
	}
	if opened == 0 {
		t.Fatal("expected to open at least one own thread")
	}

	// Second sync with the same snapshot must not churn handles.
	before := live[0].Handle
	live2 := cache.Sync(snap.Threads)
	if live2[0].Handle != before {
		t.Error("handle churned across syncs for a live thread")
	}

	// Threads absent from the snapshot must be dropped.
	cache.Sync(snap.Threads[:1])
	if len(cache.Entries()) != 1 {
		t.Errorf("expected 1 entry after shrink, got %d", len(cache.Entries()))
	}
}
