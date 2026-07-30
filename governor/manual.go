//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/config"
	"ThreadOrchestra/thread"
	"ThreadOrchestra/util"
	"strings"
)

// manualApplier enforces the user's explicit per-thread config rules — the
// original purpose of the config.Thread list, previously a stub. Each thread
// is matched against the first rule whose name glob fits and tuned exactly
// once; the applied label is remembered so the auto actuator defers to any
// thread the user has claimed.
type manualApplier struct {
	rules  []config.Thread
	labels map[thread.Key]string
}

func newManualApplier(rules []config.Thread) *manualApplier {
	return &manualApplier{rules: rules, labels: make(map[thread.Key]string)}
}

// Apply walks this tick's threads and applies matching rules to any not yet
// handled. A thread with no handle this tick is retried on a later tick.
func (m *manualApplier) Apply(sample *Sample, facts map[thread.Key]*Facts) {
	if len(m.rules) == 0 {
		return
	}

	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}
		if _, done := m.labels[key]; done {
			continue
		}

		entry := threadSample.Entry
		if entry == nil || entry.Handle == 0 {
			continue
		}

		module := ""
		if f := facts[key]; f != nil {
			module = f.Module
		}

		for _, rule := range m.rules {
			if !matchThreadRule(rule, threadSample.Description, module) {
				continue
			}

			applied := entry.ApplyRule(rule)
			if len(applied) > 0 {
				m.labels[key] = "manual:" + strings.Join(applied, ",")
			} else {
				// Matched but nothing changed (capability missing / already set);
				// still claim it so the actuator leaves it alone.
				m.labels[key] = "manual"
			}
			break
		}
	}
}

// matchThreadRule matches a rule's name glob against the thread's description
// or, in full mode, its start module.
func matchThreadRule(rule config.Thread, name, module string) bool {
	if rule.Name == "" {
		return false
	}
	return util.Match(rule.Name, name) || (module != "" && util.Match(rule.Name, module))
}

// Reset clears manual ownership so rules re-apply after a UI-requested revert.
func (m *manualApplier) Reset() {
	m.labels = make(map[thread.Key]string)
}

// Owns reports whether a manual rule has claimed this thread.
func (m *manualApplier) Owns(key thread.Key) bool {
	_, ok := m.labels[key]
	return ok
}

// AppliedLabel is the UI's "applied" text for a manually tuned thread, or "".
func (m *manualApplier) AppliedLabel(key thread.Key) string {
	return m.labels[key]
}
