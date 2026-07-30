//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/thread"
	"strings"
	"time"
)

// aggression is the parsed form of the "aggression" config knob.
type aggression int

const (
	aggConservative aggression = iota // raise-only: promote, never demote
	aggStandard                       // + demote high-confidence Telemetry/PoolIdle
	aggAggressive                     // full bucket policy
)

func parseAggression(s string) aggression {
	switch s {
	case "conservative":
		return aggConservative
	case "aggressive":
		return aggAggressive
	default:
		return aggStandard
	}
}

// THREAD_PRIORITY_* relative values and the knobs the actuator writes.
const (
	priorityNormal = 0 // THREAD_PRIORITY_NORMAL

	ioPriorityLow  = 1 // Low
	ioPriorityHigh = 3 // High

	// MEMORY_PRIORITY_MEDIUM — demotes a background thread's pages ahead of
	// the game's without starving it outright (plan §Policy).
	backgroundMemoryPriority uint32 = 3

	// Demotions require more evidence than promotions.
	demoteMinConfidence = 0.5

	// Sustained fraction of polls a thread is seen Ready (runnable but not
	// running) that marks it CPU-starved — a demotion we must undo.
	starvationReadyRatio = 0.25
)

// actionState tracks what the governor has done to one thread, for cooldown,
// watchdog rollback, and the UI's "applied" column.
type actionState struct {
	lastChange  time.Time
	label       string
	bucket      Bucket // what the current settings were applied *for*
	idealCPU    int    // sticky: the physical core this thread was steered onto
	hasIdeal    bool
	demoted     bool // we lowered this thread; the watchdog may roll it back
	quarantined bool // rolled back after starvation — never touched again
}

// Actuator turns per-thread verdicts into thread modifications, filtered by
// aggression level and the handle's real capabilities, and owns the rollback
// watchdog. It never touches a thread in observe mode (governor never calls
// Apply then); Watchdog and AppliedLabel stay safe no-ops in that case.
type Actuator struct {
	governor         *Governor
	aggression       aggression
	cooldown         time.Duration
	promotionCeiling int
	demotionFloor    int
	nextCore         int // round-robin cursor into the physical-core leads
	states           map[thread.Key]*actionState
}

func NewActuator(g *Governor) *Actuator {
	ceiling := 2
	if g.auto.PromotionCeiling != nil {
		ceiling = *g.auto.PromotionCeiling
	}
	floor := -1
	if g.auto.DemotionFloor != nil {
		floor = *g.auto.DemotionFloor
	}

	return &Actuator{
		governor:         g,
		aggression:       parseAggression(g.auto.Aggression),
		cooldown:         time.Duration(g.auto.CooldownMS) * time.Millisecond,
		promotionCeiling: ceiling,
		demotionFloor:    floor,
		states:           make(map[thread.Key]*actionState),
	}
}

// Apply walks this tick's stable verdicts and enacts the bucket policy. Only
// called in auto optimisation during PhaseStable, so it can assume the game is
// in a settled state.
func (a *Actuator) Apply(sample *Sample, facts map[thread.Key]*Facts, verdicts map[thread.Key]Verdict) {
	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}

		entry := threadSample.Entry
		if entry == nil || entry.Handle == 0 {
			continue
		}

		verdict, ok := verdicts[key]
		if !ok || !verdict.Stable {
			continue // only act on classifications that held for stable_windows
		}

		f := facts[key]
		if f == nil {
			continue
		}

		a.applyOne(key, entry, verdict, f)
	}
}

