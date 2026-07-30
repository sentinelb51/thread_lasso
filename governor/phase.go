//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/thread"
	"sort"
)

// Phase is the coarse game state; actuation is frozen outside PhaseStable so
// the governor never tunes against menu/loading behavior that is about to
// change (the two System Informer captures show the hot set flipping
// completely between menu and gameplay).
type Phase int

const (
	PhaseWarmup Phase = iota
	PhaseStable
	PhaseTransition
	PhaseLoading
)

var phaseNames = map[Phase]string{
	PhaseWarmup:     "warmup",
	PhaseStable:     "stable",
	PhaseTransition: "transition",
	PhaseLoading:    "loading",
}

func (p Phase) String() string { return phaseNames[p] }

const (
	loadingReadRate  = 30e6 // bytes/sec of disk reads → asset streaming
	hotSetSize       = 6
	hotSetMinOverlap = 0.5 // Jaccard below this = the hot set flipped
	settleTicks      = 3
	warmupTicks      = 4
)

type PhaseDetector struct {
	prevHot map[thread.Key]bool
	settle  int
	ticks   int
}

func NewPhaseDetector() *PhaseDetector {
	return &PhaseDetector{}
}

func (p *PhaseDetector) Update(tracker *Tracker) Phase {
	p.ticks++

	hot := hotSet(tracker)
	overlap := jaccard(p.prevHot, hot)
	p.prevHot = hot

	if p.ticks <= warmupTicks {
		return PhaseWarmup
	}

	if tracker.ReadRate > loadingReadRate {
		p.settle = settleTicks
		return PhaseLoading
	}

	if overlap < hotSetMinOverlap {
		p.settle = settleTicks
		return PhaseTransition
	}

	if p.settle > 0 {
		p.settle--
		return PhaseTransition
	}

	return PhaseStable
}

func hotSet(tracker *Tracker) map[thread.Key]bool {
	type rated struct {
		key  thread.Key
		rate float64
	}

	all := make([]rated, 0, len(tracker.Series))
	for key, series := range tracker.Series {
		all = append(all, rated{key, series.CyclesRateShort})
	}

	sort.Slice(all, func(i, j int) bool { return all[i].rate > all[j].rate })

	hot := make(map[thread.Key]bool, hotSetSize)
	for i := 0; i < len(all) && i < hotSetSize; i++ {
		if all[i].rate > 0 {
			hot[all[i].key] = true
		}
	}

	return hot
}

func jaccard(a, b map[thread.Key]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}

	intersection := 0
	for key := range a {
		if b[key] {
			intersection++
		}
	}

	union := len(a) + len(b) - intersection
	if union == 0 {
		return 1
	}

	return float64(intersection) / float64(union)
}
