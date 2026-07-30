//go:build windows && amd64

package thread

import (
	"ThreadOrchestra/process"

	"golang.org/x/sys/windows"
)

// Key identifies a thread across snapshots. TID alone is unsafe: Windows
// reuses thread IDs, so CreateTime disambiguates a recycled TID.
type Key struct {
	TID        uint32
	CreateTime int64
}

// Entry is a cached, opened thread.
type Entry struct {
	Key          Key
	Handle       windows.Handle // 0 if the open failed (retried next Sync)
	Access       uint32         // rights actually granted
	Capabilities process.Capabilities
	Description  string // GetThreadDescription, cached at open
	Original     *Original
	lastSeen     uint64
}

// Cache owns thread handles for the lifetime of one game session. Not safe
// for concurrent use; the governor loop is the only caller.
type Cache struct {
	mode    process.AccessMode
	entries map[Key]*Entry
	seq     uint64
}

func NewCache(mode process.AccessMode) *Cache {
	return &Cache{
		mode:    mode,
		entries: make(map[Key]*Entry),
	}
}

// Sync reconciles the cache against a fresh snapshot: opens handles for new
// threads (falling back to limited rights if full-mode access is denied per
// thread), and closes handles of threads that no longer exist. Returns the
// entries for exactly the threads in the snapshot, index-aligned.
func (c *Cache) Sync(threads []process.ThreadSnapshot) []*Entry {
	c.seq++
	live := make([]*Entry, len(threads))

	for i, snapshot := range threads {
		key := Key{TID: snapshot.TID, CreateTime: snapshot.CreateTime}

		entry, ok := c.entries[key]
		if !ok {
			entry = &Entry{Key: key}
			c.entries[key] = entry
		}
		entry.lastSeen = c.seq

		// A zero handle means never opened or a previous attempt failed
		// (e.g. thread protected, transient churn) — retry each Sync.
		if entry.Handle == 0 {
			c.open(entry)
		}

		live[i] = entry
	}

	for key, entry := range c.entries {
		if entry.lastSeen != c.seq {
			c.drop(entry)
			delete(c.entries, key)
		}
	}

	return live
}

func (c *Cache) open(entry *Entry) {
	access := c.mode.ThreadAccess()

	handle, err := process.OpenThreadHandle(entry.Key.TID, access)
	if err != nil && c.mode == process.AccessFull {
		// Anti-cheat may strip full rights per thread; degrade to limited.
		access = process.AccessLimited.ThreadAccess()
		handle, err = process.OpenThreadHandle(entry.Key.TID, access)
	}
	if err != nil {
		return
	}

	entry.Handle = handle
	entry.Access = access
	entry.Capabilities = process.CapabilitiesFor(access)

	if entry.Capabilities.QueryDescription {
		if description, err := process.ThreadDescription(handle); err == nil {
			entry.Description = description
		}
	}
}

func (c *Cache) drop(entry *Entry) {
	if entry.Handle != 0 {
		windows.CloseHandle(entry.Handle)
		entry.Handle = 0
	}
}

// Lookup returns the cached entry for a key, or nil if it is not tracked.
func (c *Cache) Lookup(key Key) *Entry {
	return c.entries[key]
}

// Entries returns all live entries (for teardown iteration).
func (c *Cache) Entries() []*Entry {
	all := make([]*Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		all = append(all, entry)
	}

	return all
}

// Close closes every cached handle. Callers must restore journaled settings
// before calling Close.
func (c *Cache) Close() {
	for key, entry := range c.entries {
		c.drop(entry)
		delete(c.entries, key)
	}
}