func (a *Actuator) applyOne(key thread.Key, entry *thread.Entry, verdict Verdict, f *Facts) {
	// Manual rules win outright: never fight a thread the user has pinned.
	if a.governor.manual.Owns(key) {
		return
	}

	state := a.stateFor(key)
	if state.quarantined {
		return
	}

	// The classification that motivated the current settings no longer holds,
	// so undo them — before the cooldown check, because leaving a thread tuned
	// for a bucket it is no longer in is the thing we are fixing. Previously
	// state.bucket was recorded and never read, so a thread misclassified once
	// (typically during loading, when the hot set looks nothing like gameplay)
	// kept those settings for the rest of the session; only the starvation
	// watchdog could ever undo anything.
	if state.bucket != BucketNone && state.bucket != verdict.Bucket {
		tuned := entry.Touched()
		if err := entry.Restore(); err != nil {
			return // journal intact; try again next tick
		}

		state.bucket = BucketNone
		state.demoted = false
		state.hasIdeal = false

		if tuned {
			// Re-tune for the new bucket only after a cooldown, so a flapping
			// classification cannot drive a burst of changes.
			state.label = "reverted"
			state.lastChange = time.Now()
			return
		}
		// Nothing had actually been applied; fall through and tune normally.
	}

	if !state.lastChange.IsZero() && time.Since(state.lastChange) < a.cooldown {
		return
	}

	var applied []string
	demoted := false

	switch verdict.Bucket {
	case BucketCritical:
		if ok, _ := entry.RaisePriorityTo(a.promotionCeiling); ok {
			applied = append(applied, "prio↑")
		}
		if len(a.governor.auto.CriticalCores) > 0 {
			if ok, _ := entry.ApplyCpuSets(a.governor.auto.CriticalCores); ok {
				applied = append(applied, "cpuset")
			}
		}
		// Steer each critical thread onto a distinct physical core so the
		// frame-critical set does not pile onto SMT siblings (full mode only).
		// Assigned once and remembered: the round-robin cursor used to advance
		// on every re-apply, so a thread that stayed critical was moved to a
		// different core every cooldown, throwing away its cache warmth.
		if cpu, has := a.idealCoreFor(state); has {
			if ok, _ := entry.ApplyIdealProcessor(cpu); ok {
				applied = append(applied, "ideal")
			}
		}
		if a.aggression == aggAggressive {
			if ok, _ := entry.ApplyIoPriority(ioPriorityHigh); ok {
				applied = append(applied, "io↑")
			}
		}

	case BucketInteractive:
		// Only guarantee it is not stuck below Normal; never demote.
		if ok, _ := entry.RaisePriorityTo(priorityNormal); ok {
			applied = append(applied, "prio=norm")
		}

	case BucketBackground:
		if a.aggression == aggConservative {
			return // raise-only mode never demotes
		}
		if a.aggression == aggStandard {
			// Standard demotes only the two roles we are most sure are junk.
			if (verdict.Role != RoleTelemetry && verdict.Role != RolePoolIdle) ||
				verdict.Confidence < demoteMinConfidence {
				return
			}
		}

		if ok, _ := entry.LowerPriorityTo(a.demotionFloor); ok {
			applied = append(applied, "prio↓")
			demoted = true
		}

		if a.aggression == aggAggressive {
			if ok, _ := entry.ApplyMemoryPriority(backgroundMemoryPriority); ok {
				applied = append(applied, "mem↓")
			}
			// I/O throttling is safe for telemetry but not for asset loaders,
			// which must stream at full speed even when classified background.
			if verdict.Role == RoleTelemetry {
				if ok, _ := entry.ApplyIoPriority(ioPriorityLow); ok {
					applied = append(applied, "io↓")
				}
				if ok, _ := entry.ApplyEcoQoS(); ok {
					applied = append(applied, "eco")
				}
			}
			if len(a.governor.auto.BackgroundCores) > 0 {
				if ok, _ := entry.ApplyCpuSets(a.governor.auto.BackgroundCores); ok {
					applied = append(applied, "cpuset")
				}
			}
		}

	default:
		return // BucketNone / BucketUntouchable — leave the thread alone
	}

	if len(applied) == 0 {
		// Nothing needed changing — the thread already matches its bucket.
		// Record the bucket anyway so the reclassification check above can see
		// it later, and start the cooldown so we stop re-probing every tick.
		if state.bucket == BucketNone {
			state.bucket = verdict.Bucket
			state.lastChange = time.Now()
		}
		return
	}

	state.lastChange = time.Now()
	state.label = strings.Join(applied, ",")
	state.bucket = verdict.Bucket
	if demoted {
		state.demoted = true
	}
}

// idealCoreFor returns the physical core this thread should be steered onto,
// assigning one from the round-robin cursor on first use and reusing it
// thereafter. Reports false when topology is unavailable (limited mode).
func (a *Actuator) idealCoreFor(state *actionState) (int, bool) {
	if state.hasIdeal {
		return state.idealCPU, true
	}
	if a.governor.topology == nil {
		return 0, false
	}

	leads := a.governor.topology.PhysicalCoreLeads()
	if len(leads) == 0 {
		return 0, false
	}

	state.idealCPU = leads[a.nextCore%len(leads)]
	state.hasIdeal = true
	a.nextCore++

	return state.idealCPU, true
}

// Watchdog rolls back any demoted thread that is now CPU-starved: a background
// classification that turned out to be load-bearing. The thread is fully
// reverted and quarantined so the governor stops fighting the scheduler.
func (a *Actuator) Watchdog(facts map[thread.Key]*Facts) {
	for key, state := range a.states {
		if !state.demoted || state.quarantined {
			continue
		}

		f := facts[key]
		if f == nil || f.Series.ReadyRatio <= starvationReadyRatio {
			continue
		}

		entry := a.governor.sampler.Cache().Lookup(key)
		if entry == nil {
			continue
		}
		if err := entry.Restore(); err != nil {
			continue // keep trying next tick
		}

		state.demoted = false
		state.quarantined = true
		state.label = "rolled-back"
	}
}

// Reset forgets all per-thread action state, letting the governor re-tune from
// scratch after a UI-requested revert. Threads themselves are restored
// separately (by the governor's RestoreAll) before this is called.
func (a *Actuator) Reset() {
	a.states = make(map[thread.Key]*actionState)
	a.nextCore = 0
}

// AppliedLabel is the UI's "applied" column: the last action taken on a
// thread, or "" when the governor has not touched it.
func (a *Actuator) AppliedLabel(key thread.Key) string {
	if state, ok := a.states[key]; ok {
		return state.label
	}
	return ""
}

func (a *Actuator) stateFor(key thread.Key) *actionState {
	state, ok := a.states[key]
	if !ok {
		state = &actionState{}
		a.states[key] = state
	}
	return state
}
