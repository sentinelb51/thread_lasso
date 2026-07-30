//go:build windows && amd64

package governor

import (
	"ThreadOrchestra/thread"
	"sort"
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
	idealSlot   int    // index of that core in the topology's lead list
	hasIdeal    bool
	demoted     bool // we lowered this thread; the watchdog may roll it back
	quarantined bool // rolled back after starvation — never touched again
}

// roleImportance orders the critical set for physical-core placement: the
// threads whose stalls are most visible to the player get first pick of the
// least-contended core. Roles outside the critical set never reach placement.
var roleImportance = map[Role]int{
	RoleMainSim:      5,
	RoleRenderSubmit: 4,
	RoleGPUWorker:    3,
	RoleAudio:        2,
	RoleInput:        1,
}

// bucketImportance ranks buckets for the same ordering. Critical threads are
// placed before anything else claims a core.
func bucketImportance(b Bucket) int {
	switch b {
	case BucketCritical:
		return 2
	case BucketInteractive:
		return 1
	case BucketBackground:
		return 0
	default:
		return -1
	}
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
	coreLoad         []int // threads currently steered onto each physical-core lead
	states           map[thread.Key]*actionState
	pending          []pendingAction // reused across ticks to keep Apply allocation-free
}

// pendingAction is one thread's actuation, queued so the tick can be ordered by
// importance before anything is applied.
type pendingAction struct {
	key     thread.Key
	entry   *thread.Entry
	verdict Verdict
	facts   *Facts
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
//
// Threads are ordered by importance rather than by snapshot order, because the
// order decides who gets the least-contended physical core: the main thread
// should not lose a dedicated core to whichever job worker happened to appear
// first in the kernel's thread list.
func (a *Actuator) Apply(sample *Sample, facts map[thread.Key]*Facts, verdicts map[thread.Key]Verdict) {
	a.pending = a.pending[:0]

	for i := range sample.Threads {
		threadSample := &sample.Threads[i]
		key := thread.Key{TID: threadSample.TID, CreateTime: threadSample.CreateTime}

		entry := threadSample.Entry
		if entry == nil || entry.Handle == 0 {
			continue
		}

		verdict, ok := verdicts[key]
		if !ok || !verdict.Stable {
			continue // only act on classifications the bucket filter has committed
		}

		f := facts[key]
		if f == nil {
			continue
		}

		a.pending = append(a.pending, pendingAction{key, entry, verdict, f})
	}

	sort.SliceStable(a.pending, func(i, j int) bool {
		left, right := &a.pending[i], &a.pending[j]
		if bl, br := bucketImportance(left.verdict.Bucket), bucketImportance(right.verdict.Bucket); bl != br {
			return bl > br
		}
		if rl, rr := roleImportance[left.verdict.Role], roleImportance[right.verdict.Role]; rl != rr {
			return rl > rr
		}

		return left.facts.CyclesShare > right.facts.CyclesShare
	})

	for i := range a.pending {
		action := &a.pending[i]
		a.applyOne(action.key, action.entry, action.verdict, action.facts)
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
		a.releaseCore(state)

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
		// Demotions require more evidence than promotions at every level: a
		// wrongly demoted thread costs frames for as long as it stays demoted,
		// while a wrongly untouched one costs nothing. Aggressive used to skip
		// this check entirely, so the most invasive mode was also the one acting
		// on the weakest evidence.
		if verdict.Confidence < demoteMinConfidence {
			return
		}
		if a.aggression == aggStandard {
			// Standard demotes only the two roles we are most sure are junk.
			if verdict.Role != RoleTelemetry && verdict.Role != RolePoolIdle {
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
// assigning one on first use and reusing it thereafter. The core is the
// least-occupied lead, which replaces a round-robin cursor that only ever
// advanced: cores were never given back when a thread died or was reverted, so
// after a session's worth of thread churn the surviving critical threads could
// all be pointed at the same physical core while the rest of the machine sat
// idle. Reports false when topology is unavailable (limited mode).
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
	if len(a.coreLoad) != len(leads) {
		a.coreLoad = make([]int, len(leads))
	}

	// Ties resolve to the lowest core index, so with the importance ordering in
	// Apply the main thread lands on the first lead every session.
	slot := 0
	for i, load := range a.coreLoad {
		if load < a.coreLoad[slot] {
			slot = i
		}
	}

	a.coreLoad[slot]++
	state.idealSlot = slot
	state.idealCPU = leads[slot]
	state.hasIdeal = true

	return state.idealCPU, true
}

// releaseCore returns a thread's physical-core reservation to the pool.
func (a *Actuator) releaseCore(state *actionState) {
	if !state.hasIdeal {
		return
	}

	state.hasIdeal = false
	if state.idealSlot < len(a.coreLoad) && a.coreLoad[state.idealSlot] > 0 {
		a.coreLoad[state.idealSlot]--
	}
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
		a.releaseCore(state)
	}
}

// Prune drops action state for threads that no longer exist. Without it the
// state map grew for the whole session, and — worse — dead threads went on
// holding physical-core reservations that live threads were then steered away
// from.
func (a *Actuator) Prune(live map[thread.Key]*Series) {
	for key, state := range a.states {
		if _, ok := live[key]; !ok {
			a.releaseCore(state)
			delete(a.states, key)
		}
	}
}

// Reset forgets all per-thread action state, letting the governor re-tune from
// scratch after a UI-requested revert. Threads themselves are restored
// separately (by the governor's RestoreAll) before this is called.
func (a *Actuator) Reset() {
	a.states = make(map[thread.Key]*actionState)
	a.coreLoad = nil
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
